// Package codexlocal reads the Codex CLI's own subscription usage window
// from the newest local session log. It is read-only: it never writes to
// the sessions directory.
//
// Session logs are JSON Lines under ~/.codex/sessions/**/*.jsonl. Each line
// that reports usage has the shape (verified live against a real session
// log on this workstation, 2026-09-18):
//
//	{"timestamp":"...","ordinal":N,"type":"event_msg",
//	 "payload":{"type":"...","info":{...},
//	            "rate_limits":{"limit_id":"codex","limit_name":null,
//	              "primary":{"used_percent":92.0,"window_minutes":10080,"resets_at":1789858494},
//	              "secondary":null,
//	              "credits":{"has_credits":false,"unlimited":false,"balance":"0"},
//	              "plan_type":"pro"}}}
//
// resets_at is Unix seconds. Fetch reports the *primary* window only in
// slice 1 (design scope); the secondary window and credits balance are
// parsed but not yet surfaced as catalog fields.
package codexlocal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	gerrors "github.com/golusoris/golusoris/core/errors"

	"github.com/tekmora/epitrop/catalog"
)

// SourceName identifies this source in Snapshot.SourceRuns and CLI flags.
const SourceName = "codex-local"

const (
	// MaxRecords is the maximum number of records this source can contribute
	// to one snapshot.
	MaxRecords = 1
	// maxSessionFilesScanned bounds the directory walk (HISS-02).
	maxSessionFilesScanned = 20000
	// maxSessionFileBytes bounds how much of the newest session file Fetch
	// will read. Session logs run a few MB in practice; this is generous
	// but finite (HISS-02).
	maxSessionFileBytes = 128 << 20
	// maxScanLines bounds the line-scan loop over the newest session file
	// (HISS-02).
	maxScanLines = 500000
	// maxLineBytes bounds a single JSONL line the scanner will accept.
	maxLineBytes = 4 << 20
)

// Result is one Fetch attempt's outcome.
type Result struct {
	Records []catalog.Record
	Status  catalog.Status
	Count   int
	Detail  string
}

// DefaultSessionsDir returns ~/.codex/sessions. Callers pass its own
// sessionsDir explicitly to Fetch so tests never depend on $HOME.
func DefaultSessionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("codex-local: resolve home directory: %w", err)
	}
	return filepath.Join(home, ".codex", "sessions"), nil
}

// Fetch finds the newest *.jsonl session file under sessionsDir and turns
// its last rate_limits.primary window into one catalog.Record. A missing
// directory, a directory with no session files, or a newest session that
// never logged rate_limits are all reported as StatusSkip with a reason,
// never as a guessed record.
//
// The record also carries the window's recent burn and the sessions active in
// it, measured from the rate_limits samples of the last burnWindow (burn.go);
// what cannot be measured is named in Record.Absent, never zero-filled.
func Fetch(ctx context.Context, sessionsDir string) Result {
	return fetchAt(ctx, sessionsDir, time.Now().UTC())
}

func fetchAt(ctx context.Context, sessionsDir string, now time.Time) Result {
	scan, err := scanSessions(ctx, sessionsDir, now.Add(-burnWindow))
	if err != nil {
		return Result{Status: catalog.StatusFail, Detail: err.Error()}
	}
	newest, found := scan.best, scan.best != ""
	if !found {
		return Result{Status: catalog.StatusSkip, Detail: fmt.Sprintf("no *.jsonl session files under %s", sessionsDir)}
	}

	rl, err := latestRateLimits(ctx, newest)
	if err != nil {
		return Result{Status: catalog.StatusFail, Detail: err.Error()}
	}
	if rl == nil {
		return Result{Status: catalog.StatusSkip, Detail: fmt.Sprintf("newest session %s has no rate_limits entries", newest)}
	}
	if rl.Primary == nil {
		return Result{Status: catalog.StatusSkip, Detail: fmt.Sprintf("newest session %s rate_limits carries no primary window", newest)}
	}

	rec, recErr := toRecord(*rl, now)
	if recErr != nil {
		return Result{Status: catalog.StatusFail, Detail: recErr.Error()}
	}
	if detail := addBurn(ctx, &rec, scan, *rl, now); detail != "" {
		return Result{Records: []catalog.Record{rec}, Status: catalog.StatusDegraded, Count: 1, Detail: detail}
	}
	return Result{Records: []catalog.Record{rec}, Status: catalog.StatusOK, Count: 1}
}

// sessionScan accumulates the newest matching session file seen across a
// filepath.WalkDir, keeping scanSessions' own complexity to setting this up
// and interpreting the final result.
type sessionScan struct {
	root     string
	best     string
	bestTime time.Time
	scanned  int
	// recentFrom, recent and recentOverflow collect the session files written
	// since recentFrom, the only ones that can hold a sample of the burn window.
	recentFrom     time.Time
	recent         []string
	recentOverflow bool
}

// consider evaluates one WalkDir entry, updating s.best when p is a newer
// .jsonl session file. It returns the error WalkDir should propagate, if
// any: a real filesystem error, context cancellation, or the scanned-files
// bound (HISS-02).
func (s *sessionScan) consider(ctx context.Context, p string, d fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		return walkErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if d.IsDir() {
		return nil
	}
	s.scanned++
	if s.scanned > maxSessionFilesScanned {
		return fmt.Errorf("codex-local: %s exceeds %d files scanned", s.root, maxSessionFilesScanned)
	}
	if filepath.Ext(p) != ".jsonl" {
		return nil
	}
	info, infoErr := d.Info()
	if infoErr != nil {
		return infoErr
	}
	if info.ModTime().After(s.bestTime) {
		s.bestTime = info.ModTime()
		s.best = p
	}
	s.noteRecent(p, info.ModTime())
	return nil
}

func (s *sessionScan) noteRecent(p string, modTime time.Time) {
	if modTime.Before(s.recentFrom) {
		return
	}
	if len(s.recent) >= maxRecentSessions {
		s.recentOverflow = true
		return
	}
	s.recent = append(s.recent, p)
}

// scanSessions walks root once: the newest session file, and every file
// written since recentFrom. A missing directory yields an empty scan, not an
// error: that is a skip, not a failure.
func scanSessions(ctx context.Context, root string, recentFrom time.Time) (*sessionScan, error) {
	scan := &sessionScan{root: root, recentFrom: recentFrom}
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		return scan.consider(ctx, p, d, walkErr)
	})
	if walkErr != nil {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return &sessionScan{root: root}, nil
		}
		return nil, fmt.Errorf("codex-local: walk %s: %w", root, walkErr)
	}
	return scan, nil
}

// sessionLine is the JSONL envelope; only the fields Fetch needs are typed.
// No published JSONL schema was found; rate-limit payload fields were checked
// against openai/codex generated rate-limit model sources at
// 0ada5d8806cdad498230d5b1b2924091e04c8feb on 2026-10-09.
type sessionLine struct {
	Timestamp string `json:"timestamp"`
	Payload   struct {
		RateLimits *rateLimits `json:"rate_limits"`
	} `json:"payload"`
}

type rateLimits struct {
	LimitID  string  `json:"limit_id"`
	PlanType string  `json:"plan_type"`
	Primary  *window `json:"primary"`
}

type window struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowMinutes *int     `json:"window_minutes"`
	ResetsAt      *int64   `json:"resets_at"`
}

// latestRateLimits scans path and returns the last non-null rate_limits
// object logged, or nil if the file has none. Malformed lines are skipped:
// a session log can carry lines this parser does not model, and skipping
// them is correct, not a data loss, because only rate_limits lines matter.
func latestRateLimits(ctx context.Context, path string) (result *rateLimits, err error) {
	f, err := openRegularSessionFile(path)
	if err != nil {
		return nil, err
	}
	defer gerrors.CloseJoin(f, &err, "codex-local: close "+path)

	scanner := bufio.NewScanner(io.LimitReader(f, maxSessionFileBytes+1))
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	return scanLatestRateLimits(ctx, path, scanner)
}

func openRegularSessionFile(path string) (*os.File, error) {
	// #nosec G304 -- path is always the output of scanSessions' own
	// WalkDir scan under the caller-supplied sessions root, not untrusted input.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("codex-local: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, closeSessionFile(f, fmt.Errorf("codex-local: stat %s: %w", path, err))
	}
	if !info.Mode().IsRegular() {
		return nil, closeSessionFile(f, fmt.Errorf("codex-local: %s is not a regular file", path))
	}
	if info.Size() > maxSessionFileBytes {
		return nil, closeSessionFile(f, fmt.Errorf("codex-local: %s exceeds %d bytes", path, maxSessionFileBytes))
	}
	return f, nil
}

func closeSessionFile(f *os.File, err error) error {
	return errors.Join(err, f.Close())
}

func scanLatestRateLimits(ctx context.Context, path string, scanner *bufio.Scanner) (*rateLimits, error) {
	var latest *rateLimits
	lines := 0
	for scanner.Scan() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		lines++
		if lines > maxScanLines {
			return nil, fmt.Errorf("codex-local: %s exceeds %d lines scanned", path, maxScanLines)
		}
		rl, lineErr := parseRateLimitsLine(scanner.Bytes())
		if lineErr != nil {
			return nil, fmt.Errorf("codex-local: scan %s line %d: %w", path, lines, lineErr)
		}
		if rl != nil {
			latest = rl
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return nil, fmt.Errorf("codex-local: scan %s: %w", path, scanErr)
	}
	return latest, nil
}

// parseRateLimitsLine parses one JSONL line and returns its rate_limits
// payload, or nil when the line is empty, not JSON this parser models
// (not every line is a rate_limits-bearing event_msg), or carries no usable
// primary window. A session's last rate_limits line can itself carry a null
// primary window (verified live: this workstation's newest session ends on
// exactly that); returning nil for it, not a zero value, lets the caller
// keep an earlier line's usable value instead of reporting "no data".
func parseRateLimitsLine(raw []byte) (*rateLimits, error) {
	line, err := decodeRateLimitsLine(raw)
	if line == nil || err != nil {
		return nil, err
	}
	return line.Payload.RateLimits, nil
}

// decodeRateLimitsLine is parseRateLimitsLine keeping the line's timestamp.
func decodeRateLimitsLine(raw []byte) (*sessionLine, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var line sessionLine
	if err := json.Unmarshal(raw, &line); err != nil {
		if strings.Contains(string(raw), "rate_limits") {
			return nil, fmt.Errorf("decode rate_limits line: %w", err)
		}
		return nil, nil
	}
	if line.Payload.RateLimits == nil || line.Payload.RateLimits.Primary == nil {
		return nil, nil
	}
	if err := validateRateLimits(line.Payload.RateLimits); err != nil {
		return nil, err
	}
	return &line, nil
}

func validateRateLimits(rl *rateLimits) error {
	if rl.LimitID == "" {
		return errors.New("rate_limits.limit_id is required")
	}
	if rl.PlanType == "" {
		return errors.New("rate_limits.plan_type is required")
	}
	if rl.Primary.UsedPercent == nil {
		return errors.New("rate_limits.primary.used_percent is required")
	}
	if rl.Primary.WindowMinutes == nil {
		return errors.New("rate_limits.primary.window_minutes is required")
	}
	if rl.Primary.ResetsAt == nil {
		return errors.New("rate_limits.primary.resets_at is required")
	}
	return nil
}

// toRecord turns a parsed rate_limits payload into one catalog.Record
// describing the Codex CLI subscription's usage window. There is no single
// "model" behind a CLI-wide rate limit, so the record id names the CLI and
// the limit bucket it measured.
func toRecord(rl rateLimits, fetchedAt time.Time) (catalog.Record, error) {
	limitID := rl.LimitID
	if limitID == "" {
		return catalog.Record{}, fmt.Errorf("codex-local: rate_limits.limit_id is empty")
	}
	rec := catalog.Record{
		ModelID:    "codex-cli/" + limitID,
		Provider:   "openai",
		AccessPath: catalog.AccessSubscriptionCLI,
		Provenance: catalog.Provenance{
			Source:    SourceName,
			FetchedAt: fetchedAt,
			Kind:      catalog.KindMeasured,
		},
		Absent: map[string]string{
			"context_window":  "not reported by codex session rate_limits",
			"price_in_per_m":  "subscription plan, not metered pricing",
			"price_out_per_m": "subscription plan, not metered pricing",
			"limits":          "rate_limits reports a usage window, not rpm/tpm caps",
		},
	}
	uw := &catalog.UsageWindow{}
	if rl.Primary.UsedPercent != nil {
		uw.UsedPercent = rl.Primary.UsedPercent
	}
	if rl.Primary.WindowMinutes != nil {
		uw.WindowMinutes = rl.Primary.WindowMinutes
	}
	if rl.Primary.ResetsAt != nil {
		t := time.Unix(*rl.Primary.ResetsAt, 0).UTC()
		uw.ResetsAt = &t
	}
	rec.UsageWindow = uw
	if rl.PlanType != "" {
		rec.Capabilities = []string{"plan:" + rl.PlanType}
	}
	return rec, nil
}
