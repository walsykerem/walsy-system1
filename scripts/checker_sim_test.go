package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// runSim runs checker_sim.sh from the repo root. GOCACHE is forwarded so the
// child reuses the caller's build cache; all other scratch stays in the
// script's own TMPDIR.
func runSim(t *testing.T, skipLive bool) (string, error) {
	t.Helper()
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(repo, "scripts", "checker_sim.sh"))
	cmd.Dir = repo
	env := append(os.Environ(), "GOCACHE="+os.Getenv("GOCACHE"))
	if skipLive {
		env = append(env, "CHECKER_SIM_SKIP_LIVE=1")
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The script must emit one PASS/FAIL line per active check and end with a
// consistent tally. CHECKER_SIM_SKIP_LIVE=1 leaves only deterministic offline
// checks, so the whole repo must be green: a FAIL there is a real regression.
func TestCheckerSimScript(t *testing.T) {
	out, err := runSim(t, true)
	if err != nil {
		t.Fatalf("skip-live checker exited non-zero: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	checks, fails := 0, 0
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "PASS "):
			checks++
		case strings.HasPrefix(line, "FAIL "):
			checks++
			fails++
		case strings.HasPrefix(line, "SKIP "):
		}
	}
	if checks == 0 {
		t.Fatalf("no checks reported:\n%s", out)
	}
	if !strings.Contains(out, "SKIP dashboard-live") {
		t.Fatalf("skip-live should skip dashboard checks:\n%s", out)
	}
	verdict := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "checker-sim:") {
			verdict = line
		}
	}
	if verdict == "" {
		t.Fatalf("no verdict line:\n%s", out)
	}
	if fails != 0 {
		t.Fatalf("skip-live checker FAIL count = %d, want 0:\n%s", fails, out)
	}
	want := "checker-sim: " + strconv.Itoa(checks-fails) + " PASS " + strconv.Itoa(fails) + " FAIL"
	if verdict != want {
		t.Fatalf("verdict = %q, want %q:\n%s", verdict, want, out)
	}
}

// With live checks on, the script still ends in a self-consistent verdict and
// exits 0 iff nothing failed. A stopped dashboard is a SKIP, not a FAIL, but if
// every check skipped there is no evidence for a PASS verdict.
func TestCheckerSimScriptVerdict(t *testing.T) {
	out, err := runSim(t, false)
	if !strings.Contains(out, "checker-sim:") {
		t.Fatalf("no verdict line:\n%s", out)
	}
	failed, live := false, 0
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "PASS "), strings.HasPrefix(line, "FAIL "):
			live++
		}
		if strings.HasPrefix(line, "FAIL ") {
			failed = true
		}
	}
	if live == 0 {
		t.Skipf("dashboard unavailable; checker verdict inconclusive:\n%s", out)
	}
	if failed && err == nil {
		t.Fatalf("FAIL printed but exit 0:\n%s", out)
	}
	if !failed && err != nil {
		t.Fatalf("no FAIL printed but exit non-zero: %v\n%s", err, out)
	}
}
