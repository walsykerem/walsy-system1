package app

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// deadlineRecReader yields one newline-terminated frame per Read and records
// every SetDeadline refresh the bridge performs on its client input.
type deadlineRecReader struct {
	frames    [][]byte
	pos       int
	deadlines atomic.Int64
}

func (r *deadlineRecReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.frames) {
		return 0, io.EOF
	}
	f := r.frames[r.pos]
	r.pos++
	return copy(p, f), nil
}

func (r *deadlineRecReader) SetDeadline(time.Time) error {
	r.deadlines.Add(1)
	return nil
}

func TestBridgeStdioDeadlineRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	r := &deadlineRecReader{frames: [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"),
		[]byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"),
	}}
	var toClient, toServer bytes.Buffer
	bridgeStdio(r, &toClient, &toServer, strings.NewReader(""))

	if got := r.deadlines.Load(); got < 2 {
		t.Fatalf("SetDeadline refreshes = %d, want >= 2 (one per client read)", got)
	}
	if !bytes.Contains(toServer.Bytes(), []byte(`"id":1`)) ||
		!bytes.Contains(toServer.Bytes(), []byte(`"id":2`)) {
		t.Fatalf("both client frames must be forwarded: %q", toServer.Bytes())
	}
}

// silentDeadlineConn blocks the upstream read until released and records the
// deadline applied before it. The test never waits out httpReadTimeout.
type silentDeadlineConn struct {
	deadlines atomic.Int64
	deadline  atomic.Value // time.Time
	release   chan struct{}
}

func (c *silentDeadlineConn) SetDeadline(t time.Time) error {
	c.deadlines.Add(1)
	c.deadline.Store(t)
	return nil
}

func (c *silentDeadlineConn) Read([]byte) (int, error) {
	<-c.release
	return 0, io.EOF
}

func TestBridgeStdioUpstreamDeadline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	up := &silentDeadlineConn{release: make(chan struct{})}
	released := false
	defer func() {
		if !released {
			close(up.release)
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		bridgeStdio(bytes.NewReader(nil), io.Discard, io.Discard, up)
	}()

	deadlined := false
	for i := 0; i < 500; i++ {
		if up.deadlines.Load() > 0 {
			deadlined = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !deadlined {
		t.Fatal("silent upstream read had no deadline")
	}
	d, ok := up.deadline.Load().(time.Time)
	if !ok || d.Before(time.Now().Add(httpReadTimeout-time.Second)) {
		t.Fatalf("upstream deadline = %v, want ~now+%v", d, httpReadTimeout)
	}

	close(up.release)
	released = true
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not return after silent upstream was released")
	}
}

func TestSoRoundWhitespaceMarker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d := newDecisionBridge()

	// Whitespace around the "result" key used to defeat the raw byte splice.
	frame := []byte(`{"jsonrpc":"2.0","id":1, "result" : {"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"up","version":"1"}}}`)
	out := d.inspectServer(frame)
	if !bytes.Contains(out, []byte(markerKey)) {
		t.Fatalf("capability marker missing after whitespace-spliced result: %s", out)
	}
	if !json.Valid(out) {
		t.Fatalf("spliced envelope is not valid JSON: %s", out)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("spliced envelope: %v", err)
	}
	if !bytes.Contains(env["id"], []byte("1")) {
		t.Fatalf("id not preserved: %s", out)
	}

	// A stdio frame carries its trailing newline; the rewritten frame must too.
	withNL := append(append([]byte(nil), frame...), '\n')
	outNL := d.inspectServer(withNL)
	if !bytes.HasSuffix(outNL, []byte("\n")) {
		t.Fatalf("trailing newline dropped: %q", outNL)
	}

	// Frames that receive no marker must keep their exact input bytes.
	plain := []byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if got := d.inspectServer(plain); !bytes.Equal(got, plain) {
		t.Fatalf("non-marker frame changed: %q", got)
	}
}
