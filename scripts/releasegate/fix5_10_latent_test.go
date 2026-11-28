package main

import (
	"strings"
	"testing"
)

func TestFix5_10StrictGateVerdict(t *testing.T) {
	cases := []struct {
		name, raw string
		wantPass  bool
	}{
		{"dup-choice", `{"answers":{"final":{"choice":"incomplete","confidence":0.9},"final":{"choice":"complete","confidence":0.9}}}`, false},
		{"case-choice", `{"answers":{"final":{"choice":"incomplete","confidence":0.9,"Choice":"complete"}}}`, false},
		{"two-docs", `{"answers":{"final":{"choice":"incomplete","confidence":0.9}}}{"answers":{"final":{"choice":"complete","confidence":0.9}}}`, false},
		{"case-top", `{"ANSWERS":{"final":{"choice":"complete","confidence":0.9}}}`, false},
		{"over-1mib", `{"answers":{"final":{"choice":"complete","confidence":0.9},"pad":"` + strings.Repeat("x", 1<<20) + `"}}`, false},
		{"confidence-1.5", `{"answers":{"final":{"choice":"complete","confidence":1.5}}}`, false},
		{"good", `{"answers":{"final":{"type":"choice","choice":"complete","confidence":0.9}}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := decodeResponse(strings.NewReader(tc.raw))
			pass := err == nil && verdict(resp.Answers["final"], 0.8) == nil
			if pass != tc.wantPass {
				t.Fatalf("malformed reply accepted as PASS: %s", tc.raw[:min(len(tc.raw), 160)])
			}
		})
	}
}
