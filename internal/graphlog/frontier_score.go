package graphlog

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graph"
	"github.com/tekmora/epitrop/internal/units"
)

// RankAlgorithmVersion identifies the ordering and term definitions used by RankFrontier.
const RankAlgorithmVersion = "v1"

// RankedNode carries a node's identity and score terms from one graph prefix.
type RankedNode struct {
	ID    string             `json:"id"`
	Title string             `json:"title"`
	Terms eventlog.RankTerms `json:"terms"`
}

// RankFrontier scores exactly the nodes the graph allows a slot to take, best first.
func RankFrontier(snap graph.Snapshot, facts *RankFacts) []RankedNode {
	return RankNodes(snap, graph.Frontier(snap), facts)
}

// RankNodes scores a subset of the snapshot's nodes, independent of their input order.
func RankNodes(snap graph.Snapshot, nodes []graph.SnapshotNode, facts *RankFacts) []RankedNode {
	if len(nodes) == 0 {
		return []RankedNode{}
	}
	status, reverse := rankReverseGraph(snap)
	out := make([]RankedNode, 0, len(nodes))
	for i := 0; i < len(nodes) && i < graph.MaxNodes; i++ {
		id := nodes[i].ID
		progress := rankProgress(id, status, reverse, len(snap.Nodes))
		out = append(out, RankedNode{ID: id, Title: nodes[i].Title, Terms: facts.computeTerms(id, progress)})
	}
	slices.SortFunc(out, compareRankedNodes)
	return out
}

// rankReverseGraph indexes node statuses and edges from dependencies to dependants.
func rankReverseGraph(snap graph.Snapshot) (map[string]graph.StatusName, map[string][]string) {
	status := make(map[string]graph.StatusName, len(snap.Nodes))
	reverse := make(map[string][]string, len(snap.Nodes))
	for i := 0; i < len(snap.Nodes) && i < graph.MaxNodes; i++ {
		node := snap.Nodes[i]
		status[node.ID] = node.Status.Name
		for j := 0; j < len(node.DependsOn) && j < graph.MaxEdges; j++ {
			dep := node.DependsOn[j]
			reverse[dep] = append(reverse[dep], node.ID)
		}
	}
	return status, reverse
}

// rankProgress counts the distinct unfinished transitive dependants of rootID.
func rankProgress(rootID string, status map[string]graph.StatusName, reverse map[string][]string, maxNodes int) int {
	if len(reverse[rootID]) == 0 {
		return 0
	}
	visited := make(map[string]bool, maxNodes)
	visited[rootID] = true
	queue := make([]string, 0, maxNodes)
	queue = append(queue, rootID)
	progress := 0
	for head := 0; head < len(queue) && head < graph.MaxNodes; head++ {
		children := reverse[queue[head]]
		for i := 0; i < len(children) && i < graph.MaxEdges; i++ {
			child := children[i]
			if !visited[child] {
				visited[child] = true
				queue = append(queue, child)
				st := status[child]
				if st != graph.StatusLanded && st != graph.StatusDropped {
					progress++
				}
			}
		}
	}
	return progress
}

// compareRankedNodes applies the v1 ordering, with ID as the final tie-breaker.
func compareRankedNodes(a, b RankedNode) int {
	x, y := a.Terms, b.Terms
	if c := cmp.Compare(y.Progress, x.Progress); c != 0 {
		return c
	}
	if c := cmp.Compare(x.Cost, y.Cost); c != 0 {
		return c
	}
	if c := cmp.Compare(y.ElapsedSeconds, x.ElapsedSeconds); c != 0 {
		return c
	}
	if x.Confident != y.Confident {
		if x.Confident {
			return -1
		}
		return 1
	}
	return cmp.Compare(a.ID, b.ID)
}

type rankNodeFacts struct {
	readyAt    time.Time
	hasReadyAt bool
	steps      int
	inherited  bool
}

// RankFacts folds the history used to score nodes in one replay attempt.
type RankFacts struct {
	mu          sync.Mutex
	nodes       map[string]*rankNodeFacts
	last        time.Time
	hasLast     bool
	lastSeq     uint64
	initialized bool
}

// NewRankFacts returns an empty history ready to fold event-log records.
func NewRankFacts() *RankFacts {
	return &RankFacts{nodes: make(map[string]*rankNodeFacts)}
}

// Reducer applies the graph reducer and observes ready times, transitions into work
// statuses, and the newest record time. Nodes inherited from a base lack prior history.
func (f *RankFacts) Reducer() eventlog.Reducer {
	return f.reduce
}

// reduce observes one record without changing the graph reducer's state or decisions.
func (f *RankFacts) reduce(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.initFirstCall(st); err != nil {
		return eventlog.State{}, err
	}
	graphRecord := isRankGraphRecord(rec)
	beforeStatus, err := rankBeforeStatuses(st, rec, graphRecord)
	if err != nil {
		return eventlog.State{}, err
	}
	next, err := Reducer(st, rec)
	if err != nil {
		return eventlog.State{}, err
	}
	applied := graphRecord && next.GraphStagesIgnored == st.GraphStagesIgnored
	recTime, err := rankRecordTime(rec, applied)
	if err != nil {
		return eventlog.State{}, err
	}
	f.recordLastTime(rec.Seq, recTime)
	if !applied {
		return next, nil
	}
	if err := f.updateNodeFacts(next, beforeStatus, recTime); err != nil {
		return eventlog.State{}, fmt.Errorf("score: seq %d: update node facts: %w", rec.Seq, err)
	}
	return next, nil
}

// isRankGraphRecord reports whether rec can change node statuses observed by the rank fold.
func isRankGraphRecord(rec eventlog.Record) bool {
	switch rec.Type {
	case units.AssignedType, units.ReleasedType, StageCommitted:
		return true
	default:
		return false
	}
}

// rankBeforeStatuses reads the pre-reduction statuses for graph records only.
func rankBeforeStatuses(st eventlog.State, rec eventlog.Record, graphRecord bool) (map[string]graph.StatusName, error) {
	if !graphRecord {
		return nil, nil
	}
	statuses, err := rankNodeStatuses(st)
	if err != nil {
		return nil, fmt.Errorf("score: seq %d: read before status: %w", rec.Seq, err)
	}
	return statuses, nil
}

// initFirstCall marks nodes in the starting state as inherited before observing the tail.
func (f *RankFacts) initFirstCall(st eventlog.State) error {
	if f.initialized {
		return nil
	}
	f.initialized = true
	if len(st.Graph) == 0 {
		return nil
	}
	g, err := Graph(st)
	if err != nil {
		return fmt.Errorf("score: inherited graph: %w", err)
	}
	snap, err := g.Snapshot()
	if err != nil {
		return fmt.Errorf("score: inherited snapshot: %w", err)
	}
	for i := 0; i < len(snap.Nodes) && i < graph.MaxNodes; i++ {
		f.nodes[snap.Nodes[i].ID] = &rankNodeFacts{inherited: true}
	}
	return nil
}

// rankNodeStatuses reads the node statuses used to observe graph transitions.
func rankNodeStatuses(st eventlog.State) (map[string]graph.StatusName, error) {
	if len(st.Graph) == 0 {
		return map[string]graph.StatusName{}, nil
	}
	g, err := Graph(st)
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

// rankRecordTime requires a valid time for applied graph records and otherwise uses it if valid.
func rankRecordTime(rec eventlog.Record, applied bool) (time.Time, error) {
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

// recordLastTime keeps the time of the newest record with an observed time.
func (f *RankFacts) recordLastTime(seq uint64, t time.Time) {
	if !t.IsZero() && (!f.hasLast || seq >= f.lastSeq) {
		f.last, f.lastSeq, f.hasLast = t, seq, true
	}
}

// updateNodeFacts observes entries into ready and work statuses in an applied record.
func (f *RankFacts) updateNodeFacts(st eventlog.State, beforeStatus map[string]graph.StatusName, recTime time.Time) error {
	afterStatus, err := rankNodeStatuses(st)
	if err != nil {
		return err
	}
	for id, after := range afterStatus {
		before, hadBefore := beforeStatus[id]
		fn, exists := f.nodes[id]
		if !exists {
			fn = &rankNodeFacts{}
			f.nodes[id] = fn
		}
		if after == graph.StatusReady && (!hadBefore || before != graph.StatusReady) {
			fn.readyAt, fn.hasReadyAt = recTime, true
		}
		if rankStepStatus(after) && (!hadBefore || before != after) {
			fn.steps++
		}
	}
	return nil
}

// rankStepStatus reports whether entering name counts toward observed cost.
func rankStepStatus(name graph.StatusName) bool {
	switch name {
	case graph.StatusClaimed, graph.StatusImplementing, graph.StatusReview:
		return true
	default:
		return strings.HasPrefix(string(name), "fix-round-")
	}
}

// computeTerms combines snapshot progress with the history available for nodeID.
func (f *RankFacts) computeTerms(nodeID string, progress int) eventlog.RankTerms {
	f.mu.Lock()
	defer f.mu.Unlock()

	fn := f.nodes[nodeID]
	if fn == nil {
		return eventlog.RankTerms{Progress: progress}
	}
	elapsed := int64(0)
	if fn.hasReadyAt && f.hasLast {
		if diff := f.last.Sub(fn.readyAt); diff > 0 {
			elapsed = int64(diff / time.Second)
		}
	}
	return eventlog.RankTerms{
		ElapsedSeconds: elapsed,
		Progress:       progress,
		Cost:           fn.steps,
		Confident:      !fn.inherited,
	}
}
