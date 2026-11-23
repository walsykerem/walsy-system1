package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

//go:embed web/index.html web/style.css
var dashboardAssets embed.FS

type dashboardProvider struct {
	Name        string   `json:"name"`
	Base        string   `json:"base"`
	KeyEnv      string   `json:"key_env"`
	Baked       bool     `json:"baked"`
	Models      []string `json:"models"`
	KeySet      bool     `json:"keySet"`
	KeyCount    int      `json:"-"`
	KeySlots    []int    `json:"-"`
	DefaultBase string   `json:"-"`
	CanReset    bool     `json:"-"`
}

const defaultKeyEnv = "API_KEY"

type providersResponse struct {
	Providers  []dashboardProvider `json:"providers"`
	CustomSlot string              `json:"custom_slot"`
}

type saveProviderRequest struct {
	Name   string `json:"name"`
	Base   string `json:"base"`
	KeyEnv string `json:"keyEnv"`
}

type sheetProviderRequest struct {
	Name     string   `json:"name"`
	Key      string   `json:"key"`
	Models   []string `json:"models"`
	ClearKey bool     `json:"clearKey"`
}

type comboStepJSON struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Weight   int    `json:"weight,omitempty"`
}

type comboRequest struct {
	Names []string         `json:"names"`
	Steps *[]comboStepJSON `json:"steps"`
}

func isBakedName(name string) bool {
	for _, p := range bakedProviders {
		if p.Name == name {
			return true
		}
	}
	return false
}

// providerView tolerates a missing state file, but a corrupt or rejected state
// must surface in the API response instead of being hidden.
func providerView() (providersResponse, error) {
	reg, overrides, err := loadRegistryWithOverrides()
	if err != nil {
		return providersResponse{}, err
	}
	models, err := loadModels()
	if err != nil {
		return providersResponse{}, err
	}
	out := providersResponse{Providers: make([]dashboardProvider, 0, len(reg))}
	for _, p := range reg {
		baked := isBakedName(p.Name)
		list := models[p.Name]
		if list == nil {
			list = []string{}
		}
		count := keyCount(p.Name)
		out.Providers = append(out.Providers, dashboardProvider{
			Name: p.Name, Base: p.Base, KeyEnv: p.KeyEnv, Baked: baked,
			Models: list, KeySet: count > 0, KeyCount: count, KeySlots: keySlots(p.Name),
			DefaultBase: bakedBase(p.Name), CanReset: overrides[p.Name] && bakedBase(p.Name) != "",
		})
		if !baked {
			out.CustomSlot = p.Name
		}
	}
	return out, nil
}

func providerExists(view providersResponse, name string) bool {
	for _, p := range view.Providers {
		if p.Name == name {
			return true
		}
	}
	return false
}

func providerBase(view providersResponse, name string) string {
	for _, p := range view.Providers {
		if p.Name == name {
			return p.Base
		}
	}
	return ""
}

// saveCustomProvider upserts one registry line without touching the others.
// It validates through the real trust boundary before promoting the temp file;
// field values are never logged or echoed in errors.
func saveCustomProvider(name, base, keyEnv string) error {
	name, base, keyEnv = strings.TrimSpace(name), strings.TrimSpace(base), strings.TrimSpace(keyEnv)
	switch {
	case name == "":
		return fmt.Errorf("name required")
	case base == "":
		return fmt.Errorf("base required")
	case keyEnv == "":
		return fmt.Errorf("key env required")
	case isDemoName(name):
		return fmt.Errorf("name reserved")
	case strings.ContainsRune(name+base+keyEnv, ',') || hasControl(name+base+keyEnv):
		return fmt.Errorf("field has separator")
	case validProviderBase(base):
	default:
		return fmt.Errorf("base rejected: %s", baseRejectReason(base))
	}
	path, err := registryPath()
	if err != nil {
		return err
	}
	data, err := readRegistryData(path)
	if err != nil {
		return err
	}
	lines, err := parseRegistryLines(data)
	if err != nil {
		return err
	}
	next := provider{Name: name, Base: base, KeyEnv: keyEnv}
	found := false
	for i := range lines {
		if lines[i].Name == name {
			lines[i] = next
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, next)
	}
	return writeRegistryLines(lines)
}

func existingKeyEnv(name string) string {
	list, err := loadRegistry()
	if err != nil {
		return ""
	}
	for _, p := range list {
		if p.Name == name {
			return p.KeyEnv
		}
	}
	return ""
}

func uiProviderEndpoint(w http.ResponseWriter, r *http.Request) {
	form, ok := uiForm(w, r)
	if !ok {
		uiGo(w, r, "/", "INVALID FORM")
		return
	}
	name := strings.TrimSpace(form.Get("name"))
	target := sheetTarget(name)
	base := strings.TrimSpace(form.Get("base"))
	keyEnv := strings.TrimSpace(form.Get("keyEnv"))
	if name == "" || isDemoName(name) {
		uiGo(w, r, target, "INVALID PROVIDER")
		return
	}
	if reason := baseRejectReason(base); reason != "" {
		uiGo(w, r, target, "ENDPOINT REJECTED: "+reason)
		return
	}
	if keyEnv == "" {
		keyEnv = existingKeyEnv(name)
		if keyEnv == "" {
			keyEnv = defaultKeyEnv
		}
	}
	if err := saveCustomProvider(name, base, keyEnv); err != nil {
		uiGo(w, r, target, "ENDPOINT SAVE FAILED")
		return
	}
	uiGo(w, r, target, "ENDPOINT SAVED")
}

func uiProviderEndpointReset(w http.ResponseWriter, r *http.Request) {
	form, ok := uiForm(w, r)
	if !ok {
		uiGo(w, r, "/", "INVALID FORM")
		return
	}
	name := strings.TrimSpace(form.Get("name"))
	target := sheetTarget(name)
	if name == "" || isDemoName(name) {
		uiGo(w, r, target, "INVALID PROVIDER")
		return
	}
	if bakedBase(name) == "" {
		// A custom slot has no default: resetting it used to delete the provider
		// and leave its stored key behind on disk.
		uiGo(w, r, target, "NO DEFAULT ENDPOINT TO RESET")
		return
	}
	if err := removeRegistryEntry(name); err != nil {
		uiGo(w, r, target, "ENDPOINT RESET FAILED")
		return
	}
	uiGo(w, r, target, "ENDPOINT RESET")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	// JSON answer bodies are state snapshots too: a heuristic cache would serve
	// a stale provider list after a write.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg})
}

const maxRequestBytes = 1 << 16

// decodeRequest accepts only a single JSON value with the expected content
// type. A trailing value is rejected so a request cannot hide a second body.
func decodeRequest(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if dec.Decode(v) != nil {
		return false
	}
	return dec.Decode(&struct{}{}) == io.EOF
}

// stateMu serializes dashboard writes. Every mutating POST is a
// load-modify-save cycle, so parallel saves would lose updates.
var stateMu sync.Mutex

// unlockedPost lists POST routes the router leaves unlocked. The probe/run
// routes never write state; /ui/providers/keys takes stateMu itself for its
// add/remove/clear actions so a 2s key TEST cannot stall every other save.
// probeKeyStatus returns only the HTTP status class of a key probe. The key
// value is never returned, logged, or echoed; only Authorization/x-api-key
// headers carry it, and the body is discarded.
func probeKeyStatus(name string, index int) int {
	keys, err := loadKeys(name)
	if err != nil || index < 1 || index > len(keys) {
		return http.StatusUnauthorized
	}
	view, err := providerView()
	if err != nil {
		return http.StatusForbidden
	}
	base := providerBase(view, name)
	if !validProviderBase(base) {
		return http.StatusForbidden
	}
	req, err := http.NewRequest(http.MethodPost, base, strings.NewReader(`{"messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		return http.StatusForbidden
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+keys[index-1])
	req.Header.Set("x-api-key", keys[index-1])
	resp, err := keyTestClient.Do(req)
	if err != nil {
		return http.StatusForbidden
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode
}

var unlockedPost = map[string]bool{
	"/api/keytest":       true,
	"/api/combo/run":     true,
	"/ui/combo/run":      true,
	"/ui/build":          true,
	"/ui/providers/keys": true,
}

// isLoopbackName accepts only the names the dashboard is actually bound to.
func isLoopbackName(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// loopbackHost rejects DNS rebinding: a hostile name that resolves to
// 127.0.0.1 would otherwise pass any Origin==Host comparison.
func loopbackHost(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return isLoopbackName(strings.Trim(host, "[]"))
}

// sameOrigin requires the Origin to equal r.Host exactly. Any other localhost
// port is still a different origin, so a name allowlist would hand every local
// dev page the write API. r.Host is loopback-validated before this runs.
// Requests without an Origin (curl, CLI clients) are allowed.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return u.Host == r.Host
}

func serveAsset(w http.ResponseWriter, name, contentType string) {
	data, err := fs.ReadFile(dashboardAssets, "web/"+name)
	if err != nil {
		http.Error(w, "asset missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

var (
	errStateUnavailable = errors.New("state unavailable")
	errInvalidProvider  = errors.New("invalid provider")
	errInvalidField     = errors.New("invalid field")
	errInvalidModel     = errors.New("invalid model")
	errStateWriteFailed = errors.New("state write failed")
)

// applyProviderSheet is the single trust boundary for both JSON and form saves.
// preserveModels=true keeps the stored model list (form posts only edit the key).
func applyProviderSheet(req sheetProviderRequest, preserveModels bool) error {
	view, err := providerView()
	if err != nil {
		return errStateUnavailable
	}
	if req.Name == "" || isDemoName(req.Name) || !providerExists(view, req.Name) {
		return errInvalidProvider
	}
	// An empty key field means "keep", so only a non-empty one is validated.
	if (req.Key != "" && !validKey(req.Key)) || len(req.Models) > maxModelsPerProvider {
		return errInvalidField
	}
	models := make([]string, 0, len(req.Models))
	for _, m := range req.Models {
		m = strings.TrimSpace(m)
		if !validModel(m) {
			return errInvalidModel
		}
		models = append(models, m)
	}
	if !preserveModels {
		all, err := loadModels()
		if err != nil {
			return errStateUnavailable
		}
		previous := all[req.Name]
		all[req.Name] = models
		if err := saveModels(all); err != nil {
			return errStateWriteFailed
		}
		// A model the sheet dropped must leave the combo too, exactly as the
		// form path prunes on remove (uiProviderModel).
		kept := make(map[string]bool, len(models))
		for _, m := range models {
			kept[m] = true
		}
		for _, m := range previous {
			if !kept[m] {
				if err := pruneCombo(req.Name, m); err != nil {
					return errStateWriteFailed
				}
			}
		}
	}
	// An empty key field means "keep" unless the user explicitly asks to clear.
	// Append instead of replace so the other slots the keys UI manages survive.
	if req.ClearKey {
		if err := saveKey(req.Name, ""); err != nil {
			return errStateWriteFailed
		}
	} else if req.Key != "" {
		if err := addKey(req.Name, req.Key); err != nil {
			return errStateWriteFailed
		}
	}
	return nil
}

func handleProviderSheet(w http.ResponseWriter, r *http.Request) {
	var req sheetProviderRequest
	if !decodeRequest(w, r, &req) {
		writeAPIError(w, "invalid JSON")
		return
	}
	// A JSON body without "models" means key-only: keep the stored list instead
	// of replacing it with nil, which would silently wipe every model.
	if err := applyProviderSheet(req, req.Models == nil); err != nil {
		writeAPIError(w, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleComboGet(w http.ResponseWriter) {
	stored, err := loadCombo()
	if err != nil {
		writeAPIError(w, "state unavailable")
		return
	}
	steps := make([]comboStepJSON, 0, len(stored))
	for _, s := range stored {
		steps = append(steps, comboStepJSON{Provider: s.Provider, Model: s.Model, Weight: s.Weight})
	}
	writeJSON(w, http.StatusOK, map[string]any{"steps": steps})
}

func handleComboSave(w http.ResponseWriter, r *http.Request) {
	var req comboRequest
	if !decodeRequest(w, r, &req) || req.Steps == nil {
		writeAPIError(w, "invalid JSON")
		return
	}
	if len(*req.Steps) > maxComboSteps {
		writeAPIError(w, "too many steps")
		return
	}
	view, err := providerView()
	if err != nil {
		writeAPIError(w, "state unavailable")
		return
	}
	steps := make([]stateStep, 0, len(*req.Steps))
	for _, s := range *req.Steps {
		if s.Provider == "" || isDemoName(s.Provider) || !providerExists(view, s.Provider) ||
			(s.Model != "" && !validModel(s.Model)) {
			writeAPIError(w, "invalid step")
			return
		}
		steps = append(steps, stateStep{Provider: s.Provider, Model: s.Model, Weight: s.Weight})
	}
	if err := saveCombo(steps); err != nil {
		writeAPIError(w, "state write failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleComboRun(w http.ResponseWriter, r *http.Request) {
	var req comboRequest
	if !decodeRequest(w, r, &req) {
		writeAPIError(w, "invalid JSON")
		return
	}
	if len(req.Names) > maxComboSteps {
		writeAPIError(w, "too many names")
		return
	}
	if req.Steps != nil && len(*req.Steps) > maxComboSteps {
		writeAPIError(w, "too many steps")
		return
	}
	view, err := providerView()
	if err != nil {
		writeAPIError(w, "state unavailable")
		return
	}
	steps := make([]stateStep, 0)
	switch {
	case req.Steps != nil:
		for _, s := range *req.Steps {
			if s.Provider == "" || isDemoName(s.Provider) || !providerExists(view, s.Provider) ||
				(s.Model != "" && !validModel(s.Model)) {
				writeAPIError(w, "invalid step")
				return
			}
			steps = append(steps, stateStep{Provider: s.Provider, Model: s.Model, Weight: s.Weight})
		}
	case len(req.Names) > 0:
		for _, name := range req.Names {
			if name == "" || !providerExists(view, name) {
				writeAPIError(w, "invalid name")
				return
			}
			steps = append(steps, stateStep{Provider: name})
		}
	default:
		stored, err := loadCombo()
		if err != nil {
			writeAPIError(w, "state unavailable")
			return
		}
		steps = append(steps, stored...)
	}
	writeJSON(w, http.StatusOK, runComboSteps(view, steps))
}

// toComboSteps maps stored steps onto probe steps; separated from the run so
// ordering can be asserted without a network call.
func toComboSteps(view providersResponse, steps []stateStep) []ComboStep {
	runSteps := make([]ComboStep, 0, len(steps))
	for _, s := range steps {
		name := s.Provider
		if s.Model != "" {
			name += "/" + s.Model
		}
		step := ComboStep{Name: name, Base: providerBase(view, s.Provider), Weight: s.Weight}
		// First stored key of the provider; a rejected key then fails over to the
		// next step instead of being reported as a reachable winner.
		if keys, err := loadKeys(s.Provider); err == nil && len(keys) > 0 {
			step.Key = keys[0]
		}
		runSteps = append(runSteps, step)
	}
	return runSteps
}

func runComboSteps(view providersResponse, steps []stateStep) ComboOutcome {
	return RunCombo(toComboSteps(view, steps), 0)
}

// soReleaseComplete asks the same SO endpoint the decision hub uses for a
// typed "complete" verdict. A client-supplied verdict is never trusted.
func soReleaseComplete() bool {
	if soEndpoint == "" || baseRejectReason(soEndpoint) != "" {
		return false
	}
	key := soAPIKey()
	if key == "" {
		return false
	}
	body, err := json.Marshal(map[string]any{
		"model": "clef",
		"state": "stable release gate: verify all four attestations",
		"questions": map[string]soQuestion{
			"release": {Type: "choice", Criteria: map[string]string{
				"complete":   "every attestation verified",
				"incomplete": "one or more attestations missing",
			}},
		},
	})
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), soTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, soEndpoint, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)
	resp, err := soClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return false
	}
	var out struct {
		Answers map[string]soAnswer `json:"answers"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false
	}
	a, ok := out.Answers["release"]
	return ok && strings.EqualFold(a.Choice, "complete") && a.Confidence >= 0.8
}

// feedbackMutationReject is the single guard for every feedback mutation path.
func feedbackMutationReject(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "result": "MUTATION TOKEN REJECT"})
}

func NewDashboardHandler() http.Handler {
	sourceDir := findSourceDir()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r) {
			writeAPIError(w, "host not allowed")
			return
		}
		if r.Method == http.MethodPost && !unlockedPost[r.URL.Path] {
			stateMu.Lock()
			defer stateMu.Unlock()
		}
		switch r.URL.Path {
		case "/style.css":
			serveAsset(w, "style.css", "text/css; charset=utf-8")
		case "/api/logs":
			if r.Method != http.MethodGet {
				writeAPIError(w, "method not allowed")
				return
			}
			rows, err := auditLogRows()
			if err != nil {
				writeAPIError(w, "logs unavailable")
				return
			}
			writeJSON(w, http.StatusOK, rows)
		case "/api/logs/clear":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method or origin not allowed")
				return
			}
			if err := clearLogs(); err != nil {
				writeAPIError(w, "logs clear failed")
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		case "/api/keytest":
			if r.Method != http.MethodPost {
				writeAPIError(w, "method not allowed")
				return
			}
			var req struct {
				Provider string `json:"provider"`
				Model    string `json:"model"`
			}
			if !decodeRequest(w, r, &req) || req.Provider == "" {
				writeAPIError(w, "invalid request")
				return
			}
			models, err := loadModels()
			if err != nil || len(models[req.Provider]) == 0 {
				writeJSON(w, http.StatusOK, map[string]any{"status": 401, "result": "INVALID KEY", "winner": ""})
				return
			}
			selected := req.Model
			if selected == "" {
				selected = models[req.Provider][0]
			}
			// Test all keys for the selected model. A 401 stops; 403 moves on to
			// the next model, which the client selects and posts again.
			for k := 1; k <= keyCount(req.Provider); k++ {
				switch probeKeyStatus(req.Provider, k) {
				case http.StatusOK:
					writeJSON(w, http.StatusOK, map[string]any{"status": 200, "result": "WINNER " + selected, "winner": selected})
					return
				case http.StatusUnauthorized:
					writeJSON(w, http.StatusOK, map[string]any{"status": 401, "result": "INVALID KEY", "winner": ""})
					return
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"status": 403, "result": "FORBIDDEN", "winner": ""})
		case "/api/system-prompt":
			switch r.Method {
			case http.MethodGet:
				writeJSON(w, http.StatusOK, map[string]any{"prompt": loadSystemPrompt()})
			case http.MethodPost:
				var req struct {
					Prompt string `json:"prompt"`
				}
				if !decodeRequest(w, r, &req) {
					writeAPIError(w, "invalid request")
					return
				}
				prompt, truncated := capSystemPrompt(req.Prompt)
				if err := saveSystemPrompt(prompt); err != nil {
					writeAPIError(w, "prompt save failed")
					return
				}
				msg := "PROMPT SAVED"
				if truncated {
					msg = "PROMPT SAVED - TRUNCATED TO 8192 CHARS"
				}
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": msg})
			default:
				writeAPIError(w, "method not allowed")
			}
		case "/api/prompts/add":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method or origin not allowed")
				return
			}
			var req struct {
				Name string `json:"name"`
				Text string `json:"text"`
			}
			if !decodeRequest(w, r, &req) {
				writeAPIError(w, "invalid request")
				return
			}
			name, text := strings.TrimSpace(req.Name), strings.TrimSpace(req.Text)
			if name == "" || text == "" || len(name) > maxPromptName || len(text) > maxSystemPrompt {
				writeAPIError(w, "invalid request")
				return
			}
			if err := addPrompt(name, text); err != nil {
				writeAPIError(w, "prompt save failed")
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		case "/api/prompts":
			switch r.Method {
			case http.MethodGet:
				writeJSON(w, http.StatusOK, loadPrompts())
			case http.MethodPost:
				if !sameOrigin(r) {
					writeAPIError(w, "cross-origin request rejected")
					return
				}
				var req struct {
					Name string `json:"name"`
					Text string `json:"text"`
				}
				if !decodeRequest(w, r, &req) || req.Name == "" || req.Text == "" {
					writeAPIError(w, "invalid request")
					return
				}
				if err := addPrompt(req.Name, req.Text); err != nil {
					writeAPIError(w, "prompt save failed")
					return
				}
				writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
			default:
				writeAPIError(w, "method not allowed")
			}
		case "/api/health":
			if r.Method != http.MethodGet {
				writeAPIError(w, "method not allowed")
				return
			}
			writeJSON(w, http.StatusOK, healthFingerprint())
		case "/api/feedback":
			switch r.Method {
			case http.MethodGet:
				writeJSON(w, http.StatusOK, map[string]any{
					"ok": true, "readonly": true, "so_mode": soMode(),
				})
			case http.MethodPost:
				feedbackMutationReject(w)
			default:
				writeAPIError(w, "method not allowed")
			}
		case "/api/feedback/mutation", "/ui/feedback", "/api/feedback/token", "/api/operator/feedback":
			if r.Method == http.MethodPost && sameOrigin(r) {
				feedbackMutationReject(w)
				return
			}
			writeAPIError(w, "method not allowed")
		case "/api/release":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method or origin not allowed")
				return
			}
			var rel struct {
				Criteria map[string]bool `json:"criteria"`
			}
			if !decodeRequest(w, r, &rel) {
				writeAPIError(w, "invalid request")
				return
			}
			for _, name := range []string{"f1_build_equivalence", "verify_pass", "so_final_complete", "m1_approval"} {
				if !rel.Criteria[name] {
					writeJSON(w, http.StatusOK, map[string]any{"stable_release": false})
					return
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"stable_release": soReleaseComplete()})
		case "/api/providers":
			if r.Method == http.MethodPost {
				if !sameOrigin(r) {
					writeAPIError(w, "cross-origin request rejected")
					return
				}
				var req saveProviderRequest
				if !decodeRequest(w, r, &req) {
					writeAPIError(w, "invalid JSON")
					return
				}
				if err := saveCustomProvider(req.Name, req.Base, req.KeyEnv); err != nil {
					writeAPIError(w, err.Error())
					return
				}
				writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
				return
			}
			if r.Method != http.MethodGet {
				// PUT/DELETE used to fall through to the read path and answer 200.
				writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
				return
			}
			view, err := providerView()
			if err != nil {
				writeAPIError(w, "state unavailable")
				return
			}
			writeJSON(w, http.StatusOK, view)
		case "/api/providers/sheet":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method not allowed")
				return
			}
			handleProviderSheet(w, r)
		case "/api/combo":
			switch r.Method {
			case http.MethodPost:
				if !sameOrigin(r) {
					writeAPIError(w, "cross-origin request rejected")
					return
				}
				handleComboSave(w, r)
			case http.MethodGet:
				handleComboGet(w)
			default:
				// PUT/DELETE/PATCH used to fall through to the read path and answer 200.
				writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
			}
		case "/api/combo/run":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method not allowed")
				return
			}
			handleComboRun(w, r)
		case "/favicon.ico":
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent)
		case "/":
			if r.Method == http.MethodPost {
				// N1: JSON-RPC frames reach the same decision door as every other
				// transport; the dashboard itself stays read-only.
				body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
				if err == nil && len(body) <= maxRequestBytes && bytes.Contains(body, []byte(`"jsonrpc"`)) {
					_ = newDecisionBridge().decideFrame(body)
				}
			}
			if r.Method != http.MethodGet {
				writeAPIError(w, "method not allowed")
				return
			}
			uiMain(w, r)
		case "/ui/providers":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method or origin not allowed")
				return
			}
			uiAddProvider(w, r)
		case "/ui/providers/endpoint":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method or origin not allowed")
				return
			}
			uiProviderEndpoint(w, r)
		case "/ui/providers/reset":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method or origin not allowed")
				return
			}
			uiProviderEndpointReset(w, r)
		case "/ui/providers/remove":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method or origin not allowed")
				return
			}
			uiProviderRemove(w, r)
		case "/ui/providers/sheet":
			// Read-only: the sheet writes through /ui/providers/{endpoint,keys,models}.
			if r.Method != http.MethodGet {
				writeAPIError(w, "method not allowed")
				return
			}
			uiProviderSheet(w, r)
		case "/ui/providers/keys":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method or origin not allowed")
				return
			}
			uiProviderKey(w, r)
		case "/ui/providers/models":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method or origin not allowed")
				return
			}
			uiProviderModel(w, r)
		case "/ui/combo/add":
			if r.Method == http.MethodGet {
				uiComboAddForm(w, r)
			} else if r.Method == http.MethodPost && sameOrigin(r) {
				uiComboAdd(w, r)
			} else {
				writeAPIError(w, "method or origin not allowed")
			}
		case "/ui/combo/add/sheet":
			if r.Method != http.MethodGet {
				writeAPIError(w, "method not allowed")
				return
			}
			uiComboAddSheet(w, r)
		case "/ui/combo":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method or origin not allowed")
				return
			}
			uiComboMove(w, r)
		case "/ui/combo/sheet":
			if r.Method != http.MethodGet {
				writeAPIError(w, "method not allowed")
				return
			}
			uiComboSheet(w, r)
		case "/ui/logs":
			if r.Method != http.MethodGet {
				writeAPIError(w, "method not allowed")
				return
			}
			uiLogsSheet(w, r)
		case "/ui/prompts":
			if r.Method != http.MethodGet {
				writeAPIError(w, "method not allowed")
				return
			}
			uiPromptsSheet(w, r)
		case "/ui/prompts/add":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method or origin not allowed")
				return
			}
			uiPromptAdd(w, r)
		case "/ui/prompts/remove":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method or origin not allowed")
				return
			}
			uiPromptRemove(w, r)
		case "/ui/combo/run":
			// POST only: this is the iframe form target, never a standalone page.
			if r.Method != http.MethodPost {
				writeAPIError(w, "method not allowed")
				return
			}
			if !sameOrigin(r) {
				writeAPIError(w, "cross-origin request rejected")
				return
			}
			uiComboRun(w)
		case "/ui/build-target":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method or origin not allowed")
				return
			}
			handleUISetBuildTarget(w, r)
		case "/ui/build":
			if r.Method != http.MethodPost || !sameOrigin(r) {
				writeAPIError(w, "method or origin not allowed")
				return
			}
			handleUIBuild(w, r, sourceDir)
		default:
			writeNotFound(w, r)
		}
	})
}

// writeNotFound answers a browser with the styled page and anything else with
// the JSON error shape the API clients already parse.
func writeNotFound(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>NOT FOUND</title><link rel="stylesheet" href="/style.css"></head><body><main class="page"><h1 class="panel-title">NOT FOUND</h1><p class="hint">That address does not exist on this dashboard.</p><p><a class="btn" href="/">BACK TO DASHBOARD</a></p></main></body></html>`)
}

// RunDashboard serves the dashboard on loopback with explicit timeouts, matching
// the MCP HTTP server, so a slow client cannot pin a connection. The write
// deadline outlasts buildTimeout or a slow build's status page is cut off.
func RunDashboard(addr string) error {
	srv := &http.Server{
		Addr:              "127.0.0.1:" + strings.TrimPrefix(addr, ":"),
		Handler:           NewDashboardHandler(),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      buildTimeout + httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
	return srv.ListenAndServe()
}
