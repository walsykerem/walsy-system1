package app

import (
	"bytes"
	"testing"
	"time"
)

type closeNotify struct {
	*bytes.Buffer
	done chan struct{}
}

func (c *closeNotify) Close() error { close(c.done); return nil }

// round-trip: frames cross verbatim, including id/progressToken/protocolVersion
// and unknown fields. Passthrough is byte-for-byte, so no structural assertion.
func TestRelayStdioVerbatim(t *testing.T) {
	in := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"_meta":{"progressToken":"p-1"},"name":"x","weird":{"nested":[1,2,3]}}}` + "\n" +
		`{"jsonrpc":"2.0","id":8,"method":"initialize","params":{"protocolVersion":"2026-07-28"}}` + "\n")
	upIn := &closeNotify{Buffer: &bytes.Buffer{}, done: make(chan struct{})}
	var clientOut bytes.Buffer

	relayStdio(bytes.NewReader(in), &clientOut, upIn, bytes.NewReader(in)) // upstream echoes input

	select {
	case <-upIn.done:
	case <-time.After(time.Second):
		t.Fatal("upstream write side never closed")
	}
	if !bytes.Equal(upIn.Bytes(), in) {
		t.Fatalf("upstream got %q, want %q", upIn.Bytes(), in)
	}
	if !bytes.Equal(clientOut.Bytes(), in) {
		t.Fatalf("client got %q, want verbatim %q", clientOut.Bytes(), in)
	}
}
