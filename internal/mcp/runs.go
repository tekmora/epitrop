package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/slot"
)

// Tool names of the slot runs; a client sees them prefixed with the server's name.
const (
	ToolRunsList  = "runs_list"
	ToolRunStatus = "run_status"
)

// The phases of a slot run, as the unit's evidence pointer tells them: the runner points at
// its start file before the job starts and at its report once the run has ended.
const (
	RunStarted = "started"
	RunEnded   = "ended"
)

// registerRuns adds the read-only slot run tools over reader to s.
func registerRuns(s *sdkmcp.Server, reader UnitReader) {
	sdkmcp.AddTool(s, readOnly(ToolRunsList, "List the slot runs of the run record, one per unit whose evidence is a run's start file or report, sorted by unit id. A phase, started or ended, narrows the list."), runsList(reader))
	sdkmcp.AddTool(s, readOnly(ToolRunStatus, "Show the slot run of one unit by unit id: its start record while it runs, the runner's report once it has ended. A report whose digest does not match the run record is refused."), runStatus(reader))
}

// runPhase is the phase of the run u points at, or "" when its evidence is no run's.
func runPhase(u eventlog.Unit) string {
	if u.Evidence == nil {
		return ""
	}
	switch filepath.Base(u.Evidence.Path) {
	case slot.StartFile:
		return RunStarted
	case slot.ReportFile:
		return RunEnded
	}
	return ""
}

// RunsListIn narrows the list.
type RunsListIn struct {
	Phase string `json:"phase,omitempty" jsonschema:"run phase, started or ended; empty lists every run"`
}

// RunSummary is one run in a list. Evidence is the path of its start file or report.
type RunSummary struct {
	Unit      string `json:"unit"`
	Phase     string `json:"phase"`
	Status    string `json:"status"`
	Evidence  string `json:"evidence"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// RunsListOut lists the runs.
type RunsListOut struct {
	Runs []RunSummary `json:"runs"`
}

func runsList(reader UnitReader) sdkmcp.ToolHandlerFor[RunsListIn, RunsListOut] {
	return func(ctx context.Context, _ *sdkmcp.CallToolRequest, in RunsListIn) (*sdkmcp.CallToolResult, RunsListOut, error) {
		if in.Phase != "" && in.Phase != RunStarted && in.Phase != RunEnded {
			return nil, RunsListOut{}, fmt.Errorf("%s: phase %q is neither %s nor %s", ToolRunsList, in.Phase, RunStarted, RunEnded)
		}
		ctx, cancel := context.WithTimeout(ctx, CallTimeout)
		defer cancel()
		st, err := reader.Replay(ctx)
		if err != nil {
			return nil, RunsListOut{}, fmt.Errorf("%s: %w", ToolRunsList, err)
		}
		out := RunsListOut{Runs: make([]RunSummary, 0, len(st.Units))}
		for _, u := range st.Units {
			phase := runPhase(u)
			if phase == "" || (in.Phase != "" && phase != in.Phase) {
				continue
			}
			out.Runs = append(out.Runs, RunSummary{Unit: u.ID, Phase: phase, Status: u.Status, Evidence: u.Evidence.Path, UpdatedAt: u.UpdatedAt})
		}
		sort.Slice(out.Runs, func(i, j int) bool { return out.Runs[i].Unit < out.Runs[j].Unit })
		return nil, out, nil
	}
}

// RunStatusIn names the unit.
type RunStatusIn struct {
	Unit string `json:"unit" jsonschema:"unit id"`
}

// RunStatusOut is one run: Start while it runs, Report once it has ended.
type RunStatusOut struct {
	Unit      string       `json:"unit"`
	Phase     string       `json:"phase"`
	Status    string       `json:"status"`
	Evidence  string       `json:"evidence"`
	UpdatedAt string       `json:"updated_at,omitempty"`
	Start     *slot.Start  `json:"start,omitempty"`
	Report    *slot.Report `json:"report,omitempty"`
}

func runStatus(reader UnitReader) sdkmcp.ToolHandlerFor[RunStatusIn, RunStatusOut] {
	return func(ctx context.Context, _ *sdkmcp.CallToolRequest, in RunStatusIn) (*sdkmcp.CallToolResult, RunStatusOut, error) {
		ctx, cancel := context.WithTimeout(ctx, CallTimeout)
		defer cancel()
		u, err := reader.Show(ctx, in.Unit)
		if err != nil {
			return nil, RunStatusOut{}, fmt.Errorf("%s: %w", ToolRunStatus, err)
		}
		phase := runPhase(u)
		if phase == "" {
			return nil, RunStatusOut{}, fmt.Errorf("%s: unit %s points at no slot run", ToolRunStatus, u.ID)
		}
		out := RunStatusOut{Unit: u.ID, Phase: phase, Status: u.Status, Evidence: u.Evidence.Path, UpdatedAt: u.UpdatedAt}
		if phase == RunStarted {
			start, readErr := slot.ReadStart(filepath.Dir(u.Evidence.Path))
			if readErr != nil {
				return nil, RunStatusOut{}, fmt.Errorf("%s: %w", ToolRunStatus, readErr)
			}
			out.Start = &start
			return nil, out, nil
		}
		report, err := slot.ReadPointedReport(*u.Evidence)
		if err != nil {
			return nil, RunStatusOut{}, fmt.Errorf("%s: %w", ToolRunStatus, err)
		}
		out.Report = &report
		return nil, out, nil
	}
}
