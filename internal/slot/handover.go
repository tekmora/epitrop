package slot

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tekmora/epitrop/internal/atomicfile"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/units"
)

// MaxAnswersBytes bounds an answers file.
const MaxAnswersBytes = 1 << 20

// maxAnswerLines bounds the lines of an answers file, blank lines and comments included.
const maxAnswerLines = 1 << 16

// maxReportBytes bounds a run's report: the result it carries is bounded by MaxResultBytes,
// and the runner adds less than as much again.
const maxReportBytes = 2 * MaxResultBytes

var (
	// questionIDRegex reads the id of a question line: DECISION or ASK, a space, the id and
	// a colon.
	questionIDRegex = regexp.MustCompile(`^(?:DECISION|ASK) ([A-Za-z0-9][A-Za-z0-9._-]{0,127}):`)
	answerIDRegex   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// HandoverUnit is what the next session needs to know of one open unit: its record, the
// last report of a run of it, the questions of that run that have no answer, and the notes
// nobody has read.
type HandoverUnit struct {
	ID        string
	Status    string
	UpdatedAt string
	Worktree  string
	Branch    string
	PR        int
	Lane      string
	Relaunch  string
	Evidence  string
	// Outcome and Summary come from the run's report, when the evidence pointer names one.
	Outcome string
	Summary string
	// Pending holds the ids of the report's questions that the answers do not hold; a
	// question line without an id is held as it is written.
	Pending []string
	Unread  []eventlog.UnitNote
	// ReportError says why the report the pointer names was not read. Pending is then
	// unknown, not empty.
	ReportError string
}

// QuestionID returns the id of a DECISION or ASK line, and false for a line without one.
func QuestionID(line string) (string, bool) {
	m := questionIDRegex.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ReadAnswers reads an answers file: one answer per line, its id, then optionally a tab and
// the answer's text. Blank lines and lines starting with # are skipped. A line that is
// neither, an id given twice and a file above MaxAnswersBytes are refused: a misread file
// would hide an open question as answered.
func ReadAnswers(path string) (map[string]string, error) {
	body, err := readRegular(path, MaxAnswersBytes)
	if err != nil {
		return nil, fmt.Errorf("slot: answers: %w", err)
	}
	answers := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 4096), MaxAnswersBytes)
	for n := 1; n <= maxAnswerLines && scanner.Scan(); n++ {
		if err = addAnswer(answers, scanner.Text()); err != nil {
			return nil, fmt.Errorf("slot: answers %s:%d: %w", path, n, err)
		}
	}
	if err = scanner.Err(); err != nil {
		return nil, fmt.Errorf("slot: answers %s: %w", path, err)
	}
	return answers, nil
}

func addAnswer(answers map[string]string, line string) error {
	if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
		return nil
	}
	id, text, _ := strings.Cut(line, "\t")
	if !answerIDRegex.MatchString(id) {
		return fmt.Errorf("%q is no answer id", id)
	}
	if _, seen := answers[id]; seen {
		return fmt.Errorf("answer %s given twice", id)
	}
	answers[id] = text
	return nil
}

// RepoOf returns the repository part of a unit id: the text before its # or : suffix.
func RepoOf(id string) string {
	if i := strings.IndexAny(id, "#:"); i >= 0 {
		return id[:i]
	}
	return id
}

// Handover lists the open units of one repository, sorted by id. It reads the run record it
// is given and the reports its evidence pointers name, and writes nothing.
func Handover(st eventlog.State, repo string, answers map[string]string) ([]HandoverUnit, error) {
	if repo == "" || RepoOf(repo) != repo {
		return nil, fmt.Errorf("slot: handover: %q is no repository of a unit id", repo)
	}
	ids := make([]string, 0, len(st.Units))
	for id, u := range st.Units {
		if RepoOf(id) == repo && !units.IsTerminalStatus(u.Status) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]HandoverUnit, 0, len(ids))
	for i := 0; i < len(ids); i++ {
		out = append(out, handoverUnit(st.Units[ids[i]], answers))
	}
	return out, nil
}

func handoverUnit(u eventlog.Unit, answers map[string]string) HandoverUnit {
	h := HandoverUnit{ID: u.ID, Status: u.Status, UpdatedAt: u.UpdatedAt, Worktree: u.Worktree,
		Branch: u.Branch, PR: u.PR, Lane: u.Lane, Relaunch: "safe", Unread: units.UnreadNotes(u)}
	if safe, reason := units.RelaunchRule(u); !safe {
		h.Relaunch = "no: " + reason
	}
	if u.Evidence == nil {
		return h
	}
	h.Evidence = u.Evidence.Path
	if filepath.Base(u.Evidence.Path) != ReportFile {
		return h
	}
	report, err := ReadPointedReport(*u.Evidence)
	if err != nil {
		h.ReportError = err.Error()
		return h
	}
	h.Outcome, h.Summary = report.Outcome, report.Summary
	h.Pending = pendingQuestions(report.Questions, answers)
	return h
}

// ReadPointedReport reads the report an evidence pointer names and refuses one whose digest
// does not start with the pointer's, which holds 12 to 64 digits: the report changed after
// the run was recorded.
func ReadPointedReport(pointer eventlog.UnitEvidence) (Report, error) {
	body, err := readRegular(pointer.Path, maxReportBytes)
	if err != nil {
		return Report{}, err
	}
	if len(pointer.SHA256) < 12 || !strings.HasPrefix(digestOf(body), pointer.SHA256) {
		return Report{}, fmt.Errorf("%s does not match the recorded digest", pointer.Path)
	}
	var report Report
	if err = json.Unmarshal(body, &report); err != nil {
		return Report{}, fmt.Errorf("%s is not valid: %w", pointer.Path, err)
	}
	return report, nil
}

func pendingQuestions(questions []string, answers map[string]string) []string {
	var pending []string
	for i := 0; i < len(questions); i++ {
		id, ok := QuestionID(questions[i])
		if !ok {
			pending = append(pending, questions[i])
			continue
		}
		if _, answered := answers[id]; !answered {
			pending = append(pending, id)
		}
	}
	return pending
}

// ErrResumeTooLarge says the resume note does not fit the bound of a slot's input.
var ErrResumeTooLarge = errors.New("slot: the resume note is larger than a slot's input bound")

// ResumeNote writes the hand-over of a repository as the resume note a slot run takes. A
// note above MaxInputBytes is refused, not cut: a cut note would drop units silently.
func ResumeNote(repo string, list []HandoverUnit) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Hand-over: %s\n\n%d open units.\n", repo, len(list))
	for i := 0; i < len(list); i++ {
		writeResumeUnit(&b, list[i])
	}
	if b.Len() > MaxInputBytes {
		return nil, fmt.Errorf("%w: %d bytes, the bound is %d", ErrResumeTooLarge, b.Len(), MaxInputBytes)
	}
	return b.Bytes(), nil
}

func writeResumeUnit(b *bytes.Buffer, h HandoverUnit) {
	fmt.Fprintf(b, "\n## %s\n\n", h.ID)
	fields := [][2]string{
		{"status", h.Status}, {"updated", h.UpdatedAt}, {"worktree", h.Worktree}, {"branch", h.Branch},
		{"lane", h.Lane}, {"relaunch", h.Relaunch}, {"evidence", h.Evidence}, {"outcome", h.Outcome},
	}
	if h.PR != 0 {
		fields = append(fields, [2]string{"pr", "#" + strconv.Itoa(h.PR)})
	}
	for i := 0; i < len(fields); i++ {
		if fields[i][1] != "" {
			fmt.Fprintf(b, "- %s: %q\n", fields[i][0], fields[i][1])
		}
	}
	if h.ReportError != "" {
		fmt.Fprintf(b, "- report unreadable, pending decisions unknown: %q\n", h.ReportError)
	}
	if h.Summary != "" {
		fmt.Fprintf(b, "- summary: %q\n", h.Summary)
	}
	for i := 0; i < len(h.Pending); i++ {
		fmt.Fprintf(b, "- pending decision: %q\n", h.Pending[i])
	}
	for i := 0; i < len(h.Unread); i++ {
		fmt.Fprintf(b, "- unread note %d from %q: %q\n", h.Unread[i].Seq, h.Unread[i].From, h.Unread[i].Text)
	}
}

// WriteResumeNote replaces path with body through a file beside it, so a reader never sees
// half a note.
func WriteResumeNote(path string, body []byte) error {
	if err := atomicfile.Replace(path, body, 0o600); err != nil {
		return fmt.Errorf("slot: write resume note %s: %w", path, err)
	}
	return nil
}
