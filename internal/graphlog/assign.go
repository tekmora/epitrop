package graphlog

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graph"
	"github.com/tekmora/epitrop/internal/units"
)

// Frontier returns the nodes a slot may take next (graph.Frontier) of the graph the log
// holds. It writes nothing.
func (s *Service) Frontier(ctx context.Context) ([]graph.SnapshotNode, error) {
	snap, _, err := s.Show(ctx)
	if err != nil {
		return nil, err
	}
	return graph.Frontier(snap), nil
}

// Assign takes node off the frontier and opens its unit in repo on lane, in one record:
// the node goes from ready to claimed and the unit, repo:node, is claimed with a link to
// the node. A node not on the frontier and a unit already open are refused, both checked
// under the log's lock. Starting the slot is a separate step.
func (s *Service) Assign(ctx context.Context, repo string, lane string, node string, by string) (eventlog.Unit, error) {
	return s.AssignUnit(ctx, repo, units.AssignPayload{Node: node, Lane: lane, By: by})
}

// AssignUnit checks both the ranked source position and eligibility under the append lock.
// An assignment without selection evidence uses the original named-node checks.
func (s *Service) AssignUnit(ctx context.Context, repo string, payload units.AssignPayload) (eventlog.Unit, error) {
	id, err := units.AssignedID(repo, payload.Node)
	if err != nil {
		return eventlog.Unit{}, err
	}
	if err = units.ValidateAssign(id, payload); err != nil {
		return eventlog.Unit{}, err
	}
	return s.appendAssignment(ctx, units.AssignedType, payload, func(snap graph.Snapshot, st eventlog.State, seq uint64) (string, error) {
		if rank := payload.Selection; rank != nil && rank.LogPosition != seq {
			return "", FrontierMovedError{From: rank.LogPosition, To: seq}
		}
		if refusal := graph.FrontierRefusal(snap, payload.Node); refusal != nil {
			return "", fmt.Errorf("graphlog: assign: %w", refusal)
		}
		return id, units.AssignRefusal(st, id)
	})
}

// FrontierMovedError means a writer advanced the log after the selection was made.
type FrontierMovedError struct {
	From, To uint64
}

// Error reports both positions so the operator can identify the stale selection.
func (e FrontierMovedError) Error() string {
	return fmt.Sprintf("graphlog: frontier moved from log position %d to %d; retry assignment", e.From, e.To)
}

// ExitCode distinguishes a retryable stale selection from a permanent refusal.
func (FrontierMovedError) ExitCode() int { return 75 }

// Release gives a claimed node back, in one record: the node goes from claimed to ready and
// its open unit is dropped with reason. A node without an open assigned unit is refused.
func (s *Service) Release(ctx context.Context, node string, reason string, by string) (eventlog.Unit, error) {
	payload := units.ReleasePayload{Node: node, Reason: reason, By: by}
	if err := units.ValidateRelease(payload); err != nil {
		return eventlog.Unit{}, err
	}
	a := assignment{node: node, from: graph.StatusClaimed, to: graph.StatusReady}
	return s.appendAssignment(ctx, units.ReleasedType, payload, func(snap graph.Snapshot, st eventlog.State, _ uint64) (string, error) {
		if refusal := assignmentRefusal(snap, a); refusal != nil {
			return "", fmt.Errorf("graphlog: release: %w", refusal)
		}
		return units.ReleaseTarget(st, node)
	})
}

// appendAssignment appends one assignment record after check has accepted it against the
// graph and the run record, both replayed under the log's lock. check returns the unit id
// the record names. It returns the unit as the record leaves it.
func (s *Service) appendAssignment(ctx context.Context, recordType string, payload any,
	check func(graph.Snapshot, eventlog.State, uint64) (string, error)) (eventlog.Unit, error) {
	if err := readyContext(ctx, recordType); err != nil {
		return eventlog.Unit{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return eventlog.Unit{}, fmt.Errorf("graphlog: marshal %s: %w", recordType, err)
	}
	writer, err := eventlog.Open(s.dir, s.signer, s.limits)
	if err != nil {
		return eventlog.Unit{}, err
	}
	var before eventlog.State
	rec, err := writer.AppendChecked(ctx, func(cctx context.Context) (eventlog.Event, error) {
		current, seq, wErr := s.writableAt(cctx)
		if wErr != nil {
			return eventlog.Event{}, wErr
		}
		snap, sErr := current.Snapshot()
		if sErr != nil {
			return eventlog.Event{}, sErr
		}
		st, rErr := s.replayWith(cctx, units.FoldName, units.UnitReducer)
		if rErr != nil {
			return eventlog.Event{}, rErr
		}
		id, cErr := check(snap, st, seq)
		if cErr != nil {
			return eventlog.Event{}, cErr
		}
		before = st
		return eventlog.Event{Type: recordType, TaskID: id, Payload: body}, nil
	})
	if err != nil {
		return eventlog.Unit{}, err
	}
	after, err := units.UnitReducer(before, rec)
	if err != nil {
		return eventlog.Unit{}, err
	}
	return after.Units[rec.TaskID], nil
}
