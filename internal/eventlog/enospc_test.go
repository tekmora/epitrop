package eventlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
	"github.com/tekmora/epitrop/internal/atomicfile"
)

func readHeadRaw(t testing.TB, dir string) []byte {
	t.Helper()
	path := filepath.Join(eventsDir(dir), "HEAD.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func checkNoTmpFiles(t testing.TB, dir string) {
	t.Helper()
	entries, err := os.ReadDir(eventsDir(dir))
	if err != nil {
		t.Fatalf("readDir %s: %v", eventsDir(dir), err)
	}
	for i := 0; i < len(entries); i++ {
		if strings.Contains(entries[i].Name(), ".tmp-") {
			t.Fatalf("unexpected temp file left behind: %s", entries[i].Name())
		}
	}
}

func TestAppendHeadENOSPC(t *testing.T) {
	dir, signer, limits := testLog(t)
	w := openWriter(t, dir, signer, limits)
	ctx := testContext(t)
	if _, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)}); err != nil {
		t.Fatalf("Append(1) = %v, want nil", err)
	}
	if _, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-2", Payload: raw(`{"state":"open"}`)}); err != nil {
		t.Fatalf("Append(2) = %v, want nil", err)
	}
	realSync := syncFile
	syncFile = func(f *os.File) error {
		if strings.Contains(f.Name(), ".HEAD.json.tmp-") {
			return &os.PathError{Op: "fsync", Path: f.Name(), Err: syscall.ENOSPC}
		}
		return realSync(f)
	}
	t.Cleanup(func() { syncFile = realSync })

	_, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-3", Payload: raw(`{"state":"open"}`)})
	if err == nil {
		t.Fatal("Append(3) with HEAD ENOSPC = nil, want error")
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Append(3) error = %v, want errors.Is ENOSPC", err)
	}
	rec3Path := eventPath(eventsDir(dir), 3)
	if _, statErr := os.Stat(rec3Path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("record 3 file %s still exists: %v", rec3Path, statErr)
	}
	checkNoTmpFiles(t, dir)

	syncFile = realSync
	rec, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-3", Payload: raw(`{"state":"open"}`)})
	if err != nil {
		t.Fatalf("next Append(3) = %v, want nil", err)
	}
	if rec.Seq != 3 {
		t.Fatalf("rec.Seq = %d, want 3", rec.Seq)
	}
	st := replayTasks(t, dir, signer.PublicKey(), limits)
	if len(st.Tasks) != 3 {
		t.Fatalf("replayed tasks len = %d, want 3 (no duplicate)", len(st.Tasks))
	}
}

func TestAppendRecordENOSPC(t *testing.T) {
	dir, signer, limits := testLog(t)
	w := openWriter(t, dir, signer, limits)
	ctx := testContext(t)
	if _, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)}); err != nil {
		t.Fatalf("Append(1) = %v, want nil", err)
	}
	realSync := syncFile
	targetName := eventFileName(2)
	syncFile = func(f *os.File) error {
		if strings.Contains(filepath.Base(f.Name()), "."+targetName+".tmp-") {
			return &os.PathError{Op: "fsync", Path: f.Name(), Err: syscall.ENOSPC}
		}
		return realSync(f)
	}
	t.Cleanup(func() { syncFile = realSync })

	_, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-2", Payload: raw(`{"state":"open"}`)})
	if err == nil {
		t.Fatal("Append(2) with record ENOSPC = nil, want error")
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Append(2) error = %v, want errors.Is ENOSPC", err)
	}
	rec2Path := eventPath(eventsDir(dir), 2)
	if _, statErr := os.Stat(rec2Path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("record 2 file %s exists: %v", rec2Path, statErr)
	}
	checkNoTmpFiles(t, dir)

	syncFile = realSync
	rec, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-2", Payload: raw(`{"state":"open"}`)})
	if err != nil {
		t.Fatalf("next Append(2) = %v, want nil", err)
	}
	if rec.Seq != 2 {
		t.Fatalf("rec.Seq = %d, want 2", rec.Seq)
	}
}

func plantOrphanTail(t *testing.T, dir string, w *Writer) {
	t.Helper()
	ctx := testContext(t)
	if _, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)}); err != nil {
		t.Fatalf("Append(1) = %v", err)
	}
	if _, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-2", Payload: raw(`{"state":"open"}`)}); err != nil {
		t.Fatalf("Append(2) = %v", err)
	}
	head2Bytes := readHeadRaw(t, dir)
	if _, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-3", Payload: raw(`{"state":"open"}`)}); err != nil {
		t.Fatalf("Append(3) = %v", err)
	}
	headPath := filepath.Join(eventsDir(dir), "HEAD.json")
	if err := os.WriteFile(headPath, head2Bytes, 0o600); err != nil {
		t.Fatalf("restore HEAD.json: %v", err)
	}
}

func TestRepairTailOrphanPastHeadPlanted(t *testing.T) {
	dir, signer, limits := testLog(t)
	w := openWriter(t, dir, signer, limits)
	ctx := testContext(t)
	plantOrphanTail(t, dir, w)
	if _, err := Replay(ctx, dir, signer.PublicKey(), limits, TaskReducer); err == nil || !strings.Contains(err.Error(), "HEAD mismatch") {
		t.Fatalf("Replay() = %v, want HEAD mismatch error", err)
	}
	repaired, err := w.RepairTail(ctx)
	if err != nil || !repaired {
		t.Fatalf("RepairTail() = (%v, %v), want (true, nil)", repaired, err)
	}
	if _, statErr := os.Stat(eventPath(eventsDir(dir), 3)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("record 3 file still exists after repair: %v", statErr)
	}
	repairedAgain, err := w.RepairTail(ctx)
	if err != nil || repairedAgain {
		t.Fatalf("second RepairTail() = (%v, %v), want (false, nil)", repairedAgain, err)
	}
	rec, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-3", Payload: raw(`{"state":"open"}`)})
	if err != nil || rec.Seq != 3 {
		t.Fatalf("next Append() = (seq %d, %v), want (seq 3, nil)", rec.Seq, err)
	}
}

func TestRepairTailOrphanRecord1NoHead(t *testing.T) {
	dir, signer, limits := testLog(t)
	w := openWriter(t, dir, signer, limits)
	ctx := testContext(t)
	if _, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)}); err != nil {
		t.Fatalf("Append(1) = %v", err)
	}
	headPath := filepath.Join(eventsDir(dir), "HEAD.json")
	if err := os.Remove(headPath); err != nil {
		t.Fatalf("remove %s: %v", headPath, err)
	}
	repaired, err := w.RepairTail(ctx)
	if err != nil {
		t.Fatalf("RepairTail() = %v, want nil", err)
	}
	if !repaired {
		t.Fatal("RepairTail() = false, want true")
	}
	if _, statErr := os.Stat(eventPath(eventsDir(dir), 1)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("record 1 file still exists after repair: %v", statErr)
	}
	rec, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	if err != nil || rec.Seq != 1 {
		t.Fatalf("next Append() = (seq %d, %v), want (seq 1, nil)", rec.Seq, err)
	}
}

func TestRepairTailNegative_TwoRecordsPastHead(t *testing.T) {
	dir, signer, limits := testLog(t)
	w := openWriter(t, dir, signer, limits)
	ctx := testContext(t)
	if _, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)}); err != nil {
		t.Fatalf("Append(1) = %v", err)
	}
	head1Bytes := readHeadRaw(t, dir)
	if _, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-2", Payload: raw(`{"state":"open"}`)}); err != nil {
		t.Fatalf("Append(2) = %v", err)
	}
	if _, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-3", Payload: raw(`{"state":"open"}`)}); err != nil {
		t.Fatalf("Append(3) = %v", err)
	}
	headPath := filepath.Join(eventsDir(dir), "HEAD.json")
	if err := os.WriteFile(headPath, head1Bytes, 0o600); err != nil {
		t.Fatalf("restore HEAD: %v", err)
	}
	repaired, err := w.RepairTail(ctx)
	if err == nil {
		t.Fatal("RepairTail() with 2 records past HEAD = nil, want error")
	}
	if repaired {
		t.Fatal("RepairTail() = true, want false")
	}
	if _, err = os.Stat(eventPath(eventsDir(dir), 2)); err != nil {
		t.Fatalf("record 2 was removed: %v", err)
	}
	if _, err = os.Stat(eventPath(eventsDir(dir), 3)); err != nil {
		t.Fatalf("record 3 was removed: %v", err)
	}
}

func TestRepairTailNegative_ForeignKey(t *testing.T) {
	dir, signer, limits := testLog(t)
	w := openWriter(t, dir, signer, limits)
	ctx := testContext(t)
	if _, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)}); err != nil {
		t.Fatalf("Append(1) = %v", err)
	}
	foreignSeed := make([]byte, 32)
	for i := 0; i < len(foreignSeed); i++ {
		foreignSeed[i] = byte(i + 42)
	}
	foreignSigner := testSigner(t, foreignSeed, limits.Clock)
	head1Data := readHeadRaw(t, dir)
	writeRecordDirect(t, dir, foreignSigner, limits, 2, hashHex(readRecordBytes(t, dir, 1)), "task.created", "task-2", raw(`{"state":"open"}`))
	if err := os.WriteFile(filepath.Join(eventsDir(dir), "HEAD.json"), head1Data, 0o600); err != nil {
		t.Fatalf("restore HEAD: %v", err)
	}
	repaired, err := w.RepairTail(ctx)
	if err == nil {
		t.Fatal("RepairTail() with foreign key = nil, want error")
	}
	if repaired {
		t.Fatal("RepairTail() = true, want false")
	}
	if _, err = os.Stat(eventPath(eventsDir(dir), 2)); err != nil {
		t.Fatalf("record 2 was removed: %v", err)
	}
}

func TestRepairTailNegative_BrokenPrev(t *testing.T) {
	dir, signer, limits := testLog(t)
	w := openWriter(t, dir, signer, limits)
	ctx := testContext(t)
	if _, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)}); err != nil {
		t.Fatalf("Append(1) = %v", err)
	}
	head1Data := readHeadRaw(t, dir)
	brokenPrev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	writeRecordDirect(t, dir, signer, limits, 2, brokenPrev, "task.created", "task-2", raw(`{"state":"open"}`))
	if err := os.WriteFile(filepath.Join(eventsDir(dir), "HEAD.json"), head1Data, 0o600); err != nil {
		t.Fatalf("restore HEAD: %v", err)
	}
	repaired, err := w.RepairTail(ctx)
	if err == nil {
		t.Fatal("RepairTail() with broken prev = nil, want error")
	}
	if repaired {
		t.Fatal("RepairTail() = true, want false")
	}
	if _, err = os.Stat(eventPath(eventsDir(dir), 2)); err != nil {
		t.Fatalf("record 2 was removed: %v", err)
	}
}

func TestRepairTailNegative_RecordsAtOrBelowHeadKept(t *testing.T) {
	dir, signer, limits := testLog(t)
	w := openWriter(t, dir, signer, limits)
	ctx := testContext(t)
	for i := 1; i <= 3; i++ {
		ev := Event{Type: "task.created", TaskID: fmt.Sprintf("task-%d", i), Payload: raw(`{"state":"open"}`)}
		if _, err := w.Append(ctx, ev); err != nil {
			t.Fatalf("Append(%d) = %v", i, err)
		}
	}
	repaired, err := w.RepairTail(ctx)
	if err != nil {
		t.Fatalf("RepairTail() = %v, want nil", err)
	}
	if repaired {
		t.Fatal("RepairTail() = true, want false")
	}
	for i := 1; i <= 3; i++ {
		path := eventPath(eventsDir(dir), uint64(i))
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("record %d file %s missing: %v", i, path, err)
		}
	}
}

func readRecordBytes(t testing.TB, dir string, seq uint64) []byte {
	t.Helper()
	path := eventPath(eventsDir(dir), seq)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func writeRecordDirect(t testing.TB, dir string, signer *corereceipt.Signer, limits Limits, seq uint64, prev string, typ string, taskID string, payload json.RawMessage) {
	t.Helper()
	canPayload, err := canonicalPayload(payload)
	if err != nil {
		t.Fatalf("canonicalPayload: %v", err)
	}
	rec := Record{
		Seq:     seq,
		Prev:    prev,
		Time:    limits.Clock.Now().UTC().Format(time.RFC3339Nano),
		Type:    typ,
		TaskID:  taskID,
		Payload: canPayload,
	}
	unsigned, err := canonicalJSON(recordUnsigned(rec))
	if err != nil {
		t.Fatalf("canonicalJSON(unsigned): %v", err)
	}
	rec.Receipt, err = signer.Sign(corereceipt.Run{Command: AppendCommand, Output: unsigned})
	if err != nil {
		t.Fatalf("signer.Sign: %v", err)
	}
	full, err := canonicalJSON(rec)
	if err != nil {
		t.Fatalf("canonicalJSON(full): %v", err)
	}
	if err = os.WriteFile(eventPath(eventsDir(dir), seq), full, 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
}

func assertHeadSeq(t testing.TB, dir string, pub string, wantSeq uint64) {
	t.Helper()
	headRaw := readHeadRaw(t, dir)
	var h head
	if decErr := decodeHead(filepath.Join(eventsDir(dir), "HEAD.json"), headRaw, pub, &h); decErr != nil {
		t.Fatalf("decode HEAD.json: %v", decErr)
	}
	if h.Seq != wantSeq {
		t.Fatalf("HEAD.json seq = %d, want %d", h.Seq, wantSeq)
	}
}

func failHeadDirSync(t *testing.T, dir string) func() bool {
	t.Helper()
	evDir := eventsDir(dir)
	var headTempFsyncSeen bool
	var failedOnce bool
	realSync := syncFile
	syncFile = func(f *os.File) error {
		if strings.Contains(f.Name(), ".HEAD.json.tmp-") {
			headTempFsyncSeen = true
			return realSync(f)
		}
		if headTempFsyncSeen && !failedOnce && filepath.Clean(f.Name()) == filepath.Clean(evDir) {
			failedOnce = true
			return &os.PathError{Op: "fsync", Path: f.Name(), Err: syscall.ENOSPC}
		}
		return realSync(f)
	}
	t.Cleanup(func() { syncFile = realSync })
	return func() bool { return failedOnce }
}

func clearEventsDir(t testing.TB, dir string) {
	t.Helper()
	entries, err := os.ReadDir(eventsDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(entries); i++ {
		name := entries[i].Name()
		if isEventRecord(name) || name == "HEAD.json" {
			if rmErr := os.Remove(filepath.Join(eventsDir(dir), name)); rmErr != nil {
				t.Fatal(rmErr)
			}
		}
	}
}

func TestAppendHeadDirSyncFailure_RecordPreserved(t *testing.T) {
	dir, signer, limits := testLog(t)
	w := openWriter(t, dir, signer, limits)
	ctx := testContext(t)
	appendTasks(t, dir, w, 1)

	failed := failHeadDirSync(t, dir)
	_, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-2", Payload: raw(`{"state":"open"}`)})
	if err == nil || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Append(2) error = %v, want errors.Is ENOSPC", err)
	}
	if !failed() {
		t.Fatal("expected dir sync error after HEAD rename was never triggered")
	}

	assertHeadSeq(t, dir, signer.PublicKey(), 2)

	rec2Path := eventPath(eventsDir(dir), 2)
	if _, statErr := os.Stat(rec2Path); statErr != nil {
		t.Fatalf("record 2 file %s missing: %v", rec2Path, statErr)
	}

	syncFile = atomicfile.Sync
	rec3, err := w.Append(ctx, Event{Type: "task.created", TaskID: "task-3", Payload: raw(`{"state":"open"}`)})
	if err != nil || rec3.Seq != 3 {
		t.Fatalf("next Append(3) = (seq %d, %v), want (seq 3, nil)", rec3.Seq, err)
	}

	st, err := Replay(ctx, dir, signer.PublicKey(), limits, TaskReducer)
	if err != nil || len(st.Tasks) != 3 {
		t.Fatalf("Replay() = (%d tasks, %v), want (3 tasks, nil)", len(st.Tasks), err)
	}
}

func TestFirstHead_SegmentPresentNoHead_RecordKept(t *testing.T) {
	dir, signer, limits := testLog(t)
	w := openWriter(t, dir, signer, limits)
	ctx := testContext(t)
	withSegmentRecords(t, 1)
	appendTasks(t, dir, w, 2)
	if moved := rotate(t, w); moved == 0 {
		t.Fatal("Rotate() moved 0, want >= 1")
	}
	if err := os.Remove(eventPath(eventsDir(dir), 2)); err != nil {
		t.Fatalf("remove record 2: %v", err)
	}
	if err := os.Remove(filepath.Join(eventsDir(dir), "HEAD.json")); err != nil {
		t.Fatalf("remove HEAD.json: %v", err)
	}
	writeRecordDirect(t, dir, signer, limits, 1, zeroHash, "task.created", "task-1", raw(`{"state":"open"}`))
	rec1Path := eventPath(eventsDir(dir), 1)

	repaired, err := w.RepairTail(ctx)
	if err == nil || repaired {
		t.Fatalf("RepairTail() = (%v, %v), want (false, err)", repaired, err)
	}
	if !strings.Contains(err.Error(), "missing HEAD with existing segment") {
		t.Fatalf("RepairTail() error = %v, want error naming existing segment", err)
	}
	if _, statErr := os.Stat(rec1Path); statErr != nil {
		t.Fatalf("record 1 file was removed: %v", statErr)
	}
}

func TestFirstHead_BasePresentNoHead_RecordKept(t *testing.T) {
	_, dir, w, _ := compactedLog(t)
	ctx := testContext(t)
	clearEventsDir(t, dir)
	writeRecordDirect(t, dir, w.signer, w.limits, 1, zeroHash, "task.created", "task-1", raw(`{"state":"open"}`))
	rec1Path := eventPath(eventsDir(dir), 1)

	repaired, err := w.RepairTail(ctx)
	if err == nil || repaired {
		t.Fatalf("RepairTail() = (%v, %v), want (false, err)", repaired, err)
	}
	if !strings.Contains(err.Error(), "missing HEAD with existing base") {
		t.Fatalf("RepairTail() error = %v, want error naming existing base", err)
	}
	if _, statErr := os.Stat(rec1Path); statErr != nil {
		t.Fatalf("record 1 file was removed: %v", statErr)
	}
}
