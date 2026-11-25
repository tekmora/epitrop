package units

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graph"
)

const (
	// AssignedType is the one record that takes a node off the graph's frontier and opens
	// its unit: the graph reads it as ready to claimed, the run record as a claimed unit.
	// One record, so a crash cannot leave a claimed node without its unit or the reverse.
	AssignedType = "unit.assigned"
	// ReleasedType gives an assigned node back: claimed to ready in the graph, the unit
	// dropped in the run record, with the reason.
	ReleasedType = "unit.released"

	// SelectionNext takes the first eligible row. SelectionNamed takes an operator's row.
	SelectionNext  = "next"
	SelectionNamed = "named"
)

// AssignPayload is the payload of unit.assigned. The record's task id is the unit id,
// the repository, a colon and Node.
type AssignPayload = eventlog.UnitAssignment

// ReleasePayload is the payload of unit.released.
type ReleasePayload struct {
	Node   string `json:"node"`
	Reason string `json:"reason"`
	By     string `json:"by,omitempty"`
}

// AssignedID returns the unit id of node in repo and refuses one that is no unit id: a node
// id that does not fit the suffix of a unit id, or a repository holding # or :, cannot be
// assigned.
func AssignedID(repo string, node string) (string, error) {
	id := repo + ":" + node
	if !IsValidID(id) {
		return "", fmt.Errorf("units: %q in repository %q gives no valid unit id", node, repo)
	}
	return id, nil
}

// ValidateAssign checks an assignment's input. It does not depend on the log.
func ValidateAssign(id string, payload AssignPayload) error {
	if !strings.HasSuffix(id, ":"+payload.Node) || !IsValidID(id) {
		return fmt.Errorf("units: unit id %q does not name node %q", id, payload.Node)
	}
	if strings.TrimSpace(payload.Lane) == "" {
		return fmt.Errorf("units: lane is required")
	}
	if err := checkPlainField("lane", payload.Lane); err != nil {
		return err
	}
	if payload.By != "" && !senderRegex.MatchString(payload.By) {
		return fmt.Errorf("units: invalid sender %q", payload.By)
	}
	return validateSelection(payload.Selection)
}

// validateSelection accepts legacy records without a rank and checks the fields
// shared by every version of a recorded ranking.
func validateSelection(rank *eventlog.RankSelection) error {
	if rank == nil {
		return nil
	}
	if rank.Mode != SelectionNamed && rank.Mode != SelectionNext {
		return fmt.Errorf("units: unknown assignment selection mode %q", rank.Mode)
	}
	if rank.AlgorithmVersion == "" || rank.LogPosition == 0 {
		return fmt.Errorf("units: ranked assignment requires a version and source log position")
	}
	if rank.Terms.ElapsedSeconds < 0 || rank.Terms.Cost < 0 || rank.Terms.Progress < 0 {
		return fmt.Errorf("units: negative assignment rank term")
	}
	return nil
}

// ValidateRelease checks a release's input. It does not depend on the log.
func ValidateRelease(payload ReleasePayload) error {
	if payload.Node == "" {
		return fmt.Errorf("units: node is required")
	}
	if len(strings.TrimSpace(payload.Reason)) < 1 || len(payload.Reason) > MaxNoteBytes {
		return fmt.Errorf("units: release reason must be 1..%d bytes", MaxNoteBytes)
	}
	if payload.By != "" && !senderRegex.MatchString(payload.By) {
		return fmt.Errorf("units: invalid sender %q", payload.By)
	}
	return nil
}

// AssignRefusal says why the run record st cannot open unit id, or nil. A unit that was
// landed or dropped may be assigned again; an open one may not.
func AssignRefusal(st eventlog.State, id string) error {
	if assignAllowed(st, id) {
		return nil
	}
	if unit, exists := st.Units[id]; exists {
		return fmt.Errorf("units: %s is already open in status %s", id, unit.Status)
	}
	return fmt.Errorf("units: record already holds %d units", MaxUnits)
}

func assignAllowed(st eventlog.State, id string) bool {
	if unit, exists := st.Units[id]; exists {
		return IsTerminalStatus(unit.Status)
	}
	return len(st.Units) < MaxUnits
}

// ReleaseTarget returns the open unit assigned from node, and refuses a node no open unit
// was assigned from.
func ReleaseTarget(st eventlog.State, node string) (string, error) {
	var ids []string
	for id, u := range st.Units {
		if u.Node == node && !IsTerminalStatus(u.Status) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) != 1 {
		return "", fmt.Errorf("units: node %q has %d open assigned units, want 1", node, len(ids))
	}
	return ids[0], nil
}

// ReadAssignment decodes the same assignment for both the graph and unit folds.
func ReadAssignment(rec eventlog.Record) (AssignPayload, error) {
	var payload AssignPayload
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		return AssignPayload{}, fmt.Errorf("eventlog: seq %d: payload: %w", rec.Seq, err)
	}
	if err := ValidateAssign(rec.TaskID, payload); err != nil {
		return AssignPayload{}, fmt.Errorf("eventlog: seq %d: %w", rec.Seq, err)
	}
	if rank := payload.Selection; rank != nil && rank.LogPosition != rec.Seq-1 {
		return AssignPayload{}, fmt.Errorf("eventlog: seq %d: assignment ranked at %d", rec.Seq, rank.LogPosition)
	}
	return payload, nil
}

func reduceUnitAssigned(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	payload, err := ReadAssignment(rec)
	if err != nil {
		return eventlog.State{}, err
	}
	if !assignAllowed(st, rec.TaskID) {
		st.UnitRecordsIgnored++
		return st, nil
	}
	prev := st.Units[rec.TaskID]
	// A new assignment is a new run of the unit: only the notes carry over.
	st.Units[rec.TaskID] = eventlog.Unit{
		ID: rec.TaskID, Status: string(graph.StatusClaimed), Lane: payload.Lane, By: payload.By,
		Node: payload.Node, Assignment: &payload, UpdatedAt: rec.Time, Notes: prev.Notes, ReadUpto: prev.ReadUpto,
	}
	return st, nil
}

func reduceUnitReleased(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	var payload ReleasePayload
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		return eventlog.State{}, fmt.Errorf("eventlog: seq %d: payload: %w", rec.Seq, err)
	}
	if err := ValidateRelease(payload); err != nil {
		return eventlog.State{}, fmt.Errorf("eventlog: seq %d: %w", rec.Seq, err)
	}
	unit, exists := st.Units[rec.TaskID]
	if !exists || unit.Node != payload.Node || IsTerminalStatus(unit.Status) {
		st.UnitRecordsIgnored++
		return st, nil
	}
	unit.Status = string(graph.StatusDropped)
	unit.UpdatedAt = rec.Time
	if payload.By != "" {
		unit.By = payload.By
	}
	unit.Released = &eventlog.UnitRelease{Seq: rec.Seq, Reason: payload.Reason, At: rec.Time}
	st.Units[rec.TaskID] = unit
	return st, nil
}
