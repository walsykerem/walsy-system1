package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// W18 simulation: MANIFEST T3 and M1. Go test entry points must start with
// "Test", so they are Test_w18_*; every other package-level identifier here
// carries the w18_ prefix to stay clear of sibling simulation files.
// Traffic goes only to a local httptest SO stub. No real key, endpoint,
// commit, push, restart or repo file is touched.

const w18Caps = `"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"`

const w18YesReply = `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`

type w18Read struct {
	b   []byte
	n   int
	err error
}

func w18Base(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("WALSY_ROUTER_DENY", "")
	routerReset()
	t.Cleanup(routerReset)
	return home
}

func w18Frame(id int, method, params string) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{%s}}`+"\n", id, method, params))
}

type w18SO struct {
	srv      *httptest.Server
	entered  chan struct{}
	release  chan struct{}
	enterOne sync.Once
	freeOne  sync.Once
	posts    atomic.Int32
}

func w18NewSO(t *testing.T, reply string) *w18SO {
	t.Helper()
	s := &w18SO{entered: make(chan struct{}), release: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.posts.Add(1)
		s.enterOne.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-time.After(6 * time.Second): // fail-safe: never leak a blocked handler
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(s.srv.Close)
	t.Cleanup(s.unblock)
	return s
}

func (s *w18SO) attach(t *testing.T) {
	t.Helper()
	oldEP, oldClient := soEndpoint, soClient
	soEndpoint, soClient = s.srv.URL, s.srv.Client()
	t.Cleanup(func() { soEndpoint, soClient = oldEP, oldClient })
}

func (s *w18SO) unblock() { s.freeOne.Do(func() { close(s.release) }) }

func (s *w18SO) waitEntered(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-s.entered:
	case <-time.After(d):
		t.Fatal("no SO POST reached the stub: gate not exercised")
	}
}

type w18Metrics struct {
	mu       sync.Mutex
	lines    []string
	markerAt time.Time
	marker   chan struct{}
	one      sync.Once
}

func w18AttachMetrics(t *testing.T) *w18Metrics {
	t.Helper()
	m := &w18Metrics{marker: make(chan struct{})}
	old := metricOut
	metricOut = m
	t.Cleanup(func() { metricOut = old })
	return m
}

func (m *w18Metrics) Write(p []byte) (int, error) {
	m.mu.Lock()
	m.lines = append(m.lines, string(p))
	if bytes.Contains(p, []byte(`"route":"`+markerKey+`"`)) && bytes.Contains(p, []byte(`"reason_code":"key_set"`)) {
		if m.markerAt.IsZero() {
			m.markerAt = time.Now()
		}
		m.one.Do(func() { close(m.marker) })
	}
	m.mu.Unlock()
	return len(p), nil
}

func (m *w18Metrics) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, line := range m.lines {
		if strings.Contains(line, `"route":"`+markerKey+`"`) && strings.Contains(line, `"reason_code":"key_set"`) {
			n++
		}
	}
	return n
}

func (m *w18Metrics) at() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.markerAt
}

func (m *w18Metrics) waitMarker(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-m.marker:
	case <-time.After(d):
		t.Fatal("advisory decision marker was never emitted")
	}
}

// w18FlushWriter observes the real handler's Flush without modifying it.
type w18FlushWriter struct {
	http.ResponseWriter
	one       *sync.Once
	flushed   chan time.Time
	flushedAt time.Time
}

func (w *w18FlushWriter) Flush() {
	w.one.Do(func() {
		w.flushedAt = time.Now()
		select {
		case w.flushed <- w.flushedAt:
		default:
		}
	})
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type w18SignalWriter struct {
	io.Writer
	one    *sync.Once
	signal chan struct{}
	at     time.Time
}

func (w *w18SignalWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 {
		w.one.Do(func() {
			w.at = time.Now()
			close(w.signal)
		})
	}
	return n, err
}

func w18Post(t *testing.T, srv *httptest.Server, frame []byte) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("build POST: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST frame: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, b
}

// T3, HTTP path. A blocking SO stub makes pre-forward marker placement
// observable: the client would get no response byte until SO answered.
func Test_w18_T3_HTTPAdvisoryMarkerAfterFlush(t *testing.T) {
	w18Base(t)
	t.Setenv("EVREN_API_KEY", "w18-http-key")
	so := w18NewSO(t, w18YesReply)
	so.attach(t)
	metrics := w18AttachMetrics(t)

	flushed := make(chan time.Time, 1)
	var flushOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fw := &w18FlushWriter{ResponseWriter: w, one: &flushOnce, flushed: flushed}
		mcpHTTPHandler().ServeHTTP(fw, r)
	}))
	defer srv.Close()

	frame := w18Frame(1, "initialize", `"name":"w18",`+w18Caps)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("build POST: %v", err)
	}
	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, e := srv.Client().Do(req)
		if e != nil {
			errCh <- e
			return
		}
		respCh <- resp
	}()
	var resp *http.Response
	select {
	case resp = <-respCh:
	case e := <-errCh:
		t.Fatalf("POST frame: %v", e)
	case <-time.After(4 * time.Second):
		t.Fatal("no HTTP response headers while SO was pending")
	}
	defer resp.Body.Close()

	first := make(chan w18Read, 1)
	go func() {
		b := make([]byte, len(frame))
		n, e := resp.Body.Read(b)
		first <- w18Read{b: b, n: n, err: e}
	}()
	var got w18Read
	select {
	case got = <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("client response withheld until SO answered: marker is pre-forward")
	}
	if got.err != nil || got.n <= 0 {
		t.Fatalf("first response read: n=%d err=%v", got.n, got.err)
	}
	if !bytes.Equal(got.b[:got.n], frame[:got.n]) {
		t.Fatalf("first bytes %q, want echo prefix %q", got.b[:got.n], frame[:got.n])
	}
	var flushAt time.Time
	select {
	case flushAt = <-flushed:
	case <-time.After(time.Second):
		t.Fatal("handler never flushed the response")
	}
	so.waitEntered(t, 3*time.Second)
	if n := so.posts.Load(); n != 1 {
		t.Fatalf("SO posts = %d, want 1", n)
	}

	// Still blocked here: a byte reached the client before any decision.
	so.unblock()
	metrics.waitMarker(t, 3*time.Second)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read rest: %v", err)
	}
	body := append(append([]byte{}, got.b[:got.n]...), rest...)
	if !bytes.Equal(body, frame) {
		t.Fatalf("response %q, want exact echo %q", body, frame)
	}
	if n := metrics.count(); n != 1 {
		t.Fatalf("decision markers = %d, want 1", n)
	}
	if metrics.at().Before(flushAt) {
		t.Fatalf("marker at %v precedes Flush at %v", metrics.at(), flushAt)
	}
}

// T3, stdio/unix path: real bridgeStdio must forward the frame before the
// advisory decision, with the marker after that forward.
func Test_w18_T3_StdioAdvisoryForwardFirst(t *testing.T) {
	w18Base(t)
	t.Setenv("EVREN_API_KEY", "w18-stdio-key")
	so := w18NewSO(t, w18YesReply)
	so.attach(t)
	metrics := w18AttachMetrics(t)

	clientInR, clientInW := io.Pipe()
	upInR, upInW := io.Pipe()
	upOutR, upOutW := io.Pipe()
	t.Cleanup(func() {
		_ = clientInW.Close()
		_ = upOutW.Close()
		_ = upInR.Close()
	})
	forwarded := make(chan struct{})
	var forwardOnce sync.Once
	upSignal := &w18SignalWriter{Writer: upInW, one: &forwardOnce, signal: forwarded}
	done := make(chan struct{})
	var clientOut bytes.Buffer // advisory mode writes no reply here
	go func() {
		bridgeStdio(clientInR, &clientOut, upSignal, upOutR)
		close(done)
	}()

	frame := w18Frame(2, "initialize", `"name":"w18",`+w18Caps)
	if _, err := clientInW.Write(frame); err != nil {
		t.Fatalf("client write: %v", err)
	}
	readCh := make(chan w18Read, 1)
	go func() {
		b := make([]byte, len(frame))
		n, e := upInR.Read(b)
		readCh <- w18Read{b: b, n: n, err: e}
	}()
	var passed w18Read
	select {
	case passed = <-readCh:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream got no frame while SO was pending: decision is pre-forward")
	}
	if passed.err != nil || passed.n <= 0 || !bytes.Equal(passed.b[:passed.n], frame[:passed.n]) {
		t.Fatalf("forwarded bytes n=%d err=%v payload=%q", passed.n, passed.err, passed.b)
	}
	select {
	case <-forwarded:
	case <-time.After(time.Second):
		t.Fatal("upstream write signal missing")
	}
	so.waitEntered(t, 3*time.Second)
	so.unblock()
	_ = clientInW.Close()
	_ = upOutW.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bridgeStdio did not return after both sides closed")
	}
	metrics.waitMarker(t, 3*time.Second)
	if n := so.posts.Load(); n != 1 {
		t.Fatalf("SO posts = %d, want 1", n)
	}
	if n := metrics.count(); n != 1 {
		t.Fatalf("decision markers = %d, want 1", n)
	}
	if metrics.at().Before(upSignal.at) {
		t.Fatalf("marker at %v precedes upstream forward at %v", metrics.at(), upSignal.at)
	}
}

// T3 control: enforcing mode must hold the reply until SO answers. This is
// what the advisory path deliberately does not do.
func Test_w18_T3_EnforcingPreForwardContrast(t *testing.T) {
	w18Base(t)
	t.Setenv("WALSY_SO_MODE", "enforcing")
	t.Setenv("EVREN_API_KEY", "w18-enforcing-key")
	so := w18NewSO(t, w18YesReply)
	so.attach(t)
	metrics := w18AttachMetrics(t)
	srv := httptest.NewServer(mcpHTTPHandler())
	defer srv.Close()

	frame := w18Frame(3, "initialize", `"name":"w18",`+w18Caps)
	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := srv.Client().Post(srv.URL, "application/json", bytes.NewReader(frame))
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()
	select {
	case <-respCh:
		t.Fatal("enforcing reply arrived before SO answered")
	case e := <-errCh:
		t.Fatalf("POST frame: %v", e)
	case <-time.After(600 * time.Millisecond):
	}
	so.waitEntered(t, 3*time.Second)
	so.unblock()
	var resp *http.Response
	select {
	case resp = <-respCh:
	case e := <-errCh:
		t.Fatalf("POST after SO release: %v", e)
	case <-time.After(4 * time.Second):
		t.Fatal("enforcing reply never arrived")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if !bytes.Equal(body, frame) {
		t.Fatalf("reply %q, want echo %q", body, frame)
	}
	if n := metrics.count(); n != 1 {
		t.Fatalf("decision markers = %d, want 1", n)
	}
}
