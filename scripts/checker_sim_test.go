package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
// consistent tally. Outcomes depend on whole-repo health, so the test asserts
// the report shape, not that every check passes.
func TestCheckerSimScript(t *testing.T) {
	out, err := runSim(t, true)
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
	if fails > 0 && err == nil {
		t.Fatalf("FAIL printed but exit 0:\n%s", out)
	}
	if fails == 0 && err != nil {
		t.Fatalf("no FAIL printed but exit non-zero: %v\n%s", err, out)
	}
	want := "checker-sim: " + itoa(checks-fails) + " PASS " + itoa(fails) + " FAIL"
	if verdict != want {
		t.Fatalf("verdict = %q, want %q:\n%s", verdict, want, out)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// With live checks on, the script still ends in a self-consistent verdict and
// exits 0 iff nothing failed. A stopped dashboard is a SKIP, not a FAIL.
func TestCheckerSimScriptVerdict(t *testing.T) {
	out, err := runSim(t, false)
	if !strings.Contains(out, "checker-sim:") {
		t.Fatalf("no verdict line:\n%s", out)
	}
	failed := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "FAIL ") {
			failed = true
		}
	}
	if failed && err == nil {
		t.Fatalf("FAIL printed but exit 0:\n%s", out)
	}
	if !failed && err != nil {
		t.Fatalf("no FAIL printed but exit non-zero: %v\n%s", err, out)
	}
}
