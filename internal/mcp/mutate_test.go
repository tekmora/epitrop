package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tekmora/epitrop/internal/units"
)

// controlledJobs counts the starts and stops it is asked for and fails them with err.
type controlledJobs struct {
	fakeJobs
	started, stopped []string
	err              error
	deadlineLeft     bool
}

func (c *controlledJobs) Start(ctx context.Context, name string) error {
	c.started = append(c.started, name)
	c.noteDeadline(ctx)
	return c.err
}

func (c *controlledJobs) Stop(ctx context.Context, name string) error {
	c.stopped = append(c.stopped, name)
	c.noteDeadline(ctx)
	return c.err
}

// noteDeadline records whether the call may run longer than a read-only call.
func (c *controlledJobs) noteDeadline(ctx context.Context) {
	deadline, ok := ctx.Deadline()
	c.deadlineLeft = ok && time.Until(deadline) > CallTimeout
}

// mutable connects a server with the mutating tools over one event log.
func mutable(t *testing.T) (*sdkmcp.ClientSession, *controlledJobs, *units.Service) {
	t.Helper()
	svc, ledger := testLog(t, t.TempDir())
	jobs := &controlledJobs{}
	cs := connect(t, Sources{Jobs: jobs, Units: svc, Mutate: &Mutators{Jobs: jobs, Units: svc, Effects: ledger}})
	return cs, jobs, svc
}

func toolNames(t *testing.T, cs *sdkmcp.ClientSession) map[string]*sdkmcp.Tool {
	t.Helper()
	res, err := cs.ListTools(testContext(t), nil)
	if err != nil {
		t.Fatalf("ListTools() = %v, want nil", err)
	}
	out := make(map[string]*sdkmcp.Tool, len(res.Tools))
	for i := 0; i < len(res.Tools); i++ {
		out[res.Tools[i].Name] = res.Tools[i]
	}
	return out
}

func TestMutatingToolsOnlyWithMutate(t *testing.T) {
	readOnlyTools := toolNames(t, connect(t, Sources{Jobs: &fakeJobs{}, Units: testUnits(t)}))
	cs, _, _ := mutable(t)
	all := toolNames(t, cs)
	for _, name := range []string{ToolJobsStart, ToolJobsStop, ToolUnitSet} {
		if _, ok := readOnlyTools[name]; ok {
			t.Fatalf("server without Mutate lists %s", name)
		}
		tool, ok := all[name]
		if !ok {
			t.Fatalf("server with Mutate does not list %s", name)
		}
		assertMutatingAnnotations(t, tool, name == ToolJobsStop)
	}
	if len(all) != len(readOnlyTools)+3 {
		t.Fatalf("server with Mutate lists %d tools, want %d", len(all), len(readOnlyTools)+3)
	}
}

// assertMutatingAnnotations checks that tool is marked as changing state, idempotent and
// closed world, and destructive only when destructive is set.
func assertMutatingAnnotations(t *testing.T, tool *sdkmcp.Tool, destructive bool) {
	t.Helper()
	a := tool.Annotations
	if a == nil || a.ReadOnlyHint || !a.IdempotentHint || a.DestructiveHint == nil || a.OpenWorldHint == nil || *a.OpenWorldHint {
		t.Fatalf("%s annotations = %+v, want mutating, idempotent, closed world", tool.Name, a)
	}
	if *a.DestructiveHint != destructive {
		t.Fatalf("%s destructive = %v, want %v", tool.Name, *a.DestructiveHint, destructive)
	}
}

func TestUnitSetRunsOncePerKey(t *testing.T) {
	cs, _, svc := mutable(t)
	set := map[string]any{"key": "req-1", "id": unitA, "status": "implementing"}
	if out := mutate(t, cs, ToolUnitSet, set); !out.Executed || out.Skipped {
		t.Fatalf("first unit_set = %+v, want executed", out)
	}
	record(t, svc, unitA, "review")
	if out := mutate(t, cs, ToolUnitSet, set); out.Executed || !out.Skipped {
		t.Fatalf("repeated unit_set = %+v, want skipped", out)
	}
	if u := show(t, svc, unitA); u.Status != "review" {
		t.Fatalf("status after the repeat = %q, want review: the repeat ran again", u.Status)
	}
	set["status"] = "landing"
	if out := mutate(t, cs, ToolUnitSet, set); !out.Executed {
		t.Fatalf("unit_set with the same key and other arguments = %+v, want executed", out)
	}
	if u := show(t, svc, unitA); u.Status != "landing" {
		t.Fatalf("status = %q, want landing", u.Status)
	}
}

func TestUnitSetReopensOnlyWhenAsked(t *testing.T) {
	cs, _, svc := mutable(t)
	record(t, svc, unitA, "landed")
	res := call(t, cs, ToolUnitSet, map[string]any{"key": "req-2", "id": unitA, "status": "ready"}, nil)
	if !res.IsError || !strings.Contains(errorText(res), "reopen") {
		t.Fatalf("unit_set on a landed unit = %q (error %v), want a refusal naming reopen", errorText(res), res.IsError)
	}
	if out := mutate(t, cs, ToolUnitSet, map[string]any{"key": "req-3", "id": unitA, "status": "ready", "reopen": true}); !out.Executed {
		t.Fatalf("unit_set with reopen = %+v, want executed", out)
	}
	if u := show(t, svc, unitA); u.Status != "ready" {
		t.Fatalf("status = %q, want ready", u.Status)
	}
}

func TestJobsToolsRunOncePerKey(t *testing.T) {
	cs, jobs, _ := mutable(t)
	for i := 0; i < 2; i++ {
		mutate(t, cs, ToolJobsStart, map[string]any{"key": "start-1", "name": "idle"})
		mutate(t, cs, ToolJobsStop, map[string]any{"key": "stop-1", "name": "idle"})
	}
	if strings.Join(jobs.started, ",") != "idle" || strings.Join(jobs.stopped, ",") != "idle" {
		t.Fatalf("started %v, stopped %v; want idle once each", jobs.started, jobs.stopped)
	}
	if !jobs.deadlineLeft {
		t.Fatalf("a job call ran under %v or less, want MutationTimeout", CallTimeout)
	}
	mutate(t, cs, ToolJobsStart, map[string]any{"key": "start-1", "name": "other"})
	if strings.Join(jobs.started, ",") != "idle,other" {
		t.Fatalf("started %v, want other started under the same key", jobs.started)
	}
}

func TestFailedMutationStaysOpen(t *testing.T) {
	cs, jobs, _ := mutable(t)
	jobs.err = errors.New("planted start failure")
	args := map[string]any{"key": "start-2", "name": "idle"}
	for _, want := range []string{"planted start failure", "open claim"} {
		res := call(t, cs, ToolJobsStart, args, nil)
		if !res.IsError || !strings.Contains(errorText(res), want) {
			t.Fatalf("jobs_start = %q (error %v), want an error with %q", errorText(res), res.IsError, want)
		}
	}
	if len(jobs.started) != 1 {
		t.Fatalf("started %v, want one attempt: a failed key is not run again", jobs.started)
	}
}

func TestMutatingToolsRefuseBadInput(t *testing.T) {
	cs, jobs, svc := mutable(t)
	long := strings.Repeat("k", 129)
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{ToolJobsStart, map[string]any{"key": "k", "name": ""}, "job name is required"},
		{ToolJobsStop, map[string]any{"key": "k", "name": ""}, "job name is required"},
		{ToolJobsStart, map[string]any{"key": "", "name": "idle"}, "invalid key"},
		{ToolJobsStop, map[string]any{"key": "-k", "name": "idle"}, "invalid key"},
		{ToolJobsStart, map[string]any{"key": "a b", "name": "idle"}, "invalid key"},
		{ToolJobsStart, map[string]any{"key": long, "name": "idle"}, "invalid key"},
		{ToolUnitSet, map[string]any{"key": "k", "id": "no id", "status": "implementing"}, "invalid unit id"},
		{ToolUnitSet, map[string]any{"key": "k", "id": unitA, "status": "almost"}, "invalid status"},
		{ToolUnitSet, map[string]any{"key": long, "id": unitA, "status": "implementing"}, "invalid key"},
	}
	for _, tc := range cases {
		res := call(t, cs, tc.tool, tc.args, nil)
		if !res.IsError || !strings.Contains(errorText(res), tc.want) {
			t.Fatalf("%s %v = %q (error %v), want an error with %q", tc.tool, tc.args, errorText(res), res.IsError, tc.want)
		}
	}
	if len(jobs.started)+len(jobs.stopped) != 0 {
		t.Fatalf("started %v, stopped %v; want nothing run", jobs.started, jobs.stopped)
	}
	if st, err := svc.Replay(testContext(t)); err != nil || len(st.Units) != 0 {
		t.Fatalf("Replay() = %d units, %v; want nothing recorded", len(st.Units), err)
	}
	if out := mutate(t, cs, ToolJobsStart, map[string]any{"key": long[:128], "name": "idle"}); !out.Executed {
		t.Fatalf("jobs_start with a 128-byte key = %+v, want executed", out)
	}
}

func mutate(t *testing.T, cs *sdkmcp.ClientSession, tool string, args map[string]any) MutationOut {
	t.Helper()
	var out MutationOut
	call(t, cs, tool, args, &out)
	return out
}

func show(t *testing.T, svc *units.Service, id string) UnitStatusOut {
	t.Helper()
	u, err := svc.Show(testContext(t), id)
	if err != nil {
		t.Fatalf("Show(%s) = %v", id, err)
	}
	return UnitStatusOut{ID: u.ID, Status: u.Status}
}
