package app

import (
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	routerDir             = ".walsy-system1"
	routerAuditName       = "audit.jsonl"
	routerAuditMaxBytes   = 8 << 20
	routerDefaultBudget   = 3
	routerDefaultFailures = 3
	routerDefaultCooldown = 30 * time.Second
	routerMaxConfigBytes  = 64 << 10
	routerMaxFieldLen     = 64
)

// routerBreaker tracks recent failures for one tool or provider.
const (
	routerDefaultLatencyRing = 16
	routerDefaultLatencyMS   = 100
)

type routerBreaker struct {
	failures     int
	last         time.Time
	halfOpen     bool
	latencies    [routerDefaultLatencyRing]time.Duration
	latencyIdx   int
	latencyCount int
	slowStreak   int
}

type routerPolicy struct {
	init             bool
	budgetMax        int
	budgetLeft       int
	budgetRefills    int
	latencyRing      int
	latencyThreshold time.Duration
	deny             map[string]bool
	mirror           string
	threshold        int
	cooldown         time.Duration
	breakers         map[string]routerBreaker
}

var (
	routerMu    sync.Mutex
	routerState routerPolicy
)

// routerAuditKeys is the only key set accepted by routerAudit. Unknown keys
// and their values are dropped before anything reaches disk.
var routerAuditKeys = map[string]bool{
	"tool": true, "status": true, "bucket": true, "decision": true,
	"phase": true, "mode": true, "provider": true, "outcome": true,
	"reason": true, "budget": true, "count": true, "latency_ms": true,
	"method": true, "allow": true, "credits": true,
	"source": true, "id": true, "nonce": true,
	"prev": true, "head": true, "response_id": true,
	"approver": true, "hash": true, "label": true,
}

// routerReset clears per-process counters. Tests call it between cases.
func routerReset() {
	routerMu.Lock()
	defer routerMu.Unlock()
	routerState = routerPolicy{}
}

func routerInitLocked() {
	if routerState.init {
		return
	}
	routerState.init = true
	routerState.deny = make(map[string]bool)
	routerState.breakers = make(map[string]routerBreaker)
	routerState.budgetMax = routerIntConfig("retry_budget", routerDefaultBudget, 0, 64)
	routerState.budgetLeft = routerState.budgetMax
	routerState.threshold = routerIntConfig("breaker_threshold", routerDefaultFailures, 1, 32)
	routerState.cooldown = routerDurationConfig("breaker_cooldown", routerDefaultCooldown)
	routerState.latencyRing = routerIntConfig("latency_ring_size", routerDefaultLatencyRing, 2, routerDefaultLatencyRing)
	routerState.latencyThreshold = time.Duration(routerIntConfig("latency_threshold_ms", routerDefaultLatencyMS, 1, 10000)) * time.Millisecond
	routerState.mirror = routerMirrorConfig()
	for _, n := range strings.Split(routerConfigValue("deny"), ",") {
		if name, ok := routerSafeField(strings.TrimSpace(n)); ok {
			routerState.deny[name] = true
		}
	}
}

// routerAllow denies only tools on an explicit env/config deny list. Payload,
// tool arguments and model output never influence this decision.
func routerAllow(tool string) bool {
	routerMu.Lock()
	defer routerMu.Unlock()
	routerInitLocked()
	name, ok := routerSafeField(tool)
	if !ok {
		return true
	}
	return !routerState.deny[name]
}

// routerBudgetSpend spends one tool-call token of the current session budget.
// spent=false means the budget is used up; max is the session budget so the
// refusal can name it. A missing or malformed retry_budget falls back to the
// default (3), so bad config never blocks; an explicit retry_budget=0 does.
func routerBudgetSpend() (left, max int, spent bool) {
	routerMu.Lock()
	defer routerMu.Unlock()
	routerInitLocked()
	if routerState.budgetLeft <= 0 {
		return 0, routerState.budgetMax, false
	}
	routerState.budgetLeft--
	return routerState.budgetLeft, routerState.budgetMax, true
}

// routerMaxRefills bounds how many new sessions may refill the retry budget, so
// looping initialize cannot make the per-session throttle unbounded.
const routerMaxRefills = 8

// routerBudgetReset refills the budget at its documented boundary: a new MCP
// session (initialize) or process start. Refills are bounded by routerMaxRefills;
// past that the budget stays spent.
func routerBudgetReset() {
	routerMu.Lock()
	defer routerMu.Unlock()
	routerInitLocked()
	if routerState.budgetRefills >= routerMaxRefills {
		return
	}
	routerState.budgetRefills++
	routerState.budgetLeft = routerState.budgetMax
}

// routerBudgetOK spends one retry token. Default budget is 3; advisory mode
// never calls this automatically, so no retry loop exists in the default path.
func routerBudgetOK() bool {
	_, _, spent := routerBudgetSpend()
	return spent
}

// routerBudgetSpendLatency spends one extra retry token when an observed SO
// call exceeds the latency threshold. It is additive to routerBudgetSpend.
func routerBudgetSpendLatency(latency time.Duration) (left, max int, spent bool) {
	routerMu.Lock()
	defer routerMu.Unlock()
	routerInitLocked()
	if latency <= 0 || latency <= routerState.latencyThreshold || routerState.budgetLeft <= 0 {
		return routerState.budgetLeft, routerState.budgetMax, false
	}
	routerState.budgetLeft--
	return routerState.budgetLeft, routerState.budgetMax, true
}

// latencyMedian is the median of the samples currently in the rolling ring.
func (b *routerBreaker) latencyMedian() time.Duration {
	if b.latencyCount <= 0 {
		return 0
	}
	samples := make([]time.Duration, 0, b.latencyCount)
	for _, d := range b.latencies {
		if d > 0 {
			samples = append(samples, d)
		}
	}
	if len(samples) == 0 {
		return 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[len(samples)/2]
}

// routerRecord feeds the circuit breaker. Success closes the breaker. An
// optional observed latency above the threshold opens it after a short run of
// slow samples, without spending retry budget.
func routerRecord(tool string, ok bool, observed ...time.Duration) {
	routerMu.Lock()
	defer routerMu.Unlock()
	routerInitLocked()
	name, safe := routerSafeField(tool)
	if !safe {
		return
	}
	b := routerState.breakers[name]
	now := time.Now()
	if len(observed) > 0 && observed[0] > 0 {
		b.latencies[b.latencyIdx] = observed[0]
		b.latencyIdx = (b.latencyIdx + 1) % routerState.latencyRing
		if b.latencyCount < routerState.latencyRing {
			b.latencyCount++
		}
		if b.latencyMedian() > routerState.latencyThreshold {
			if b.slowStreak == 0 {
				b.last = now // latency circuit opens here; its cooldown starts now
			}
			b.slowStreak++
		} else {
			b.slowStreak = 0
		}
		// A half-open probe must reach the ok/fail branches below so its result
		// closes or reopens the circuit; a slow probe is still evidence.
		if !b.halfOpen && b.slowStreak >= routerState.threshold {
			b.failures = routerState.threshold
			b.last, b.halfOpen = now, false
			routerState.breakers[name] = b
			return
		}
	}
	if ok {
		// Success closes the breaker, including a successful half-open probe of
		// a latency-opened circuit whose stale slow samples still dominate.
		if b.halfOpen || b.failures == 0 && b.slowStreak == 0 {
			delete(routerState.breakers, name)
		} else {
			// Keep b.last: it is also the latency circuit's open time.
			b.failures, b.halfOpen = 0, false
			routerState.breakers[name] = b
		}
		return
	}
	if b.halfOpen || b.failures == 0 || now.Sub(b.last) >= routerState.cooldown {
		b.failures = 0
	}
	b.failures++
	b.last = now
	b.halfOpen = false
	routerState.breakers[name] = b
}

// routerBreakerOpen reports whether a tool must be skipped. After cooldown a
// single half-open probe is allowed and leased for one cooldown; while it is
// outstanding further calls wait, and a failed probe resets the failure window.
func routerBreakerOpen(tool string) bool {
	routerMu.Lock()
	defer routerMu.Unlock()
	routerInitLocked()
	name, ok := routerSafeField(tool)
	if !ok {
		return false
	}
	b := routerState.breakers[name]
	if b.failures < routerState.threshold {
		// Two slow median samples open the circuit: waiting for a full ring
		// admits a burst of slow calls before the breaker reacts.
		if b.latencyCount < 2 || b.latencyMedian() <= routerState.latencyThreshold {
			return false
		}
		// A latency-opened circuit obeys the same cooldown rule below.
	}
	if time.Since(b.last) < routerState.cooldown {
		return true
	}
	// Grant one probe and lease it for one cooldown: a status read also calls
	// this, and a probe consumed without a recorded result must expire.
	b.halfOpen = true
	b.last = time.Now()
	routerState.breakers[name] = b
	return false
}

// routerMirrorURL returns the optional canary target, or empty when disabled.
func routerMirrorURL() string {
	routerMu.Lock()
	defer routerMu.Unlock()
	routerInitLocked()
	return routerState.mirror
}

// routerAudit appends a redacted JSONL record to ~/.walsy-system1/audit.jsonl.
// Only allowlisted keys and sanitized name/bucket values reach disk.
func routerAudit(event string, kv ...string) {
	fields := map[string]string{"ts": strconv.FormatInt(time.Now().UnixMilli(), 10)}
	if name, ok := routerSafeField(event); ok {
		fields["event"] = name
	} else {
		fields["event"] = "event"
	}
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := routerSafeField(kv[i])
		if !ok || !routerAuditKeys[key] {
			continue
		}
		value, ok := routerSafeField(kv[i+1])
		if !ok {
			value = "redacted"
		}
		fields[key] = value
	}
	if len(kv)%2 != 0 {
		fields["bucket"] = "odd_kv"
	}
	line, err := json.Marshal(fields)
	if err != nil {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, routerDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	path := filepath.Join(dir, routerAuditName)
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return
		}
		if st.Size() > routerAuditMaxBytes {
			_ = os.Rename(path, path+".1")
		}
	} else if !os.IsNotExist(err) {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_ = os.Chmod(path, 0600)
	_, _ = f.Write(append(line, '\n'))
}

// routerSafeField accepts short names and bucket labels only, never free text.
func routerSafeField(s string) (string, bool) {
	if s == "" || len(s) > routerMaxFieldLen {
		return "", false
	}
	for _, c := range s {
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		digit := c >= '0' && c <= '9'
		if !letter && !digit && c != '-' && c != '_' && c != '.' && c != '/' {
			return "", false
		}
	}
	return s, true
}

// routerConfigValue reads explicit env first, then the opt-in config file.
// The raw config is never logged, audited or returned wholesale.
func routerConfigValue(key string) string {
	if value, ok := os.LookupEnv("WALSY_ROUTER_" + strings.ToUpper(key)); ok {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	f, err := os.Open(filepath.Join(home, routerDir, "config.txt"))
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, routerMaxConfigBytes+1))
	if err != nil || len(data) > routerMaxConfigBytes {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		i := strings.IndexByte(line, '=')
		if i <= 0 || strings.TrimSpace(line[:i]) != "router_"+key {
			continue
		}
		return strings.TrimSpace(line[i+1:])
	}
	return ""
}

func routerIntConfig(key string, def, min, max int) int {
	value, err := strconv.Atoi(strings.TrimSpace(routerConfigValue(key)))
	if err != nil {
		return def
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func routerDurationConfig(key string, def time.Duration) time.Duration {
	value, err := time.ParseDuration(strings.TrimSpace(routerConfigValue(key)))
	if err != nil || value <= 0 {
		return def
	}
	if value > time.Hour {
		return time.Hour
	}
	return value
}

func routerMirrorConfig() string {
	value := strings.TrimSpace(routerConfigValue("mirror_url"))
	if value == "" {
		return ""
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return value
}
