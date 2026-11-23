package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Simulation-only coverage for MANIFEST N28/N29. Every case drives real
// production code (askSOQ, route, serveUnix, listenUnix, bridgeStdio) with a
// loopback httptest SO stub or real unix sockets. No live SO call, no key file
// read, no production edit.

const simN28ValidSO = `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`

const simN28State = `{"state":"classify this request","method":"tools/call","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"opt_in":true,"phase":"before-model","authority":true,"calibrated":true}`

func simN28Setup(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	for _, k := range []string{
		"WALSY_ROUTER_RETRY_BUDGET", "WALSY_ROUTER_BREAKER_THRESHOLD",
		"WALSY_ROUTER_BREAKER_COOLDOWN", "WALSY_ROUTER_DENY",
	} {
		t.Setenv(k, "")
	}
	routerReset()
	t.Cleanup(routerReset)
}

func simN28Stub(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	oldEndpoint, oldClient, oldTimeout := soEndpoint, soClient, soTimeout
	soEndpoint, soClient = srv.URL, srv.Client()
	t.Setenv("EVREN_API_KEY", "sim-n28-test-key")
	t.Cleanup(func() {
		soEndpoint, soClient, soTimeout = oldEndpoint, oldClient, oldTimeout
		srv.Close()
	})
}

func TestSimN28SOGuardAndLatency(t *testing.T) {
	q := soQuestion{Type: "choice", Criteria: map[string]string{"yes": "admit", "no": "pass"}}

	t.Run("healthy_post_and_route", func(t *testing.T) {
		simN28Setup(t)
		var hits int32
		simN28Stub(t, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			body, err := io.ReadAll(r.Body)
			if err != nil || r.Method != http.MethodPost {
				t.Errorf("bad SO request: method=%s err=%v", r.Method, err)
			}
			if r.Header.Get("x-api-key") != "sim-n28-test-key" {
				t.Error("SO stub missing x-api-key")
			}
			if !bytes.Contains(body, []byte(`"questions":{`)) || !bytes.Contains(body, []byte(`"model":"clef"`)) {
				t.Errorf("unexpected SO wire body: %s", body)
			}
			_, _ = io.WriteString(w, simN28ValidSO)
		})
		before := routerBudgetLeft()
		d := askSOQ("sim state", q)
		if d.Choice != "yes" || d.Conf < 0.9 || d.TimedOut {
			t.Fatalf("healthy SO decision = %+v", d)
		}
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Fatalf("SO posts = %d, want 1", got)
		}
		if got := routerBudgetLeft(); got != before {
			t.Fatalf("askSOQ spent retry budget: %d -> %d", before, got)
		}
		r := route(simN28State, nil)
		if r.Decision != decPin || r.Reason != "so-admit" {
			t.Fatalf("route after healthy SO = %+v", r)
		}
		if got := atomic.LoadInt32(&hits); got != 2 {
			t.Fatalf("route SO posts = %d, want 2", got)
		}
	})

	t.Run("breaker_skips_post_then_half_open_recovers", func(t *testing.T) {
		simN28Setup(t)
		t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "2")
		t.Setenv("WALSY_ROUTER_BREAKER_COOLDOWN", "20ms")
		routerReset()
		var degraded atomic.Bool
		degraded.Store(true)
		var hits int32
		simN28Stub(t, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			if degraded.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, simN28ValidSO)
		})

		d1, d2 := askSO("classify this"), askSO("classify this")
		if d1.Choice != "" || d2.Choice != "" || d1.TimedOut || d2.TimedOut {
			t.Fatalf("HTTP 500 decisions = %+v, %+v", d1, d2)
		}
		if got := atomic.LoadInt32(&hits); got != 2 {
			t.Fatalf("degraded SO posts = %d, want 2", got)
		}

		degraded.Store(false) // endpoint recovered; breaker must still skip
		if d := askSO("classify this"); d.Choice != "" {
			t.Fatalf("open breaker must fail open: %+v", d)
		}
		if r := route(simN28State, nil); r.Decision != decPass || r.Reason != "so-degraded" {
			t.Fatalf("route with open breaker = %+v", r)
		}
		if got := atomic.LoadInt32(&hits); got != 2 {
			t.Fatalf("open breaker leaked %d posts", got-2)
		}

		time.Sleep(40 * time.Millisecond) // cooldown permits one half-open probe
		if d := askSO("classify this"); d.Choice != "yes" {
			t.Fatalf("half-open healthy probe = %+v", d)
		}
		if got := atomic.LoadInt32(&hits); got != 3 {
			t.Fatalf("posts after half-open probe = %d, want 3", got)
		}
		if routerBreakerOpen("systemone") {
			t.Fatal("success did not close the breaker")
		}
	})

	t.Run("empty_budget_skips_post", func(t *testing.T) {
		simN28Setup(t)
		t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "1")
		routerReset()
		if !routerBudgetOK() {
			t.Fatal("could not spend the only tool-call token")
		}
		if got := routerBudgetLeft(); got != 0 {
			t.Fatalf("budget left = %d, want 0", got)
		}
		var hits int32
		simN28Stub(t, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			_, _ = io.WriteString(w, simN28ValidSO)
		})
		if d := askSO("classify this"); d.Choice != "" {
			t.Fatalf("empty budget must fail open: %+v", d)
		}
		if got := atomic.LoadInt32(&hits); got != 0 {
			t.Fatalf("empty budget leaked %d SO posts", got)
		}
	})

	t.Run("concurrency_cap_fails_open_without_waiting", func(t *testing.T) {
		simN28Setup(t)
		entered := make(chan struct{}, 4)
		release := make(chan struct{})
		var once sync.Once
		freeAll := func() { once.Do(func() { close(release) }) }
		defer freeAll()
		var hits int32
		simN28Stub(t, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			entered <- struct{}{}
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
			_, _ = io.WriteString(w, simN28ValidSO)
		})

		results := make(chan soDecision, 3)
		for i := 0; i < 2; i++ {
			go func() { results <- askSOQ("sim state", q) }()
		}
		for i := 0; i < 2; i++ {
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatalf("only %d of 2 SO calls entered the stub", i)
			}
		}
		go func() { results <- askSOQ("sim state", q) }()
		select {
		case d := <-results:
			if d.Choice != "" || d.TimedOut {
				t.Fatalf("over-cap SO call must fail open: %+v", d)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatal("over-cap SO call waited for a slot instead of failing open")
		}
		if got := atomic.LoadInt32(&hits); got != 2 {
			t.Fatalf("over-cap SO posts = %d, want 2", got)
		}

		release <- struct{}{} // free exactly one slot
		select {
		case d := <-results:
			if d.Choice != "yes" {
				t.Fatalf("in-cap SO call = %+v", d)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("freed slot did not finish an in-cap call")
		}
		freeAll()
		select {
		case d := <-results:
			if d.Choice != "yes" {
				t.Fatalf("second in-cap SO call = %+v", d)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("second in-cap call did not finish")
		}
		if got := atomic.LoadInt32(&hits); got != 2 {
			t.Fatalf("total SO posts = %d, want 2", got)
		}
	})

	t.Run("slow_so_trips_breaker_without_spending_budget", func(t *testing.T) {
		simN28Setup(t)
		t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "2")
		t.Setenv("WALSY_ROUTER_BREAKER_COOLDOWN", "1h")
		routerReset()
		var slow atomic.Bool
		slow.Store(true)
		var hits int32
		simN28Stub(t, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			if slow.Load() {
				time.Sleep(120 * time.Millisecond)
			}
			_, _ = io.WriteString(w, simN28ValidSO)
		})
		soTimeout = 40 * time.Millisecond
		before := routerBudgetLeft()

		d1, d2 := askSO("classify this"), askSO("classify this")
		if !d1.TimedOut || !d2.TimedOut || d1.Choice != "" || d2.Choice != "" {
			t.Fatalf("slow SO decisions = %+v, %+v", d1, d2)
		}
		if got := atomic.LoadInt32(&hits); got != 2 {
			t.Fatalf("slow SO posts = %d, want 2", got)
		}
		if got := routerBudgetLeft(); got != before {
			t.Fatalf("SO latency changed retry budget: %d -> %d", before, got)
		}

		slow.Store(false) // endpoint now fast, but the breaker must skip it
		if d := askSO("classify this"); d.Choice != "" || d.TimedOut {
			t.Fatalf("latency-opened breaker must fail open: %+v", d)
		}
		if got := atomic.LoadInt32(&hits); got != 2 {
			t.Fatalf("latency-opened breaker leaked %d posts", got-2)
		}
	})
}

func sim29Paths(t *testing.T, suffix string) (string, string) {
	t.Helper()
	base := filepath.Join(os.TempDir(), fmt.Sprintf("sim29-%d-%s", os.Getpid(), suffix))
	for _, p := range []string{base + "-client.sock", base + "-upstream.sock"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(p) })
	}
	return base + "-client.sock", base + "-upstream.sock"
}

func TestSimN29UnixPerClientDialCap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "sim-n29-test-key")

	t.Run("sixty_five_clients_one_dial_each", func(t *testing.T) {
		clientPath, upstreamPath := sim29Paths(t, "cap")
		l, err := listenUnix(clientPath)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		up, err := listenUnix(upstreamPath)
		if err != nil {
			t.Fatal(err)
		}
		upDone := make(chan struct{})
		go func() {
			defer close(upDone)
			for {
				c, err := up.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer c.Close()
					frame, err := bufio.NewReader(c).ReadBytes('\n')
					if err == nil {
						_, _ = c.Write(frame) // verbatim upstream echo
					}
				}(c)
			}
		}()
		defer func() {
			_ = up.Close()
			<-upDone
		}()

		entered := make(chan struct{}, unixMaxConns+2)
		release := make(chan struct{})
		var once sync.Once
		freeAll := func() { once.Do(func() { close(release) }) }
		defer freeAll()
		var dialCount int64
		// Same shape as main.go runUnix: one fresh upstream dial per client.
		dial := func() (net.Conn, error) {
			atomic.AddInt64(&dialCount, 1)
			entered <- struct{}{}
			select {
			case <-release:
			case <-time.After(5 * time.Second):
				return nil, io.EOF
			}
			return net.Dial("unix", upstreamPath)
		}
		serveDone := make(chan struct{})
		go func() {
			serveUnix(l, dial) // real relay + advisory decision bridge
			close(serveDone)
		}()
		defer func() {
			_ = l.Close()
			<-serveDone
		}()

		const clients = unixMaxConns + 1
		conns := make([]net.Conn, 0, clients)
		frames := make([][]byte, 0, clients)
		for i := 0; i < clients; i++ {
			c, err := net.DialTimeout("unix", clientPath, 2*time.Second)
			if err != nil {
				t.Fatalf("client %d dial: %v", i, err)
			}
			conns = append(conns, c)
			frames = append(frames, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"}`+"\n", i)))
		}
		defer func() {
			for _, c := range conns {
				_ = c.Close()
			}
		}()

		for i := 0; i < unixMaxConns; i++ {
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatalf("only %d of %d upstream dials started", i, unixMaxConns)
			}
		}
		select {
		case <-entered:
			t.Fatal("concurrency cap exceeded: 65th upstream dial started")
		case <-time.After(150 * time.Millisecond):
		}
		if got := atomic.LoadInt64(&dialCount); got != unixMaxConns {
			t.Fatalf("upstream dials before release = %d, want %d", got, unixMaxConns)
		}

		for i, c := range conns {
			_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if _, err := c.Write(frames[i]); err != nil {
				t.Fatalf("client %d write: %v", i, err)
			}
		}
		// bridgeStdio holds a slot until the client socket closes: release the
		// blocked dials, then retire each client after its verbatim echo so the
		// queued 65th client also gets admitted and served.
		freeAll()
		echoErr := make(chan string, clients)
		for i, c := range conns {
			go func(i int, c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				got, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					echoErr <- fmt.Sprintf("client %d read: %v", i, err)
					return
				}
				if !bytes.Equal(got, frames[i]) {
					echoErr <- fmt.Sprintf("client %d frame = %q, want %q", i, got, frames[i])
					return
				}
				echoErr <- ""
			}(i, c)
		}
		for i := 0; i < clients; i++ {
			if msg := <-echoErr; msg != "" {
				t.Fatal(msg)
			}
		}
		if got := atomic.LoadInt64(&dialCount); got != clients {
			t.Fatalf("total upstream dials = %d, want %d (one per client)", got, clients)
		}
	})

	t.Run("dial_failure_is_eof_not_error_frame", func(t *testing.T) {
		clientPath, _ := sim29Paths(t, "fail")
		l, err := listenUnix(clientPath)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		serveDone := make(chan struct{})
		go func() {
			serveUnix(l, func() (net.Conn, error) { return nil, fmt.Errorf("simulated dial failure") })
			close(serveDone)
		}()
		defer func() {
			_ = l.Close()
			<-serveDone
		}()

		c, err := net.DialTimeout("unix", clientPath, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n")); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 256)
		n, err := c.Read(buf)
		if err != io.EOF {
			t.Fatalf("dial failure read: n=%d err=%v body=%q", n, err, buf[:n])
		}
		if bytes.Contains(buf[:n], []byte("error")) {
			t.Fatalf("synthetic error frame on dial failure: %q", buf[:n])
		}
	})
}
