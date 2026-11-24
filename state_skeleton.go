package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// ErrStateTruncated marks an SO-bound state payload that exceeded the hard cap.
// Callers map it to the typed "input_truncated" reason before routing.
var ErrStateTruncated = errors.New("so state truncated")

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
// Policy A: a distilled decision shape (request/criteria/evidence) plus an
// omission list derived mechanically by soStateFromEnvelope, never caller-claimed.
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
	Request    string      `json:"request,omitempty"`
	Criteria   []string    `json:"criteria,omitempty"`
	Evidence   []string    `json:"evidence,omitempty"`
	Omitted    []string    `json:"omitted,omitempty"`
	Unverified []string    `json:"unverified,omitempty"`
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
		return "", fmt.Errorf("%w: so state %d chars exceeds cap %d", ErrStateTruncated, n, maxSOStateChars)
	}
	return s, nil
}

const (
	// soOmittedMarker is mechanical: only the cap path writes it, never a caller.
	soOmittedMarker = "mechanical: omitted over-cap state lines"
	// shortcut: 256-rune verbatim lines; raise when long single-line states must survive.
	maxSOEvidenceLineChars = 256
)

// distillSOState keeps verbatim lines under the same cap: keyword hits first,
// then leading lines. It clears the bulky fields and records the mechanical
// omission; no new wire field is introduced. A line that cannot fit verbatim is
// dropped, never cut mid-token.
func distillSOState(s StateSkeleton) (StateSkeleton, bool) {
	for _, o := range s.Omitted {
		if o == soOmittedMarker {
			return s, true
		}
	}
	var lines []string
	add := func(text string) {
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if line == "" || utf8.RuneCountInString(line) > maxSOEvidenceLineChars {
				continue
			}
			lines = append(lines, line)
		}
	}
	add(s.State)
	add(s.Request)
	for _, c := range s.Criteria {
		add(c)
	}
	for _, e := range s.Evidence {
		add(e)
	}
	if len(lines) == 0 {
		return StateSkeleton{}, false
	}
	seen := make(map[string]bool, len(lines))
	var ordered []string
	for pass := 0; pass < 2; pass++ {
		for _, line := range lines {
			if seen[line] || (pass == 0) != isLabelShaped(line) {
				continue
			}
			seen[line] = true
			ordered = append(ordered, line)
		}
	}
	out := s
	out.State, out.Request, out.Criteria, out.Evidence = "", "", nil, nil
	out.Omitted = append(append([]string(nil), s.Omitted...), soOmittedMarker)
	for _, line := range ordered {
		out.Evidence = append(out.Evidence, line)
		b, err := json.Marshal(out)
		if err == nil && utf8.RuneCountInString(string(b)) <= maxSOStateChars {
			continue
		}
		out.Evidence = out.Evidence[:len(out.Evidence)-1]
		break
	}
	if len(out.Evidence) == 0 {
		return StateSkeleton{}, false
	}
	return out, true
}

// marshalSOState serializes the skeleton and enforces the cap in one step, so a
// caller cannot POST an uncapped payload. Over-cap input is distilled into
// bounded verbatim Evidence with a mechanical omission marker; if nothing fits,
// capSOState's error stands. It never emits a prompt body.
func marshalSOState(s StateSkeleton) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	payload := string(b)
	_, overErr := capSOState(payload)
	if overErr == nil {
		return payload, nil
	}
	d, ok := distillSOState(s)
	if !ok {
		return "", overErr
	}
	b, err = json.Marshal(d)
	if err != nil {
		return "", err
	}
	payload = string(b)
	if _, err := capSOState(payload); err != nil {
		return "", err
	}
	return payload, nil
}
