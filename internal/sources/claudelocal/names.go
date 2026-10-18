package claudelocal

// Ported from cordanaLLM/praetor tribunus/internal/sources/claudelocal at 5d4acfb (EUPL-1.2).

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"sort"

	"github.com/tekmora/epitrop/internal/sources/sessionlog"
)

// LimitCounts is what CountLimitNames found: how often each rateLimitType
// and each quotaLimits status value occurs across the session logs.
type LimitCounts struct {
	Files       int
	QuotaLines  int
	Types       map[string]int
	Statuses    map[string]int
	SkippedLong int
}

// otherValue stands for a value that is not a plain identifier; such a value
// is counted, never printed, since only identifier-shaped values are known
// to be field enums rather than free text.
const otherValue = "<other>"

var enumValue = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// CountLimitNames reads every session file under projectsDir, whatever its
// age, and counts the rateLimitType and status values of quotaLimits lines.
// It decodes nothing else and keeps no value but these counts.
func CountLimitNames(ctx context.Context, projectsDir string) (LimitCounts, error) {
	counts := LimitCounts{Types: map[string]int{}, Statuses: map[string]int{}}
	_, err := sessionlog.WalkJSONL(ctx, projectsDir, maxEntriesWalked, func(path string, _ fs.FileInfo) error {
		counts.Files++
		if counts.Files > maxFilesRead {
			return fmt.Errorf("more than %d session files", maxFilesRead)
		}
		skipped, scanErr := sessionlog.ScanLines(ctx, path, scanLimits, counts.line)
		counts.SkippedLong += skipped
		return scanErr
	})
	if err != nil {
		return counts, fmt.Errorf("claude-local: %w", err)
	}
	return counts, nil
}

func (c *LimitCounts) line(raw []byte) {
	if !containsQuota(raw) {
		return
	}
	var entry struct {
		QuotaLimits *quotaLimits `json:"quotaLimits"`
	}
	if json.Unmarshal(raw, &entry) != nil || entry.QuotaLimits == nil {
		return
	}
	c.QuotaLines++
	c.Types[enumOrOther(entry.QuotaLimits.RateLimitType)]++
	c.Statuses[enumOrOther(entry.QuotaLimits.Status)]++
}

func enumOrOther(value string) string {
	if enumValue.MatchString(value) {
		return value
	}
	return otherValue
}

// SortedKeys returns a count map's keys, most frequent first, then by name.
func SortedKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}
