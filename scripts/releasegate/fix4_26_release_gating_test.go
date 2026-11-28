package main

import (
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Child mode: stub SO on loopback, then call the real main. A blocking verdict
// reaches os.Exit(1) in this child, so the parent can assert the exit code.
func TestFix4_26ReleaseGateBlocks(t *testing.T) {
	if os.Getenv("FIX4_26_GATE_HELPER") == "1" {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, os.Getenv("FIX4_26_GATE_REPLY"))
		}))
		defer srv.Close()
		_ = os.Setenv("WALSY_SO_ENDPOINT", srv.URL)
		_ = os.Setenv("EVREN_API_KEY", "fix4-26-stub-key")
		flag.CommandLine = flag.NewFlagSet("releasegate", flag.ContinueOnError)
		os.Args = []string{"releasegate", "-so-final-complete=0.8"}
		main()
		return
	}

	cases := []struct {
		name, approval, reply string
		wantCode              int
		wantErr               string
	}{
		{"no approval", "", `{"answers":{"final":{"type":"choice","choice":"complete","confidence":0.9}}}`, 1, "approval requires both"},
		{"low confidence", "fix4-26-approver", `{"answers":{"final":{"type":"choice","choice":"complete","confidence":0.5}}}`, 1, "not complete"},
		{"approved high confidence", "fix4-26-approver", `{"answers":{"final":{"type":"choice","choice":"complete","confidence":0.9}}}`, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "release.tag"), []byte("0.1.0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte("# 0.1.0\nlocal checks\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			if tc.approval != "" {
				writeConfig(t, home, tc.approval)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestFix4_26ReleaseGateBlocks$")
			cmd.Dir = root
			cmd.Env = append(os.Environ(),
				"FIX4_26_GATE_HELPER=1",
				"FIX4_26_GATE_REPLY="+tc.reply,
				"RELEASE_APPROVAL="+tc.approval,
				"HOME="+home,
			)
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("helper: %v\n%s", err, out)
				}
				code = ee.ExitCode()
			}
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d\n%s", code, tc.wantCode, out)
			}
			if tc.wantErr != "" && !strings.Contains(string(out), tc.wantErr) {
				t.Fatalf("output %q omits %q", out, tc.wantErr)
			}
			if tc.wantCode == 0 && !strings.Contains(string(out), "release gate: PASS") {
				t.Fatalf("output %q omits PASS", out)
			}
			if tc.wantCode != 0 && strings.Contains(string(out), "release gate: PASS") {
				t.Fatalf("blocked gate printed PASS: %q", out)
			}
		})
	}
}
