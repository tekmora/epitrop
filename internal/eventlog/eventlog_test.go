package eventlog

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/golusoris/golusoris/core/clock"
	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
)

func TestAppendReplayRoundTrip(t *testing.T) {
	dir, signer, limits := testLog(t)
	rec1 := appendEvent(t, dir, signer, limits, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open","title":"Write log"}`)})
	rec2 := appendEvent(t, dir, signer, limits, Event{Type: "task.state_changed", TaskID: "task-1", Payload: raw(`{"state":"done"}`)})
	if rec1.Seq != 1 || rec2.Seq != 2 {
		t.Fatalf("seqs = %d,%d, want 1,2", rec1.Seq, rec2.Seq)
	}
	st := replayTasks(t, dir, signer.PublicKey(), limits)
	if st.Tasks["task-1"].State != "done" {
		t.Fatalf("state = %+v, want task-1 done", st)
	}
}

func TestReplayTwiceIdenticalJCS(t *testing.T) {
	dir, signer, limits := testLog(t)
	appendEvent(t, dir, signer, limits, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	appendEvent(t, dir, signer, limits, Event{Type: "task.created", TaskID: "task-2", Payload: raw(`{"state":"ready"}`)})
	first := replayStateBytes(t, dir, signer.PublicKey(), limits)
	second := replayStateBytes(t, dir, signer.PublicKey(), limits)
	if !bytes.Equal(first, second) {
		t.Fatalf("replay bytes differ\nfirst=%s\nsecond=%s", first, second)
	}
	want := `{"tasks":{"task-1":{"id":"task-1","payload":{"state":"open"},"state":"open"},"task-2":{"id":"task-2","payload":{"state":"ready"},"state":"ready"}}}`
	if string(first) != want {
		t.Fatalf("replay JCS = %s, want %s", first, want)
	}
}

func TestGoldenFixtureReplay(t *testing.T) {
	dir := filepath.Join("testdata", "golden")
	limits := testLimits()
	pub := strings.TrimSpace(readText(t, filepath.Join(dir, "public_key.txt")))
	st := replayTasks(t, dir, pub, limits)
	got, err := st.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON() = %v, want nil", err)
	}
	want := strings.TrimSpace(readText(t, filepath.Join(dir, "state.jcs")))
	if string(got) != want {
		t.Fatalf("golden state = %s, want %s", got, want)
	}
}

func TestTamperedPayloadByteNamesSeq(t *testing.T) {
	dir, signer, limits := testLog(t)
	appendEvent(t, dir, signer, limits, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	replaceInFile(t, eventPath(eventsDir(dir), 1), "open", "done")
	_, err := Replay(testContext(t), dir, signer.PublicKey(), limits, TaskReducer)
	assertErrContains(t, err, "seq 1", "verify receipt")
}

func TestTornFileNamesSeqAndFile(t *testing.T) {
	dir, signer, limits := testLog(t)
	appendEvent(t, dir, signer, limits, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	path := eventPath(eventsDir(dir), 1)
	data := []byte(readText(t, path))
	writeFile(t, path, data[:len(data)/2], 0o600)
	_, err := Replay(testContext(t), dir, signer.PublicKey(), limits, TaskReducer)
	assertErrContains(t, err, "seq 1", "00000000000000000001.json", "canonicalize")
}

func TestGapMissingMiddleFile(t *testing.T) {
	dir, signer, limits := testLog(t)
	appendEvent(t, dir, signer, limits, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	appendEvent(t, dir, signer, limits, Event{Type: "task.state_changed", TaskID: "task-1", Payload: raw(`{"state":"ready"}`)})
	appendEvent(t, dir, signer, limits, Event{Type: "task.state_changed", TaskID: "task-1", Payload: raw(`{"state":"done"}`)})
	if err := os.Remove(eventPath(eventsDir(dir), 2)); err != nil {
		t.Fatalf("Remove(seq2) = %v, want nil", err)
	}
	_, err := Replay(testContext(t), dir, signer.PublicKey(), limits, TaskReducer)
	assertErrContains(t, err, "gap at seq 2")
}

func TestDeletedTailHeadMismatch(t *testing.T) {
	dir, signer, limits := testLog(t)
	appendEvent(t, dir, signer, limits, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	appendEvent(t, dir, signer, limits, Event{Type: "task.state_changed", TaskID: "task-1", Payload: raw(`{"state":"done"}`)})
	if err := os.Remove(eventPath(eventsDir(dir), 2)); err != nil {
		t.Fatalf("Remove(seq2) = %v, want nil", err)
	}
	_, err := Replay(testContext(t), dir, signer.PublicKey(), limits, TaskReducer)
	assertErrContains(t, err, "HEAD mismatch")
}

func TestNonCanonicalRecordRefused(t *testing.T) {
	dir, signer, limits := testLog(t)
	appendEvent(t, dir, signer, limits, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	path := eventPath(eventsDir(dir), 1)
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(readText(t, path)), "", "  "); err != nil {
		t.Fatalf("Indent(record) = %v, want nil", err)
	}
	writeFile(t, path, pretty.Bytes(), 0o600)
	_, err := Replay(testContext(t), dir, signer.PublicKey(), limits, TaskReducer)
	assertErrContains(t, err, "non-canonical")
}

func TestWrongPublicKeyFailsAtSeqOne(t *testing.T) {
	dir, signer, limits := testLog(t)
	appendEvent(t, dir, signer, limits, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	wrong := testSigner(t, bytes.Repeat([]byte{9}, 32), limits.Clock).PublicKey()
	_, err := Replay(testContext(t), dir, wrong, limits, TaskReducer)
	assertErrContains(t, err, "seq 1", "public key")
}

func TestRecordOver64KiBRefusedAtAppend(t *testing.T) {
	dir, signer, limits := testLog(t)
	large := `{"state":"open","blob":"` + strings.Repeat("x", MaxRecordBytes) + `"}`
	writer := openWriter(t, dir, signer, limits)
	_, err := writer.Append(testContext(t), Event{Type: "task.created", TaskID: "task-1", Payload: raw(large)})
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("Append(large) = %v, want ErrRecordTooLarge", err)
	}
}

func TestLockTimeoutFailsClosed(t *testing.T) {
	dir, signer, limits := testLog(t)
	if err := os.MkdirAll(eventsDir(dir), 0o750); err != nil {
		t.Fatalf("MkdirAll(events) = %v, want nil", err)
	}
	lock, err := acquireLock(testContext(t), eventsDir(dir), time.Second)
	if err != nil {
		t.Fatalf("acquireLock() = %v, want nil", err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Fatalf("lock.Close() = %v, want nil", err)
		}
	}()
	limits.LockTimeout = 20 * time.Millisecond
	_, err = openWriter(t, dir, signer, limits).Append(testContext(t), Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("Append(locked) = %v, want ErrLockTimeout", err)
	}
}

// testQueueLockTimeout is the lock timeout of the tests that queue writers. It only ends a
// failing run: the last of n queued writers waits for n-1 appends of four syncs each, and one
// second was not enough for sixteen of them on a disk under write load (#83).
const testQueueLockTimeout = time.Minute

// slowSync makes every sync of a file or directory take at least d, as a disk under write
// load does. The real sync is back when the test ends.
func slowSync(t *testing.T, d time.Duration) {
	t.Helper()
	real := syncFile
	syncFile = func(f *os.File) error {
		time.Sleep(d)
		return real(f)
	}
	t.Cleanup(func() { syncFile = real })
}

// appendConcurrently starts total writers at once, one record each, and returns their results.
// The writers' context outlasts limits.LockTimeout: a cancelled context ends the lock wait
// too, so a shorter one would cut every queue at its own deadline instead.
func appendConcurrently(t testing.TB, dir string, signer *corereceipt.Signer, limits Limits, total int) []error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), limits.LockTimeout+10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ev := Event{Type: "task.created", TaskID: "task-" + jsonInt(id), Payload: raw(`{"state":"open"}`)}
			_, err := openWriter(t, dir, signer, limits).Append(ctx, ev)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	out := make([]error, 0, total)
	for err := range errs {
		out = append(out, err)
	}
	return out
}

// requireContiguous fails unless the log holds exactly the records 1 to want.
func requireContiguous(t testing.TB, dir string, want int) {
	t.Helper()
	seqs := eventSeqs(t, eventsDir(dir))
	if len(seqs) != want {
		t.Fatalf("records = %v, want %d", seqs, want)
	}
	for i, seq := range seqs {
		if seq != uint64(i+1) {
			t.Fatalf("records = %v, want 1 to %d without a gap", seqs, want)
		}
	}
}

func TestConcurrentAppendersContiguous(t *testing.T) {
	dir, signer, limits := testLog(t)
	limits.LockTimeout = testQueueLockTimeout
	const total = 16
	errs := appendConcurrently(t, dir, signer, limits, total)
	for i := 0; i < len(errs); i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent Append() = %v, want nil", errs[i])
		}
	}
	requireContiguous(t, dir, total)
}

// BenchmarkQueuedAppenders measures the queue the lock timeout has to cover: sixteen writers
// start at once on a fresh log, one record each, on the disk that holds the temporary
// directory. ns/append is the time the whole queue took divided by the writers, the figure
// docs/architecture-v2.md quotes. A benchmark measures and asserts no speed; it fails only
// when a writer times out or the log has a gap.
func BenchmarkQueuedAppenders(b *testing.B) {
	const writers = 16
	limits := testLimits()
	limits.LockTimeout = testQueueLockTimeout
	signer := testSigner(b, testSeed(), limits.Clock)
	for b.Loop() {
		b.StopTimer()
		dir := b.TempDir()
		b.StartTimer()
		errs := appendConcurrently(b, dir, signer, limits, writers)
		b.StopTimer()
		for i := 0; i < len(errs); i++ {
			if errs[i] != nil {
				b.Fatalf("queued Append() = %v, want nil", errs[i])
			}
		}
		requireContiguous(b, dir, writers)
		b.StartTimer()
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*writers), "ns/append")
}

// On a disk where a sync is slow, a lock timeout that covers the queue lets every writer
// through. Eight writers at four syncs of 10 ms each: the last one waits about 280 ms.
func TestQueuedAppendersPassWhenTheLockTimeoutCoversTheQueue(t *testing.T) {
	dir, signer, limits := testLog(t)
	slowSync(t, 10*time.Millisecond)
	limits.LockTimeout = testQueueLockTimeout
	const total = 8
	errs := appendConcurrently(t, dir, signer, limits, total)
	for i := 0; i < len(errs); i++ {
		if errs[i] != nil {
			t.Fatalf("queued Append() = %v, want nil", errs[i])
		}
	}
	requireContiguous(t, dir, total)
}

// The same queue against a lock timeout of 100 ms: at 40 ms an append, the writers behind
// the first few wait longer than that. They fail closed with the lock timeout, and what the
// others appended is whole and without a gap.
func TestQueuedAppendersFailClosedWhenTheLockTimeoutIsShorterThanTheQueue(t *testing.T) {
	dir, signer, limits := testLog(t)
	slowSync(t, 10*time.Millisecond)
	limits.LockTimeout = 100 * time.Millisecond
	const total = 8
	errs := appendConcurrently(t, dir, signer, limits, total)
	timedOut := 0
	for i := 0; i < len(errs); i++ {
		switch {
		case errors.Is(errs[i], ErrLockTimeout):
			timedOut++
		case errs[i] != nil:
			t.Fatalf("queued Append() = %v, want nil or a lock timeout", errs[i])
		}
	}
	if timedOut == 0 {
		t.Fatalf("no writer timed out, want at least one: %d writers at 40 ms each cannot all wait less than 100 ms", total)
	}
	requireContiguous(t, dir, total-timedOut)
}

// Sixteen queued writers on a real disk under load must all commit without timing out or
// leaving gaps, and the test logs what an append cost (#83, #146). Under the workflow's
// load sixteen appends have taken up to 15 s, past the default timeout, so the test waits as
// the docs advise for a slow disk: with a timeout that only ends a failing run. The test
// is gated by EPITROP_EVENTLOG_LOAD_TEST=1 because it needs real filesystem syncs under
// write load; the eventlog-load.yml workflow runs it against a btrfs mount under stress-ng.
func TestQueuedAppendersUnderLoad(t *testing.T) {
	if os.Getenv("EPITROP_EVENTLOG_LOAD_TEST") != "1" {
		t.Skip("real disk under load, run by workflow eventlog-load.yml")
	}
	const lockTimeout = 60 * time.Second
	dir, signer, limits := testLog(t)
	limits.LockTimeout = lockTimeout
	const total = 16
	start := time.Now()
	errs := appendConcurrently(t, dir, signer, limits, total)
	elapsed := time.Since(start)
	t.Logf("%d queued appends took %s, %s an append", total, elapsed.Round(time.Millisecond), (elapsed / total).Round(time.Millisecond))
	for i := 0; i < len(errs); i++ {
		if errs[i] != nil {
			t.Fatalf("queued Append() = %v, want nil", errs[i])
		}
	}
	requireContiguous(t, dir, total)
}

// An append costs four syncs: the record's file and its directory, then HEAD's file and the
// directory again. The cost of queued writers in the docs is counted from this number, and
// the slow-disk tests slow down exactly these calls.
func TestAnAppendSyncsFourTimes(t *testing.T) {
	dir, signer, limits := testLog(t)
	calls := 0
	real := syncFile
	syncFile = func(f *os.File) error {
		calls++
		return real(f)
	}
	t.Cleanup(func() { syncFile = real })
	if _, err := openWriter(t, dir, signer, limits).Append(testContext(t), Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)}); err != nil {
		t.Fatalf("Append() = %v, want nil", err)
	}
	if calls != 4 {
		t.Fatalf("one append synced %d times, want 4", calls)
	}
}

// The lock timeout bounds the wait for the lock, not the append: a lone writer never waits,
// so the shortest timeout is enough for it however slow the disk is.
func TestLoneAppenderNeedsNoLockWaitOnASlowDisk(t *testing.T) {
	dir, signer, limits := testLog(t)
	slowSync(t, 10*time.Millisecond)
	limits.LockTimeout = time.Millisecond
	if _, err := openWriter(t, dir, signer, limits).Append(testContext(t), Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)}); err != nil {
		t.Fatalf("lone Append() = %v, want nil", err)
	}
	requireContiguous(t, dir, 1)
}

func TestKeyInRepoTreeRefused(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatalf("Mkdir(.git) = %v, want nil", err)
	}
	keyPath := filepath.Join(repo, "seed.hex")
	writeKey(t, keyPath, 0o600)
	_, err := NewSignerFromKeyFile(testContext(t), keyPath, filepath.Join(repo, ".epitrop"), testLimits().Clock)
	assertErrContains(t, err, "inside git working tree")
}

func TestKeyMode0644Refused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Go reports 0666 for every writable file on Windows; the mode check does not run there")
	}
	base := t.TempDir()
	keyPath := filepath.Join(base, "seed.hex")
	writeKey(t, keyPath, 0o644)
	_, err := NewSignerFromKeyFile(testContext(t), keyPath, filepath.Join(t.TempDir(), "epitrop-eventlog-test"), testLimits().Clock)
	assertErrContains(t, err, "group or other")
}

func TestKeyModeWindowsAccepts0666(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-specific test: windows reports 0666 for all files")
	}
	base := t.TempDir()
	keyPath := filepath.Join(base, "seed.hex")
	writeKey(t, keyPath, 0o666)
	signer, err := NewSignerFromKeyFile(testContext(t), keyPath, filepath.Join(base, "eventlog"), testLimits().Clock)
	if err != nil {
		t.Fatalf("NewSignerFromKeyFile() = %v, want nil on windows", err)
	}
	if signer == nil {
		t.Fatal("signer is nil, want non-nil")
	}
}

func TestMaxReplayRecordsExceeded(t *testing.T) {
	dir, signer, limits := testLog(t)
	appendEvent(t, dir, signer, limits, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	appendEvent(t, dir, signer, limits, Event{Type: "task.state_changed", TaskID: "task-1", Payload: raw(`{"state":"done"}`)})
	limits.MaxReplayRecords = 1
	_, err := Replay(testContext(t), dir, signer.PublicKey(), limits, TaskReducer)
	assertErrContains(t, err, "max replay records")
}

func TestUnknownEventTypeFailsWithPosition(t *testing.T) {
	dir, signer, limits := testLog(t)
	appendEvent(t, dir, signer, limits, Event{Type: "task.unknown", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	_, err := Replay(testContext(t), dir, signer.PublicKey(), limits, TaskReducer)
	assertErrContains(t, err, "seq 1", "unknown event type")
}

func testLog(t testing.TB) (string, *corereceipt.Signer, Limits) {
	t.Helper()
	limits := testLimits()
	return t.TempDir(), testSigner(t, testSeed(), limits.Clock), limits
}

func testLimits() Limits {
	return Limits{Clock: clock.NewFake(), LockTimeout: time.Second, MaxReplayRecords: 1000, MaxRecordBytes: MaxRecordBytes}
}

func testSigner(t testing.TB, seed []byte, clk clock.Clock) *corereceipt.Signer {
	t.Helper()
	signer, err := corereceipt.NewSignerFromSeed(seed, clk)
	if err != nil {
		t.Fatalf("NewSignerFromSeed() = %v, want nil", err)
	}
	return signer
}

func testSeed() []byte {
	seed := make([]byte, 32)
	for i := 0; i < len(seed); i++ {
		seed[i] = byte(i + 1)
	}
	return seed
}

func openWriter(t testing.TB, dir string, signer *corereceipt.Signer, limits Limits) *Writer {
	t.Helper()
	writer, err := Open(dir, signer, limits)
	if err != nil {
		t.Fatalf("Open() = %v, want nil", err)
	}
	return writer
}

func appendEvent(t *testing.T, dir string, signer *corereceipt.Signer, limits Limits, ev Event) Record {
	t.Helper()
	rec, err := openWriter(t, dir, signer, limits).Append(testContext(t), ev)
	if err != nil {
		t.Fatalf("Append() = %v, want nil", err)
	}
	return rec
}

func replayTasks(t *testing.T, dir string, pub string, limits Limits) State {
	t.Helper()
	st, err := Replay(testContext(t), dir, pub, limits, TaskReducer)
	if err != nil {
		t.Fatalf("Replay() = %v, want nil", err)
	}
	return st
}

func replayStateBytes(t *testing.T, dir string, pub string, limits Limits) []byte {
	t.Helper()
	out, err := replayTasks(t, dir, pub, limits).CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON() = %v, want nil", err)
	}
	return out
}

func testContext(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func raw(s string) []byte {
	return []byte(s)
}

func readText(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) // #nosec G304 -- callers pass test fixture files and temporary event log paths
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v, want nil", path, err)
	}
	return string(body)
}

func writeFile(t *testing.T, path string, body []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, body, mode); err != nil { // #nosec G703 -- callers write fixture files and temporary event log paths
		t.Fatalf("WriteFile(%s) = %v, want nil", path, err)
	}
}

func replaceInFile(t *testing.T, path string, old string, repl string) {
	t.Helper()
	data := strings.Replace(readText(t, path), old, repl, 1)
	writeFile(t, path, []byte(data), 0o600)
}

func assertErrContains(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want failure")
	}
	text := err.Error()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Fatalf("error %q does not contain %q", text, part)
		}
	}
}

func eventSeqs(t testing.TB, dir string) []uint64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s) = %v, want nil", dir, err)
	}
	seqs := make([]uint64, 0, len(entries))
	for _, entry := range entries {
		if isEventRecord(entry.Name()) {
			seq, err := seqFromName(entry.Name())
			if err != nil {
				t.Fatalf("seqFromName(%s) = %v, want nil", entry.Name(), err)
			}
			seqs = append(seqs, seq)
		}
	}
	sort.Slice(seqs, func(i int, j int) bool { return seqs[i] < seqs[j] })
	return seqs
}

func writeKey(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	body := []byte(hex.EncodeToString(testSeed()))
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatalf("WriteFile(key) = %v, want nil", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod(key) = %v, want nil", err)
	}
}

func jsonInt(v int) string {
	body, err := json.Marshal(v)
	if err != nil {
		return "0"
	}
	return string(body)
}

// twoLogs writes two three-record logs signed with the same key whose
// records differ only in their task ids. A record or HEAD copied from one
// into the other therefore carries a valid signature, and only the chain,
// sequence and HEAD checks can refuse it.
func twoLogs(t *testing.T) (string, string, *corereceipt.Signer, Limits) {
	t.Helper()
	a, signer, limits := testLog(t)
	b, _, _ := testLog(t)
	for i := 0; i < 3; i++ {
		appendEvent(t, a, signer, limits, Event{Type: "task.created", TaskID: fmt.Sprintf("a-%d", i), Payload: raw(`{"state":"open"}`)})
		appendEvent(t, b, signer, limits, Event{Type: "task.created", TaskID: fmt.Sprintf("b-%d", i), Payload: raw(`{"state":"open"}`)})
	}
	return a, b, signer, limits
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	body, err := os.ReadFile(from) // #nosec G304 -- callers copy numbered records between temporary event logs
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v", from, err)
	}
	writeFile(t, to, body, 0o600)
}

func TestSplicedSignedRecordBreaksChain(t *testing.T) {
	a, b, signer, limits := twoLogs(t)
	copyFile(t, eventPath(eventsDir(b), 2), eventPath(eventsDir(a), 2))
	_, err := Replay(testContext(t), a, signer.PublicKey(), limits, TaskReducer)
	assertErrContains(t, err, "seq 2", "prev mismatch")
}

func TestRecordSeqMustMatchItsFile(t *testing.T) {
	a, b, signer, limits := twoLogs(t)
	copyFile(t, eventPath(eventsDir(b), 3), eventPath(eventsDir(a), 2))
	_, err := Replay(testContext(t), a, signer.PublicKey(), limits, TaskReducer)
	assertErrContains(t, err, "seq 2", "record seq 3")
}

func TestHeadReceiptMustSignThisHead(t *testing.T) {
	a, b, signer, limits := twoLogs(t)
	var headA, headB map[string]any
	for _, h := range []struct {
		dir  string
		into *map[string]any
	}{{a, &headA}, {b, &headB}} {
		dec := json.NewDecoder(strings.NewReader(readText(t, filepath.Join(eventsDir(h.dir), "HEAD.json"))))
		dec.UseNumber()
		if err := dec.Decode(h.into); err != nil {
			t.Fatalf("decode HEAD of %s: %v", h.dir, err)
		}
	}
	headA["receipt"] = headB["receipt"]
	forged, err := canonicalJSON(headA)
	if err != nil {
		t.Fatalf("canonicalJSON(forged HEAD) = %v", err)
	}
	writeFile(t, filepath.Join(eventsDir(a), "HEAD.json"), forged, 0o600)
	_, err = Replay(testContext(t), a, signer.PublicKey(), limits, TaskReducer)
	assertErrContains(t, err, "HEAD.json", "verify")
}

func TestHeadHashMustMatchTail(t *testing.T) {
	a, b, signer, limits := twoLogs(t)
	copyFile(t, filepath.Join(eventsDir(b), "HEAD.json"), filepath.Join(eventsDir(a), "HEAD.json"))
	_, err := Replay(testContext(t), a, signer.PublicKey(), limits, TaskReducer)
	assertErrContains(t, err, "HEAD mismatch", "hash mismatch")
}

func TestKnownDomain(t *testing.T) {
	if !KnownDomain("task.created") || !KnownDomain("job.started") || !KnownDomain("unit.recorded") {
		t.Fatalf("expected task, job, and unit domains to be known")
	}
	if KnownDomain("foo.bar") || KnownDomain("unknown.event") || KnownDomain("") {
		t.Fatalf("expected unknown domains to be false")
	}
}

func TestTaskReducerSharedLogReplay(t *testing.T) {
	dir, signer, limits := testLog(t)
	appendEvent(t, dir, signer, limits, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	appendEvent(t, dir, signer, limits, Event{Type: "job.started", TaskID: "job-1", Payload: raw(`{"state":"running"}`)})
	appendEvent(t, dir, signer, limits, Event{Type: "unit.recorded", TaskID: "tekmora/epitrop#22", Payload: raw(`{"status":"implementing"}`)})
	appendEvent(t, dir, signer, limits, Event{Type: "task.state_changed", TaskID: "task-1", Payload: raw(`{"state":"done"}`)})

	st := replayTasks(t, dir, signer.PublicKey(), limits)
	if st.Tasks["task-1"].State != "done" {
		t.Fatalf("task-1 state = %s, want done", st.Tasks["task-1"].State)
	}

	appendEvent(t, dir, signer, limits, Event{Type: "foo.bar", TaskID: "foo-1", Payload: raw(`{}`)})
	_, err := Replay(testContext(t), dir, signer.PublicKey(), limits, TaskReducer)
	assertErrContains(t, err, "seq 5", "unknown event type \"foo.bar\"")
}

func TestTransientReplayErrorRecognizesHeadFailures(t *testing.T) {
	for _, text := range []string{
		"eventlog: missing HEAD with last seq 1",
		"eventlog: HEAD mismatch at seq 3",
		"eventlog: read /state/events/HEAD.json: no such file or directory",
	} {
		if !transientReplayError(errors.New(text)) {
			t.Fatalf("transientReplayError(%q) = false, want true", text)
		}
	}
	if transientReplayError(errors.New("eventlog: seq 2: signature does not verify")) {
		t.Fatal("transientReplayError(bad signature) = true, want false: only a HEAD race is worth a retry")
	}
	// Errno 32 is ERROR_SHARING_VIOLATION on Windows, where an append's rename causes it;
	// elsewhere it is another error and no reason to retry.
	busy := fmt.Errorf("eventlog: read HEAD.json: %w", &os.PathError{Op: "open", Path: "HEAD.json", Err: syscall.Errno(32)})
	if got, want := transientReplayError(busy), runtime.GOOS == "windows"; got != want {
		t.Fatalf("transientReplayError(errno 32 on %s) = %t, want %t", runtime.GOOS, got, want)
	}
}

var errStableTransient = errors.New("eventlog: missing HEAD with last seq 1")

// transientTwice fails transiently twice, then returns 7, and counts its calls.
func transientTwice(calls *int) func() (int, error) {
	return func() (int, error) {
		*calls++
		if *calls < 3 {
			return 0, errStableTransient
		}
		return 7, nil
	}
}

func TestRetryStableTriesUntilTheErrorIsNotTransient(t *testing.T) {
	calls := 0
	if got, err := retryStable(testContext(t), time.Second, transientTwice(&calls)); got != 7 || err != nil || calls != 3 {
		t.Fatalf("retryStable(transient twice) = %d, %v after %d calls, want 7, nil after 3", got, err, calls)
	}
	calls = 0
	if _, err := retryStable(testContext(t), 0, transientTwice(&calls)); !errors.Is(err, errStableTransient) || calls != 1 {
		t.Fatalf("retryStable(budget 0) = %v after %d calls, want the transient error after 1", err, calls)
	}
	ctx, cancel := context.WithCancel(testContext(t))
	cancel()
	calls = 0
	if _, err := retryStable(ctx, time.Second, transientTwice(&calls)); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("retryStable(ended context) = %v after %d calls, want context.Canceled after 0", err, calls)
	}
}

// TestRetryStableReturnsTheOperationsOwnError: callers see op's error as op returned it,
// never core/retry's wrapping of it.
func TestRetryStableReturnsTheOperationsOwnError(t *testing.T) {
	permanent := errors.New("eventlog: seq 2: signature does not verify")
	calls := 0
	_, err := retryStable(testContext(t), time.Second, func() (int, error) { calls++; return 0, permanent })
	if err == nil || err.Error() != permanent.Error() || calls != 1 {
		t.Fatalf("retryStable(permanent) = %v after %d calls, want %q after 1", err, calls, permanent)
	}
	_, err = retryStable(testContext(t), 30*time.Millisecond, func() (int, error) { return 0, errStableTransient })
	if err == nil || err.Error() != errStableTransient.Error() {
		t.Fatalf("retryStable(transient past the budget) = %v, want %q", err, errStableTransient)
	}
}

func TestStableAttemptsCoverTheBudget(t *testing.T) {
	tests := []struct {
		budget time.Duration
		want   int
	}{
		{-time.Second, 1},
		{0, 1},
		{lockRetryDelay - 1, 2},
		{lockRetryDelay, 3},
		{time.Second, 102},
	}
	for i := 0; i < len(tests); i++ {
		if got := stableAttempts(tests[i].budget); got != tests[i].want {
			t.Fatalf("stableAttempts(%v) = %d, want %d", tests[i].budget, got, tests[i].want)
		}
	}
}

func TestStableBudgetIsTheLockTimeout(t *testing.T) {
	if got := stableBudget(Limits{}); got != defaultLockTimeout {
		t.Fatalf("stableBudget(no lock timeout) = %v, want the default %v", got, defaultLockTimeout)
	}
	if got := stableBudget(Limits{LockTimeout: 2 * time.Second}); got != 2*time.Second {
		t.Fatalf("stableBudget(lock timeout 2s) = %v, want 2s", got)
	}
}

// TestReplayStableWaitsOutASlowAppend: an append under write load takes about 0.9 s
// (docs/architecture-v2.md, lock timeout), so a replay waits for HEAD as long as a writer
// waits for the lock, not a fixed second.
func TestReplayStableWaitsOutASlowAppend(t *testing.T) {
	st, err := replayStableWithHeadBackAfter(t, 1500*time.Millisecond, 3*time.Second)
	if err != nil || len(st.Tasks) != 1 {
		t.Fatalf("ReplayStable(HEAD restored after 1.5s, lock timeout 3s) = %d tasks, %v, want 1, nil", len(st.Tasks), err)
	}
}

// TestReplayStableWaitsOutAnAppendInFlight: a replay that lands between an append's record
// and its HEAD write succeeds once HEAD is there.
func TestReplayStableWaitsOutAnAppendInFlight(t *testing.T) {
	st, err := replayStableWithHeadBackAfter(t, 60*time.Millisecond, defaultLockTimeout)
	if err != nil || len(st.Tasks) != 1 {
		t.Fatalf("ReplayStable(HEAD restored after 60ms) = %d tasks, %v, want 1, nil", len(st.Tasks), err)
	}
}

// replayStableWithHeadBackAfter removes HEAD after one append, restores it after delay the
// way an append writes it, and returns what ReplayStable saw meanwhile with lockTimeout.
func replayStableWithHeadBackAfter(t *testing.T, delay, lockTimeout time.Duration) (State, error) {
	t.Helper()
	dir, signer, limits := testLog(t)
	limits.LockTimeout = lockTimeout
	appendEvent(t, dir, signer, limits, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	head := filepath.Join(dir, "events", "HEAD.json")
	body, err := os.ReadFile(head) // #nosec G304 -- head is the event log head created by testLog
	if err != nil {
		t.Fatalf("ReadFile(HEAD) = %v, want nil", err)
	}
	if err = os.Remove(head); err != nil {
		t.Fatalf("Remove(HEAD) = %v, want nil", err)
	}
	if _, err = Replay(testContext(t), dir, signer.PublicKey(), limits, TaskReducer); err == nil {
		t.Fatal("Replay(no HEAD) = nil, want the failure ReplayStable retries")
	}
	restored := make(chan error, 1)
	go func() {
		time.Sleep(delay)
		// An append replaces HEAD atomically; os.WriteFile would show the replay an empty file.
		restored <- atomicReplace(filepath.Dir(head), filepath.Base(head), body, 0o600)
	}()
	st, err := ReplayStable(testContext(t), dir, signer.PublicKey(), limits, TaskReducer)
	if writeErr := <-restored; writeErr != nil {
		t.Fatalf("restore HEAD = %v, want nil", writeErr)
	}
	return st, err
}

func TestReplayStableBoundsItsRetriesAndSkipsThemForOtherErrors(t *testing.T) {
	dir, signer, limits := testLog(t)
	appendEvent(t, dir, signer, limits, Event{Type: "foo.bar", TaskID: "x", Payload: raw(`{}`)})
	began := time.Now()
	if _, err := ReplayStable(testContext(t), dir, signer.PublicKey(), limits, TaskReducer); err == nil || !strings.Contains(err.Error(), "unknown event type") {
		t.Fatalf("ReplayStable(unknown type) = %v, want that error", err)
	}
	if took := time.Since(began); took > 500*time.Millisecond {
		t.Fatalf("ReplayStable took %v on an error no retry can fix, want it returned at once", took)
	}
	valid, validSigner, validLimits := testLog(t)
	appendEvent(t, valid, validSigner, validLimits, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	if err := os.Remove(filepath.Join(valid, "events", "HEAD.json")); err != nil {
		t.Fatalf("Remove(HEAD) = %v, want nil", err)
	}
	began = time.Now()
	_, err := ReplayStable(testContext(t), valid, validSigner.PublicKey(), validLimits, TaskReducer)
	if err == nil || !strings.Contains(err.Error(), "HEAD") {
		t.Fatalf("ReplayStable(HEAD gone for good) = %v, want the HEAD error", err)
	}
	if took := time.Since(began); took < 500*time.Millisecond || took > 5*time.Second {
		t.Fatalf("ReplayStable gave up after %v, want about a second of retries", took)
	}
}

func TestNormalizeLimitsDefaultsAndBoundaries(t *testing.T) {
	clk := clock.NewFake()
	checkNormalizeLimitsDefaults(t, clk)
	checkNormalizeLimitsCustom(t, clk)

	cases := []struct {
		name    string
		limits  Limits
		wantErr string
	}{
		{"nil clock", Limits{}, "limits clock is required"},
		{"negative lock timeout", Limits{Clock: clk, LockTimeout: -1}, "lock timeout must be non-negative"},
		{"negative replay cap", Limits{Clock: clk, MaxReplayRecords: -1}, "max replay records must be non-negative"},
		{"record bytes below 1", Limits{Clock: clk, MaxRecordBytes: -1}, "max record bytes must be 1..65536"},
		{"record bytes above cap", Limits{Clock: clk, MaxRecordBytes: MaxRecordBytes + 1}, "max record bytes must be 1..65536"},
	}
	for i := 0; i < len(cases); i++ {
		checkNormalizeLimitsError(t, cases[i].name, cases[i].limits, cases[i].wantErr)
	}
}

func checkNormalizeLimitsDefaults(t *testing.T, clk clock.Clock) {
	t.Helper()
	got, err := normalizeLimits(Limits{Clock: clk})
	if err != nil {
		t.Fatalf("normalizeLimits(zero fields) = %v, want nil", err)
	}
	if got.LockTimeout != defaultLockTimeout || got.MaxReplayRecords != defaultReplayCap || got.MaxRecordBytes != MaxRecordBytes {
		t.Fatalf("got defaults %+v, want lockTimeout=%v replayCap=%d recordBytes=%d", got, defaultLockTimeout, defaultReplayCap, MaxRecordBytes)
	}
}

func checkNormalizeLimitsCustom(t *testing.T, clk clock.Clock) {
	t.Helper()
	custom := Limits{
		Clock:            clk,
		LockTimeout:      1 * time.Nanosecond,
		MaxReplayRecords: 1,
		MaxRecordBytes:   1,
	}
	got, err := normalizeLimits(custom)
	if err != nil {
		t.Fatalf("normalizeLimits(boundary=1) = %v, want nil", err)
	}
	if got.LockTimeout != 1*time.Nanosecond || got.MaxReplayRecords != 1 || got.MaxRecordBytes != 1 {
		t.Fatalf("got pass-through %+v, want boundary values 1", got)
	}
}

func checkNormalizeLimitsError(t *testing.T, name string, limits Limits, wantErr string) {
	t.Helper()
	_, err := normalizeLimits(limits)
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Errorf("%s: err = %v, want error containing %q", name, err, wantErr)
	}
}

func TestAtomicReplaceCleanupOnFailure(t *testing.T) {
	dir := t.TempDir()
	causeErr := errors.New("simulated-fsync-failure")
	realSync := syncFile
	syncFile = func(f *os.File) error {
		return causeErr
	}
	t.Cleanup(func() { syncFile = realSync })

	err := atomicReplace(dir, "record.json", []byte("sample-data"), 0o600)
	if err == nil {
		t.Fatal("atomicReplace() = nil, want error")
	}
	if !errors.Is(err, causeErr) {
		t.Fatalf("atomicReplace() = %v, want cause %v", err, causeErr)
	}

	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("ReadDir() = %v", readErr)
	}
	for i := 0; i < len(entries); i++ {
		if strings.HasPrefix(entries[i].Name(), ".") {
			t.Fatalf("found leftover temp file: %s", entries[i].Name())
		}
	}
	if len(entries) != 0 {
		t.Fatalf("found %d entries in dir, want 0", len(entries))
	}
}

func TestAppendCheckedPositive(t *testing.T) {
	dir, signer, limits := testLog(t)
	writer := openWriter(t, dir, signer, limits)
	ctx := testContext(t)
	rec, err := writer.AppendChecked(ctx, func(context.Context) (Event, error) {
		return Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)}, nil
	})
	if err != nil {
		t.Fatalf("AppendChecked() = %v, want nil", err)
	}
	if rec.Seq != 1 {
		t.Fatalf("rec.Seq = %d, want 1", rec.Seq)
	}
	st := replayTasks(t, dir, signer.PublicKey(), limits)
	if st.Tasks["task-1"].State != "open" {
		t.Fatalf("replayed task state = %q, want open", st.Tasks["task-1"].State)
	}
}

func TestAppendCheckedNegativeCheckErrorLeavesLogUnchanged(t *testing.T) {
	dir, signer, limits := testLog(t)
	writer := openWriter(t, dir, signer, limits)
	ctx := testContext(t)
	sentinel := errors.New("check refused")
	_, err := writer.AppendChecked(ctx, func(context.Context) (Event, error) {
		return Event{}, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("AppendChecked() error = %v, want sentinel %v", err, sentinel)
	}
	seqs := eventSeqs(t, eventsDir(dir))
	if len(seqs) != 0 {
		t.Fatalf("records in events dir = %v, want empty", seqs)
	}
	headPath := filepath.Join(eventsDir(dir), "HEAD.json")
	if _, err := os.Stat(headPath); !os.IsNotExist(err) {
		t.Fatalf("HEAD.json exists after refused check: %v", err)
	}
}

func TestAppendCheckedNilCheckRefused(t *testing.T) {
	dir, signer, limits := testLog(t)
	writer := openWriter(t, dir, signer, limits)
	ctx := testContext(t)
	_, err := writer.AppendChecked(ctx, nil)
	assertErrContains(t, err, "check function is required")
}

// TestAppendCheckedRaceSerialization verifies that check closures are serialized by the lock.
// Writer A enters its check, signals aEntered, and starts writer B. Writer A waits up to 300ms.
// Because the lock is held by writer A, writer B's check cannot run until A finishes and commits.
// With a check outside the lock, B enters at once while A waits, sees an empty log (seq 0), and the test fails.
func TestAppendCheckedRaceSerialization(t *testing.T) {
	dir, signer, limits := testLog(t)
	limits.LockTimeout = 5 * time.Second
	writerA := openWriter(t, dir, signer, limits)
	writerB := openWriter(t, dir, signer, limits)
	ctx := testContext(t)
	if err := os.MkdirAll(eventsDir(dir), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	aEntered := make(chan struct{})
	bEntered := make(chan struct{})
	var bSawSeqs int
	var bErr error
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-aEntered
		_, bErr = writerB.AppendChecked(ctx, func(cctx context.Context) (Event, error) {
			close(bEntered)
			st, replayErr := Replay(cctx, dir, signer.PublicKey(), limits, TaskReducer)
			if replayErr != nil {
				return Event{}, replayErr
			}
			bSawSeqs = len(st.Tasks)
			if bSawSeqs != 1 {
				return Event{}, fmt.Errorf("check B observed %d records, want 1", bSawSeqs)
			}
			return Event{Type: "task.created", TaskID: "task-b", Payload: raw(`{"state":"open"}`)}, nil
		})
	}()

	recA, errA := writerA.AppendChecked(ctx, func(context.Context) (Event, error) {
		close(aEntered)
		select {
		case <-bEntered:
		case <-time.After(300 * time.Millisecond):
		}
		return Event{Type: "task.created", TaskID: "task-a", Payload: raw(`{"state":"open"}`)}, nil
	})
	if errA != nil {
		t.Fatalf("writer A AppendChecked() = %v, want nil", errA)
	}
	if recA.Seq != 1 {
		t.Fatalf("recA.Seq = %d, want 1", recA.Seq)
	}

	wg.Wait()
	if bErr != nil {
		t.Fatalf("writer B AppendChecked() = %v, want nil", bErr)
	}
	if bSawSeqs != 1 {
		t.Fatalf("B observed %d records, want 1 (must run after A commits under lock)", bSawSeqs)
	}
}

func TestReplayEmptyEventsDir(t *testing.T) {
	dir, signer, limits := testLog(t)
	if err := os.MkdirAll(eventsDir(dir), 0o750); err != nil {
		t.Fatalf("MkdirAll(eventsDir) = %v, want nil", err)
	}
	st, err := Replay(testContext(t), dir, signer.PublicKey(), limits, TaskReducer)
	if err != nil {
		t.Fatalf("Replay(empty events dir) = %v, want nil", err)
	}
	if len(st.Tasks) != 0 {
		t.Fatalf("len(st.Tasks) = %d, want 0", len(st.Tasks))
	}

	// Boundary twin: events dir holding a record file but no HEAD.json still fails with "missing HEAD".
	dir2, signer2, limits2 := testLog(t)
	appendEvent(t, dir2, signer2, limits2, Event{Type: "task.created", TaskID: "task-1", Payload: raw(`{"state":"open"}`)})
	headPath := filepath.Join(eventsDir(dir2), "HEAD.json")
	if err := os.Remove(headPath); err != nil {
		t.Fatalf("Remove(HEAD.json) = %v, want nil", err)
	}
	_, err = Replay(testContext(t), dir2, signer2.PublicKey(), limits2, TaskReducer)
	assertErrContains(t, err, "missing HEAD")
}
