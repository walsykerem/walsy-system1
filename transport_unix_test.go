package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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

// unixDoneListener signals when serveUnix closes an accepted connection.
// The server-side decision hook runs before that deferred Close, so waiting
// on done also waits for the hook's read of global state to finish.
type unixDoneListener struct {
	net.Listener
	done chan struct{}
}

func (l *unixDoneListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &unixDoneConn{Conn: c, done: l.done}, nil
}

type unixDoneConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *unixDoneConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
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
	done := make(chan struct{})
	go serveUnix(&unixDoneListener{Listener: l, done: done}, nil)
	defer l.Close()

	c, err := net.Dial("unix", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("frame")); err != nil {
		t.Fatal(err)
	}
	// Half-close the write side: with no upstream the server must drain the
	// frame and close the connection, never emit an error frame.
	uc, ok := c.(*net.UnixConn)
	if !ok {
		t.Fatal("expected a unix connection")
	}
	if err := uc.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != io.EOF {
		t.Fatalf("server must drain and close with no error frame: n=%d err=%v body=%q", n, err, buf[:n])
	}
	c.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serveUnix did not close accepted unix connection")
	}
}

// The accept loop serves at most unixMaxConns connections at once: a client
// beyond the cap waits until a slot frees instead of spawning a goroutine.
func TestServeUnixConcurrencyCap(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	started := make(chan struct{}, unixMaxConns*2)
	release := make(chan struct{})
	done := make(chan struct{})
	defer close(done)
	dial := func() (net.Conn, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-done:
		}
		return nil, io.EOF // passthrough: no upstream
	}
	go serveUnix(l, dial)
	conns := make([]net.Conn, 0, unixMaxConns+1)
	for i := 0; i < unixMaxConns+1; i++ {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < unixMaxConns; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d dials started", i, unixMaxConns)
		}
	}
	select {
	case <-started:
		t.Fatal("concurrency cap exceeded: more than unixMaxConns dials ran at once")
	case <-time.After(100 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("a freed slot did not admit the queued client")
	}
}
