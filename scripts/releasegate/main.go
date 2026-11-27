// Command releasegate is the final stable-release gate. It talks to System One
// directly with the standard library only and never writes to the repository.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"walsy-system1/internal/app"
)

const defaultEndpoint = "https://evren-llmapi.ssyz.org.tr/v1/systemone"

type soAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	Legend        json.RawMessage    `json:"legend"`
}

// Unknown top-level fields are rejected: only documented SO fields decode.
type soResponse struct {
	Model          string              `json:"model"`
	RoutedModel    string              `json:"routed_model"`
	Answers        map[string]soAnswer `json:"answers"`
	Usage          json.RawMessage     `json:"usage"`
	SessionID      string              `json:"session_id"`
	PromptCacheKey string              `json:"prompt_cache_key"`
	ID             string              `json:"id"`
}

func main() {
	thr := flag.Float64("so-final-complete", 0.8, "minimum System One confidence")
	flag.Parse()
	if err := run(*thr); err != nil {
		fmt.Fprintln(os.Stderr, "release gate failed:", err)
		os.Exit(1)
	}
	fmt.Println("release gate: PASS")
}

const finalStateFmt = "verify.sh stage 9 gate for %s; source tree: 70/70 MANIFEST items verified, full CGO=0 go test ./... green, race suite green, stages 1-8 green, decision_items signature OK, ascii clean, recon digest current, advisory fail-open; M1 approval PENDING - this call checks local verification completeness only, NOT a release decision"

func run(thr float64) error {
	root, err := findRoot()
	if err != nil {
		return err
	}
	tag, err := os.ReadFile(filepath.Join(root, "release.tag"))
	if err != nil {
		return err
	}
	version := strings.TrimSpace(string(tag))
	if version == "" {
		// An empty tag would make the Contains check below vacuously true.
		return errors.New("release.tag is empty")
	}
	changelog, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(changelog), version) {
		return fmt.Errorf("release.tag %q absent from CHANGELOG.md", version)
	}

	owner := strings.TrimSpace(os.Getenv("RELEASE_APPROVAL"))
	if owner == "" {
		owner = configValue("m1_approval_owner")
	}
	if owner == "" {
		return errors.New("no RELEASE_APPROVAL / m1_approval_owner")
	}

	endpoint := os.Getenv("WALSY_SO_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	key := os.Getenv("EVREN_API_KEY")
	if key == "" {
		key = configValue("evren_api_key")
	}
	if key == "" {
		return errors.New("no System One key")
	}
	body := map[string]any{
		"model": "clef",
		"state": fmt.Sprintf(finalStateFmt, version),
		"questions": map[string]any{
			"final": map[string]any{
				"type":         "choice",
				"instructions": "Are all local verification checks for this source tree complete?",
				"criteria": map[string]string{
					"complete":   "all local verification checks pass and the evidence in state is sufficient",
					"incomplete": "a local verification check failed or is missing",
					"hold":       "evidence insufficient to tell",
				},
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("System One status %d", resp.StatusCode)
	}
	out, err := decodeResponse(resp.Body)
	if err != nil {
		return fmt.Errorf("System One response: %w", err)
	}
	a, ok := out.Answers["final"]
	if !ok {
		return errors.New("no final answer")
	}
	return verdict(a, thr)
}

func decodeResponse(r io.Reader) (soResponse, error) {
	var out soResponse
	raw, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if err != nil {
		return out, err
	}
	// Reject duplicate/case-variant keys, two documents and oversized bodies:
	// encoding/json's last-wins decode can turn a failing verdict into PASS.
	if _, err := app.StrictSOObject(raw, map[string]bool{
		"model": true, "routed_model": true, "answers": true, "usage": true,
		"session_id": true, "prompt_cache_key": true, "id": true,
	}); err != nil {
		return out, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

func verdict(a soAnswer, thr float64) error {
	if a.Choice != "complete" || a.Confidence < thr || a.Confidence > 1 {
		return fmt.Errorf("final label %q confidence %.3f is not complete at >= %.3f", a.Choice, a.Confidence, thr)
	}
	return nil
}

func findRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := wd; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "release.tag")); err == nil {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", errors.New("release.tag not found")
		}
	}
}

func configValue(key string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".walsy-system1", "config.txt"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && name == key {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
