package main

import (
	"fmt"
	"io"
	"net/http"
	"testing"
)

// F3: fork invariance. A decision's label/choice comes only from Answers["gate"];
// sibling answers - even with higher confidence and opposite labels - never steer it.
func TestFlideF3GateOnlyAnswerWins(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "advisory")
	routerReset()
	t.Cleanup(routerReset)

	for _, tc := range []struct{ gate, sibling, want string }{
		{"yes", "no-go", "yes"},
		{"no", "go", "no"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			body := fmt.Sprintf(`{"answers":{"gate":{"type":"choice","choice":%q,"confidence":0.92},`+
				`"go":{"type":"choice","choice":%q,"confidence":0.99},`+
				`"m1_approval":{"type":"choice","choice":"not-approved","confidence":0.99}}}`, tc.gate, tc.sibling)
			withSO(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			})
			q := soQuestion{Type: "choice", Criteria: map[string]string{"yes": "admit", "no": "pass"}}
			d := askSOQ(`{"state":"m1 approval final check","need_label":true}`, q)
			if d.Choice != tc.want || d.Conf != 0.92 {
				t.Fatalf("choice=%q conf=%v, want %q/0.92", d.Choice, d.Conf, tc.want)
			}
		})
	}
}
