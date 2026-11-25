package score

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graph"
	"github.com/tekmora/epitrop/internal/graphlog"
	"github.com/tekmora/epitrop/internal/units"
)

// AlgorithmVersion identifies the ordering and term definitions used by Rank.
const AlgorithmVersion = "v1"

// Terms holds the observed scoring terms also recorded in an assignment.
type Terms = eventlog.RankTerms

// Ranked carries the node identity and terms from one graph prefix.
type Ranked struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Terms Terms  `json:"terms"`
}

// Frontier describes the eligible rows at LogPosition, best first.
type Frontier struct {
	AlgorithmVersion string   `json:"algorithm_version"`
	LogPosition      uint64   `json:"log_position"`
	Rows             []Ranked `json:"rows"`

	snapshot graph.Snapshot
}

// ReadFrontier ranks the graph and its history from one successful replay attempt.
func ReadFrontier(ctx context.Context, svc *graphlog.Service) (Frontier, error) {
	var facts *Facts
	st, seq, err := svc.ReplayGraph(ctx, func() eventlog.Reducer {
		facts = NewFacts()
		return facts.Reducer()
	})
	if err != nil {
		return Frontier{}, err
	}
	snap, err := snapshot(st)
	if err != nil {
		return Frontier{}, err
	}
	return Frontier{
		AlgorithmVersion: AlgorithmVersion, LogPosition: seq,
		Rows: rankFrontier(snap, facts), snapshot: snap,
	}, nil
}

// Select returns the named eligible row, or the first row when node is empty,
// together with the evidence the writer checks and saves on the assignment.
func (f Frontier) Select(node string, lane string, by string) (units.AssignPayload, error) {
	mode := units.SelectionNamed
	if node == "" {
		if len(f.Rows) == 0 {
			return units.AssignPayload{}, errors.New("units assign: frontier is empty")
		}
		mode, node = units.SelectionNext, f.Rows[0].ID
	}
	for i := 0; i < len(f.Rows); i++ {
		if row := f.Rows[i]; row.ID == node {
			return units.AssignPayload{Node: node, Lane: lane, By: by, Selection: &eventlog.RankSelection{
				Mode: mode, AlgorithmVersion: f.AlgorithmVersion, LogPosition: f.LogPosition, Terms: row.Terms,
			}}, nil
		}
	}
	if err := graph.FrontierRefusal(f.snapshot, node); err != nil {
		return units.AssignPayload{}, err
	}
	return units.AssignPayload{}, fmt.Errorf("score: eligible node %q missing from ranked rows", node)
}

// Rank returns the frontier of st, best first:
//
//	Progress desc, Cost asc, ElapsedSeconds desc, Confident (true first), ID asc.
//
// The ID key makes the order total: the same log always gives the same order whatever order the snapshot lists nodes in.
func Rank(st eventlog.State, f *Facts) ([]Ranked, error) {
	if f == nil {
		return nil, errors.New("score: nil facts")
	}
	snap, err := snapshot(st)
	if err != nil {
		return nil, err
	}
	return rankFrontier(snap, f), nil
}

// snapshot reads the same graph representation used by the assignment check.
func snapshot(st eventlog.State) (graph.Snapshot, error) {
	g, err := graphlog.Graph(st)
	if err != nil {
		return graph.Snapshot{}, fmt.Errorf("score: graph from state: %w", err)
	}
	return g.Snapshot()
}

// rankFrontier scores exactly the nodes the graph allows a slot to take.
func rankFrontier(snap graph.Snapshot, f *Facts) []Ranked {
	return rankNodes(snap, graph.Frontier(snap), f)
}

// rankNodes scores and orders frontier, a subset of snap's nodes in any order. The snapshot
// already lists nodes by ID, so the order Rank returns must not lean on the input order: the
// full comparator, with ID last, decides it.
func rankNodes(snap graph.Snapshot, frontier []graph.SnapshotNode, f *Facts) []Ranked {
	if len(frontier) == 0 {
		return []Ranked{}
	}
	status, reverse := buildReverseGraph(snap)
	out := make([]Ranked, 0, len(frontier))
	for i := 0; i < len(frontier) && i < graph.MaxNodes; i++ {
		id := frontier[i].ID
		prog := computeProgress(id, status, reverse, len(snap.Nodes))
		terms := f.computeTerms(id, prog)
		out = append(out, Ranked{ID: id, Title: frontier[i].Title, Terms: terms})
	}
	slices.SortFunc(out, compareRanked)
	return out
}

func buildReverseGraph(snap graph.Snapshot) (map[string]graph.StatusName, map[string][]string) {
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

func computeProgress(rootID string, status map[string]graph.StatusName, reverse map[string][]string, maxNodes int) int {
	visited := make(map[string]bool, maxNodes)
	visited[rootID] = true
	queue := make([]string, 0, maxNodes)
	queue = append(queue, rootID)
	progress := 0

	for head := 0; head < len(queue) && head < graph.MaxNodes; head++ {
		curr := queue[head]
		children := reverse[curr]
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

func compareRanked(a, b Ranked) int {
	if c := compareTerms(a.Terms, b.Terms); c != 0 {
		return c
	}
	return cmpAsc(a.ID, b.ID)
}

func compareTerms(a, b Terms) int {
	if a.Progress != b.Progress {
		return cmpDesc(a.Progress, b.Progress)
	}
	if a.Cost != b.Cost {
		return cmpAsc(a.Cost, b.Cost)
	}
	if a.ElapsedSeconds != b.ElapsedSeconds {
		return cmpDesc(a.ElapsedSeconds, b.ElapsedSeconds)
	}
	if a.Confident != b.Confident {
		if a.Confident {
			return -1
		}
		return 1
	}
	return 0
}

func cmpAsc[T cmp.Ordered](a, b T) int {
	return cmp.Compare(a, b)
}

func cmpDesc[T cmp.Ordered](a, b T) int {
	return cmp.Compare(b, a)
}
