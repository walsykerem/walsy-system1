package main

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
