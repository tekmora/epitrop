package slot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const testUnit = "epitrop:unit-1"

// resultFixture is one result a run may leave, in the fixture format of the architecture
// document: a violation is a result the runner must refuse, a compliant case one it must
// take. Files are the workspace files the run wrote beside it, by path.
type resultFixture struct {
	ID        string            `json:"id"`
	Rule      string            `json:"rule"`
	Direction string            `json:"direction"`
	Document  string            `json:"document"`
	Files     map[string]string `json:"files"`
	Expect    struct {
		Result string `json:"result"`
		// Says is a part of the refusal the runner has to give, "" for an accepted result.
		Says string `json:"says"`
	} `json:"expect"`
}

func loadResultFixtures(t *testing.T) []resultFixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "result", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("Glob(testdata/result) = %d files, %v; want the fixtures", len(paths), err)
	}
	sort.Strings(paths)
	fixtures := make([]resultFixture, 0, len(paths))
	for i := 0; i < len(paths); i++ {
		body, err := os.ReadFile(paths[i])
		if err != nil {
			t.Fatalf("ReadFile(%s) = %v, want nil", paths[i], err)
		}
		var fixture resultFixture
		if err = json.Unmarshal(body, &fixture); err != nil {
			t.Fatalf("%s: %v", paths[i], err)
		}
		if fixture.ID+".json" != filepath.Base(paths[i]) || fixture.Rule != "slot-result" {
			t.Fatalf("%s: id %q, rule %q; want the file's name and slot-result", paths[i], fixture.ID, fixture.Rule)
		}
		fixtures = append(fixtures, fixture)
	}
	return fixtures
}

// writeWorkspace builds a run's workspace: the result document and the files beside it.
func writeWorkspace(t *testing.T, document string, files map[string]string) string {
	t.Helper()
	ws := t.TempDir()
	write := func(rel string, body string) {
		path := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("MkdirAll(%s) = %v, want nil", rel, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) = %v, want nil", rel, err)
		}
	}
	write(ResultFile, document)
	for rel, body := range files {
		write(rel, body)
	}
	return ws
}

// checkResultFixture replays one fixture and returns what is wrong with the outcome, "" when
// nothing is. It fails a violation that was accepted and a compliant result that was refused.
func checkResultFixture(t *testing.T, fixture resultFixture) string {
	t.Helper()
	_, err := ReadResult(writeWorkspace(t, fixture.Document, fixture.Files), testUnit)
	switch {
	case fixture.Direction == "violation" && err == nil:
		return "a violation was accepted"
	case fixture.Direction == "compliant" && err != nil:
		return "a compliant result was refused: " + err.Error()
	case (fixture.Expect.Result == "accepted") != (err == nil):
		return "expect.result does not match the direction"
	case err != nil && !errors.Is(err, ErrResult):
		return "refused with another error than ErrResult: " + err.Error()
	case err != nil && !strings.Contains(err.Error(), fixture.Expect.Says):
		return "the refusal does not say " + fixture.Expect.Says + ": " + err.Error()
	}
	return ""
}

func TestResultFixtures(t *testing.T) {
	fixtures := loadResultFixtures(t)
	directions := map[string]int{}
	for i := 0; i < len(fixtures); i++ {
		fixture := fixtures[i]
		directions[fixture.Direction]++
		t.Run(fixture.ID, func(t *testing.T) {
			if wrong := checkResultFixture(t, fixture); wrong != "" {
				t.Fatalf("%s (%s): %s", fixture.ID, fixture.Direction, wrong)
			}
		})
	}
	if directions["violation"] == 0 || directions["compliant"] == 0 || len(directions) != 2 {
		t.Fatalf("fixture directions = %v, want violations and compliant results and nothing else", directions)
	}
}

// The checker itself is checked: a fixture that claims the wrong outcome, or the wrong
// refusal, is reported.
func TestResultCheckerRefusesAFixtureThatClaimsTheWrongOutcome(t *testing.T) {
	sound := `{"unit":"epitrop:unit-1","outcome":"failed","summary":"The build does not start."}`
	other := `{"unit":"epitrop:unit-2","outcome":"failed","summary":"x"}`
	planted := func(direction string, document string, result string, says string) resultFixture {
		fixture := resultFixture{ID: "planted", Rule: "slot-result", Direction: direction, Document: document}
		fixture.Expect.Result, fixture.Expect.Says = result, says
		return fixture
	}
	cases := []struct {
		name    string
		fixture resultFixture
	}{
		{"a result for another unit planted as compliant", planted("compliant", other, "accepted", "")},
		{"a sound result planted as a violation", planted("violation", sound, "refused", "")},
		{"a refusal that says something else", planted("violation", other, "refused", "needs the head commit")},
		{"a result that contradicts the direction", planted("violation", other, "accepted", "")},
	}
	for i := 0; i < len(cases); i++ {
		if wrong := checkResultFixture(t, cases[i].fixture); wrong == "" {
			t.Errorf("%s passed the checker, want it reported", cases[i].name)
		}
	}
	if wrong := checkResultFixture(t, planted("compliant", sound, "accepted", "")); wrong != "" {
		t.Errorf("a sound fixture was reported: %s", wrong)
	}
}

func requireRefused(t *testing.T, ws string, unit string, says string) {
	t.Helper()
	_, err := ReadResult(ws, unit)
	if !errors.Is(err, ErrResult) || !strings.Contains(err.Error(), says) {
		t.Fatalf("ReadResult() = %v, want a refused result that says %q", err, says)
	}
}

// What a result file is matters as much as what it holds: a missing file, a symlink, a
// directory and a file above the size bound are refused; one exactly at the bound is read.
func TestReadResultRefusesWhatIsNoRegularFile(t *testing.T) {
	sound := `{"unit":"epitrop:unit-1","outcome":"failed","summary":"x"}`
	requireRefused(t, t.TempDir(), testUnit, "result.json")
	ws := writeWorkspace(t, sound, nil)
	requireRefused(t, ws, "unit-1", `invalid unit id "unit-1"`)
	link := t.TempDir()
	if err := os.MkdirAll(filepath.Join(link, filepath.FromSlash(OutDir)), 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	if err := os.Symlink(filepath.Join(ws, filepath.FromSlash(ResultFile)), filepath.Join(link, filepath.FromSlash(ResultFile))); err != nil {
		t.Fatalf("Symlink() = %v, want nil", err)
	}
	requireRefused(t, link, testUnit, "is not a regular file")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(ResultFile)), 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	requireRefused(t, dir, testUnit, "is not a regular file")
	padded := func(size int) string {
		head := `{"unit":"epitrop:unit-1","outcome":"failed","summary":"x"}`
		return head + strings.Repeat(" ", size-len(head))
	}
	if _, err := ReadResult(writeWorkspace(t, padded(MaxResultBytes), nil), testUnit); err != nil {
		t.Fatalf("ReadResult(document at the size bound) = %v, want nil", err)
	}
	requireRefused(t, writeWorkspace(t, padded(MaxResultBytes+1), nil), testUnit, "is larger than 65536 bytes")
}

// An evidence file reached through a symlink that leaves the evidence directory is refused,
// also when what it points at holds exactly what the result claims. So is a file above the
// size bound.
func TestReadResultRefusesEvidenceOutsideOrTooLarge(t *testing.T) {
	log := "gofmt ok\nvet ok\ntests ok\n"
	const digest = "5ed945c32508"
	fixtures := loadResultFixtures(t)
	var done resultFixture
	for i := 0; i < len(fixtures); i++ {
		if fixtures[i].ID == "result-done-with-evidence" {
			done = fixtures[i]
		}
	}
	if !strings.Contains(done.Document, digest) {
		t.Fatalf("fixture result-done-with-evidence = %q, want the document with digest %s", done.Document, digest)
	}
	outside := filepath.Join(t.TempDir(), "gate.log")
	if err := os.WriteFile(outside, []byte(log), 0o600); err != nil {
		t.Fatalf("WriteFile(outside) = %v, want nil", err)
	}
	ws := writeWorkspace(t, done.Document, nil)
	evidence := filepath.Join(ws, filepath.FromSlash(EvidenceDir))
	if err := os.MkdirAll(evidence, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	if err := os.Symlink(outside, filepath.Join(evidence, "gate.log")); err != nil {
		t.Fatalf("Symlink() = %v, want nil", err)
	}
	requireRefused(t, ws, testUnit, "leaves the evidence directory through a symlink")
	inside := writeWorkspace(t, done.Document, map[string]string{EvidenceDir + "/real.log": log})
	if err := os.Symlink("real.log", filepath.Join(inside, filepath.FromSlash(EvidenceDir), "gate.log")); err != nil {
		t.Fatalf("Symlink() = %v, want nil", err)
	}
	requireRefused(t, inside, testUnit, "is not a regular file")
	large := writeWorkspace(t, done.Document, map[string]string{EvidenceDir + "/gate.log": strings.Repeat("x", MaxEvidenceBytes+1)})
	requireRefused(t, large, testUnit, "is larger than 8388608 bytes")
}

// A file that was swapped between the look and the open is refused: what is read has to be
// the file that was checked.
func TestReadOpenedRefusesAFileThatIsNotTheOneLookedAt(t *testing.T) {
	looked, opened := createLookedAndOpenedFiles(t, t.TempDir())
	before, err := os.Lstat(looked)
	if err != nil {
		t.Fatalf("Lstat() = %v, want nil", err)
	}
	f, err := os.Open(opened) // #nosec G304 -- opened is created by createLookedAndOpenedFiles in t.TempDir
	if err != nil {
		t.Fatalf("Open() = %v, want nil", err)
	}
	t.Cleanup(func() {
		if closeErr := f.Close(); closeErr != nil {
			t.Errorf("Close() = %v, want nil", closeErr)
		}
	})
	if _, err = readOpened(f, before, MaxResultBytes); err == nil || !strings.Contains(err.Error(), "changed while it was opened") {
		t.Fatalf("readOpened(another file) = %v, want it refused", err)
	}
	same, err := os.Lstat(opened)
	if err != nil {
		t.Fatalf("Lstat() = %v, want nil", err)
	}
	if body, err := readOpened(f, same, MaxResultBytes); err != nil || string(body) != "x\n" {
		t.Fatalf("readOpened(the same file) = %q, %v; want its bytes", body, err)
	}
}

func createLookedAndOpenedFiles(t *testing.T, dir string) (looked, opened string) {
	t.Helper()
	looked, opened = filepath.Join(dir, "looked"), filepath.Join(dir, "opened")
	for _, path := range []string{looked, opened} {
		if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
			t.Fatalf("WriteFile() = %v, want nil", err)
		}
	}
	return looked, opened
}

func TestResultSchemaIsACopy(t *testing.T) {
	first := ResultSchema()
	first[0] = 'X'
	if second := ResultSchema(); len(second) == 0 || second[0] == 'X' {
		t.Fatal("ResultSchema() handed out its own bytes, want a copy each time")
	}
}
