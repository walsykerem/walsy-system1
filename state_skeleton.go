package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// maxSOStateChars is the hard per-request character cap on serialized SO state.
// shortcut: 4096 runes is a local ceiling, raise only after a measured sweep of
// real request sizes; the cap is enforced by marshalSOState, never by convention.
const maxSOStateChars = 4096

// PromptRef names a prompt body by id and SHA-256. The body is never stored in,
// or emitted from, the SO-bound skeleton.
type PromptRef struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

// StateSkeleton is the explicit SO-bound state shape. Prompt bodies are absent
// by construction; omitempty suppresses zero fields so no empty block is sent.
// shortcut: shape mirrors stateEnvelope without untrusted prompt text; extend
// only when SO asks for a field that is not already derivable from state.
type StateSkeleton struct {
	State      string      `json:"state,omitempty"`
	Method     string      `json:"method,omitempty"`
	NeedLabel  bool        `json:"need_label,omitempty"`
	NoScratch  bool        `json:"no_scratch,omitempty"`
	Safe       bool        `json:"safe,omitempty"`
	Reversible bool        `json:"reversible,omitempty"`
	OptIn      bool        `json:"opt_in,omitempty"`
	Phase      string      `json:"phase,omitempty"`
	Authority  bool        `json:"authority,omitempty"`
	Calibrated bool        `json:"calibrated,omitempty"`
	Item       string      `json:"item,omitempty"`
	Prompts    []PromptRef `json:"prompts,omitempty"`
}

// newPromptRef hashes a caller-supplied body and discards it. No body is kept.
func newPromptRef(id, body string) (PromptRef, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return PromptRef{}, fmt.Errorf("empty prompt id")
	}
	sum := sha256.Sum256([]byte(body))
	return PromptRef{ID: id, SHA256: hex.EncodeToString(sum[:])}, nil
}

// buildStateSkeleton converts caller-supplied prompt bodies into sorted refs.
// Bodies are hashed then dropped; only the prompt set's ids and digests enter
// the state. Sorting by id makes the shape deterministic across map iterations.
func buildStateSkeleton(state string, promptBodies map[string]string) (StateSkeleton, error) {
	s := StateSkeleton{State: state}
	ids := make([]string, 0, len(promptBodies))
	for id := range promptBodies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ref, err := newPromptRef(id, promptBodies[id])
		if err != nil {
			return StateSkeleton{}, err
		}
		s.Prompts = append(s.Prompts, ref)
	}
	return s, nil
}

// capSOState enforces the hard character cap on an SO-bound payload. Over-limit
// input is rejected, not partly sent: nothing is invented and no truncated
// state can reach the POST. Counting runes keeps the cap independent of encoding.
func capSOState(s string) (string, error) {
	n := utf8.RuneCountInString(s)
	if n > maxSOStateChars {
		return "", fmt.Errorf("so state %d chars exceeds cap %d", n, maxSOStateChars)
	}
	return s, nil
}

// marshalSOState serializes the skeleton and enforces the cap in one step, so a
// caller cannot POST an uncapped payload. It never emits a prompt body.
func marshalSOState(s StateSkeleton) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return capSOState(string(b))
}
