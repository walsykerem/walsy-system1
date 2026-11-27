package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type fix5RoundTrip func(*http.Request) (*http.Response, error)

func (f fix5RoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFix5_10StrictClaimVerdict(t *testing.T) {
	cases := []struct {
		name, raw string
		wantDone  bool
	}{
		{"dup-choice", `{"answers":{"claim":{"choice":"missing","confidence":0.9},"claim":{"choice":"done","confidence":0.9}}}`, false},
		{"case-choice", `{"answers":{"claim":{"choice":"missing","confidence":0.9,"Choice":"done"}}}`, false},
		{"two-docs", `{"answers":{"claim":{"choice":"missing","confidence":0.9}}}{"answers":{"claim":{"choice":"done","confidence":0.9}}}`, false},
		{"case-top", `{"ANSWERS":{"claim":{"choice":"done","confidence":0.9}}}`, false},
		{"over-1mib", `{"answers":{"claim":{"choice":"done","confidence":0.9},"pad":"` + strings.Repeat("x", 1<<20) + `"}}`, false},
		{"confidence-1.5", `{"answers":{"claim":{"choice":"done","confidence":1.5}}}`, false},
		{"good", `{"answers":{"claim":{"choice":"done","confidence":0.9}}}`, true},
		{"good-with-model", `{"model":"clef","answers":{"claim":{"choice":"done","confidence":0.9}}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := http.DefaultTransport
			defer func() { http.DefaultTransport = old }()
			http.DefaultTransport = fix5RoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(tc.raw)),
					Header:     http.Header{},
				}, nil
			})
			choice, _, err := ask(claim{id: "c1", text: "evidence"})
			done := err == nil && choice == "done"
			if done != tc.wantDone {
				t.Fatalf("malformed reply accepted as done: %s", tc.raw[:min(len(tc.raw), 160)])
			}
		})
	}
}
