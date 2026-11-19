// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

// Command lab is the entrypoint of the network lab image: it runs the
// scenario suite, or brings the lab up for interactive use and changes its
// network conditions. See test/e2e/README.md.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"openrung/test/e2e/lab"
)

const usage = `usage: lab [-v] <command>

Run the suite or the lab:
  test [go test flags]       run the scenario suite (the image default)
  up                         build the lab and keep it running
  down                       stop every service and remove the network
  status                     services, listed relays, active impairments

Look inside:
  sh <node> [command...]     run a shell or a command inside a node
  logs <service>             print a service's log
  start|stop|kill|restart <service>

Drive the client (runs in node "user"):
  client [connect flags]     start the headless client and wait for it
  fetch [url]                fetch through the client's proxy (default: exit IP)

Change the network:
  net shape <link> <profile | key=value...>   keys: delay jitter loss rate
  net shape <link> clear
  net block <node> <dst>[:port] [tcp|udp] [drop|reset|unreachable]
  net allow-only <node> <proto:port,...> [drop|reset|unreachable]
  net down|up <link>         cut or restore a link
  net nat <router> cone|symmetric
  net forward <router> <port> <host:port>
  net clear [node]           remove impairments (everywhere by default)
  net list                   nodes, links and shape profiles

-v prints every command the lab runs.
`

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "-v" {
		lab.Trace = func(line string) { fmt.Fprintln(os.Stderr, "+ "+line) }
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := dispatch(args[0], args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lab:", err)
		os.Exit(1)
	}
}

func dispatch(command string, args []string) error {
	switch command {
	case "test":
		// Replace this process so the suite's exit code is the container's.
		argv := append([]string{"e2e.test", "-test.v"}, args...)
		return syscall.Exec("/usr/local/bin/e2e.test", argv, os.Environ())
	case "up":
		return up()
	case "down":
		lab.Down()
		return nil
	case "status":
		status, err := lab.Status()
		fmt.Print(status)
		return err
	case "sh":
		if len(args) == 0 {
			return errors.New("sh needs a node")
		}
		command := args[1:]
		if len(command) == 0 {
			command = []string{"sh"}
		}
		argv := append([]string{"ip", "netns", "exec", args[0]}, command...)
		return syscall.Exec("/sbin/ip", argv, os.Environ())
	case "logs":
		if len(args) != 1 {
			return errors.New("logs needs a service")
		}
		log, err := lab.ReadLog(args[0], 0)
		fmt.Print(log)
		return err
	case "start", "stop", "kill", "restart":
		if len(args) != 1 {
			return fmt.Errorf("%s needs a service", command)
		}
		return service(command, args[0])
	case "client":
		return client(args)
	case "fetch":
		url := "http://exit.example/ip"
		if len(args) > 0 {
			url = args[0]
		}
		body, err := lab.CurrentClient().Fetch(url, 30*time.Second)
		fmt.Println(body)
		return err
	case "net":
		if len(args) == 0 {
			return errors.New("net needs a subcommand")
		}
		return network(args[0], args[1:])
	case "internet":
		if len(args) != 1 {
			return errors.New("internet needs a CA output path")
		}
		return lab.ServeInternet(args[0])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q\n%s", command, usage)
}

func up() error {
	if err := lab.Up(); err != nil {
		return err
	}
	status, err := lab.Status()
	if err != nil {
		return err
	}
	fmt.Print(status)
	fmt.Println("lab is up; open a shell in this container and run `lab help`")
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	lab.Down()
	return nil
}

func service(command, name string) error {
	if command != "start" {
		stop := lab.Stop
		if command == "kill" {
			stop = lab.Kill
		}
		if err := stop(name); err != nil {
			return err
		}
		if command != "restart" {
			return nil
		}
	}
	definition, ok := lab.ServiceByName(name)
	if !ok {
		return fmt.Errorf("unknown service %q", name)
	}
	return lab.Start(definition)
}

func client(args []string) error {
	_ = lab.Stop("client")
	client, err := lab.StartClient(lab.ClientConfig{Args: args})
	if err != nil {
		return err
	}
	via, err := client.WaitConnected(90 * time.Second)
	if err != nil {
		return err
	}
	exit, err := client.ExitIP(15 * time.Second)
	if err != nil {
		return err
	}
	fmt.Printf("connected via %s; traffic exits at %s\n", via, exit)
	return nil
}

func network(command string, args []string) error {
	switch command {
	case "shape":
		if len(args) < 2 {
			return errors.New("shape needs a link and a profile, key=value pairs, or clear")
		}
		shape, err := parseShape(args[1:])
		if err != nil {
			return err
		}
		return lab.ShapeLink(args[0], shape)
	case "block":
		if len(args) < 2 {
			return errors.New("block needs a node and a destination")
		}
		match, action, err := parseBlock(args[1:])
		if err != nil {
			return err
		}
		_, err = lab.Block(args[0], match, action)
		return err
	case "allow-only":
		if len(args) < 2 {
			return errors.New("allow-only needs a node and a proto:port list")
		}
		var allowed []lab.Match
		for _, item := range strings.Split(args[1], ",") {
			proto, port, ok := strings.Cut(item, ":")
			number, err := strconv.Atoi(port)
			if !ok || err != nil {
				return fmt.Errorf("bad allow-only entry %q (want proto:port)", item)
			}
			allowed = append(allowed, lab.Match{Proto: proto, Port: number})
		}
		action := lab.Drop
		if len(args) > 2 {
			action = lab.Action(args[2])
		}
		return lab.AllowOnly(args[0], allowed, action)
	case "down", "up":
		if len(args) != 1 {
			return fmt.Errorf("%s needs a link", command)
		}
		return lab.SetLinkDown(args[0], command == "down")
	case "nat":
		if len(args) != 2 {
			return errors.New("nat needs a router and a mode")
		}
		return lab.SetNAT(args[0], lab.NATMode(args[1]))
	case "forward":
		if len(args) != 3 {
			return errors.New("forward needs a router, a port and a host:port")
		}
		port, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("bad port %q", args[1])
		}
		_, err = lab.PortForward(args[0], port, args[2])
		return err
	case "clear":
		if len(args) == 0 {
			return lab.ClearNetwork()
		}
		return lab.ClearNode(args[0])
	case "list":
		fmt.Println("nodes:   ", strings.Join(lab.Nodes(), " "))
		fmt.Println("links:   ", strings.Join(lab.LinkNames(), " "))
		fmt.Println("profiles:")
		for _, name := range lab.ProfileNames() {
			fmt.Printf("  %-12s %s\n", name, lab.Profiles[name])
		}
		return nil
	}
	return fmt.Errorf("unknown net subcommand %q", command)
}

func parseShape(args []string) (lab.Shape, error) {
	if len(args) == 1 {
		if args[0] == "clear" {
			return lab.Shape{}, nil
		}
		if profile, ok := lab.Profiles[args[0]]; ok {
			return profile, nil
		}
	}
	var shape lab.Shape
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if !ok {
			return shape, fmt.Errorf("unknown profile or setting %q (profiles: %s)", arg, strings.Join(lab.ProfileNames(), ", "))
		}
		var err error
		switch key {
		case "delay":
			shape.Delay, err = time.ParseDuration(value)
		case "jitter":
			shape.Jitter, err = time.ParseDuration(value)
		case "loss":
			shape.Loss, err = strconv.ParseFloat(strings.TrimSuffix(value, "%"), 64)
		case "rate":
			shape.Rate = value
		default:
			err = fmt.Errorf("unknown key (want delay, jitter, loss or rate)")
		}
		if err != nil {
			return shape, fmt.Errorf("bad setting %q: %w", arg, err)
		}
	}
	return shape, nil
}

func parseBlock(args []string) (lab.Match, lab.Action, error) {
	match, action := lab.Match{}, lab.Drop
	host, port, hasPort := strings.Cut(args[0], ":")
	match.Dst = host
	if hasPort {
		number, err := strconv.Atoi(port)
		if err != nil {
			return match, action, fmt.Errorf("bad port in %q", args[0])
		}
		match.Port, match.Proto = number, "tcp"
	}
	for _, arg := range args[1:] {
		switch arg {
		case "tcp", "udp":
			match.Proto = arg
		default:
			action = lab.Action(arg)
		}
	}
	return match, action, nil
}
