package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/slot"
	"github.com/tekmora/epitrop/internal/units"
)

// writeRunFile writes v as name in a new output directory and returns the evidence pointer
// the runner would record for it.
func writeRunFile(t *testing.T, name string, v any) *eventlog.UnitEvidence {
	t.Helper()
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	path := filepath.Join(t.TempDir(), name)
	if err = os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return &eventlog.UnitEvidence{Path: path, SHA256: hex.EncodeToString(sum[:]), Lines: strings.Count(string(body), "\n")}
}

// testRuns records unitA as ended with a report, unitB as started, and unitC with evidence
// that is no run's.
func testRuns(t *testing.T) (*units.Service, slot.Start, slot.Report) {
	t.Helper()
	svc := testUnits(t)
	ctx := testContext(t)
	start := slot.Start{Unit: unitB, Workspace: "/w/b", Branch: "slot/b", Base: "main", Job: "slot-codex", PreparedAt: "2026-10-17T03:00:00Z"}
	report := slot.Report{Unit: unitA, Outcome: slot.OutcomeDone, Status: "review", Workspace: "/w/a", Branch: "slot/a", Base: "main", Published: true, Summary: "done"}
	records := []struct {
		id      string
		payload units.RecordPayload
	}{
		{unitA, units.RecordPayload{Status: "review", Evidence: writeRunFile(t, slot.ReportFile, report)}},
		{unitB, units.RecordPayload{Status: "implementing", Evidence: writeRunFile(t, slot.StartFile, start)}},
		{unitC, units.RecordPayload{Status: "review", Evidence: writeRunFile(t, "notes.json", map[string]string{})}},
	}
	for _, r := range records {
		if _, err := svc.Record(ctx, r.id, r.payload); err != nil {
			t.Fatalf("Record(%s) = %v, want nil", r.id, err)
		}
	}
	return svc, start, report
}

func TestRunsListReadsBackTheRuns(t *testing.T) {
	svc, _, _ := testRuns(t)
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: svc})
	var all RunsListOut
	call(t, cs, ToolRunsList, map[string]any{}, &all)
	if len(all.Runs) != 2 || all.Runs[0].Unit != unitA || all.Runs[1].Unit != unitB {
		t.Fatalf("runs_list = %+v, want %s and %s sorted, without %s", all, unitA, unitB, unitC)
	}
	if all.Runs[0].Phase != RunEnded || all.Runs[0].Status != "review" || filepath.Base(all.Runs[0].Evidence) != slot.ReportFile {
		t.Fatalf("runs_list %s = %+v, want ended, review, its report", unitA, all.Runs[0])
	}
	if all.Runs[1].Phase != RunStarted || all.Runs[1].Status != "implementing" {
		t.Fatalf("runs_list %s = %+v, want started, implementing", unitB, all.Runs[1])
	}
}

func TestRunsListNarrowsToAPhase(t *testing.T) {
	svc, _, _ := testRuns(t)
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: svc})
	var started RunsListOut
	call(t, cs, ToolRunsList, map[string]any{"phase": RunStarted}, &started)
	if len(started.Runs) != 1 || started.Runs[0].Unit != unitB {
		t.Fatalf("runs_list phase=started = %+v, want only %s", started, unitB)
	}

	res := call(t, cs, ToolRunsList, map[string]any{"phase": "running"}, nil)
	if !res.IsError || !strings.Contains(errorText(res), `phase "running"`) {
		t.Fatalf("runs_list phase=running = %v %q, want a tool error naming the phase", res.IsError, errorText(res))
	}
}

func TestRunsListEmptyIsAList(t *testing.T) {
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: testUnits(t)})
	var out RunsListOut
	call(t, cs, ToolRunsList, map[string]any{}, &out)
	if out.Runs == nil || len(out.Runs) != 0 {
		t.Fatalf("runs_list over an empty record = %+v, want an empty list", out)
	}
}

func TestRunStatusReadsTheStartOrTheReport(t *testing.T) {
	svc, start, report := testRuns(t)
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: svc})

	var ended RunStatusOut
	call(t, cs, ToolRunStatus, map[string]any{"unit": unitA}, &ended)
	if ended.Phase != RunEnded || ended.Start != nil || ended.Report == nil || !reflect.DeepEqual(*ended.Report, report) {
		t.Fatalf("run_status %s = %+v, want the report %+v", unitA, ended, report)
	}

	var running RunStatusOut
	call(t, cs, ToolRunStatus, map[string]any{"unit": unitB}, &running)
	if running.Phase != RunStarted || running.Report != nil || running.Start == nil || !reflect.DeepEqual(*running.Start, start) {
		t.Fatalf("run_status %s = %+v, want the start %+v", unitB, running, start)
	}
}

func TestRunStatusRefusals(t *testing.T) {
	svc, _, _ := testRuns(t)
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: svc})
	res := call(t, cs, ToolRunStatus, map[string]any{"unit": unitC}, nil)
	if !res.IsError || !strings.Contains(errorText(res), "points at no slot run") {
		t.Fatalf("run_status %s = %v %q, want a tool error", unitC, res.IsError, errorText(res))
	}

	u, err := svc.Show(testContext(t), unitA)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(u.Evidence.Path, []byte(`{"unit":"changed"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res = call(t, cs, ToolRunStatus, map[string]any{"unit": unitA}, nil)
	if !res.IsError || !strings.Contains(errorText(res), "does not match the recorded digest") {
		t.Fatalf("run_status of a changed report = %v %q, want a digest refusal", res.IsError, errorText(res))
	}

	res = call(t, cs, ToolRunStatus, map[string]any{"unit": "tekmora/epitrop#999"}, nil)
	if !res.IsError {
		t.Fatalf("run_status of an unknown unit = %q, want a tool error", errorText(res))
	}
}
