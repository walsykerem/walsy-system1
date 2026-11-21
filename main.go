package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
)

// v1: stdio relay. Client spawns this binary as its MCP `type:local` command
// and passes the real upstream command in argv. Frames pass through verbatim;
// no SO call, no decision, no 503. One session metric line.
func main() {
	transport := flag.String("transport", "stdio", "transport: stdio|unix|http")
	listen := flag.String("listen", "", "unix socket path or http listen addr")
	flag.Parse()

	switch *transport {
	case "stdio":
		runStdio(flag.Args())
	case "unix":
		runUnix(*listen)
	case "http":
		runHTTP(*listen)
	default:
		fmt.Fprintln(os.Stderr, "transport must be stdio, unix or http")
		os.Exit(2)
	}
}

func runStdio(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: walsy-system1 [-transport stdio] <upstream MCP command> [args...]")
		os.Exit(2)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stderr = os.Stderr // upstream diagnostics are not secrets; stderr is our metric channel

	upIn, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "upstream stdin:", err)
		os.Exit(1)
	}
	upOut, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "upstream stdout:", err)
		os.Exit(1)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "upstream start:", err)
		os.Exit(1)
	}

	emitMetric(metric{Seq: 1, PID: os.Getpid(), Decision: "passthrough", Reason: "shadow", Passthrough: true})
	relayStdio(os.Stdin, os.Stdout, upIn, upOut)
	_ = cmd.Wait() // session ended: nothing to preserve, no retry storm
}

// unix/http modes have no upstream wiring yet: listener + relay only. With no
// upstream the relay is a transparent no-op — no 503, no synthetic error frame.
func runUnix(path string) {
	if path == "" {
		fmt.Fprintln(os.Stderr, "-listen unix socket path required")
		os.Exit(2)
	}
	l, err := listenUnix(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unix listen:", err)
		os.Exit(1)
	}
	emitMetric(metric{Seq: 1, PID: os.Getpid(), Decision: "passthrough", Reason: "shadow", Passthrough: true, ListenAddr: l.Addr().String()})
	serveUnix(l, nil)
}

func runHTTP(addr string) {
	l, err := listenHTTP(addr) // emits listen_addr metric with the OS-chosen port
	if err != nil {
		fmt.Fprintln(os.Stderr, "http listen:", err)
		os.Exit(1)
	}
	if err := serveHTTP(l); err != nil {
		fmt.Fprintln(os.Stderr, "http serve:", err)
	}
}
