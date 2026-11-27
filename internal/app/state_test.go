package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testSecret = "sk-test-DO-NOT-LEAK-928374"

func statePaths(t *testing.T) (string, string, string, string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".walsy-system1")
	return dir,
		filepath.Join(dir, "models.txt"),
		filepath.Join(dir, "combo.txt"),
		filepath.Join(dir, "keys")
}

func requireMode0600(t *testing.T, path string) os.FileInfo {
	t.Helper()
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("%s mode = %o, want 600", filepath.Base(path), st.Mode().Perm())
	}
	if st.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s is a symlink", filepath.Base(path))
	}
	return st
}

func TestStateFileModesAndPersistence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, modelsPath, comboPath, keysDir := statePaths(t)
	_ = dir
	s, _ := dashboardTestServer(t)

	var sheetResp map[string]any
	code := postJSONTest(t, s.URL+"/api/providers/sheet", sheetProviderRequest{
		Name: "Evren System One", Key: testSecret, Models: []string{"m1", "m2", "m1"},
	}, &sheetResp)
	if code != http.StatusOK || sheetResp["ok"] != true {
		t.Fatalf("sheet: code=%d body=%v", code, sheetResp)
	}
	requireMode0600(t, modelsPath)
	keyPath := filepath.Join(keysDir, keyFileName("Evren System One", t))
	requireMode0600(t, keyPath)

	steps := []comboStepJSON{
		{Provider: "Evren System One", Model: "m2"},
		{Provider: "Evren System One", Model: "m1"},
	}
	code = postJSONTest(t, s.URL+"/api/combo", map[string]any{"steps": steps}, &sheetResp)
	if code != http.StatusOK || sheetResp["ok"] != true {
		t.Fatalf("combo save: code=%d body=%v", code, sheetResp)
	}
	requireMode0600(t, comboPath)

	modelData, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(modelData), "Evren System One\tm1\n") != 1 {
		t.Fatalf("models not deduped: %q", modelData)
	}

	models, err := loadModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(models["Evren System One"]) != 2 || models["Evren System One"][0] != "m1" {
		t.Fatalf("reloaded models = %v", models)
	}
	combo, err := loadCombo()
	if err != nil {
		t.Fatal(err)
	}
	if len(combo) != 2 || combo[0].Model != "m2" || combo[1].Model != "m1" {
		t.Fatalf("reloaded combo order = %v", combo)
	}
	if data, err := readStateFile(keyPath); err != nil || len(data) == 0 {
		t.Fatalf("key not set after save: %v", err)
	}

	var comboOut map[string][]comboStepJSON
	code = postJSONTest(t, s.URL+"/api/combo", map[string]any{"steps": []comboStepJSON{}}, nil)
	if code != http.StatusOK {
		t.Fatalf("clear combo: %d", code)
	}
	code = getJSONTest(t, s.URL+"/api/combo", &comboOut)
	if code != http.StatusOK || comboOut["steps"] == nil || len(comboOut["steps"]) != 0 {
		t.Fatalf("empty combo = %d %v", code, comboOut)
	}
}

func TestSecretsNeverInResponsesOrErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, _ := dashboardTestServer(t)

	var resp map[string]any
	code := postJSONTest(t, s.URL+"/api/providers/sheet", sheetProviderRequest{
		Name: "Evren System One", Key: testSecret, Models: []string{"m1"},
	}, &resp)
	if code != http.StatusOK {
		t.Fatalf("sheet: %d", code)
	}

	code = postJSONTest(t, s.URL+"/api/providers/sheet", sheetProviderRequest{
		Name: "Evren System One", Key: "bad\tkey", Models: []string{"m1"},
	}, &resp)
	if code != http.StatusBadRequest || resp["ok"] != false {
		t.Fatalf("bad key should fail: %d %v", code, resp)
	}

	var combo map[string]any
	code = getJSONTest(t, s.URL+"/api/combo", &combo)
	if code != http.StatusOK {
		t.Fatalf("combo get: %d", code)
	}

	req := dashRequest(http.MethodGet, "/api/providers", nil)
	rec := httptest.NewRecorder()
	NewDashboardHandler().ServeHTTP(rec, req)
	body := rec.Body.String()

	for _, blob := range []string{body, jsonString(t, combo), jsonString(t, resp)} {
		if strings.Contains(blob, testSecret) || strings.Contains(blob, "bad\tkey") {
			t.Fatalf("secret leaked in response: %q", blob)
		}
	}
	var view providersResponse
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range view.Providers {
		if p.Name == "Evren System One" {
			found = true
			if !p.KeySet {
				t.Fatal("keySet false after save")
			}
			if p.Models == nil || len(p.Models) != 1 || p.Models[0] != "m1" {
				t.Fatalf("models = %v", p.Models)
			}
		}
	}
	if !found {
		t.Fatal("provider missing from view")
	}

	code = postJSONTest(t, s.URL+"/api/providers/sheet", sheetProviderRequest{
		Name: "Evren System One", Key: "", Models: []string{"m1"}, ClearKey: true,
	}, &resp)
	if code != http.StatusOK {
		t.Fatalf("clear key: %d", code)
	}
	keyPath := filepath.Join(keysDirForHome(t), keyFileName("Evren System One", t))
	if _, err := os.Lstat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("key file should be removed, got %v", err)
	}
}

func TestStateFileRejectsSymlinkAndMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, modelsPath, comboPath, _ := statePaths(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(modelsPath, []byte("Evren System One\tm1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadModels(); err == nil {
		t.Fatal("wrong-mode models.txt must be rejected")
	}
	if err := os.Chmod(modelsPath, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadModels(); err != nil {
		t.Fatalf("0600 models.txt must load: %v", err)
	}

	if err := os.WriteFile(comboPath, []byte("Evren System One\tm1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "combo-link.txt")
	if err := os.Symlink(comboPath, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readStateFile(link); err == nil {
		t.Fatal("symlink must be rejected")
	}
}

func TestSavingModelsPreservesKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, _ := dashboardTestServer(t)
	var out map[string]any
	code := postJSONTest(t, s.URL+"/api/providers/sheet", sheetProviderRequest{
		Name: "Evren System One", Key: testSecret, Models: []string{"m1"},
	}, &out)
	if code != http.StatusOK {
		t.Fatalf("initial sheet: %d", code)
	}
	keysDir := keysDirForHome(t)
	keyPath := filepath.Join(keysDir, keyFileName("Evren System One", t))
	data, err := os.ReadFile(keyPath)
	if err != nil || string(data) != testSecret {
		t.Fatalf("initial key = %q, err = %v", data, err)
	}

	// Saving model selection with a blank key must keep the stored key.
	code = postJSONTest(t, s.URL+"/api/providers/sheet", sheetProviderRequest{
		Name: "Evren System One", Key: "", Models: []string{"m1", "m2"},
	}, &out)
	if code != http.StatusOK {
		t.Fatalf("model-only save: %d", code)
	}
	data, err = os.ReadFile(keyPath)
	if err != nil || string(data) != testSecret {
		t.Fatalf("key wiped by model save: %q, err = %v", data, err)
	}
	models, err := loadModels()
	if err != nil || len(models["Evren System One"]) != 2 {
		t.Fatalf("models not saved: %v, err = %v", models, err)
	}
}

func TestStateFileTooLargeRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, modelsPath, _, _ := statePaths(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelsPath, bytes.Repeat([]byte("x"), maxStateFileBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadModels(); err == nil {
		t.Fatal("oversized state file must be rejected")
	}
}

func TestStateDirSymlinkRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, "outside")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".walsy-system1")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := stateDir(); err == nil {
		t.Fatal("symlinked state dir must be rejected")
	}
}

func TestStateSymlinkWriteRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, modelsPath, _, _ := statePaths(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(home, "outside.txt")
	if err := os.WriteFile(outside, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(modelsPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, modelsPath); err != nil {
		t.Fatal(err)
	}
	if err := saveModels(map[string][]string{"Evren System One": {"m1"}}); err == nil {
		t.Fatal("write through symlink must be rejected")
	}
	data, err := os.ReadFile(outside)
	if err != nil || len(data) != 0 {
		t.Fatalf("symlink target modified: %q, err = %v", data, err)
	}
}

func TestDemoModelsNeverPersisted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, _ := dashboardTestServer(t)
	var resp map[string]any
	code := postJSONTest(t, s.URL+"/api/providers/sheet", sheetProviderRequest{
		Name: "DEMO/OK", Key: testSecret, Models: []string{"evil"},
	}, &resp)
	if code != http.StatusBadRequest || resp["ok"] != false {
		t.Fatalf("demo sheet must fail: %d %v", code, resp)
	}
	code = postJSONTest(t, s.URL+"/api/combo", map[string]any{
		"steps": []comboStepJSON{{Provider: "DEMO/OK", Model: "evil"}},
	}, &resp)
	if code != http.StatusBadRequest || resp["ok"] != false {
		t.Fatalf("demo combo must fail: %d %v", code, resp)
	}
	_, modelsPath, comboPath, _ := statePaths(t)
	for _, p := range []string{modelsPath, comboPath} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("demo wrote %s: %v", filepath.Base(p), err)
		}
	}
}

func getJSONTest(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode %s: %v (%q)", url, err, data)
	}
	return resp.StatusCode
}

func jsonString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func keysDirForHome(t *testing.T) string {
	t.Helper()
	_, _, _, dir := statePaths(t)
	return dir
}

func keyFileName(name string, t *testing.T) string {
	t.Helper()
	p, err := keyFilePath(name)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(p)
}

func soConfigPathForTest(t *testing.T) string {
	t.Helper()
	p, err := soConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func writeSOConfigForTest(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := soConfigPathForTest(t)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// validKey is the field shape check used by the sheet handler; "" is the
// keep/clear sentinel there, so storage boundaries reject empty instead.
func TestAddKeyRejectsMultilineKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	err := addKey("P", "sk-one\nsk-two")
	if err == nil || err.Error() != "invalid key" {
		t.Fatalf("addKey multiline err = %v, want invalid key", err)
	}
	if got := keyCount("P"); got != 0 {
		t.Fatalf("keyCount = %d, want 0", got)
	}
}

// A failed promote must leave neither a temp file nor a damaged state file.
func TestFailedWriteLeavesNoTempFile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("immutable-file rename rejection is darwin-only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, modelsPath, _, _ := statePaths(t)
	if err := saveModels(map[string][]string{"P": {"m1"}}); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("chflags", "uchg", modelsPath).CombinedOutput(); err != nil {
		t.Skipf("chflags unavailable: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("chflags", "nouchg", modelsPath).Run() })

	if err := saveModels(map[string][]string{"P": {"m2"}}); err == nil {
		t.Fatal("rename over immutable file should fail")
	}
	assertNoStateTemp(t, dir)
	models, err := loadModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(models["P"]) != 1 || models["P"][0] != "m1" {
		t.Fatalf("state after failed write = %v, want previous m1", models)
	}
}

// TestSheetModelReplacementPrunesCombo guards: a model dropped through the JSON
// sheet API must also leave the saved combo, exactly as the form path prunes on
// remove. The second sheet body has a non-nil models list, so it replaces the
// stored list rather than preserving it.
func TestSheetModelReplacementPrunesCombo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, _ := dashboardTestServer(t)

	if code := postJSONTest(t, s.URL+"/api/providers/sheet", sheetProviderRequest{
		Name: "Evren System One", Models: []string{"m1", "m2"},
	}, nil); code != http.StatusOK {
		t.Fatalf("sheet add: %d", code)
	}
	if code := postJSONTest(t, s.URL+"/api/combo", map[string]any{"steps": []comboStepJSON{
		{Provider: "Evren System One", Model: "m1"},
		{Provider: "Evren System One", Model: "m2"},
	}}, nil); code != http.StatusOK {
		t.Fatalf("combo save: %d", code)
	}
	if combo, err := loadCombo(); err != nil || len(combo) != 2 {
		t.Fatalf("combo before prune = %v (%v)", combo, err)
	}

	if code := postJSONTest(t, s.URL+"/api/providers/sheet", sheetProviderRequest{
		Name: "Evren System One", Models: []string{"m1"},
	}, nil); code != http.StatusOK {
		t.Fatalf("sheet replace: %d", code)
	}
	combo, err := loadCombo()
	if err != nil {
		t.Fatal(err)
	}
	if len(combo) != 1 || combo[0].Provider != "Evren System One" || combo[0].Model != "m1" {
		t.Fatalf("combo after sheet drop = %v, want only m1", combo)
	}
}

// TestComboRunStepsHonorWeightAndValidate guards: the run endpoint's inline steps
// must keep the submitted weight (the priority order) and apply the same validity
// checks the save endpoint does. A dropped weight would let the first-listed step
// win instead of the higher-weighted one.
func TestComboRunStepsHonorWeightAndValidate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	if err := saveCustomProvider("RUN/A", up.URL+"/a", "A_KEY"); err != nil {
		t.Fatal(err)
	}
	if err := saveCustomProvider("RUN/B", up.URL+"/b", "B_KEY"); err != nil {
		t.Fatal(err)
	}
	s, _ := dashboardTestServer(t)

	var out ComboOutcome
	code := postJSONTest(t, s.URL+"/api/combo/run", comboRequest{Steps: &[]comboStepJSON{
		{Provider: "RUN/A", Model: "m1", Weight: 1},
		{Provider: "RUN/B", Model: "m2", Weight: 9},
	}}, &out)
	if code != http.StatusOK {
		t.Fatalf("run status = %d", code)
	}
	if out.Winner != "RUN/B/m2" {
		t.Fatalf("winner = %q, want RUN/B/m2 (submitted weight not honored)", out.Winner)
	}

	code = postJSONTest(t, s.URL+"/api/combo/run", comboRequest{Steps: &[]comboStepJSON{
		{Provider: "RUN/A", Model: "bad\tmodel"},
	}}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid model status = %d, want 400", code)
	}
}

func assertNoStateTemp(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".walsy-state-") || strings.HasPrefix(e.Name(), ".providers-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// An operator hand-edit or a copy that rewrote keys.txt with CRLF must not
// silently drop every key: models.txt, combo.txt and config.txt already trim
// the same "\r", and validKey rejects it, so trimming here is lossless.
func TestLoadKeysToleratesCRLFFile(t *testing.T) {
	gapStateHome(t)
	const name = "CRLF Provider"
	if err := writeKeyList(name, []string{"key-a", "key-b"}); err != nil {
		t.Fatal(err)
	}
	path, err := keyFilePath(name)
	if err != nil {
		t.Fatal(err)
	}
	gapStateWriteState(t, path, []byte("key-a\r\nkey-b\r\n"))
	keys, err := loadKeys(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "key-a" || keys[1] != "key-b" {
		t.Fatalf("keys = %q, want [key-a key-b]", keys)
	}
	if n := keyCount(name); n != 2 {
		t.Fatalf("keyCount = %d, want 2", n)
	}
}
