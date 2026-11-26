package app

import (
	"bufio"
	"bytes"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuf is the clientOut sink: the forwarded frame and the enforcing block
// reply can come from different goroutines.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestIdleReadDeadline pins the helper's condition: a stdio *os.File reader is
// never deadlined, a socket (net.Conn) keeps the bounded-slot guard.
func TestIdleReadDeadline(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()
	defer pw.Close()
	if d, arm := idleReadDeadline(pr); arm || !d.IsZero() {
		t.Fatalf("os.Pipe reader got deadline %v, arm=%v; stdio must stay exempt", d, arm)
	}

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	before := time.Now()
	d, arm := idleReadDeadline(a)
	if !arm {
		t.Fatal("net.Conn reader must be deadlined")
	}
	if d.Before(before.Add(httpReadTimeout - time.Second)) {
		t.Fatalf("net.Conn deadline = %v, want ~now+%v", d, httpReadTimeout)
	}
}

// TestBridgeStdioIdleBetweenFrames runs the bridge over real *os.File pipes,
// leaves the session idle past a short gap, and asserts the read loop still
// relays both directions. A timed-out stdio read would leave the upstream
// reply unread forever.
func TestBridgeStdioIdleBetweenFrames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "advisory")

	cInR, cInW, err := os.Pipe() // client stdin
	if err != nil {
		t.Fatalf("os.Pipe client: %v", err)
	}
	uInR, uInW, err := os.Pipe() // upstream stdin
	if err != nil {
		t.Fatalf("os.Pipe upstream in: %v", err)
	}
	uOutR, uOutW, err := os.Pipe() // upstream stdout
	if err != nil {
		t.Fatalf("os.Pipe upstream out: %v", err)
	}
	var clientOut lockedBuf

	done := make(chan struct{})
	go func() {
		defer close(done)
		bridgeStdio(cInR, &clientOut, uInW, uOutR)
	}()
	defer func() {
		// Tear down both loops: EOF on either side ends the bridge.
		_ = cInW.Close()
		_ = uOutW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("bridgeStdio did not return after both sides reached EOF")
		}
	}()

	up := bufio.NewReader(uInR)
	readLine := func() string {
		t.Helper()
		_ = uInR.SetReadDeadline(time.Now().Add(3 * time.Second))
		line, err := up.ReadString('\n')
		if err != nil {
			t.Fatalf("upstream read: %v (got %q)", err, line)
		}
		return line
	}

	initFrame := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	if _, err := cInW.Write([]byte(initFrame)); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	if got := readLine(); !strings.Contains(got, `"id":1`) {
		t.Fatalf("initialize not relayed upstream: %q", got)
	}

	time.Sleep(200 * time.Millisecond) // idle window a 30s deadline would survive

	listFrame := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	if _, err := cInW.Write([]byte(listFrame)); err != nil {
		t.Fatalf("write tools/list after idle: %v", err)
	}
	if got := readLine(); !strings.Contains(got, `"id":2`) {
		t.Fatalf("frame after idle not relayed upstream: %q", got)
	}

	reply := `{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}` + "\n"
	if _, err := uOutW.Write([]byte(reply)); err != nil {
		t.Fatalf("write upstream reply: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(clientOut.String(), `"id":2`) {
		if time.Now().After(deadline) {
			t.Fatalf("reply after idle never reached client: %q", clientOut.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
