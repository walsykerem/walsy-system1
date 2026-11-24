package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// provider is one registry slot. KeyEnv holds an env var NAME, never the key.
type provider struct {
	Name   string
	Base   string
	KeyEnv string
	Cost   int // optional per-call cost from the 4th registry column; lower is tried first
}

// bakedProviders: compile-time default registry (plan.md S1e, line 17).
// Endpoint defaults come from each provider's public API docs; user m04250
// requires every slot to show a real, editable default.
var bakedProviders = [...]provider{
	{Name: "OpenRouter", Base: "https://openrouter.ai/api/v1/systemone"},
	{Name: "Ollama Cloud", Base: "https://ollama.com/v1/systemone"},
	{Name: "v1m System One", Base: "https://v1m.ir/v1/systemone"},
	{Name: "Evren System One", Base: "https://evren-llmapi.ssyz.org.tr/v1/systemone", KeyEnv: "EVREN_API_KEY"},
	{Name: "Command Code", Base: "https://api.commandcode.ai/provider/v1/systemone", KeyEnv: "COMMANDCODE_API_KEY"},
	{Name: "Nace Drex", Base: "https://console.nace.ai/v1/systemone"},
	{Name: "Ollama Local", Base: "http://localhost:11434/v1/systemone"},
	{Name: "Cloudflare Clef", Base: "https://api.cloudflare.com/client/v4/accounts/YOUR_ACCOUNT_ID/ai/run/@cf/cloudflare/clef"},
	{Name: "OpenCode Zen", Base: "https://opencode.ai/zen/v1/systemone"},
}

func bakedRegistry() []provider {
	return append([]provider(nil), bakedProviders[:]...)
}

func registryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home unavailable")
	}
	return filepath.Join(home, ".walsy-system1", "providers.txt"), nil
}

// loadRegistry reads the fixed #10 slot path; missing file is not an error.
func loadRegistry() ([]provider, error) {
	list, _, err := loadRegistryWithOverrides()
	return list, err
}

// loadRegistryWithOverrides also reports which names own a registry line, so
// the UI can offer a reset back to the baked default.
func loadRegistryWithOverrides() ([]provider, map[string]bool, error) {
	path, err := registryPath()
	if err != nil {
		return bakedRegistry(), nil, nil
	}
	return loadRegistryFromWithOverrides(path)
}

// loadRegistryFrom is the parameterizable trust-boundary check used by tests.
func loadRegistryFrom(path string) ([]provider, error) {
	list, _, err := loadRegistryFromWithOverrides(path)
	return list, err
}

func loadRegistryFromWithOverrides(path string) ([]provider, map[string]bool, error) {
	data, err := readRegistryData(path)
	if err != nil {
		return nil, nil, err
	}
	lines, err := parseRegistryLines(data)
	if err != nil {
		return nil, nil, err
	}
	enforcing := soMode() == soModeEnforcing
	// shortcut: a costed loopback line defines the local probe pool and drops
	// baked loopback slots; uncosted registries keep every baked slot. Upgrade:
	// expose slot priority in the UI instead of inferring it from the cost column.
	explicitLocal := false
	for _, p := range lines {
		if enforcing && soProviderOverrideDeniedLine(p) {
			continue
		}
		if p.Cost > 0 && isLoopbackBase(p.Base) {
			explicitLocal = true
			break
		}
	}
	out := bakedRegistry()
	if explicitLocal {
		kept := out[:0]
		for _, p := range out {
			if !isLoopbackBase(p.Base) {
				kept = append(kept, p)
			}
		}
		out = kept
	}
	overrides := make(map[string]bool)
	for _, p := range lines {
		if enforcing && soProviderOverrideDeniedLine(p) {
			routerAudit("provider", "reason", soProviderOverrideDenied)
			continue
		}
		overrides[p.Name] = true
		replaced := false
		for i := range out {
			if out[i].Name == p.Name {
				out[i] = p // a later line for the same name wins
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, p)
		}
	}
	return out, overrides, nil
}

// readRegistryData applies the 0600 regular non-symlink trust boundary.
func readRegistryData(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("registry stat failed")
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("registry symlink rejected")
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("registry not regular")
	}
	if st.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("registry mode rejected")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("registry read failed")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStateFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("registry read failed")
	}
	if len(data) > maxStateFileBytes {
		return nil, fmt.Errorf("registry too large")
	}
	return data, nil
}

// parseRegistryLines validates newline-separated name,base,keyEnv lines.
func parseRegistryLines(data []byte) ([]provider, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil, nil
	}
	out := make([]provider, 0)
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Contains(line, "\r") {
			return nil, fmt.Errorf("registry line has CR")
		}
		fields := strings.Split(line, ",")
		if len(fields) != 3 && len(fields) != 4 {
			return nil, fmt.Errorf("registry line needs 3 or 4 fields")
		}
		name := strings.TrimSpace(fields[0])
		base := strings.TrimSpace(fields[1])
		keyEnv := strings.TrimSpace(fields[2])
		if name == "" || base == "" || keyEnv == "" {
			return nil, fmt.Errorf("registry line has empty field")
		}
		if isDemoName(name) {
			return nil, fmt.Errorf("registry name rejected")
		}
		if !validProviderBase(base) {
			return nil, fmt.Errorf("registry endpoint rejected")
		}
		cost := 0
		if len(fields) == 4 {
			var err error
			cost, err = strconv.Atoi(strings.TrimSpace(fields[3]))
			if err != nil || cost < 0 {
				return nil, fmt.Errorf("registry cost rejected")
			}
		}
		out = append(out, provider{Name: name, Base: base, KeyEnv: keyEnv, Cost: cost})
	}
	return out, nil
}

// writeRegistryLines promotes a validated 0600 temp file over the registry.
func writeRegistryLines(lines []provider) error {
	path, err := registryPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("registry dir failed")
	}
	var b strings.Builder
	for _, p := range lines {
		if p.Name == "" || p.Base == "" || p.KeyEnv == "" || p.Cost < 0 || isDemoName(p.Name) ||
			len(p.Name) > maxProviderNameLen ||
			strings.ContainsRune(p.Name+p.Base+p.KeyEnv, ',') || hasControl(p.Name+p.Base+p.KeyEnv) ||
			!validProviderBase(p.Base) {
			return fmt.Errorf("invalid field")
		}
		b.WriteString(p.Name)
		b.WriteByte(',')
		b.WriteString(p.Base)
		b.WriteByte(',')
		b.WriteString(p.KeyEnv)
		if p.Cost > 0 {
			b.WriteByte(',')
			b.WriteString(strconv.Itoa(p.Cost))
		}
		b.WriteByte('\n')
	}
	f, err := os.CreateTemp(dir, ".providers-*")
	if err != nil {
		return fmt.Errorf("temp file failed")
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return fmt.Errorf("temp mode failed")
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return fmt.Errorf("temp write failed")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("temp sync failed")
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("temp close failed")
	}
	if _, err := loadRegistryFrom(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("registry replace failed")
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("registry dir sync failed")
	}
	return nil
}

// removeRegistryEntry drops one registry line; baked defaults then return.
func removeRegistryEntry(name string) error {
	if name == "" || isDemoName(name) {
		return fmt.Errorf("invalid field")
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
	out := make([]provider, 0, len(lines))
	changed := false
	for _, p := range lines {
		if p.Name == name {
			changed = true
			continue
		}
		out = append(out, p)
	}
	if !changed {
		return nil
	}
	return writeRegistryLines(out)
}

const soProviderOverrideDenied = "so-provider-override-denied"

// soProviderOverrideDeniedLine reports a registry line that would replace a
// baked provider or its SO credential in enforcing mode.
func soProviderOverrideDeniedLine(p provider) bool {
	for _, b := range bakedProviders {
		if strings.EqualFold(b.Name, p.Name) {
			return true
		}
	}
	return strings.EqualFold(p.Base, bakedBase("Evren System One")) || strings.EqualFold(p.KeyEnv, "EVREN_API_KEY")
}

func bakedBase(name string) string {
	if name == "Evren" {
		return bakedBase("Evren System One")
	}
	for _, p := range bakedProviders {
		if p.Name == name {
			return p.Base
		}
	}
	return ""
}

// validProviderBase permits remote HTTPS and loopback HTTP/HTTPS. Client-side
// listeners stay loopback-only; this is only an outbound provider endpoint.
func validProviderBase(base string) bool {
	return baseRejectReason(base) == ""
}

// baseRejectReason names why a base would leak a key or cannot be called, so
// the UI can say it instead of silently snapping the field back.
func baseRejectReason(base string) string {
	u, err := url.Parse(base)
	switch {
	case err != nil || u.Hostname() == "":
		return "NEEDS A FULL URL WITH https:// AND A HOST"
	case u.User != nil:
		return "CREDENTIALS BELONG IN THE KEY FIELD, NOT THE URL"
	case strings.EqualFold(u.Scheme, "https"):
		return ""
	case strings.EqualFold(u.Scheme, "http") && isLoopbackHost(u.Hostname()):
		return ""
	case strings.EqualFold(u.Scheme, "http"):
		return "PLAIN HTTP ONLY FOR 127.0.0.1 OR localhost, USE https:// HERE"
	}
	return "SCHEME MUST BE http OR https"
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLoopbackBase(base string) bool {
	u, err := url.Parse(base)
	return err == nil && isLoopbackHost(u.Hostname())
}

// pinTTL bounds how long a sticky provider stays trusted without a fresh
// success. RAM only: restart -> empty -> config order.
const pinTTL = 15 * time.Minute

// pin is the sticky winner. RAM only: restart -> empty -> config order.
type pin struct {
	mu      sync.Mutex
	name    string
	list    []provider
	expires time.Time
	gen     atomic.Uint64 // engine identity; bumped on pin change, never time-based
}

func newPin(list []provider) *pin {
	return &pin{list: append([]provider(nil), list...)}
}

func (p *pin) current() provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Clock expiry drops the sticky choice but never rotates engine identity.
	if p.name != "" && !time.Now().Before(p.expires) {
		p.name = ""
		p.expires = time.Time{}
	}
	if p.name != "" && time.Now().Before(p.expires) {
		for _, pr := range p.list {
			if pr.Name == p.name {
				return pr
			}
		}
	}
	if len(p.list) == 0 {
		return provider{}
	}
	return p.list[0]
}

func (p *pin) markSuccess(name string) {
	p.mu.Lock()
	want := p.name
	if want == "" || !time.Now().Before(p.expires) {
		if len(p.list) > 0 {
			want = p.list[0].Name
		}
	}
	if want != "" && want != name {
		p.gen.Add(1)
	}
	p.name = name
	p.expires = time.Now().Add(pinTTL)
	p.mu.Unlock()
}

// generation reports the current engine identity. Never a TTL or wall clock.
func (p *pin) generation() uint64 { return p.gen.Load() }

// rotateOnFailure drops the pin and moves to deterministic hash order's head.
func (p *pin) rotateOnFailure() provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gen.Add(1)
	p.name = ""
	p.expires = time.Time{}
	ordered := rotationOrder(p.list)
	if len(ordered) == 0 {
		return provider{}
	}
	return ordered[0]
}

// rotationOrder: sort by sha256(Name), Name as tie-break. No score, no clock.
func rotationOrder(list []provider) []provider {
	ordered := append([]provider(nil), list...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Cost != ordered[j].Cost {
			return ordered[i].Cost < ordered[j].Cost
		}
		a := sha256.Sum256([]byte(ordered[i].Name))
		b := sha256.Sum256([]byte(ordered[j].Name))
		ha, hb := hex.EncodeToString(a[:]), hex.EncodeToString(b[:])
		if ha == hb {
			return ordered[i].Name < ordered[j].Name
		}
		return ha < hb
	})
	return ordered
}
