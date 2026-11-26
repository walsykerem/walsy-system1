package app

import (
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
)

// v1: stdio relay. Client spawns this binary as its MCP `type:local` command
// and passes the real upstream command in argv. Frames pass through verbatim;
// the advisory decision engine never breaks the stream and never returns 503.
// Flags are package-level so flag.Lookup sees them at init time (acceptance).
var (
	flagTransport   = flag.String("transport", "stdio", "transport: stdio|unix|http|dashboard|tools")
	flagListen      = flag.String("listen", "", "unix socket path or http listen addr")
	flagUpstream    = flag.String("upstream", "", "upstream unix socket path (unix) or http(s) URL (http)")
	flagPrintConfig = flag.Bool("print-client-config", false, "print OpenCode MCP client config and exit")
	flagDashboard   = flag.Bool("dashboard", false, "run loopback combo dashboard")
	flagDashboardIn = flag.String("dashboard-listen", "", "also serve the loopback dashboard on this port, so it comes up with the MCP server")
)

// Main is the process entry point; cmd/walsy-system1 only calls this.
func Main() {
	flag.Parse()
	if *flagPrintConfig {
		fmt.Println(opencodeMCPSnippet("walsy-system1", *flagTransport, *flagListen, *flagUpstream, flag.Args()))
		return
	}
	if *flagDashboard {
		*flagTransport = "dashboard" // -dashboard is an alias for -transport dashboard
	}
	if err := soModeStartupError(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// The dashboard rides along with the MCP server instead of a boot agent:
	// it comes up when the client starts the transport and goes down with it.
	if *flagDashboardIn != "" && *flagTransport != "dashboard" {
		startDashboardWithMCP(*flagDashboardIn)
	}

	switch *flagTransport {
	case "stdio":
		runStdio(flag.Args())
	case "unix":
		runUnix(*flagListen, *flagUpstream)
	case "http":
		runHTTP(*flagListen, *flagUpstream)
	case "dashboard":
		if err := RunDashboard(*flagListen); err != nil {
			fmt.Fprintln(os.Stderr, "dashboard:", err)
			os.Exit(1)
		}
	case "tools":
		runToolsServer(os.Stdin, os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, "transport must be stdio, unix, http, dashboard or tools")
		os.Exit(2)
	}
}

func runStdio(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: walsy-system1 [-transport stdio] <upstream MCP command> [args...]")
		os.Exit(2)
	}
	cmd := exec.Command(args[0], args[1:]...)
	// Upstream stderr shares the metric channel; give it a distinct prefix the
	// upstream cannot forge as a session metric (metric lines start with '{').
	cmd.Stderr = &prefixWriter{w: os.Stderr, prefix: []byte("upstream: ")}

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
	// Record the upstream exit in the session metric boolean field; never as a secret.
	if err := cmd.Wait(); err != nil {
		emitMetric(metric{Seq: 2, PID: os.Getpid(), UpstreamExit: "error", Decision: "passthrough"})
	}
}

// prefixWriter tags each upstream stderr Write so it cannot be mistaken for a
// session metric line (which is always one JSON object starting with '{').
type prefixWriter struct {
	w      interface{ Write([]byte) (int, error) }
	prefix []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	_, _ = p.w.Write(p.prefix)
	return p.w.Write(b)
}

// runUnix listens on a loopback unix socket and, when -upstream is given, dials
// one upstream unix connection per client. No upstream keeps the legacy
// transparent no-op: no 503 and no synthetic error frame.
func runUnix(path, upstream string) {
	if path == "" {
		fmt.Fprintln(os.Stderr, "-listen unix socket path required")
		os.Exit(2)
	}
	l, err := listenUnix(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unix listen:", err)
		os.Exit(1)
	}
	var dial func() (net.Conn, error)
	if upstream != "" {
		dial = func() (net.Conn, error) { return net.Dial("unix", upstream) }
	}
	emitMetric(metric{Seq: 1, PID: os.Getpid(), Decision: "passthrough", Reason: "shadow", Passthrough: true, ListenAddr: l.Addr().String()})
	serveUnix(l, dial)
}

func runHTTP(addr, upstream string) {
	if upstream != "" {
		u, err := url.Parse(upstream)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fmt.Fprintln(os.Stderr, "-upstream must be an http(s) URL")
			os.Exit(2)
		}
	}
	l, err := listenHTTP(addr) // emits listen_addr metric with the OS-chosen port
	if err != nil {
		fmt.Fprintln(os.Stderr, "http listen:", err)
		os.Exit(1)
	}
	if err := serveHTTP(l, upstream); err != nil {
		fmt.Fprintln(os.Stderr, "http serve:", err)
	}
}
