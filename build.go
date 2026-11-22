package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Exactly seven build target ids: six cross-compile pairs plus native.
var buildTargets = map[string][2]string{
	"darwin/arm64":  {"darwin", "arm64"},
	"darwin/amd64":  {"darwin", "amd64"},
	"linux/amd64":   {"linux", "amd64"},
	"linux/arm64":   {"linux", "arm64"},
	"windows/amd64": {"windows", "amd64"},
	"windows/arm64": {"windows", "arm64"},
}

const (
	buildTargetFile = "build-target.txt"
	buildTimeout    = 300 * time.Second
	maxBuildTail    = 4096
	errSourceDir    = "source directory not found; run the dashboard from the repo root"
)

var errUnknownBuildTarget = errors.New("unknown build target")

// resolveBuildTarget is the only trust boundary for target ids. "native" is
// resolved on every call so the host platform is picked at request time.
func resolveBuildTarget(id string) (string, string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "native" {
		return runtime.GOOS, runtime.GOARCH, nil
	}
	pair, ok := buildTargets[id]
	if !ok {
		return "", "", errUnknownBuildTarget
	}
	return pair[0], pair[1], nil
}

func buildTargetPath() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, buildTargetFile), nil
}

// loadBuildTarget fails open to native: no selection yet means this machine.
func loadBuildTarget() string {
	path, err := buildTargetPath()
	if err != nil {
		return "native"
	}
	data, err := readStateFile(path)
	if err != nil {
		return "native"
	}
	id := strings.ToLower(strings.TrimSpace(string(data)))
	if _, _, err := resolveBuildTarget(id); err != nil {
		return "native"
	}
	return id
}

func saveBuildTarget(id string) error {
	id = strings.ToLower(strings.TrimSpace(id))
	if _, _, err := resolveBuildTarget(id); err != nil {
		return err
	}
	path, err := buildTargetPath()
	if err != nil {
		return err
	}
	return writeStateFile(path, []byte(id+"\n"))
}

func effectiveBuildTarget() (string, string) {
	goos, goarch, _ := resolveBuildTarget(loadBuildTarget())
	return goos, goarch
}

// findSourceDir locates the module root from the running binary, falling back
// to the process working directory for `go run`.
func findSourceDir() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if st, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && st.Mode().IsRegular() {
			return dir
		}
	}
	if dir, err := os.Getwd(); err == nil {
		if st, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && st.Mode().IsRegular() {
			return dir
		}
	}
	return ""
}

func buildOutputTail(out []byte) string {
	if len(out) > maxBuildTail {
		out = out[len(out)-maxBuildTail:]
	}
	return string(out)
}

// renderBuildResult reuses the dashboard template; html/template escapes the
// captured toolchain output before it reaches the status region.
func renderBuildResult(w http.ResponseWriter, status string) {
	p := uiHomePage("")
	p.BuildStatus = status
	renderPage(w, p)
}

func handleUISetBuildTarget(w http.ResponseWriter, r *http.Request) {
	form, ok := uiForm(w, r)
	if !ok {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	id := form.Get("target")
	if _, _, err := resolveBuildTarget(id); err != nil {
		http.Error(w, "unknown build target", http.StatusBadRequest)
		return
	}
	if err := saveBuildTarget(id); err != nil {
		http.Error(w, "state write failed", http.StatusBadRequest)
		return
	}
	renderBuildResult(w, "TARGET SET: "+id)
}

// buildSlot is a single-slot guard: a second /ui/build while one is running is
// refused instead of spawning another 300s toolchain process.
var buildSlot = make(chan struct{}, 1)

// runGoBuild is a var so tests can hold the slot without invoking the toolchain.
var runGoBuild = func(ctx context.Context, dir, rel, goos, goarch string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-o", rel, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	return cmd.CombinedOutput()
}

func handleUIBuild(w http.ResponseWriter, r *http.Request, sourceDir string) {
	select {
	case buildSlot <- struct{}{}:
		defer func() { <-buildSlot }()
	default:
		renderBuildResult(w, "BUILD ALREADY RUNNING")
		return
	}
	if sourceDir == "" {
		// Same 200 + status banner shape as every other build failure; a status
		// code here raced renderPage's own headers.
		renderBuildResult(w, errSourceDir)
		return
	}
	goos, goarch := effectiveBuildTarget()
	name := "walsy-system1-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	rel := filepath.ToSlash(filepath.Join("dist", name))
	if err := os.MkdirAll(filepath.Join(sourceDir, "dist"), 0755); err != nil {
		renderBuildResult(w, "BUILD FAILED: dist directory unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), buildTimeout)
	defer cancel()
	out, err := runGoBuild(ctx, sourceDir, rel, goos, goarch)
	tail := buildOutputTail(out)
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		renderBuildResult(w, "BUILD TIMED OUT AFTER 300s\n"+tail)
	case err != nil:
		renderBuildResult(w, "BUILD FAILED\n"+tail)
	default:
		renderBuildResult(w, "BUILD OK: "+rel+" ("+goos+"/"+goarch+")\n"+tail)
	}
}
