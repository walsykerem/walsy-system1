package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"time"
)

// Fixed SO contract: one endpoint, one 1500ms shot, no retry.
var (
	soEndpoint = "https://evren-llmapi.ssyz.org.tr/v1/systemone"
	soTimeout  = 1500 * time.Millisecond
	soClient   = &http.Client{}
)

// layer0 is the deterministic pre-gate. Raw prompts/headers never enter it.
type layer0 struct {
	NeedLabel  bool
	NoScratch  bool
	Safe       bool
	Reversible bool
}

// gate admits only the four-way conjunction; anything else passes through.
func gate(f layer0) bool {
	return f.NeedLabel && f.NoScratch && f.Safe && f.Reversible
}

// soDecision is the typed label. Conf drives admission only — never logged raw.
type soDecision struct {
	Choice   string // "yes" / "no" / "other"
	Conf     float64
	TimedOut bool
}

type soQuestion struct {
	Type     string            `json:"type"`
	Criteria map[string]string `json:"criteria,omitempty"`
}

// askSO sends only the already secret-cleaned user state. Server-sourced
// systemPrompt/message must be stripped by the caller before this point.
// Any failure is a degraded passthrough: zero decision, never a 503.
func askSO(state string) soDecision {
	key := os.Getenv("EVREN_API_KEY") // never logged
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

	resp, err := soClient.Do(req)
	if err != nil {
		d := soDecision{}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			d.TimedOut = true
		}
		return d
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return soDecision{}
	}

	var out struct {
		Answers map[string]struct {
			Choice     string  `json:"choice"`
			Confidence float64 `json:"confidence"`
		} `json:"answers"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return soDecision{}
	}
	a, ok := out.Answers["gate"]
	if !ok || (a.Choice != "yes" && a.Choice != "no" && a.Choice != "other") {
		return soDecision{}
	}
	if math.IsNaN(a.Confidence) || a.Confidence < 0.8 || a.Confidence > 1 {
		return soDecision{}
	}
	return soDecision{Choice: a.Choice, Conf: a.Confidence}
}
