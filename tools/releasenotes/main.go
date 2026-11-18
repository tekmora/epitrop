// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

// Command releasenotes writes one version's section of CHANGELOG.md to a file. The release
// workflow gives that file to GoReleaser as the release notes, so a release page shows the
// text that was merged as the changelog and not a list of commit subjects.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const (
	maxChangelogBytes = 4 << 20
	maxChangelogLines = 100000
	maxLineURLs       = 256
	sectionPrefix     = "## "
	urlEndChars       = " \t\r\n\v\f)\"'>"
)

// releaseVersion is a version as a changelog heading carries it: no leading "v", an
// optional prerelease part. "Unreleased" is not one, so that section is never published.
var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "releasenotes:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("releasenotes", flag.ContinueOnError)
	changelog := flags.String("changelog", "CHANGELOG.md", "changelog file to read")
	version := flags.String("version", "", "version or tag whose section is wanted, such as v0.1.0")
	out := flags.String("out", "", "file to write the section to; it must not exist")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *out == "" {
		return errors.New("-out is required")
	}
	text, err := readBounded(*changelog)
	if err != nil {
		return err
	}
	notes, err := section(text, *version)
	if err != nil {
		return fmt.Errorf("%s: %w", *changelog, err)
	}
	return writeNew(*out, notes)
}

func readBounded(path string) (string, error) {
	// #nosec G304 -- the path is the -changelog flag the caller supplies directly.
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open changelog: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxChangelogBytes+1))
	if closeErr := file.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("read changelog %s: %w", path, err)
	}
	if len(body) > maxChangelogBytes {
		return "", fmt.Errorf("read changelog %s: larger than %d bytes", path, maxChangelogBytes)
	}
	return string(body), nil
}

// writeNew refuses a file that exists: the notes of one release never replace another's.
func writeNew(path string, notes string) error {
	// #nosec G302 G304 -- the path is the -out flag the caller supplies directly; release notes are public text.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create notes file: %w", err)
	}
	_, err = file.WriteString(notes)
	if closeErr := file.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write notes file %s: %w", path, err)
	}
	return nil
}

// section returns the body of the one "## [<version>]" section of a changelog, without its
// heading and without blank lines around it. A version that has no section, more than one
// section or a section without text is an error: a release is not published with notes
// that are missing or belong to another version. A section ends at the next line that
// starts a second-level heading.
func section(changelog string, version string) (string, error) {
	version = strings.TrimPrefix(version, "v")
	if !releaseVersion.MatchString(version) {
		return "", fmt.Errorf("version %q is not of the form 1.2.3 or 1.2.3-rc.1", version)
	}
	lines, err := splitBoundedLines(changelog)
	if err != nil {
		return "", err
	}
	start, err := headingLine(lines, version)
	if err != nil {
		return "", err
	}
	body := trimBlankLines(lines[start+1 : sectionEnd(lines, start+1)])
	if len(body) == 0 {
		return "", fmt.Errorf("the section of %s holds no text", version)
	}
	return strings.Join(body, "\n") + "\n", nil
}

func splitBoundedLines(text string) ([]string, error) {
	lines := strings.Split(text, "\n")
	if len(lines) > maxChangelogLines {
		return nil, fmt.Errorf("more than %d lines", maxChangelogLines)
	}
	return lines, nil
}

// latestReleased returns the version of the newest "## [X.Y.Z...]" heading in a
// changelog that is not "Unreleased".
func latestReleased(changelog string) (string, error) {
	lines, err := splitBoundedLines(changelog)
	if err != nil {
		return "", err
	}
	for i := 0; i < len(lines) && i < maxChangelogLines; i++ {
		ver, ok := headingVersion(lines[i])
		if !ok || ver == "Unreleased" {
			continue
		}
		if !releaseVersion.MatchString(ver) {
			return "", fmt.Errorf("version %q at line %d is not of the form 1.2.3 or 1.2.3-rc.1", ver, i+1)
		}
		return ver, nil
	}
	return "", errors.New("no released version in changelog")
}

// readmeReleaseBadge returns the version of the one label=release badge's
// message=v<version> query parameter in a README.
func readmeReleaseBadge(readme string) (string, error) {
	lines, err := splitBoundedLines(readme)
	if err != nil {
		return "", err
	}
	found := -1
	version := ""
	for i := 0; i < len(lines) && i < maxChangelogLines; i++ {
		ver, ok, err := badgeMessageVersion(lines[i])
		if err != nil {
			return "", fmt.Errorf("line %d: %w", i+1, err)
		}
		if !ok {
			continue
		}
		if found >= 0 {
			return "", fmt.Errorf("label=release badge at line %d and at line %d", found+1, i+1)
		}
		found = i
		version = ver
	}
	if found < 0 {
		return "", errors.New("no label=release badge")
	}
	return version, nil
}

func badgeMessageVersion(line string) (string, bool, error) {
	found := false
	message := ""
	rest := line
	for n := 0; n < maxLineURLs && rest != ""; n++ {
		raw, tail, ok := nextURLToken(rest)
		if !ok {
			break
		}
		rest = tail
		msg, isRelease := releaseBadgeMessage(raw)
		if !isRelease {
			continue
		}
		if found {
			return "", false, errors.New("more than one label=release badge on one line")
		}
		found = true
		message = msg
	}
	if !found {
		return "", false, nil
	}
	ver, ok := strings.CutPrefix(message, "v")
	if !ok {
		return "", false, errors.New("label=release badge has no message=v<version>")
	}
	if !releaseVersion.MatchString(ver) {
		return "", false, fmt.Errorf("badge version %q is not of the form 1.2.3 or 1.2.3-rc.1", ver)
	}
	return ver, true, nil
}

func releaseBadgeMessage(rawURL string) (string, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	query := parsed.Query()
	if query.Get("label") != "release" {
		return "", false
	}
	return query.Get("message"), true
}

func nextURLToken(s string) (string, string, bool) {
	start := nextURLStart(s)
	if start < 0 {
		return "", "", false
	}
	sub := s[start:]
	if end := strings.IndexAny(sub, urlEndChars); end >= 0 {
		return sub[:end], sub[end:], true
	}
	return sub, "", true
}

func nextURLStart(s string) int {
	httpIdx := strings.Index(s, "http://")
	httpsIdx := strings.Index(s, "https://")
	if httpIdx < 0 {
		return httpsIdx
	}
	if httpsIdx >= 0 && httpsIdx < httpIdx {
		return httpsIdx
	}
	return httpIdx
}

func headingLine(lines []string, version string) (int, error) {
	found := -1
	for i := 0; i < len(lines) && i < maxChangelogLines; i++ {
		if !isHeadingOf(lines[i], version) {
			continue
		}
		if found >= 0 {
			return 0, fmt.Errorf("version %s has a section at line %d and at line %d", version, found+1, i+1)
		}
		found = i
	}
	if found < 0 {
		return 0, fmt.Errorf("no section for version %s", version)
	}
	return found, nil
}

// headingVersion returns the bracketed name of a "## [<name>]" line when the closing
// bracket stands at the end of the line or before a space, as in "## [1.2.3] - 2026-10-11".
func headingVersion(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, sectionPrefix+"[")
	if !ok {
		return "", false
	}
	version, after, ok := strings.Cut(rest, "]")
	if !ok || version == "" || (after != "" && !strings.HasPrefix(after, " ")) {
		return "", false
	}
	return version, true
}

// isHeadingOf matches "## [1.2.3]" alone or followed by a space, as in "## [1.2.3] - 2026-10-11".
// The closing bracket keeps 1.2.3 from matching the heading of 1.2.3-rc.1.
func isHeadingOf(line string, version string) bool {
	got, ok := headingVersion(line)
	return ok && got == version
}

func sectionEnd(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], sectionPrefix) {
			return i
		}
	}
	return len(lines)
}

func trimBlankLines(lines []string) []string {
	first, last := 0, len(lines)
	for first < last && strings.TrimSpace(lines[first]) == "" {
		first++
	}
	for last > first && strings.TrimSpace(lines[last-1]) == "" {
		last--
	}
	return lines[first:last]
}
