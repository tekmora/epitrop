// Package claudelocal measures the Claude Code subscription's usage windows
// from the local session logs. It is read-only: it never writes to the
// projects directory.
//
// Session logs are JSON Lines under ~/.claude/projects/**/*.jsonl. Two line
// shapes matter (field names verified against the logs on kcromm,
// 2026-10-13; values not read):
//
//	{"type":"assistant","timestamp":"...","requestId":"...","sessionId":"...","agentId":"...",
//	 "message":{"id":"...","model":"claude-opus-5-5","usage":{"input_tokens":N,"output_tokens":N,
//	   "cache_creation_input_tokens":N,"cache_read_input_tokens":N,
//	   "cache_creation":{"ephemeral_5m_input_tokens":N,"ephemeral_1h_input_tokens":N},"speed":"..."}}}
//	{"type":"assistant","timestamp":"...","quotaLimits":{"status":"rejected","rateLimitType":"five_hour","resetsAt":1789858494}}
//
// Only these usage and quota fields are decoded. Message content, tool input
// and every other field is never decoded into a value, kept or logged.
//
// The provider reports no usage percentage in these logs, so the source
// derives one. The cost-equivalent of the usage inside the window is divided
// by a ceiling: the cost-equivalent the window held when the provider
// rejected a request. Without an observed rejection the ceiling is unknown,
// and the record carries no used_percent; consumers treat an absent
// used_percent as unknown.
//
// Ported from cordanaLLM/praetor tribunus/internal/sources/claudelocal at 5d4acfb (EUPL-1.2).
package claudelocal

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/tekmora/epitrop/catalog"
	"github.com/tekmora/epitrop/internal/sources/sessionlog"
)

// SourceName identifies this source in Snapshot.SourceRuns and CLI flags.
const SourceName = "claude-local"

const (
	// MaxRecords is the maximum number of records this source can contribute
	// to one snapshot.
	MaxRecords = maxStoredWindows
	// maxEntriesWalked bounds the directory walk (HISS-02).
	maxEntriesWalked = 50000
	// maxFilesRead bounds the session files read in one fetch.
	maxFilesRead = 10000
	// maxUsageEvents bounds the usage entries kept across all files.
	maxUsageEvents = 1000000
	// maxRejections bounds the rejected-request entries kept.
	maxRejections = 10000
	// weeklyWindow is the longest window measured; files not written within
	// it cannot hold usage of any current window and are not read.
	weeklyWindow = 7 * 24 * time.Hour
	// burnWindow is the sliding window the burn and the active agents are measured over.
	burnWindow = 15 * time.Minute
)

// scanLimits bounds the scan of one session file. Lines longer than
// MaxLineBytes (large tool results) are skipped whole.
var scanLimits = sessionlog.Limits{MaxFileBytes: 256 << 20, MaxLines: 1000000, MaxLineBytes: 8 << 20}

// Result is one Fetch attempt's outcome.
type Result struct {
	Records []catalog.Record
	Status  catalog.Status
	Count   int
	Detail  string
}

// DefaultProjectsDir returns ~/.claude/projects. Callers pass their own
// directory to Fetch so tests never depend on $HOME.
func DefaultProjectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("claude-local: resolve home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// Options are the ceiling store settings of one fetch.
type Options struct {
	// StateDir holds the ceiling store; empty keeps no store, so a ceiling
	// lives only as long as its rejection stays in the scanned logs.
	StateDir string
	// Account names the store file; empty is DefaultAccount.
	Account string
	// MaxCeilingAge is how old a ceiling's rejection may be; zero is DefaultMaxCeilingAge.
	MaxCeilingAge time.Duration
}

// Fetch reads every session file under projectsDir written within the
// weekly window and returns one record per usage window: five_hour and
// seven_day always, plus every other rateLimitType a rejection names. A
// missing directory or one without session files is a skip; a corrupt
// ceiling store is a failure.
func Fetch(ctx context.Context, projectsDir string, now time.Time, opts Options) Result {
	logs, path, err := openLogScan(now, opts)
	if err != nil {
		return Result{Status: catalog.StatusFail, Detail: "claude-local: " + err.Error()}
	}
	found, err := sessionlog.WalkJSONL(ctx, projectsDir, maxEntriesWalked, func(file string, info fs.FileInfo) error {
		return logs.readFile(ctx, projectsDir, file, info)
	})
	if err != nil {
		return Result{Status: catalog.StatusFail, Detail: "claude-local: " + err.Error()}
	}
	if !found {
		return Result{Status: catalog.StatusSkip, Detail: fmt.Sprintf("no *.jsonl session files under %s", projectsDir)}
	}
	records := logs.records()
	if len(records) > MaxRecords {
		return Result{Status: catalog.StatusFail, Detail: fmt.Sprintf("claude-local: rejections name %d windows, more than %d", len(records), MaxRecords)}
	}
	detail := logs.detail()
	if logs.storeChanged {
		if err := saveStore(path, logs.store); err != nil {
			detail += "; ceiling store not written: " + err.Error()
		}
	}
	return Result{Records: records, Status: catalog.StatusOK, Count: len(records), Detail: detail}
}

// openLogScan prepares a scan and loads the account's ceiling store, if any.
func openLogScan(now time.Time, opts Options) (*logScan, string, error) {
	logs := newLogScan(now)
	if opts.MaxCeilingAge < 0 {
		return nil, "", fmt.Errorf("maximum ceiling age %s is negative", opts.MaxCeilingAge)
	}
	if opts.MaxCeilingAge > 0 {
		logs.maxAge = opts.MaxCeilingAge
	}
	if opts.StateDir == "" {
		return logs, "", nil
	}
	account := opts.Account
	if account == "" {
		account = DefaultAccount
	}
	if !accountName.MatchString(account) {
		return nil, "", fmt.Errorf("account %q must match %s", account, accountName)
	}
	path := storePath(opts.StateDir, account)
	store, err := loadStore(path, account)
	if err != nil {
		return nil, "", err
	}
	logs.store = store
	return logs, path, nil
}

// readFile scans one session file, unless it was last written before the weekly window.
func (s *logScan) readFile(ctx context.Context, root, path string, info fs.FileInfo) error {
	if info.ModTime().Before(s.now.Add(-weeklyWindow)) {
		return nil
	}
	if rel, err := filepath.Rel(root, path); err == nil {
		s.currentFile = filepath.ToSlash(rel)
	} else {
		s.currentFile = filepath.Base(path)
	}
	s.files++
	if s.files > maxFilesRead {
		return fmt.Errorf("more than %d session files written within %s", maxFilesRead, weeklyWindow)
	}
	skipped, err := sessionlog.ScanLines(ctx, path, scanLimits, s.line)
	s.skippedLines += skipped
	if err != nil {
		return err
	}
	if s.overflow != "" {
		return fmt.Errorf("%s", s.overflow)
	}
	return nil
}

func (s *logScan) detail() string {
	return fmt.Sprintf("%d files read, %d usage entries, %d rejections, %d overlong lines skipped",
		s.files, len(s.usage), len(s.rejections), s.skippedLines)
}
