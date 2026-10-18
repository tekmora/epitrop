package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/effects"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/units"
)

// Tools that change the run record or the jobs. The server registers them only when the
// configuration allows mutations; their names are fixed like the read-only ones.
const (
	ToolJobsStart = "jobs_start"
	ToolJobsStop  = "jobs_stop"
	ToolUnitSet   = "unit_set"
)

// MutationTimeout bounds one mutating call. A stop waits for the job's grace period, at
// most config.MaxStopGraceSeconds, and then for the kill.
const MutationTimeout = time.Duration(config.MaxStopGraceSeconds)*time.Second + time.Minute

// argsDigestLen is how many hex digits of the arguments' digest the action type carries.
const argsDigestLen = 16

// JobControl starts and stops a configured job; *supervisor.Supervisor is one.
type JobControl interface {
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
}

// UnitWriter records a unit's status; *units.Service is one.
type UnitWriter interface {
	Record(ctx context.Context, id string, payload units.RecordPayload) (eventlog.Unit, error)
}

// EffectRunner runs a side effect at most once per key; *effects.Ledger is one.
type EffectRunner interface {
	Do(ctx context.Context, key effects.Key, fn func(context.Context) error) (effects.Result, error)
}

// Mutators are what the mutating tools change, and the ledger that makes each call
// idempotent.
type Mutators struct {
	Jobs    JobControl
	Units   UnitWriter
	Effects EffectRunner
}

// RegisterMutating adds the mutating tools over mut to s. Each call carries a key that
// names the request: the effect ledger runs a key with the same arguments at most once,
// and a repeat reports skipped. The same key with other arguments is another request.
func RegisterMutating(s *sdkmcp.Server, mut Mutators) {
	sdkmcp.AddTool(s, mutating(ToolJobsStart, "Start one configured job. A job that runs already is left alone.", false), jobsStart(mut))
	sdkmcp.AddTool(s, mutating(ToolJobsStop, "Stop one configured job: its stop signal, the grace period, then a kill.", true), jobsStop(mut))
	sdkmcp.AddTool(s, mutating(ToolUnitSet, "Record a work unit's status in the run record.", false), unitSet(mut))
}

// mutating describes a tool that changes state outside the server and is idempotent by
// its key.
func mutating(name, description string, destructive bool) *sdkmcp.Tool {
	closed := false
	return &sdkmcp.Tool{
		Name:        name,
		Description: description,
		Annotations: &sdkmcp.ToolAnnotations{DestructiveHint: &destructive, IdempotentHint: true, OpenWorldHint: &closed},
	}
}

// MutationOut says what the call did.
type MutationOut struct {
	// Executed is true when this call made the change.
	Executed bool `json:"executed"`
	// Skipped is true when an earlier call with the same key and arguments made it.
	Skipped bool `json:"skipped"`
}

// JobIn names the job to start or stop.
type JobIn struct {
	Key  string `json:"key" jsonschema:"idempotency key of this request: letters, digits, dot, underscore, hyphen; at most 128"`
	Name string `json:"name" jsonschema:"job name"`
}

// UnitSetIn is the status to record.
type UnitSetIn struct {
	Key    string `json:"key" jsonschema:"idempotency key of this request: letters, digits, dot, underscore, hyphen; at most 128"`
	ID     string `json:"id" jsonschema:"unit id"`
	Status string `json:"status" jsonschema:"unit status"`
	Reopen bool   `json:"reopen,omitempty" jsonschema:"reopen a unit that is done"`
}

func jobsStart(mut Mutators) sdkmcp.ToolHandlerFor[JobIn, MutationOut] {
	return func(ctx context.Context, _ *sdkmcp.CallToolRequest, in JobIn) (*sdkmcp.CallToolResult, MutationOut, error) {
		if in.Name == "" {
			return nil, MutationOut{}, fmt.Errorf("%s: a job name is required", ToolJobsStart)
		}
		out, err := once(ctx, mut.Effects, ToolJobsStart, in.Key, in.Name, func(ctx context.Context) error {
			return mut.Jobs.Start(ctx, in.Name)
		})
		return nil, out, err
	}
}

func jobsStop(mut Mutators) sdkmcp.ToolHandlerFor[JobIn, MutationOut] {
	return func(ctx context.Context, _ *sdkmcp.CallToolRequest, in JobIn) (*sdkmcp.CallToolResult, MutationOut, error) {
		if in.Name == "" {
			return nil, MutationOut{}, fmt.Errorf("%s: a job name is required", ToolJobsStop)
		}
		out, err := once(ctx, mut.Effects, ToolJobsStop, in.Key, in.Name, func(ctx context.Context) error {
			return mut.Jobs.Stop(ctx, in.Name)
		})
		return nil, out, err
	}
}

func unitSet(mut Mutators) sdkmcp.ToolHandlerFor[UnitSetIn, MutationOut] {
	return func(ctx context.Context, _ *sdkmcp.CallToolRequest, in UnitSetIn) (*sdkmcp.CallToolResult, MutationOut, error) {
		if !units.IsValidID(in.ID) {
			return nil, MutationOut{}, fmt.Errorf("%s: invalid unit id %q", ToolUnitSet, in.ID)
		}
		if !units.IsValidStatus(in.Status) {
			return nil, MutationOut{}, fmt.Errorf("%s: invalid status %q", ToolUnitSet, in.Status)
		}
		args := struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Reopen bool   `json:"reopen"`
		}{in.ID, in.Status, in.Reopen}
		out, err := once(ctx, mut.Effects, ToolUnitSet, in.Key, args, func(ctx context.Context) error {
			_, err := mut.Units.Record(ctx, in.ID, units.RecordPayload{Status: in.Status, Reopen: in.Reopen})
			return err
		})
		return nil, out, err
	}
}

// once runs fn under the effect key of the client's key, the tool and a digest of args,
// within MutationTimeout.
func once(ctx context.Context, ledger EffectRunner, tool, key string, args any, fn func(context.Context) error) (MutationOut, error) {
	action, err := actionType(tool, args)
	if err != nil {
		return MutationOut{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, MutationTimeout)
	defer cancel()
	// The ledger refuses a key outside effects.Key.Valid before fn runs.
	res, err := ledger.Do(ctx, effects.Key{TaskID: key, ActionType: action}, fn)
	if err != nil {
		return MutationOut{}, fmt.Errorf("%s: %w", tool, err)
	}
	return MutationOut{Executed: res.Executed, Skipped: res.Skipped}, nil
}

// actionType is the tool's name and a digest of its arguments, so that one key with other
// arguments is not mistaken for a repeat.
func actionType(tool string, args any) (string, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("%s: arguments: %w", tool, err)
	}
	sum := sha256.Sum256(body)
	return tool + "." + hex.EncodeToString(sum[:])[:argsDigestLen], nil
}
