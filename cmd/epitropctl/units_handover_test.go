package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/slot"
)

// captureHandover runs `units handover` and returns what it printed.
func captureHandover(t *testing.T, args ...string) (string, error) {
	t.Helper()
	old := slotOut
	var out bytes.Buffer
	slotOut = &out
	defer func() { slotOut = old }()
	err := runUnits(append([]string{"handover"}, args...))
	return out.String(), err
}

// The hand-over of a repository reads the run record and the report its pointer names,
// prints the pending decisions, writes the resume note and marks no note read.
func TestRunUnitsHandoverEndToEnd(t *testing.T) {
	cfgPath, _ := writeUnitsConfig(t)
	dir := t.TempDir()
	id := "tekmora/epitrop#5"
	reportPath := setupHandoverRecords(t, cfgPath, dir, id)
	answers := filepath.Join(dir, "answers.tsv")
	if err := os.WriteFile(answers, []byte("epitrop-licence\tEUPL-1.2\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(answers) = %v", err)
	}
	resume := filepath.Join(dir, "resume.md")
	checkHandoverRounds(t, cfgPath, answers, resume, id, reportPath)
	checkHandoverResumeNote(t, resume)
}

func setupHandoverRecords(t *testing.T, cfgPath string, dir string, id string) string {
	t.Helper()
	report := []byte(`{"unit":"tekmora/epitrop#5","outcome":"blocked","summary":"needs a name","questions":["DECISION epitrop-name: which","ASK epitrop-licence: which"]}` + "\n")
	reportPath := filepath.Join(dir, slot.ReportFile)
	if err := os.WriteFile(reportPath, report, 0o600); err != nil {
		t.Fatalf("WriteFile(report) = %v", err)
	}
	sum := sha256.Sum256(report)
	steps := [][]string{
		{"set", "--config=" + cfgPath, id, "--status=implementing", "--lane=slot-1"},
		{"set", "--config=" + cfgPath, id, "--status=blocked", "--evidence=" + reportPath + " sha256:" + hex.EncodeToString(sum[:]) + " lines:1"},
		{"note", "--config=" + cfgPath, "--from=controller", id, "see the ledger"},
		{"set", "--config=" + cfgPath, "tekmora/other#1", "--status=implementing"},
	}
	for i := 0; i < len(steps); i++ {
		if err := runUnits(steps[i]); err != nil {
			t.Fatalf("runUnits(%q) = %v", steps[i], err)
		}
	}
	return reportPath
}

func checkHandoverRounds(t *testing.T, cfgPath string, answers string, resume string, id string, reportPath string) {
	t.Helper()
	for round := 0; round < 2; round++ {
		out, err := captureHandover(t, "--config="+cfgPath, "--repo=tekmora/epitrop", "--answers="+answers, "--resume-out="+resume)
		if err != nil {
			t.Fatalf("units handover = %v, want nil", err)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[1], id+"\tblocked\t") || !strings.Contains(lines[1], "\tslot-1\t") ||
			!strings.Contains(lines[1], "\tblocked\t\"epitrop-name\"\t1\t"+reportPath) {
			t.Fatalf("round %d printed %q, want one unit with its one pending decision and one unread note", round, out)
		}
	}
}

func checkHandoverResumeNote(t *testing.T, resume string) {
	t.Helper()
	note, err := os.ReadFile(resume) // #nosec G304 -- resume is the note path under t.TempDir
	if err != nil || !strings.Contains(string(note), `- pending decision: "epitrop-name"`) || !strings.Contains(string(note), `"see the ledger"`) {
		t.Fatalf("resume.md = %q, %v; want the pending decision and the unread note", note, err)
	}
}

// A refusal of the answers file or the repository leaves no resume note.
func TestRunUnitsHandoverRefusals(t *testing.T) {
	cfgPath, _ := writeUnitsConfig(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "answers.tsv")
	if err := os.WriteFile(bad, []byte("not an id\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(answers) = %v", err)
	}
	resume := filepath.Join(dir, "resume.md")
	cases := map[string][]string{
		"bad answers":     {"--config=" + cfgPath, "--repo=r", "--answers=" + bad, "--resume-out=" + resume},
		"missing answers": {"--config=" + cfgPath, "--repo=r", "--answers=" + filepath.Join(dir, "none"), "--resume-out=" + resume},
		"no repository":   {"--config=" + cfgPath, "--resume-out=" + resume},
		"a unit id":       {"--config=" + cfgPath, "--repo=r#1", "--resume-out=" + resume},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := captureHandover(t, args...); err == nil {
				t.Fatalf("units handover %q = nil, want a refusal", args)
			}
			if _, err := os.Lstat(resume); !os.IsNotExist(err) {
				t.Fatalf("resume note written (%v), want none", err)
			}
		})
	}
}
