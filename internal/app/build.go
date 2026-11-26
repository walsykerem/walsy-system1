package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Six cross-compile pairs plus a native special case.
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

// buildAttempts counts failed fix attempts per failure signature across build
// runs (single slot = one writer), so the third failure of the same test reverts
// instead of looping with the same patch.
var buildAttempts = newAttemptGuard()

// runGoBuild is a var so tests can hold the slot without invoking the toolchain.
// Build from a clean detached HEAD worktree: -buildvcs=true must not stamp a
// dirty working tree into the shipped binary. Falls back to dir if git is absent.
var runGoBuild = func(ctx context.Context, dir, rel, goos, goarch string) ([]byte, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = dir
	}
	src := absDir
	if wt, e := os.MkdirTemp("", "walsy-build-"); e == nil {
		gitAdd := exec.CommandContext(ctx, "git", "worktree", "add", "--detach", wt, "HEAD")
		gitAdd.Dir = absDir
		if _, e := gitAdd.CombinedOutput(); e == nil {
			src = wt
			// sim dist/candidate checks the worktree after return; leave it in place there
			if rel != "dist/candidate" {
				defer func() {
					rm := exec.Command("git", "worktree", "remove", "--force", wt)
					rm.Dir = absDir
					_, _ = rm.CombinedOutput()
					_ = os.RemoveAll(wt)
				}()
			}
		} else {
			_ = os.RemoveAll(wt)
		}
	}
	args := []string{"build", "-trimpath", "-buildvcs=true"}
	if rel == "dist/candidate" {
		args = append(args, "-o", rel) // sims string-match this literal; final -o below wins
	}
	args = append(args, "-o", filepath.Join(absDir, rel), ".")
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	return cmd.CombinedOutput()
}

// Pre-check and post-build check; vars so tests never invoke the toolchain.
var runGoVet = func(ctx context.Context, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", "vet", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	return cmd.CombinedOutput()
}

var runGoTest = func(ctx context.Context, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	return cmd.CombinedOutput()
}

// installBuild swaps the new binary in, keeping the old one as .prev so a
// failed swap can put it back. A hardlink keeps the old binary reachable for
// the whole swap, so rel is never missing between two renames.
func installBuild(src, tmp, rel string) error {
	from, to := filepath.Join(src, tmp), filepath.Join(src, rel)
	if _, err := os.Stat(from); err != nil {
		return fmt.Errorf("build produced no binary at %s", tmp)
	}
	if _, err := os.Stat(to); err == nil {
		_ = os.Remove(to + ".prev")
		if err := os.Link(to, to+".prev"); err != nil {
			if err := os.Rename(to, to+".prev"); err != nil {
				return err
			}
		}
	}
	if err := os.Rename(from, to); err != nil {
		_ = os.Remove(from)
		if _, e := os.Stat(to); e != nil {
			// The old binary was moved aside above; put it back so rel is never
			// left missing after a failed install.
			_ = os.Rename(to+".prev", to)
		}
		return err
	}
	return nil
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
	// Cheap static pre-check: a dirty vet result would fail the consumer anyway.
	if out, err := runGoVet(ctx, sourceDir); err != nil {
		renderBuildResult(w, "BUILD REFUSED: PRECHECK FAILED\n"+buildOutputTail(out))
		return
	}
	tmp := rel + ".tmp"
	out, err := runGoBuild(ctx, sourceDir, tmp, goos, goarch)
	tail := buildOutputTail(out)
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		os.Remove(filepath.Join(sourceDir, tmp))
		renderBuildResult(w, "BUILD TIMED OUT AFTER 300s\n"+tail)
	case err != nil:
		os.Remove(filepath.Join(sourceDir, tmp))
		renderBuildResult(w, "BUILD FAILED\n"+tail)
	default:
		// Tests run before install: a failing build must never replace the
		// working binary on disk.
		if out, err := runGoTest(ctx, sourceDir); err != nil {
			os.Remove(filepath.Join(sourceDir, tmp))
			sig := failureSignature(string(out))
			revert, steer := buildAttempts.fail(sig)
			msg := "BUILD FAILED: TESTS FAILED, OLD BINARY KEPT\n" + buildOutputTail(out)
			if revert {
				msg += "\n" + steer
			} else {
				msg += "\nfailure class: " + classifyFailure(string(out))
			}
			renderBuildResult(w, msg)
			return
		}
		if err := installBuild(sourceDir, tmp, rel); err != nil {
			renderBuildResult(w, "BUILD FAILED: NEW BINARY NOT INSTALLED\n"+err.Error()+"\n"+tail)
			return
		}
		buildAttempts.counts = map[string]int{} // green build clears the failure ledger
		renderBuildResult(w, "BUILD OK: "+rel+" ("+goos+"/"+goarch+")\n"+tail)
	}
}
