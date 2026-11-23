package main

import (
	"strings"
	"testing"
)

func TestVerdict(t *testing.T) {
	cases := []struct {
		name    string
		a       soAnswer
		wantErr bool
	}{
		{"complete high", soAnswer{Choice: "complete", Confidence: 0.95}, false},
		{"incomplete high", soAnswer{Choice: "incomplete", Confidence: 0.95}, true},
		{"complete low", soAnswer{Choice: "complete", Confidence: 0.5}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verdict(tc.a, 0.8)
			if got := err != nil; got != tc.wantErr {
				t.Fatalf("verdict() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), tc.a.Choice) {
				t.Fatalf("error %q omits label %q", err, tc.a.Choice)
			}
		})
	}
}

func TestFinalStateClaimsOnlyLocalVerification(t *testing.T) {
	for _, want := range []string{"PENDING", "verified", "NOT a release decision"} {
		if !strings.Contains(finalStateFmt, want) {
			t.Fatalf("finalStateFmt omits %q; do not restore an evidence-free release claim", want)
		}
	}
}

func TestDecodeResponseWithRoutedModel(t *testing.T) {
	const raw = `{"model":"clef","routed_model":"Cloudflare/clef","answers":{"final":{"type":"choice","choice":"complete","confidence":0.97}},"usage":{"total_tokens":10}}`
	out, err := decodeResponse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if out.RoutedModel != "Cloudflare/clef" {
		t.Fatalf("routed_model = %q", out.RoutedModel)
	}
	if out.Answers["final"].Choice != "complete" {
		t.Fatalf("choice = %q", out.Answers["final"].Choice)
	}
}
