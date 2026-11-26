package app

import (
	"bytes"
	"strings"
	"testing"
)

// TestSimGapCacheRelayStartWipe: two MCP relay starts must never share a
// decision cell. Each start builds a fresh bridge (and therefore a fresh RAM
// cache); the second identical frame has to re-ask SO even though the first
// start cached an admit for the same key. A persisted or package-level cache
// would serve the old cell and leave the second POST uncounted.
func TestSimGapCacheRelayStartWipe(t *testing.T) {
	w11_base(t)
	srv, st := w11_stub(t, func(post int) string {
		if post == 1 {
			return w11_reply("yes", 0.95)(post)
		}
		return w11_reply("no", 0.95)(post) // changed oracle answer after restart
	})
	w11_attach(t, srv)

	const upstreamReply = `{"jsonrpc":"2.0","id":7,"result":{"ok":true}}` + "\n"
	for start := 1; start <= 2; start++ {
		var clientOut, upstreamIn bytes.Buffer
		relayStdio(strings.NewReader(w35_frame), &clientOut, &upstreamIn, strings.NewReader(upstreamReply))

		if got := st.count(); got != start {
			t.Fatalf("relay start %d: SO posts = %d, want %d (stale decision survived a relay restart)",
				start, got, start)
		}
		if n := strings.Count(upstreamIn.String(), `"method":"tools/call"`); n != 1 {
			t.Fatalf("relay start %d: forwarded %d frames, want 1", start, n)
		}
		if !strings.Contains(clientOut.String(), `"ok":true`) {
			t.Fatalf("relay start %d: upstream reply not returned: %q", start, clientOut.String())
		}
	}

	// The second start must carry its own oracle answer, not the cached admit.
	d := newDecisionBridge()
	r := d.decide([]byte(w35_req))
	if r.Reason != "so-no" || !r.Abstained || st.count() != 3 {
		t.Fatalf("post-restart decision = %+v posts=%d, want so-no/abstain/3 (stale 'yes' served)",
			r, st.count())
	}
}
