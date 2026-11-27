package graphlog

import (
	"testing"

	"github.com/tekmora/epitrop/internal/graph"
)

// TestRankProgressLeafAllocs verifies that a leaf needs no traversal storage.
func TestRankProgressLeafAllocs(t *testing.T) {
	const maxNodes = 1024
	status := map[string]graph.StatusName{"leaf": graph.StatusReady}
	reverse := map[string][]string{}
	var progress int
	allocs := testing.AllocsPerRun(100, func() {
		progress = rankProgress("leaf", status, reverse, maxNodes)
	})
	if progress != 0 {
		t.Fatalf("leaf progress = %d, want 0", progress)
	}
	if allocs != 0 {
		t.Fatalf("leaf allocations per run = %g, want 0", allocs)
	}
}

// TestRankProgressThroughFinishedNode verifies that finished dependants are
// traversed but do not contribute to the score.
func TestRankProgressThroughFinishedNode(t *testing.T) {
	status := map[string]graph.StatusName{
		"root":     graph.StatusReady,
		"finished": graph.StatusLanded,
		"leaf":     graph.StatusReady,
	}
	reverse := map[string][]string{
		"root":     {"finished"},
		"finished": {"leaf"},
	}
	if got := rankProgress("root", status, reverse, len(status)); got != 1 {
		t.Fatalf("root progress = %d, want 1", got)
	}
	if got := rankProgress("finished", status, reverse, len(status)); got != 1 {
		t.Fatalf("finished node progress = %d, want 1", got)
	}
}
