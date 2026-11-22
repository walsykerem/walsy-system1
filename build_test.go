package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveBuildTarget(t *testing.T) {
	cases := []struct {
		id, goos, goarch string
	}{
		{"darwin/arm64", "darwin", "arm64"},
		{"darwin/amd64", "darwin", "amd64"},
		{"linux/amd64", "linux", "amd64"},
		{"linux/arm64", "linux", "arm64"},
		{"windows/amd64", "windows", "amd64"},
		{"windows/arm64", "windows", "arm64"},
		{"native", runtime.GOOS, runtime.GOARCH},
		{"  NATIVE  ", runtime.GOOS, runtime.GOARCH},
		{"DARWIN/ARM64", "darwin", "arm64"},
	}
	for _, tc := range cases {
		goos, goarch, err := resolveBuildTarget(tc.id)
		if err != nil || goos != tc.goos || goarch != tc.goarch {
			t.Errorf("resolveBuildTarget(%q) = %q/%q, %v; want %q/%q", tc.id, goos, goarch, err, tc.goos, tc.goarch)
		}
	}
	for _, bad := range []string{"", "plan9/amd64", "darwin/386", "linux/386", "native/x", "../evil"} {
		if _, _, err := resolveBuildTarget(bad); err == nil {
			t.Errorf("resolveBuildTarget(%q): want error", bad)
		}
	}
}

func TestBuildTargetPersistence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".walsy-system1", buildTargetFile)
	if got := loadBuildTarget(); got != "native" {
		t.Fatalf("default target = %q, want native", got)
	}
	if err := saveBuildTarget("LINUX/ARM64"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "linux/arm64\n" {
		t.Fatalf("state = %q, want canonical lowercase id", data)
	}
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("state mode = %o, want 600", st.Mode().Perm())
	}
	if got := loadBuildTarget(); got != "linux/arm64" {
		t.Fatalf("reloaded target = %q", got)
	}
	goos, goarch := effectiveBuildTarget()
	if goos != "linux" || goarch != "arm64" {
		t.Fatalf("effective = %s/%s", goos, goarch)
	}
	if err := saveBuildTarget("plan9/amd64"); err == nil {
		t.Fatal("unknown target accepted")
	}
	if got := loadBuildTarget(); got != "linux/arm64" {
		t.Fatalf("rejected target changed state: %q", got)
	}
}

func TestBuildTargetRouteValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	post := func(target string) *httptest.ResponseRecorder {
		form := url.Values{"target": {target}}.Encode()
		req := dashRequest(http.MethodPost, "/ui/build-target", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		NewDashboardHandler().ServeHTTP(rec, req)
		return rec
	}
	bad := post("plan9/amd64")
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "unknown build target") {
		t.Fatalf("invalid target: status = %d body = %q", bad.Code, bad.Body.String())
	}
	good := post("DARWIN/AMD64")
	if good.Code != http.StatusOK {
		t.Fatalf("valid target: status = %d body = %q", good.Code, good.Body.String())
	}
	if got := loadBuildTarget(); got != "darwin/amd64" {
		t.Fatalf("saved target = %q", got)
	}
}

func TestBuildSlotRejectsConcurrentBuild(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	started := make(chan struct{})
	release := make(chan struct{})
	orig, origVet, origTest := runGoBuild, runGoVet, runGoTest
	runGoVet = func(context.Context, string) ([]byte, error) { return nil, nil }
	runGoTest = func(context.Context, string) ([]byte, error) { return nil, nil }
	runGoBuild = func(_ context.Context, dir, rel, _, _ string) ([]byte, error) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte("bin"), 0755); err != nil {
			return nil, err
		}
		close(started)
		<-release
		return nil, nil
	}
	dir := t.TempDir()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleUIBuild(httptest.NewRecorder(), dashRequest(http.MethodPost, "/ui/build", nil), dir)
	}()
	t.Cleanup(func() { runGoBuild, runGoVet, runGoTest = orig, origVet, origTest })
	<-started
	rec := httptest.NewRecorder()
	handleUIBuild(rec, dashRequest(http.MethodPost, "/ui/build", nil), dir)
	if !strings.Contains(rec.Body.String(), "BUILD ALREADY RUNNING") {
		t.Fatalf("second build body = %q", rec.Body.String())
	}
	close(release)
	<-done
}

// A failed test run must never leave the untested binary live, and the banner
// must say what actually happened: restored, or discarded on a first build.
func TestInstallBuildAndDiscardFailed(t *testing.T) {
	dir := t.TempDir()
	rel := filepath.Join("dist", "app")
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Join(dir, "dist"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return ""
		}
		return string(b)
	}

	// First build: nothing to restore, so the failing binary must be removed.
	write("dist/app.tmp", "new")
	if err := installBuild(dir, "dist/app.tmp", rel); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if got := discardFailedBuild(dir, rel); !strings.Contains(got, "NO PREVIOUS BINARY") {
		t.Fatalf("first build failure banner = %q", got)
	}
	if got := read("dist/app"); got != "" {
		t.Fatalf("failing first build left %q live", got)
	}

	// Later build: the previous binary comes back and the banner says so.
	write("dist/app", "old")
	write("dist/app.tmp", "new")
	if err := installBuild(dir, "dist/app.tmp", rel); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if got := read("dist/app"); got != "new" {
		t.Fatalf("installed binary = %q", got)
	}
	if got := discardFailedBuild(dir, rel); got != "BUILD REVERTED: TESTS FAILED" {
		t.Fatalf("revert banner = %q", got)
	}
	if got := read("dist/app"); got != "old" {
		t.Fatalf("reverted binary = %q", got)
	}

	// A build that produced no binary is an error, not a silent success.
	if err := installBuild(dir, "dist/missing", rel); err == nil {
		t.Fatal("installBuild accepted a missing build output")
	}
}
