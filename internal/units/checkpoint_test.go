// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package units

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golusoris/golusoris/core/clock"
	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/gitexec"
)

func newTestServiceInDir(t *testing.T, dir string) (*Service, clock.Clock, *corereceipt.Signer, eventlog.Limits) {
	t.Helper()
	clk := clock.NewFake()
	seed := make([]byte, 32)
	for i := 0; i < len(seed); i++ {
		seed[i] = byte(i + 1)
	}
	signer, err := corereceipt.NewSignerFromSeed(seed, clk)
	if err != nil {
		t.Fatalf("NewSignerFromSeed: %v", err)
	}
	limits := eventlog.Limits{Clock: clk, LockTimeout: time.Second, MaxReplayRecords: 20000}
	svc, err := NewService(dir, signer, clk, limits)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, clk, signer, limits
}

func TestKillAtEachStatusBoundary(t *testing.T) {
	dir := t.TempDir()
	ctx := testContext(t)
	statuses := []string{"ready", "claimed", "implementing", "review", "landing", "landed"}

	for i := 0; i < len(statuses); i++ {
		svc, _, signer, limits := newTestServiceInDir(t, dir)
		status := statuses[i]
		unit, err := svc.Record(ctx, unitA, RecordPayload{Status: status})
		if err != nil {
			t.Fatalf("step %d Record(%s) = %v", i, status, err)
		}
		expectedSeq := uint64(i + 1)

		svcRestart, _, _, _ := newTestServiceInDir(t, dir)
		st, use, err := svcRestart.ReplayCheckpointed(ctx)
		if err != nil {
			t.Fatalf("step %d ReplayCheckpointed() = %v", i, err)
		}
		if use.Seq != expectedSeq {
			t.Fatalf("step %d checkpoint use seq = %d, want %d", i, use.Seq, expectedSeq)
		}

		fullSt, err := eventlog.Replay(ctx, dir, signer.PublicKey(), limits, UnitReducer)
		if err != nil {
			t.Fatalf("step %d full Replay() = %v", i, err)
		}
		assertStatesEqual(t, i, st, fullSt)
		if unit.Status != status {
			t.Fatalf("step %d unit.Status = %s, want %s", i, unit.Status, status)
		}
	}
}

func assertStatesEqual(t *testing.T, step int, got, want eventlog.State) {
	t.Helper()
	gotJSON, err := got.CanonicalJSON()
	if err != nil {
		t.Fatalf("step %d CanonicalJSON(got): %v", step, err)
	}
	wantJSON, err := want.CanonicalJSON()
	if err != nil {
		t.Fatalf("step %d CanonicalJSON(want): %v", step, err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("step %d state mismatch:\ngot:  %s\nwant: %s", step, gotJSON, wantJSON)
	}
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src) // #nosec G304 -- copyFlatDir supplies files from the temporary event log
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600) // #nosec G703 -- copyFlatDir supplies a destination in the temporary event log
}

func copyFlatDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for i := 0; i < len(entries); i++ {
		if entries[i].IsDir() {
			continue
		}
		s := filepath.Join(src, entries[i].Name())
		d := filepath.Join(dst, entries[i].Name())
		if err := copyFile(s, d); err != nil {
			return err
		}
	}
	return nil
}

type divergedFixture struct {
	dirA     string
	signerA  *corereceipt.Signer
	limitsA  eventlog.Limits
	fullStB  eventlog.State
	ckptPath string
	hashA1   string
	hashB1   string
}

func setupDivergedLogs(t *testing.T) divergedFixture {
	t.Helper()
	dirA, dirB := t.TempDir(), t.TempDir()
	ctx := testContext(t)
	svcA, _, signerA, limitsA := newTestServiceInDir(t, dirA)
	mustRecord(t, svcA, unitA, RecordPayload{Status: "ready"})

	ckptPath := filepath.Join(dirA, "checkpoints", "units", fmt.Sprintf("%020d.json", 1))
	if _, err := os.Stat(ckptPath); err != nil {
		t.Fatalf("checkpoint seq 1 missing: %v", err)
	}

	svcB, _, _, _ := newTestServiceInDir(t, dirB)
	mustRecord(t, svcB, unitA, RecordPayload{Status: "blocked"})
	hashA1 := fileHash(t, filepath.Join(dirA, "events", fmt.Sprintf("%020d.json", 1)))
	hashB1 := fileHash(t, filepath.Join(dirB, "events", fmt.Sprintf("%020d.json", 1)))

	fullStB, err := eventlog.Replay(ctx, dirB, signerA.PublicKey(), limitsA, UnitReducer)
	if err != nil {
		t.Fatalf("full Replay(B): %v", err)
	}
	if err := os.RemoveAll(filepath.Join(dirA, "events")); err != nil {
		t.Fatalf("RemoveAll(eventsA): %v", err)
	}
	if err := copyFlatDir(filepath.Join(dirB, "events"), filepath.Join(dirA, "events")); err != nil {
		t.Fatalf("copyFlatDir(eventsB, eventsA): %v", err)
	}
	return divergedFixture{dirA: dirA, signerA: signerA, limitsA: limitsA, fullStB: fullStB, ckptPath: ckptPath, hashA1: hashA1, hashB1: hashB1}
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) // #nosec G304 -- callers pass event files from temporary checkpoint fixtures
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// readDivergedRecord reads the record at seq and returns its log.diverged payload.
func readDivergedRecord(t *testing.T, eventsDir string, seq uint64) eventlog.Divergence {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(eventsDir, fmt.Sprintf("%020d.json", seq))) // #nosec G304 -- eventsDir is the temporary checkpoint fixture event directory
	if err != nil {
		t.Fatalf("read record %d: %v", seq, err)
	}
	var rec eventlog.Record
	if err = json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("unmarshal record %d: %v", seq, err)
	}
	if rec.Type != "log.diverged" {
		t.Fatalf("record %d type = %q, want log.diverged", seq, rec.Type)
	}
	var div eventlog.Divergence
	if err = json.Unmarshal(rec.Payload, &div); err != nil {
		t.Fatalf("unmarshal log.diverged payload: %v", err)
	}
	return div
}

func readHeadSeq(t *testing.T, eventsDir string) uint64 {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(eventsDir, "HEAD.json")) // #nosec G304 -- eventsDir is the temporary checkpoint fixture event directory
	if err != nil {
		t.Fatalf("read HEAD.json: %v", err)
	}
	var head struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		t.Fatalf("unmarshal HEAD.json: %v", err)
	}
	return head.Seq
}

func TestDivergedCopyRefusal(t *testing.T) {
	fix := setupDivergedLogs(t)
	ctx := testContext(t)

	svcRestart, _, _, _ := newTestServiceInDir(t, fix.dirA)
	st, use, err := svcRestart.ReplayCheckpointed(ctx)
	checkDivergedCheckpointFiles(t, fix, st, use, err)

	eventsDir := filepath.Join(fix.dirA, "events")
	headSeqAfterDiverged := checkDivergedRecordContent(t, eventsDir, fix)
	checkSecondReplayAppendsNothing(t, svcRestart, ctx, eventsDir, headSeqAfterDiverged)
}

func checkDivergedCheckpointFiles(t *testing.T, fix divergedFixture, st eventlog.State, use eventlog.CheckpointUse, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("ReplayCheckpointed on diverged log = %v", err)
	}
	if use.Diverged == nil {
		t.Fatal("use.Diverged = nil, want divergence reported")
	}
	if _, err := os.Stat(fix.ckptPath); !os.IsNotExist(err) {
		t.Fatalf("checkpoint %s still exists without .refused", fix.ckptPath)
	}
	if _, err := os.Stat(fix.ckptPath + ".refused"); err != nil {
		t.Fatalf("checkpoint %s.refused missing: %v", fix.ckptPath, err)
	}
	assertStatesEqual(t, 0, st, fix.fullStB)
}

func checkDivergedRecordContent(t *testing.T, eventsDir string, fix divergedFixture) uint64 {
	t.Helper()
	headSeqAfterDiverged := readHeadSeq(t, eventsDir)
	if headSeqAfterDiverged != 2 {
		t.Fatalf("headSeq after log.diverged = %d, want 2", headSeqAfterDiverged)
	}
	// The record names both heads: the checkpoint's and the log's as it was found.
	div := readDivergedRecord(t, eventsDir, 2)
	if div.Checkpoint.Name != "units" || div.Checkpoint.Seq != 1 || div.Checkpoint.Hash != fix.hashA1 {
		t.Fatalf("log.diverged checkpoint = %+v, want units seq 1 hash %s", div.Checkpoint, fix.hashA1)
	}
	if div.Log.Seq != 1 || div.Log.Hash != fix.hashB1 || div.Log.HashAtCheckpointSeq != fix.hashB1 {
		t.Fatalf("log.diverged log = %+v, want seq 1 hash %s at checkpoint seq", div.Log, fix.hashB1)
	}
	return headSeqAfterDiverged
}

func checkSecondReplayAppendsNothing(t *testing.T, svcRestart *Service, ctx context.Context, eventsDir string, headSeqAfterDiverged uint64) {
	t.Helper()
	// Second Replay appends nothing more.
	_, use2, err := svcRestart.ReplayCheckpointed(ctx)
	if err != nil {
		t.Fatalf("second ReplayCheckpointed: %v", err)
	}
	if use2.Diverged != nil {
		t.Fatalf("second replay diverged: %+v", use2.Diverged)
	}
	if headSeqSecond := readHeadSeq(t, eventsDir); headSeqSecond != headSeqAfterDiverged {
		t.Fatalf("second replay appended records: before=%d after=%d", headSeqAfterDiverged, headSeqSecond)
	}
}

// TestDivergenceLoggedOnceWhenTwoReplaysSeeIt stands for two writers whose replays both saw
// the diverged checkpoint before either renamed it: the second finds the file gone under the
// lock and appends nothing.
func TestDivergenceLoggedOnceWhenTwoReplaysSeeIt(t *testing.T) {
	fix := setupDivergedLogs(t)
	ctx := testContext(t)
	_, use, err := eventlog.ReplayCheckpointed(ctx, fix.dirA, fix.signerA.PublicKey(), fix.limitsA, "units", UnitReducer)
	if err != nil {
		t.Fatalf("eventlog.ReplayCheckpointed = %v", err)
	}
	if use.Diverged == nil {
		t.Fatal("use.Diverged = nil, want divergence reported")
	}
	svc, _, _, _ := newTestServiceInDir(t, fix.dirA)
	for i := 0; i < 2; i++ {
		if err = svc.handleDivergence(ctx, use.Diverged); err != nil {
			t.Fatalf("handleDivergence #%d = %v, want nil", i+1, err)
		}
	}
	if got := readHeadSeq(t, filepath.Join(fix.dirA, "events")); got != 2 {
		t.Fatalf("head seq = %d, want 2: one log.diverged record", got)
	}
}

func TestStatusChangeRotatesClosedRanges(t *testing.T) {
	dir := t.TempDir()
	ctx := testContext(t)
	svc, _, signer, limits := newTestServiceInDir(t, dir)
	fillTo259(t, svc)
	before, err := eventlog.Replay(ctx, dir, signer.PublicKey(), limits, UnitReducer)
	if err != nil {
		t.Fatalf("Replay() before rotation = %v", err)
	}
	if _, err = svc.Record(ctx, unitA, RecordPayload{Status: "claimed"}); err != nil {
		t.Fatalf("Record(claimed) = %v", err)
	}
	assertRotatedTo256(t, dir)
	after, err := eventlog.Replay(ctx, dir, signer.PublicKey(), limits, UnitReducer)
	if err != nil {
		t.Fatalf("Replay() after rotation = %v", err)
	}
	if len(after.Units[unitA].Notes) != len(before.Units[unitA].Notes) || after.Units[unitA].Status != "claimed" {
		t.Fatalf("after rotation unit = %d notes, status %q", len(after.Units[unitA].Notes), after.Units[unitA].Status)
	}
	_, use, err := svc.ReplayCheckpointed(ctx)
	if err != nil || use.Seq != 260 {
		t.Fatalf("ReplayCheckpointed() = seq %d, %v; want seq 260", use.Seq, err)
	}
}

// fillTo259 records unitA as ready, then two rounds of 128 notes and the inbox read that
// marks them: records 1 to 259, with no status change after the first.
func fillTo259(t *testing.T, svc *Service) {
	t.Helper()
	ctx := testContext(t)
	if _, err := svc.Record(ctx, unitA, RecordPayload{Status: "ready"}); err != nil {
		t.Fatalf("Record(ready) = %v", err)
	}
	for round := 0; round < 2; round++ {
		for i := 0; i < 128; i++ {
			if _, err := svc.Note(ctx, unitA, "tester", fmt.Sprintf("note %d.%d", round, i)); err != nil {
				t.Fatalf("Note(%d.%d) = %v", round, i, err)
			}
		}
		if _, err := svc.Inbox(ctx, unitA); err != nil {
			t.Fatalf("Inbox(round %d) = %v", round, err)
		}
	}
}

func assertRotatedTo256(t *testing.T, dir string) {
	t.Helper()
	segment := filepath.Join(dir, "events", "segments", fmt.Sprintf("%020d-%020d.jsonl", 1, 256))
	if _, err := os.Stat(segment); err != nil {
		t.Fatalf("segment after status change: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "events", fmt.Sprintf("%020d.json", 1))); !os.IsNotExist(err) {
		t.Fatalf("record 1 after rotation: stat = %v, want not exist", err)
	}
}

func TestStatusChangeRecordsTheLogInGit(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "--quiet")
	dir := filepath.Join(repo, ".epitrop")
	ctx := testContext(t)
	svc, _, _, _ := newTestServiceInDir(t, dir)
	svc.RecordInGit(eventlog.GitRecord{Ref: "refs/epitrop/events", AuthorName: "epitrop", AuthorEmail: "epitrop@example.invalid", Timeout: 30 * time.Second})
	if _, err := svc.Record(ctx, unitA, RecordPayload{Status: "ready"}); err != nil {
		t.Fatalf("Record(ready) = %v", err)
	}
	first := gitIn(t, repo, "rev-parse", "refs/epitrop/events")
	if _, err := svc.Note(ctx, unitA, "tester", "no status change"); err != nil {
		t.Fatalf("Note() = %v", err)
	}
	if got := gitIn(t, repo, "rev-parse", "refs/epitrop/events"); got != first {
		t.Fatalf("a note moved the ref to %s; only a checkpoint commits", got)
	}
	if _, err := svc.Record(ctx, unitA, RecordPayload{Status: "claimed"}); err != nil {
		t.Fatalf("Record(claimed) = %v", err)
	}
	second := gitIn(t, repo, "rev-parse", "refs/epitrop/events")
	if parent := gitIn(t, repo, "rev-parse", second+"^"); parent != first {
		t.Fatalf("the status change's commit follows %s, want %s", parent, first)
	}
	files := gitIn(t, repo, "ls-tree", "-r", "--name-only", second)
	want := "checkpoints/units/00000000000000000001.json\ncheckpoints/units/00000000000000000003.json\n" +
		"events/00000000000000000001.json\nevents/00000000000000000002.json\nevents/00000000000000000003.json\nevents/HEAD.json"
	if files != want {
		t.Fatalf("the commit holds\n%s\nwant\n%s", files, want)
	}
}

func TestStatusChangeWithoutCompactKeepsTheSegments(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "--quiet")
	dir := filepath.Join(repo, ".epitrop")
	ctx := testContext(t)
	svc, _, _, _ := newTestServiceInDir(t, dir)
	svc.RecordInGit(eventlog.GitRecord{Ref: "refs/epitrop/events", AuthorName: "epitrop", AuthorEmail: "epitrop@example.invalid", Timeout: 30 * time.Second})
	fillASegment(ctx, t, svc)
	for _, status := range []string{"claimed", "implementing"} {
		if _, err := svc.Record(ctx, unitA, RecordPayload{Status: status}); err != nil {
			t.Fatalf("Record(%s) = %v", status, err)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "events", "segments")); err != nil || len(entries) == 0 {
		t.Fatalf("segments without compaction = %d (%v), want kept", len(entries), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "checkpoints", eventlog.BaseName)); !os.IsNotExist(err) {
		t.Fatalf("base without compaction = %v, want none", err)
	}
}

func TestStatusChangeCompactsTheLogWhenAsked(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "--quiet")
	dir := filepath.Join(repo, ".epitrop")
	ctx := testContext(t)
	svc, _, signer, limits := newTestServiceInDir(t, dir)
	svc.RecordInGit(eventlog.GitRecord{Ref: "refs/epitrop/events", AuthorName: "epitrop", AuthorEmail: "epitrop@example.invalid", Timeout: 30 * time.Second})
	svc.CompactWith([]eventlog.Fold{Fold()})
	fillASegment(ctx, t, svc)
	// The status change rotates the notes into a segment, commits it, and only then compacts
	// it away: a compaction before the commit would find no commit holding the segment.
	for _, status := range []string{"claimed", "implementing"} {
		if _, err := svc.Record(ctx, unitA, RecordPayload{Status: status}); err != nil {
			t.Fatalf("Record(%s) = %v", status, err)
		}
		if bases := entryCount(t, filepath.Join(dir, "checkpoints", eventlog.BaseName)); bases != 1 {
			t.Fatalf("bases after %s = %d, want one", status, bases)
		}
		if segs := entryCount(t, filepath.Join(dir, "events", "segments")); segs != 0 {
			t.Fatalf("segments after %s = %d, want none", status, segs)
		}
	}
	if _, err := eventlog.Replay(ctx, dir, signer.PublicKey(), limits, UnitReducer); err == nil {
		t.Fatal("Replay() without a fold on a compacted log = nil, want error")
	}
	got, err := svc.Show(ctx, unitA)
	if err != nil {
		t.Fatalf("Show() on a compacted log = %v", err)
	}
	if got.Status != "implementing" {
		t.Fatalf("status on a compacted log = %q, want implementing", got.Status)
	}
}

// fillASegment records unitA ready and then one segment's worth of notes, which write no
// checkpoint.
func fillASegment(ctx context.Context, t *testing.T, svc *Service) {
	t.Helper()
	if _, err := svc.Record(ctx, unitA, RecordPayload{Status: "ready"}); err != nil {
		t.Fatalf("Record(ready) = %v", err)
	}
	// One segment holds 256 records.
	for i := 0; i < 256; i++ {
		if _, err := svc.Note(ctx, unitA, "tester", fmt.Sprintf("note %d", i)); err != nil {
			t.Fatalf("Note(%d) = %v", i, err)
		}
	}
}

// entryCount returns the number of entries in dir.
func entryCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s) = %v", dir, err)
	}
	return len(entries)
}

// gitIn runs git in dir without the machine's own settings.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitexec.Run(testContext(t), dir, gitexec.Env(os.Environ()), nil, args...)
	if err != nil {
		t.Fatalf("git %v = %v", args, err)
	}
	return out
}
