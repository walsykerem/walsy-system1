package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// Resource ceilings for dashboard writes. Bounds are enforced at the API layer.
const (
	maxModelsPerProvider = 50
	maxComboSteps        = 50
	maxProviderNameLen   = 128
	maxKeysPerProvider   = 8
	maxStateFileBytes    = 1 << 20
)

// stateStep is one persisted provider/model pair. Model may be empty.
type stateStep struct {
	Provider string
	Model    string
	// Weight is the failover priority hint; 0 keeps the historical 2-column
	// line so an old combo.txt stays byte-identical.
	Weight int
}

func isDemoName(name string) bool {
	return strings.HasPrefix(name, "DEMO/")
}

// validKey is the field shape check used by the sheet handler, where "" means
// keep/clear. Storage boundaries (addKey, saveKey) reject empty separately.
// loadKeys splits on "\n", so separators must never get in.
func validKey(k string) bool {
	if len(k) > 4096 || (k != "" && strings.TrimSpace(k) == "") || strings.ContainsAny(k, "\r\n") {
		return false
	}
	for _, r := range k {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// hasControl guards the comma registry: names are stored one per line and
// models one per TAB field, so a control byte would split or drop the record.
func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func validModel(m string) bool {
	if len(m) < 1 || len(m) > 256 {
		return false
	}
	for _, r := range m {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func stateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home unavailable")
	}
	dir := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("state dir failed")
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0700 {
		return "", fmt.Errorf("state dir rejected")
	}
	return dir, nil
}

// readStateFile applies the same trust boundary as loadRegistryFrom.
// A missing file is an empty state, not an error. Paths stay out of errors.
func readStateFile(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("state file stat failed")
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("state file symlink rejected")
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("state file not regular")
	}
	if st.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("state file mode rejected")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("state file read failed")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStateFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("state file read failed")
	}
	if len(data) > maxStateFileBytes {
		return nil, fmt.Errorf("state file too large")
	}
	return data, nil
}

// syncDir flushes the directory entry so a completed rename survives power
// loss. Only EINVAL (platform cannot fsync a directory) is tolerated.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) && errors.Is(pe.Err, syscall.EINVAL) {
			return nil
		}
		return err
	}
	return nil
}

// removeStateFile deletes a state file and flushes the directory so a cleared
// key cannot reappear after a crash.
func removeStateFile(path string) error {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return syncDir(filepath.Dir(path))
}

// writeStateFile promotes a 0600 temp file, never following an existing symlink.
func writeStateFile(path string, data []byte) error {
	dir, err := stateDir()
	if err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if parent != dir && parent != filepath.Join(dir, "keys") {
		return fmt.Errorf("state path rejected")
	}
	if parent != dir {
		if err := os.MkdirAll(parent, 0700); err != nil {
			return fmt.Errorf("keys dir failed")
		}
		if st, err := os.Lstat(parent); err != nil || !st.IsDir() ||
			st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0700 {
			return fmt.Errorf("keys dir rejected")
		}
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return fmt.Errorf("state target rejected")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("state target stat failed")
	}
	f, err := os.CreateTemp(dir, ".walsy-state-*")
	if err != nil {
		return fmt.Errorf("state temp failed")
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return fmt.Errorf("state temp mode failed")
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("state temp write failed")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("state temp sync failed")
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("state temp close failed")
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("state replace failed")
	}
	// The rename is atomic and already durable enough to be seen; a failed
	// directory sync must not be reported as a write failure after the fact.
	_ = syncDir(parent)
	if parent != dir {
		_ = syncDir(dir)
	}
	return nil
}

func loadModels() (map[string][]string, error) {
	out := make(map[string][]string)
	dir, err := stateDir()
	if err != nil {
		return out, err
	}
	data, err := readStateFile(filepath.Join(dir, "models.txt"))
	if err != nil {
		return out, err
	}
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 2)
		if len(f) != 2 || f[0] == "" || len(f[0]) > maxProviderNameLen ||
			!validModel(f[1]) || isDemoName(f[0]) {
			continue
		}
		key := f[0] + "\t" + f[1]
		if seen[key] {
			continue
		}
		seen[key] = true
		out[f[0]] = append(out[f[0]], f[1])
	}
	return out, nil
}

func saveModels(models map[string][]string) error {
	dir, err := stateDir()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(models))
	for name := range models {
		if !isDemoName(name) && name != "" && len(name) <= maxProviderNameLen {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var b strings.Builder
	seen := make(map[string]bool)
	for _, name := range names {
		for _, m := range models[name] {
			if !validModel(m) {
				continue
			}
			key := name + "\t" + m
			if seen[key] {
				continue
			}
			seen[key] = true
			b.WriteString(name)
			b.WriteByte('\t')
			b.WriteString(m)
			b.WriteByte('\n')
		}
	}
	return writeStateFile(filepath.Join(dir, "models.txt"), []byte(b.String()))
}

func loadCombo() ([]stateStep, error) {
	out := make([]stateStep, 0)
	dir, err := stateDir()
	if err != nil {
		return out, err
	}
	data, err := readStateFile(filepath.Join(dir, "combo.txt"))
	if err != nil {
		return out, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 3)
		if len(f) < 2 || f[0] == "" || len(f[0]) > maxProviderNameLen || isDemoName(f[0]) ||
			(f[1] != "" && !validModel(f[1])) {
			continue
		}
		step := stateStep{Provider: f[0], Model: f[1]}
		if len(f) == 3 {
			if w, err := strconv.Atoi(f[2]); err == nil {
				step.Weight = w
			}
		}
		out = append(out, step)
	}
	return out, nil
}

// pruneCombo drops combo steps for one model, or every step of a provider when
// model is empty. A removed provider/model must never linger in the combo run.
func pruneCombo(provider, model string) error {
	steps, err := loadCombo()
	if err != nil {
		return err
	}
	kept := make([]stateStep, 0, len(steps))
	for _, s := range steps {
		if s.Provider == provider && (model == "" || s.Model == model) {
			continue
		}
		kept = append(kept, s)
	}
	if len(kept) == len(steps) {
		return nil
	}
	return saveCombo(kept)
}

func saveCombo(steps []stateStep) error {
	dir, err := stateDir()
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, s := range steps {
		if s.Provider == "" || isDemoName(s.Provider) || (s.Model != "" && !validModel(s.Model)) {
			continue
		}
		b.WriteString(s.Provider)
		b.WriteByte('\t')
		b.WriteString(s.Model)
		if s.Weight != 0 {
			b.WriteByte('\t')
			b.WriteString(strconv.Itoa(s.Weight))
		}
		b.WriteByte('\n')
	}
	return writeStateFile(filepath.Join(dir, "combo.txt"), []byte(b.String()))
}

func keyFilePath(name string) (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(dir, "keys", hex.EncodeToString(sum[:8])), nil
}

func saveKey(name, key string) error {
	path, err := keyFilePath(name)
	if err != nil {
		return err
	}
	if key == "" {
		if err := removeStateFile(path); err != nil {
			return fmt.Errorf("key remove failed")
		}
		return nil
	}
	if !validKey(key) {
		return fmt.Errorf("invalid key")
	}
	return writeStateFile(path, []byte(key))
}

// loadKeys reads stored keys without ever returning them to callers that log
// or render state. Existing single-key files are treated as a one-item list.
func loadKeys(name string) ([]string, error) {
	path, err := keyFilePath(name)
	if err != nil {
		return nil, err
	}
	data, err := readStateFile(path)
	if err != nil {
		return nil, err
	}
	keys := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" && validKey(line) {
			keys = append(keys, line)
		}
	}
	return keys, nil
}

func keyCount(name string) int {
	keys, err := loadKeys(name)
	if err != nil {
		return 0
	}
	return len(keys)
}

func keySlots(name string) []int {
	count := keyCount(name)
	slots := make([]int, 0, count)
	for i := 1; i <= count; i++ {
		slots = append(slots, i)
	}
	return slots
}

// A duplicate add and a full key list are distinct outcomes: the UI must say
// which one happened instead of reporting a save that did not occur.
var (
	errKeyDuplicate = errors.New("key already saved")
	errKeyLimit     = errors.New("key limit reached")
)

func addKey(name, key string) error {
	if key == "" || !validKey(key) {
		return fmt.Errorf("invalid key")
	}
	keys, err := loadKeys(name)
	if err != nil {
		return err
	}
	for _, existing := range keys {
		if existing == key {
			return errKeyDuplicate
		}
	}
	if len(keys) >= maxKeysPerProvider {
		return errKeyLimit
	}
	return writeKeyList(name, append(keys, key))
}

func removeKeyAt(name string, index int) error {
	keys, err := loadKeys(name)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(keys) {
		return fmt.Errorf("invalid key index")
	}
	keys = append(keys[:index], keys[index+1:]...)
	return writeKeyList(name, keys)
}

func writeKeyList(name string, keys []string) error {
	path, err := keyFilePath(name)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		if err := removeStateFile(path); err != nil {
			return fmt.Errorf("key remove failed")
		}
		return nil
	}
	return writeStateFile(path, []byte(strings.Join(keys, "\n")))
}

// --- System One configuration -------------------------------------------
// config.txt holds explicit opt-ins only. It never holds secrets.

const (
	soModeAdvisory  = "advisory"
	soModeEnforcing = "enforcing"
)

func soConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home unavailable")
	}
	return filepath.Join(home, ".walsy-system1", "config.txt"), nil
}

// readSOConfig parses key=value lines. Malformed lines and comments are
// ignored; a missing or rejected file is an empty config (fail-open).
func readSOConfig() map[string]string {
	out := map[string]string{}
	path, err := soConfigPath()
	if err != nil {
		return out
	}
	data, err := readStateFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.SplitN(line, "=", 2)
		if len(f) != 2 {
			continue
		}
		key := strings.TrimSpace(f[0])
		if key == "" || len(key) > 64 {
			continue
		}
		out[key] = strings.TrimSpace(f[1])
	}
	return out
}

func validSOMode(v string) bool {
	return v == soModeAdvisory || v == soModeEnforcing
}

// soModeStartupError refuses startup until an operator selects a mode
// explicitly. soMode() keeps its fail-open string contract for in-process
// callers; only main.go uses this check before any serve loop begins.
func soModeStartupError() error {
	cfg := readSOConfig()
	envMode := strings.ToLower(strings.TrimSpace(os.Getenv("WALSY_SO_MODE")))
	fileMode := strings.ToLower(strings.TrimSpace(cfg["so_mode"]))
	mode := fileMode
	if validSOMode(envMode) {
		mode = envMode
	}
	if !validSOMode(mode) {
		return fmt.Errorf("WALSY_SO_MODE unset or invalid (no so_mode in config.txt): set WALSY_SO_MODE=advisory|enforcing")
	}
	if mode == soModeEnforcing {
		name := strings.TrimSpace(os.Getenv("WALSY_SO_APPROVER"))
		if name == "" {
			name = strings.TrimSpace(cfg["approver"])
		}
		if name == "" {
			return fmt.Errorf("so_mode=enforcing requires a named approver (WALSY_SO_APPROVER or config approver)")
		}
	}
	return nil
}

// soMode resolves the active mode. Env overrides the file; unknown values are
// ignored at both layers. Selecting enforcing requires a named approver from
// WALSY_SO_APPROVER or config.txt; without one the flip is audited as denied.
func soMode() string {
	cfg := readSOConfig()
	var v string
	if e := strings.ToLower(strings.TrimSpace(os.Getenv("WALSY_SO_MODE"))); validSOMode(e) {
		v = e
	} else if f := strings.ToLower(strings.TrimSpace(cfg["so_mode"])); validSOMode(f) {
		v = f
	}
	if v == soModeEnforcing {
		name := strings.TrimSpace(os.Getenv("WALSY_SO_APPROVER"))
		if name == "" {
			name = strings.TrimSpace(cfg["approver"])
		}
		if name == "" {
			routerAudit("so_mode", "mode", "enforcing", "outcome", "denied")
		} else {
			routerAudit("so_mode", "mode", "enforcing", "approver", name)
		}
	}
	if v == "" {
		return soModeAdvisory
	}
	return v
}

// System prompts panel: named, user-editable notes appended to SO state as untrusted data.
const (
	systemPromptFile    = "system_prompt.txt"
	promptsFile         = "prompts.json"
	maxSystemPrompt     = 8192
	maxPromptName       = 64
	maxPrompts          = 32
	defaultSystemPrompt = "Review the seven checks and list the missing checks and fix them."
)

// Prompt is one named system prompt. Text is capped; name is a short label.
type Prompt struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

var promptDelimiter = regexp.MustCompile(`<\|[^>]{0,32}\|>|\[/?INST\]|<<[^>]{0,32}>>`)

func systemPromptPath() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, systemPromptFile), nil
}

func promptsPath() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, promptsFile), nil
}

// capSystemPrompt truncates at a rune boundary at or before the byte cap.
func capSystemPrompt(s string) (string, bool) {
	if len(s) <= maxSystemPrompt {
		return s, false
	}
	cut := s[:maxSystemPrompt]
	for len(cut) > 0 {
		if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size > 1 {
			return cut, true
		}
		cut = cut[:len(cut)-1]
	}
	return "", true
}

// escapePromptDelimiters neutralizes instruction-shaped tokens in untrusted text.
func escapePromptDelimiters(s string) string {
	return promptDelimiter.ReplaceAllString(s, "[DELIMITER]")
}

func loadSystemPrompt() string {
	path, err := systemPromptPath()
	if err != nil {
		return defaultSystemPrompt
	}
	data, err := readStateFile(path)
	if err != nil || len(data) == 0 {
		return defaultSystemPrompt
	}
	s, _ := capSystemPrompt(string(data))
	return s
}

// saveSystemPrompt keeps the legacy single-prompt API working and mirrors the
// text into prompts.json under "default", so named prompts carry it to SO too.
func saveSystemPrompt(s string) error {
	s, _ = capSystemPrompt(s)
	path, err := systemPromptPath()
	if err != nil {
		return err
	}
	if err := writeStateFile(path, []byte(s)); err != nil {
		return err
	}
	ps := loadPrompts()
	for i := range ps {
		if ps[i].Name == "default" {
			ps[i].Text = s
			return savePrompts(ps)
		}
	}
	return addPrompt("default", s)
}

// loadPrompts reads named prompts. With no prompts.json it migrates a legacy
// system_prompt.txt once; a clean home starts with no saved prompts.
func loadPrompts() []Prompt {
	path, err := promptsPath()
	if err != nil {
		return nil
	}
	data, err := readStateFile(path)
	if err != nil {
		return nil
	}
	if len(data) == 0 {
		legacy, lerr := systemPromptPath()
		if lerr != nil {
			return nil
		}
		old, oerr := readStateFile(legacy)
		if oerr != nil || len(old) == 0 {
			return nil
		}
		seed := []Prompt{{Name: "default", Text: capText(string(old))}}
		if err := savePrompts(seed); err != nil {
			return nil
		}
		return seed
	}
	var ps []Prompt
	if json.Unmarshal(data, &ps) != nil {
		return nil
	}
	return ps
}

func capText(s string) string {
	s, _ = capSystemPrompt(s)
	return s
}

func savePrompts(ps []Prompt) error {
	path, err := promptsPath()
	if err != nil {
		return err
	}
	b, err := json.Marshal(ps)
	if err != nil {
		return err
	}
	return writeStateFile(path, b)
}

// addPrompt stores a new named prompt. Name is trimmed and non-empty; text is
// capped at maxSystemPrompt bytes. Names are unique.
func addPrompt(name, text string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxPromptName {
		return fmt.Errorf("invalid prompt name")
	}
	ps := loadPrompts()
	if len(ps) >= maxPrompts {
		return fmt.Errorf("prompt limit reached")
	}
	for _, p := range ps {
		if p.Name == name {
			return fmt.Errorf("prompt name exists")
		}
	}
	ps = append(ps, Prompt{Name: name, Text: capText(text)})
	return savePrompts(ps)
}

func removePrompt(name string) error {
	ps := loadPrompts()
	out := ps[:0]
	found := false
	for _, p := range ps {
		if p.Name == name {
			found = true
			continue
		}
		out = append(out, p)
	}
	if !found {
		return fmt.Errorf("prompt not found")
	}
	return savePrompts(out)
}

// promptSOBlock renders all named prompts as untrusted data for SO. It returns
// "" when none are saved. Each text is scrubbed, delimiter-escaped and capped.
func promptSOBlock() string {
	ps := loadPrompts()
	if len(ps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## USER PROMPTS (untrusted data)\n")
	for _, p := range ps {
		text := escapePromptDelimiters(scrubText(capText(p.Text)))
		b.WriteByte('[')
		b.WriteString(p.Name)
		b.WriteString("]\n")
		b.WriteString(text)
		b.WriteByte('\n')
	}
	return b.String()
}

// capabilityConfig is the set of explicit client opt-ins forwarded to SO.
type capabilityConfig struct {
	NeedLabel  bool
	NoScratch  bool
	Safe       bool
	Reversible bool
	Authority  bool
	Calibrated bool
	Phase      string
}

// soCapability is the explicit opt-in surface. Defaults stay off; a client
// must send every capability flag with a model-before phase to enter SO.
func soCapability() capabilityConfig { return capabilityConfig{} }
