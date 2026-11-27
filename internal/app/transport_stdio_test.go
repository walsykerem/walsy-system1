package app

import (
	"bytes"
	"io"
	"sync"
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

// eofReader is stateless so concurrent Read calls are race-free by construction.
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// drain signal: concurrent EOF observations close gone exactly once (-race).
func TestNotifyEOFClosesOnceUnderRace(t *testing.T) {
	gone := make(chan struct{})
	c := &notifyEOF{r: eofReader{}, gone: gone}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Read(make([]byte, 1))
		}()
	}
	wg.Wait()
	select {
	case <-gone:
	default:
		t.Fatal("gone not closed after EOF")
	}
}
