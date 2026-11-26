package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// A malformed JSON line ends the tools stream silently, and a frame that parses
// as JSON but is not a JSON-RPC object is skipped without costing the frames
// that follow it. Both branches are observable protocol, not internals: the
// decoder must neither invent a reply for garbage nor die on a stray array.
func TestSimGapMCPMalformedFrameStream(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var out strings.Builder
	runToolsServer(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"`+"\n"), &out)
	if out.Len() != 0 {
		t.Fatalf("malformed JSON frame produced a reply: %q", out.String())
	}

	out.Reset()
	runToolsServer(strings.NewReader("[1,2]\n"+pingFrame), &out)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("non-object frame replies = %d, want only the ping: %q", len(lines), out.String())
	}
	var reply struct {
		ID     int            `json:"id"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &reply); err != nil {
		t.Fatalf("ping reply not JSON: %v: %s", err, lines[0])
	}
	if reply.ID != 1 || reply.Result == nil {
		t.Fatalf("frame after a skipped non-object was not served: %s", lines[0])
	}
}

// A system1_decide call whose arguments are valid JSON but not an object must
// fail as invalid params (-32602) without reaching System One: the gate frame
// lift must pass the malformed call through unchanged rather than fabricating
// a state that would arm the SO hook.
func TestSimGapMCPMalformedToolArguments(t *testing.T) {
	stub := w25_env(t, "advisory")
	var out bytes.Buffer
	frame := w25_frame("tools/call", 7, `"name":"system1_decide","arguments":"oops"`)
	runToolsServer(bytes.NewReader(append(frame, '\n')), &out)

	if got := stub.count(); got != 0 {
		t.Fatalf("malformed arguments reached System One: %d POST(s)", got)
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &rpc); err != nil {
		t.Fatalf("tools reply not JSON: %v: %s", err, out.String())
	}
	if rpc.Result != nil {
		t.Fatalf("malformed arguments produced a tool result: %s", out.String())
	}
	if rpc.Error == nil || rpc.Error.Code != -32602 {
		t.Fatalf("malformed arguments error = %+v, want -32602: %s", rpc.Error, out.String())
	}
	if !strings.Contains(rpc.Error.Message, "cannot unmarshal") {
		t.Fatalf("malformed arguments message = %q, want the unmarshal error", rpc.Error.Message)
	}
}
