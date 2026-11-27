package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The MCP surface must be discoverable under the release name and must keep
// the functional tool names stable.
func TestFix6NamingSurfaces(t *testing.T) {
	faW32Env(t)
	var out bytes.Buffer
	runToolsServer(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n"), &out)
	list := out.String()
	for _, name := range []string{"walsy-system1", "system1_status", "system1_audit", "system1_decide"} {
		if !strings.Contains(list, name) {
			t.Fatalf("tools/list missing %q: %s", name, list)
		}
	}
	for _, legacy := range []string{"systemone", "system one", "Smart Router (System One)"} {
		if !strings.Contains(strings.ToLower(list), strings.ToLower(legacy)) {
			t.Fatalf("tools/list lost legacy alias %q", legacy)
		}
	}
}

// The MCP system1_decide tool auto-invokes the gate on an explicit call and
// admits a >=0.8 typed answer without a second ask.
func TestFix6MCPAutoDecideAtThreshold(t *testing.T) {
	item, ok := decisionItemFor("intake.intent")
	if !ok || !item.Gate || len(item.Proceed) == 0 {
		t.Fatal("intake.intent must be a gated item")
	}
	thr := item.threshold(defaultThreshold())
	body := fmt.Sprintf(`{"answers":{"gate":{"type":"choice","choice":%q,"confidence":%v}}}`, item.Proceed[0], thr)
	simSOFixture(t, body, nil)
	frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"system1_decide","arguments":{"item":"intake.intent","state":"simulate mcp auto decide"}}}` + "\n")
	var out bytes.Buffer
	runToolsServer(strings.NewReader(frame), &out)
	var reply struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
		t.Fatalf("MCP reply invalid: %v: %s", err, out.String())
	}
	if len(reply.Result.Content) != 1 {
		t.Fatalf("MCP reply content: %+v", reply.Result)
	}
	var decision decideReply
	if err := json.Unmarshal([]byte(reply.Result.Content[0].Text), &decision); err != nil {
		t.Fatalf("decision reply invalid: %v: %s", err, reply.Result.Content[0].Text)
	}
	if decision.Item != "intake.intent" || decision.Label != item.Proceed[0] || !decision.Admitted {
		t.Fatalf("MCP decision not auto-admitted at %v: %+v", thr, decision)
	}
}

// MCP decisions go through the operator combo on the default endpoint; a
// repeated frame must read the cached label without a second SO POST.
func TestFix6MCPDecisionUsesComboAndCaches(t *testing.T) {
	w11_base(t)
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &req)
		if req.Model != "model-combo" {
			t.Errorf("combo model = %q, want model-combo", req.Model)
		}
		_, _ = io.WriteString(w, w11_reply("build", 0.95)(1))
	}))
	t.Cleanup(srv.Close)
	if err := saveCustomProvider("Fix6 Combo", srv.URL+"/v1/systemone", "FIX6_COMBO_KEY"); err != nil {
		t.Fatal(err)
	}
	if err := saveCombo([]stateStep{{Provider: "Fix6 Combo", Model: "model-combo"}}); err != nil {
		t.Fatal(err)
	}
	oldEP, oldClient, oldTimeout := soEndpoint, soClient, soTimeout
	soEndpoint, soClient, soTimeout = soEndpointDefaultBase, srv.Client(), 2*time.Second
	t.Cleanup(func() { soEndpoint, soClient, soTimeout = oldEP, oldClient, oldTimeout })
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("EVREN_API_KEY", "w11-n16-n17-key")
	routerReset()
	t.Cleanup(routerReset)

	frame := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"system1_decide","arguments":{"item":"intake.intent","state":"classify combo request"}}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"system1_decide","arguments":{"item":"intake.intent","state":"classify combo request"}}}` + "\n"
	var out bytes.Buffer
	runToolsServer(strings.NewReader(frame), &out)
	replies := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(replies) != 2 {
		t.Fatalf("MCP replies = %d, want 2: %s", len(replies), out.String())
	}
	for i, line := range replies {
		out.Reset()
		out.WriteString(line)
		var reply struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out.Bytes(), &reply); err != nil || len(reply.Result.Content) != 1 {
			t.Fatalf("MCP reply %d invalid: %v: %s", i, err, out.String())
		}
		var decision decideReply
		if err := json.Unmarshal([]byte(reply.Result.Content[0].Text), &decision); err != nil || !decision.Admitted {
			t.Fatalf("MCP decision %d not admitted: %v: %s", i, err, reply.Result.Content[0].Text)
		}
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("SO POSTs across two MCP frames = %d, want 1", got)
	}
}
