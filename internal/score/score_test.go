package score

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graph"
	"github.com/tekmora/epitrop/internal/graphlog"
	"github.com/tekmora/epitrop/internal/units"
)

func testNode(id string) graph.Node {
	return graph.Node{
		ID:     id,
		Kind:   graph.KindUnit,
		Title:  "Task " + id,
		Status: graph.Status{Name: graph.StatusProposed},
	}
}

func addNode(id string) graph.Event {
	return graph.Event{Type: graph.EventNodeAdded, Node: testNode(id)}
}

func addEdge(id string, dependsOn string) graph.Event {
	return graph.Event{Type: graph.EventEdgeAdded, Edge: graph.Edge{NodeID: id, DependsOn: dependsOn}}
}

func setStatus(id string, to graph.StatusName) graph.Event {
	return graph.Event{Type: graph.EventStatusChanged, NodeID: id, Status: graph.Status{Name: to}}
}

func stagePayload(t *testing.T, ops ...graph.Event) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(graphlog.StagePayload{Ops: ops})
	if err != nil {
		t.Fatalf("Marshal(stage) = %v, want nil", err)
	}
	return body
}

func stageRecord(t *testing.T, seq uint64, tm time.Time, ops ...graph.Event) eventlog.Record {
	t.Helper()
	return eventlog.Record{
		Seq:     seq,
		Time:    tm.UTC().Format(time.RFC3339Nano),
		Type:    graphlog.StageCommitted,
		Payload: stagePayload(t, ops...),
	}
}

func assignRecord(t *testing.T, seq uint64, tm time.Time, id string, payload any, typ string) eventlog.Record {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal(assign) = %v, want nil", err)
	}
	return eventlog.Record{
		Seq:     seq,
		Time:    tm.UTC().Format(time.RFC3339Nano),
		Type:    typ,
		TaskID:  id,
		Payload: body,
	}
}

func testElapsedPart1(t *testing.T, f *Facts, t0 time.Time) eventlog.State {
	t.Helper()
	red := f.Reducer()
	st, err := red(eventlog.State{}, stageRecord(t, 1, t0, addNode("a"), setStatus("a", graph.StatusReady)))
	if err != nil {
		t.Fatalf("step 1: %v", err)
	}
	st, err = red(st, stageRecord(t, 2, t0.Add(90*time.Second), addNode("other")))
	if err != nil {
		t.Fatalf("step 2: %v", err)
	}
	ranked, err := Rank(st, f)
	if err != nil {
		t.Fatalf("rank 1: %v", err)
	}
	if len(ranked) != 1 || ranked[0].Terms.ElapsedSeconds != 90 {
		t.Fatalf("got elapsed %d, want 90", ranked[0].Terms.ElapsedSeconds)
	}
	return st
}

func testElapsedPart2(t *testing.T, f *Facts, st eventlog.State, t0 time.Time) eventlog.State {
	t.Helper()
	red := f.Reducer()
	assign := assignRecord(t, 3, t0.Add(30*time.Second), "r:a", units.AssignPayload{Node: "a", Lane: "l"}, units.AssignedType)
	st, err := red(st, assign)
	if err != nil {
		t.Fatalf("step 3: %v", err)
	}
	release := assignRecord(t, 4, t0.Add(60*time.Second), "r:a", units.ReleasePayload{Node: "a", Reason: "x"}, units.ReleasedType)
	st, err = red(st, release)
	if err != nil {
		t.Fatalf("step 4: %v", err)
	}
	st, err = red(st, stageRecord(t, 5, t0.Add(90*time.Second), addNode("other2")))
	if err != nil {
		t.Fatalf("step 5: %v", err)
	}
	ranked, err := Rank(st, f)
	if err != nil {
		t.Fatalf("rank 2: %v", err)
	}
	if len(ranked) != 1 || ranked[0].Terms.ElapsedSeconds != 30 {
		t.Fatalf("got elapsed %d, want 30", ranked[0].Terms.ElapsedSeconds)
	}
	return st
}

func testElapsedPart3(t *testing.T, f *Facts, st eventlog.State, t0 time.Time) {
	t.Helper()
	red := f.Reducer()
	st, err := red(st, stageRecord(t, 6, t0.Add(50*time.Second), addNode("other3")))
	if err != nil {
		t.Fatalf("step 6: %v", err)
	}
	ranked, err := Rank(st, f)
	if err != nil {
		t.Fatalf("rank 3: %v", err)
	}
	if len(ranked) != 1 || ranked[0].Terms.ElapsedSeconds != 0 {
		t.Fatalf("got elapsed %d, want 0", ranked[0].Terms.ElapsedSeconds)
	}
}

func TestElapsedCountsFromTheLastMoveIntoReady(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	f := NewFacts()
	st := testElapsedPart1(t, f, t0)
	st = testElapsedPart2(t, f, st, t0)
	testElapsedPart3(t, f, st, t0)
}

func TestProgressCountsOpenDependants(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	f := NewFacts()
	ops := []graph.Event{
		addNode("a"), setStatus("a", graph.StatusReady),
		addNode("b"), addEdge("b", "a"),
		addNode("c"), addEdge("c", "b"),
		addNode("d"), addEdge("d", "a"),
		addNode("x"), addEdge("x", "a"), setStatus("x", graph.StatusDropped),
	}
	st, err := f.Reducer()(eventlog.State{}, stageRecord(t, 1, t0, ops...))
	if err != nil {
		t.Fatalf("reduce stage: %v", err)
	}
	ranked, err := Rank(st, f)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if len(ranked) != 1 || ranked[0].ID != "a" {
		t.Fatalf("frontier = %+v, want only a", ranked)
	}
	if ranked[0].Terms.Progress != 3 {
		t.Fatalf("Progress(a) = %d, want 3", ranked[0].Terms.Progress)
	}
}

func testCostN1(t *testing.T, f *Facts, t0 time.Time) eventlog.State {
	t.Helper()
	red := f.Reducer()
	st, err := red(eventlog.State{}, stageRecord(t, 1, t0, addNode("n1"), setStatus("n1", graph.StatusReady)))
	if err != nil {
		t.Fatalf("add n1: %v", err)
	}
	st, err = red(st, assignRecord(t, 2, t0.Add(time.Second), "r:n1", units.AssignPayload{Node: "n1", Lane: "l"}, units.AssignedType))
	if err != nil {
		t.Fatalf("assign n1: %v", err)
	}
	st, err = red(st, assignRecord(t, 3, t0.Add(2*time.Second), "r:n1", units.ReleasePayload{Node: "n1", Reason: "x"}, units.ReleasedType))
	if err != nil {
		t.Fatalf("release n1: %v", err)
	}
	return st
}

func testCostN2(t *testing.T, f *Facts, st eventlog.State, t0 time.Time) eventlog.State {
	t.Helper()
	red := f.Reducer()
	var err error
	st, err = red(st, stageRecord(t, 4, t0.Add(3*time.Second), addNode("n2"), setStatus("n2", graph.StatusClaimed)))
	if err != nil {
		t.Fatalf("n2 claimed: %v", err)
	}
	st, err = red(st, stageRecord(t, 5, t0.Add(4*time.Second), setStatus("n2", graph.StatusImplementing)))
	if err != nil {
		t.Fatalf("n2 implementing: %v", err)
	}
	st, err = red(st, stageRecord(t, 6, t0.Add(5*time.Second), setStatus("n2", graph.StatusReview)))
	if err != nil {
		t.Fatalf("n2 review: %v", err)
	}
	st, err = red(st, stageRecord(t, 7, t0.Add(6*time.Second), setStatus("n2", "fix-round-1")))
	if err != nil {
		t.Fatalf("n2 fix-round-1: %v", err)
	}
	st, err = red(st, stageRecord(t, 8, t0.Add(7*time.Second), setStatus("n2", graph.StatusReady)))
	if err != nil {
		t.Fatalf("n2 ready: %v", err)
	}
	return st
}

func testCostN3(t *testing.T, f *Facts, st eventlog.State, t0 time.Time) eventlog.State {
	t.Helper()
	red := f.Reducer()
	st, err := red(st, stageRecord(t, 9, t0.Add(8*time.Second), addNode("n3"), setStatus("n3", graph.StatusReady)))
	if err != nil {
		t.Fatalf("n3 ready: %v", err)
	}
	return st
}

func assertCosts(t *testing.T, f *Facts, st eventlog.State) {
	t.Helper()
	ranked, err := Rank(st, f)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	costs := make(map[string]int)
	for i := 0; i < len(ranked); i++ {
		costs[ranked[i].ID] = ranked[i].Terms.Cost
	}
	if costs["n1"] != 1 || costs["n2"] != 4 || costs["n3"] != 0 {
		t.Fatalf("costs = %v, want n1:1, n2:4, n3:0", costs)
	}
}

func TestCostCountsWorkHandedBack(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	f := NewFacts()
	st := testCostN1(t, f, t0)
	st = testCostN2(t, f, st, t0)
	st = testCostN3(t, f, st, t0)
	assertCosts(t, f, st)
}

func TestInheritedNodesAreNotConfident(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	st0, err := graphlog.Reducer(eventlog.State{}, stageRecord(t, 1, t0, addNode("old"), setStatus("old", graph.StatusReady)))
	if err != nil {
		t.Fatalf("plain reducer: %v", err)
	}

	f := NewFacts()
	rec2 := stageRecord(t, 2, t0.Add(10*time.Second), addNode("fresh"), setStatus("fresh", graph.StatusReady))
	st1, err := f.Reducer()(st0, rec2)
	if err != nil {
		t.Fatalf("facts reducer: %v", err)
	}

	ranked, err := Rank(st1, f)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	byID := make(map[string]Ranked, len(ranked))
	for i := 0; i < len(ranked); i++ {
		byID[ranked[i].ID] = ranked[i]
	}
	oldNode, ok := byID["old"]
	if !ok || oldNode.Terms.Confident || oldNode.Terms.ElapsedSeconds != 0 {
		t.Fatalf("old node: ok=%v, terms=%+v, want confident=false, elapsed=0", ok, oldNode.Terms)
	}
	freshNode, ok := byID["fresh"]
	if !ok || !freshNode.Terms.Confident {
		t.Fatalf("fresh node: ok=%v, terms=%+v, want confident=true", ok, freshNode.Terms)
	}
}

func TestRankOrdersByTheTerms(t *testing.T) {
	testRankKeyProgress(t)
	testRankKeyCost(t)
	testRankKeyElapsed(t)
	testRankKeyConfident(t)
	testRankKeyID(t)
}

func testRankKeyProgress(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	f := NewFacts()
	ops := []graph.Event{
		addNode("p1"), setStatus("p1", graph.StatusReady),
		addNode("p2"), setStatus("p2", graph.StatusReady),
		addNode("dep1"), addEdge("dep1", "p1"),
	}
	st, err := f.Reducer()(eventlog.State{}, stageRecord(t, 1, t0, ops...))
	if err != nil {
		t.Fatalf("progress stage: %v", err)
	}
	ranked, err := Rank(st, f)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if len(ranked) < 2 || ranked[0].ID != "p1" || ranked[1].ID != "p2" {
		t.Fatalf("order = %+v, want [p1, p2]", ranked)
	}
}

func testRankKeyCost(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	f := NewFacts()
	red := f.Reducer()
	st, err := red(eventlog.State{}, stageRecord(t, 1, t0, addNode("c1"), setStatus("c1", graph.StatusReady), addNode("c2"), setStatus("c2", graph.StatusReady)))
	if err != nil {
		t.Fatalf("cost stage 1: %v", err)
	}
	st, err = red(st, assignRecord(t, 2, t0.Add(time.Second), "r:c2", units.AssignPayload{Node: "c2", Lane: "l"}, units.AssignedType))
	if err != nil {
		t.Fatalf("assign c2: %v", err)
	}
	st, err = red(st, assignRecord(t, 3, t0.Add(2*time.Second), "r:c2", units.ReleasePayload{Node: "c2", Reason: "x"}, units.ReleasedType))
	if err != nil {
		t.Fatalf("release c2: %v", err)
	}
	ranked, err := Rank(st, f)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if len(ranked) < 2 || ranked[0].ID != "c1" || ranked[1].ID != "c2" {
		t.Fatalf("order = %+v, want [c1, c2]", ranked)
	}
}

func testRankKeyElapsed(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	f := NewFacts()
	red := f.Reducer()
	st, err := red(eventlog.State{}, stageRecord(t, 1, t0, addNode("e1"), setStatus("e1", graph.StatusReady)))
	if err != nil {
		t.Fatalf("elapsed stage 1: %v", err)
	}
	st, err = red(st, stageRecord(t, 2, t0.Add(10*time.Second), addNode("e2"), setStatus("e2", graph.StatusReady)))
	if err != nil {
		t.Fatalf("elapsed stage 2: %v", err)
	}
	st, err = red(st, stageRecord(t, 3, t0.Add(30*time.Second), addNode("dummy")))
	if err != nil {
		t.Fatalf("elapsed stage 3: %v", err)
	}
	ranked, err := Rank(st, f)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if len(ranked) < 2 || ranked[0].ID != "e1" || ranked[1].ID != "e2" {
		t.Fatalf("order = %+v, want [e1, e2]", ranked)
	}
}

func testRankKeyConfident(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	st0, err := graphlog.Reducer(eventlog.State{}, stageRecord(t, 1, t0, addNode("a_old"), setStatus("a_old", graph.StatusReady)))
	if err != nil {
		t.Fatalf("stage 1: %v", err)
	}
	f := NewFacts()
	st1, err := f.Reducer()(st0, stageRecord(t, 2, t0, addNode("z_new"), setStatus("z_new", graph.StatusReady)))
	if err != nil {
		t.Fatalf("stage 2: %v", err)
	}
	ranked, err := Rank(st1, f)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if len(ranked) < 2 || ranked[0].ID != "z_new" || ranked[1].ID != "a_old" {
		t.Fatalf("order = %+v, want [z_new, a_old]", ranked)
	}
}

func testRankKeyID(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	f := NewFacts()
	ops := []graph.Event{
		addNode("beta"), setStatus("beta", graph.StatusReady),
		addNode("alpha"), setStatus("alpha", graph.StatusReady),
	}
	st, err := f.Reducer()(eventlog.State{}, stageRecord(t, 1, t0, ops...))
	if err != nil {
		t.Fatalf("id stage: %v", err)
	}
	ranked, err := Rank(st, f)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if len(ranked) < 2 || ranked[0].ID != "alpha" || ranked[1].ID != "beta" {
		t.Fatalf("order = %+v, want [alpha, beta]", ranked)
	}
}

func TestTheSameLogGivesTheSameOrder(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	f := NewFacts()
	ops := []graph.Event{
		addNode("eq1"), setStatus("eq1", graph.StatusReady),
		addNode("eq2"), setStatus("eq2", graph.StatusReady),
		addNode("eq3"), setStatus("eq3", graph.StatusReady),
		addNode("lead"), setStatus("lead", graph.StatusReady),
		addNode("child"), addEdge("child", "lead"),
	}
	st, err := f.Reducer()(eventlog.State{}, stageRecord(t, 1, t0, ops...))
	if err != nil {
		t.Fatalf("reduce stage: %v", err)
	}
	want, err := Rank(st, f)
	if err != nil {
		t.Fatalf("initial rank: %v", err)
	}
	ids := make([]string, 0, len(want))
	for i := 0; i < len(want); i++ {
		ids = append(ids, want[i].ID)
	}
	if !reflect.DeepEqual(ids, []string{"lead", "eq1", "eq2", "eq3"}) {
		t.Fatalf("order = %v, want [lead eq1 eq2 eq3]", ids)
	}
	snap := snapshotOf(t, st)
	frontier := graph.Frontier(snap)
	// The snapshot lists nodes by ID, so Rank alone never sees another order; shuffle the
	// frontier rankNodes receives to prove the comparator, not the input, decides ties.
	for perm := 0; perm < 50; perm++ {
		shuffled := slices.Clone(frontier)
		rng := rand.New(rand.NewPCG(42, uint64(perm)))
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		if got := rankNodes(snap, shuffled, f); !reflect.DeepEqual(got, want) {
			t.Fatalf("perm %d mismatch:\ngot  %+v\nwant %+v", perm, got, want)
		}
	}
}

func snapshotOf(t *testing.T, st eventlog.State) graph.Snapshot {
	t.Helper()
	g, err := graphlog.Graph(st)
	if err != nil {
		t.Fatalf("Graph = %v, want nil", err)
	}
	snap, err := g.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot = %v, want nil", err)
	}
	return snap
}

func TestBadRecordTimeIsRefused(t *testing.T) {
	rec := eventlog.Record{
		Seq:     42,
		Time:    "yesterday",
		Type:    graphlog.StageCommitted,
		Payload: stagePayload(t, addNode("a")),
	}
	f := NewFacts()
	_, err := f.Reducer()(eventlog.State{}, rec)
	if err == nil {
		t.Fatalf("got nil error, want error for bad time")
	}
	if !strings.Contains(err.Error(), "42") {
		t.Fatalf("error %q does not name seq 42", err.Error())
	}
}

func TestRankNilFacts(t *testing.T) {
	_, err := Rank(eventlog.State{}, nil)
	if err == nil {
		t.Fatalf("Rank(nil) = nil error, want error")
	}
}
