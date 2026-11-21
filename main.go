package main

import (
	"fmt"
	"os"
	"os/exec"
)

// v1: shadow-mode stdio relay. Client spawns this binary as its MCP `type:local`
// command and passes the real upstream server command in argv. Frames pass
// through verbatim; no SO call, no decision, no 503. One session metric line.
func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: walsy-system1 <upstream MCP command> [args...]")
		os.Exit(2)
	}

	cmd := exec.Command(os.Args[1], os.Args[2:]...)
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

	emitMetric(metric{
		Seq:         1,
		PID:         os.Getpid(),
		Decision:    "passthrough",
		Reason:      "shadow",
		Passthrough: true,
	})

	relayStdio(os.Stdin, os.Stdout, upIn, upOut)
	_ = cmd.Wait() // session ended: nothing to preserve, no retry storm
}
