package slot

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golusoris/golusoris/core/clock"
	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/gitexec"
	"github.com/tekmora/epitrop/internal/units"
)

// standIn is the job of a test run. Its work is done in Start, by the test's own function,
// directly in the workspace: what a sandboxed harness would do, without the sandbox.
type standIn struct {
	name         string
	preflightErr error
	startErr     error
	waitErr      error
	adoptErr     error
	end          JobEnd
	work         func()
	started      int
	waited       int
	adopted      int
}

func (j *standIn) Name() string {
	if j.name != "" {
		return j.name
	}
	return "slot-1"
}

func (j *standIn) Preflight(context.Context) error { return j.preflightErr }

func (j *standIn) Start(context.Context) error {
	j.started++
	if j.startErr != nil {
		return j.startErr
	}
	if j.work != nil {
		j.work()
	}
	return nil
}

func (j *standIn) Wait(context.Context) (JobEnd, error) {
	j.waited++
	return j.end, j.waitErr
}

func (j *standIn) Adopt(context.Context, string) error {
	j.adopted++
	return j.adoptErr
}

// failingRecorder is the run record with one planted failure: the record with this status.
type failingRecorder struct {
	*units.Service
	failStatus string
}

func (r failingRecorder) Record(ctx context.Context, id string, payload units.RecordPayload) (eventlog.Unit, error) {
	if payload.Status == r.failStatus {
		return eventlog.Unit{}, errors.New("planted: the run record is not writable")
	}
	return r.Service.Record(ctx, id, payload)
}

func runRecord(t *testing.T) *units.Service {
	t.Helper()
	clk := clock.NewFake()
	seed := make([]byte, 32)
	for i := 0; i < len(seed); i++ {
		seed[i] = byte(i + 1)
	}
	signer, err := corereceipt.NewSignerFromSeed(seed, clk)
	if err != nil {
		t.Fatalf("NewSignerFromSeed() = %v, want nil", err)
	}
	svc, err := units.NewService(t.TempDir(), signer, clk, eventlog.Limits{Clock: clk, LockTimeout: time.Second, MaxReplayRecords: 20000})
	if err != nil {
		t.Fatalf("NewService() = %v, want nil", err)
	}
	return svc
}

// runWork is what a test run leaves behind.
type runWork struct {
	outcome   string
	commit    bool
	head      string // "" names the commit the work made; anything else is claimed as it is
	evidence  bool
	questions []string
	// stage runs before the commit, for a test whose commit carries more than a file.
	stage func(ws Workspace)
	// after runs when the result is written, for a test that spoils something.
	after func(ws Workspace)
	// inOut runs last, on the run's output directory. A sandboxed run cannot reach that
	// directory; a test does, to make the runner's own writing there fail.
	inOut func(out string)
}

func commitWork(t *testing.T, ws Workspace, stage func(Workspace)) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws.Dir, "third.txt"), []byte("third\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	gitIn(t, ws.Dir, "add", "--all")
	if stage != nil {
		stage(ws)
	}
	gitIn(t, ws.Dir, "commit", "--quiet", "--message", "the unit's work")
	return gitIn(t, ws.Dir, "rev-parse", "HEAD")
}

func writeWorkEvidence(t *testing.T, dir string) Evidence {
	t.Helper()
	body := []byte("ok 1\nok 2\n")
	path := EvidenceDir + "/tests/unit.log"
	full := filepath.Join(dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	if err := os.WriteFile(full, body, 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	return Evidence{Path: path, SHA256: digestOf(body), Lines: 2}
}

func writeWorkResult(t *testing.T, dir string, result Result) {
	t.Helper()
	doc, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}
	if err = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ResultFile)), doc, 0o600); err != nil {
		t.Fatalf("WriteFile(result) = %v, want nil", err)
	}
}

// doWork does a run's work in ws: a commit, an evidence file and the result.
func doWork(t *testing.T, ws Workspace, out string, w runWork) {
	t.Helper()
	head := gitIn(t, ws.Dir, "rev-parse", "HEAD")
	if w.commit {
		head = commitWork(t, ws, w.stage)
	}
	if w.head != "" {
		head = w.head
	}
	result := Result{Unit: testUnit, Outcome: w.outcome, Summary: "The retry is in.", Questions: w.questions}
	if w.outcome == OutcomeDone || w.commit {
		result.Head = head
	}
	if w.evidence {
		result.Evidence = []Evidence{writeWorkEvidence(t, ws.Dir)}
	}
	writeWorkResult(t, ws.Dir, result)
	if w.after != nil {
		w.after(ws)
	}
	if w.inOut != nil {
		w.inOut(out)
	}
}

// testRun is one prepared run: a repository, a run record and a stand-in job.
type testRun struct {
	repo   string
	base   string
	record *units.Service
	// recorder is what the run records through; a test replaces it to plant a failure.
	recorder UnitRecorder
	job      *standIn
	req      RunRequest
}

func newTestRun(t *testing.T, w *runWork) *testRun {
	t.Helper()
	repo, _, second := testRepo(t)
	run := &testRun{repo: repo, base: second, record: runRecord(t), job: &standIn{end: JobEnd{Recorded: true}}}
	run.req = RunRequest{Request: request(t, repo, second), Out: filepath.Join(t.TempDir(), "out")}
	run.recorder = run.record
	run.req.Open = func(_ context.Context, ws Workspace, out string) (Job, UnitRecorder, error) {
		if w != nil {
			run.job.work = func() { doWork(t, ws, out, *w) }
		}
		return run.job, run.recorder, nil
	}
	return run
}

func (r *testRun) unit(t *testing.T) eventlog.Unit {
	t.Helper()
	unit, err := r.record.Show(slotCtx(t), testUnit)
	if err != nil {
		t.Fatalf("Show() = %v, want nil", err)
	}
	return unit
}

// repoRefs lists every ref of the repository: its branches and anything the runner left.
func repoRefs(t *testing.T, repo string) string {
	t.Helper()
	return gitIn(t, repo, "for-each-ref", "--format=%(refname) %(objectname)")
}

func readReport(t *testing.T, out string) (Report, []byte) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(out, ReportFile)) // #nosec G304 -- callers pass the temporary output directory for the test run
	if err != nil {
		t.Fatalf("ReadFile(report) = %v, want nil", err)
	}
	var report Report
	if err = json.Unmarshal(body, &report); err != nil {
		t.Fatalf("Unmarshal(report) = %v, want nil", err)
	}
	return report, body
}

// gitFed is gitIn for a command that reads its standard input.
func gitFed(t *testing.T, dir string, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(slotCtx(t), "git", args...) // #nosec G204 -- fixture callers supply git argv in their temporary repository
	cmd.Dir, cmd.Env, cmd.Stdin = dir, gitexec.Env(os.Environ()), strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v = %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// plant writes a file for a test that puts something where the runner does not expect it.
func plant(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) = %v, want nil", path, err)
	}
}

func TestRunRecordsADoneRunAsReviewWithItsBranchAndEvidence(t *testing.T) {
	run := newTestRun(t, &runWork{outcome: OutcomeDone, commit: true, evidence: true})
	report, err := Run(slotCtx(t), run.req)
	if err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	head := gitIn(t, run.req.Dir, "rev-parse", "HEAD")
	if report.Outcome != OutcomeDone || report.Status != "review" || report.Head != head || !report.Published || !report.Taken() {
		t.Fatalf("report = %+v, want a done run at %s, published, for review", report, head)
	}
	branch := Branch(testUnit)
	assertDoneRunRepoRefs(t, run, branch, head)
	assertDoneRunEvidenceCopy(t, run, report)
	assertDoneRunResultCopy(t, run, report)
	body := assertDoneRunReport(t, run.req.Out, report)
	assertDoneRunUnit(t, run, branch, body)
}

func assertDoneRunRepoRefs(t *testing.T, run *testRun, branch, head string) {
	t.Helper()
	if got := gitIn(t, run.repo, "rev-parse", "refs/heads/"+branch); got != head {
		t.Fatalf("the repository's %s is at %s, want %s", branch, got, head)
	}
	if refs := repoRefs(t, run.repo); strings.Contains(refs, "refs/epitrop/") {
		t.Fatalf("refs = %q, want the runner's own ref removed again", refs)
	}
	if current := gitIn(t, run.repo, "rev-parse", "HEAD"); current != run.base {
		t.Fatalf("the repository's own branch moved to %s, want it at %s", current, run.base)
	}
}

func assertDoneRunEvidenceCopy(t *testing.T, run *testRun, report Report) {
	t.Helper()
	copied, err := os.ReadFile(filepath.Join(run.req.Out, EvidenceCopies, "tests", "unit.log"))
	if err != nil || string(copied) != "ok 1\nok 2\n" {
		t.Fatalf("evidence copy = %q, %v; want the file the run wrote", copied, err)
	}
	if len(report.Evidence) != 1 || report.Evidence[0].Path != "evidence/tests/unit.log" || report.Evidence[0].SHA256 != digestOf(copied) || report.Evidence[0].Lines != 2 {
		t.Fatalf("report evidence = %+v, want the copy with its digest and line count", report.Evidence)
	}
}

func assertDoneRunResultCopy(t *testing.T, run *testRun, report Report) {
	t.Helper()
	result, err := os.ReadFile(filepath.Join(run.req.Out, ResultCopy))
	if err != nil || report.Result == nil || digestOf(result) != report.Result.SHA256 || report.Result.Path != ResultCopy {
		t.Fatalf("result copy: %v, pointer %+v; want the copy the report names", err, report.Result)
	}
	if wrote := readFile(t, filepath.Join(run.req.Dir, filepath.FromSlash(ResultFile))); string(result) != wrote || wrote == "" {
		t.Fatalf("result copy = %q, want the result the run wrote %q", result, wrote)
	}
}

func assertDoneRunReport(t *testing.T, out string, report Report) []byte {
	t.Helper()
	written, body := readReport(t, out)
	if written.Outcome != report.Outcome || written.Head != report.Head || written.Status != "review" || written.Job == nil || !written.Job.Recorded {
		t.Fatalf("written report = %+v, want what Run returned", written)
	}
	return body
}

func assertDoneRunUnit(t *testing.T, run *testRun, branch string, body []byte) {
	t.Helper()
	unit := run.unit(t)
	if unit.Status != "review" || unit.Worktree != run.req.Dir || unit.Branch != branch {
		t.Fatalf("unit = %+v, want review with the workspace and the branch", unit)
	}
	if unit.Evidence == nil || unit.Evidence.Path != filepath.Join(run.req.Out, ReportFile) || unit.Evidence.SHA256 != digestOf(body) || unit.Evidence.Lines != strings.Count(string(body), "\n") {
		t.Fatalf("unit evidence = %+v, want a pointer to the runner's report", unit.Evidence)
	}
	if run.job.started != 1 || run.job.waited != 1 {
		t.Fatalf("job started %d times and waited %d times, want once each", run.job.started, run.job.waited)
	}
}

// A run that stops with a question has made no commit: nothing is published, and the unit is
// blocked with the question on record.
func TestRunRecordsABlockedRunWithoutABranch(t *testing.T) {
	question := "DECISION epitrop-retries: how many | options: a) 3 b) 5 | recommended: a | blocks: unit"
	run := newTestRun(t, &runWork{outcome: OutcomeBlocked, questions: []string{question}})
	before := repoRefs(t, run.repo)
	report, err := Run(slotCtx(t), run.req)
	if err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	assertBlockedRunReport(t, report, question, run.base)
	if after := repoRefs(t, run.repo); after != before {
		t.Fatalf("refs = %q, want them unchanged %q", after, before)
	}
	if unit := run.unit(t); unit.Status != "blocked" || unit.Evidence == nil {
		t.Fatalf("unit = %+v, want blocked with a pointer to the report", unit)
	}
}

func assertBlockedRunReport(t *testing.T, report Report, question, base string) {
	t.Helper()
	if report.Outcome != OutcomeBlocked || report.Status != "blocked" || report.Published || report.Head != base {
		t.Fatalf("report = %+v, want a blocked run at its base, not published", report)
	}
	if len(report.Questions) != 1 || report.Questions[0] != question {
		t.Fatalf("questions = %q, want the run's question", report.Questions)
	}
}

// A failed run that committed is published too: its commits are what the next attempt or a
// reviewer starts from.
func TestRunPublishesTheBranchOfAFailedRun(t *testing.T) {
	run := newTestRun(t, &runWork{outcome: OutcomeFailed, commit: true})
	report, err := Run(slotCtx(t), run.req)
	if err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if report.Outcome != OutcomeFailed || report.Status != "blocked" || !report.Published {
		t.Fatalf("report = %+v, want a failed run, blocked, with its branch published", report)
	}
	if got := gitIn(t, run.repo, "rev-parse", "refs/heads/"+Branch(testUnit)); got != report.Head {
		t.Fatalf("the repository's branch is at %s, want %s", got, report.Head)
	}
}

func TestRunRecordsARefusedStartAsBlocked(t *testing.T) {
	run := newTestRun(t, &runWork{outcome: OutcomeDone, commit: true})
	run.job.startErr = errors.Join(ErrNotStarted, errors.New("the scope was refused"))
	before := repoRefs(t, run.repo)
	report, err := Run(slotCtx(t), run.req)
	if err != nil {
		t.Fatalf("Run() = %v, want nil: a refused start is recorded, not an error", err)
	}
	assertRefusedStartReport(t, report)
	if run.job.waited != 0 {
		t.Fatalf("waited %d times for a job that did not start, want 0", run.job.waited)
	}
	if after := repoRefs(t, run.repo); after != before {
		t.Fatalf("refs = %q, want them unchanged %q", after, before)
	}
	written, _ := readReport(t, run.req.Out)
	if unit := run.unit(t); unit.Status != "blocked" || unit.Evidence == nil || written.Outcome != OutcomeRefused {
		t.Fatalf("unit = %+v, report %+v; want blocked with the report of the refusal", unit, written)
	}
}

func assertRefusedStartReport(t *testing.T, report Report) {
	t.Helper()
	if report.Outcome != OutcomeRefused || report.Status != "blocked" || !strings.Contains(report.Reason, "the scope was refused") || report.Taken() || report.Job != nil {
		t.Fatalf("report = %+v, want a refused start with its reason", report)
	}
}

// When the runner cannot tell what became of the job, it records nothing more: the unit
// stays implementing and the error says so.
func TestRunLeavesTheUnitImplementingWhenTheJobIsOutOfSight(t *testing.T) {
	cases := []struct {
		name string
		edit func(*standIn)
		want string
	}{
		{"a start whose outcome is not known", func(j *standIn) { j.startErr = errors.New("the wait for the start ended") }, "start the job"},
		{"a wait that fails", func(j *standIn) { j.waitErr = errors.New("the event log does not replay") }, "wait for the job"},
	}
	for i := 0; i < len(cases); i++ {
		run := newTestRun(t, &runWork{outcome: OutcomeDone, commit: true})
		cases[i].edit(run.job)
		_, err := Run(slotCtx(t), run.req)
		if err == nil || !strings.Contains(err.Error(), cases[i].want) || !strings.Contains(err.Error(), "stays implementing") {
			t.Errorf("%s: Run() = %v, want an error that says the unit stays implementing", cases[i].name, err)
		}
		if unit := run.unit(t); unit.Status != "implementing" || unit.Evidence == nil || unit.Evidence.Path != filepath.Join(run.req.Out, StartFile) {
			t.Errorf("%s: unit = %+v, want implementing with a pointer to %s", cases[i].name, unit, StartFile)
		}
		if _, statErr := os.Lstat(filepath.Join(run.req.Out, ReportFile)); !os.IsNotExist(statErr) {
			t.Errorf("%s: a report was written (%v), want none", cases[i].name, statErr)
		}
		if _, statErr := os.Lstat(run.req.Dir); statErr != nil {
			t.Errorf("%s: the workspace is gone (%v), want it kept", cases[i].name, statErr)
		}
	}
}

// What stops a run before its job starts leaves nothing: no workspace, no output directory
// and no record that the unit is being worked on.
func TestRunLeavesNothingWhenItCannotBegin(t *testing.T) {
	cases := []struct {
		name string
		edit func(*testRun)
		want string
	}{
		{"a configuration that cannot be opened", func(r *testRun) {
			r.req.Open = func(context.Context, Workspace, string) (Job, UnitRecorder, error) {
				return nil, nil, errors.New("planted: no configuration")
			}
		}, "planted: no configuration"},
		{"no job", func(r *testRun) {
			r.req.Open = func(context.Context, Workspace, string) (Job, UnitRecorder, error) { return nil, r.record, nil }
		}, "has no job or no run record"},
		{"no run record", func(r *testRun) {
			r.req.Open = func(context.Context, Workspace, string) (Job, UnitRecorder, error) { return r.job, nil, nil }
		}, "has no job or no run record"},
		{"a failed preflight", func(r *testRun) { r.job.preflightErr = errors.New("planted: the slot is busy") }, "cannot start: planted: the slot is busy"},
		{"a unit that cannot be recorded as implementing", func(r *testRun) {
			r.recorder = failingRecorder{Service: r.record, failStatus: "implementing"}
		}, "planted: the run record is not writable"},
	}
	for i := 0; i < len(cases); i++ {
		run := newTestRun(t, nil)
		cases[i].edit(run)
		_, err := Run(slotCtx(t), run.req)
		if err == nil || !strings.Contains(err.Error(), cases[i].want) {
			t.Errorf("%s: Run() = %v, want an error holding %q", cases[i].name, err, cases[i].want)
		}
		for _, path := range []string{run.req.Dir, run.req.Out} {
			if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
				t.Errorf("%s: %s is there (%v), want nothing left", cases[i].name, path, statErr)
			}
		}
		if run.job.started != 0 {
			t.Errorf("%s: the job was started %d times, want 0", cases[i].name, run.job.started)
		}
		if _, showErr := run.record.Show(slotCtx(t), testUnit); showErr == nil {
			t.Errorf("%s: the unit is on record, want no record", cases[i].name)
		}
	}
}

func TestRunRefusesARequestItCannotKeepApart(t *testing.T) {
	cases := []struct {
		name string
		edit func(*testRun)
		want string
	}{
		{"nothing to open", func(r *testRun) { r.req.Open = nil }, "needs a way to open"},
		{"a relative output directory", func(r *testRun) { r.req.Out = "out" }, "absolute paths"},
		{"an output directory inside the workspace", func(r *testRun) { r.req.Out = filepath.Join(r.req.Dir, ".slot", "report") }, "lie inside one another"},
		{"a workspace inside the output directory", func(r *testRun) { r.req.Out = filepath.Dir(r.req.Dir) }, "lie inside one another"},
		{"the workspace as the output directory", func(r *testRun) { r.req.Out = r.req.Dir }, "lie inside one another"},
		{"an output directory that exists", func(r *testRun) {
			if err := os.Mkdir(r.req.Out, 0o750); err != nil {
				t.Fatalf("Mkdir() = %v, want nil", err)
			}
		}, "exists"},
	}
	for i := 0; i < len(cases); i++ {
		run := newTestRun(t, nil)
		cases[i].edit(run)
		_, err := Run(slotCtx(t), run.req)
		if err == nil || !strings.Contains(err.Error(), cases[i].want) {
			t.Errorf("%s: Run() = %v, want an error holding %q", cases[i].name, err, cases[i].want)
		}
		if _, statErr := os.Lstat(run.req.Dir); !os.IsNotExist(statErr) {
			t.Errorf("%s: the workspace was made (%v), want nothing prepared", cases[i].name, statErr)
		}
	}
	// A sibling whose name only starts like the workspace's is not inside it.
	run := newTestRun(t, &runWork{outcome: OutcomeBlocked})
	run.req.Out = run.req.Dir + "-out"
	if _, err := Run(slotCtx(t), run.req); err != nil {
		t.Fatalf("Run(output directory beside the workspace) = %v, want nil", err)
	}
}

// When the final record fails, the report is written and the error names the run's end.
func TestRunSaysWhenTheFinalRecordFails(t *testing.T) {
	run := newTestRun(t, &runWork{outcome: OutcomeDone, commit: true})
	run.recorder = failingRecorder{Service: run.record, failStatus: "review"}
	report, err := Run(slotCtx(t), run.req)
	if err == nil || !strings.Contains(err.Error(), "ended as done, and its record failed") {
		t.Fatalf("Run() = %v, want an error naming the run's end", err)
	}
	if written, _ := readReport(t, run.req.Out); written.Outcome != OutcomeDone || report.Outcome != OutcomeDone {
		t.Fatalf("report = %+v, written %+v; want the done run reported", report, written)
	}
	if unit := run.unit(t); unit.Status != "implementing" {
		t.Fatalf("unit = %+v, want it still implementing", unit)
	}
}

// A result the runner does not take changes nothing in the repository: no branch, no ref of
// the runner's own. The unit is blocked and the report says why.
func TestRunTakesNoResultItCannotStandBehind(t *testing.T) {
	spoil := func(name string, body string) func(Workspace) {
		return func(ws Workspace) { plant(t, filepath.Join(ws.Dir, filepath.FromSlash(name)), body) }
	}
	cases := []struct {
		name string
		work runWork
		want string
	}{
		{"no result", runWork{outcome: OutcomeDone, commit: true, after: func(ws Workspace) {
			if err := os.Remove(filepath.Join(ws.Dir, filepath.FromSlash(ResultFile))); err != nil {
				t.Fatalf("Remove() = %v, want nil", err)
			}
		}}, "result.json"},
		{"a result that breaks the schema", runWork{outcome: OutcomeDone, commit: true, after: spoil(ResultFile, `{"unit": "epitrop:unit-1", "outcome": "finished", "summary": "x"}`)}, "is not valid"},
		{"a head the branch is not at", runWork{outcome: OutcomeDone, commit: true, head: strings.Repeat("0a", 20)}, "the result names head"},
		{"evidence that changed after the result was written", runWork{outcome: OutcomeDone, commit: true, evidence: true, after: spoil(EvidenceDir+"/tests/unit.log", "not ok 1\nok 2\n")}, "sha256"},
		{"a branch that does not descend from its base", runWork{outcome: OutcomeFailed, after: func(ws Workspace) {
			tree := gitIn(t, ws.Dir, "rev-parse", "HEAD^{tree}")
			orphan := gitIn(t, ws.Dir, "commit-tree", "-m", "no parent", tree)
			gitIn(t, ws.Dir, "update-ref", "refs/heads/"+ws.Branch, orphan)
		}}, "does not descend from its base"},
		{"a branch that was removed", runWork{outcome: OutcomeFailed, after: func(ws Workspace) {
			gitIn(t, ws.Dir, "switch", "--quiet", "--detach")
			gitIn(t, ws.Dir, "branch", "--quiet", "--delete", "--force", ws.Branch)
		}}, "from the workspace"},
	}
	for i := 0; i < len(cases); i++ {
		checkNotTaken(t, cases[i].name, cases[i].work, cases[i].want)
	}
}

// checkNotTaken runs work and requires that the run is recorded as not-taken with a reason
// holding want: nothing of the result is in the report, no ref of the repository changed,
// and the unit is blocked with a pointer to the report.
func checkNotTaken(t *testing.T, name string, work runWork, want string) {
	t.Helper()
	checkRunNotTaken(t, name, newTestRun(t, &work), want)
}

// checkRunNotTaken is checkNotTaken for a prepared run.
func checkRunNotTaken(t *testing.T, name string, run *testRun, want string) {
	t.Helper()
	before := repoRefs(t, run.repo)
	report, err := Run(slotCtx(t), run.req)
	if err != nil {
		t.Errorf("%s: Run() = %v, want nil: the run is recorded", name, err)
		return
	}
	assertRunNotTakenReport(t, name, report, want)
	if after := repoRefs(t, run.repo); after != before {
		t.Errorf("%s: refs = %q, want them unchanged %q", name, after, before)
	}
	assertRunNotTakenUnitAndWritten(t, name, run)
}

func assertRunNotTakenReport(t *testing.T, name string, report Report, want string) {
	t.Helper()
	if report.Outcome != OutcomeNotTaken || report.Status != "blocked" || !strings.Contains(report.Reason, want) {
		t.Errorf("%s: report = %+v, want not-taken with a reason holding %q", name, report, want)
	}
	if report.Taken() || report.Published || report.Head != "" || report.Result != nil || len(report.Evidence) != 0 || report.Summary != "" {
		t.Errorf("%s: report = %+v, want nothing of the result carried", name, report)
	}
}

func assertRunNotTakenUnitAndWritten(t *testing.T, name string, run *testRun) {
	t.Helper()
	if unit := run.unit(t); unit.Status != "blocked" || unit.Evidence == nil {
		t.Errorf("%s: unit = %+v, want blocked with a pointer to the report", name, unit)
	}
	if written, _ := readReport(t, run.req.Out); written.Outcome != OutcomeNotTaken || written.Result != nil || len(written.Evidence) != 0 {
		t.Errorf("%s: written report = %+v, want the same refusal", name, written)
	}
}

// The repository's own branch of the unit's name is never forced: one that holds other
// commits, or that is checked out, stays as it is.
func TestRunNeverForcesTheRepositorysBranch(t *testing.T) {
	branch := Branch(testUnit)
	cases := []struct {
		name    string
		prepare func(t *testing.T, repo string, base string)
	}{
		{"a branch with other commits", func(t *testing.T, repo string, base string) {
			gitIn(t, repo, "switch", "--quiet", "--create", branch, base)
			gitIn(t, repo, "commit", "--quiet", "--allow-empty", "--message", "somebody else's commit")
			gitIn(t, repo, "switch", "--quiet", "--detach", base)
		}},
		{"a branch that is checked out", func(t *testing.T, repo string, base string) {
			gitIn(t, repo, "switch", "--quiet", "--create", branch, base)
		}},
	}
	for i := 0; i < len(cases); i++ {
		run := newTestRun(t, &runWork{outcome: OutcomeDone, commit: true})
		cases[i].prepare(t, run.repo, run.base)
		before := repoRefs(t, run.repo)
		report, err := Run(slotCtx(t), run.req)
		if err != nil {
			t.Errorf("%s: Run() = %v, want nil", cases[i].name, err)
			continue
		}
		if report.Outcome != OutcomeNotTaken || report.Published || !strings.Contains(report.Reason, "the repository does not take") {
			t.Errorf("%s: report = %+v, want not-taken: the repository does not take the branch", cases[i].name, report)
		}
		if after := repoRefs(t, run.repo); after != before {
			t.Errorf("%s: refs = %q, want them unchanged %q", cases[i].name, after, before)
		}
	}
	assertAncestorBranchMovesForward(t, branch)
}

func assertAncestorBranchMovesForward(t *testing.T, branch string) {
	t.Helper()
	// A branch that is an ancestor of the run's head moves forward.
	run := newTestRun(t, &runWork{outcome: OutcomeDone, commit: true})
	gitIn(t, run.repo, "branch", "--quiet", branch, run.base)
	report, err := Run(slotCtx(t), run.req)
	if err != nil || report.Outcome != OutcomeDone || !report.Published {
		t.Fatalf("Run(branch at the base) = %+v, %v; want it moved forward", report, err)
	}
	if got := gitIn(t, run.repo, "rev-parse", "refs/heads/"+branch); got != report.Head {
		t.Fatalf("the repository's branch is at %s, want %s", got, report.Head)
	}
}

// After a run, the workspace's git directory is the run's own writing. Nothing in it is run
// when the runner takes the branch: neither a setting that names a command nor a hook.
func TestRunRunsNothingTheWorkspaceNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the slot sandbox does not run on Windows")
	}
	marks := t.TempDir()
	plant := func(ws Workspace) {
		plantWorkspaceSettingsAndHooks(t, ws, marks)
	}
	run := newTestRun(t, &runWork{outcome: OutcomeDone, commit: true, evidence: true, after: plant})
	report, err := Run(slotCtx(t), run.req)
	if err != nil || report.Outcome != OutcomeDone || !report.Published {
		t.Fatalf("Run() = %+v, %v; want the run taken", report, err)
	}
	assertNoWorkspaceCommandsRan(t, marks)
	// The other direction: what was planted does run when a git command is run inside the
	// workspace, so its silence above is the runner's doing.
	gitIn(t, run.req.Dir, "status", "--porcelain")
	if _, err = os.Lstat(filepath.Join(marks, "fsmonitor")); err != nil {
		t.Fatalf("a git command inside the workspace ran nothing of it (%v): the test plants nothing that could run", err)
	}
}

func plantWorkspaceSettingsAndHooks(t *testing.T, ws Workspace, marks string) {
	t.Helper()
	settings := map[string]string{
		"uploadpack.packObjectsHook": "pack",
		"core.fsmonitor":             "fsmonitor",
		"core.sshCommand":            "ssh",
		"core.pager":                 "pager",
		"core.editor":                "editor",
		"core.alternateRefsCommand":  "alternates",
	}
	for key, name := range settings {
		gitIn(t, ws.Dir, "config", key, "touch "+filepath.Join(marks, name))
	}
	hooks := filepath.Join(ws.Dir, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	hookNames := []string{"pre-auto-gc", "reference-transaction", "post-checkout", "pre-push", "post-update", "pre-receive", "update"}
	for i := 0; i < len(hookNames); i++ {
		hook := hookNames[i]
		script := "#!/bin/sh\ntouch " + filepath.Join(marks, "hook-"+hook) + "\n"
		// #nosec G306 -- a hook has to be executable to run; the test proves it does not.
		if err := os.WriteFile(filepath.Join(hooks, hook), []byte(script), 0o700); err != nil {
			t.Fatalf("WriteFile(hook) = %v, want nil", err)
		}
	}
}

func assertNoWorkspaceCommandsRan(t *testing.T, marks string) {
	t.Helper()
	ran, err := os.ReadDir(marks)
	if err != nil {
		t.Fatalf("ReadDir() = %v, want nil", err)
	}
	if len(ran) != 0 {
		names := make([]string, 0, len(ran))
		for i := 0; i < len(ran); i++ {
			names = append(names, ran[i].Name())
		}
		t.Fatalf("commands of the workspace ran: %q, want none", names)
	}
}

// A branch git would not take as this unit's work is not taken by the runner either: one
// whose objects git's own check refuses, and one whose ref names a tag instead of a commit.
func TestRunTakesNoBranchThatIsNoSoundCommit(t *testing.T) {
	refused := func(ws Workspace) {
		blob := gitIn(t, ws.Dir, "rev-parse", "HEAD:first.txt")
		tree := gitFed(t, ws.Dir, "100644 blob "+blob+"\t.git\n", "mktree")
		bad := gitIn(t, ws.Dir, "commit-tree", "-p", "HEAD", "-m", "a tree with an entry named .git", tree)
		gitIn(t, ws.Dir, "update-ref", "refs/heads/"+ws.Branch, bad)
	}
	tagged := func(ws Workspace) {
		gitIn(t, ws.Dir, "tag", "--annotate", "--message", "not a commit", "mark")
		gitIn(t, ws.Dir, "symbolic-ref", "refs/heads/"+ws.Branch, "refs/tags/mark")
	}
	checkNotTaken(t, "objects git's check refuses", runWork{outcome: OutcomeFailed, after: refused}, "hasDotgit")
	checkNotTaken(t, "a branch that names a tag", runWork{outcome: OutcomeFailed, after: tagged}, "names a tag, not a commit")
}

// When the runner cannot write a copy, the run is not taken and nothing stays of it: no ref
// of the runner's own, and no word in the report of the copies that were made before.
func TestRunTakesNothingWhenACopyCannotBeWritten(t *testing.T) {
	taken := func(out string) { plant(t, filepath.Join(out, ResultCopy), "{}\n") }
	blocked := func(out string) { plant(t, filepath.Join(out, EvidenceCopies), "a file where the copies go\n") }
	checkNotTaken(t, "a result copy that is there already", runWork{outcome: OutcomeDone, commit: true, inOut: taken}, "file exists")
	want := "not a directory"
	if runtime.GOOS == "windows" {
		want = "The system cannot find the path specified"
	}
	checkNotTaken(t, "no place for the evidence copies", runWork{outcome: OutcomeDone, commit: true, evidence: true, inOut: blocked}, want)
}

// A report that cannot be written is no record: the run ends with an error, and the unit
// stays implementing, without a pointer to a file the runner did not write.
func TestRunRecordsNothingWithoutItsReport(t *testing.T) {
	work := runWork{outcome: OutcomeDone, commit: true, inOut: func(out string) { plant(t, filepath.Join(out, ReportFile), "{}\n") }}
	run := newTestRun(t, &work)
	_, err := Run(slotCtx(t), run.req)
	if err == nil || !strings.Contains(err.Error(), "file exists") {
		t.Fatalf("Run() = %v, want the error of the report that could not be written", err)
	}
	if unit := run.unit(t); unit.Status != "implementing" || unit.Evidence == nil || unit.Evidence.Path != filepath.Join(run.req.Out, StartFile) {
		t.Fatalf("unit = %+v, want implementing with a pointer to %s", unit, StartFile)
	}
	if body := readFile(t, filepath.Join(run.req.Out, ReportFile)); body != "{}\n" {
		t.Fatalf("the file that was in the report's place = %q, want it as it was", body)
	}
}

// A ref of the runner's own that an earlier run left behind, one that was stopped half-way,
// does not stand in the way: it is the runner's to overwrite, and it is gone afterwards.
func TestRunTakesItsBranchOverALeftoverOfAnEarlierRun(t *testing.T) {
	run := newTestRun(t, &runWork{outcome: OutcomeDone, commit: true})
	tree := gitIn(t, run.repo, "rev-parse", "HEAD^{tree}")
	orphan := gitIn(t, run.repo, "commit-tree", "-m", "left by an earlier run", tree)
	gitIn(t, run.repo, "update-ref", stagedRef(Workspace{Branch: Branch(testUnit)}), orphan)
	report, err := Run(slotCtx(t), run.req)
	if err != nil || report.Outcome != OutcomeDone || !report.Published {
		t.Fatalf("Run() = %+v, %v; want the run taken", report, err)
	}
	if refs := repoRefs(t, run.repo); strings.Contains(refs, "refs/epitrop/") || strings.Contains(refs, orphan) {
		t.Fatalf("refs = %q, want the leftover removed", refs)
	}
}

// The fetch brings the unit's branch and nothing else the run made: no tag, no other branch
// and no FETCH_HEAD. Every ref the runner writes meanwhile is that branch or lies under
// refs/epitrop/, outside the branches; the repository's own hook records them.
func TestRunBringsNothingButTheUnitsBranch(t *testing.T) {
	work := runWork{outcome: OutcomeDone, commit: true, after: func(ws Workspace) {
		gitIn(t, ws.Dir, "tag", "v9")
		gitIn(t, ws.Dir, "tag", "--annotate", "--message", "a release", "v10")
		gitIn(t, ws.Dir, "branch", "--quiet", "other")
	}}
	run := newTestRun(t, &work)
	written := filepath.Join(t.TempDir(), "refs.log")
	hook := "#!/bin/sh\nwhile read -r old new ref; do echo \"$ref\" >>'" + written + "'; done\n"
	// #nosec G306 -- a hook has to be executable to run.
	if err := os.WriteFile(filepath.Join(run.repo, ".git", "hooks", "reference-transaction"), []byte(hook), 0o700); err != nil {
		t.Fatalf("WriteFile(hook) = %v, want nil", err)
	}
	before := repoRefs(t, run.repo)
	report, err := Run(slotCtx(t), run.req)
	if err != nil || report.Outcome != OutcomeDone || !report.Published {
		t.Fatalf("Run() = %+v, %v; want the run taken", report, err)
	}
	branch := "refs/heads/" + Branch(testUnit)
	if after := repoRefs(t, run.repo); after != before+"\n"+branch+" "+report.Head {
		t.Fatalf("refs = %q, want %q and the unit's branch at %s", after, before, report.Head)
	}
	if _, statErr := os.Lstat(filepath.Join(run.repo, ".git", "FETCH_HEAD")); !os.IsNotExist(statErr) {
		t.Fatalf("the repository has a FETCH_HEAD (%v), want none", statErr)
	}
	assertWrittenRefsHook(t, written, branch)
}

func assertWrittenRefsHook(t *testing.T, written, branch string) {
	t.Helper()
	names := strings.Fields(readFile(t, written))
	staged := 0
	for i := 0; i < len(names); i++ {
		if strings.HasPrefix(names[i], "refs/epitrop/slot/") {
			staged++
		} else if names[i] != branch {
			t.Errorf("the runner wrote the ref %s, want only the unit's branch and its own ref", names[i])
		}
	}
	if staged == 0 {
		t.Fatalf("refs written = %q, want the runner's own ref among them: the hook saw nothing", names)
	}
}

// A commit of the run that moves a submodule makes git fetch that submodule in the
// repository, from its remote, unless the fetch is told not to. The runner's fetch reaches
// the workspace and nothing else.
func TestRunFetchesNoSubmoduleOfTheRepository(t *testing.T) {
	sub := t.TempDir()
	gitIn(t, sub, "init", "--quiet")
	gitIn(t, sub, "commit", "--quiet", "--allow-empty", "--message", "the submodule")
	work := runWork{outcome: OutcomeDone, commit: true, stage: func(ws Workspace) {
		gitIn(t, ws.Dir, "update-index", "--cacheinfo", "160000,"+strings.Repeat("0a", 20)+",sub")
	}}
	run := newTestRun(t, &work)
	gitIn(t, run.repo, "-c", "protocol.file.allow=always", "submodule", "--quiet", "add", "--", sub, "sub")
	gitIn(t, run.repo, "commit", "--quiet", "--message", "add the submodule")
	run.req.Base = gitIn(t, run.repo, "rev-parse", "HEAD")
	report, err := Run(slotCtx(t), run.req)
	if err != nil || report.Outcome != OutcomeDone || !report.Published {
		t.Fatalf("Run() = %+v, %v; want the run taken", report, err)
	}
	if moved := gitIn(t, run.repo, "rev-parse", report.Head+":sub"); moved != strings.Repeat("0a", 20) {
		t.Fatalf("the run's commit has the submodule at %s, want it moved: the test plants nothing to fetch", moved)
	}
	if _, statErr := os.Lstat(filepath.Join(run.repo, ".git", "modules", "sub", "FETCH_HEAD")); !os.IsNotExist(statErr) {
		t.Fatalf("the repository's submodule was fetched (%v), want it left alone", statErr)
	}
}

// copyOut reads an evidence file a second time, after the run could have changed it. The
// copy is made only when that reading gives the digest the result names, within the bound.
func TestCopyOutTakesNoEvidenceThatChangedSinceItWasChecked(t *testing.T) {
	large := strings.Repeat("x", MaxEvidenceBytes+1)
	cases := []struct {
		name    string
		body    string
		present bool
		claimed string
		want    string
	}{
		{"another content", "not ok 1\n", true, digestOf([]byte("ok 1\n")), "changed after it was checked"},
		{"a file that is gone", "", false, digestOf(nil), "no such file"},
		{"a file beyond the bound", large, true, digestOf([]byte(large)), "is larger than"},
	}
	for i := 0; i < len(cases); i++ {
		workspace, out := t.TempDir(), t.TempDir()
		entry := Evidence{Path: EvidenceDir + "/unit.log", SHA256: cases[i].claimed}
		file := filepath.Join(workspace, filepath.FromSlash(entry.Path))
		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			t.Fatalf("MkdirAll() = %v, want nil", err)
		}
		if cases[i].present {
			plant(t, file, cases[i].body)
		}
		var report Report
		err := copyOut(workspace, out, Result{Evidence: []Evidence{entry}}, []byte("{}\n"), &report)
		matched := strings.Contains(err.Error(), cases[i].want) || (cases[i].want == "no such file" && errors.Is(err, fs.ErrNotExist))
		if !errors.Is(err, ErrResult) || !matched {
			t.Errorf("%s: copyOut() = %v, want a refused result holding %q", cases[i].name, err, cases[i].want)
		}
		if _, statErr := os.Lstat(filepath.Join(out, EvidenceCopies, "unit.log")); !os.IsNotExist(statErr) {
			t.Errorf("%s: the evidence was copied (%v), want no copy", cases[i].name, statErr)
		}
	}
}

// A fetched branch the runner cannot read is not taken, and the report says which reading
// failed: neither the head nor what the head names is assumed.
func TestRunTakesNoBranchItCannotRead(t *testing.T) {
	real := git
	t.Cleanup(func() { git = real })
	failing := func(command string, last string) {
		git = func(ctx context.Context, dir string, args ...string) (string, error) {
			if len(args) > 0 && args[0] == command && (last == "" || args[len(args)-1] == last) {
				return "", errors.New("planted: git cannot read it")
			}
			return real(ctx, dir, args...)
		}
	}
	failing("rev-parse", stagedRef(Workspace{Branch: Branch(testUnit)}))
	checkNotTaken(t, "a head that cannot be read", runWork{outcome: OutcomeDone, commit: true}, "read the fetched "+Branch(testUnit))
	failing("cat-file", "")
	checkNotTaken(t, "a head whose kind cannot be read", runWork{outcome: OutcomeDone, commit: true}, "read what the fetched "+Branch(testUnit))
}

type preemptBeforeStartRecorder struct {
	UnitRecorder
	preemptReason string
}

func (r preemptBeforeStartRecorder) Record(ctx context.Context, id string, payload units.RecordPayload) (eventlog.Unit, error) {
	u, err := r.UnitRecorder.Record(ctx, id, payload)
	if err != nil {
		return u, err
	}
	if payload.Status == "implementing" {
		if svc, ok := r.UnitRecorder.(*units.Service); ok {
			return svc.Preempt(ctx, id, r.preemptReason, "operator")
		}
	}
	return u, nil
}

func TestRunPreemptedBeforeStart(t *testing.T) {
	run := newTestRun(t, nil)
	run.recorder = preemptBeforeStartRecorder{UnitRecorder: run.record, preemptReason: "priority halt"}
	report, err := Run(slotCtx(t), run.req)
	if err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if report.Outcome != OutcomePreempted {
		t.Fatalf("report.Outcome = %s, want %s", report.Outcome, OutcomePreempted)
	}
	if report.Reason != "priority halt" {
		t.Fatalf("report.Reason = %q, want priority halt", report.Reason)
	}
	if report.Taken() {
		t.Fatal("report.Taken() = true, want false")
	}
	if run.job.started != 0 {
		t.Fatalf("run.job.started = %d, want 0", run.job.started)
	}
	assertPreemptedUnit(t, run, "priority halt")
}

func assertPreemptedUnit(t *testing.T, run *testRun, reason string) {
	t.Helper()
	u := run.unit(t)
	if u.Status != "blocked" {
		t.Fatalf("unit status = %s, want blocked", u.Status)
	}
	if u.Preempted == nil || u.Preempted.Reason != reason {
		t.Fatalf("unit.Preempted = %+v, want %s", u.Preempted, reason)
	}
	if u.Evidence == nil || filepath.Base(u.Evidence.Path) != ReportFile {
		t.Fatalf("unit.Evidence = %+v, want pointer to %s", u.Evidence, ReportFile)
	}
}

func TestRunPreemptedAfterWait(t *testing.T) {
	work := runWork{outcome: OutcomeDone, commit: true, evidence: true}
	run := newTestRun(t, &work)
	origOpen := run.req.Open
	run.req.Open = func(ctx context.Context, ws Workspace, out string) (Job, UnitRecorder, error) {
		job, rec, err := origOpen(ctx, ws, out)
		if err != nil {
			return nil, nil, err
		}
		run.job.work = func() {
			doWork(t, ws, out, work)
			if _, preemptErr := run.record.Preempt(ctx, testUnit, "stopped while waiting", "operator"); preemptErr != nil {
				t.Fatalf("Preempt: %v", preemptErr)
			}
		}
		return job, rec, nil
	}
	report, err := Run(slotCtx(t), run.req)
	if err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if report.Outcome != OutcomePreempted {
		t.Fatalf("report.Outcome = %s, want %s", report.Outcome, OutcomePreempted)
	}
	if report.Taken() {
		t.Fatal("report.Taken() = true, want false")
	}
	if report.Published {
		t.Fatal("report.Published = true, want false (no branch published on preemption)")
	}
	if report.Result != nil {
		t.Fatalf("report.Result = %+v, want nil (no result taken)", report.Result)
	}
	u := run.unit(t)
	if u.Status != "blocked" {
		t.Fatalf("unit status = %s, want blocked", u.Status)
	}
}
