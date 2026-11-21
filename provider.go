package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// provider is one registry slot. KeyEnv holds an env var NAME, never the key.
type provider struct {
	Name   string
	Base   string
	KeyEnv string
}

// bakedProviders: compile-time 9-slot registry (plan.md §1e, line 17).
// Base is empty where plan.md names no endpoint — never invent a URL.
var bakedProviders = [...]provider{
	{Name: "OpenRouter"},
	{Name: "Ollama Cloud"},
	{Name: "OpenCode Free"},
	{Name: "v1m System One"},
	{Name: "Evren System One", Base: "https://evren-llmapi.ssyz.org.tr", KeyEnv: "EVREN_API_KEY"},
	{Name: "Nace Drex"},
	{Name: "Ollama Local"},
	{Name: "Cloudflare"},
	{Name: "OpenCode Zen"},
}

func bakedRegistry() []provider {
	return append([]provider(nil), bakedProviders[:]...)
}

// loadRegistry reads the fixed #10 slot path; missing file is not an error.
func loadRegistry() ([]provider, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return bakedRegistry(), nil // no home ⇒ no file ⇒ config order
	}
	return loadRegistryFrom(filepath.Join(home, ".walsy-system1", "providers.txt"))
}

// loadRegistryFrom is the parameterizable trust-boundary check used by tests.
func loadRegistryFrom(path string) ([]provider, error) {
	st, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return bakedRegistry(), nil
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
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("registry read failed")
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		return bakedRegistry(), nil
	}
	if strings.ContainsAny(line, "\r\n") {
		return nil, fmt.Errorf("registry must be one line")
	}
	fields := strings.SplitN(line, ",", 3)
	if len(fields) != 3 {
		return nil, fmt.Errorf("registry line needs 3 fields")
	}
	name := strings.TrimSpace(fields[0])
	base := strings.TrimSpace(fields[1])
	keyEnv := strings.TrimSpace(fields[2])
	if name == "" || base == "" || keyEnv == "" {
		return nil, fmt.Errorf("registry line has empty field")
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("registry endpoint rejected")
	}
	return append(bakedRegistry(), provider{Name: name, Base: base, KeyEnv: keyEnv}), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// pin is the sticky winner. RAM only: restart ⇒ empty ⇒ config order.
type pin struct {
	mu   sync.Mutex
	name string
	list []provider
}

func newPin(list []provider) *pin {
	return &pin{list: append([]provider(nil), list...)}
}

func (p *pin) current() provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.name != "" {
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
	p.name = name
	p.mu.Unlock()
}

// rotateOnFailure drops the pin and moves to deterministic hash order's head.
func (p *pin) rotateOnFailure() provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.name = ""
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
