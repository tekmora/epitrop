package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	maxLogPathResolveSteps = 4096
	maxLogPathLinkHops     = 40
)

// validateJobNamesAndLogPaths refuses a repeated job name, then a repeated log file.
func validateJobNamesAndLogPaths(jobs []JobConfig) error {
	if err := validateJobNames(jobs); err != nil {
		return err
	}
	return validateJobLogPaths(jobs)
}

// Each existing component resolves through symlinks in order, so on Unix a ..
// after a symlink climbs from the link target, as the shim's open does (on
// Windows .. is already gone after normalization). A symlink whose target does
// not exist yet is followed to its target, because the shim's O_CREATE open
// creates that target; at most 40 such symlinks are followed, the Linux limit
// for one path. From the first component that still cannot resolve (missing,
// permission denied, not a directory, unreadable symlink, or past the
// 40-symlink cap) the rest joins on lexically; a symlink created after load is
// not seen.
func validateJobLogPaths(jobs []JobConfig) error {
	seen := make(map[string]int, len(jobs))
	for i := 0; i < len(jobs); i++ {
		resolved, err := resolvePossiblyMissingAbs(jobs[i].LogPath)
		if err != nil {
			return fmt.Errorf("config: jobs/%d/log_path: %w", i, err)
		}
		if first, ok := seen[resolved]; ok {
			return fmt.Errorf("config: jobs/%d/log_path: %q names the same file as jobs/%d (%q)", i, jobs[i].LogPath, first, jobs[first].LogPath)
		}
		seen[resolved] = i
	}
	return nil
}

// unresolvedAbs on Unix returns path unchanged when absolute and otherwise
// prepends the working directory without cleaning .., because the open follows
// a symlink before a later ..; on Windows it returns filepath.Abs, because
// Windows normalizes the path (drive-relative forms, .. lexically) before the
// open.
func unresolvedAbs(path string) (string, error) {
	if runtime.GOOS == "windows" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("config: abs %s: %w", path, err)
		}
		return abs, nil
	}
	if filepath.IsAbs(path) {
		return path, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("config: abs %s: %w", path, err)
	}
	return wd + string(filepath.Separator) + path, nil
}

// resolveExistingAncestor resolves each component of abs through symlinks in
// order, so on Unix a .. after a symlink climbs from the link target, as the
// shim's open does (on Windows .. is already gone after normalization). A
// symlink whose target does not exist yet is followed to its target, because
// the shim's O_CREATE open creates that target; at most 40 such symlinks are
// followed, the Linux limit for one path. From the first component that still
// cannot resolve (missing, permission denied, not a directory, unreadable
// symlink, or past the 40-symlink cap) the rest joins on lexically; a symlink
// created after load is not seen.
func resolveExistingAncestor(abs string) string {
	vol := filepath.VolumeName(abs)
	resolved := vol + string(filepath.Separator)
	pending := splitLogPathComponents(abs[len(vol):])
	hops := 0
	for step := 0; step < maxLogPathResolveSteps && len(pending) > 0; step++ {
		candidate := filepath.Join(resolved, pending[0])
		if next, err := filepath.EvalSymlinks(candidate); err == nil {
			resolved = next
			pending = pending[1:]
			continue
		}
		target, ok := readLogPathSymlink(candidate)
		if !ok || hops >= maxLogPathLinkHops {
			break
		}
		hops++
		resolved, pending = spliceLogPathTarget(resolved, target, pending[1:])
	}
	return filepath.Join(append([]string{resolved}, pending...)...)
}

// splitLogPathComponents splits path on forward slashes and the host path separator.
func splitLogPathComponents(path string) []string {
	return strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == filepath.Separator })
}

// readLogPathSymlink returns path's link target when path is a symbolic link.
func readLogPathSymlink(path string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	target, err := os.Readlink(path)
	if err != nil || target == "" {
		return "", false
	}
	return target, true
}

// spliceLogPathTarget prepends target's components before rest, restarting at target's root when absolute.
func spliceLogPathTarget(resolved, target string, rest []string) (string, []string) {
	if filepath.IsAbs(target) {
		vol := filepath.VolumeName(target)
		resolved = vol + string(filepath.Separator)
		target = target[len(vol):]
	}
	return resolved, append(splitLogPathComponents(target), rest...)
}
