package slot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/eventlog"
)

// writeReport writes a run's report into a directory of its own and returns the pointer the
// runner records for it.
func writeReport(t *testing.T, report Report) *eventlog.UnitEvidence {
	t.Helper()
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal(report) = %v, want nil", err)
	}
	path := filepath.Join(t.TempDir(), ReportFile)
	if err = os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("WriteFile(report) = %v, want nil", err)
	}
	return &eventlog.UnitEvidence{Path: path, SHA256: digestOf(body), Lines: 1}
}

func writeAnswers(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "answers.tsv")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(answers) = %v, want nil", err)
	}
	return path
}

// The hand-over of a repository holds its open units and no other: the units of another
// repository whose name starts the same, and landed or dropped units, stay out.
func TestHandoverListsTheOpenUnitsOfOneRepository(t *testing.T) {
	st := eventlog.State{Units: map[string]eventlog.Unit{
		"tekmora/epitrop#2":        {ID: "tekmora/epitrop#2", Status: "implementing", Lane: "slot-1", Worktree: "/w/2"},
		"tekmora/epitrop:n23-part": {ID: "tekmora/epitrop:n23-part", Status: "claimed"},
		"tekmora/epitrop#1":        {ID: "tekmora/epitrop#1", Status: "landed"},
		"tekmora/epitrop#3":        {ID: "tekmora/epitrop#3", Status: "dropped"},
		"tekmora/epitrop-web#4":    {ID: "tekmora/epitrop-web#4", Status: "implementing"},
	}}
	list, err := Handover(st, "tekmora/epitrop", nil)
	if err != nil {
		t.Fatalf("Handover() = %v, want nil", err)
	}
	ids := make([]string, 0, len(list))
	for i := 0; i < len(list); i++ {
		ids = append(ids, list[i].ID)
	}
	if want := []string{"tekmora/epitrop#2", "tekmora/epitrop:n23-part"}; !slices.Equal(ids, want) {
		t.Fatalf("units = %q, want %q", ids, want)
	}
	if list[0].Lane != "slot-1" || list[0].Worktree != "/w/2" || list[0].Relaunch != "safe" {
		t.Fatalf("unit = %+v, want its lane, worktree and relaunch rule", list[0])
	}
}

// A repository name that is no repository part of a unit id is refused, not matched loosely.
func TestHandoverRefusesWhatIsNoRepository(t *testing.T) {
	for _, repo := range []string{"", "tekmora/epitrop#2", "tekmora/epitrop:x"} {
		if _, err := Handover(eventlog.State{}, repo, nil); err == nil {
			t.Fatalf("Handover(%q) = nil, want a refusal", repo)
		}
	}
}

// The pending decisions are the question ids of the last run's report that the answers do
// not hold; a question without an id stays pending as it is written. The unread notes come
// along, and nothing is marked read.
func TestHandoverNamesTheUnansweredQuestionsAndUnreadNotes(t *testing.T) {
	pointer := writeReport(t, Report{Unit: "r#1", Outcome: OutcomeBlocked, Summary: "needs a name", Questions: []string{
		"DECISION r-name: which name | options: a) x b) y",
		"ASK r-licence: which licence | tried: docs",
		"ASK without an id",
	}})
	unit := eventlog.Unit{ID: "r#1", Status: "blocked", Evidence: pointer, ReadUpto: 1, Notes: []eventlog.UnitNote{
		{Seq: 1, From: "a", Text: "read"}, {Seq: 2, From: "controller", Text: "see r-name"},
	}}
	answers, err := ReadAnswers(writeAnswers(t, "# answers\n\nr-licence\tEUPL-1.2\nother-id\n"))
	if err != nil {
		t.Fatalf("ReadAnswers() = %v, want nil", err)
	}
	list, err := Handover(eventlog.State{Units: map[string]eventlog.Unit{"r#1": unit}}, "r", answers)
	if err != nil {
		t.Fatalf("Handover() = %v, want nil", err)
	}
	h := list[0]
	if want := []string{"r-name", "ASK without an id"}; !slices.Equal(h.Pending, want) {
		t.Fatalf("pending = %q, want %q", h.Pending, want)
	}
	if h.Outcome != OutcomeBlocked || h.Summary != "needs a name" || h.ReportError != "" {
		t.Fatalf("unit = %+v, want the report's outcome and summary", h)
	}
	if len(h.Unread) != 1 || h.Unread[0].Seq != 2 {
		t.Fatalf("unread = %+v, want note 2 only", h.Unread)
	}
	if !strings.HasPrefix(h.Relaunch, "no: ") {
		t.Fatalf("relaunch = %q, want no for a blocked unit", h.Relaunch)
	}
}

// A report that changed after its run was recorded, or that is gone, leaves the pending
// decisions unknown and says why; it never reads as nothing pending.
func TestHandoverRefusesAReportThatIsNotTheRecordedOne(t *testing.T) {
	cases := map[string]func(*testing.T, *eventlog.UnitEvidence){
		"changed":            spoilReportChanged,
		"gone":               spoilReportGone,
		"a link":             spoilReportLink,
		"a digest too short": func(_ *testing.T, p *eventlog.UnitEvidence) { p.SHA256 = p.SHA256[:11] },
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			runHandoverRefusedReportCase(t, spoil)
		})
	}
}

func spoilReportChanged(t *testing.T, p *eventlog.UnitEvidence) {
	t.Helper()
	if err := os.WriteFile(p.Path, []byte(`{"unit":"r#1","outcome":"done"}`), 0o600); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
}

func spoilReportGone(t *testing.T, p *eventlog.UnitEvidence) {
	t.Helper()
	if err := os.Remove(p.Path); err != nil {
		t.Fatalf("Remove = %v", err)
	}
}

func spoilReportLink(t *testing.T, p *eventlog.UnitEvidence) {
	t.Helper()
	target := p.Path + ".real"
	if err := os.Rename(p.Path, target); err != nil {
		t.Fatalf("Rename = %v", err)
	}
	if err := os.Symlink(target, p.Path); err != nil {
		t.Fatalf("Symlink = %v", err)
	}
}

func runHandoverRefusedReportCase(t *testing.T, spoil func(*testing.T, *eventlog.UnitEvidence)) {
	t.Helper()
	pointer := writeReport(t, Report{Unit: "r#1", Outcome: OutcomeBlocked, Questions: []string{"ASK r-x: y"}})
	spoil(t, pointer)
	unit := eventlog.Unit{ID: "r#1", Status: "blocked", Evidence: pointer}
	list, err := Handover(eventlog.State{Units: map[string]eventlog.Unit{"r#1": unit}}, "r", nil)
	if err != nil {
		t.Fatalf("Handover() = %v, want nil", err)
	}
	if list[0].ReportError == "" || list[0].Pending != nil {
		t.Fatalf("unit = %+v, want a report error and no pending list", list[0])
	}
}

// A pointer may hold the first 12 digits of the digest only; a start file is no report.
func TestHandoverTakesAShortDigestAndSkipsAStartFile(t *testing.T) {
	pointer := writeReport(t, Report{Unit: "r#1", Outcome: OutcomeDone, Questions: []string{"ASK r-x: y"}})
	pointer.SHA256 = pointer.SHA256[:12]
	start := &eventlog.UnitEvidence{Path: "/nowhere/" + StartFile, SHA256: strings.Repeat("0", 64)}
	st := eventlog.State{Units: map[string]eventlog.Unit{
		"r#1": {ID: "r#1", Status: "review", Evidence: pointer},
		"r#2": {ID: "r#2", Status: "implementing", Evidence: start},
	}}
	list, err := Handover(st, "r", nil)
	if err != nil {
		t.Fatalf("Handover() = %v, want nil", err)
	}
	if list[0].ReportError != "" || !slices.Equal(list[0].Pending, []string{"r-x"}) {
		t.Fatalf("unit 1 = %+v, want the report read through the short digest", list[0])
	}
	if list[1].ReportError != "" || list[1].Evidence != start.Path || list[1].Outcome != "" {
		t.Fatalf("unit 2 = %+v, want the start file named and not read", list[1])
	}
}

// An answers file that cannot be read as written is refused as a whole.
func TestReadAnswersRefusesAMisreadFile(t *testing.T) {
	cases := map[string]string{
		"an id with a space":   "r name\n",
		"a question line":      "DECISION r-x: y\n",
		"an id given twice":    "r-x\nr-x\tagain\n",
		"a leading dash":       "-r\n",
		"an id of 129 digits":  strings.Repeat("a", 129) + "\n",
		"a line above the cap": strings.Repeat("a", MaxAnswersBytes+1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadAnswers(writeAnswers(t, body)); err == nil {
				t.Fatalf("ReadAnswers(%q...) = nil, want a refusal", body[:min(len(body), 20)])
			}
		})
	}
	if answers, err := ReadAnswers(writeAnswers(t, strings.Repeat("a", 128)+"\n")); err != nil || len(answers) != 1 {
		t.Fatalf("ReadAnswers(id of 128) = %v, %v, want one answer", answers, err)
	}
}

// The resume note holds what the next session needs, with every field quoted so a note
// cannot forge a line of its own.
func TestResumeNoteQuotesEveryField(t *testing.T) {
	list := []HandoverUnit{{ID: "r#1", Status: "blocked", PR: 7, Summary: "line\n## r#9", Pending: []string{"r-name"},
		Unread: []eventlog.UnitNote{{Seq: 2, From: "c\n## r#8", Text: "x\n- pending decision: fake"}}}}
	note, err := ResumeNote("r", list)
	if err != nil {
		t.Fatalf("ResumeNote() = %v, want nil", err)
	}
	for _, want := range []string{"# Hand-over: r\n", "\n## r#1\n", `- pr: "#7"`, `- pending decision: "r-name"`, `- summary: "line\n## r#9"`} {
		if !strings.Contains(string(note), want) {
			t.Fatalf("note = %s, want %q", note, want)
		}
	}
	if strings.Count(string(note), "\n## ") != 1 || strings.Count(string(note), "\n- pending decision:") != 1 {
		t.Fatalf("note = %s, want the summary and the note unable to add a unit or a decision", note)
	}
}

// The note holds at the bound of a slot's input and is refused one byte above it, not cut.
func TestResumeNoteBound(t *testing.T) {
	sized := func(n int) []HandoverUnit {
		base, err := ResumeNote("r", []HandoverUnit{{ID: "r#1", Summary: ""}})
		if err != nil {
			t.Fatalf("ResumeNote() = %v, want nil", err)
		}
		// An empty summary prints no line; a summary adds its quoted text on a line of its own.
		return []HandoverUnit{{ID: "r#1", Summary: strings.Repeat("s", n-len(base)-len("- summary: \"\"\n"))}}
	}
	note, err := ResumeNote("r", sized(MaxInputBytes))
	if err != nil || len(note) != MaxInputBytes {
		t.Fatalf("ResumeNote(%d bytes) = %d bytes, %v; want it whole", MaxInputBytes, len(note), err)
	}
	note, err = ResumeNote("r", sized(MaxInputBytes+1))
	if !errors.Is(err, ErrResumeTooLarge) || note != nil {
		t.Fatalf("ResumeNote(%d bytes) = %d bytes, %v; want ErrResumeTooLarge", MaxInputBytes+1, len(note), err)
	}
}

// Writing the note replaces the file whole and leaves no file beside it.
func TestWriteResumeNoteReplacesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resume.md")
	for _, body := range []string{"first\n", "second\n"} {
		if err := WriteResumeNote(path, []byte(body)); err != nil {
			t.Fatalf("WriteResumeNote() = %v, want nil", err)
		}
		got, err := os.ReadFile(path) // #nosec G304 -- path is resume.md under t.TempDir
		if err != nil || string(got) != body {
			t.Fatalf("resume.md = %q, %v; want %q", got, err, body)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory holds %v, %v; want resume.md only", entries, err)
	}
	if err = WriteResumeNote(filepath.Join(dir, "missing", "resume.md"), []byte("x")); err == nil {
		t.Fatalf("WriteResumeNote(missing directory) = nil, want an error")
	}
}
