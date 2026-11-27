package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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

// A version change must invalidate the installed binary before it is served;
// the same version must reuse the artifact without invoking the toolchain.
func TestBuildVersionInvalidatesInstalledBinary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	rel := filepath.Join("dist", "walsy-system1-"+runtime.GOOS+"-"+runtime.GOARCH)
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0755); err != nil {
		t.Fatal(err)
	}
	setVersion := func(v string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "release.tag"), []byte(v+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	builds := 0
	origBuild, origVet, origTest := runGoBuild, runGoVet, runGoTest
	t.Cleanup(func() { runGoBuild, runGoVet, runGoTest = origBuild, origVet, origTest })
	runGoVet = func(context.Context, string) ([]byte, error) { return nil, nil }
	runGoTest = func(context.Context, string) ([]byte, error) { return nil, nil }
	runGoBuild = func(_ context.Context, d, out, _, _ string) ([]byte, error) {
		builds++
		return nil, os.WriteFile(filepath.Join(d, out), []byte("built-"+strconv.Itoa(builds)), 0755)
	}
	build := func() {
		t.Helper()
		rec := httptest.NewRecorder()
		handleUIBuild(rec, dashRequest(http.MethodPost, "/ui/build", nil), dir)
		if !strings.Contains(rec.Body.String(), "BUILD OK") {
			t.Fatalf("banner = %q", rec.Body.String())
		}
	}
	readBinary := func() string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	setVersion("1.0.0")
	build()
	if builds != 1 || readBinary() != "built-1" {
		t.Fatalf("first build: builds=%d binary=%q", builds, readBinary())
	}
	build()
	if builds != 1 || readBinary() != "built-1" {
		t.Fatalf("unchanged version rebuilt: builds=%d binary=%q", builds, readBinary())
	}

	// Simulate an update shipped over an installed binary, then rebuild.
	if err := os.WriteFile(filepath.Join(dir, rel), []byte("stale-1.0.0"), 0755); err != nil {
		t.Fatal(err)
	}
	setVersion("1.1.1")
	build()
	if builds != 2 || readBinary() != "built-2" {
		t.Fatalf("version change did not rebuild: builds=%d binary=%q", builds, readBinary())
	}
	build()
	if builds != 2 || readBinary() != "built-2" {
		t.Fatalf("new version not cached: builds=%d binary=%q", builds, readBinary())
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

// A failing test run must never replace the working binary: tests run before
// the swap, so the old binary stays live and the banner says so.
func TestFailedTestRunKeepsOldBinary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	rel := filepath.Join("dist", "walsy-system1-"+runtime.GOOS+"-"+runtime.GOARCH)
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rel), []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	origBuild, origVet, origTest := runGoBuild, runGoVet, runGoTest
	t.Cleanup(func() { runGoBuild, runGoVet, runGoTest = origBuild, origVet, origTest })
	runGoVet = func(context.Context, string) ([]byte, error) { return nil, nil }
	runGoBuild = func(_ context.Context, d, out, _, _ string) ([]byte, error) {
		if err := os.WriteFile(filepath.Join(d, out), []byte("new"), 0755); err != nil {
			return nil, err
		}
		return nil, nil
	}
	runGoTest = func(context.Context, string) ([]byte, error) {
		return []byte("--- FAIL: TestX"), errors.New("exit status 1")
	}
	rec := httptest.NewRecorder()
	handleUIBuild(rec, dashRequest(http.MethodPost, "/ui/build", nil), dir)
	if body := rec.Body.String(); !strings.Contains(body, "TESTS FAILED") {
		t.Fatalf("banner = %q", body)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, rel)); string(got) != "old" {
		t.Fatalf("failing build replaced the live binary: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, rel+".tmp")); !os.IsNotExist(err) {
		t.Fatal("failing build left its .tmp behind")
	}
}
