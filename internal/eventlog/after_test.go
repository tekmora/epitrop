package eventlog

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

// allRecords is every record of the log at dir in order, as a full replay reads them.
func allRecords(t *testing.T, dir string, pub string, limits Limits) []Record {
	t.Helper()
	var recs []Record
	_, err := Replay(testContext(t), dir, pub, limits, func(st State, rec Record) (State, error) {
		recs = append(recs, rec)
		return st, nil
	})
	if err != nil {
		t.Fatalf("Replay() = %v, want nil", err)
	}
	return recs
}

func replayAfter(t *testing.T, dir string, pub string, limits Limits, after uint64) []Record {
	t.Helper()
	recs, err := ReplayAfter(testContext(t), dir, pub, limits, after)
	if err != nil {
		t.Fatalf("ReplayAfter(%d) = %v, want nil", after, err)
	}
	return recs
}

func assertSeqs(t *testing.T, recs []Record, first uint64, last uint64) {
	t.Helper()
	if first > last {
		if len(recs) != 0 {
			t.Fatalf("records = %d, want none", len(recs))
		}
		return
	}
	if uint64(len(recs)) != last-first+1 {
		t.Fatalf("records = %d, want seqs %d..%d", len(recs), first, last)
	}
	for i, rec := range recs {
		if rec.Seq != first+uint64(i) {
			t.Fatalf("record %d has seq %d, want %d", i, rec.Seq, first+uint64(i))
		}
	}
}

// fileLog writes n records as files, without segments.
func fileLog(t *testing.T, n int) (string, string, Limits) {
	t.Helper()
	dir, signer, limits := testLog(t)
	appendTasks(t, dir, openWriter(t, dir, signer, limits), n)
	return dir, signer.PublicKey(), limits
}

func TestReplayAfterReturnsTheRecordsAfterTheCursor(t *testing.T) {
	dir, pub, limits := fileLog(t, 5)
	got := replayAfter(t, dir, pub, limits, 2)
	assertSeqs(t, got, 3, 5)
	if want := allRecords(t, dir, pub, limits)[2:]; !reflect.DeepEqual(got, want) {
		t.Fatalf("ReplayAfter(2) = %+v, want the records a full replay reads, %+v", got, want)
	}
}

func TestReplayAfterZeroReturnsTheWholeLog(t *testing.T) {
	dir, pub, limits := fileLog(t, 3)
	assertSeqs(t, replayAfter(t, dir, pub, limits, 0), 1, 3)
}

func TestReplayAfterOnAnEmptyLogReturnsNothing(t *testing.T) {
	dir, signer, limits := testLog(t)
	openWriter(t, dir, signer, limits)
	assertSeqs(t, replayAfter(t, dir, signer.PublicKey(), limits, 0), 1, 0)
	if _, err := ReplayAfter(testContext(t), dir, signer.PublicKey(), limits, 1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReplayAfter(1) before the first append = %v, want a missing events directory", err)
	}
}

func TestReplayAfterAtHeadReturnsNothing(t *testing.T) {
	dir, pub, limits := fileLog(t, 3)
	assertSeqs(t, replayAfter(t, dir, pub, limits, 3), 4, 3)
}

func TestReplayAfterPastHeadFails(t *testing.T) {
	dir, pub, limits := fileLog(t, 3)
	_, err := ReplayAfter(testContext(t), dir, pub, limits, 4)
	assertErrContains(t, err, "cursor at seq 4", "no such record")
}

func TestReplayAfterReadsSegmentsAndFiles(t *testing.T) {
	dir, _, w, limits := rotatedLog(t)
	pub := w.signer.PublicKey()
	want := allRecords(t, dir, pub, limits)
	for _, after := range []uint64{0, 3, 4, 9, 10, 11} {
		got := replayAfter(t, dir, pub, limits, after)
		if len(got) != len(want[after:]) || len(got) > 0 && !reflect.DeepEqual(got, want[after:]) {
			t.Fatalf("ReplayAfter(%d) gave seqs %v, want %d..11", after, seqsOf(got), after+1)
		}
	}
}

func seqsOf(recs []Record) []uint64 {
	seqs := make([]uint64, 0, len(recs))
	for _, rec := range recs {
		seqs = append(seqs, rec.Seq)
	}
	return seqs
}

func TestReplayAfterBelowTheBaseFailsAsCompacted(t *testing.T) {
	_, dir, w, _ := compactedLog(t)
	for _, after := range []uint64{0, 3, 8} {
		_, err := ReplayAfter(testContext(t), dir, w.signer.PublicKey(), w.limits, after)
		if !errors.Is(err, ErrCursorCompacted) {
			t.Fatalf("ReplayAfter(%d) = %v, want ErrCursorCompacted", after, err)
		}
		assertErrContains(t, err, "base at seq 9")
	}
}

func TestReplayAfterFromTheBaseOrPastItReadsTheRest(t *testing.T) {
	_, dir, w, _ := compactedLog(t)
	pub := w.signer.PublicKey()
	assertSeqs(t, replayAfter(t, dir, pub, w.limits, 9), 10, 11)
	assertSeqs(t, replayAfter(t, dir, pub, w.limits, 10), 11, 11)
	assertSeqs(t, replayAfter(t, dir, pub, w.limits, 11), 12, 11)
}

func TestReplayAfterChecksTheLinkToTheCursorRecord(t *testing.T) {
	dir, pub, limits := fileLog(t, 3)
	replaceInFile(t, eventPath(eventsDir(dir), 2), "task-1", "task-9")
	_, err := ReplayAfter(testContext(t), dir, pub, limits, 2)
	assertErrContains(t, err, "seq 3", "prev mismatch")
}

func TestReplayAfterChecksTheRecordsItReturns(t *testing.T) {
	dir, pub, limits := fileLog(t, 3)
	replaceInFile(t, eventPath(eventsDir(dir), 3), "open", "done")
	_, err := ReplayAfter(testContext(t), dir, pub, limits, 1)
	assertErrContains(t, err, "seq 3", "verify receipt")
}

func TestReplayAfterChecksHead(t *testing.T) {
	dir, pub, limits := fileLog(t, 3)
	if err := os.Remove(eventPath(eventsDir(dir), 3)); err != nil {
		t.Fatalf("Remove(seq3) = %v, want nil", err)
	}
	_, err := ReplayAfter(testContext(t), dir, pub, limits, 1)
	assertErrContains(t, err, "HEAD mismatch")
}

func TestReplayAfterIsBoundedFromTheCursor(t *testing.T) {
	dir, pub, limits := fileLog(t, 5)
	limits.MaxReplayRecords = 2
	assertSeqs(t, replayAfter(t, dir, pub, limits, 3), 4, 5)
	_, err := ReplayAfter(testContext(t), dir, pub, limits, 2)
	assertErrContains(t, err, "max replay records 2 exceeded")
}

func TestReplayAfterChecksItsInputs(t *testing.T) {
	dir, pub, limits := fileLog(t, 1)
	if _, err := ReplayAfter(testContext(t), "", pub, limits, 0); err == nil {
		t.Fatal("ReplayAfter() with no dir = nil, want error")
	}
	if _, err := ReplayAfter(testContext(t), dir, "zz", limits, 0); err == nil {
		t.Fatal("ReplayAfter() with a bad key = nil, want error")
	}
	if _, err := ReplayAfter(testContext(t), dir, pub, Limits{}, 0); err == nil {
		t.Fatal("ReplayAfter() without a clock = nil, want error")
	}
}
