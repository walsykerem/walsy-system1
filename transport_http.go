package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
)

// mcpProtocolVersionHeader is the single home of the Streamable HTTP header
// name. No SSE anywhere: responses are one JSON body, never text/event-stream.
const mcpProtocolVersionHeader = "MCP-Protocol-Version"

const maxHTTPFrame = 1 << 20

// listenHTTP binds loopback only. Empty addr ⇒ MCP_PROXY_PORT override, else
// 127.0.0.1:0 (OS picks a free port). No port++ retry, no SO_REUSEPORT, no
// discovery file; the chosen address goes to the existing stderr metric line.
func listenHTTP(addr string) (net.Listener, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
		if p := os.Getenv("MCP_PROXY_PORT"); p != "" {
			addr = "127.0.0.1:" + p
		}
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("http listen host must be loopback")
	}
	if port != "0" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("invalid http listen port")
		}
	}
	l, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, err
	}
	emitMetric(metric{
		Seq:         1,
		PID:         os.Getpid(),
		Decision:    "passthrough",
		Reason:      "shadow",
		Passthrough: true,
		ListenAddr:  l.Addr().String(),
	})
	return l, nil
}

// setMCPProtocolVersion echoes the client's version on the response — the only
// place that touches the header. Absent header ⇒ absent response header.
func setMCPProtocolVersion(w http.ResponseWriter, r *http.Request) {
	if v := r.Header.Get(mcpProtocolVersionHeader); v != "" {
		w.Header().Set(mcpProtocolVersionHeader, v)
	}
}

// mcpHTTPHandler is the Streamable HTTP JSON-RPC endpoint. With no upstream
// configured the relay is a transparent no-op: the frame is echoed byte-for-byte,
// never replaced by a synthetic error or a 503. GET is rejected (no SSE).
func mcpHTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxHTTPFrame))
		if err != nil {
			return // client sees no response; never a synthetic error frame
		}
		setMCPProtocolVersion(w, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body) // verbatim passthrough
	})
}

func serveHTTP(l net.Listener) error {
	return http.Serve(l, mcpHTTPHandler())
}
