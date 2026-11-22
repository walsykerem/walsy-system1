package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func gateFrame(method string, id int) []byte {
	if method == "tools/call" {
		return []byte(`{"jsonrpc":"2.0","id":` + itoaTest(id) +
			`,"method":"tools/call","params":{"name":"read_file"}}`)
	}
	return []byte(`{"jsonrpc":"2.0","id":` + itoaTest(id) +
		`,"method":"` + method + `","params":{}}`)
}

func itoaTest(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

type gateReply struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func TestBudgetGateTable(t *testing.T) {
	cases := []struct {
		name    string
		budget  string
		frames  []string // method per frame, in order
		blocked []bool   // expected blocked flag per frame
	}{
		{
			name:    "missing budget passes",
			budget:  "",
			frames:  []string{"tools/call"},
			blocked: []bool{false},
		},
		{
			name:    "malformed budget passes",
			budget:  "lots",
			frames:  []string{"tools/call"},
			blocked: []bool{false},
		},
		{
			name:    "exhausted budget blocks and steers",
			budget:  "1",
			frames:  []string{"tools/call", "tools/call", "tools/call"},
			blocked: []bool{false, true, true},
		},
		{
			name:    "initialize refills and admits again",
			budget:  "1",
			frames:  []string{"tools/call", "tools/call", "initialize", "tools/call"},
			blocked: []bool{false, true, false, false},
		},
		{
			name:    "explicit zero budget blocks",
			budget:  "0",
			frames:  []string{"tools/call"},
			blocked: []bool{true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetRouterForTest(t)
			t.Setenv("EVREN_API_KEY", "")
			t.Setenv("WALSY_SO_MODE", "")
			t.Setenv("WALSY_ROUTER_RETRY_BUDGET", tc.budget)
			routerReset()
			d := newDecisionBridge()
			for i, method := range tc.frames {
				resp, blocked := d.enforcingGate(gateFrame(method, i+1))
				if blocked != tc.blocked[i] {
					t.Fatalf("frame %d (%s) blocked=%v want %v resp=%s",
						i, method, blocked, tc.blocked[i], resp)
				}
				if !blocked {
					continue
				}
				var reply gateReply
				if err := json.Unmarshal(resp, &reply); err != nil {
					t.Fatalf("frame %d refusal not JSON: %v %q", i, err, resp)
				}
				if reply.Error.Code != -32001 {
					t.Fatalf("frame %d code = %d, want -32001", i, reply.Error.Code)
				}
				for _, want := range []string{"initialize", "RETRY_BUDGET", "alternative"} {
					if !strings.Contains(reply.Error.Message, want) {
						t.Fatalf("frame %d steer missing %q: %q", i, want, reply.Error.Message)
					}
				}
			}
		})
	}
}
