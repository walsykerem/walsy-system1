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

// State quotes recorded local evidence verbatim; it never re-runs or asserts a
// check this process did not perform.
const finalStateFmt = "verify.sh stage 9 gate for %s; local evidence files verified present (recorded outputs, NOT re-run by this gate):\n%s\nM1 approval owner: %s; approval PENDING - this call checks local verification evidence only, NOT a release decision; advisory fail-open"

var evidenceFiles = []string{
	"scripts/finalgate/evidence/FINALGATE-R9.txt",
	"scripts/finalgate/evidence/K12.txt",
	"scripts/finalgate/evidence/K15.txt",
	"scripts/finalgate/evidence/K16.txt",
}

// localEvidence fails if any required evidence file is missing or unreadable,
// so the gate never submits a verdict request without all of its evidence.
func localEvidence(root string) (string, error) {
	var b strings.Builder
	for _, rel := range evidenceFiles {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return "", fmt.Errorf("required evidence %s unavailable: %w", rel, err)
		}
		fmt.Fprintf(&b, "[%s]\n%s\n", rel, strings.TrimSpace(string(raw)))
	}
	return b.String(), nil
}

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

	evidence, err := localEvidence(root)
	if err != nil {
		return err
	}

	owner, err := approvalOwner()
	if err != nil {
		return err
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
		"state": fmt.Sprintf(finalStateFmt, version, evidence, owner),
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
	if err := checkModel(out); err != nil {
		return err
	}
	return verdict(a, thr)
}

// approvalOwner requires the same named owner from env and config: a mismatch
// means the release is not approved, not a fallback to either side.
func approvalOwner() (string, error) {
	envOwner := strings.TrimSpace(os.Getenv("RELEASE_APPROVAL"))
	cfgOwner := strings.TrimSpace(configValue("m1_approval_owner"))
	if envOwner == "" || cfgOwner == "" {
		return "", errors.New("approval requires both RELEASE_APPROVAL and m1_approval_owner")
	}
	if envOwner != cfgOwner {
		return "", errors.New("approval mismatch: RELEASE_APPROVAL and m1_approval_owner differ")
	}
	return envOwner, nil
}

func checkModel(out soResponse) error {
	for _, got := range []string{out.Model, out.RoutedModel} {
		if got == "" {
			continue
		}
		last := got
		if i := strings.LastIndex(got, "/"); i >= 0 {
			last = got[i+1:]
		}
		if last != "clef" {
			return fmt.Errorf("System One model %q is not clef", got)
		}
	}
	return nil
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
	if a.Type != "choice" {
		return fmt.Errorf("final answer type %q is not choice", a.Type)
	}
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
