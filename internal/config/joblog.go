package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// validateJobNamesAndLogPaths refuses a repeated job name, then a repeated log file.
func validateJobNamesAndLogPaths(jobs []JobConfig) error {
	if err := validateJobNames(jobs); err != nil {
		return err
	}
	return validateJobLogPaths(jobs)
}

// Each path resolves through symlinks in its longest existing ancestor, so a
// symlinked directory or file naming the same log is refused; a symlink created
// after load is not seen; .. is cleaned lexically before resolution.
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

// resolveExistingAncestor resolves symlinks in the longest prefix of the clean
// absolute path that filepath.EvalSymlinks can resolve, then joins the
// unresolved rest back on. Any EvalSymlinks error (missing, permission denied,
// or not a directory) walks one directory up so symlinks above an uncreated or
// inaccessible component still resolve.
func resolveExistingAncestor(abs string) string {
	abs = filepath.Clean(abs)
	maxSteps := strings.Count(abs, string(filepath.Separator)) + 1
	dir := abs
	rest := ""
	for step := 0; step < maxSteps; step++ {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
	return abs
}
