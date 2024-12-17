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

type Config struct {
    interfaces []string
    identityFile string
    parallel int
    latency string
    yes bool
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

func readHosts() []string {
    var hosts []string
    scanner := bufio.NewScanner(os.Stdin)
    for scanner.Scan() {
        hosts = append(hosts, strings.TrimSpace(scanner.Text()))
    }
    return hosts
}

// func getSSHClient(host string, config Config) (*ssh.Client, error) {
//     key, err := os.ReadFile(config.identityFile)
//     if err != nil {
//         return nil, fmt.Errorf("unable to read private key: %v", err)
//     }
//
//     signer, err := ssh.ParsePrivateKey(key)
//     if err != nil {
//         return nil, fmt.Errorf("unable to parse private key: %v", err)
//     }
//
//     sshConfig := &ssh.ClientConfig{
//         User: "root",
//         Auth: []ssh.AuthMethod{
//             ssh.PublicKeys(signer),
//         },
//         HostKeyCallback: ssh.InsecureIgnoreHostKey(),
//         Timeout: 5 * time.Second,
//     }
//
//     client, err := ssh.Dial("tcp", host+":22", sshConfig)
//     if err != nil {
//         return nil, fmt.Errorf("failed to dial: %v", err)
//     }
//
//     return client, nil
// }
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
// func runCommand(client *ssh.Client, cmd string) (string, error) {
//     session, err := client.NewSession()
//     if err != nil {
//         return "", err
//     }
//     defer session.Close()
//
//     output, err := session.CombinedOutput(cmd)
//     return string(output), err
// }

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


func getInterfaces(client *ssh.Client) ([]string, error) {
    output, err := runCommand(client, "ls -1 /sys/class/net | grep -E '^(eth|ens|eno|enp)'")
    if err != nil {
        return nil, err
    }
    return strings.Split(strings.TrimSpace(output), "\n"), nil
}

// func processSessions(hosts []string, cmd string, config Config) {
//     jobs := make(chan string, len(hosts))
//     results := make(chan string, len(hosts))
//     var wg sync.WaitGroup
//
//     // Start worker pool
//     for i := 0; i < config.parallel; i++ {
//         wg.Add(1)
//         go worker(jobs, results, &wg, cmd, config)
//     }
//
//     // Send jobs
//     for _, host := range hosts {
//         jobs <- host
//     }
//     close(jobs)
//
//     // Wait for workers to finish
//     wg.Wait()
//     close(results)
//
//     // Print results
//     for result := range results {
//         fmt.Println(result)
//     }
// }

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

func worker(jobs <-chan string, results chan<- string, wg *sync.WaitGroup, cmd string, config Config) {
    defer wg.Done()

    for host := range jobs {
        switch cmd {
        case "ping":
            // Execute ping locally instead of over SSH
            output, err := executePing(host)
            if err != nil {
                results <- fmt.Sprintf("%s\tError: %v", host, err)
                continue
            }
            results <- fmt.Sprintf("%s\t%s", host, output)

        case "apply":
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

            var interfaces []string
            if len(config.interfaces) > 0 {
                interfaces = config.interfaces
            } else {
                var err error
                interfaces, err = getInterfaces(client)
                if err != nil {
                    results <- fmt.Sprintf("%s\tError: %v", host, err)
                    continue
                }
            }

            for _, iface := range interfaces {
                _, err := runCommand(client, fmt.Sprintf("tc qdisc add dev %s root netem delay %s", iface, config.latency))
                if err != nil {
                    results <- fmt.Sprintf("%s\tError applying to %s: %v", host, iface, err)
                    continue
                }
            }
            results <- fmt.Sprintf("%s\tLatency applied", host)

        case "remove":
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

            var interfaces []string
            if len(config.interfaces) > 0 {
                interfaces = config.interfaces
            } else {
                var err error
                interfaces, err = getInterfaces(client)
                if err != nil {
                    results <- fmt.Sprintf("%s\tError: %v", host, err)
                    continue
                }
            }

            for _, iface := range interfaces {
                _, err := runCommand(client, fmt.Sprintf("tc qdisc del dev %s root", iface))
                if err != nil {
                    results <- fmt.Sprintf("%s\tError removing from %s: %v", host, iface, err)
                    continue
                }
            }
            results <- fmt.Sprintf("%s\tLatency removed", host)
        }
    }
}
func executePing(host string) (string, error) {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    cmd := exec.CommandContext(ctx, "ping", "-c", "1", "-W", "3", host)
    
    // Create a pipe for the stderr
    var stderr bytes.Buffer
    cmd.Stderr = &stderr
    
    output, err := cmd.Output()
    if err != nil {
        if ctx.Err() == context.DeadlineExceeded {
            return "", fmt.Errorf("ping timed out after 5 seconds")
        }
        // Include the stderr in the error message
        if stderr.Len() > 0 {
            return "", fmt.Errorf("%v: %s", err, stderr.String())
        }
        return "", err
    }

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
// func executePing(host string) (string, error) {
//     // Create context with timeout
//     ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//     defer cancel()
//
//     cmd := exec.CommandContext(ctx, "ping", "-c", "1", "-W", "3", host)
//     output, err := cmd.Output()
//     if err != nil {
//         if ctx.Err() == context.DeadlineExceeded {
//             return "", fmt.Errorf("ping timed out after 5 seconds")
//         }
//         return "", err
//     }
//
//     // Parse the ping output to extract just the time
//     lines := strings.Split(string(output), "\n")
//     for _, line := range lines {
//         if strings.Contains(line, "time=") {
//             parts := strings.Split(line, "time=")
//             if len(parts) > 1 {
//                 timeStr := strings.Split(parts[1], " ")[0]
//                 return timeStr, nil
//             }
//         }
//     }
//
//     return "", fmt.Errorf("could not parse ping output")
// }
// func executePing(host string) (string, error) {
//     output, err := exec.Command("ping", "-c", "1", host).Output()
//     if err != nil {
//         return "", err
//     }
//
//     // Parse the ping output to extract just the time
//     lines := strings.Split(string(output), "\n")
//     for _, line := range lines {
//         if strings.Contains(line, "time=") {
//             parts := strings.Split(line, "time=")
//             if len(parts) > 1 {
//                 timeStr := strings.Split(parts[1], " ")[0]
//                 return timeStr, nil
//             }
//         }
//     }
//
//     return "", fmt.Errorf("could not parse ping output")
// }
//
