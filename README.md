# clustercrippler

The input is a single field text file containing a list of hostnames or IP addresses:

	$ cat myhosts.txt 
	mail4c999
	mail5c999
	mail6c999
	mail7c999

To ping them all concurrently:

	$ cat myhosts.txt  | ./bin/clustercrippler ping
	mail4c999       2.06
	mail5c999       2.03
	mail7c999       1.74
	mail6c999       2.67

To apply the network latency crippling:

	$ cat myhosts.txt  | ./bin/clustercrippler apply 10ms
	mail6c999       Latency applied to interfaces: ens18, ens19
	mail4c999       Latency applied to interfaces: ens18, ens19
	mail5c999       Latency applied to interfaces: ens18, ens19
	mail7c999       Latency applied to interfaces: ens18, ens19

To remove the crippling:

	$ cat myhosts.txt  | ./bin/clustercrippler remove
	mail6c999       Latency removed from interfaces: ens18, ens19
	mail4c999       Latency removed from interfaces: ens18, ens19
	mail5c999       Latency removed from interfaces: ens18, ens19
	mail7c999       Latency removed from interfaces: ens18, ens19

To only apply crippling rules to specific interface(s), provide a CSV list of desired iface names like: `-I/--interfaces "eth1,eth0`

It only applys to interfaces that actually exist on the host:

	$ cat myhosts.txt  | ./bin/clustercrippler -I "ens18,ens20,enp999" apply 10ms
	mail6c999       Latency applied to interfaces: ens18
	mail4c999       Latency applied to interfaces: ens18
	mail5c999       Latency applied to interfaces: ens18
	mail7c999       Latency applied to interfaces: ens18

Removing them will only remove them from interfaces that exist and have qdisc rules applied:

	$ cat myhosts.txt  | ./bin/clustercrippler remove
	mail6c999       Latency removed from interfaces: ens18
	mail4c999       Latency removed from interfaces: ens18
	mail5c999       Latency removed from interfaces: ens18
	mail7c999       Latency removed from interfaces: ens18

To run more or less concurrent SSH sessions, use `-p/--parallel=N`

	$ cat myhosts.txt  | ./bin/clustercrippler -p 20  ping
	mail4c999       1.92
	mail5c999       1.87
	mail6c999       1.63
	mail7c999       1.78
