package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A replayed tools/call id must reuse its first reply: one SO POST, no second
// application, even when the first attempt was rate limited with Retry-After.
func TestFix4_19ToolsCallReplayIsIdempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("EVREN_API_KEY", "test-key")
	routerReset()

	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	origEndpoint, origClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	defer func() { soEndpoint, soClient = origEndpoint, origClient }()

	const frame = `{"jsonrpc":"2.0","id":19,"method":"tools/call","params":{"name":"system1_decide","arguments":{"item":"intake.intent","state":"classify replay once"}}}`
	replies := toolsSession(t, frame, frame)
	if len(replies) != 2 {
		t.Fatalf("replies = %d, want 2", len(replies))
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("SO POSTs = %d, want 1 for a replayed id (429/Retry-After)", got)
	}
	text := func(r map[string]any) string {
		return r["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	first, replay := text(replies[0]), text(replies[1])
	if first != replay {
		t.Fatalf("replay replies differ: %q / %q", first, replay)
	}
	if strings.Contains(first, "test-key") {
		t.Fatal("reply leaked key material")
	}
}
