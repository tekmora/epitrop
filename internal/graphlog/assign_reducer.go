package graphlog

import (
	"encoding/json"
	"fmt"

	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graph"
	"github.com/tekmora/epitrop/internal/units"
)

// assignment is what the graph reads from a unit.assigned or unit.released record: the node
// and the move it makes.
type assignment struct {
	node string
	from graph.StatusName
	to   graph.StatusName
}

// readAssignment reads a unit.assigned or unit.released record and refuses one no writer
// produces, with the same checks the run record applies to it.
func readAssignment(rec eventlog.Record) (assignment, error) {
	if rec.Type == units.AssignedType {
		payload, err := units.ReadAssignment(rec)
		if err != nil {
			return assignment{}, err
		}
		return assignment{node: payload.Node, from: graph.StatusReady, to: graph.StatusClaimed}, nil
	}
	var payload units.ReleasePayload
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		return assignment{}, fmt.Errorf("eventlog: seq %d: payload: %w", rec.Seq, err)
	}
	if err := units.ValidateRelease(payload); err != nil {
		return assignment{}, fmt.Errorf("eventlog: seq %d: %w", rec.Seq, err)
	}
	return assignment{node: payload.Node, from: graph.StatusClaimed, to: graph.StatusReady}, nil
}

// assignmentRefusal says why the graph cannot make the move, or nil: an assignment needs
// the node on the frontier, a release needs it claimed.
func assignmentRefusal(snap graph.Snapshot, a assignment) error {
	if a.to == graph.StatusClaimed {
		return graph.FrontierRefusal(snap, a.node)
	}
	for i := 0; i < len(snap.Nodes); i++ {
		if snap.Nodes[i].ID == a.node {
			if snap.Nodes[i].Status.Name != a.from {
				return fmt.Errorf("graph: node %q is %s, not %s", a.node, snap.Nodes[i].Status.Name, a.from)
			}
			return nil
		}
	}
	return fmt.Errorf("graph: node %q does not exist", a.node)
}

// applyAssignment returns the graph after the move, and false when the graph refuses it.
func applyAssignment(current *graph.Graph, a assignment) (*graph.Graph, bool) {
	snap, err := current.Snapshot()
	if err != nil || assignmentRefusal(snap, a) != nil {
		return nil, false
	}
	return applyStage(current, []graph.Event{{Type: graph.EventStatusChanged, NodeID: a.node, Status: graph.Status{Name: a.to}}})
}

// reduceAssignment applies a unit.assigned or unit.released record to the graph. A move the
// graph refuses is counted with the ignored stages, as a refused stage is: the writer checks
// it under the log's lock, so only a record no writer of this version wrote reaches here.
func reduceAssignment(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	a, err := readAssignment(rec)
	if err != nil {
		return eventlog.State{}, err
	}
	current, err := Graph(st)
	if err != nil {
		return eventlog.State{}, fmt.Errorf("eventlog: seq %d: %w", rec.Seq, err)
	}
	next, applied := applyAssignment(current, a)
	if !applied {
		st.GraphStagesIgnored++
		return st, nil
	}
	if st.Graph, err = next.JCS(); err != nil {
		return eventlog.State{}, fmt.Errorf("eventlog: seq %d: %w", rec.Seq, err)
	}
	return st, nil
}
