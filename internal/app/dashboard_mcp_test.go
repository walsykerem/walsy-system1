package app

import (
	"net"
	"net/http"
	"testing"
	"time"
)

// The dashboard must come up with the MCP server and go down with it, and a
// port that is already served must not take the MCP server down with it.
func TestMCPDashboardComesUpWithTheTransport(t *testing.T) {
	port := freeLoopbackPort(t)
	startDashboardWithMCP(port)
	waitForDashboard(t, port)

	// A second start on the same port is a no-op: the first keeps serving.
	startDashboardWithMCP(port)
	waitForDashboard(t, port)
}

// freeLoopbackPort returns a loopback port that is free right now.
func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	return port
}

// waitForDashboard waits until the dashboard answers on the port.
func waitForDashboard(t *testing.T, port string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:" + port + "/")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("dashboard did not answer on 127.0.0.1:%s", port)
}
