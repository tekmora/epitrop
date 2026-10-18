package score

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graph"
	"github.com/tekmora/epitrop/internal/graphlog"
	"github.com/tekmora/epitrop/internal/units"
)

type nodeFacts struct {
	readyAt    time.Time
	hasReadyAt bool
	steps      int
	inherited  bool
}

// Facts folds what the log says about each node.
type Facts struct {
	mu          sync.Mutex
	nodes       map[string]*nodeFacts
	last        time.Time
	hasLast     bool
	lastSeq     uint64
	initialized bool
}

// NewFacts returns an empty Facts store ready to fold event-log records.
func NewFacts() *Facts {
	return &Facts{
		nodes: make(map[string]*nodeFacts),
	}
}

// Reducer applies graphlog.Reducer and, comparing node statuses before/after each record, records per node:
//
//	readyAt  = time of the record that last moved the node INTO ready (or added it as ready)
//	steps    = count of records that moved the node into claimed, implementing, review or fix-round-N
//
// and Last = time of the newest record folded (any type).
// Nodes already present in the state handed to the FIRST call (checkpoint or compaction base) are inherited: their
// facts before that point are unknown.
func (f *Facts) Reducer() eventlog.Reducer {
	return func(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
		return f.reduce(st, rec)
	}
}

func (f *Facts) reduce(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.initFirstCall(st); err != nil {
		return eventlog.State{}, err
	}
	graphRecord := isGraphRecord(rec.Type)
	beforeStatus := map[string]graph.StatusName{}
	if graphRecord {
		var err error
		if beforeStatus, err = nodeStatuses(st); err != nil {
			return eventlog.State{}, fmt.Errorf("score: seq %d: read before status: %w", rec.Seq, err)
		}
	}
	nextSt, err := graphlog.Reducer(st, rec)
	if err != nil {
		return eventlog.State{}, err
	}
	applied := graphRecord && nextSt.GraphStagesIgnored == st.GraphStagesIgnored
	recTime, err := parseRecordTime(rec, applied)
	if err != nil {
		return eventlog.State{}, err
	}
	f.recordLastTime(rec.Seq, recTime)
	if !applied {
		return nextSt, nil
	}
	if err := f.updateNodeFacts(nextSt, beforeStatus, recTime); err != nil {
		return eventlog.State{}, fmt.Errorf("score: seq %d: update node facts: %w", rec.Seq, err)
	}
	return nextSt, nil
}

func (f *Facts) initFirstCall(st eventlog.State) error {
	if f.initialized {
		return nil
	}
	f.initialized = true
	if len(st.Graph) == 0 {
		return nil
	}
	g, err := graphlog.Graph(st)
	if err != nil {
		return fmt.Errorf("score: inherited graph: %w", err)
	}
	snap, err := g.Snapshot()
	if err != nil {
		return fmt.Errorf("score: inherited snapshot: %w", err)
	}
	for i := 0; i < len(snap.Nodes) && i < graph.MaxNodes; i++ {
		f.nodes[snap.Nodes[i].ID] = &nodeFacts{inherited: true}
	}
	return nil
}

func nodeStatuses(st eventlog.State) (map[string]graph.StatusName, error) {
	if len(st.Graph) == 0 {
		return map[string]graph.StatusName{}, nil
	}
	g, err := graphlog.Graph(st)
	if err != nil {
		return nil, fmt.Errorf("graph: %w", err)
	}
	snap, err := g.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	res := make(map[string]graph.StatusName, len(snap.Nodes))
	for i := 0; i < len(snap.Nodes) && i < graph.MaxNodes; i++ {
		res[snap.Nodes[i].ID] = snap.Nodes[i].Status.Name
	}
	return res, nil
}

// isGraphRecord reports whether graphlog.Reducer can change node statuses for this record type;
// only those records pay for a status snapshot.
func isGraphRecord(recType string) bool {
	return recType == units.AssignedType || recType == units.ReleasedType || recType == graphlog.StageCommitted
}

func parseRecordTime(rec eventlog.Record, applied bool) (time.Time, error) {
	if rec.Time == "" {
		if applied {
			return time.Time{}, fmt.Errorf("score: seq %d: empty record time", rec.Seq)
		}
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, rec.Time)
	if err != nil {
		if applied {
			return time.Time{}, fmt.Errorf("score: seq %d: invalid time %q: %w", rec.Seq, rec.Time, err)
		}
		return time.Time{}, nil
	}
	return t, nil
}

func (f *Facts) recordLastTime(seq uint64, t time.Time) {
	if t.IsZero() {
		return
	}
	if !f.hasLast || seq >= f.lastSeq {
		f.last = t
		f.lastSeq = seq
		f.hasLast = true
	}
}

func (f *Facts) updateNodeFacts(st eventlog.State, beforeStatus map[string]graph.StatusName, recTime time.Time) error {
	afterStatus, err := nodeStatuses(st)
	if err != nil {
		return err
	}
	for id, after := range afterStatus {
		before, hadBefore := beforeStatus[id]
		fn, exists := f.nodes[id]
		if !exists {
			fn = &nodeFacts{}
			f.nodes[id] = fn
		}
		if after == graph.StatusReady && (!hadBefore || before != graph.StatusReady) {
			fn.readyAt = recTime
			fn.hasReadyAt = true
		}
		if isStepStatus(after) && (!hadBefore || before != after) {
			fn.steps++
		}
	}
	return nil
}

func isStepStatus(name graph.StatusName) bool {
	switch name {
	case graph.StatusClaimed, graph.StatusImplementing, graph.StatusReview:
		return true
	default:
		return strings.HasPrefix(string(name), "fix-round-")
	}
}

func (f *Facts) computeTerms(nodeID string, progress int) Terms {
	f.mu.Lock()
	defer f.mu.Unlock()

	fn := f.nodes[nodeID]
	if fn == nil {
		return Terms{Progress: progress}
	}
	elapsed := int64(0)
	if fn.hasReadyAt && f.hasLast {
		diff := f.last.Sub(fn.readyAt)
		if diff > 0 {
			elapsed = int64(diff / time.Second)
		}
	}
	return Terms{
		ElapsedSeconds: elapsed,
		Progress:       progress,
		Cost:           fn.steps,
		Confident:      !fn.inherited,
	}
}
