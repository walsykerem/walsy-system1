package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// mcpProtocolVersionHeader is the single home of the Streamable HTTP header
// name. No SSE: responses are one JSON body, never text/event-stream.
const mcpProtocolVersionHeader = "MCP-Protocol-Version"

// SEP-2243 method/name headers: when present they must match the JSON-RPC
// envelope, and the server mirrors them back on the response.
const (
	mcpMethodHeader = "Mcp-Method"
	mcpNameHeader   = "Mcp-Name"
)

// maxHTTPFrame bounds request bodies. Oversized bodies get HTTP 413 before
// being read into memory.
const maxHTTPFrame = 8 << 20 // 8 MiB

// readHeaderTimeout is the only timeout that touches the first request: it
// bounds the headers, never the response. Body/Write/Idle bounds stop a slow
// client pinning a connection; no latency is added to a normal request.
const (
	httpReadHeaderTimeout = 5 * time.Second
	httpReadTimeout       = 30 * time.Second
	httpWriteTimeout      = 30 * time.Second
	httpIdleTimeout       = 60 * time.Second
)

// listenHTTP binds loopback only. Empty addr means MCP_PROXY_PORT override,
// else 127.0.0.1:0 (OS picks a free port). No port++ retry, no SSE, no
// discovery file. The chosen address is reported in the existing stderr metric.
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
		return nil, errNonLoopback
	}
	if port != "0" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return nil, errInvalidPort
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

var (
	errNonLoopback = &httpListenError{"http listen host must be loopback"}
	errInvalidPort = &httpListenError{"invalid http listen port"}
)

type httpListenError struct{ s string }

func (e *httpListenError) Error() string { return e.s }

// setMCPProtocolVersion echoes the client's version on the response - the
// only place that touches the header. Absent header means absent response header.
func setMCPProtocolVersion(w http.ResponseWriter, r *http.Request) {
	if v := r.Header.Get(mcpProtocolVersionHeader); v != "" {
		w.Header().Set(mcpProtocolVersionHeader, v)
	}
}

// originAllowed permits clients without Origin (non-browser) and browser
// origins from loopback only. A mismatched browser origin is rejected with 403.
func originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

// sep2243Valid checks optional Mcp-Method/Mcp-Name headers against the frame.
func sep2243Valid(r *http.Request, f frame) bool {
	if v := r.Header.Get(mcpMethodHeader); v != "" && v != f.Method {
		return false
	}
	if v := r.Header.Get(mcpNameHeader); v != "" && v != toolName(f.Params) {
		return false
	}
	return true
}

func mirrorSEP2243(w http.ResponseWriter, r *http.Request) {
	for _, h := range []string{mcpMethodHeader, mcpNameHeader} {
		if v := r.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
}

var upstreamHTTPClient = &http.Client{Timeout: httpReadTimeout}

type proxyResult struct {
	status  int
	body    []byte
	headers http.Header
	failed  bool
}

// requestID reads the JSON-RPC id from a frame so an error reply can echo it
// instead of answering every failure with id:null.
func requestID(body []byte) json.RawMessage {
	var f struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(body, &f) != nil {
		return nil
	}
	return f.ID
}

func proxyHTTP(ctx context.Context, upstream string, body []byte, r *http.Request) proxyResult {
	fail := proxyResult{status: http.StatusOK, body: rpcError(requestID(body), -32000, "upstream unavailable"), failed: true}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream, bytes.NewReader(body))
	if err != nil {
		return fail
	}
	req.Header.Set("Content-Type", "application/json")
	for _, h := range []string{mcpProtocolVersionHeader, mcpMethodHeader, mcpNameHeader} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := upstreamHTTPClient.Do(req)
	if err != nil {
		return fail
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPFrame+1))
	if err != nil {
		return fail
	}
	// maxHTTPFrame+1 tells a full-cap body from a truncated one: only a body
	// past the cap was cut by LimitReader and needs an error frame.
	if int64(len(out)) > maxHTTPFrame {
		return proxyResult{status: http.StatusOK,
			body: rpcError(requestID(body), -32001, "upstream response too large"), failed: true}
	}
	// A 304 has no JSON-RPC body: forwarding it would hand the frontend an
	// empty reply with stale validators attached.
	if resp.StatusCode == http.StatusNotModified {
		return proxyResult{status: http.StatusOK,
			body: rpcError(requestID(body), -32002, "upstream conditional response"), failed: true}
	}
	return proxyResult{status: resp.StatusCode, body: out, headers: resp.Header}
}

func (d *decisionBridge) advisoryHTTP(request []byte, f frame) {
	r, _ := d.decideFrameDetailed(request)
	d.advisoryHooks(request)
	recordDecisionReason(r.Reason)
	// Outbound (server -> client) mirror: the HTTP response is already written
	// and flushed, so chain the decision for this direction without delaying it.
	det := receiptDetails{Event: "outbound_response", Force: true,
		FrameID: safeReceiptField(receiptFrameID(f)),
		TS:      time.Now().UTC().Format(time.RFC3339)}
	d.emitReceiptDetailed(r, "response", "", det)
}

// mcpHTTPHandler is the Streamable HTTP JSON-RPC endpoint. With no upstream it
// echoes frames byte-for-byte; with an upstream it proxies. Either way advisory
// decisions run after response flush, never blocking or logging payloads.
func mcpHTTPHandler() http.Handler { return mcpHTTPHandlerUpstream("") }

// writeHTTPJSONError keeps the JSON-RPC endpoint's content type and no-store
// policy consistent on rejection paths too.
func writeHTTPJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func mcpHTTPHandlerUpstream(upstream string) http.Handler {
	d := newDecisionBridge()
	enforcing := soMode() == "enforcing"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		// Reply bodies are state snapshots: no heuristic cache may reuse them,
		// including on the error paths below.
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !originAllowed(r) {
			writeHTTPJSONError(w, http.StatusForbidden, "origin forbidden")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxHTTPFrame)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeHTTPJSONError(w, http.StatusRequestEntityTooLarge, "request too large")
				return
			}
			// A truncated or aborted body used to return without writing a
			// status, which the net/http server answers as 200 with no frame.
			writeHTTPJSONError(w, http.StatusBadRequest, "request unreadable")
			return
		}
		relayLog("client", body)
		f, derr := decodeFrame(body)
		if derr != nil {
			writeHTTPJSONError(w, http.StatusBadRequest, "invalid JSON-RPC")
			return
		}
		if !sep2243Valid(r, f) {
			writeHTTPJSONError(w, http.StatusBadRequest, "invalid MCP headers")
			return
		}

		if enforcing {
			if resp, blocked := d.enforcingGate(body); blocked {
				recordDenial("http-enforcing")
				setMCPProtocolVersion(w, r)
				mirrorSEP2243(w, r)
				// Streamable HTTP: a gated notification (no id, no reply) is a 202
				// with no body. Answering 200 + empty JSON body is unparseable.
				if len(resp) == 0 {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Content-Type-Options", "nosniff")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(resp)
				return
			}
			// E1: promotion preconditions are checked after the single decide
			// POST, so capability refusals keep their typed message and a
			// blocked SO still holds the reply. Non-decision frames (initialize)
			// are not promotion-gated.
			if f.Method == "tools/call" && !promotionPreconditionsMet() {
				routerAudit("gate", "reason", "promotion-missing", "status", "denied")
				recordDenial("promotion-missing")
				setMCPProtocolVersion(w, r)
				mirrorSEP2243(w, r)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Content-Type-Options", "nosniff")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(rpcError(f.ID, -32001, "blocked: promotion preconditions missing"))
				return
			}
		}

		out := proxyResult{status: http.StatusOK, body: body}
		if upstream != "" {
			out = proxyHTTP(r.Context(), upstream, body, r)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		setMCPProtocolVersion(w, r)
		mirrorSEP2243(w, r)
		for _, h := range []string{mcpProtocolVersionHeader, mcpMethodHeader, mcpNameHeader} {
			if v := out.headers.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		// FA14.1: decision outcome headers. Advisory SO runs after Flush, so
		// its result is unavailable here; report mode/elapsed and unknown
		// cache/confidence rather than a fabricated raw confidence number.
		w.Header().Set("X-Decision-Backend", soMode())
		w.Header().Set("X-Decision-Cache", "unknown")
		w.Header().Set("X-Decision-Confidence", "unknown")
		w.Header().Set("X-Decision-Elapsed-MS", strconv.FormatInt(time.Since(started).Milliseconds(), 10))
		w.WriteHeader(out.status)
		relayLog("server", out.body)
		_, _ = w.Write(out.body)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		// Advisory SO runs in a worker after the body is flushed. Keep the
		// handler alive until it finishes, but cap the wait so a slow/blocked
		// SO never holds the client past the deadline budget (F5/T2/N2.2).
		// No Content-Length: net/http keeps the chunked body open until return,
		// so callers reading to EOF observe the bounded advisory window.
		if !enforcing && !out.failed {
			done := make(chan struct{})
			go func() {
				d.advisoryHTTP(body, f)
				close(done)
			}()
			// shortcut: 100ms observation window for advisory SO; raise only
			// if a slow SO must finish before the client reply is sent.
			select {
			case <-done:
			case <-time.After(100 * time.Millisecond):
			}
		}
	})
}

// serveHTTP runs the loopback HTTP server with explicit timeouts so a slow
// client cannot pin a connection forever. Optional upstream enables proxy mode;
// empty keeps the legacy echo endpoint used by existing tests.
func serveHTTP(l net.Listener, upstream ...string) error {
	handler := mcpHTTPHandler()
	if len(upstream) > 0 && upstream[0] != "" {
		handler = mcpHTTPHandlerUpstream(upstream[0])
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
	return srv.Serve(l)
}
