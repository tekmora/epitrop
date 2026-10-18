package main

// Ported from cordanaLLM/praetor tribunus/cmd/tribunusctl/claude_limits.go at 5d4acfb (EUPL-1.2).

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/tekmora/epitrop/internal/sources/claudelocal"
)

// claudeLimitsTimeout bounds the whole read (HISS-02).
const claudeLimitsTimeout = 3 * time.Minute

// runClaudeLimits is the read-only "claude-limits" subcommand: it counts the
// rateLimitType and status values of the quotaLimits lines in every Claude
// Code session log and prints only those counts.
func runClaudeLimits(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("claude-limits", flag.ContinueOnError)
	dir := fs.String("claude-projects-dir", "", "Claude Code projects directory (default: ~/.claude/projects)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		resolved, err := claudelocal.DefaultProjectsDir()
		if err != nil {
			return fmt.Errorf("claude-limits: %w", err)
		}
		*dir = resolved
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudeLimitsTimeout)
	defer cancel()
	counts, err := claudelocal.CountLimitNames(ctx, *dir)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "files %d, quotaLimits lines %d, overlong lines skipped %d\n", counts.Files, counts.QuotaLines, counts.SkippedLong); err != nil {
		return err
	}
	if err := printCounts(out, "rateLimitType", counts.Types); err != nil {
		return err
	}
	return printCounts(out, "status", counts.Statuses)
}

func printCounts(out io.Writer, field string, counts map[string]int) error {
	for _, key := range claudelocal.SortedKeys(counts) {
		if _, err := fmt.Fprintf(out, "%s %s %d\n", field, key, counts[key]); err != nil {
			return err
		}
	}
	return nil
}
