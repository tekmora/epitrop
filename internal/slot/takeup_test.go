package slot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/units"
)

// T1 takeover equals uninterrupted: two runs from identical runWork{outcome: done, commit: true}.
func TestTakeUpEqualsUninterruptedRun(t *testing.T) {
	work := runWork{outcome: OutcomeDone, commit: true, evidence: true}
	runA := newTestRun(t, &work)
	repA, err := Run(slotCtx(t), runA.req)
	if err != nil {
		t.Fatalf("Run(A) = %v, want nil", err)
	}
	runB := newTestRun(t, &work)
	runB.job.waitErr = errors.New("simulated runner crash after start")
	if _, err = Run(slotCtx(t), runB.req); err == nil {
		t.Fatal("Run(B) = nil, want simulated crash error")
	}
	if unitB := runB.unit(t); unitB.Status != "implementing" {
		t.Fatalf("unit B status = %s, want implementing before take up", unitB.Status)
	}
	standInB2 := &standIn{end: runA.job.end}
	reqTakeUp := TakeUpRequest{
		Repo: runB.req.Repo,
		Out:  runB.req.Out,
		Open: func(_ context.Context, _ string) (Job, UnitRecorder, error) {
			return standInB2, runB.record, nil
		},
	}
	repB, err := TakeUp(slotCtx(t), reqTakeUp)
	if err != nil {
		t.Fatalf("TakeUp(B) = %v, want nil", err)
	}
	unitA := runA.unit(t)
	unitB := runB.unit(t)
	assertTakeUpEqualsRunReportAndTree(t, runA, runB, repA, repB)
	assertTakeUpUnitEvidence(t, runB, unitA, unitB)
}

func assertTakeUpEqualsRunReportAndTree(t *testing.T, runA, runB *testRun, repA, repB Report) {
	t.Helper()
	if repB.Outcome != repA.Outcome || repB.Status != repA.Status || repB.Published != repA.Published || repB.Head == "" {
		t.Fatalf("report B = %+v, want equal to report A %+v", repB, repA)
	}
	// The two runs use separate repositories made at different times, so their commit ids differ
	// whenever the runs cross a second; the published trees must be the same.
	treeA := gitIn(t, runA.repo, "rev-parse", repA.Head+"^{tree}")
	if treeB := gitIn(t, runB.repo, "rev-parse", repB.Head+"^{tree}"); treeB != treeA {
		t.Fatalf("published tree B = %s, want tree A %s (heads %s, %s)", treeB, treeA, repB.Head, repA.Head)
	}
}

func assertTakeUpUnitEvidence(t *testing.T, runB *testRun, unitA, unitB eventlog.Unit) {
	t.Helper()
	if unitB.Status != unitA.Status || unitB.Status != "review" {
		t.Fatalf("unit B status = %s, want %s (review)", unitB.Status, unitA.Status)
	}
	wantPath := filepath.Join(runB.req.Out, ReportFile)
	if unitB.Evidence == nil || unitB.Evidence.Path != wantPath {
		t.Fatalf("unit B evidence pointer = %+v, want path %s", unitB.Evidence, wantPath)
	}
	body, err := os.ReadFile(wantPath) // #nosec G304 -- wantPath is the temporary output file of the test run
	if err != nil {
		t.Fatalf("ReadFile(report B) = %v", err)
	}
	if digestOf(body) != unitB.Evidence.SHA256 {
		t.Fatalf("digest mismatch: file has %s, evidence has %s", digestOf(body), unitB.Evidence.SHA256)
	}
}

// T2 job gone without a valid result: Run killed after start with runWork that leaves no result.
func TestTakeUpJobGoneWithoutValidResult(t *testing.T) {
	work := runWork{
		outcome: OutcomeDone,
		commit:  true,
		after: func(ws Workspace) {
			if err := os.Remove(filepath.Join(ws.Dir, filepath.FromSlash(ResultFile))); err != nil {
				t.Fatalf("Remove(result): %v", err)
			}
		},
	}
	run := newTestRun(t, &work)
	run.job.waitErr = errors.New("simulated runner crash after start")
	if _, err := Run(slotCtx(t), run.req); err == nil {
		t.Fatal("Run() = nil, want simulated crash error")
	}
	freshJob := &standIn{end: JobEnd{Recorded: true}}
	req := TakeUpRequest{
		Repo: run.req.Repo,
		Out:  run.req.Out,
		Open: func(_ context.Context, _ string) (Job, UnitRecorder, error) {
			return freshJob, run.record, nil
		},
	}
	rep, err := TakeUp(slotCtx(t), req)
	if err != nil {
		t.Fatalf("TakeUp() = %v, want nil: not-taken outcome is recorded", err)
	}
	unit := run.unit(t)
	if unit.Status != "blocked" {
		t.Fatalf("unit status = %s, want blocked", unit.Status)
	}
	if rep.Outcome != OutcomeNotTaken || rep.Reason == "" {
		t.Fatalf("report = %+v, want OutcomeNotTaken with non-empty reason", rep)
	}
}

// T3 job alive: TakeUp with an adopter/job whose Wait first reports running then the end.
func TestTakeUpJobAliveWaitsWithoutStarting(t *testing.T) {
	work := runWork{outcome: OutcomeDone, commit: true}
	run := newTestRun(t, &work)
	run.job.waitErr = errors.New("simulated runner crash after start")
	if _, err := Run(slotCtx(t), run.req); err == nil {
		t.Fatal("Run() = nil, want simulated crash error")
	}
	freshJob := &standIn{end: JobEnd{Recorded: true}}
	req := TakeUpRequest{
		Repo: run.req.Repo,
		Out:  run.req.Out,
		Open: func(_ context.Context, _ string) (Job, UnitRecorder, error) {
			return freshJob, run.record, nil
		},
	}
	if _, err := TakeUp(slotCtx(t), req); err != nil {
		t.Fatalf("TakeUp() = %v, want nil", err)
	}
	if freshJob.started != 0 {
		t.Fatalf("freshJob.started = %d, want 0 (TakeUp must not start the job again)", freshJob.started)
	}
	if freshJob.waited != 1 {
		t.Fatalf("freshJob.waited = %d, want 1", freshJob.waited)
	}
	if freshJob.adopted != 1 {
		t.Fatalf("freshJob.adopted = %d, want 1", freshJob.adopted)
	}
}

// T4 negatives: TakeUp refuses (error, unit unchanged, no run.json written).
func TestTakeUpNegatives(t *testing.T) {
	t.Run("missing start.json", func(t *testing.T) {
		run := newKilledRun(t)
		if err := os.Remove(filepath.Join(run.req.Out, StartFile)); err != nil {
			t.Fatalf("Remove(start.json): %v", err)
		}
		checkTakeUpRefusal(t, run, "read start.json")
	})

	t.Run("run.json exists", func(t *testing.T) {
		work := runWork{outcome: OutcomeDone, commit: true}
		run := newTestRun(t, &work)
		if _, err := Run(slotCtx(t), run.req); err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
		freshJob := &standIn{end: JobEnd{Recorded: true}}
		req := TakeUpRequest{
			Repo: run.req.Repo,
			Out:  run.req.Out,
			Open: func(_ context.Context, _ string) (Job, UnitRecorder, error) {
				return freshJob, run.record, nil
			},
		}
		if _, err := TakeUp(slotCtx(t), req); err == nil || !strings.Contains(err.Error(), "the run was recorded already") {
			t.Fatalf("TakeUp(run.json exists) = %v, want recorded already refusal", err)
		}
	})

	t.Run("unit not implementing", func(t *testing.T) {
		run := newKilledRun(t)
		if _, err := run.record.Record(slotCtx(t), testUnit, units.RecordPayload{Status: "blocked"}); err != nil {
			t.Fatalf("Record(blocked): %v", err)
		}
		checkTakeUpRefusal(t, run, "cannot take up unit")
		if unit := run.unit(t); unit.Status != "blocked" {
			t.Fatalf("unit status = %s, want blocked", unit.Status)
		}
	})

	t.Run("worktree mismatch", func(t *testing.T) {
		run := newKilledRun(t)
		otherDir := "/other/workspace/dir"
		if _, err := run.record.Record(slotCtx(t), testUnit, units.RecordPayload{Status: "implementing", Worktree: &otherDir}); err != nil {
			t.Fatalf("Record(other worktree): %v", err)
		}
		checkTakeUpRefusal(t, run, "cannot take up unit")
	})

	t.Run("evidence pointer names another file", func(t *testing.T) {
		run := newKilledRun(t)
		wrongPointer := &eventlog.UnitEvidence{Path: "/other/start.json", SHA256: strings.Repeat("0a", 32), Lines: 1}
		if _, err := run.record.Record(slotCtx(t), testUnit, units.RecordPayload{Status: "implementing", Evidence: wrongPointer}); err != nil {
			t.Fatalf("Record(wrong evidence): %v", err)
		}
		checkTakeUpRefusal(t, run, "cannot take up unit")
	})
}

// TestTakeUpRefusesARequestItCannotCheck refuses a context without a deadline and relative
// paths before it reads anything.
func TestTakeUpRefusesARequestItCannotCheck(t *testing.T) {
	run := newKilledRun(t)
	open := func(_ context.Context, _ string) (Job, UnitRecorder, error) {
		return &standIn{end: JobEnd{Recorded: true}}, run.record, nil
	}
	cases := []struct {
		name string
		ctx  context.Context
		req  TakeUpRequest
		want string
	}{
		{"no deadline", context.Background(), TakeUpRequest{Repo: run.req.Repo, Out: run.req.Out, Open: open}, "slot: take up requires a context deadline"},
		{"relative output directory", slotCtx(t), TakeUpRequest{Repo: run.req.Repo, Out: "out", Open: open}, "absolute paths"},
		{"relative repository", slotCtx(t), TakeUpRequest{Repo: "repo", Out: run.req.Out, Open: open}, "absolute paths"},
	}
	for _, c := range cases {
		if _, err := TakeUp(c.ctx, c.req); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: TakeUp() = %v, want error holding %q", c.name, err, c.want)
		}
	}
	if unit := run.unit(t); unit.Status != "implementing" {
		t.Fatalf("unit status = %s, want implementing", unit.Status)
	}
}

func newKilledRun(t *testing.T) *testRun {
	t.Helper()
	work := runWork{outcome: OutcomeDone, commit: true}
	run := newTestRun(t, &work)
	run.job.waitErr = errors.New("simulated runner crash after start")
	if _, err := Run(slotCtx(t), run.req); err == nil {
		t.Fatal("Run() = nil, want simulated crash error")
	}
	return run
}

func checkTakeUpRefusal(t *testing.T, run *testRun, wantSubstring string) {
	t.Helper()
	freshJob := &standIn{end: JobEnd{Recorded: true}}
	req := TakeUpRequest{
		Repo: run.req.Repo,
		Out:  run.req.Out,
		Open: func(_ context.Context, _ string) (Job, UnitRecorder, error) {
			return freshJob, run.record, nil
		},
	}
	beforeUnit := run.unit(t)
	if _, err := TakeUp(slotCtx(t), req); err == nil || !strings.Contains(err.Error(), wantSubstring) {
		t.Fatalf("TakeUp() = %v, want error holding %q", err, wantSubstring)
	}
	if _, statErr := os.Lstat(filepath.Join(run.req.Out, ReportFile)); !os.IsNotExist(statErr) {
		t.Fatalf("report file was written (%v), want none", statErr)
	}
	afterUnit := run.unit(t)
	if afterUnit.Status != beforeUnit.Status {
		t.Fatalf("unit status moved from %s to %s, want unchanged", beforeUnit.Status, afterUnit.Status)
	}
}

// T6 begin writes start.json before the implementing record.
func TestBeginWritesStartFileBeforeImplementingRecord(t *testing.T) {
	work := runWork{outcome: OutcomeDone, commit: true}
	run := newTestRun(t, &work)
	run.job.waitErr = errors.New("simulated runner crash after start")
	if _, err := Run(slotCtx(t), run.req); err == nil {
		t.Fatal("Run() = nil, want simulated crash error")
	}
	start, err := ReadStart(run.req.Out)
	if err != nil {
		t.Fatalf("ReadStart() = %v, want nil", err)
	}
	assertStartFileContents(t, start, run)
	assertImplementingUnitEvidence(t, run)
}

func assertStartFileContents(t *testing.T, start Start, run *testRun) {
	t.Helper()
	if start.Unit != testUnit {
		t.Fatalf("start.Unit = %q, want %q", start.Unit, testUnit)
	}
	if start.Workspace != run.req.Dir {
		t.Fatalf("start.Workspace = %q, want %q", start.Workspace, run.req.Dir)
	}
	if start.Branch != Branch(testUnit) {
		t.Fatalf("start.Branch = %q, want %q", start.Branch, Branch(testUnit))
	}
	if start.Base != run.base {
		t.Fatalf("start.Base = %q, want %q", start.Base, run.base)
	}
	if start.Job != run.job.Name() {
		t.Fatalf("start.Job = %q, want %q", start.Job, run.job.Name())
	}
	if start.PreparedAt == "" {
		t.Fatal("start.PreparedAt is empty")
	}
}

func assertImplementingUnitEvidence(t *testing.T, run *testRun) {
	t.Helper()
	unit := run.unit(t)
	if unit.Status != "implementing" {
		t.Fatalf("unit.Status = %q, want implementing", unit.Status)
	}
	wantPath := filepath.Join(run.req.Out, StartFile)
	if unit.Evidence == nil || unit.Evidence.Path != wantPath {
		t.Fatalf("unit.Evidence = %+v, want pointer to %s", unit.Evidence, wantPath)
	}
	body, err := os.ReadFile(wantPath) // #nosec G304 -- wantPath is the temporary output file of the test run
	if err != nil {
		t.Fatalf("ReadFile(start.json): %v", err)
	}
	if digestOf(body) != unit.Evidence.SHA256 {
		t.Fatalf("digest mismatch: start.json has %s, unit evidence has %s", digestOf(body), unit.Evidence.SHA256)
	}
}

func TestTakeUpRefusesPreemptedUnit(t *testing.T) {
	work := runWork{outcome: OutcomeDone, commit: true}
	run := newTestRun(t, &work)
	run.job.waitErr = errors.New("simulated runner crash after start")
	if _, err := Run(slotCtx(t), run.req); err == nil {
		t.Fatal("Run() = nil, want crash")
	}
	ctx := slotCtx(t)
	if _, err := run.record.Preempt(ctx, testUnit, "preempted before takeover", "operator"); err != nil {
		t.Fatalf("Preempt: %v", err)
	}
	reqTakeUp := TakeUpRequest{
		Repo: run.req.Repo,
		Out:  run.req.Out,
		Open: func(_ context.Context, _ string) (Job, UnitRecorder, error) {
			return &standIn{end: JobEnd{Recorded: true}}, run.record, nil
		},
	}
	_, err := TakeUp(ctx, reqTakeUp)
	if err == nil || !strings.Contains(err.Error(), "unit was preempted: preempted before takeover") {
		t.Fatalf("TakeUp() = %v, want error mentioning unit was preempted", err)
	}
	if _, statErr := os.Lstat(filepath.Join(run.req.Out, ReportFile)); !os.IsNotExist(statErr) {
		t.Fatalf("report file was written (%v), want none", statErr)
	}
}

type waitWorkJob struct {
	*standIn
	waitWork func()
}

func (j *waitWorkJob) Wait(ctx context.Context) (JobEnd, error) {
	if j.waitWork != nil {
		j.waitWork()
	}
	return j.standIn.Wait(ctx)
}

func TestTakeUpPreemptedAfterWait(t *testing.T) {
	work := runWork{outcome: OutcomeDone, commit: true, evidence: true}
	run := newTestRun(t, &work)
	run.job.waitErr = errors.New("simulated runner crash after start")
	if _, err := Run(slotCtx(t), run.req); err == nil {
		t.Fatal("Run() = nil, want crash")
	}
	ctx := slotCtx(t)
	job2 := &waitWorkJob{
		standIn: &standIn{end: JobEnd{Recorded: true}},
		waitWork: func() {
			if _, preemptErr := run.record.Preempt(ctx, testUnit, "preempted during takeover wait", "operator"); preemptErr != nil {
				t.Fatalf("Preempt: %v", preemptErr)
			}
		},
	}
	reqTakeUp := TakeUpRequest{
		Repo: run.req.Repo,
		Out:  run.req.Out,
		Open: func(_ context.Context, _ string) (Job, UnitRecorder, error) {
			return job2, run.record, nil
		},
	}
	rep, err := TakeUp(ctx, reqTakeUp)
	if err != nil {
		t.Fatalf("TakeUp() = %v, want nil", err)
	}
	if rep.Outcome != OutcomePreempted {
		t.Fatalf("rep.Outcome = %s, want %s", rep.Outcome, OutcomePreempted)
	}
	if rep.Reason != "preempted during takeover wait" {
		t.Fatalf("rep.Reason = %q, want preempted during takeover wait", rep.Reason)
	}
	if rep.Taken() {
		t.Fatal("rep.Taken() = true, want false")
	}
	u := run.unit(t)
	if u.Status != "blocked" {
		t.Fatalf("unit status = %s, want blocked", u.Status)
	}
}
