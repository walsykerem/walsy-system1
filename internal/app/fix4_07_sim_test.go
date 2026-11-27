package app

import (
	"bytes"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// These tests observe the shipped port contract without touching slice 06's
// transport_http.go: net.Listen is the only bind, the OS picks 127.0.0.1:0,
// and the chosen address goes to the stderr metric. No port++ retry exists.

func fix407BusyListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("loopback listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func fix407HostPort(t *testing.T, addr string) (string, string) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	return host, port
}

func fix407CaptureMetric(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	orig := metricOut
	metricOut = buf
	t.Cleanup(func() { metricOut = orig })
	return buf
}

// Red reference (silently swallow the bind error) fails the same contract the
// real listenHTTP passes: busy port => explicit error, no listener, no metric.
func TestFix407BusyPortExplicitFailureSimulation(t *testing.T) {
	held := fix407BusyListener(t)
	busy := held.Addr().String()
	contract := func(l net.Listener, err error) bool { return err != nil && l == nil }

	silentSuccess := func(addr string) (net.Listener, error) {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, nil // shortcut: models the rejected defect; never called by production
		}
		return l, nil
	}
	redListener, redErr := silentSuccess(busy)
	if contract(redListener, redErr) {
		t.Fatal("red reference passed the busy-port contract; simulation is not discriminating")
	}
	if redListener != nil {
		_ = redListener.Close()
	}

	buf := fix407CaptureMetric(t)
	got, err := listenHTTP(busy)
	if !contract(got, err) {
		t.Fatalf("busy explicit port: listener=%v err=%v, want explicit error and no listener", got, err)
	}
	if buf.Len() != 0 {
		t.Fatalf("failed bind emitted metric %q, want none", buf.String())
	}
}

func TestFix407EphemeralPortReportedAndReleased(t *testing.T) {
	t.Setenv("MCP_PROXY_PORT", "")
	buf := fix407CaptureMetric(t)
	l, err := listenHTTP("")
	if err != nil {
		t.Fatalf("ephemeral listen: %v", err)
	}
	addr := l.Addr().String()
	t.Cleanup(func() { _ = l.Close() })
	host, port := fix407HostPort(t, addr)
	if host != "127.0.0.1" {
		t.Fatalf("ephemeral host = %q, want 127.0.0.1", host)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 {
		t.Fatalf("ephemeral port = %q, want positive integer", port)
	}
	var rec struct {
		ListenAddr string `json:"listen_addr"`
		Seq        int    `json:"seq"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("metric line %q: %v", buf.String(), err)
	}
	if rec.ListenAddr != addr || rec.Seq != 1 {
		t.Fatalf("metric listen_addr=%q seq=%d, want %q and 1", rec.ListenAddr, rec.Seq, addr)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("rebind after cleanup at %s: %v", addr, err)
	}
	_ = again.Close()
}

func TestFix407ExplicitPortHasNoPlusOneFallback(t *testing.T) {
	held := fix407BusyListener(t)
	_, port := fix407HostPort(t, held.Addr().String())
	t.Setenv("MCP_PROXY_PORT", port)
	buf := fix407CaptureMetric(t)
	l, err := listenHTTP("")
	if l != nil {
		_ = l.Close()
		t.Fatal("busy MCP_PROXY_PORT silently returned a listener")
	}
	if err == nil {
		t.Fatal("busy MCP_PROXY_PORT returned no error, want explicit bind failure")
	}
	if buf.Len() != 0 {
		t.Fatalf("failed bind emitted metric %q, want none", buf.String())
	}
	n, _ := strconv.Atoi(port)
	if n >= 65535 {
		return
	}
	next, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(n+1)))
	if err != nil {
		t.Fatalf("port+1 was consumed by fallback: %v", err)
	}
	_ = next.Close()
}

func TestFix407PortRangeBounds(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:-1", "127.0.0.1:65536"} {
		l, err := listenHTTP(addr)
		if l != nil {
			_ = l.Close()
		}
		if err != errInvalidPort {
			t.Fatalf("listenHTTP(%q) = %v, want errInvalidPort", addr, err)
		}
	}
}

func TestFix407IPv6LoopbackBind(t *testing.T) {
	l, err := listenHTTP("[::1]:0")
	if err != nil {
		if strings.Contains(err.Error(), "cannot assign requested address") || strings.Contains(err.Error(), "address family not supported") {
			t.Skipf("IPv6 loopback unavailable: %v", err)
		}
		t.Fatalf("IPv6 loopback listen: %v", err)
	}
	defer l.Close()
	host, port := fix407HostPort(t, l.Addr().String())
	if host != "::1" {
		t.Fatalf("IPv6 host = %q, want ::1", host)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 {
		t.Fatalf("IPv6 port = %q, want positive integer", port)
	}
}

func TestFix407ConcurrentEphemeralBindsAreDistinct(t *testing.T) {
	t.Setenv("MCP_PROXY_PORT", "")
	const n = 8
	listeners := make([]net.Listener, n)
	addrs := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			listeners[i], errs[i] = listenHTTP("")
			if listeners[i] != nil {
				addrs[i] = listeners[i].Addr().String()
			}
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := range listeners {
		if errs[i] != nil {
			t.Errorf("concurrent bind %d: %v", i, errs[i])
			continue
		}
		if seen[addrs[i]] {
			t.Errorf("duplicate bind %q at %d; check-then-bind race", addrs[i], i)
		}
		seen[addrs[i]] = true
		_ = listeners[i].Close()
	}
}
