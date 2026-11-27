package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/golusoris/golusoris/core/codec/jcs"
	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graphlog"
	"github.com/tekmora/epitrop/internal/units"
)

// unitsGraphRun runs one units command that prints through graphOut.
func unitsGraphRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	old := graphOut
	graphOut = &out
	defer func() { graphOut = old }()
	err := runUnits(args)
	return out.String(), err
}

func statusOp(id string, to string) string {
	return `{"type":"graph.status_changed","node_id":"` + id + `","status":{"name":"` + to + `"}}`
}

func printedUnit(t *testing.T, out string) eventlog.Unit {
	t.Helper()
	var u eventlog.Unit
	if err := json.Unmarshal([]byte(out), &u); err != nil {
		t.Fatalf("output is no unit: %v\n%s", err, out)
	}
	return u
}

// frontier, assign and release run end to end against one log: the frontier names the
// ready node whose dependency landed, an assignment takes it off, a release puts it back.
func TestRunUnitsFrontierAssignReleaseEndToEnd(t *testing.T) {
	cfg, _, _ := graphConfig(t, 12)
	setupUnitsFrontierStage(t, cfg)
	checkUnitsFrontierAssignRefusals(t, cfg)
	checkUnitsFrontierAssignSuccess(t, cfg)
	checkUnitsFrontierReleaseSuccess(t, cfg)
	checkUnitsFrontierReleaseRefusal(t, cfg)
}

func setupUnitsFrontierStage(t *testing.T, cfg string) {
	t.Helper()
	ops := []string{nodeOp("a"), nodeOp("b"), nodeOp("c"), edgeOp("b", "a"), edgeOp("c", "b"),
		statusOp("a", "ready"), statusOp("b", "ready"), statusOp("c", "ready")}
	for _, to := range []string{"claimed", "implementing", "review", "landing", "landed"} {
		ops = append(ops, statusOp("a", to))
	}
	stage := stageFile(t, `{"ops":[`+strings.Join(ops, ",")+`]}`)
	if _, _, err := graphRun(t, "", "apply", "--config", cfg, "--file", stage); err != nil {
		t.Fatalf("apply = %v, want nil", err)
	}
	out, err := unitsGraphRun(t, "frontier", "--config", cfg)
	if err != nil || out != "b\t\"Task b\"\n" {
		t.Fatalf("frontier = %q, %v; want b alone", out, err)
	}
}

func checkUnitsFrontierAssignRefusals(t *testing.T, cfg string) {
	t.Helper()
	if _, err := unitsGraphRun(t, "assign", "--config", cfg, "--repo", "tekmora/epitrop", "c"); err == nil || !strings.Contains(err.Error(), "--repo and --lane") {
		t.Fatalf("assign without a lane = %v, want it refused", err)
	}
	if _, err := unitsGraphRun(t, "assign", "--config", cfg, "--repo", "tekmora/epitrop", "--lane", "l", "c"); err == nil || !strings.Contains(err.Error(), `depends on "b"`) {
		t.Fatalf("assign c = %v, want it refused for its dependency", err)
	}
}

func checkUnitsFrontierAssignSuccess(t *testing.T, cfg string) {
	t.Helper()
	out, err := unitsGraphRun(t, "assign", "--config", cfg, "--repo", "tekmora/epitrop", "--lane", "l", "--by", "slot-1", "b")
	if u := printedUnit(t, out); err != nil || u.ID != "tekmora/epitrop:b" || u.Status != "claimed" || u.Node != "b" {
		t.Fatalf("assign b = %+v, %v; want the claimed unit of b", u, err)
	}
	if out, err = unitsGraphRun(t, "frontier", "--config", cfg); err != nil || out != "" {
		t.Fatalf("frontier after the assignment = %q, %v; want empty", out, err)
	}
}

func checkUnitsFrontierReleaseSuccess(t *testing.T, cfg string) {
	t.Helper()
	if _, err := unitsGraphRun(t, "release", "--config", cfg, "b"); err == nil || !strings.Contains(err.Error(), "--reason is required") {
		t.Fatalf("release without a reason = %v, want it refused", err)
	}
	out, err := unitsGraphRun(t, "release", "--config", cfg, "--reason", "lane down", "b")
	if u := printedUnit(t, out); err != nil || u.Status != "dropped" || u.Released == nil || u.Released.Reason != "lane down" {
		t.Fatalf("release b = %+v, %v; want the unit dropped with the reason", u, err)
	}
	if out, err = unitsGraphRun(t, "frontier", "--config", cfg); err != nil || out != "b\t\"Task b\"\n" {
		t.Fatalf("frontier after the release = %q, %v; want b again", out, err)
	}
}

func checkUnitsFrontierReleaseRefusal(t *testing.T, cfg string) {
	t.Helper()
	if _, err := unitsGraphRun(t, "release", "--config", cfg, "--reason", "again", "b"); err == nil || !strings.Contains(err.Error(), "not claimed") {
		t.Fatalf("release of a ready node = %v, want it refused", err)
	}
}

// rankedFixture opens the CLI and a second reader/writer on the same temporary signed log.
type rankedFixture struct {
	path   string
	dir    string
	graph  *graphlog.Service
	log    *eventlog.Writer
	signer *corereceipt.Signer
	limits eventlog.Limits
}

// newRankedFixture starts with no events. Every key and path comes from graphConfig.
func newRankedFixture(t *testing.T) rankedFixture {
	t.Helper()
	path, dir, _ := graphConfig(t, 12)
	cfg, err := config.Load(testContext(t), path)
	if err != nil {
		t.Fatal(err)
	}
	signer, clk, err := eventLogSigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	limits := eventLogLimits(cfg, clk)
	log, err := eventlog.Open(dir, signer, limits)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := newGraphService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rankedFixture{path: path, dir: dir, graph: svc, log: log, signer: signer, limits: limits}
}

// stage makes all named nodes ready in one record, so all their timestamps tie.
func (f rankedFixture) stage(t *testing.T, ids string, edges ...string) {
	t.Helper()
	var ops []string
	for i := 0; i < len(ids); i++ {
		id := ids[i : i+1]
		ops = append(ops, nodeOp(id), statusOp(id, "ready"))
	}
	ops = append(ops, edges...)
	stage := `{"ops":[` + strings.Join(ops, ",") + `]}`
	if _, _, err := graphRun(t, stage, "apply", "--config", f.path, "--file=-"); err != nil {
		t.Fatal(err)
	}
}

// chain gives z two unfinished dependants and leaves a and b tied.
func (f rankedFixture) chain(t *testing.T, ids string) {
	t.Helper()
	f.stage(t, ids, edgeOp("x", "z"), edgeOp("y", "x"))
}

// assign runs the CLI with one fixed assignment identity and the given selectors.
func (f rankedFixture) assign(t *testing.T, selectors ...string) (string, error) {
	t.Helper()
	args := []string{"assign", "--repo=r", "--lane=lane-1", "--by=actor-1", "--config", f.path}
	return unitsGraphRun(t, append(args, selectors...)...)
}

// frontierRow decodes the public wire names independently of the implementation.
// The map distinguishes a missing zero or false term from one explicitly emitted.
type frontierRow struct {
	ID    string         `json:"id"`
	Title string         `json:"title"`
	Terms map[string]any `json:"terms"`
}

// frontierDocument is the versioned JSON expected by a consumer of units frontier.
type frontierDocument struct {
	Version string        `json:"algorithm_version"`
	Seq     uint64        `json:"log_position"`
	Rows    []frontierRow `json:"rows"`
}

// read checks the comparator identity and distinguishes an empty array from null.
func (f rankedFixture) read(t *testing.T, seq uint64) frontierDocument {
	t.Helper()
	out, err := unitsGraphRun(t, "frontier", "--config", f.path, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc frontierDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != "v1" || doc.Seq != seq || doc.Rows == nil {
		t.Fatalf("unexpected frontier at %d: %s", seq, out)
	}
	return doc
}

// expectedRow constructs an exact row for a node just made ready at the log head.
func expectedRow(id string, progress, cost int, confident bool) frontierRow {
	return frontierRow{ID: id, Title: "Task " + id, Terms: map[string]any{
		"elapsed_seconds": float64(0), "progress": float64(progress), "cost": float64(cost), "confident": confident,
	}}
}

// TestRankedFrontierInsertionOrder checks eligibility, progress and the final ID tie-break.
func TestRankedFrontierInsertionOrder(t *testing.T) {
	for _, ids := range []string{"zxyab", "bayxz"} {
		f := newRankedFixture(t)
		f.chain(t, ids)
		out, err := unitsGraphRun(t, "frontier", "--config", f.path)
		if err != nil || out != "z\t\"Task z\"\na\t\"Task a\"\nb\t\"Task b\"\n" {
			t.Fatalf("text frontier for %s = %q, %v", ids, out, err)
		}
		got := f.read(t, 1)
		want := []frontierRow{expectedRow("z", 2, 0, true), expectedRow("a", 0, 0, true), expectedRow("b", 0, 0, true)}
		if !reflect.DeepEqual(got.Rows, want) {
			t.Fatalf("insertion %s: rows = %+v, want %+v", ids, got.Rows, want)
		}
	}
}

// TestRankedFrontierEmptyAndSingleton exercises both ends of first-row selection.
func TestRankedFrontierEmptyAndSingleton(t *testing.T) {
	f := newRankedFixture(t)
	if rows := f.read(t, 0).Rows; len(rows) != 0 {
		t.Fatalf("empty graph rows = %+v", rows)
	}
	if _, err := f.assign(t, "--next"); err == nil || !strings.Contains(err.Error(), "frontier is empty") {
		t.Fatalf("empty selection = %v", err)
	}
	f.stage(t, "s")
	if rows := f.read(t, 1).Rows; !reflect.DeepEqual(rows, []frontierRow{expectedRow("s", 0, 0, true)}) {
		t.Fatalf("singleton rows = %+v", rows)
	}
	if _, err := f.assign(t, "--next"); err != nil {
		t.Fatal(err)
	}
	if rows := f.read(t, 2).Rows; len(rows) != 0 {
		t.Fatalf("frontier after claiming singleton = %+v", rows)
	}
}

// TestRankedAssignRefused checks that invalid selections and an open unit cannot append.
func TestRankedAssignRefused(t *testing.T) {
	f := newRankedFixture(t)
	f.chain(t, "bayxz")
	cases := []struct {
		args []string
		want string
	}{
		{nil, "exactly one"}, {[]string{""}, "exactly one"},
		{[]string{"--next", "b"}, "exactly one"}, {[]string{"a", "b"}, "exactly one"},
		{[]string{"x"}, `depends on "z"`}, {[]string{"missing"}, "does not exist"},
	}
	for _, tc := range cases {
		if _, err := f.assign(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("assign %q = %v, want %s", tc.args, err, tc.want)
		}
	}
	f.read(t, 1)
	if err := runUnits([]string{"set", "r:z", "--status=claimed", "--config", f.path}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.assign(t, "--next"); err == nil || !strings.Contains(err.Error(), "already open") {
		t.Fatalf("assign over open unit = %v", err)
	}
	f.read(t, 2)
}

// assignmentJSON builds the expected assignment event from an independently decoded rank row.
func assignmentJSON(doc frontierDocument, row frontierRow, mode string) map[string]any {
	return map[string]any{
		"node": row.ID, "by": "actor-1", "lane": "lane-1",
		"selection": map[string]any{
			"mode": mode, "algorithm_version": doc.Version, "log_position": float64(doc.Seq), "terms": row.Terms,
		},
	}
}

// checkAssignmentJSON compares all fields, including the absence of a rank on older records.
func checkAssignmentJSON(t *testing.T, body []byte, want map[string]any) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("assignment = %s, want %+v", body, want)
	}
}

// checkClaim verifies the exact number, identity and payload of the assignment records.
func (f rankedFixture) checkClaim(t *testing.T, source uint64, node string, want map[string]any) {
	t.Helper()
	records, err := f.log.ReplayAfter(testContext(t), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Type != units.AssignedType || records[0].TaskID != "r:"+node {
		t.Fatalf("records after %d = %+v, want one assignment of %s", source, records, node)
	}
	checkAssignmentJSON(t, records[0].Payload, want)
}

// checkReopened checks the original assignment in show and the open unit's exact resume table.
func (f rankedFixture) checkReopened(t *testing.T, node string, want map[string]any) {
	t.Helper()
	id := "r:" + node
	out := captureStdoutUnits(t, func() {
		if err := runUnits([]string{"show", id, "--config", f.path}); err != nil {
			t.Fatal(err)
		}
	})
	var shown struct {
		ID, Node, Status, Lane string
		UpdatedAt              string          `json:"updated_at"`
		Original               json.RawMessage `json:"assignment"`
	}
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.ID != id || shown.Node != node || shown.Status != "claimed" {
		t.Fatalf("unexpected unit after reopen: %s", out)
	}
	checkAssignmentJSON(t, shown.Original, want)
	out = captureStdoutUnits(t, func() {
		if err := runUnits([]string{"resume", "--config", f.path}); err != nil {
			t.Fatal(err)
		}
	})
	const header = "id\tstatus\tupdated_at\tworktree\tbranch\tpr\tlane\trelaunch\n"
	wantTable := header + fmt.Sprintf("%s\tclaimed\t%s\t\t\t0\t%s\tsafe\n", id, shown.UpdatedAt, shown.Lane)
	if out != wantTable {
		t.Fatalf("resume = %q, want %q", out, wantTable)
	}
}

// TestRankedAssignmentReplay uses fresh copies for the automatic and operator choices.
func TestRankedAssignmentReplay(t *testing.T) {
	for _, tc := range []struct {
		arg, mode, node string
		progress        int
	}{{"--next", "next", "z", 2}, {"b", "named", "b", 0}} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newRankedFixture(t)
			f.chain(t, "zxyab")
			doc := frontierDocument{Version: "v1", Seq: 1}
			want := assignmentJSON(doc, expectedRow(tc.node, tc.progress, 0, true), tc.mode)
			out, err := f.assign(t, tc.arg)
			if err != nil {
				t.Fatal(err)
			}
			u := printedUnit(t, out)
			if u.Node != tc.node || u.By != "actor-1" || u.Lane != "lane-1" {
				t.Fatalf("assign %s = %+v", tc.arg, u)
			}
			f.checkReopened(t, tc.node, want)
			f.checkClaim(t, doc.Seq, tc.node, want)
			f.checkReleasedCost(t, tc.node, want)
		})
	}
}

// checkReleasedCost changes the current actor and lane, then checks the claim cost on release.
func (f rankedFixture) checkReleasedCost(t *testing.T, node string, original map[string]any) {
	t.Helper()
	if err := runUnits([]string{"set", "r:" + node, "--status=claimed", "--lane=other", "--by=other", "--config", f.path}); err != nil {
		t.Fatal(err)
	}
	f.checkReopened(t, node, original)
	if _, err := unitsGraphRun(t, "release", node, "--reason=retry", "--config", f.path); err != nil {
		t.Fatal(err)
	}
	for _, row := range f.read(t, 4).Rows {
		if row.ID == node {
			if row.Terms["cost"] != float64(1) {
				t.Fatalf("released %s terms = %+v, want cost 1", node, row.Terms)
			}
			return
		}
	}
	t.Fatalf("released node %s missing", node)
}

// TestLegacyAssignmentReplay appends a pre-ranking payload and checks both reopened views.
func TestLegacyAssignmentReplay(t *testing.T) {
	f := newRankedFixture(t)
	f.stage(t, "s")
	ev := eventlog.Event{Type: units.AssignedType, TaskID: "r:s", Payload: json.RawMessage(`{"node":"s","lane":"old","by":"operator"}`)}
	if _, err := f.log.Append(testContext(t), ev); err != nil {
		t.Fatal(err)
	}
	f.checkReopened(t, "s", map[string]any{"node": "s", "lane": "old", "by": "operator"})
	f.checkClaim(t, 1, "s", map[string]any{"node": "s", "lane": "old", "by": "operator"})
}

// TestUnitCheckpointAssignmentEvidence checks legacy and current snapshots of the same history.
// checkCheckpointUse verifies that legacy checkpoints are skipped and replayed
// and that current checkpoints are reused without divergence.
func checkCheckpointUse(t *testing.T, legacy bool, seq uint64, diverged bool, skipped int) {
	t.Helper()
	if diverged {
		t.Fatal("checkpoint use diverged")
	}
	if legacy {
		if seq != 0 || skipped == 0 {
			t.Fatalf("legacy checkpoint: want skip and replay (seq 0, skipped > 0), got seq %d, skipped %d", seq, skipped)
		}
		return
	}
	if seq != 2 {
		t.Fatalf("current checkpoint: want reuse at seq 2, got seq %d", seq)
	}
}

func TestUnitCheckpointAssignmentEvidence(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			f := newRankedFixture(t)
			ctx := testContext(t)
			const id = "r:s"
			f.writeAssignmentCheckpoint(t, ctx, id, legacy)
			svc, err := units.NewService(f.dir, f.signer, f.limits.Clock, f.limits)
			if err != nil {
				t.Fatal(err)
			}
			got, use, err := svc.ReplayCheckpointed(ctx)
			if err != nil {
				t.Fatal(err)
			}
			checkCheckpointUse(t, legacy, use.Seq, use.Diverged != nil, len(use.Skipped))
			checkCheckpointAssignment(t, got.Units[id])
			u, err := svc.Record(ctx, id, units.RecordPayload{Status: "implementing"})
			if err != nil {
				t.Fatal(err)
			}
			checkCheckpointAssignment(t, u)
		})
	}
}

// writeAssignmentCheckpoint appends an assignment and an edit, then checkpoints both records.
// A legacy checkpoint omits the original assignment evidence from its snapshot only.
func (f rankedFixture) writeAssignmentCheckpoint(t *testing.T, ctx context.Context, id string, legacy bool) {
	t.Helper()
	for _, ev := range []eventlog.Event{
		{Type: units.AssignedType, TaskID: id, Payload: json.RawMessage(`{"node":"s","lane":"original","by":"operator"}`)},
		{Type: "unit.recorded", TaskID: id, Payload: json.RawMessage(`{"status":"claimed","lane":"edited","by":"editor"}`)},
	} {
		if _, err := f.log.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	st, err := eventlog.ReplayFoldStable(ctx, f.dir, f.signer.PublicKey(), f.limits, units.FoldName, units.UnitReducer)
	if err != nil {
		t.Fatal(err)
	}
	if legacy {
		u := st.Units[id]
		u.Assignment = nil
		st.Units[id] = u
	}
	records, err := f.log.ReplayAfter(ctx, 1)
	if err != nil || len(records) != 1 {
		t.Fatalf("records after 1 = %d, %v", len(records), err)
	}
	hash := sha256.Sum256(fixtureCanonical(t, records[0]))
	if err := f.log.WriteCheckpoint(ctx, units.FoldName, 2, fmt.Sprintf("%x", hash), st); err != nil {
		t.Fatal(err)
	}
}

// checkCheckpointAssignment distinguishes sticky current fields from original evidence.
func checkCheckpointAssignment(t *testing.T, u eventlog.Unit) {
	t.Helper()
	if u.Lane != "edited" || u.By != "editor" || u.Assignment == nil ||
		u.Assignment.Lane != "original" || u.Assignment.By != "operator" || u.Assignment.Selection != nil {
		t.Fatalf("current fields or original assignment lost: unit=%+v assignment=%+v", u, u.Assignment)
	}
}

// TestUnitsRebuildCheckpoint exercises ordinary CLI reads and checked writes
// with a cap that permits rebuilding the prefix but not replaying prefix plus suffix.
func TestUnitsRebuildCheckpoint(t *testing.T) {
	f := newRankedFixture(t)
	ctx := testContext(t)
	const id = "r:s"
	f.writeAssignmentCheckpoint(t, ctx, id, true)
	legacy := readUnitsCheckpoint(t, f.dir, 2)
	publishStale := staleUnitsCheckpointPublisher(t, ctx, f, legacy)
	if _, err := f.log.Append(ctx, eventlog.Event{Type: "unit.note", TaskID: id, Payload: json.RawMessage(`{"from":"editor","text":"suffix"}`)}); err != nil {
		t.Fatal(err)
	}

	cfg := loadUnitsRebuildConfig(t, f, 2)
	svc, err := newUnitsService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Show(ctx, id); err == nil || !strings.Contains(err.Error(), "max replay records") {
		t.Fatalf("Show before maintenance = %v, want replay cap error", err)
	}
	args := []string{"rebuild-checkpoint", "--config", f.path}
	raceUnitsCheckpointRebuild(t, args, publishStale)
	if t.Failed() {
		return
	}
	repaired := readUnitsCheckpoint(t, f.dir, 2)
	if bytes.Equal(legacy, repaired) {
		t.Fatal("legacy checkpoint not repaired")
	}
	if err := publishStale(); err != nil {
		t.Fatal(err)
	}
	if err := runUnits(args); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(repaired, readUnitsCheckpoint(t, f.dir, 2)) {
		t.Fatal("delayed publication or repeated repair changed the repaired checkpoint")
	}
	checkUnitsRebuildResult(t, f, svc, id)
}

// staleUnitsCheckpointPublisher captures the legacy state and its signed boundary
// so the normal writer can attempt publication during and after repair.
func staleUnitsCheckpointPublisher(t *testing.T, ctx context.Context, f rankedFixture, legacy []byte) func() error {
	t.Helper()
	var cp eventlog.Checkpoint
	if err := json.Unmarshal(legacy, &cp); err != nil {
		t.Fatal(err)
	}
	var stale eventlog.State
	if err := json.Unmarshal(cp.State, &stale); err != nil {
		t.Fatal(err)
	}
	return func() error {
		return f.log.WriteCheckpoint(ctx, units.FoldName, cp.Seq, cp.Hash, stale)
	}
}

// TestUnitsRebuildCheckpointRefusesUnverifiedPrefix checks that failed maintenance
// leaves the signed checkpoint and HEAD byte-for-byte unchanged.
func TestUnitsRebuildCheckpointRefusesUnverifiedPrefix(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cap          int
		removePrefix bool
		want         string
	}{
		{name: "missing original record", cap: 2, removePrefix: true, want: "gap at seq 1"},
		{name: "configured replay cap", cap: 1, want: "max replay records 1 exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRankedFixture(t)
			ctx := testContext(t)
			f.writeAssignmentCheckpoint(t, ctx, "r:s", true)
			legacy := readUnitsCheckpoint(t, f.dir, 2)
			headPath := unitsRebuildHeadPath(t, f.dir)
			head := readUnitsFixtureFile(t, f.dir, headPath)
			loadUnitsRebuildConfig(t, f, tc.cap)
			if tc.removePrefix {
				removeUnitsRebuildPrefix(t, headPath)
			}
			err := runUnits([]string{"rebuild-checkpoint", "--config", f.path})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("rebuild-checkpoint = %v, want error containing %q", err, tc.want)
			}
			assertUnitsRebuildStateUnchanged(t, f.dir, headPath, legacy, head)
		})
	}
}

// assertUnitsRebuildStateUnchanged checks that a failed rebuild preserves the
// signed checkpoint at sequence 2 and HEAD byte for byte.
func assertUnitsRebuildStateUnchanged(t *testing.T, dir, headPath string, checkpoint, head []byte) {
	t.Helper()
	if !bytes.Equal(checkpoint, readUnitsCheckpoint(t, dir, 2)) {
		t.Error("failed rebuild changed the signed checkpoint")
	}
	if !bytes.Equal(head, readUnitsFixtureFile(t, dir, headPath)) {
		t.Error("failed rebuild changed HEAD")
	}
}

// removeUnitsRebuildPrefix removes record 1 but leaves the checkpoint boundary
// intact so repair selects the signed checkpoint and must verify the prefix.
func removeUnitsRebuildPrefix(t *testing.T, headPath string) {
	t.Helper()
	path := filepath.Join(filepath.Dir(headPath), fmt.Sprintf("%020d.json", 1))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// unitsRebuildHeadPath finds the HEAD written by the fixture without assuming
// the event directory's name.
func unitsRebuildHeadPath(t *testing.T, dir string) string {
	t.Helper()
	var path string
	err := filepath.WalkDir(dir, func(candidate string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == "HEAD.json" {
			if path != "" {
				return fmt.Errorf("multiple HEAD.json files under %s", dir)
			}
			path = candidate
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatalf("no HEAD.json under %s", dir)
	}
	return path
}

// loadUnitsRebuildConfig sets the replay cap in the fixture's config file and loads it.
func loadUnitsRebuildConfig(t *testing.T, f rankedFixture, maxReplayRecords int) config.Config {
	t.Helper()
	ctx := testContext(t)
	doc, err := config.ReadDocument(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	log, ok := doc["event_log"].(map[string]any)
	if !ok {
		t.Fatal("fixture config has no event_log object")
	}
	log["max_replay_records"] = maxReplayRecords
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EventLog.MaxReplayRecords != maxReplayRecords {
		t.Fatalf("loaded replay limit = %d, want %d", cfg.EventLog.MaxReplayRecords, maxReplayRecords)
	}
	return cfg
}

// raceUnitsCheckpointRebuild runs two CLI repairs alongside a stale publication.
func raceUnitsCheckpointRebuild(t *testing.T, args []string, publishStale func() error) {
	t.Helper()
	start := make(chan struct{})
	results := make(chan error, 3)
	for _, op := range []func() error{
		func() error { return runUnits(args) },
		func() error { return runUnits(args) },
		publishStale,
	} {
		go func(op func() error) {
			<-start
			results <- op()
		}(op)
	}
	close(start)
	for i := 0; i < 3; i++ {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
}

// readUnitsCheckpoint returns the exact publication bytes for the given sequence.
func readUnitsCheckpoint(t *testing.T, dir string, seq uint64) []byte {
	t.Helper()
	path := filepath.Join(dir, "checkpoints", units.FoldName, fmt.Sprintf("%020d.json", seq))
	return readUnitsFixtureFile(t, dir, path)
}

// readUnitsFixtureFile reads the exact bytes of path without allowing it to escape dir.
func readUnitsFixtureFile(t *testing.T, dir, path string) []byte {
	t.Helper()
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsLocal(rel) || rel == "." {
		t.Fatalf("fixture file %q is not below %q", path, dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Errorf("close fixture root %s: %v", dir, err)
		}
	}()
	body, err := root.ReadFile(rel)
	if err != nil {
		t.Fatalf("read fixture file %s: %v", path, err)
	}
	return body
}

// checkUnitsRebuildResult verifies that normal reads and checked writes use the
// repaired checkpoint and that repair did not append an event.
func checkUnitsRebuildResult(t *testing.T, f rankedFixture, svc *units.Service, id string) {
	t.Helper()
	checkUnitsRebuildShow(t, f, id)
	checkUnitsRebuildRecord(t, f, svc, id)
}

// checkUnitsRebuildShow verifies original assignment evidence and suffix replay
// through the CLI under the configured replay cap.
func checkUnitsRebuildShow(t *testing.T, f rankedFixture, id string) {
	t.Helper()
	out := captureStdoutUnits(t, func() {
		if err := runUnits([]string{"show", id, "--config", f.path}); err != nil {
			t.Fatal(err)
		}
	})
	shown := printedUnit(t, out)
	checkCheckpointAssignment(t, shown)
	if len(shown.Notes) != 1 || shown.Notes[0].Seq != 3 || shown.Notes[0].Text != "suffix" {
		t.Fatalf("suffix not applied: %+v", shown.Notes)
	}
}

// checkUnitsRebuildRecord verifies that a checked write preserves the original
// evidence and is the only event appended after the suffix.
func checkUnitsRebuildRecord(t *testing.T, f rankedFixture, svc *units.Service, id string) {
	t.Helper()
	ctx := testContext(t)
	u, err := svc.Record(ctx, id, units.RecordPayload{Status: "implementing"})
	if err != nil {
		t.Fatal(err)
	}
	checkCheckpointAssignment(t, u)
	if u.Status != "implementing" {
		t.Fatalf("checked Record status = %q", u.Status)
	}
	records, err := f.log.ReplayAfter(ctx, 3)
	if err != nil || len(records) != 1 || records[0].Seq != 4 || records[0].Type != "unit.recorded" {
		t.Fatalf("records after suffix = %+v, %v", records, err)
	}
}

// TestFrontierMembershipParity compares IDs, not order or ranking terms, at one head.
func TestFrontierMembershipParity(t *testing.T) {
	f := newRankedFixture(t)
	f.chain(t, "zxyab")
	nodes, err := f.graph.Frontier(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	plain := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		plain[node.ID] = true
	}
	rows := f.read(t, 1).Rows
	ranked := make(map[string]bool, len(rows))
	for _, row := range rows {
		ranked[row.ID] = true
	}
	want := map[string]bool{"a": true, "b": true, "z": true}
	if len(plain) != len(nodes) || len(ranked) != len(rows) || !reflect.DeepEqual(plain, want) || !reflect.DeepEqual(ranked, want) {
		t.Fatalf("membership: wrapper=%v ranked=%v want=%v", nodes, rows, want)
	}
}

// savedClaim prepares a next-mode claim from the first saved public row.
func (f rankedFixture) savedClaim(t *testing.T, doc frontierDocument) func(context.Context) error {
	t.Helper()
	return f.savedClaimWithMode(t, doc, "next")
}

// savedClaimWithMode passes the first saved public row and the given mode to the checked writer used by units assign.
func (f rankedFixture) savedClaimWithMode(t *testing.T, doc frontierDocument, mode string) func(context.Context) error {
	t.Helper()
	if len(doc.Rows) == 0 {
		t.Fatal("no row to select")
	}
	body, err := json.Marshal(assignmentJSON(doc, doc.Rows[0], mode))
	if err != nil {
		t.Fatal(err)
	}
	var p units.AssignPayload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	// Use the pre-existing payload type so the fixture also compiles on the baseline.
	writer, ok := any(f.graph).(interface {
		AssignUnit(context.Context, string, units.AssignPayload) (eventlog.Unit, error)
	})
	if !ok {
		t.Fatal("checked writer cannot accept a ranked selection")
	}
	return func(ctx context.Context) error {
		_, err := writer.AssignUnit(ctx, "r", p)
		return err
	}
}

// TestRankedAssignRejectsFalseFirstRow presents an eligible lower row with its exact terms as next.
func TestRankedAssignRejectsFalseFirstRow(t *testing.T) {
	f := newRankedFixture(t)
	f.chain(t, "zxyab")
	doc := f.read(t, 1)
	if len(doc.Rows) < 2 || doc.Rows[0].ID != "z" || doc.Rows[1].ID != "a" {
		t.Fatalf("unexpected rows: %+v", doc.Rows)
	}
	falseFirst := doc
	falseFirst.Rows = doc.Rows[1:]
	if err := f.savedClaim(t, falseFirst)(testContext(t)); err == nil {
		t.Fatal("accepted a lower-ranked row as next")
	}
	f.read(t, doc.Seq)
	if _, err := f.assign(t, "a"); err != nil {
		t.Fatalf("explicit assignment of the same row: %v", err)
	}
	f.checkClaim(t, doc.Seq, "a", assignmentJSON(doc, doc.Rows[1], "named"))
}

// TestRankedSameHeadRace gives two consumers the same read before either takes the lock.
func TestRankedSameHeadRace(t *testing.T) {
	f := newRankedFixture(t)
	f.chain(t, "zxyab")
	doc := f.read(t, 1)
	claims := []func(context.Context) error{f.savedClaim(t, doc), f.savedClaim(t, f.read(t, 1))}
	ctx := testContext(t)
	var wg sync.WaitGroup
	var errs [2]error
	for i := range claims {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = claims[i](ctx)
		}()
	}
	wg.Wait()
	codes := [2]int{commandExit(errs[0], io.Discard), commandExit(errs[1], io.Discard)}
	if codes != [2]int{0, 75} && codes != [2]int{75, 0} {
		t.Fatalf("racing claims: exit %v, errors %v", codes, errs)
	}
	want := assignmentJSON(doc, doc.Rows[0], "next")
	f.checkReopened(t, "z", want)
	f.checkClaim(t, doc.Seq, "z", want)
}

// TestRankedUnrelatedAppend rejects a stale read even when its winner remains eligible.
func TestRankedUnrelatedAppend(t *testing.T) {
	f := newRankedFixture(t)
	f.stage(t, "s")
	claim := f.savedClaim(t, f.read(t, 1))
	if _, err := f.log.Append(testContext(t), eventlog.Event{Type: "log.tick"}); err != nil {
		t.Fatal(err)
	}
	if err := claim(testContext(t)); commandExit(err, io.Discard) != 75 {
		t.Fatalf("stale eligible claim = %v, want exit 75", err)
	}
	doc := f.read(t, 2)
	if _, err := f.assign(t, "--next"); err != nil {
		t.Fatal(err)
	}
	f.checkClaim(t, 2, "s", assignmentJSON(doc, doc.Rows[0], "next"))
}

// fixtureCanonical produces the canonical signed bytes expected by the log reader.
func fixtureCanonical(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	body, err := jcs.Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// compactFixtureHead writes a signed graph base at the fixture's only record.
// Keeping the covered loose record also permits appending a tail with the normal writer.
func (f rankedFixture) compactFixtureHead(t *testing.T) {
	t.Helper()
	records, err := f.log.ReplayAfter(testContext(t), 0)
	if err != nil || len(records) != 1 {
		t.Fatalf("base fixture records = %d, %v", len(records), err)
	}
	st, err := f.graph.Replay(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	rec := records[0]
	hash := sha256.Sum256(fixtureCanonical(t, rec))
	base := map[string]any{
		"seq": rec.Seq, "hash": fmt.Sprintf("%x", hash),
		"states": map[string]eventlog.State{graphlog.FoldName: st, units.FoldName: {}},
	}
	proof, err := f.signer.Sign(corereceipt.Run{Command: eventlog.BaseCommand, Output: fixtureCanonical(t, base)})
	if err != nil {
		t.Fatal(err)
	}
	base["receipt"] = proof
	dir := filepath.Join(f.dir, "checkpoints", eventlog.BaseName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%020d.json", rec.Seq)), fixtureCanonical(t, base), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRankedCompactedBase checks a base without reducer calls, then an observed tail.
func TestRankedCompactedBase(t *testing.T) {
	f := newRankedFixture(t)
	f.stage(t, "a")
	f.compactFixtureHead(t)
	inherited := expectedRow("a", 0, 0, false)
	if rows := f.read(t, 1).Rows; !reflect.DeepEqual(rows, []frontierRow{inherited}) {
		t.Fatalf("base-only rows = %+v", rows)
	}
	f.stage(t, "z")
	doc := f.read(t, 2)
	if !reflect.DeepEqual(doc.Rows, []frontierRow{expectedRow("z", 0, 0, true), inherited}) {
		t.Fatalf("base plus tail = %+v", doc)
	}
	if _, err := f.assign(t, "a"); err != nil {
		t.Fatal(err)
	}
	f.checkClaim(t, 2, "a", assignmentJSON(doc, inherited, "named"))
}

// checkStaleClaimNoAppend runs a saved claim and checks that it fails without advancing the log.
func (f rankedFixture) checkStaleClaimNoAppend(t *testing.T, seq uint64, claim func(context.Context) error) {
	t.Helper()
	ctx := testContext(t)
	claimErr := claim(ctx)
	records, err := f.log.ReplayAfter(ctx, seq)
	if err != nil {
		t.Fatal(err)
	}
	if claimErr == nil || len(records) != 0 {
		t.Fatalf("stale claim = %v, appended %d records; want rejection without an append", claimErr, len(records))
	}
}

// TestRankedAssignSameHeadCompaction checks that a changed score invalidates an
// issued selection even when compaction preserves both the head and the winner.
func TestRankedAssignSameHeadCompaction(t *testing.T) {
	for _, tc := range []struct {
		mode, selector string
	}{
		{"next", "--next"},
		{"named", "a"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newRankedFixture(t)
			f.stage(t, "a")
			before := f.read(t, 1)
			if want := []frontierRow{expectedRow("a", 0, 0, true)}; !reflect.DeepEqual(before.Rows, want) {
				t.Fatalf("frontier before compaction = %+v, want %+v", before.Rows, want)
			}
			stale := f.savedClaimWithMode(t, before, tc.mode)

			f.compactFixtureHead(t)
			after := f.read(t, before.Seq)
			if want := []frontierRow{expectedRow("a", 0, 0, false)}; !reflect.DeepEqual(after.Rows, want) {
				t.Fatalf("frontier after compaction = %+v, want %+v", after.Rows, want)
			}

			f.checkStaleClaimNoAppend(t, before.Seq, stale)

			if _, err := f.assign(t, tc.selector); err != nil {
				t.Fatalf("fresh %s claim: %v", tc.mode, err)
			}
			want := assignmentJSON(after, after.Rows[0], tc.mode)
			f.checkClaim(t, after.Seq, "a", want)
			f.checkReopened(t, "a", want)
		})
	}
}
