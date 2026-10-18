package score

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graph"
	"github.com/tekmora/epitrop/internal/graphlog"
)

// Terms holds the four scoring terms computed for a candidate frontier node.
type Terms struct {
	ElapsedSeconds int64 // Last - readyAt, whole seconds, floored; 0 if negative (clock skew between writers) or unknown
	Progress       int   // number of nodes NOT landed and NOT dropped that depend on this node, directly or transitively
	Cost           int   // steps: work already spent on the node and handed back
	Confident      bool  // false when the node is inherited (its ready time / steps came before the first folded record)
}

// Ranked pairs a task graph node ID with its scoring terms.
type Ranked struct {
	ID    string
	Terms Terms
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
	g, err := graphlog.Graph(st)
	if err != nil {
		return nil, fmt.Errorf("score: graph from state: %w", err)
	}
	snap, err := g.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("score: snapshot: %w", err)
	}
	return rankNodes(snap, graph.Frontier(snap), f), nil
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
		out = append(out, Ranked{ID: id, Terms: terms})
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
