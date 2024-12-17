package main

import (
    "bufio"
		"bytes"
    "fmt"
    "os"
    "strings"
    "sync"
    "time"
		"os/exec"
		"context"
		"net"
    "golang.org/x/crypto/ssh"
    "path/filepath"
    "github.com/spf13/pflag"
)

// Config holds all the runtime configuration options for the program.
// this includes user-specified flags and command arguments.
type Config struct {
    interfaces []string    // list of network interfaces to apply/remove latency rules
    identityFile string   // path to ssh private key file
    parallel int          // number of concurrent workers
    latency string        // latency value to apply (e.g., "10ms")
    yes bool             // skip confirmation for apply command
}

const warningBanner = `
▗▖ ▗▖ ▗▄▖ ▗▄▄▖ ▗▖  ▗▖▗▄▄▄▖▗▖  ▗▖ ▗▄▄▖
▐▌ ▐▌▐▌ ▐▌▐▌ ▐▌▐▛▚▖▐▌  █  ▐▛▚▖▐▌▐▌   
▐▌ ▐▌▐▛▀▜▌▐▛▀▚▖▐▌ ▝▜▌  █  ▐▌ ▝▜▌▐▌▝▜▌
▐▙█▟▌▐▌ ▐▌▐▌ ▐▌▐▌  ▐▌▗▄█▄▖▐▌  ▐▌▝▚▄▞

`

func main() {
    config := parseFlags()

    // Get remaining non-flag arguments
    args := pflag.Args()
    if len(args) < 1 {
        fmt.Println("Usage: clustercrippler [apply|remove|ping] [latency]")
        os.Exit(1)
    }

    cmd := args[0]

    // Read hosts early to get count for warning message
    hosts := readHosts()

    if cmd == "apply" {
        if len(args) < 2 {
            fmt.Println("Error: latency parameter required for apply command")
            os.Exit(1)
        }
        config.latency = args[1]
        
        if !config.yes {
            fmt.Print(warningBanner)
            fmt.Printf("This will degrade network performance on %d target hosts.\n", len(hosts))
            fmt.Println("If you are sure, re-run this command with --yes to continue")
            os.Exit(1)
        }
    }

    processSessions(hosts, cmd, config)
}

// parseFlags processes command line flags and returns a populated Config.
// it uses spf13/pflag for posix-style flag handling and sets up
// default values where appropriate.
func parseFlags() Config {
    var config Config
    var interfaceList string

    // Define flags with references to variables
    pflag.StringVarP(&interfaceList, "interfaces", "I", "", "Comma-separated list of interfaces")
    pflag.StringVarP(&config.identityFile, "identity", "i", filepath.Join(os.Getenv("HOME"), ".ssh/id_rsa"), "SSH identity file")
    pflag.IntVarP(&config.parallel, "parallel", "p", 10, "Number of parallel workers")
    pflag.BoolVarP(&config.yes, "yes", "y", false, "Skip confirmation prompt and proceed with apply")
    
    pflag.Parse()

    // Split interface list if provided
    if interfaceList != "" {
        config.interfaces = strings.Split(interfaceList, ",")
    }

    return config
}

// readHosts reads hostnames from stdin, one per line.
// typically used with a pipe, e.g., `cat hosts.txt | clustercrippler`.
func readHosts() []string {
    var hosts []string
    scanner := bufio.NewScanner(os.Stdin)
    for scanner.Scan() {
        hosts = append(hosts, strings.TrimSpace(scanner.Text()))
    }
    return hosts
}

// getSSHClient establishes an SSH connection to a host using the provided config.
// it sets up timeouts and handles authentication using the specified private key.
// returns an established client or an error if connection fails.
func getSSHClient(host string, config Config) (*ssh.Client, error) {
    key, err := os.ReadFile(config.identityFile)
    if err != nil {
        return nil, fmt.Errorf("unable to read private key: %v", err)
    }

    signer, err := ssh.ParsePrivateKey(key)
    if err != nil {
        return nil, fmt.Errorf("unable to parse private key: %v", err)
    }

    sshConfig := &ssh.ClientConfig{
        User: "root",
        Auth: []ssh.AuthMethod{
            ssh.PublicKeys(signer),
        },
        HostKeyCallback: ssh.InsecureIgnoreHostKey(),
        Timeout: 10 * time.Second,
    }

    // Add a timeout for the dial operation
    conn, err := net.DialTimeout("tcp", host+":22", 5*time.Second)
    if err != nil {
        return nil, fmt.Errorf("failed to connect: %v", err)
    }

    c, chans, reqs, err := ssh.NewClientConn(conn, host+":22", sshConfig)
    if err != nil {
        conn.Close()
        return nil, fmt.Errorf("failed to establish SSH connection: %v", err)
    }

    return ssh.NewClient(c, chans, reqs), nil
}


// runCommand executes a command on the remote host via SSH with a timeout.
// it captures both stdout and stderr, and ensures cleanup of resources.
// the timeout prevents hanging on slow/unresponsive hosts.
func runCommand(client *ssh.Client, cmd string) (string, error) {
    session, err := client.NewSession()
    if err != nil {
        return "", err
    }
    defer session.Close()

    // Create a channel to handle command completion
    done := make(chan struct{})
    var output []byte
    var cmdErr error

    go func() {
        output, cmdErr = session.CombinedOutput(cmd)
        close(done)
    }()

    // Wait for either command completion or timeout
    select {
    case <-done:
        return string(output), cmdErr
    case <-time.After(30 * time.Second):
        session.Close() // Force close the session on timeout
        return "", fmt.Errorf("command timed out after 30 seconds")
    }
}


// checkPrerequisites verifies that the required tools are available on the target host.
// specifically checks for:
// - tc command (part of iproute2)
// - sch_netem kernel module (will attempt to load it if not loaded)
func checkPrerequisites(client *ssh.Client) error {
    // Check for tc command
    _, err := runCommand(client, "which tc")
    if err != nil {
        return fmt.Errorf("tc command not found")
    }

    // Try to load sch_netem module
    _, err = runCommand(client, "modprobe sch_netem")
    if err != nil {
        return fmt.Errorf("sch_netem kernel module not available")
    }

    return nil
}


// getInterfaces retrieves a list of ethernet interfaces on the remote host.
// looks for interfaces starting with eth, ens, eno, or enp.
// returns a slice of interface names or an error if the command fails.
func getInterfaces(client *ssh.Client) ([]string, error) {
    output, err := runCommand(client, "ls -1 /sys/class/net | grep -E '^(eth|ens|eno|enp)'")
    if err != nil {
        return nil, err
    }
    return strings.Split(strings.TrimSpace(output), "\n"), nil
}

// processSessions sets up and manages the worker pool for parallel execution.
// it creates channels for job distribution and result collection,
// spawns worker goroutines, and handles graceful shutdown.
// also manages an async output handler to prevent output blocking.
func processSessions(hosts []string, cmd string, config Config) {
    jobs := make(chan string, len(hosts))
    results := make(chan string, len(hosts))
    var wg sync.WaitGroup

    // Start output handler goroutine
    done := make(chan struct{})
    go func() {
        for result := range results {
            fmt.Println(result)
        }
        close(done)
    }()

    // Start worker pool
    for i := 0; i < config.parallel; i++ {
        wg.Add(1)
        go worker(jobs, results, &wg, cmd, config)
    }

    // Send jobs
    for _, host := range hosts {
        jobs <- host
    }
    close(jobs)

    // Wait for workers to finish
    wg.Wait()
    
    // Close results channel and wait for output handler to finish
    close(results)
    <-done
}

// getTcQdisc checks if a netem qdisc rule exists on the specified interface.
// used to prevent errors when removing non-existent rules.
func getTcQdisc(client *ssh.Client, iface string) (bool, error) {
    output, err := runCommand(client, fmt.Sprintf("tc qdisc show dev %s", iface))
    if err != nil {
        return false, err
    }
    return strings.Contains(output, "netem"), nil
}


// worker is the main workhorse function that processes individual hosts.
// handles all three commands (ping, apply, remove) and reports results.
// for apply/remove:
// - validates interfaces exist before attempting changes
// - only tries to remove tc rules from interfaces that have them
// - tracks which interfaces were actually modified
// - provides clear success/failure messaging
func worker(jobs <-chan string, results chan<- string, wg *sync.WaitGroup, cmd string, config Config) {
    defer wg.Done()

    for host := range jobs {
        switch cmd {
        case "ping":
            output, err := executePing(host)
            if err != nil {
                results <- fmt.Sprintf("%s\tError: %v", host, err)
                continue
            }
            results <- fmt.Sprintf("%s\t%s", host, output)

        case "apply", "remove":
            client, err := getSSHClient(host, config)
            if err != nil {
                results <- fmt.Sprintf("%s\tError: %v", host, err)
                continue
            }
            defer client.Close()

            if err := checkPrerequisites(client); err != nil {
                results <- fmt.Sprintf("%s\tError: %v", host, err)
                continue
            }

            systemIfaces, err := getInterfaces(client)
            if err != nil {
                results <- fmt.Sprintf("%s\tError: %v", host, err)
                continue
            }

            ifaceExists := make(map[string]bool)
            for _, iface := range systemIfaces {
                ifaceExists[iface] = true
            }

            var targetIfaces []string
            if len(config.interfaces) > 0 {
                for _, iface := range config.interfaces {
                    if ifaceExists[iface] {
                        targetIfaces = append(targetIfaces, iface)
                    }
                }
                if len(targetIfaces) == 0 {
                    results <- fmt.Sprintf("%s\tError: none of the specified interfaces exist on host", host)
                    continue
                }
            } else {
                targetIfaces = systemIfaces
            }

            var modifiedIfaces []string
            if cmd == "apply" {
                for _, iface := range targetIfaces {
                    _, err := runCommand(client, fmt.Sprintf("tc qdisc add dev %s root netem delay %s", iface, config.latency))
                    if err != nil {
                        results <- fmt.Sprintf("%s\tError on %s: %v", host, iface, err)
                        continue
                    }
                    modifiedIfaces = append(modifiedIfaces, iface)
                }
            } else { // remove
                for _, iface := range targetIfaces {
                    // Check if netem is actually applied to this interface
                    hasNetem, err := getTcQdisc(client, iface)
                    if err != nil {
                        results <- fmt.Sprintf("%s\tError checking %s: %v", host, iface, err)
                        continue
                    }
                    if hasNetem {
                        _, err := runCommand(client, fmt.Sprintf("tc qdisc del dev %s root", iface))
                        if err != nil {
                            results <- fmt.Sprintf("%s\tError on %s: %v", host, iface, err)
                            continue
                        }
                        modifiedIfaces = append(modifiedIfaces, iface)
                    }
                }
            }

            if len(modifiedIfaces) > 0 {
                if cmd == "apply" {
                    results <- fmt.Sprintf("%s\tLatency applied to interfaces: %s", host, strings.Join(modifiedIfaces, ", "))
                } else {
                    results <- fmt.Sprintf("%s\tLatency removed from interfaces: %s", host, strings.Join(modifiedIfaces, ", "))
                }
            } else {
                if cmd == "remove" {
                    results <- fmt.Sprintf("%s\tNo latency rules found to remove", host)
                } else {
                    results <- fmt.Sprintf("%s\tNo interfaces were modified", host)
                }
            }
        }
    }
}


// executePing runs a local ping command to measure latency to a host.
// uses context for timeout control and captures stderr for better error reporting.
// returns just the timing portion of the ping output (e.g., "10.5 ms").
func executePing(host string) (string, error) {
    // create a context with 5-second timeout
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    // run ping with a single packet and 3-second -W timeout
    cmd := exec.CommandContext(ctx, "ping", "-c", "1", "-W", "3", host)
    
    // capture stderr for better error messages
    var stderr bytes.Buffer
    cmd.Stderr = &stderr
    
    output, err := cmd.Output()
    if err != nil {
        // check if we hit our context timeout
        if ctx.Err() == context.DeadlineExceeded {
            return "", fmt.Errorf("ping timed out after 5 seconds")
        }
        // include stderr in error if available
        if stderr.Len() > 0 {
            return "", fmt.Errorf("%v: %s", err, stderr.String())
        }
        return "", err
    }

    // parse out just the timing info from the ping output
    lines := strings.Split(string(output), "\n")
    for _, line := range lines {
        if strings.Contains(line, "time=") {
            parts := strings.Split(line, "time=")
            if len(parts) > 1 {
                timeStr := strings.Split(parts[1], " ")[0]
                return timeStr, nil
            }
        }
    }

    return "", fmt.Errorf("could not parse ping output")
}
