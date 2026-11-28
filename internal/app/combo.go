package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const comboDefaultTimeout = 1200 * time.Millisecond

// ComboStep is one ordered failover entry. Base is the full URL; empty means skip.
// Weight is a priority hint: zero or negative means 1, higher is tried first.
// Key is the stored credential for the step; it is never copied into a result.
// Cost is an optional per-call price; lower cost is tried first.
// Model is the model id the probe asks for, in full (typesafe/jev-1.14).
type ComboStep struct {
	Name   string
	Base   string
	Weight int
	Cost   int
	Key    string
	Model  string
}

// ComboResult records one attempted step without ever carrying secrets.
type ComboResult struct {
	Name      string `json:"name"`
	Base      string `json:"base"`
	OK        bool   `json:"ok"`
	Status    string `json:"status"`
	Detail    string `json:"detail"`
	LatencyMS int64  `json:"latency_ms"`
}

// ComboOutcome is the deterministic result of the ordered failover loop.
type ComboOutcome struct {
	Winner string        `json:"winner"`
	Steps  []ComboResult `json:"steps"`
	Tried  int           `json:"tried"`
	Rounds int           `json:"rounds"`
}

// comboMaxRounds bounds the failover loop: the ordered list is walked first to
// last, then wrapped back to the first step, at most this often.
const comboMaxRounds = 3

// RunCombo walks the ordered steps from first to last, wraps back to the first
// and repeats, for at most comboMaxRounds passes or until one step answers.
func RunCombo(steps []ComboStep, timeout time.Duration) ComboOutcome {
	if timeout <= 0 {
		timeout = comboDefaultTimeout
	}
	out := ComboOutcome{Steps: make([]ComboResult, 0, len(steps)), Tried: len(steps)}
	order := comboOrder(steps)
	for round := 1; round <= comboMaxRounds && len(order) > 0; round++ {
		out.Rounds = round
		for _, i := range order {
			r := probeComboStep(steps[i], timeout)
			out.Steps = append(out.Steps, r)
			if r.OK {
				out.Winner = r.Name
				return out
			}
		}
	}
	return out
}

// probeComboStep attempts one entry. It never carries a key into the result.
func probeComboStep(s ComboStep, timeout time.Duration) ComboResult {
	r := ComboResult{Name: s.Name, Base: s.Base}
	start := time.Now()
	body := soProbeBody(comboModel(s))
	respB := 0
	// Every probe is an outbound call; the LOGS sheet counts them too.
	defer func() { logCall("combo", r.Status, time.Since(start), len(body), respB, nil) }()
	if s.Base == "" {
		r.Status = "no_base"
		r.Detail = "empty base"
		return r
	}
	if routerBreakerOpen(s.Name) {
		r.Status = "circuit_open"
		r.Detail = "recent failures"
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Base, strings.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		if s.Key != "" {
			req.Header.Set("Authorization", "Bearer "+s.Key)
			req.Header.Set("x-api-key", s.Key)
		}
	}
	if err != nil {
		cancel()
		routerRecord(s.Name, false)
		r.Status, r.Detail = "error", "invalid base"
		r.LatencyMS = time.Since(start).Milliseconds()
		return r
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		routerRecord(s.Name, false)
		r.Status, r.Detail = "error", "request failed"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			r.Status, r.Detail = "timeout", "timeout"
		}
		r.LatencyMS = time.Since(start).Milliseconds()
		return r
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	respB = len(bodyBytes)
	resp.Body.Close()
	cancel()
	// A probe answers one question: is this credential usable on this endpoint?
	// Only a 2xx proves it. 401/403 reject the key; 400/404/422 mean the
	// request or the model was wrong, not that the key is good, so they fail
	// over to the next step instead of winning.
	r.Status = resp.Status
	r.LatencyMS = time.Since(start).Milliseconds()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// shortcut: body validation applies only to SO-shaped endpoints; other
		// 2xx probes stay reachability-only to preserve existing behavior.
		if isSOEndpoint(s.Base) && !validSOProbeBody(bodyBytes) {
			r.Detail = "invalid system one response"
			break
		}
		r.OK = true
		r.Detail = "endpoint reachable"
		if s.Key != "" {
			r.Detail = "key accepted"
		}
	case s.Key != "" && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden):
		r.Detail = "key rejected"
	case s.Key == "" && resp.StatusCode < 500 && resp.StatusCode != http.StatusNotFound:
		// Without a credential the probe can only prove reachability: POST-only
		// endpoints answer 405/401/422, which still shows a live listener.
		r.OK = true
		r.Detail = "endpoint reachable"
	default:
		r.Detail = "non-2xx response"
	}
	routerRecord(s.Name, r.OK)
	return r
}

// isSOEndpoint matches the System One wire path. A malformed 200 from such an
// endpoint is not admission: the probe asked for a typed decision and got no
// answers object back. Keyless/non-SO probes stay reachability-only.
func isSOEndpoint(base string) bool {
	u, err := url.Parse(base)
	return err == nil && strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), "/systemone")
}

// validSOProbeBody requires a JSON object with at least one answers object,
// matching the strict SO decode used by the decision hub.
func validSOProbeBody(body []byte) bool {
	var v map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(body), &v); err != nil {
		return false
	}
	raw, ok := v["answers"]
	if !ok || len(raw) == 0 {
		return false
	}
	var answers map[string]json.RawMessage
	return json.Unmarshal(raw, &answers) == nil && len(answers) > 0
}

// comboModel returns the model id a step probes: the step's own model, else
// the last segment of its name, else the System One default. The full model
// id matters - "typesafe/jev-1.14" is a different model than "jev-1.14".
func comboModel(s ComboStep) string {
	if s.Model != "" {
		return s.Model
	}
	if i := strings.LastIndexByte(s.Name, '/'); i >= 0 && i+1 < len(s.Name) {
		return s.Name[i+1:]
	}
	return soProbeModel
}

// comboOrder returns step indices sorted by ascending cost then descending
// weight, stable for ties.
func comboOrder(steps []ComboStep) []int {
	order := make([]int, len(steps))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		ca, cb := steps[order[i]].Cost, steps[order[j]].Cost
		if ca != cb {
			return ca < cb
		}
		a, b := steps[order[i]].Weight, steps[order[j]].Weight
		if a <= 0 {
			a = 1
		}
		if b <= 0 {
			b = 1
		}
		return a > b
	})
	return order
}
