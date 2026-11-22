package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// round-trip: a unix-socket client and an in-memory upstream exchange frames
// verbatim, including id/progressToken/protocolVersion and unknown fields.
func TestServeUnixRoundTrip(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"_meta":{"progressToken":"p-1"},"protocolVersion":"2025-06-18","weird":[1,2,3]}}` + "\n")

	l, err := listenUnix(filepath.Join(t.TempDir(), "mcp.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	done := make(chan struct{})
	go func() {
		serveUnix(l, func() (net.Conn, error) {
			a, b := net.Pipe()
			go func() {
				buf := make([]byte, len(frame))
				io.ReadFull(b, buf) // one JSON-RPC frame
				b.Write(buf)        // upstream echoes verbatim
				b.Close()
			}()
			return a, nil
		})
		close(done)
	}()

	c, err := net.Dial("unix", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(frame); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(frame))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("got %q, want verbatim %q", got, frame)
	}
	l.Close()
	<-done
}

// no upstream -> transparent no-op: client closes, server drains, no error frame.
func TestServeUnixNoUpstreamPassthrough(t *testing.T) {
	// macOS sun_path is 104 bytes; use a short deterministic path.
	path := filepath.Join(os.TempDir(), fmt.Sprintf("ws1-%d.sock", os.Getpid()))
	os.Remove(path)
	l, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	go serveUnix(l, nil)
	defer l.Close()

	c, err := net.Dial("unix", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("frame")); err != nil {
		t.Fatal(err)
	}
	c.Close()
}
