package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The HTTP transport reads the body through http.MaxBytesReader(_, _, maxHTTPFrame),
// which accepts a body of exactly maxHTTPFrame bytes. The bridge must agree: a
// frame at the limit is decided like any other, and only maxHTTPFrame+1 is
// refused as frame-too-large. Otherwise one exactly-sized frame is relayed
// un-judged, which is a gate bypass in enforcing mode.
func TestFix302FrameAtTransportLimitIsDecided(t *testing.T) {
	resetRouterForTest(t)
	t.Setenv("EVREN_API_KEY", "test-key")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	orig := soEndpoint
	soEndpoint = srv.URL
	t.Cleanup(func() { soEndpoint = orig })

	d := newDecisionBridge()
	head := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"pad":"`
	tail := `"}}}`
	frame := func(n int) []byte {
		return []byte(head + strings.Repeat("a", n-len(head)-len(tail)) + tail)
	}

	under, _ := d.decideFrameDetailed(frame(maxHTTPFrame - 1))
	at, _ := d.decideFrameDetailed(frame(maxHTTPFrame))
	if len(frame(maxHTTPFrame)) != maxHTTPFrame {
		t.Fatal("test frame is not exactly maxHTTPFrame bytes")
	}
	if at.Reason == "frame-too-large" || !at.Passthrough {
		t.Fatalf("frame at exactly maxHTTPFrame = reason %q passthrough=%v, want a decided frame (same as one byte less)",
			at.Reason, at.Passthrough)
	}
	if at.Reason != under.Reason {
		t.Fatalf("boundary disagrees: %d bytes -> %q, %d bytes -> %q",
			maxHTTPFrame-1, under.Reason, maxHTTPFrame, at.Reason)
	}

	over := d.decideFrame(frame(maxHTTPFrame + 1))
	if over.Reason != "frame-too-large" || !over.Passthrough {
		t.Fatalf("frame at maxHTTPFrame+1 = reason %q passthrough=%v, want frame-too-large passthrough",
			over.Reason, over.Passthrough)
	}
}
