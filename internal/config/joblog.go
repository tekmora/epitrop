package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
// Windows .. is already gone after normalization); from the first component
// that does not resolve (missing, permission denied, not a directory) the rest
// joins on lexically; a symlink created after load is not seen.
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

// resolveExistingAncestor resolves each existing component of abs through
// symlinks in order, so on Unix a .. after a symlink climbs from the link
// target, as the shim's open does (on Windows .. is already gone after
// normalization); from the first component that does not resolve (missing,
// permission denied, or not a directory) the rest joins on lexically; a
// symlink created after load is not seen.
func resolveExistingAncestor(abs string) string {
	vol := filepath.VolumeName(abs)
	rest := abs[len(vol):]
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == filepath.Separator })
	resolved := vol + string(filepath.Separator)
	for i := 0; i < len(parts); i++ {
		next, err := filepath.EvalSymlinks(filepath.Join(resolved, parts[i]))
		if err != nil {
			return filepath.Join(append([]string{resolved}, parts[i:]...)...)
		}
		resolved = next
	}
	return resolved
}
