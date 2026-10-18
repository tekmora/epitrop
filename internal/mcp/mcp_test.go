package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
	"github.com/jonboulle/clockwork"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tekmora/epitrop/internal/effects"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/supervisor"
	"github.com/tekmora/epitrop/internal/units"
)

const (
	unitA = "tekmora/epitrop#35"
	unitB = "tekmora/epitrop#7"
	unitC = "tekmora/epitrop#120"
)

type fakeJobs struct {
	statuses []supervisor.JobStatus
	err      error
	asked    []string
}

func (f *fakeJobs) Status(_ context.Context, name string) ([]supervisor.JobStatus, error) {
	f.asked = append(f.asked, name)
	return f.statuses, f.err
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testUnits(t *testing.T) *units.Service {
	t.Helper()
	svc, _ := testLog(t, t.TempDir())
	return svc
}

// testLog opens the units service and the effect ledger over one event log in dir.
func testLog(t *testing.T, dir string) (*units.Service, *effects.Ledger) {
	t.Helper()
	clk := clockwork.NewFakeClock()
	seed := make([]byte, 32)
	for i := 0; i < len(seed); i++ {
		seed[i] = byte(i + 7)
	}
	signer, err := corereceipt.NewSignerFromSeed(seed, clk)
	if err != nil {
		t.Fatalf("NewSignerFromSeed() = %v, want nil", err)
	}
	limits := eventlog.Limits{Clock: clk, LockTimeout: time.Second, MaxReplayRecords: 1000}
	svc, err := units.NewService(dir, signer, clk, limits)
	if err != nil {
		t.Fatalf("NewService() = %v, want nil", err)
	}
	ledger, err := effects.NewLedger(dir, signer, clk, limits)
	if err != nil {
		t.Fatalf("NewLedger() = %v, want nil", err)
	}
	return svc, ledger
}

func strPtr(s string) *string { return &s }

// connect serves src on an in-memory transport and returns a connected client session.
func connect(t *testing.T, src Sources) *sdkmcp.ClientSession {
	t.Helper()
	ctx := testContext(t)
	serverT, clientT := sdkmcp.NewInMemoryTransports()
	ss, err := NewServer("test", src).Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server Connect() = %v, want nil", err)
	}
	t.Cleanup(func() { closeSession(t, "server", ss.Close) })
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "test"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client Connect() = %v, want nil", err)
	}
	t.Cleanup(func() { closeSession(t, "client", cs.Close) })
	return cs
}

func closeSession(t *testing.T, side string, closeFn func() error) {
	t.Helper()
	if err := closeFn(); err != nil {
		t.Errorf("%s session Close() = %v, want nil", side, err)
	}
}

// call calls a tool and decodes its structured content into out. It fails the test on a
// protocol error, and on a tool error when out is given; with out nil it returns the result
// so a caller can read IsError.
func call(t *testing.T, cs *sdkmcp.ClientSession, name string, args map[string]any, out any) *sdkmcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(testContext(t), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) = %v, want nil", name, err)
	}
	if out == nil {
		return res
	}
	if res.IsError {
		t.Fatalf("CallTool(%s) = tool error %q, want a result", name, errorText(res))
	}
	body, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content of %s: %v", name, err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode structured content of %s: %v (%s)", name, err, body)
	}
	return res
}

func errorText(res *sdkmcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestToolsListedReadOnly(t *testing.T) {
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: testUnits(t)})
	res, err := cs.ListTools(testContext(t), nil)
	if err != nil {
		t.Fatalf("ListTools() = %v, want nil", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s annotations = %+v, want readOnlyHint true", tool.Name, tool.Annotations)
		}
		if tool.OutputSchema == nil {
			t.Errorf("tool %s has no output schema, want one", tool.Name)
		}
	}
	sort.Strings(names)
	want := []string{ToolJobsStatus, ToolRunStatus, ToolRunsList, ToolUnitStatus, ToolUnitsList, ToolWatchStatus, ToolWatchesList}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
}

func TestJobsStatusMapsEveryField(t *testing.T) {
	jobs := &fakeJobs{statuses: []supervisor.JobStatus{
		{Name: "sync", State: supervisor.StateRunning, PID: 41, ShimPID: 40, Since: "2026-10-17T03:00:00Z", Restarts: 2, Sandbox: "bwrap", Stale: true, LastBeat: "2026-10-17T03:00:05Z"},
		{Name: "watch", State: supervisor.StateStopped},
	}}
	cs := connect(t, Sources{Jobs: jobs, Units: testUnits(t)})
	var out JobsStatusOut
	call(t, cs, ToolJobsStatus, map[string]any{"name": "sync"}, &out)
	want := JobsStatusOut{Jobs: []Job{
		{Name: "sync", State: "running", PID: 41, Since: "2026-10-17T03:00:00Z", Restarts: 2, Sandbox: "bwrap", Stale: true, LastBeat: "2026-10-17T03:00:05Z"},
		{Name: "watch", State: "stopped"},
	}}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("jobs_status = %+v, want %+v", out, want)
	}
	if !reflect.DeepEqual(jobs.asked, []string{"sync"}) {
		t.Fatalf("supervisor asked for %q, want [sync]", jobs.asked)
	}
}

func TestJobsStatusEmptyIsAList(t *testing.T) {
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: testUnits(t)})
	res := call(t, cs, ToolJobsStatus, map[string]any{}, nil)
	body, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(body) != `{"jobs":[]}` {
		t.Fatalf("jobs_status with no jobs = %s, want {\"jobs\":[]}", body)
	}
}

func TestJobsStatusErrorIsToolError(t *testing.T) {
	cs := connect(t, Sources{Jobs: &fakeJobs{err: errors.New("supervisor: unknown job \"nope\"")}, Units: testUnits(t)})
	res := call(t, cs, ToolJobsStatus, map[string]any{"name": "nope"}, nil)
	if !res.IsError || !strings.Contains(errorText(res), `unknown job "nope"`) {
		t.Fatalf("jobs_status error = %v %q, want a tool error naming the job", res.IsError, errorText(res))
	}
}

func TestUnitsListReadsBackRecordedUnits(t *testing.T) {
	svc := testUnits(t)
	ctx := testContext(t)
	records := []struct {
		id      string
		payload units.RecordPayload
	}{
		{unitB, units.RecordPayload{Status: "implementing", Branch: strPtr("feat/b")}},
		{unitA, units.RecordPayload{Status: "review", Branch: strPtr("feat/a")}},
		{unitC, units.RecordPayload{Status: "implementing"}},
	}
	for _, r := range records {
		if _, err := svc.Record(ctx, r.id, r.payload); err != nil {
			t.Fatalf("Record(%s) = %v, want nil", r.id, err)
		}
	}
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: svc})

	var all UnitsListOut
	call(t, cs, ToolUnitsList, map[string]any{}, &all)
	if got := summaryIDs(all); !reflect.DeepEqual(got, []string{unitC, unitA, unitB}) {
		t.Fatalf("units_list ids = %v, want sorted %v", got, []string{unitC, unitA, unitB})
	}
	if all.Units[1].Status != "review" || all.Units[1].Branch != "feat/a" {
		t.Fatalf("units_list %s = %+v, want status review, branch feat/a", unitA, all.Units[1])
	}

	var implementing UnitsListOut
	call(t, cs, ToolUnitsList, map[string]any{"status": "implementing"}, &implementing)
	if got := summaryIDs(implementing); !reflect.DeepEqual(got, []string{unitC, unitB}) {
		t.Fatalf("units_list status=implementing ids = %v, want %v", got, []string{unitC, unitB})
	}

	var none UnitsListOut
	call(t, cs, ToolUnitsList, map[string]any{"status": "landed"}, &none)
	if none.Units == nil || len(none.Units) != 0 {
		t.Fatalf("units_list status=landed = %+v, want an empty list", none)
	}
}

func summaryIDs(out UnitsListOut) []string {
	ids := make([]string, 0, len(out.Units))
	for _, u := range out.Units {
		ids = append(ids, u.ID)
	}
	return ids
}

func TestUnitStatusReadsBackOneUnit(t *testing.T) {
	svc := testUnits(t)
	if _, err := svc.Record(testContext(t), unitA, units.RecordPayload{Status: "implementing", Worktree: strPtr("/w/a"), Lane: strPtr("local")}); err != nil {
		t.Fatalf("Record() = %v, want nil", err)
	}
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: svc})
	var out UnitStatusOut
	call(t, cs, ToolUnitStatus, map[string]any{"id": unitA}, &out)
	if out.ID != unitA || out.Status != "implementing" || out.Worktree != "/w/a" || out.Lane != "local" || out.UnreadNotes != 0 {
		t.Fatalf("unit_status = %+v, want the recorded fields and no unread note", out)
	}
}

// unreadNotes reads the unit's unread note count through the tool.
func unreadNotes(t *testing.T, cs *sdkmcp.ClientSession) int {
	t.Helper()
	var out UnitStatusOut
	call(t, cs, ToolUnitStatus, map[string]any{"id": unitA}, &out)
	return out.UnreadNotes
}

func TestUnitStatusCountsUnreadNotesWithoutReading(t *testing.T) {
	svc := testUnits(t)
	ctx := testContext(t)
	if _, err := svc.Record(ctx, unitA, units.RecordPayload{Status: "implementing"}); err != nil {
		t.Fatalf("Record() = %v, want nil", err)
	}
	if _, err := svc.Note(ctx, unitA, "controller", "rebase on main"); err != nil {
		t.Fatalf("Note() = %v, want nil", err)
	}
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: svc})
	// Reading is not taking: the note stays unread for the unit's own inbox.
	for i := 0; i < 2; i++ {
		if got := unreadNotes(t, cs); got != 1 {
			t.Fatalf("unit_status read %d: unread notes = %d, want 1", i+1, got)
		}
	}
	if _, err := svc.Inbox(ctx, unitA); err != nil {
		t.Fatalf("Inbox() = %v, want nil", err)
	}
	if got := unreadNotes(t, cs); got != 0 {
		t.Fatalf("unit_status after the inbox read: unread notes = %d, want 0", got)
	}
}

func TestUnitStatusRefusals(t *testing.T) {
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: testUnits(t)})
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing unit", map[string]any{"id": unitA}, "does not exist"},
		{"invalid id", map[string]any{"id": "not a unit"}, "invalid unit id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := call(t, cs, ToolUnitStatus, tc.args, nil)
			if !res.IsError || !strings.Contains(errorText(res), tc.want) {
				t.Fatalf("unit_status(%v) = %v %q, want a tool error containing %q", tc.args, res.IsError, errorText(res), tc.want)
			}
		})
	}
}

func TestUnitStatusRequiresID(t *testing.T) {
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: testUnits(t)})
	res, err := cs.CallTool(testContext(t), &sdkmcp.CallToolParams{Name: ToolUnitStatus, Arguments: map[string]any{}})
	if err == nil && !res.IsError {
		t.Fatalf("unit_status without id = %+v, want a refusal", res)
	}
}

func TestUnitsListSortsByID(t *testing.T) {
	svc := testUnits(t)
	ctx := testContext(t)
	want := make([]string, 0, 12)
	for i := 12; i >= 1; i-- {
		id := fmt.Sprintf("tekmora/epitrop#%d", 100+i)
		if _, err := svc.Record(ctx, id, units.RecordPayload{Status: "implementing"}); err != nil {
			t.Fatalf("Record(%s) = %v, want nil", id, err)
		}
		want = append([]string{id}, want...)
	}
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: svc})
	var out UnitsListOut
	call(t, cs, ToolUnitsList, map[string]any{}, &out)
	if got := summaryIDs(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("units_list ids = %v, want %v", got, want)
	}
}
