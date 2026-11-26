package app

import (
	"bytes"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// A frame past the transport cap must never reach the upstream, not even as a
// truncated prefix: the read loop stops at the oversized frame. An earlier
// valid frame still crosses verbatim in both directions.
func TestSimGapTransportOversizedFrameNoPartialForward(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "advisory")
	t.Setenv("EVREN_API_KEY", "")

	valid := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	reply := []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` + "\n")
	oversized := append(bytes.Repeat([]byte("a"), maxStdioFrame+1), '\n')
	in := append(append([]byte{}, valid...), oversized...)

	var toUpstream, toClient bytes.Buffer
	relayStdio(bytes.NewReader(in), &toClient, &toUpstream, bytes.NewReader(reply))

	if !bytes.Equal(toUpstream.Bytes(), valid) {
		t.Fatalf("upstream got %d bytes, want only the %d-byte valid frame", toUpstream.Len(), len(valid))
	}
	if !bytes.Equal(toClient.Bytes(), reply) {
		t.Fatalf("client got %q, want verbatim %q", toClient.Bytes(), reply)
	}
}

// serveUnix must arm a read deadline on every accepted connection, including
// wrapper conns, so a silent client cannot pin one of the 64 slots forever.
type simGapDeadlineConn struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
	set      bool
}

func (c *simGapDeadlineConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline, c.set = t, true
	c.mu.Unlock()
	return c.Conn.SetDeadline(t)
}

func (c *simGapDeadlineConn) snapshot() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline, c.set
}

type simGapDeadlineListener struct {
	net.Listener
	conns chan *simGapDeadlineConn
}

func (l *simGapDeadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	wrapped := &simGapDeadlineConn{Conn: c}
	l.conns <- wrapped
	return wrapped, nil
}

func TestSimGapTransportUnixConnDeadlineArmed(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	wl := &simGapDeadlineListener{Listener: l, conns: make(chan *simGapDeadlineConn, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveUnix(wl, func() (net.Conn, error) { return nil, errors.New("no upstream") })
	}()

	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var accepted *simGapDeadlineConn
	select {
	case accepted = <-wl.conns:
	case <-time.After(3 * time.Second):
		t.Fatal("serveUnix did not accept the client")
	}

	before := time.Now()
	deadline, armed := accepted.snapshot()
	if !armed {
		t.Fatal("serveUnix did not arm a connection deadline")
	}
	if d := deadline.Sub(before.Add(httpReadTimeout)); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("connection deadline = %v, want ~now+%v", deadline, httpReadTimeout)
	}

	c.Close()
	l.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serveUnix did not stop after listener close")
	}
}
