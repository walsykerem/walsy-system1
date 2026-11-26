package app

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Test_capSystemPromptRuneBoundary: a multibyte rune straddles byte 8192; the
// capped prompt must stay valid UTF-8 and within the byte cap.
func Test_capSystemPromptRuneBoundary(t *testing.T) {
	s := strings.Repeat("a", maxSystemPrompt-1) + "\u4e16\u754c" + strings.Repeat("z", 16)
	capped, truncated := capSystemPrompt(s)
	if !truncated {
		t.Fatal("truncated=false for oversize input")
	}
	if len(capped) > maxSystemPrompt {
		t.Fatalf("capped length %d > %d", len(capped), maxSystemPrompt)
	}
	if !utf8.ValidString(capped) {
		t.Fatal("capped prompt is invalid UTF-8: rune split at byte cap")
	}
	if capped != strings.Repeat("a", maxSystemPrompt-1) {
		t.Fatalf("capped prompt did not back off to rune boundary: %q", capped)
	}
}
