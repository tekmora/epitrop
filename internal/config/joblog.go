package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// MaxLogBackupIndex is the highest rotated index the job shim creates or prunes.
const MaxLogBackupIndex = MaxLogKeepFiles + 1

const (
	maxLogPathResolveSteps = 4096
	maxLogPathLinkHops     = 40
)

type jobLogKeyPair struct {
	open  string
	entry string
	via   []string
}

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
// not seen. A path the shim of another job renames or prunes, log_path.1 to
// log_path.101 through any alias, is refused. A log path whose resolution walks
// through a rotated name (a link in the chain, an existing directory, or a
// missing directory the shim creates) is refused too.
func validateJobLogPaths(jobs []JobConfig) error {
	seen := make(map[string]int, len(jobs))
	entries := make(map[string]int, len(jobs))
	keys := make([]jobLogKeyPair, len(jobs))
	for i := 0; i < len(jobs); i++ {
		open, entry, via, err := jobLogKeys(jobs[i].LogPath)
		if err != nil {
			return fmt.Errorf("config: jobs/%d/log_path: %w", i, err)
		}
		if first, ok := seen[open]; ok {
			return fmt.Errorf("config: jobs/%d/log_path: %q names the same file as jobs/%d (%q)", i, jobs[i].LogPath, first, jobs[first].LogPath)
		}
		seen[open] = i
		entries[entry] = i
		keys[i] = jobLogKeyPair{open: open, entry: entry, via: via}
	}
	return validateJobLogRotatedNames(jobs, keys, entries)
}

// validateJobLogRotatedNames refuses any job whose open target, directory
// entry, or resolution walk hits a rotated name of any job's directory entry.
func validateJobLogRotatedNames(jobs []JobConfig, keys []jobLogKeyPair, entries map[string]int) error {
	for i := 0; i < len(keys); i++ {
		if j, ok := rotatedLogOwner(keys[i], entries); ok {
			return fmt.Errorf("config: jobs/%d/log_path: %q is a rotated name of jobs/%d (%q)", i, jobs[i].LogPath, j, jobs[j].LogPath)
		}
		if walked, j, ok := rotatedLogViaOwner(keys[i].via, entries); ok {
			return fmt.Errorf("config: jobs/%d/log_path: %q passes through %q, a rotated name of jobs/%d (%q)", i, jobs[i].LogPath, walked, j, jobs[j].LogPath)
		}
	}
	return nil
}

// rotatedLogOwner checks pair.open and then pair.entry against entries.
func rotatedLogOwner(pair jobLogKeyPair, entries map[string]int) (int, bool) {
	candidates := [2]string{pair.open, pair.entry}
	for k := 0; k < len(candidates); k++ {
		if base, ok := rotatedLogBase(candidates[k]); ok {
			if owner, exists := entries[base]; exists {
				return owner, true
			}
		}
	}
	return 0, false
}

// rotatedLogViaOwner checks each path the open walk stepped through against entries.
func rotatedLogViaOwner(via []string, entries map[string]int) (string, int, bool) {
	for k := 0; k < len(via); k++ {
		if base, ok := rotatedLogBase(via[k]); ok {
			if owner, exists := entries[base]; exists {
				return via[k], owner, true
			}
		}
	}
	return "", 0, false
}

// jobLogKeys returns open, the file the shim's open writes, entry, the
// directory entry its rotation renames, so a log_path that is itself a symlink
// keeps its own name as entry, and via, the paths the open walk steps through.
// A base of "", "." or ".." needs no special case, because the join cleans it
// to the key the open resolves.
func jobLogKeys(path string) (string, string, []string, error) {
	abs, err := unresolvedAbs(path)
	if err != nil {
		return "", "", nil, err
	}
	open, via := walkLogPath(abs)
	dir, base := filepath.Split(abs)
	entry := filepath.Join(resolveExistingAncestor(dir), base)
	return open, entry, via, nil
}

// rotatedLogBase strips a canonical decimal suffix in 1..MaxLogBackupIndex.
func rotatedLogBase(key string) (string, bool) {
	dot := strings.LastIndexByte(key, '.')
	if dot <= 0 {
		return "", false
	}
	suffix := key[dot+1:]
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 1 || n > MaxLogBackupIndex || strconv.Itoa(n) != suffix {
		return "", false
	}
	return key[:dot], true
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

// protectedPathForms returns the resolved forms of path that the sandbox guard
// protects: the open reaches the physical form; a writer that joins onto the
// path (the event log writer joins events with filepath.Join) reaches the
// lexical form, so both are protected.
func protectedPathForms(path string) ([]string, error) {
	physical, err := resolvePossiblyMissingAbs(path)
	if err != nil {
		return nil, err
	}
	cleaned, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: abs %s: %w", path, err)
	}
	lexical := resolveExistingAncestor(cleaned)
	if lexical == physical {
		return []string{physical}, nil
	}
	return []string{physical, lexical}, nil
}

// resolveExistingAncestor resolves each component of abs through symlinks in
// order, so on Unix a .. after a symlink climbs from the link target, as the
// shim's open does (on Windows .. is already gone after normalization). A
// symlink whose target does not exist yet is followed to its target, because
// the shim's O_CREATE open creates that target; at most 40 such symlinks are
// followed, the Linux limit for one path. Every link of the walk counts toward
// the 40-link cap, existing or dangling, as the Linux open counts them. From
// the first component that still cannot resolve (missing, permission denied,
// not a directory, unreadable symlink, or past the 40-symlink cap) the rest
// joins on lexically; a symlink created after load is not seen.
func resolveExistingAncestor(abs string) string {
	key, _ := walkLogPath(abs)
	return key
}

// walkLogPath resolves abs component by component and records every symlink,
// existing entry, and missing trailing path the walk steps through.
func walkLogPath(abs string) (string, []string) {
	vol := filepath.VolumeName(abs)
	resolved := vol + string(filepath.Separator)
	pending := splitLogPathComponents(abs[len(vol):])
	walked := make([]string, 0, len(pending))
	hops := 0
	for step := 0; step < maxLogPathResolveSteps && len(pending) > 0; step++ {
		candidate := filepath.Join(resolved, pending[0])
		if target, ok := readLogPathSymlink(candidate); ok {
			if hops >= maxLogPathLinkHops {
				break
			}
			hops++
			walked = append(walked, candidate)
			resolved, pending = spliceLogPathTarget(resolved, target, pending[1:])
			continue
		}
		next, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			break
		}
		resolved = next
		walked = append(walked, next)
		pending = pending[1:]
	}
	key := filepath.Clean(resolved)
	for i := 0; i < len(pending); i++ {
		key = filepath.Join(key, pending[i])
		walked = append(walked, key)
	}
	return key, walked
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

// spliceLogPathTarget prepends target's components before rest, restarting at
// target's own root when absolute, at the link's volume root when root-relative
// on Windows, or keeping the link's directory when relative.
func spliceLogPathTarget(resolved, target string, rest []string) (string, []string) {
	if filepath.IsAbs(target) {
		vol := filepath.VolumeName(target)
		resolved = vol + string(filepath.Separator)
		target = target[len(vol):]
	} else if target != "" && filepath.VolumeName(target) == "" && os.IsPathSeparator(target[0]) {
		resolved = filepath.VolumeName(resolved) + string(filepath.Separator)
	}
	return resolved, append(splitLogPathComponents(target), rest...)
}
