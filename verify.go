package main

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Fixed failure buckets. Every build/test failure lands in exactly one, so a
// caller can branch without reading prose and the same failure classifies the
// same way on every run.
const (
	failCompile = "compile"
	failType    = "type"
	failTest    = "test"
	failTimeout = "timeout"
	failEnv     = "env"
	failFlake   = "flake"
)

// failureOrder is strongest evidence first: a download timeout inside a test
// run is a timeout, not a test failure.
var failureOrder = []struct {
	bucket string
	re     *regexp.Regexp
}{
	{failTimeout, regexp.MustCompile(`(?i)\b(timed out|timeout|deadline exceeded|context deadline|test timed out after)\b`)},
	{failEnv, regexp.MustCompile(`(?i)(no such host|connection refused|network is unreachable|proxyconnect|permission denied|no space left|disk full|too many open files|go: downloading|module lookup disabled|goproxy)`)},
	{failFlake, regexp.MustCompile(`(?i)\b(flaky|flake|intermittent|retry passed|retried and passed)\b`)},
	{failTest, regexp.MustCompile(`(?m)(^--- FAIL|^FAIL\s|^panic: )`)},
	{failType, regexp.MustCompile(`(?i)(cannot use .* as |does not implement|mismatched types|is not assignable|incompatible types|typeerror|undefined type)`)},
	{failCompile, regexp.MustCompile(`(?i)(syntax error|undefined:|declared and not used|cannot find package|no required module|build failed|expected .*, found )`)},
}

// classifyFailure maps failure text to one fixed bucket. Unrecognized build
// text is compile: the run failed before anything could execute.
func classifyFailure(text string) string {
	for _, r := range failureOrder {
		if r.re.MatchString(text) {
			return r.bucket
		}
	}
	return failCompile
}

// failureSignature is the stable key for one failure: bucket plus a hash of
// the normalized error lines. Line numbers, durations, temp paths, addresses,
// goroutine ids and timestamps are noise and do not change the key, so retries
// of the same failure share one attemptGuard counter.
func failureSignature(text string) string {
	lines := strings.Split(normalizeFailure(text), "\n")
	kept := make([]string, 0, 10)
	for _, l := range lines {
		if l == "" || failureNoise(l) {
			continue
		}
		kept = append(kept, l)
		if len(kept) == 10 {
			break
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(kept, "\n")))
	return classifyFailure(text) + ":" + hex.EncodeToString(sum[:8])
}

var (
	sigTempPath  = regexp.MustCompile(`(?:/private)?(?:/var/folders|/tmp)/[^\s:)]+`)
	sigAddr      = regexp.MustCompile(`0x[0-9a-f]+`)
	sigGoroutine = regexp.MustCompile(`goroutine \d+`)
	sigGoPos     = regexp.MustCompile(`\.go:\d+(?::\d+)?`)
	sigDuration  = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|\x{00b5}s|us|ms|s|m|h)\b`)
	sigClock     = regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}(?:\.\d+)?\b`)
	sigDate      = regexp.MustCompile(`\b\d{4}/\d{2}/\d{2}\b`)
	sigPID       = regexp.MustCompile(`\bpid[:= ]?\d+\b`)
	sigSpace     = regexp.MustCompile(`\s+`)
)

// normalizeFailure lowercases and strips per-run noise from failure text.
func normalizeFailure(text string) string {
	s := strings.ToLower(text)
	s = sigTempPath.ReplaceAllString(s, "<tmp>")
	s = sigAddr.ReplaceAllString(s, "<addr>")
	s = sigGoroutine.ReplaceAllString(s, "goroutine <n>")
	s = sigGoPos.ReplaceAllString(s, ".go")
	s = sigClock.ReplaceAllString(s, "<time>")
	s = sigDate.ReplaceAllString(s, "<date>")
	s = sigPID.ReplaceAllString(s, "pid <n>")
	s = sigDuration.ReplaceAllString(s, "<dur>")
	var b strings.Builder
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(sigSpace.ReplaceAllString(l, " "))
		if l != "" {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// failureNoise drops framework chatter that says nothing about the failure.
func failureNoise(l string) bool {
	for _, p := range []string{"=== run", "=== pause", "=== cont", "--- pass", "coverage:", "ok  "} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return l == "pass" || l == "fail"
}
