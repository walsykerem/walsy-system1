package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStateSkeletonHasPromptIDsAndHashesOnly(t *testing.T) {
	const bodyA = "first untrusted body"
	const bodyB = "second untrusted body"
	s, err := buildStateSkeleton("decide: x", map[string]string{"beta": bodyB, "alpha": bodyA})
	if err != nil {
		t.Fatal(err)
	}
	if s.State != "decide: x" {
		t.Fatalf("state = %q", s.State)
	}
	if len(s.Prompts) != 2 {
		t.Fatalf("prompts = %+v", s.Prompts)
	}
	if s.Prompts[0].ID != "alpha" || s.Prompts[1].ID != "beta" {
		t.Fatalf("prompt ids not sorted: %+v", s.Prompts)
	}
	for i, want := range []string{bodyA, bodyB} {
		sum := sha256.Sum256([]byte(want))
		if got := s.Prompts[i].SHA256; got != hex.EncodeToString(sum[:]) {
			t.Fatalf("prompt %d hash = %q", i, got)
		}
	}
	var obj map[string]any
	b, err := marshalSOState(s)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal([]byte(b), &obj) != nil {
		t.Fatalf("bad json: %s", b)
	}
	if _, ok := obj["text"]; ok {
		t.Fatal("serialized skeleton has prompt body text")
	}
	if strings.Contains(b, bodyA) || strings.Contains(b, bodyB) {
		t.Fatalf("serialized skeleton leaked prompt body: %s", b)
	}
	if strings.Contains(b, "body") || strings.Contains(b, "prompts\" : {}") {
		t.Fatalf("serialized skeleton has empty/body block: %s", b)
	}
}

func TestCapSOStateRejectsOverLimitDeterministically(t *testing.T) {
	over := strings.Repeat("x", maxSOStateChars+1)
	want := ""
	var first string
	for i := 0; i < 2; i++ {
		got, err := capSOState(over)
		if err == nil {
			t.Fatal("over-cap payload accepted")
		}
		if i == 0 {
			first = got
			want = err.Error()
		} else {
			if got != first {
				t.Fatal("cap result not deterministic")
			}
		}
	}
	if first != "" {
		t.Fatalf("over-cap returned payload %q, want empty", first)
	}
	if !strings.Contains(want, "exceeds cap") {
		t.Fatalf("cap error = %q", want)
	}
	if _, err := capSOState(strings.Repeat("x", maxSOStateChars)); err != nil {
		t.Fatalf("at-cap payload rejected: %v", err)
	}
}

// Policy A: the SO-bound shape carries request/criteria/evidence and a
// mechanically derived omission list. Caller-declared omitted/unverified must
// never be trusted; only the audit of this state names what is missing.
// Over-cap state is distilled, not dropped: verbatim keyword lines first,
// bounded by the same cap, with a mechanical omission marker. Lines that cannot
// fit verbatim still take the typed truncation error.
func TestMarshalSOStateDistillsOverCapVerbatim(t *testing.T) {
	lines := make([]string, 80)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %03d: %s", i, strings.Repeat("detail", 12))
	}
	lines[40] = "decide: classify this risk"
	b, err := marshalSOState(StateSkeleton{State: strings.Join(lines, "\n"), Method: "tools/call"})
	if err != nil {
		t.Fatal(err)
	}
	if utf8.RuneCountInString(b) > maxSOStateChars {
		t.Fatalf("distilled payload = %d chars: %s", utf8.RuneCountInString(b), b)
	}
	var obj map[string]any
	if json.Unmarshal([]byte(b), &obj) != nil {
		t.Fatalf("bad json: %s", b)
	}
	if _, ok := obj["state"]; ok {
		t.Fatalf("over-cap state survived: %s", b)
	}
	ev, ok := obj["evidence"].([]any)
	if !ok || len(ev) == 0 {
		t.Fatalf("evidence missing: %s", b)
	}
	if ev[0] != "decide: classify this risk" {
		t.Fatalf("keyword line not first: %v", ev[0])
	}
	om, ok := obj["omitted"].([]any)
	if !ok {
		t.Fatalf("omitted missing: %s", b)
	}
	marked := false
	for _, v := range om {
		if v == soOmittedMarker {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("mechanical omission marker missing: %s", b)
	}

	small := StateSkeleton{State: "decide: small", Method: "tools/call"}
	want, _ := json.Marshal(small)
	got, err := marshalSOState(small)
	if err != nil || got != string(want) {
		t.Fatalf("under-cap payload changed: %q, %v; want %s", got, err, want)
	}
	if _, err := marshalSOState(StateSkeleton{State: strings.Repeat("y", maxSOStateChars)}); !errors.Is(err, ErrStateTruncated) {
		t.Fatalf("over-cap without usable line: %v, want ErrStateTruncated", err)
	}
}
