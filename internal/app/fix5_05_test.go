package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// An item whose question requires a state field the caller did not supply has
// no applicable question: it must be reported as such, not as a model failure.
func TestToolsDecideInapplicableQuestionIsTyped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("EVREN_API_KEY", "test-key")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"safe","confidence":0.95}}}`))
	}))
	defer srv.Close()
	origEndpoint, origClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	defer func() { soEndpoint, soClient = origEndpoint, origClient }()

	inapp := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"system1_decide","arguments":{"item":"intake.injection","state":"{\"state\":\"decide if untrusted input is safe\"}"}}}`
	local := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"system1_decide","arguments":{"item":"reasoning.priority","state":"decide what to do next"}}}`
	replies := toolsSession(t, inapp, inapp, local)
	if len(replies) != 3 {
		t.Fatalf("replies = %d, want 3: %v", len(replies), replies)
	}
	text := func(i int) string {
		return replies[i]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	for i := range 2 {
		if got := text(i); got != `{"error":"question inapplicable: missing required state fields"}` {
			t.Fatalf("inapplicable reply %d = %s", i, got)
		}
	}
	if text(0) != text(1) {
		t.Fatalf("idempotent replay differs: %s vs %s", text(0), text(1))
	}
	if hits != 0 {
		t.Fatalf("inapplicable question made %d model calls, want 0", hits)
	}
	var localReply map[string]any
	if err := json.Unmarshal([]byte(text(2)), &localReply); err != nil {
		t.Fatalf("local reply not JSON: %s", text(2))
	}
	for k, want := range map[string]any{"item": "reasoning.priority", "local": true, "admitted": false} {
		if localReply[k] != want {
			t.Fatalf("gate:false local reply %s: %s = %v, want %v", text(2), k, localReply[k], want)
		}
	}
}
