package graphlog

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graph"
	"github.com/tekmora/epitrop/internal/units"
)

// Frontier returns the graph's eligible nodes using graph.Frontier. It writes nothing.
// It is a membership-only compatibility wrapper: it does not report rank terms or a
// source log position, and its order is not the ranked frontier's selection order.
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

// AssignUnit checks eligibility and any ranked selection under the append lock.
// An assignment without selection evidence uses the original named-node checks.
func (s *Service) AssignUnit(ctx context.Context, repo string, payload units.AssignPayload) (eventlog.Unit, error) {
	id, err := units.AssignedID(repo, payload.Node)
	if err != nil {
		return eventlog.Unit{}, err
	}
	if payload.Selection != nil {
		selection := *payload.Selection
		payload.Selection = &selection
	}
	if err = units.ValidateAssign(id, payload); err != nil {
		return eventlog.Unit{}, err
	}
	var facts *RankFacts
	newReducer := func() eventlog.Reducer {
		if payload.Selection == nil {
			return Reducer
		}
		facts = NewRankFacts()
		return facts.Reducer()
	}
	return s.appendAssignment(ctx, units.AssignedType, payload, newReducer, func(snap graph.Snapshot, st eventlog.State, seq uint64) (string, error) {
		return checkLockedAssignment(snap, st, seq, id, payload, facts)
	})
}

// checkLockedAssignment returns the unit ID if the locked replay permits the assignment.
// It checks the selection position first and verifies rank evidence only after eligibility.
func checkLockedAssignment(snap graph.Snapshot, st eventlog.State, seq uint64, id string, payload units.AssignPayload, facts *RankFacts) (string, error) {
	rank := payload.Selection
	if rank != nil && rank.LogPosition != seq {
		return "", FrontierMovedError{From: rank.LogPosition, To: seq}
	}
	if refusal := graph.FrontierRefusal(snap, payload.Node); refusal != nil {
		return "", fmt.Errorf("graphlog: assign: %w", refusal)
	}
	if err := units.AssignRefusal(st, id); err != nil {
		return "", err
	}
	if rank == nil {
		return id, nil
	}
	if err := rankSelectionRefusal(snap, facts, payload.Node, *rank); err != nil {
		return "", err
	}
	return id, nil
}

// rankSelectionRefusal checks evidence against the locked replay. Compaction can change
// the observed terms and ordering without advancing the head, so position alone is not enough.
func rankSelectionRefusal(snap graph.Snapshot, facts *RankFacts, node string, rank eventlog.RankSelection) error {
	if rank.AlgorithmVersion != RankAlgorithmVersion {
		return fmt.Errorf("graphlog: assign: unsupported rank algorithm version %q", rank.AlgorithmVersion)
	}
	rows := RankFrontier(snap, facts)
	for i, row := range rows {
		if row.ID != node {
			continue
		}
		if rank.Mode == units.SelectionNext && i != 0 {
			return fmt.Errorf("graphlog: assign: next selection must name the first ranked node %q, not %q; reread the frontier", rows[0].ID, node)
		}
		if rank.Terms != row.Terms {
			return fmt.Errorf("graphlog: assign: rank terms for node %q do not match the current frontier; reread the frontier", node)
		}
		return nil
	}
	return fmt.Errorf("graphlog: assign: node %q is missing from the ranked frontier", node)
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
	return s.appendAssignment(ctx, units.ReleasedType, payload, func() eventlog.Reducer { return Reducer }, func(snap graph.Snapshot, st eventlog.State, _ uint64) (string, error) {
		if refusal := assignmentRefusal(snap, a); refusal != nil {
			return "", fmt.Errorf("graphlog: release: %w", refusal)
		}
		return units.ReleaseTarget(st, node)
	})
}

// appendAssignment appends one assignment record after check has accepted it against the
// graph and the run record, both replayed under the log's lock. newReducer supplies the
// graph reducer for each replay attempt. check returns the unit id the record names.
// appendAssignment returns the unit as the record leaves it.
func (s *Service) appendAssignment(ctx context.Context, recordType string, payload any,
	newReducer func() eventlog.Reducer,
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
		current, seq, wErr := s.writableAtWith(cctx, newReducer)
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
