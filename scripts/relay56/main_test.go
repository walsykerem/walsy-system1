package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Guards the harness itself: a PASS can only come from a real forwarded echo.
func TestCheckForwardRejectsBlockAndAcceptsEcho(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		want int
	}{
		{"blocked", map[string]any{"error": map[string]any{"message": "blocked: no"}}, 1},
		{"wrong method", map[string]any{"method": "initialize", "params": map[string]any{"path": "/x"}}, 1},
		{"args not forwarded", map[string]any{"method": "tools/call", "params": map[string]any{"path": "/y"}}, 1},
		{"nil", nil, 1},
		{"echoed", map[string]any{"method": "tools/call", "params": map[string]any{"path": "/x"}}, 0},
	}
	for _, c := range cases {
		fail = 0
		checkForward(c.name, c.m, "/x")
		if fail != c.want {
			t.Fatalf("%s: fail=%d want %d", c.name, fail, c.want)
		}
	}
	fail = 0
}

// An empty expectation must fail instead of printing PASS for a forward.
func TestCheckForwardWithoutWantedArgFails(t *testing.T) {
	fail = 0
	checkForward("vacuous", map[string]any{"method": "tools/call", "params": map[string]any{"path": "/x"}}, "")
	if fail != 1 {
		t.Fatalf("empty wantArg: fail=%d want 1", fail)
	}
	fail = 0
}

// An empty assertion must fail instead of printing PASS for nothing.
func TestCheckWithoutWantedTextFails(t *testing.T) {
	fail = 0
	check("vacuous", "anything")
	if fail != 1 {
		t.Fatalf("empty want: fail=%d want 1", fail)
	}
	fail = 0
}

// checkReason must read the reason field the gate recorded, and must fail
// rather than pass when the log is missing or carries a different code.
func TestCheckReasonReadsAuditField(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".walsy-system1"), 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(home, ".walsy-system1", "audit.jsonl")
	body := "{\"event\":\"decision\",\"decision\":\"pass\",\"reason\":\"layer0-static\"}\n"
	if err := os.WriteFile(log, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	fail = 0
	checkReason("present", home, "layer0-static")
	if fail != 0 {
		t.Fatalf("present reason: fail=%d want 0", fail)
	}
	checkReason("absent", home, "so-no")
	if fail != 1 {
		t.Fatalf("absent reason: fail=%d want 1", fail)
	}
	fail = 0
	checkReason("no log", t.TempDir(), "so-no")
	if fail != 1 {
		t.Fatalf("missing log: fail=%d want 1", fail)
	}
	fail = 0
}

// newPipeClient wires a client to in-memory pipes: send frames arrive on
// relayIn, and replies injected on relayOut go to the single reader goroutine.
func newPipeClient(t *testing.T) (*client, *bufio.Reader, *io.PipeWriter) {
	t.Helper()
	toRelayR, toRelayW := io.Pipe()
	fromRelayR, fromRelayW := io.Pipe()
	c := newClient(toRelayW, bufio.NewReader(fromRelayR), nil, "")
	t.Cleanup(func() {
		_ = fromRelayW.Close() // EOF ends the reader goroutine
		c.close()
		_ = toRelayR.Close()
		_ = fromRelayR.Close()
		_ = toRelayW.Close()
	})
	return c, bufio.NewReader(toRelayR), fromRelayW
}

func waitForFrame(t *testing.T, relayIn *bufio.Reader, id int) {
	t.Helper()
	line, err := relayIn.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`"id":%d`, id)
	if !strings.Contains(line, want) {
		t.Fatalf("want frame with %s, got %q", want, line)
	}
}

func inject(t *testing.T, relayOut io.Writer, line string) {
	t.Helper()
	if _, err := io.WriteString(relayOut, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

// Two in-flight sends must get their own replies even when the relay answers
// out of order, skips metric lines, and sends a frame with no waiter.
func TestSendRoutesOutOfOrderReplies(t *testing.T) {
	old := replyTimeout
	replyTimeout = 2 * time.Second
	defer func() { replyTimeout = old }()

	c, relayIn, relayOut := newPipeClient(t)
	type result struct {
		id int
		m  map[string]any
	}
	results := make(chan result, 2)
	sendAsync := func(id int) {
		go func() { results <- result{id, c.send(frame{ID: id, Method: "tools/call"})} }()
	}

	sendAsync(2)
	waitForFrame(t, relayIn, 2)
	sendAsync(3)
	waitForFrame(t, relayIn, 3)

	inject(t, relayOut, "relay metric line, not json")
	inject(t, relayOut, `{"id":99,"result":"orphan"}`)
	inject(t, relayOut, `{"id":3,"result":"three"}`)
	inject(t, relayOut, `{"id":2,"result":"two"}`)

	got := make(map[int]string)
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			got[r.id] = msg(r.m)
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for send to return")
		}
	}
	if got[2] != `"two"` || got[3] != `"three"` {
		t.Fatalf("misrouted replies: %v", got)
	}
}

// A timeout must drop only its own waiter; the shared reader stays alive and a
// late reply for the timed-out id must not be delivered to a later request.
func TestSendTimeoutLeavesReaderUsable(t *testing.T) {
	old := replyTimeout
	replyTimeout = 60 * time.Millisecond
	defer func() { replyTimeout = old }()

	c, relayIn, relayOut := newPipeClient(t)
	res := make(chan map[string]any, 1)

	go func() { res <- c.send(frame{ID: 1, Method: "tools/call"}) }()
	waitForFrame(t, relayIn, 1)
	select {
	case m := <-res:
		if m != nil {
			t.Fatalf("expected timeout, got %v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not time out")
	}

	inject(t, relayOut, `{"id":1,"result":"late"}`)

	go func() { res <- c.send(frame{ID: 2, Method: "tools/call"}) }()
	waitForFrame(t, relayIn, 2)
	inject(t, relayOut, `{"id":2,"result":"ok"}`)
	select {
	case m := <-res:
		if msg(m) != `"ok"` {
			t.Fatalf("want ok, got %q", msg(m))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second send did not receive its reply")
	}
}

// A stale send's deferred cleanup must not delete a newer waiter that reused
// the same id after the reader already dispatched the old reply.
func TestForgetWaiterKeepsNewerSameID(t *testing.T) {
	c := &client{waiters: make(map[int]chan map[string]any)}
	old := make(chan map[string]any, 1)
	newer := make(chan map[string]any, 1)
	c.waiters[7] = old
	c.waiters[7] = newer // id reused while old send's cleanup is still pending
	c.forgetWaiter(7, old)
	if c.waiters[7] != newer {
		t.Fatal("stale cleanup deleted a newer waiter with the same id")
	}
}

func TestReaderStopsOnSourceClose(t *testing.T) {
	c, _, relayOut := newPipeClient(t)
	_ = relayOut.Close()
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Fatal("reader goroutine leaked after reply source closed")
	}
}
