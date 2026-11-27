package score

import (
	"context"
	"errors"
	"fmt"

	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graph"
	"github.com/tekmora/epitrop/internal/graphlog"
	"github.com/tekmora/epitrop/internal/units"
)

// AlgorithmVersion identifies the ordering and term definitions used by Rank.
const AlgorithmVersion = graphlog.RankAlgorithmVersion

// Terms holds the observed scoring terms also recorded in an assignment.
type Terms = eventlog.RankTerms

// Ranked carries the node identity and terms from one graph prefix.
type Ranked = graphlog.RankedNode

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
	return graphlog.RankFrontier(snap, f)
}

// rankNodes ranks an explicit subset so tests can check input-order independence.
func rankNodes(snap graph.Snapshot, frontier []graph.SnapshotNode, f *Facts) []Ranked {
	return graphlog.RankNodes(snap, frontier, f)
}
