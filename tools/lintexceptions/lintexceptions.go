// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

// Package lintexceptions checks the dated exceptions in .golangci.yml: every rule under
// linters.exclusions.rules carries the comment "category <A-D>, expires <YYYY-MM-DD>, #<issue>"
// on the line above it, and none is past its date.
//
// This check is itself an exception (category C, renewed 2026-10-21 for the 10 days of that
// category, expires 2026-10-31, #133): it stands in until the pinned engine's audit reads per-file
// linter exceptions with an expiry, and goes in the change that adopts that audit.
package lintexceptions

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// selfExpiry is the date this package's own exception runs out.
const selfExpiry = "2026-10-31"

// maxLines bounds the configuration lines one check reads.
const maxLines = 100000

var (
	commentLine = regexp.MustCompile(`^      # category [A-D], expires (\d{4}-\d{2}-\d{2}), #\d+$`)
	pathLine    = regexp.MustCompile(`^      - path: \^\S+\$$`)
	lintersLine = regexp.MustCompile(`^        linters: \[(gocyclo|gocognit)\]$`)
)

// Check reads the configuration text and returns the number of rules it found, or an error
// naming the first line without its exception comment, the first expired rule, or a line the
// reader does not allow inside the rules block.
func Check(config string, today time.Time) (int, error) {
	// A Windows checkout ends lines with CRLF; the rules read LF lines.
	lines := strings.Split(strings.ReplaceAll(config, "\r\n", "\n"), "\n")
	start, err := rulesStart(lines)
	if err != nil {
		return 0, err
	}
	rules := 0
	expiry := ""
	for i := start; i < len(lines) && i < maxLines; i++ {
		line := lines[i]
		if line != "" && !strings.HasPrefix(line, "      ") {
			break
		}
		kind, date := classify(line)
		switch kind {
		case "comment":
			expiry = date
		case "path":
			if err := checkRule(expiry, today, i+1); err != nil {
				return rules, err
			}
			rules++
			expiry = ""
		case "other":
			return rules, fmt.Errorf("line %d: %q is not an exception comment, path or linters line", i+1, line)
		}
	}
	return rules, nil
}

// rulesStart returns the index of the first line after "    rules:" inside "  exclusions:".
// An empty list, "    rules: []", holds no rules; the line after it ends the block.
func rulesStart(lines []string) (int, error) {
	inExclusions := false
	for i := 0; i < len(lines) && i < maxLines; i++ {
		switch {
		case lines[i] == "  exclusions:":
			inExclusions = true
		case inExclusions && (lines[i] == "    rules:" || lines[i] == "    rules: []"):
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("no linters.exclusions.rules block")
}

// classify names the kind of one line in the rules block and, for a comment, its expiry.
func classify(line string) (string, string) {
	if match := commentLine.FindStringSubmatch(line); match != nil {
		return "comment", match[1]
	}
	if pathLine.MatchString(line) {
		return "path", ""
	}
	if line == "" || lintersLine.MatchString(line) {
		return "linters", ""
	}
	return "other", ""
}

// checkRule refuses a rule whose comment is missing or whose expiry is before today.
func checkRule(expiry string, today time.Time, lineNumber int) error {
	if expiry == "" {
		return fmt.Errorf("line %d: rule has no \"category <A-D>, expires <date>, #<issue>\" comment above it", lineNumber)
	}
	expired, err := Expired(expiry, today)
	if err != nil {
		return fmt.Errorf("line %d: %w", lineNumber, err)
	}
	if expired {
		return fmt.Errorf("line %d: exception expired on %s", lineNumber, expiry)
	}
	return nil
}

// Expired reports whether the date has passed: an exception holds through its expiry day.
func Expired(expiry string, today time.Time) (bool, error) {
	date, err := time.Parse(time.DateOnly, expiry)
	if err != nil {
		return false, fmt.Errorf("expiry %q: %w", expiry, err)
	}
	day := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	return day.After(date), nil
}
