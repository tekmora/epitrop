package codexlocal

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"time"

	gerrors "github.com/golusoris/golusoris/core/errors"

	"github.com/tekmora/epitrop/catalog"
)

const (
	// burnWindow is the sliding window before the read that the burn rate and
	// the active sessions are measured over.
	burnWindow = 15 * time.Minute
	// maxRecentSessions bounds how many session files written inside
	// burnWindow are read for samples (HISS-02). Past it both fields are
	// absent, never computed from a part of the sessions.
	maxRecentSessions = 64
)

const (
	absentBurnKey   = "usage_window.burn_percent_per_hour"
	absentAgentsKey = "usage_window.active_agents"
)

type burnSample struct {
	at   time.Time
	used float64
}

// burnTally keeps the earliest and the latest rate_limits sample of one
// window (same limit_id and resets_at) inside [from, to], across sessions.
type burnTally struct {
	from, to time.Time
	limitID  string
	resetsAt int64
	first    *burnSample
	last     *burnSample
	agents   int
}

// addBurn fills the burn rate and the active session count of rec's usage
// window from the session files scan found written inside burnWindow, and
// names each field it cannot measure in rec.Absent. A non-empty return is a
// read failure the caller reports as degraded.
func addBurn(ctx context.Context, rec *catalog.Record, scan *sessionScan, latest rateLimits, now time.Time) string {
	if scan.recentOverflow {
		reason := fmt.Sprintf("more than %d session files were written in the last %s", maxRecentSessions, burnWindow)
		rec.Absent[absentBurnKey], rec.Absent[absentAgentsKey] = reason, reason
		return ""
	}
	tally := &burnTally{from: now.Add(-burnWindow), to: now, limitID: latest.LimitID, resetsAt: *latest.Primary.ResetsAt}
	for _, p := range scan.recent {
		found, err := tally.readFile(ctx, p)
		if err != nil {
			reason := "a session file written in the burn window could not be read"
			rec.Absent[absentBurnKey], rec.Absent[absentAgentsKey] = reason, reason
			return err.Error()
		}
		if found {
			tally.agents++
		}
	}
	agents := tally.agents
	rec.UsageWindow.ActiveAgents = &agents
	burn, reason := tally.rate()
	if reason != "" {
		rec.Absent[absentBurnKey] = reason
		return ""
	}
	rec.UsageWindow.BurnPercentPerHour = &burn
	return ""
}

// readFile adds every sample of path that falls in the tally's window and
// reports whether there was at least one.
func (b *burnTally) readFile(ctx context.Context, path string) (found bool, err error) {
	f, err := openRegularSessionFile(path)
	if err != nil {
		return false, err
	}
	defer gerrors.CloseJoin(f, &err, "codex-local: close "+path)

	scanner := bufio.NewScanner(io.LimitReader(f, maxSessionFileBytes+1))
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	lines := 0
	for scanner.Scan() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		lines++
		if lines > maxScanLines {
			return false, fmt.Errorf("codex-local: %s exceeds %d lines scanned", path, maxScanLines)
		}
		hit, lineErr := b.addLine(scanner.Bytes())
		if lineErr != nil {
			return false, fmt.Errorf("codex-local: scan %s line %d: %w", path, lines, lineErr)
		}
		found = found || hit
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return false, fmt.Errorf("codex-local: scan %s: %w", path, scanErr)
	}
	return found, nil
}

// addLine adds the line's sample when it reports the tally's window inside
// [from, to]. Both ends count: a sample exactly burnWindow old is inside.
func (b *burnTally) addLine(raw []byte) (bool, error) {
	line, err := decodeRateLimitsLine(raw)
	if line == nil || err != nil {
		return false, err
	}
	rl := line.Payload.RateLimits
	if rl.LimitID != b.limitID || *rl.Primary.ResetsAt != b.resetsAt {
		return false, nil
	}
	at, err := time.Parse(time.RFC3339Nano, line.Timestamp)
	if err != nil {
		return false, fmt.Errorf("rate_limits line timestamp %q: %w", line.Timestamp, err)
	}
	if at.Before(b.from) || at.After(b.to) {
		return false, nil
	}
	b.add(burnSample{at: at, used: *rl.Primary.UsedPercent})
	return true, nil
}

func (b *burnTally) add(s burnSample) {
	if b.first == nil || s.at.Before(b.first.at) {
		first := s
		b.first = &first
	}
	if b.last == nil || s.at.After(b.last.at) {
		last := s
		b.last = &last
	}
}

// rate is the rise of used_percent per hour between the earliest and the
// latest sample, or the reason it cannot be measured.
func (b *burnTally) rate() (float64, string) {
	if b.first == nil || !b.last.at.After(b.first.at) {
		return 0, fmt.Sprintf("fewer than two rate_limits samples at distinct times in the last %s", burnWindow)
	}
	delta := b.last.used - b.first.used
	if delta < 0 {
		return 0, "used_percent fell inside the burn window"
	}
	return delta / b.last.at.Sub(b.first.at).Hours(), ""
}
