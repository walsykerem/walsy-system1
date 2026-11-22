package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
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
	Choice   string // "yes" / "no" / "other"
	Conf     float64
	TimedOut bool
}

type soQuestion struct {
	Type     string            `json:"type"`
	Criteria map[string]string `json:"criteria,omitempty"`
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

// askSOQ asks one question whose label set is the caller's. A registry item
// brings its own instructions and labels, so the answer must be one of them.
func askSOQ(state string, q soQuestion) soDecision {
	key := soAPIKey()
	if key == "" {
		return soDecision{}
	}

	body, err := json.Marshal(struct {
		Model     string                `json:"model"`
		State     string                `json:"state"`
		Questions map[string]soQuestion `json:"questions"` // object, never array
	}{
		Model: "clef",
		State: state,
		Questions: map[string]soQuestion{
			"gate": {Type: "choice", Criteria: map[string]string{
				"yes":   "admit to the typed decision layer",
				"no":    "pass through unchanged",
				"other": "ambiguous; pass through",
			}},
		},
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

	var out struct {
		Answers map[string]struct {
			Choice     string  `json:"choice"`
			Confidence float64 `json:"confidence"`
		} `json:"answers"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		outcome = "bad_json"
		return soDecision{}
	}
	usage = out.Usage
	a, ok := out.Answers["gate"]
	if !ok || (a.Choice != "yes" && a.Choice != "no" && a.Choice != "other") {
		outcome = "unrouted"
		return soDecision{}
	}
	if math.IsNaN(a.Confidence) || a.Confidence < 0.8 || a.Confidence > 1 {
		outcome = "unrouted"
		return soDecision{}
	}
	outcome = "ok"
	return soDecision{Choice: a.Choice, Conf: a.Confidence}
}
