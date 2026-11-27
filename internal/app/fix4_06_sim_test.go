package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Frontend-visible HTTP cache behavior: the JSON-RPC endpoint must never hand
// a browser a heuristically cacheable or stale reply, and a failed upstream
// exchange must not leave a cached decision behind.

func fix406Frame() []byte {
	return []byte(`{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"x"}}`)
}

func fix406Upstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func fix406Client() *http.Client { return &http.Client{Timeout: 3 * time.Second} }

func fix406Post(t *testing.T, url string, hdr http.Header, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := fix406Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// Every reply on the JSON-RPC endpoint - success or error - is a state
// snapshot for the frontend. No heuristic cache may reuse it.
func TestFix406NoStoreAndJSONContentOnAllResponses(t *testing.T) {
	up := fix406Upstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fix406Frame())
	})
	srv := fix406Upstream(t, http.HandlerFunc(mcpHTTPHandlerUpstream(up.URL).ServeHTTP))

	resp := fix406Post(t, srv.URL, nil, fix406Frame())
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("success Cache-Control = %q, want no-store", got)
	}
	b, _ := io.ReadAll(resp.Body)
	if !json.Valid(b) {
		t.Fatalf("success body is not JSON: %q", b)
	}

	checkErr := func(resp *http.Response, wantStatus int, want string) {
		t.Helper()
		if resp.StatusCode != wantStatus {
			t.Fatalf("status = %d, want %d", resp.StatusCode, wantStatus)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("error Cache-Control = %q, want no-store", got)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("error Content-Type = %q, want application/json", ct)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("nosniff = %q", got)
		}
		b, _ := io.ReadAll(resp.Body)
		if !json.Valid(b) || !strings.Contains(string(b), want) {
			t.Fatalf("error body = %q, want JSON containing %q", b, want)
		}
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := fix406Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", r.StatusCode)
	}
	if got := r.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("GET Cache-Control = %q, want no-store", got)
	}
	if got := r.Header.Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow = %q", got)
	}

	checkErr(fix406Post(t, srv.URL, http.Header{"Origin": {"https://evil.example"}}, fix406Frame()),
		http.StatusForbidden, "origin forbidden")
	checkErr(fix406Post(t, srv.URL, nil, []byte(`{`)), http.StatusBadRequest, "invalid JSON-RPC")
	checkErr(fix406Post(t, srv.URL, http.Header{mcpMethodHeader: {"tools/list"}}, fix406Frame()),
		http.StatusBadRequest, "invalid MCP headers")

	rec := httptest.NewRecorder()
	bad := httptest.NewRequest(http.MethodPost, "/", nil)
	bad.Body = io.NopCloser(failingReader{})
	mcpHTTPHandlerUpstream(up.URL).ServeHTTP(rec, bad)
	checkErr(rec.Result(), http.StatusBadRequest, "request unreadable")
}

// An upstream 304 is not a usable JSON-RPC reply. It must never reach the
// frontend (empty body with 304), and it must not carry a stale validator.
func TestFix406Upstream304NeverReachesFrontend(t *testing.T) {
	up := fix406Upstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"stale-upstream"`)
		w.Header().Set("Last-Modified", "Thu, 01 Jan 1970 00:00:00 GMT")
		w.WriteHeader(http.StatusNotModified)
	})
	srv := fix406Upstream(t, http.HandlerFunc(mcpHTTPHandlerUpstream(up.URL).ServeHTTP))

	resp := fix406Post(t, srv.URL, nil, fix406Frame())
	if resp.StatusCode == http.StatusNotModified {
		t.Fatal("upstream 304 leaked to frontend")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an error frame", resp.StatusCode)
	}
	if got := resp.Header.Get("ETag"); got != "" {
		t.Fatalf("stale ETag leaked: %q", got)
	}
	if got := resp.Header.Get("Last-Modified"); got != "" {
		t.Fatalf("stale Last-Modified leaked: %q", got)
	}
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &out) != nil || string(out.ID) != "41" || out.Error.Code == 0 {
		t.Fatalf("want a JSON-RPC error frame echoing id 41, got %q", b)
	}
}

// An error frame must not advertise validators or SEP-2243 metadata from an
// oversized upstream reply: those headers describe a body we discarded.
func TestFix406OversizedErrorCarriesNoUpstreamHeaders(t *testing.T) {
	up := fix406Upstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"too-large"`)
		w.Header().Set(mcpMethodHeader, "tools/call")
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxHTTPFrame+1))
	})
	srv := fix406Upstream(t, http.HandlerFunc(mcpHTTPHandlerUpstream(up.URL).ServeHTTP))

	resp := fix406Post(t, srv.URL, nil, fix406Frame())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, h := range []string{"ETag", mcpMethodHeader, mcpNameHeader} {
		if got := resp.Header.Get(h); got != "" {
			t.Fatalf("%s leaked on error frame: %q", h, got)
		}
	}
	b, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(b, []byte("upstream response too large")) {
		t.Fatalf("error body = %q", b)
	}
}

// A failed exchange never executed the tool, so advisory SO must not run for
// it: no decision is cached or chained. Audit records are the observable: a
// healthy exchange on the same home writes them, a failed one must write none.
func TestFix406FailedProxyDoesNotWriteDecision(t *testing.T) {
	resetRouterForTest(t)
	t.Setenv("WALSY_SO_MODE", soModeAdvisory)
	t.Setenv("WALSY_SO_ENDPOINT", "")
	audit := filepath.Join(os.Getenv("HOME"), routerDir, routerAuditName)
	countAudit := func() int {
		t.Helper()
		b, err := os.ReadFile(audit)
		if err != nil {
			if os.IsNotExist(err) {
				return 0
			}
			t.Fatal(err)
		}
		// relay_frame audit lines are transport logs; only the gate/canary
		// records are written by advisory decision handling.
		return bytes.Count(b, []byte(`"reason":"cap-missing-warning"`))
	}

	healthy := fix406Upstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":41,"result":{"ok":true}}`))
	})
	good := fix406Upstream(t, http.HandlerFunc(mcpHTTPHandlerUpstream(healthy.URL).ServeHTTP))
	resp := fix406Post(t, good.URL, nil, fix406Frame())
	_, _ = io.Copy(io.Discard, resp.Body)
	baseline := countAudit()
	if baseline == 0 {
		t.Fatal("no advisory audit for a healthy exchange; observer is not working")
	}

	dead := fix406Upstream(t, func(w http.ResponseWriter, r *http.Request) {})
	dead.Close()
	broken := fix406Upstream(t, http.HandlerFunc(mcpHTTPHandlerUpstream(dead.URL).ServeHTTP))
	resp = fix406Post(t, broken.URL, nil, fix406Frame())
	b, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(b, []byte("upstream unavailable")) {
		t.Fatalf("expected an upstream failure frame, got %q", b)
	}
	if got := countAudit(); got != baseline {
		t.Fatalf("advisory ran on failed exchange: gate audit lines %d -> %d", baseline, got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("failure Cache-Control = %q", got)
	}
}

// The fix3 cap decision must stay intact: exactly maxHTTPFrame is complete.
func TestFix406ExactCapStillForwarded(t *testing.T) {
	body := bytes.Repeat([]byte("x"), maxHTTPFrame)
	up := fix406Upstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})
	srv := fix406Upstream(t, http.HandlerFunc(mcpHTTPHandlerUpstream(up.URL).ServeHTTP))
	resp := fix406Post(t, srv.URL, nil, fix406Frame())
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("exact-cap reply = %d bytes, want %d verbatim", len(got), maxHTTPFrame)
	}
}
