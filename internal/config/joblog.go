package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// MaxLogBackupIndex is the highest rotated index the job shim creates or prunes.
const MaxLogBackupIndex = MaxLogKeepFiles + 1

const (
	maxLogPathResolveSteps = 4096
	maxLogPathLinkHops     = 40
)

type foldedLogPath struct {
	prefix os.FileInfo
	tail   []string
}

type rotatedLogVia struct {
	walked   string
	baseFold foldedLogPath
}

type existingLogFile struct {
	path string
	info os.FileInfo
}

type jobLogRecord struct {
	open         string
	entry        string
	openInfo     os.FileInfo
	openFold     foldedLogPath
	entryFold    foldedLogPath
	rotBases     []foldedLogPath
	rotVia       []rotatedLogVia
	rotatedFiles []existingLogFile
}

// validateJobNamesAndLogPaths refuses a repeated job name, then a repeated log file.
func validateJobNamesAndLogPaths(jobs []JobConfig) error {
	if err := validateJobNames(jobs); err != nil {
		return err
	}
	return validateJobLogPaths(jobs)
}

// validateJobLogPaths resolves each job's log_path through symlinks in order,
// so on Unix a .. after a symlink climbs from the link target, as the shim's
// open does (on Windows .. is already gone after normalization). A symlink
// whose target does not exist yet is followed to its target, because the shim's
// O_CREATE open creates that target; at most 40 links are followed, the Linux
// limit for one path, and from the first component that cannot resolve the rest
// joins on lexically. Two paths name one file when (A) os.Stat succeeds on both
// and os.SameFile reports one file (hard links, and existing case aliases on a
// case-folding volume), or (B) walking up from filepath.Dir reaches deepest
// existing prefixes that satisfy os.SameFile and the remaining missing
// directories and base name match one by one under strings.EqualFold; (B)
// applies on every volume, refusing case-only names on a case-sensitive volume
// too, because the case rule of a volume holding a missing file is not known at
// load. Names differing only by Unicode normalization (APFS) or full case
// folding ("ß" vs "SS"), and aliases created after load, are not refused. Job i
// is refused when its open target matches an earlier job's open target under
// (A) or (B), when rotatedLogBase of its open, entry, or walked via paths
// matches any job's entry under (B), or when its existing open target matches
// an existing entry.1 to entry.101 of any job under (A).
func validateJobLogPaths(jobs []JobConfig) error {
	records := make([]jobLogRecord, 0, len(jobs))
	for i := 0; i < len(jobs); i++ {
		rec, err := buildJobLogRecord(jobs[i].LogPath)
		if err != nil {
			return fmt.Errorf("config: jobs/%d/log_path: %w", i, err)
		}
		if first, ok := duplicateJobLogOpen(rec, records); ok {
			return fmt.Errorf("config: jobs/%d/log_path: %q names the same file as jobs/%d (%q)", i, jobs[i].LogPath, first, jobs[first].LogPath)
		}
		records = append(records, rec)
	}
	return validateJobLogRotatedNames(jobs, records)
}

func buildJobLogRecord(path string) (jobLogRecord, error) {
	open, entry, via, err := jobLogKeys(path)
	if err != nil {
		return jobLogRecord{}, err
	}
	openFold := foldLogPath(open)
	entryFold := openFold
	if entry != open {
		entryFold = foldLogPath(entry)
	}
	return jobLogRecord{
		open:      open,
		entry:     entry,
		openInfo:  statExisting(open),
		openFold:  openFold,
		entryFold: entryFold,
		rotBases:  foldRotatedCandidates(open, entry),
		rotVia:    foldRotatedVia(via),
	}, nil
}

func foldRotatedCandidates(open, entry string) []foldedLogPath {
	var out []foldedLogPath
	if base, ok := rotatedLogBase(open); ok {
		out = append(out, foldLogPath(base))
	}
	if entry != open {
		if base, ok := rotatedLogBase(entry); ok {
			out = append(out, foldLogPath(base))
		}
	}
	return out
}

func foldRotatedVia(via []string) []rotatedLogVia {
	var out []rotatedLogVia
	for i := 0; i < len(via); i++ {
		if base, ok := rotatedLogBase(via[i]); ok {
			out = append(out, rotatedLogVia{walked: via[i], baseFold: foldLogPath(base)})
		}
	}
	return out
}

func duplicateJobLogOpen(rec jobLogRecord, earlier []jobLogRecord) (int, bool) {
	for j := 0; j < len(earlier); j++ {
		if sameExistingFile(rec.openInfo, earlier[j].openInfo) || sameFoldedLogPath(rec.openFold, earlier[j].openFold) {
			return j, true
		}
	}
	return 0, false
}

// validateJobLogRotatedNames refuses any job whose open target, directory
// entry, or resolution walk hits a rotated name or existing rotated file of any job.
func validateJobLogRotatedNames(jobs []JobConfig, records []jobLogRecord) error {
	populateExistingRotatedFiles(records)
	for i := 0; i < len(records); i++ {
		if j, ok := rotatedLogOwner(records[i], records); ok {
			return fmt.Errorf("config: jobs/%d/log_path: %q is a rotated name of jobs/%d (%q)", i, jobs[i].LogPath, j, jobs[j].LogPath)
		}
		if walked, j, ok := rotatedLogViaOwner(records[i].rotVia, records); ok {
			return fmt.Errorf("config: jobs/%d/log_path: %q passes through %q, a rotated name of jobs/%d (%q)", i, jobs[i].LogPath, walked, j, jobs[j].LogPath)
		}
		if rotPath, j, ok := rotatedLogFileOwner(records[i].openInfo, records); ok {
			return fmt.Errorf("config: jobs/%d/log_path: %q names the same file as %q, a rotated name of jobs/%d (%q)", i, jobs[i].LogPath, rotPath, j, jobs[j].LogPath)
		}
	}
	return nil
}

func populateExistingRotatedFiles(records []jobLogRecord) {
	if !anyOpenFileExists(records) {
		return
	}
	for i := 0; i < len(records); i++ {
		records[i].rotatedFiles = existingRotatedLogFiles(records[i].entry)
	}
}

func anyOpenFileExists(records []jobLogRecord) bool {
	for i := 0; i < len(records); i++ {
		if records[i].openInfo != nil {
			return true
		}
	}
	return false
}

func existingRotatedLogFiles(entry string) []existingLogFile {
	var found []existingLogFile
	for n := 1; n <= MaxLogBackupIndex; n++ {
		rotPath := entry + "." + strconv.Itoa(n)
		if info := statExisting(rotPath); info != nil {
			found = append(found, existingLogFile{path: rotPath, info: info})
		}
	}
	return found
}

// rotatedLogOwner checks open and entry rotated bases against every job's entry under (B).
func rotatedLogOwner(rec jobLogRecord, records []jobLogRecord) (int, bool) {
	for k := 0; k < len(rec.rotBases); k++ {
		if owner, ok := matchEntryFold(rec.rotBases[k], records); ok {
			return owner, true
		}
	}
	return 0, false
}

// rotatedLogViaOwner checks each rotated path the open walk stepped through against every job's entry under (B).
func rotatedLogViaOwner(rotVia []rotatedLogVia, records []jobLogRecord) (string, int, bool) {
	for k := 0; k < len(rotVia); k++ {
		if owner, ok := matchEntryFold(rotVia[k].baseFold, records); ok {
			return rotVia[k].walked, owner, true
		}
	}
	return "", 0, false
}

func matchEntryFold(candidate foldedLogPath, records []jobLogRecord) (int, bool) {
	for j := 0; j < len(records); j++ {
		if sameFoldedLogPath(candidate, records[j].entryFold) {
			return j, true
		}
	}
	return 0, false
}

func rotatedLogFileOwner(openInfo os.FileInfo, records []jobLogRecord) (string, int, bool) {
	if openInfo == nil {
		return "", 0, false
	}
	for j := 0; j < len(records); j++ {
		if rotPath, ok := matchExistingRotatedFile(openInfo, records[j].rotatedFiles); ok {
			return rotPath, j, true
		}
	}
	return "", 0, false
}

func matchExistingRotatedFile(openInfo os.FileInfo, files []existingLogFile) (string, bool) {
	for k := 0; k < len(files); k++ {
		if sameExistingFile(openInfo, files[k].info) {
			return files[k].path, true
		}
	}
	return "", false
}

func statExisting(path string) os.FileInfo {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return info
}

// sameExistingFile checks rule (A): both paths exist and os.SameFile reports one file.
func sameExistingFile(first, second os.FileInfo) bool {
	return first != nil && second != nil && os.SameFile(first, second)
}

// foldLogPath walks up from filepath.Dir(key) to the deepest existing prefix
// and records the remaining missing-directory and base-name components for (B).
func foldLogPath(key string) foldedLogPath {
	cur := filepath.Clean(key)
	if filepath.Dir(cur) == cur {
		return foldedLogPath{prefix: statExisting(cur)}
	}
	bound := len(splitLogPathComponents(cur[len(filepath.VolumeName(cur)):]))
	rev := make([]string, 0, bound)
	for step := 0; step < bound; step++ {
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		rev = append(rev, filepath.Base(cur))
		cur = parent
		if info := statExisting(cur); info != nil {
			slices.Reverse(rev)
			return foldedLogPath{prefix: info, tail: rev}
		}
	}
	return foldedLogPath{}
}

// sameFoldedLogPath checks rule (B): one existing ancestor directory and case-equal tail components.
func sameFoldedLogPath(first, second foldedLogPath) bool {
	if first.prefix == nil || second.prefix == nil || !os.SameFile(first.prefix, second.prefix) || len(first.tail) != len(second.tail) {
		return false
	}
	for i := 0; i < len(first.tail); i++ {
		if !strings.EqualFold(first.tail[i], second.tail[i]) {
			return false
		}
	}
	return true
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
