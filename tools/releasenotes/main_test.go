// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const sampleChangelog = `# Changelog

Introductory text.

## [Unreleased]


## [0.2.0] - 2026-11-01

### Added

- Second thing (#2)

### Fixed

- A fix (#3)


## [0.2.0-rc.1] - 2026-10-20

- Candidate thing

## [0.1.0] - 2026-10-12

- First thing (#1)
`

func TestSectionReturnsTheBodyOfOneVersion(t *testing.T) {
	cases := []struct {
		name    string
		version string
		want    string
	}{
		{name: "first released section", version: "0.2.0", want: "### Added\n\n- Second thing (#2)\n\n### Fixed\n\n- A fix (#3)\n"},
		{name: "with the tag's v", version: "v0.2.0", want: "### Added\n\n- Second thing (#2)\n\n### Fixed\n\n- A fix (#3)\n"},
		{name: "prerelease between two sections", version: "v0.2.0-rc.1", want: "- Candidate thing\n"},
		{name: "last section, ended by the file", version: "0.1.0", want: "- First thing (#1)\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := section(sampleChangelog, tc.version)
			if err != nil {
				t.Fatalf("section(%q) = %v, want nil", tc.version, err)
			}
			if got != tc.want {
				t.Fatalf("section(%q) = %q, want %q", tc.version, got, tc.want)
			}
		})
	}
}

func TestSectionDropsLinesOfWhitespaceAroundTheBody(t *testing.T) {
	changelog := "## [0.3.0]\n \t\n\n- x\n\n  - y\n\t\n \n## [0.2.0]\n\n- z\n"
	got, err := section(changelog, "0.3.0")
	if err != nil {
		t.Fatalf("section = %v, want nil", err)
	}
	if want := "- x\n\n  - y\n"; got != want {
		t.Fatalf("section = %q, want %q", got, want)
	}
}

func TestSectionRefusesWhatIsNoSectionOfThatVersion(t *testing.T) {
	cases := []struct {
		name      string
		changelog string
		version   string
		want      string
	}{
		{name: "no section", changelog: sampleChangelog, version: "0.3.0", want: "no section for version 0.3.0"},
		{name: "only its candidate has one", changelog: "## [0.3.0-rc.1] - 2026-12-01\n\n- x\n", version: "0.3.0", want: "no section for version 0.3.0"},
		{name: "only the release has one", changelog: "## [0.3.0] - 2026-12-01\n\n- x\n", version: "0.3.0-rc.1", want: "no section for version 0.3.0-rc.1"},
		{name: "a longer version that starts the same", changelog: "## [0.3.01] - 2026-12-01\n\n- x\n", version: "0.3.0", want: "no section for version 0.3.0"},
		{name: "heading of another level", changelog: "### [0.3.0]\n\n- x\n# [0.3.0]\n", version: "0.3.0", want: "no section for version 0.3.0"},
		{name: "heading not at the line start", changelog: " ## [0.3.0]\n\n- x\n", version: "0.3.0", want: "no section for version 0.3.0"},
		{name: "text right after the bracket", changelog: "## [0.3.0]x\n\n- x\n", version: "0.3.0", want: "no section for version 0.3.0"},
		{name: "two sections", changelog: "## [0.3.0]\n\n- x\n\n## [0.2.0]\n\n- y\n\n## [0.3.0] - again\n\n- z\n", version: "0.3.0", want: "a section at line 1 and at line 9"},
		{name: "empty section before the next", changelog: "## [0.3.0] - 2026-12-01\n\n \n## [0.2.0]\n\n- y\n", version: "0.3.0", want: "holds no text"},
		{name: "empty section at the end", changelog: "## [0.3.0] - 2026-12-01\n", version: "0.3.0", want: "holds no text"},
		{name: "the unreleased section", changelog: sampleChangelog, version: "Unreleased", want: "is not of the form"},
		{name: "no version", changelog: sampleChangelog, version: "", want: "is not of the form"},
		{name: "only the v", changelog: sampleChangelog, version: "v", want: "is not of the form"},
		{name: "a pattern as version", changelog: sampleChangelog, version: "0.2.0] - 2026-11-01\n", want: "is not of the form"},
		{name: "two parts", changelog: sampleChangelog, version: "0.2", want: "is not of the form"},
		{name: "text before the version", changelog: "## [x0.2.0]\n\n- x\n", version: "x0.2.0", want: "is not of the form"},
		{name: "text after the version", changelog: "## [0.2.0 ]\n\n- x\n", version: "0.2.0 ", want: "is not of the form"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := section(tc.changelog, tc.version)
			if err == nil {
				t.Fatalf("section(%q) = %q, want an error", tc.version, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("section(%q) = %v, want an error naming %q", tc.version, err, tc.want)
			}
		})
	}
}

func TestSectionBoundsTheChangelog(t *testing.T) {
	heading := "## [0.3.0]\n- x\n"
	atLimit := heading + strings.Repeat("\n", maxChangelogLines-3)
	if _, err := section(atLimit, "0.3.0"); err != nil {
		t.Fatalf("section(%d lines) = %v, want nil", maxChangelogLines, err)
	}
	over := atLimit + "\n"
	if _, err := section(over, "0.3.0"); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("section(%d lines) = %v, want a line limit error", maxChangelogLines+1, err)
	}
}

// The repository's own changelog is what the release workflow reads. Every released
// section in it must come out, so a change of the rendered format shows here and not on
// the day of a release.
func TestEveryReleasedSectionOfTheChangelogExtracts(t *testing.T) {
	path := filepath.Join("..", "..", "CHANGELOG.md")
	text, err := readBounded(path)
	if err != nil {
		t.Fatalf("readBounded(%s) = %v", path, err)
	}
	headings := regexp.MustCompile(`(?m)^## \[([^\]]+)\]`).FindAllStringSubmatch(text, -1)
	released := 0
	for i := 0; i < len(headings); i++ {
		version := headings[i][1]
		if version == "Unreleased" {
			continue
		}
		released++
		notes, err := section(text, "v"+version)
		if err != nil {
			t.Errorf("section(%s) = %v, want nil", version, err)
			continue
		}
		if strings.Contains(notes, "\n## ") || strings.HasPrefix(notes, "## ") {
			t.Errorf("section(%s) holds a second-level heading:\n%s", version, notes)
		}
	}
	if released == 0 {
		t.Fatalf("%s names no released version; the heading pattern of this test matched nothing", path)
	}
}

func TestRunWritesTheSectionToANewFile(t *testing.T) {
	dir := t.TempDir()
	changelog := filepath.Join(dir, "CHANGELOG.md")
	if err := os.WriteFile(changelog, []byte(sampleChangelog), 0o600); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	out := filepath.Join(dir, "notes.md")
	if err := run([]string{"-changelog=" + changelog, "-version=v0.1.0", "-out=" + out}); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile = %v", err)
	}
	if string(got) != "- First thing (#1)\n" {
		t.Fatalf("notes = %q, want the 0.1.0 section", got)
	}
	// A second run must not replace the notes that are there.
	err = run([]string{"-changelog=" + changelog, "-version=v0.2.0", "-out=" + out})
	if err == nil || !strings.Contains(err.Error(), "create notes file") {
		t.Fatalf("run onto an existing file = %v, want a create error", err)
	}
	again, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile after the refused run = %v", err)
	}
	if string(again) != string(got) {
		t.Fatalf("notes changed to %q", again)
	}
}

func TestRunRefusesAnIncompleteCall(t *testing.T) {
	dir := t.TempDir()
	changelog := filepath.Join(dir, "CHANGELOG.md")
	if err := os.WriteFile(changelog, []byte(sampleChangelog), 0o600); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	out := filepath.Join(dir, "notes.md")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "no out", args: []string{"-changelog=" + changelog, "-version=v0.1.0"}, want: "-out is required"},
		{name: "no version", args: []string{"-changelog=" + changelog, "-out=" + out}, want: "is not of the form"},
		{name: "a version without a section", args: []string{"-changelog=" + changelog, "-version=v9.9.9", "-out=" + out}, want: "no section for version 9.9.9"},
		{name: "no changelog", args: []string{"-changelog=" + filepath.Join(dir, "none.md"), "-version=v0.1.0", "-out=" + out}, want: "open changelog"},
		{name: "an argument left over", args: []string{"-changelog=" + changelog, "-version=v0.1.0", "-out=" + out, "extra"}, want: "unexpected argument"},
		{name: "an unknown flag", args: []string{"-tag=v0.1.0", "-out=" + out}, want: "parse flags"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := run(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run(%v) = %v, want an error naming %q", tc.args, err, tc.want)
			}
			if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
				t.Fatalf("a refused call left %s (%v)", out, statErr)
			}
		})
	}
}

func TestReadBoundedRefusesAnOversizedFile(t *testing.T) {
	dir := t.TempDir()
	atLimit := filepath.Join(dir, "at-limit.md")
	if err := os.WriteFile(atLimit, []byte(strings.Repeat("x", maxChangelogBytes)), 0o600); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	if _, err := readBounded(atLimit); err != nil {
		t.Fatalf("readBounded(%d bytes) = %v, want nil", maxChangelogBytes, err)
	}
	over := filepath.Join(dir, "over.md")
	if err := os.WriteFile(over, []byte(strings.Repeat("x", maxChangelogBytes+1)), 0o600); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	if _, err := readBounded(over); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("readBounded(%d bytes) = %v, want a size error", maxChangelogBytes+1, err)
	}
}

func TestLatestReleased(t *testing.T) {
	cases := []struct {
		name      string
		changelog string
		want      string
		wantErr   string
	}{
		{name: "newest release after Unreleased", changelog: sampleChangelog, want: "0.2.0"},
		{name: "prerelease heading as newest", changelog: "## [Unreleased]\n\n## [0.2.0-rc.1] - 2026-10-18\n\n- rc\n\n## [0.1.0] - 2026-10-11\n", want: "0.2.0-rc.1"},
		{name: "single release at boundary", changelog: "## [0.1.0]", want: "0.1.0"},
		{name: "Unreleased only", changelog: "# Changelog\n\n## [Unreleased]\n\n- pending\n", wantErr: "no released version in changelog"},
		{name: "empty input", changelog: "", wantErr: "no released version in changelog"},
		{name: "malformed heading version", changelog: "## [v0.2.0] - 2026-10-18\n", wantErr: "is not of the form"},
		{name: "over line limit", changelog: "## [0.2.0]\n" + strings.Repeat("\n", maxChangelogLines), wantErr: "more than"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := latestReleased(tc.changelog)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("latestReleased = (%q, %v), want error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("latestReleased = (%q, %v), want (%q, nil)", got, err, tc.want)
			}
		})
	}
}

func TestReadmeReleaseBadge(t *testing.T) {
	badge020 := "[![Release](https://img.shields.io/static/v1?label=release&message=v0.2.0&style=for-the-badge&color=3b82f6&labelColor=111a35)](https://github.com/tekmora/epitrop/releases)"
	badgeRC := "[![Release](https://img.shields.io/static/v1?label=release&message=v0.2.0-rc.1&style=for-the-badge&color=3b82f6&labelColor=111a35)](https://github.com/tekmora/epitrop/releases)"
	cases := []struct {
		name    string
		readme  string
		want    string
		wantErr string
	}{
		{name: "one release badge", readme: "# Title\n" + badge020 + "\n", want: "0.2.0"},
		{name: "prerelease badge at boundary", readme: badgeRC, want: "0.2.0-rc.1"},
		{name: "no badge", readme: "# Title\nNo release badge here.\n", wantErr: "no label=release badge"},
		{name: "empty input", readme: "", wantErr: "no label=release badge"},
		{name: "two badges on different lines", readme: badge020 + "\n" + badgeRC + "\n", wantErr: "label=release badge at line 1 and at line 2"},
		{name: "two badges on one line", readme: badge020 + " " + badgeRC, wantErr: "more than one label=release badge on one line"},
		{name: "badge without message=v", readme: "https://img.shields.io/static/v1?label=release&message=0.2.0", wantErr: "no message=v<version>"},
		{name: "badge with malformed version", readme: "https://img.shields.io/static/v1?label=release&message=v0.2&style=for-the-badge", wantErr: "is not of the form"},
		{name: "over line limit", readme: badge020 + strings.Repeat("\n", maxChangelogLines), wantErr: "more than"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readmeReleaseBadge(tc.readme)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("readmeReleaseBadge = (%q, %v), want error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("readmeReleaseBadge = (%q, %v), want (%q, nil)", got, err, tc.want)
			}
		})
	}
}

func checkReadmeBadgeMatchesChangelog(readmeText string, changelogText string) error {
	badge, err := readmeReleaseBadge(readmeText)
	if err != nil {
		return err
	}
	latest, err := latestReleased(changelogText)
	if err != nil {
		return err
	}
	if badge != latest {
		return &badgeMismatchError{badge: badge, latest: latest}
	}
	return nil
}

type badgeMismatchError struct {
	badge  string
	latest string
}

func (e *badgeMismatchError) Error() string {
	return "README.md release badge message=v" + e.badge + " does not match CHANGELOG.md latest release " + e.latest
}

func TestReadmeReleaseBadgeMatchesChangelog(t *testing.T) {
	readmePath := filepath.Join("..", "..", "README.md")
	readmeText, err := readBounded(readmePath)
	if err != nil {
		t.Fatalf("readBounded(%s) = %v", readmePath, err)
	}
	changelogPath := filepath.Join("..", "..", "CHANGELOG.md")
	changelogText, err := readBounded(changelogPath)
	if err != nil {
		t.Fatalf("readBounded(%s) = %v", changelogPath, err)
	}
	if err := checkReadmeBadgeMatchesChangelog(readmeText, changelogText); err != nil {
		t.Fatal(err)
	}
	badge, err := readmeReleaseBadge(readmeText)
	if err != nil {
		t.Fatalf("readmeReleaseBadge(%s) = %v", readmePath, err)
	}
	planted := strings.Replace(readmeText, "message=v"+badge, "message=v0.0.0", 1)
	if planted == readmeText {
		t.Fatalf("planted badge: message=v%s not found in %s", badge, readmePath)
	}
	if err := checkReadmeBadgeMatchesChangelog(planted, changelogText); err == nil || !strings.Contains(err.Error(), "message=v0.0.0 does not match CHANGELOG.md latest release "+badge) {
		t.Fatalf("planted v0.0.0 badge = %v, want mismatch against %s", err, badge)
	}
}
