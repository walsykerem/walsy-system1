package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Fixed SO contract: one endpoint, one bounded shot, no retry. The bound is
// advisory only - client frames are flushed before this call.
//
// Evren is only one platform that speaks this contract, not a special case:
// any registry slot whose base matches soEndpoint's host supplies the key via
// soAPIKey, and WALSY_SO_ENDPOINT points the whole gate at another platform
// with no code change. Nothing below may branch on the platform name.
var (
	soEndpoint = soEndpointDefault()

	soEndpointDefaultBase = "https://evren-llmapi.ssyz.org.tr/v1/systemone"
	soTimeout             = 10 * time.Second
	soClient              = &http.Client{}
)

// soOracleIdentity is a RAM-only hash of the SO endpoint + key identity with
// x00 separation. Key bytes are never stored or printed. A bridge pins this at
// session start; enforcing mode abstains if the endpoint/key drifts later.
func soOracleIdentity() []byte {
	h := sha256.New()
	_, _ = h.Write([]byte(soEndpoint))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(soAPIKey()))
	return h.Sum(nil)
}

// soEndpointDefault resolves the gate endpoint: WALSY_SO_ENDPOINT wins, and
// the baked Evren URL is only the shipped default for a platform-neutral slot.
func soEndpointDefault() string {
	if v := os.Getenv("WALSY_SO_ENDPOINT"); v != "" {
		return v
	}
	return soEndpointDefaultBase
}

// layer0 is the deterministic pre-gate. Raw prompts/headers never enter it.
type layer0 struct {
	NeedLabel   bool
	NoScratch   bool
	Safe        bool
	Reversible  bool
	CleanRename bool
}

// gate admits only the five-way conjunction; anything else passes through.
func gate(f layer0) bool {
	return f.NeedLabel && f.NoScratch && f.Safe && f.Reversible && f.CleanRename
}

// declLine matches a declaration being removed by a diff.
var declLine = regexp.MustCompile(`(?m)^-[ \t]*(?:func|type|var|const)[ \t]+(?:\([^)]*\)[ \t]*)?([A-Za-z_]\w*)`)

// staleRename reports a diff that deletes a declared symbol without a CALLER
// SCAN section: the model sees the hunk, never the references outside it.
func staleRename(state string) bool {
	if declLine.FindStringSubmatch(state) == nil {
		return false
	}
	return !strings.Contains(state, "CALLER SCAN")
}

// soDecision is the typed label. Conf drives admission only - never logged raw.
type soDecision struct {
	Choice        string // "yes" / "no" / "other"
	Conf          float64
	TimedOut      bool
	Margin        float64 // p1-p2; Only used when the endpoint supplies probabilities.
	MarginKnown   bool
	AnswerType    string
	Score         float64
	Probabilities map[string]float64
	LatencyMS     int64
	ResponseID    string       // "none" - no endpoint request-id exists. Never fabricated.
	Trace         RankingTrace // separate decision record; never an audit/metric field
}

type soQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions,omitempty"`
	Criteria     map[string]string `json:"criteria,omitempty"`
	Legend       []string          `json:"legend,omitempty"`
	// Requires is a local capability check only. Never on the SO wire: no
	// shipped producer emits untrusted_text/tool_output into SO state today.
	Requires []string `json:"-"`
}

// soAPIKey resolves the SO provider key generically: a stored key on the
// provider whose base matches soEndpoint, then that provider's env var. The
// legacy EVREN_API_KEY env path stays for existing callers. Values never leave
// this process except as the x-api-key header.
func soAPIKey() string {
	if k := os.Getenv("EVREN_API_KEY"); k != "" {
		return k
	}
	host := ""
	if u, err := url.Parse(soEndpoint); err == nil {
		host = u.Hostname()
	}
	if host == "" {
		return ""
	}
	list, err := loadRegistry()
	if err != nil {
		list = bakedRegistry()
	}
	for _, pr := range list {
		if u, err := url.Parse(pr.Base); err != nil || !strings.EqualFold(u.Hostname(), host) {
			continue
		}
		if keys, err := loadKeys(pr.Name); err == nil {
			for _, k := range keys {
				if k != "" {
					return k
				}
			}
		}
		if pr.KeyEnv != "" {
			if k := os.Getenv(pr.KeyEnv); k != "" {
				return k
			}
		}
	}
	return ""
}

// askSO sends only the already secret-cleaned user state. Server-sourced
// systemPrompt/message must be stripped by the caller before this point.
// Any failure is a degraded passthrough: zero decision, never a 503.
func askSO(state string) soDecision {
	return askSOQ(state, soQuestion{Type: "choice", Criteria: map[string]string{
		"yes":   "admit to the typed decision layer",
		"no":    "pass through unchanged",
		"other": "ambiguous; pass through",
	}})
}

// gateQuestion defaults to the generic yes/no/other question so askSO keeps its
// exact behaviour; a registry item brings its own instructions and labels.
func gateQuestion(q soQuestion) soQuestion {
	if len(q.Criteria) > 0 {
		return q
	}
	return soQuestion{Type: "choice", Criteria: map[string]string{
		"yes":   "admit to the typed decision layer",
		"no":    "pass through unchanged",
		"other": "ambiguous; pass through",
	}}
}

// askSOQ is the single SO POST, called only by route() inside the decide hub.
// A registry item brings its own instructions and labels, so the answer must
// be one of them.
// A below-cut answer is kept, not discarded: the label and confidence come back
// so the caller can steer with them. The cut is applied by the caller (route's
// so-low-conf branch, toolsDecide's !admitted branch), never by this parser.
func askSOQ(state string, q soQuestion) soDecision {
	// One guard: an open breaker or an empty SO budget fails open with a zero
	// decision, before key resolution or any POST. Concurrency is capped at 2.
	if routerBreakerOpen("systemone") || routerBudgetLeft() <= 0 {
		return soDecision{}
	}
	select {
	case soSem <- struct{}{}:
		defer func() { <-soSem }()
	default:
		return soDecision{}
	}
	d := askSOQOnce(state, q)
	routerRecord("systemone", d.Choice != "" && !d.TimedOut)
	return d
}

// soSem is the concurrency cap on live SO calls; a full semaphore fails open.
var soSem = make(chan struct{}, 2)

// routerBudgetLeft reads the shared router budget under routerMu. It lives here
// so askSOQ needs no new mutex and gate.go owns no router policy state.
func routerBudgetLeft() int {
	routerMu.Lock()
	defer routerMu.Unlock()
	routerInitLocked()
	return routerState.budgetLeft
}

// askSOQOnce is the single bounded POST. Label validity is checked by the
// parser; the caller applies the confidence cut and all admission policy.
func askSOQOnce(state string, q soQuestion) soDecision {
	key := soAPIKey()
	if key == "" {
		return soDecision{}
	}

	body, err := json.Marshal(struct {
		Model     string                `json:"model"`
		State     string                `json:"state"`
		Questions map[string]soQuestion `json:"questions"` // object, never array
	}{
		Model:     "clef",
		State:     state,
		Questions: map[string]soQuestion{"gate": gateQuestion(q)},
	})
	if err != nil {
		return soDecision{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), soTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, soEndpoint, bytes.NewReader(body))
	if err != nil {
		return soDecision{}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)

	start := time.Now()
	outcome, respB := "error", 0
	var usage map[string]any
	defer func() {
		logCall("systemone", outcome, time.Since(start), len(body), respB, usage)
	}()

	resp, err := soClient.Do(req)
	if err != nil {
		d := soDecision{}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			outcome = "timeout"
			d.TimedOut = true
		}
		return d
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	respB = len(raw)
	if resp.StatusCode != http.StatusOK {
		outcome = fmt.Sprintf("http_%d", resp.StatusCode)
		return soDecision{}
	}
	if rerr != nil {
		return soDecision{}
	}

	// Strict SO boundary: <=1 MiB (matching the read cap), exactly one JSON
	// document, no duplicate/case-variant keys, top-level allowlist only.
	fields, err := strictObject(raw, 1<<20, map[string]bool{"model": true, "answers": true, "usage": true})
	if err != nil {
		outcome = "bad_json"
		return soDecision{}
	}
	var out struct {
		Answers map[string]soAnswer `json:"answers"`
		Usage   map[string]any      `json:"usage"`
	}
	if b, ok := fields["answers"]; ok {
		out.Answers = map[string]soAnswer{}
		_ = json.Unmarshal(b, &out.Answers)
	}
	if b, ok := fields["usage"]; ok {
		_ = json.Unmarshal(b, &out.Usage)
	}
	if out.Answers == nil {
		outcome = "bad_json"
		return soDecision{}
	}
	a, ok := out.Answers["gate"]
	if !ok || !validSOAnswer(a, q) {
		// YC5: a missing, non-choice, misrouted or malformed answer is an
		// invalid response, never a decision. No choice is taken verbatim.
		outcome = "invalid-response"
		return soDecision{}
	}
	d := soDecision{Choice: a.Choice, Conf: a.Confidence, AnswerType: a.Type,
		Score: a.Score, Probabilities: a.Probabilities,
		LatencyMS: time.Since(start).Milliseconds(), ResponseID: "none"}
	d.Trace = rankingTrace(d, "")
	if a.Noul {
		return d // an explicit no-answer carries no choice
	}
	if len(a.Probabilities) > 0 && a.Type == "score" {
		d.Margin, d.MarginKnown = answerMargin(a.Probabilities)
	}
	outcome = "ok"
	return d
}

// soAnswer is the whole decoded SO answer for one question. Strict decoding in
// askSOQOnce rejects duplicate/case-variant keys before this struct is filled;
// validSOAnswer then enforces the answer shape.
type soAnswer struct {
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Type          string             `json:"type"`
	Score         float64            `json:"score"`
	Noul          bool               `json:"noul"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// validSOAnswer requires the answer type the question asked for, a label from
// the question's own set, and a finite confidence in [0,1]. YC5: a response
// without type=="choice" is invalid, even if it carries a plausible choice.
func validSOAnswer(a soAnswer, q soQuestion) bool {
	if a.Type != "choice" || a.Choice == "" {
		return false
	}
	if _, inSet := q.Criteria[a.Choice]; len(q.Criteria) > 0 && !inSet {
		return false
	}
	return !math.IsNaN(a.Confidence) && a.Confidence >= 0 && a.Confidence <= 1
}

// answerMargin returns p1-p2 over a label probability map. Only the endpoint's
// own distribution counts; when absent the legacy confidence path is unchanged.
func answerMargin(p map[string]float64) (float64, bool) {
	if len(p) < 2 {
		return 0, false
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	best, second := 0.0, 0.0
	for _, k := range keys {
		v := p[k]
		if v > best {
			second, best = best, v
		} else if v > second {
			second = v
		}
	}
	return best - second, true
}
