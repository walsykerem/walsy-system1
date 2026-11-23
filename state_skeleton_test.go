package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestStateSkeletonNoEmptyBlocks(t *testing.T) {
	b, err := marshalSOState(StateSkeleton{})
	if err != nil {
		t.Fatal(err)
	}
	if b != "{}" {
		t.Fatalf("empty skeleton = %q, want {}", b)
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

func TestCapSOStateCountsRunesNotBytes(t *testing.T) {
	s := strings.Repeat("\u00e9", maxSOStateChars)
	if utf8.RuneCountInString(s) != maxSOStateChars {
		t.Fatal("test setup wrong")
	}
	if _, err := capSOState(s); err != nil {
		t.Fatalf("rune-counted at-cap payload rejected: %v", err)
	}
}

func TestMarshalSOStateEnforcesCapOnPayload(t *testing.T) {
	s := StateSkeleton{State: strings.Repeat("y", maxSOStateChars)}
	if _, err := marshalSOState(s); err == nil {
		t.Fatal("marshal accepted an over-cap serialized payload")
	}
}
