package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
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
	// One signal context owns shutdown for every server this process runs.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if *flagDashboardIn != "" && *flagTransport != "dashboard" {
		startDashboardWithMCP(ctx, *flagDashboardIn)
	}

	switch *flagTransport {
	case "stdio":
		runStdio(flag.Args())
	case "unix":
		runUntilDone(ctx, func() { runUnix(*flagListen, *flagUpstream) })
	case "http":
		runUntilDone(ctx, func() { runHTTP(*flagListen, *flagUpstream) })
	case "dashboard":
		runUntilDone(ctx, func() {
			if err := RunDashboard(*flagListen); err != nil {
				fmt.Fprintln(os.Stderr, "dashboard:", err)
				os.Exit(1)
			}
		})
	case "tools":
		runUntilDone(ctx, func() { runToolsServer(os.Stdin, os.Stdout) })
	default:
		fmt.Fprintln(os.Stderr, "transport must be stdio, unix, http, dashboard or tools")
		os.Exit(2)
	}
}

// runUntilDone runs a blocking server and returns once it exits or ctx is done.
// Returning from Main ends the process, so a signal stops every server at once.
func runUntilDone(ctx context.Context, run func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		run()
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func runStdio(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: walsy-system1 [-transport stdio] <upstream MCP command> [args...]")
		os.Exit(2)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A signal cancels the same context as a vanished client stream, so one
	// upstream lifecycle rule covers both: the child is always reaped.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	go func() {
		select {
		case <-sigCtx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
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
	clientGone, relayed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(relayed)
		relayStdio(&notifyEOF{r: os.Stdin, gone: clientGone}, os.Stdout, upIn, upOut)
	}()
	select {
	case <-relayed:
	case <-sigCtx.Done():
		cancel()
	case <-clientGone:
		// stdin is half-closed: a well-behaved child flushes and exits. Only
		// kill it if it still holds the relay after the drain window.
		select {
		case <-relayed:
		case <-sigCtx.Done():
			cancel()
		case <-time.After(upstreamDrainGrace):
			cancel()
		}
	}
	// No upstream may outlive the relay that spawned it.
	cancel()
	// Record the upstream exit in the session metric boolean field; never as a secret.
	if err := cmd.Wait(); err != nil {
		emitMetric(metric{Seq: 2, PID: os.Getpid(), UpstreamExit: "error", Decision: "passthrough"})
	}
}

const upstreamDrainGrace = 2 * time.Second

// notifyEOF marks a vanished client stream without killing the upstream: the
// relay half-closes stdin first so a child can flush its last reply.
type notifyEOF struct {
	r    io.Reader
	gone chan struct{}
	once sync.Once
}

func (c *notifyEOF) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err != nil {
		c.once.Do(func() { close(c.gone) })
	}
	return n, err
}

// prefixWriter tags each upstream stderr Write so it cannot be mistaken for a
// session metric line (which is always one JSON object starting with '{').
type prefixWriter struct {
	w      interface{ Write([]byte) (int, error) }
	prefix []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	// Prefix every line, not just the first: a multi-line chunk would otherwise
	// leave unprefixed lines an upstream could pass off as metric records.
	out := make([]byte, 0, len(b)+len(p.prefix))
	out = append(out, p.prefix...)
	for i, c := range b {
		out = append(out, c)
		if c == '\n' && i < len(b)-1 {
			out = append(out, p.prefix...)
		}
	}
	if _, err := p.w.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
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
