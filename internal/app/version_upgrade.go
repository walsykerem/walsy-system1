package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Repo tags are major.minor.patch[-fix-N] (0.0.1, 0.1.0-fix-9). The requested
// 1.1e has no such format: its trailing letter maps to patch 0 (1.1e = 1.1.0).
// shortcut: letter suffixes are not real prereleases; adopt semver prerelease
// parsing if release.tag ever changes shape.
const versionCleanupFloor = "1.1e"

type parsedVersion struct{ major, minor, patch, fix int }

func parseVersion(s string) (parsedVersion, bool) {
	v := parsedVersion{}
	base, suffix, hasSuffix := strings.Cut(strings.TrimSpace(s), "-")
	fields := strings.Split(base, ".")
	if len(fields) < 2 || len(fields) > 3 {
		return v, false
	}
	nums := [3]int{}
	for i, f := range fields {
		if len(f) > 0 && f[len(f)-1] >= 'a' && f[len(f)-1] <= 'z' {
			f = f[:len(f)-1]
		}
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return v, false
		}
		nums[i] = n
	}
	v.major, v.minor, v.patch = nums[0], nums[1], nums[2]
	if hasSuffix {
		n, ok := strings.CutPrefix(suffix, "fix-")
		if !ok {
			return v, false
		}
		fix, err := strconv.Atoi(n)
		if err != nil || fix < 0 {
			return v, false
		}
		v.fix = fix
	}
	return v, true
}

func versionLess(a, b parsedVersion) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	if a.minor != b.minor {
		return a.minor < b.minor
	}
	if a.patch != b.patch {
		return a.patch < b.patch
	}
	return a.fix < b.fix
}

// upgradePackage installs target into dir and removes older pkg-* directories
// once target reaches versionCleanupFloor. dir/VERSION records the installed
// version; install writes new package files into its staging directory. A
// failed install leaves the old state untouched.
func upgradePackage(dir, installed, target string, install func(staging string) error) error {
	iv, ok := parseVersion(installed)
	if !ok {
		return fmt.Errorf("invalid installed version %q", installed)
	}
	tv, ok := parseVersion(target)
	if !ok {
		return fmt.Errorf("invalid target version %q", target)
	}
	floor, _ := parseVersion(versionCleanupFloor)
	if versionLess(tv, floor) || !versionLess(iv, tv) {
		return nil
	}
	staged, err := os.MkdirTemp(dir, ".stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staged)
	if err := install(staged); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var stale []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "pkg-") {
			continue
		}
		pv, ok := parseVersion(strings.TrimPrefix(e.Name(), "pkg-"))
		if ok && versionLess(pv, tv) {
			stale = append(stale, e.Name())
		}
	}
	// Prepare the new VERSION file before touching packages so the only
	// fallible step left at commit time is an atomic rename.
	newVersionPath := filepath.Join(dir, ".VERSION.new")
	if err := os.WriteFile(newVersionPath, []byte(target+"\n"), 0600); err != nil {
		return err
	}
	backup, err := os.MkdirTemp(dir, ".backup-")
	if err != nil {
		os.Remove(newVersionPath)
		return err
	}
	targetPath := filepath.Join(dir, "pkg-"+target)
	targetInstalled := false
	var moved []string
	rollback := func(cause error) error {
		errs := []error{cause}
		for i := len(moved) - 1; i >= 0; i-- {
			if err := os.Rename(filepath.Join(backup, moved[i]), filepath.Join(dir, moved[i])); err != nil {
				errs = append(errs, err)
			}
		}
		if targetInstalled {
			if err := os.RemoveAll(targetPath); err != nil {
				errs = append(errs, err)
			}
		}
		if err := os.Remove(newVersionPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		if len(errs) == 1 {
			os.RemoveAll(backup)
		}
		return errors.Join(errs...)
	}
	if err := os.Rename(staged, targetPath); err != nil {
		return rollback(err)
	}
	targetInstalled = true
	for _, name := range stale {
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(backup, name)); err != nil {
			return rollback(err)
		}
		moved = append(moved, name)
	}
	if err := os.Rename(newVersionPath, filepath.Join(dir, "VERSION")); err != nil {
		return rollback(err)
	}
	// Commit succeeded; discarding the backup of stale packages is best effort.
	os.RemoveAll(backup)
	return nil
}
