package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An empty or whitespace-only release.tag makes strings.Contains(changelog, "")
// always true, so the gate could print PASS for no version at all.
func TestRunRejectsEmptyReleaseTag(t *testing.T) {
	for _, tag := range []string{"", "\n", "  \n\t\n"} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "release.tag"), []byte(tag), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte("# anything\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(root)
		t.Setenv("RELEASE_APPROVAL", "approver")
		home := t.TempDir()
		dir := filepath.Join(home, ".walsy-system1")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.txt"), []byte("m1_approval_owner=approver\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		t.Setenv("EVREN_API_KEY", "key")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"answers":{"final":{"choice":"complete","confidence":0.9}}}`)
		}))
		t.Setenv("WALSY_SO_ENDPOINT", srv.URL)

		err := run(0.8)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "release.tag is empty") {
			t.Fatalf("tag %q: run error = %v, want empty-tag refusal", tag, err)
		}
	}
}
