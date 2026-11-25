package graphlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/golusoris/golusoris/core/clock"
	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
	"github.com/jonboulle/clockwork"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graph"
	"github.com/tekmora/epitrop/internal/telemetry"
)

// Service is the write path of the task graph: it checks a stage against the graph the log
// holds and appends it as one record.
type Service struct {
	dir      string
	signer   *corereceipt.Signer
	limits   eventlog.Limits
	maxDepth int
}

// NewService returns the service for the log in dir. maxDepth is the configured dependency
// depth; it binds what this service writes.
func NewService(dir string, signer *corereceipt.Signer, clk clock.Clock, limits eventlog.Limits, maxDepth int) (*Service, error) {
	if dir == "" {
		return nil, errors.New("graphlog: dir is required")
	}
	if signer == nil {
		return nil, errors.New("graphlog: signer is required")
	}
	if maxDepth < 1 || maxDepth > config.MaxGraphDepth {
		return nil, fmt.Errorf("graphlog: max depth %d is outside 1 to %d", maxDepth, config.MaxGraphDepth)
	}
	if clk == nil {
		clk = clockwork.NewRealClock()
	}
	limits.Clock = clk
	return &Service{dir: dir, signer: signer, limits: limits, maxDepth: maxDepth}, nil
}

// Replay rebuilds the state the log holds. A log directory without events yet is an empty
// state, not an error.
func (s *Service) Replay(ctx context.Context) (eventlog.State, error) {
	return s.replayWith(ctx, FoldName, Reducer)
}

// FoldName names the graph state in the base of a compacted log.
const FoldName = "graph"

// Fold is the graph state as a compacted log's base holds it.
func Fold() eventlog.Fold {
	return eventlog.Fold{Name: FoldName, Reduce: Reducer}
}

// replayWith replays the log with reducer, whose state a compacted log's base holds under
// fold; a log directory without events yet is an empty state.
func (s *Service) replayWith(ctx context.Context, fold string, reducer eventlog.Reducer) (eventlog.State, error) {
	st, _, err := s.replayFold(ctx, fold, func() eventlog.Reducer { return reducer })
	return st, err
}

// ReplayGraph folds graph records with a fresh reducer per attempt, returning
// the state and its verified log position. The reducer must apply Reducer.
func (s *Service) ReplayGraph(ctx context.Context, newReducer func() eventlog.Reducer) (eventlog.State, uint64, error) {
	return s.replayFold(ctx, FoldName, newReducer)
}

// replayFold treats a log that has never been written as the empty prefix.
func (s *Service) replayFold(ctx context.Context, fold string, newReducer func() eventlog.Reducer) (eventlog.State, uint64, error) {
	if err := readyContext(ctx, "replay"); err != nil {
		return eventlog.State{}, 0, err
	}
	events := filepath.Join(s.dir, "events")
	if _, err := os.Stat(events); os.IsNotExist(err) {
		return eventlog.State{}, 0, nil
	} else if err != nil {
		return eventlog.State{}, 0, fmt.Errorf("graphlog: stat %s: %w", events, err)
	}
	return eventlog.ReplayFoldStableAt(ctx, s.dir, s.signer.PublicKey(), s.limits, fold, newReducer)
}

// Show returns the graph the log holds and the number of stages its replay ignored.
func (s *Service) Show(ctx context.Context) (graph.Snapshot, int, error) {
	st, err := s.Replay(ctx)
	if err != nil {
		return graph.Snapshot{}, 0, err
	}
	stored, err := Graph(st)
	if err != nil {
		return graph.Snapshot{}, 0, err
	}
	snap, err := stored.Snapshot()
	return snap, st.GraphStagesIgnored, err
}

// Commit checks ops as one stage against the graph the log holds and appends the stage as
// one record. A stage the graph refuses appends nothing. It returns the graph after the
// stage.
//
// The check runs under the configured depth. A log may hold a deeper graph, written when the
// depth was higher; it still replays, but nothing more is written to it until the depth is
// raised again: a write checked against a graph that already breaks the bound would say
// nothing about the write.
//
// The check runs under the log's lock; a writer that got in first makes this stage refused
// with the graph's error.
func (s *Service) Commit(ctx context.Context, ops []graph.Event) (graph.Snapshot, error) {
	if err := readyContext(ctx, "commit"); err != nil {
		return graph.Snapshot{}, err
	}
	if err := checkOps(ops); err != nil {
		return graph.Snapshot{}, fmt.Errorf("graphlog: %w", err)
	}
	body, err := json.Marshal(StagePayload{Ops: ops})
	if err != nil {
		return graph.Snapshot{}, fmt.Errorf("graphlog: marshal stage: %w", err)
	}
	writer, err := eventlog.Open(s.dir, s.signer, s.limits)
	if err != nil {
		return graph.Snapshot{}, err
	}
	var next *graph.Graph
	_, err = writer.AppendChecked(ctx, func(cctx context.Context) (eventlog.Event, error) {
		current, _, wErr := s.writableAt(cctx)
		if wErr != nil {
			return eventlog.Event{}, wErr
		}
		applied, aErr := current.ApplyStage(ops)
		if aErr != nil {
			return eventlog.Event{}, fmt.Errorf("graphlog: stage refused: %w", aErr)
		}
		next = applied
		return eventlog.Event{Type: StageCommitted, Payload: body}, nil
	})
	if err != nil {
		return graph.Snapshot{}, err
	}
	snap, err := next.Snapshot()
	if err != nil {
		return graph.Snapshot{}, err
	}
	counts := make(map[graph.StatusName]int, len(snap.Nodes))
	for i := 0; i < len(snap.Nodes); i++ {
		counts[snap.Nodes[i].Status.Name]++
	}
	telemetry.Default().GraphTasks(ctx, counts)
	return snap, nil
}

// writableAt checks the configured depth and returns the exact graph prefix checked.
func (s *Service) writableAt(ctx context.Context) (*graph.Graph, uint64, error) {
	st, seq, err := s.ReplayGraph(ctx, func() eventlog.Reducer { return Reducer })
	if err != nil {
		return nil, 0, err
	}
	stored, err := Graph(st)
	if err != nil {
		return nil, 0, err
	}
	snap, err := stored.Snapshot()
	if err != nil {
		return nil, 0, err
	}
	current, err := graph.FromSnapshot(snap, s.maxDepth)
	if err != nil {
		return nil, 0, fmt.Errorf("graphlog: the graph in the log does not hold under the configured max depth %d, raise graph.max_depth to write to it: %w", s.maxDepth, err)
	}
	return current, seq, nil
}

func readyContext(ctx context.Context, op string) error {
	if ctx == nil {
		return fmt.Errorf("graphlog: %s: nil context", op)
	}
	if _, ok := ctx.Deadline(); !ok {
		return fmt.Errorf("graphlog: %s requires a context deadline", op)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("graphlog: %s context: %w", op, err)
	}
	return nil
}
