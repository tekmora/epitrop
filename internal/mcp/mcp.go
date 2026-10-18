// Package mcp serves the run record over the Model Context Protocol. The read-only tools
// report the jobs' state as the supervisor reports it, the work units and the slot runs as
// the event log replays them, and the release watches as configured and seen. The mutating
// tools start and stop jobs and record a unit's status; a server has them only when its
// Sources carry Mutators, and each call runs at most once per key through the effect
// ledger.
package mcp

import (
	"context"
	"fmt"
	"sort"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/supervisor"
	"github.com/tekmora/epitrop/internal/units"
)

// ServerName is the name the server reports to a client.
const ServerName = "epitrop"

// CallTimeout bounds one tool call's reads; the event log refuses a read without one.
const CallTimeout = 30 * time.Second

// Tool names; a client sees them prefixed with the server's name.
const (
	ToolJobsStatus = "jobs_status"
	ToolUnitsList  = "units_list"
	ToolUnitStatus = "unit_status"
)

// JobLister reports the configured jobs' state; *supervisor.Supervisor is one.
type JobLister interface {
	Status(ctx context.Context, name string) ([]supervisor.JobStatus, error)
}

// UnitReader replays the work units; *units.Service is one.
type UnitReader interface {
	Replay(ctx context.Context) (eventlog.State, error)
	Show(ctx context.Context, id string) (eventlog.Unit, error)
}

// Sources are what the tools read, and with Mutate set, what they change.
type Sources struct {
	Jobs  JobLister
	Units UnitReader
	// Watches, when nil, makes the release watch tools answer that the server reads none.
	Watches WatchReader
	// Mutate, when not nil, adds the mutating tools.
	Mutate *Mutators
}

// NewServer returns a server with every read-only tool registered over src, and the
// mutating tools when src.Mutate is set.
func NewServer(version string, src Sources) *sdkmcp.Server {
	s := sdkmcp.NewServer(&sdkmcp.Implementation{Name: ServerName, Version: version}, nil)
	Register(s, src)
	if src.Mutate != nil {
		RegisterMutating(s, *src.Mutate)
	}
	return s
}

// Register adds the read-only tools over src to s.
func Register(s *sdkmcp.Server, src Sources) {
	sdkmcp.AddTool(s, readOnly(ToolJobsStatus, "List the configured jobs with their state, process id, start time, restarts, sandbox and, for a job with a heartbeat, its last beat and whether it is stale. An empty name lists every job."), jobsStatus(src.Jobs))
	sdkmcp.AddTool(s, readOnly(ToolUnitsList, "List the work units of the run record, sorted by id. A status narrows the list to units with that status."), unitsList(src.Units))
	sdkmcp.AddTool(s, readOnly(ToolUnitStatus, "Show one work unit of the run record by id."), unitStatus(src.Units))
	registerWatches(s, src.Watches)
	registerRuns(s, src.Units)
}

// readOnly describes a tool that changes nothing outside the server.
func readOnly(name, description string) *sdkmcp.Tool {
	closed := false
	return &sdkmcp.Tool{
		Name:        name,
		Description: description,
		Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closed},
	}
}

// JobsStatusIn selects the jobs to list.
type JobsStatusIn struct {
	Name string `json:"name,omitempty" jsonschema:"job name; empty lists every job"`
}

// Job is one job's state.
type Job struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	PID      int    `json:"pid,omitempty"`
	Since    string `json:"since,omitempty"`
	Restarts int    `json:"restarts"`
	Sandbox  string `json:"sandbox,omitempty"`
	// Stale is set for a running job whose heartbeat is older than it allows; LastBeat is
	// its last beat, empty for a job without a heartbeat.
	Stale    bool   `json:"stale,omitempty"`
	LastBeat string `json:"last_beat,omitempty"`
}

// JobsStatusOut lists the jobs.
type JobsStatusOut struct {
	Jobs []Job `json:"jobs"`
}

func jobsStatus(jobs JobLister) sdkmcp.ToolHandlerFor[JobsStatusIn, JobsStatusOut] {
	return func(ctx context.Context, _ *sdkmcp.CallToolRequest, in JobsStatusIn) (*sdkmcp.CallToolResult, JobsStatusOut, error) {
		ctx, cancel := context.WithTimeout(ctx, CallTimeout)
		defer cancel()
		statuses, err := jobs.Status(ctx, in.Name)
		if err != nil {
			return nil, JobsStatusOut{}, fmt.Errorf("%s: %w", ToolJobsStatus, err)
		}
		out := JobsStatusOut{Jobs: make([]Job, 0, len(statuses))}
		for i := 0; i < len(statuses); i++ {
			out.Jobs = append(out.Jobs, jobOf(statuses[i]))
		}
		return nil, out, nil
	}
}

func jobOf(st supervisor.JobStatus) Job {
	return Job{Name: st.Name, State: string(st.State), PID: st.PID, Since: st.Since, Restarts: st.Restarts, Sandbox: st.Sandbox, Stale: st.Stale, LastBeat: st.LastBeat}
}

// jobsOf keys the jobs by name.
func jobsOf(statuses []supervisor.JobStatus) map[string]Job {
	out := make(map[string]Job, len(statuses))
	for i := 0; i < len(statuses); i++ {
		out[statuses[i].Name] = jobOf(statuses[i])
	}
	return out
}

// UnitsListIn narrows the list.
type UnitsListIn struct {
	Status string `json:"status,omitempty" jsonschema:"unit status; empty lists every unit"`
}

// UnitSummary is one unit in a list.
type UnitSummary struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Branch    string `json:"branch,omitempty"`
	PR        int    `json:"pr,omitempty"`
	Lane      string `json:"lane,omitempty"`
	Node      string `json:"node,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// UnitsListOut lists the units.
type UnitsListOut struct {
	Units []UnitSummary `json:"units"`
}

func unitsList(reader UnitReader) sdkmcp.ToolHandlerFor[UnitsListIn, UnitsListOut] {
	return func(ctx context.Context, _ *sdkmcp.CallToolRequest, in UnitsListIn) (*sdkmcp.CallToolResult, UnitsListOut, error) {
		ctx, cancel := context.WithTimeout(ctx, CallTimeout)
		defer cancel()
		st, err := reader.Replay(ctx)
		if err != nil {
			return nil, UnitsListOut{}, fmt.Errorf("%s: %w", ToolUnitsList, err)
		}
		out := UnitsListOut{Units: make([]UnitSummary, 0, len(st.Units))}
		for _, u := range st.Units {
			if in.Status != "" && u.Status != in.Status {
				continue
			}
			out.Units = append(out.Units, summarize(u))
		}
		sort.Slice(out.Units, func(i, j int) bool { return out.Units[i].ID < out.Units[j].ID })
		return nil, out, nil
	}
}

func summarize(u eventlog.Unit) UnitSummary {
	return UnitSummary{ID: u.ID, Status: u.Status, Branch: u.Branch, PR: u.PR, Lane: u.Lane, Node: u.Node, UpdatedAt: u.UpdatedAt}
}

// UnitStatusIn names the unit.
type UnitStatusIn struct {
	ID string `json:"id" jsonschema:"unit id"`
}

// UnitStatusOut is one unit. Identity is left out: it is stored as the recorder gave it,
// and identity_key names it.
type UnitStatusOut struct {
	ID            string                   `json:"id"`
	Status        string                   `json:"status"`
	Worktree      string                   `json:"worktree,omitempty"`
	Branch        string                   `json:"branch,omitempty"`
	PR            int                      `json:"pr,omitempty"`
	Lane          string                   `json:"lane,omitempty"`
	By            string                   `json:"by,omitempty"`
	Target        string                   `json:"target,omitempty"`
	ResolvedModel string                   `json:"resolved_model,omitempty"`
	IdentityKey   string                   `json:"identity_key,omitempty"`
	UpdatedAt     string                   `json:"updated_at,omitempty"`
	Node          string                   `json:"node,omitempty"`
	UnreadNotes   int                      `json:"unread_notes"`
	Evidence      *eventlog.UnitEvidence   `json:"evidence,omitempty"`
	Preempted     *eventlog.UnitPreemption `json:"preempted,omitempty"`
	Released      *eventlog.UnitRelease    `json:"released,omitempty"`
}

func unitStatus(reader UnitReader) sdkmcp.ToolHandlerFor[UnitStatusIn, UnitStatusOut] {
	return func(ctx context.Context, _ *sdkmcp.CallToolRequest, in UnitStatusIn) (*sdkmcp.CallToolResult, UnitStatusOut, error) {
		ctx, cancel := context.WithTimeout(ctx, CallTimeout)
		defer cancel()
		u, err := reader.Show(ctx, in.ID)
		if err != nil {
			return nil, UnitStatusOut{}, fmt.Errorf("%s: %w", ToolUnitStatus, err)
		}
		return nil, UnitStatusOut{
			ID: u.ID, Status: u.Status, Worktree: u.Worktree, Branch: u.Branch, PR: u.PR, Lane: u.Lane,
			By: u.By, Target: u.Target, ResolvedModel: u.ResolvedModel, IdentityKey: u.IdentityKey,
			UpdatedAt: u.UpdatedAt, Node: u.Node, UnreadNotes: len(units.UnreadNotes(u)),
			Evidence: u.Evidence, Preempted: u.Preempted, Released: u.Released,
		}, nil
	}
}
