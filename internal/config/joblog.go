package config

import (
	"fmt"
	"path/filepath"
)

// validateJobNamesAndLogPaths refuses a repeated job name, then a repeated log file.
func validateJobNamesAndLogPaths(jobs []JobConfig) error {
	if err := validateJobNames(jobs); err != nil {
		return err
	}
	return validateJobLogPaths(jobs)
}

// Symlink aliasing is out of scope: the log file need not exist at load time.
func validateJobLogPaths(jobs []JobConfig) error {
	seen := make(map[string]int, len(jobs))
	for i := 0; i < len(jobs); i++ {
		clean := filepath.Clean(jobs[i].LogPath)
		if first, ok := seen[clean]; ok {
			return fmt.Errorf("config: jobs/%d/log_path: %q names the same file as jobs/%d (%q)", i, jobs[i].LogPath, first, jobs[first].LogPath)
		}
		seen[clean] = i
	}
	return nil
}
