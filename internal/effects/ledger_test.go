package effects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golusoris/golusoris/core/clock"
	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/eventlog/eventlogtest"
	"github.com/tekmora/epitrop/internal/graphlog"
	"github.com/tekmora/epitrop/internal/supervisor"
	"github.com/tekmora/epitrop/internal/units"
)

func itoa(n int) string { return strconv.Itoa(n) }

func quote(s string) string { return strconv.Quote(s) }

// testLedger returns a ledger on a fresh log and the log's directory.
func testLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	dir := t.TempDir()
	return ledgerAt(t, dir), dir
}

// ledgerAt opens the log in dir. Two ledgers on one directory are two writers of one log.
func ledgerAt(t *testing.T, dir string) *Ledger {
	t.Helper()
	clk := clock.NewFake()
	signer, err := corereceipt.NewSignerFromSeed(bytes.Repeat([]byte{5}, 32), clk)
	if err != nil {
		t.Fatalf("NewSignerFromSeed() = %v, want nil", err)
	}
	ledger, err := NewLedger(dir, signer, clk, eventlog.Limits{LockTimeout: time.Minute, MaxReplayRecords: 10000})
	if err != nil {
		t.Fatalf("NewLedger() = %v, want nil", err)
	}
	return ledger
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// recordTypes returns the types of the records in the log, in order.
func recordTypes(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "events"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("ReadDir(events) = %v, want nil", err)
	}
	names := make([]string, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		if name := entries[i].Name(); strings.HasSuffix(name, ".json") && name != "HEAD.json" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	types := make([]string, 0, len(names))
	for i := 0; i < len(names); i++ {
		body, err := os.ReadFile(filepath.Join(dir, "events", names[i])) // #nosec G304 -- names are directory entries from the test event log
		if err != nil {
			t.Fatalf("ReadFile(%s) = %v, want nil", names[i], err)
		}
		var rec eventlog.Record
		if err = json.Unmarshal(body, &rec); err != nil {
			t.Fatalf("%s: %v", names[i], err)
		}
		types = append(types, rec.Type)
	}
	return types
}

func effectOf(t *testing.T, ledger *Ledger, key Key) eventlog.Effect {
	t.Helper()
	st, err := ledger.Replay(testCtx(t))
	if err != nil {
		t.Fatalf("Replay() = %v, want nil", err)
	}
	return st.Effects[key.String()]
}

var key = Key{TaskID: "task-7", ActionType: "file-issue"}

// A second delivery of a key runs nothing and is recorded; the effect ran once.
func TestDoubleDeliveryExecutesOnce(t *testing.T) {
	ledger, dir := testLedger(t)
	ran := 0
	effect := func(context.Context) error { ran++; return nil }
	first, err := ledger.Do(testCtx(t), key, effect)
	if err != nil || !first.Executed || first.Skipped {
		t.Fatalf("first Do() = %+v, %v; want it executed", first, err)
	}
	second, err := ledger.Do(testCtx(t), key, effect)
	if err != nil || second.Executed || !second.Skipped {
		t.Fatalf("second Do() = %+v, %v; want it skipped", second, err)
	}
	assertDoubleDeliveryState(t, ledger, dir, ran)
	other, err := ledger.Do(testCtx(t), Key{TaskID: "task-7", ActionType: "close-issue"}, effect)
	if err != nil || !other.Executed || ran != 2 {
		t.Fatalf("Do(same task, other action) = %+v, %v, ran %d; want a key of its own to run", other, err, ran)
	}
}

func assertDoubleDeliveryState(t *testing.T, ledger *Ledger, dir string, ran int) {
	t.Helper()
	if ran != 1 {
		t.Fatalf("the effect ran %d times, want 1", ran)
	}
	if got := strings.Join(recordTypes(t, dir), " "); got != "effect.claimed effect.done effect.skipped" {
		t.Fatalf("records = %q, want claimed, done, skipped", got)
	}
	if got := effectOf(t, ledger, key); got.State != StateDone || got.Skipped != 1 || got.Attempt != 1 {
		t.Fatalf("effect = %+v, want done, skipped once, one attempt", got)
	}
}

// Eight writers deliver the same key at once. Exactly one runs the effect; the others are
// told that they did not.
func TestConcurrentDeliveriesExecuteOnce(t *testing.T) {
	dir := t.TempDir()
	const writers = 8
	var ran atomic.Int32
	var executed atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		ledger := ledgerAt(t, dir)
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := ledger.Do(testCtx(t), key, func(context.Context) error { ran.Add(1); return nil })
			if res.Executed {
				executed.Add(1)
			}
			if err != nil && !errors.Is(err, ErrClaimedElsewhere) && !errors.Is(err, ErrUnresolved) {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	assertConcurrentDeliveriesResult(t, dir, errs, ran.Load(), executed.Load())
}

func assertConcurrentDeliveriesResult(t *testing.T, dir string, errs <-chan error, ran, executed int32) {
	t.Helper()
	for err := range errs {
		t.Fatalf("Do() = %v, want nil, a claim held elsewhere or an open claim", err)
	}
	if ran != 1 || executed != 1 {
		t.Fatalf("the effect ran %d times and %d writers report having run it, want 1 and 1", ran, executed)
	}
	if got := effectOf(t, ledgerAt(t, dir), key); got.State != StateDone || got.Attempt != 1 {
		t.Fatalf("effect = %+v, want done after one attempt", got)
	}
}

// A claim without an end, as a crash leaves it, and a failed attempt are open: nothing runs
// the effect again, the claim is listed, and it stays so however often it is delivered.
func TestOpenClaimBlocksUntilItIsResolved(t *testing.T) {
	ledger, dir := testLedger(t)
	ran := 0
	effect := func(context.Context) error { ran++; return nil }
	if _, err := ledger.append(testCtx(t), Claimed, key, payload{}); err != nil {
		t.Fatalf("append(claim) = %v, want nil", err)
	}
	failing := Key{TaskID: "task-8", ActionType: "file-issue"}
	failure := errors.New("the forge answered 502")
	if _, err := ledger.Do(testCtx(t), failing, func(context.Context) error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("Do(failing effect) = %v, want the effect's error", err)
	}
	records := len(recordTypes(t, dir))
	assertOpenClaimsRemain(t, ledger, failing, effect)
	assertOpenClaimsFinalState(t, ledger, dir, records, ran)
}

func assertOpenClaimsRemain(t *testing.T, ledger *Ledger, failing Key, effect func(context.Context) error) {
	t.Helper()
	for i := 0; i < 3; i++ {
		_, err := ledger.Do(testCtx(t), key, effect)
		if !errors.Is(err, ErrUnresolved) || !strings.Contains(err.Error(), "never ended") {
			t.Fatalf("Do(claimed key) = %v, want an open claim that never ended", err)
		}
		_, err = ledger.Do(testCtx(t), failing, effect)
		if !errors.Is(err, ErrUnresolved) || !strings.Contains(err.Error(), "the forge answered 502") {
			t.Fatalf("Do(failed key) = %v, want an open claim with the failure's reason", err)
		}
	}
}

func assertOpenClaimsFinalState(t *testing.T, ledger *Ledger, dir string, records, ran int) {
	t.Helper()
	if ran != 0 || len(recordTypes(t, dir)) != records {
		t.Fatalf("ran %d, records %d -> %d; want nothing run and nothing appended", ran, records, len(recordTypes(t, dir)))
	}
	open, err := ledger.Open(testCtx(t))
	if err != nil || len(open) != 2 || open[0].TaskID != "task-7" || open[0].State != StateClaimed || open[1].TaskID != "task-8" || open[1].State != StateFailed {
		t.Fatalf("Open() = %+v, %v; want the claimed and the failed effect, in key order", open, err)
	}
}

// A resolution is a recorded act: it names who decided, and says whether the effect did
// happen or may be attempted once more.
func TestResolveRecordsWhatBecameOfAnOpenClaim(t *testing.T) {
	ledger, _ := testLedger(t)
	ran := 0
	effect := func(context.Context) error { ran++; return nil }
	assertResolvedRetryClaim(t, ledger, effect, &ran)
	assertResolvedDoneClaim(t, ledger, effect, &ran)
	if open, err := ledger.Open(testCtx(t)); err != nil || len(open) != 0 {
		t.Fatalf("Open() = %+v, %v; want none left", open, err)
	}
}

func assertResolvedRetryClaim(t *testing.T, ledger *Ledger, effect func(context.Context) error, ran *int) {
	t.Helper()
	retried := Key{TaskID: "task-1", ActionType: "file-issue"}
	if _, err := ledger.append(testCtx(t), Claimed, retried, payload{}); err != nil {
		t.Fatalf("append(claim) = %v, want nil", err)
	}
	if err := ledger.Resolve(testCtx(t), retried, OutcomeRetry, "operator", "no issue on the forge"); err != nil {
		t.Fatalf("Resolve(retry) = %v, want nil", err)
	}
	res, err := ledger.Do(testCtx(t), retried, effect)
	if err != nil || !res.Executed || *ran != 1 {
		t.Fatalf("Do(after retry) = %+v, %v, ran %d; want it to run once", res, err, *ran)
	}
	if got := effectOf(t, ledger, retried); got.State != StateDone || got.Attempt != 2 || got.ResolvedBy != "" {
		t.Fatalf("effect after the retry = %+v, want done in a second attempt", got)
	}
}

func assertResolvedDoneClaim(t *testing.T, ledger *Ledger, effect func(context.Context) error, ran *int) {
	t.Helper()
	happened := Key{TaskID: "task-2", ActionType: "file-issue"}
	if _, err := ledger.append(testCtx(t), Claimed, happened, payload{}); err != nil {
		t.Fatalf("append(claim) = %v, want nil", err)
	}
	if err := ledger.Resolve(testCtx(t), happened, OutcomeDone, "operator", "issue 12 is there"); err != nil {
		t.Fatalf("Resolve(done) = %v, want nil", err)
	}
	res, err := ledger.Do(testCtx(t), happened, effect)
	if err != nil || !res.Skipped || *ran != 1 {
		t.Fatalf("Do(after done) = %+v, %v, ran %d; want it skipped", res, err, *ran)
	}
	if got := effectOf(t, ledger, happened); got.State != StateDone || got.ResolvedBy != "operator" || got.ResolvedNote != "issue 12 is there" {
		t.Fatalf("effect resolved as done = %+v, want who and why recorded", got)
	}
}

func TestResolveRefusesWhatIsNoResolution(t *testing.T) {
	ledger, dir := testLedger(t)
	if _, err := ledger.append(testCtx(t), Claimed, key, payload{}); err != nil {
		t.Fatalf("append(claim) = %v, want nil", err)
	}
	done := Key{TaskID: "task-9", ActionType: "file-issue"}
	if _, err := ledger.Do(testCtx(t), done, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("Do() = %v, want nil", err)
	}
	records := len(recordTypes(t, dir))
	cases := []struct {
		name    string
		key     Key
		outcome string
		by      string
		note    string
		want    string
	}{
		{"an unknown outcome", key, "abandon", "operator", "", `unknown outcome "abandon"`},
		{"nobody named", key, OutcomeDone, "", "", "names who made it"},
		{"a note longer than the bound", key, OutcomeDone, "operator", strings.Repeat("n", MaxReasonBytes+1), "note is longer than 2048 bytes"},
		{"a key that is not valid", Key{TaskID: "task 7", ActionType: "file-issue"}, OutcomeDone, "operator", "", "invalid key"},
		{"an effect that is done", done, OutcomeRetry, "operator", "", "has no open claim to resolve"},
		{"a key nobody claimed", Key{TaskID: "task-0", ActionType: "file-issue"}, OutcomeDone, "operator", "", "has no open claim to resolve"},
	}
	for i := 0; i < len(cases); i++ {
		err := ledger.Resolve(testCtx(t), cases[i].key, cases[i].outcome, cases[i].by, cases[i].note)
		if err == nil || !strings.Contains(err.Error(), cases[i].want) {
			t.Errorf("%s: Resolve() = %v, want an error holding %q", cases[i].name, err, cases[i].want)
		}
	}
	if len(recordTypes(t, dir)) != records {
		t.Fatalf("records %d -> %d, want a refused resolution to append nothing", records, len(recordTypes(t, dir)))
	}
	if err := ledger.Resolve(testCtx(t), key, OutcomeDone, "operator", strings.Repeat("n", MaxReasonBytes)); err != nil {
		t.Fatalf("Resolve(note at the bound) = %v, want nil", err)
	}
}

func TestDoRefusesWhatItCannotRun(t *testing.T) {
	ledger, dir := testLedger(t)
	effect := func(context.Context) error { return nil }
	bad := []Key{{}, {TaskID: "task-1"}, {ActionType: "file-issue"}, {TaskID: "task 1", ActionType: "file-issue"},
		{TaskID: "task-1", ActionType: "file/issue"}, {TaskID: "-task", ActionType: "file-issue"},
		{TaskID: strings.Repeat("t", 129), ActionType: "file-issue"}}
	for i := 0; i < len(bad); i++ {
		if _, err := ledger.Do(testCtx(t), bad[i], effect); err == nil || !strings.Contains(err.Error(), "invalid key") {
			t.Errorf("Do(%q) = %v, want the key refused", bad[i].String(), err)
		}
	}
	longest := Key{TaskID: strings.Repeat("t", 128), ActionType: "a.b_c-1"}
	if _, err := ledger.Do(testCtx(t), longest, effect); err != nil {
		t.Errorf("Do(key at the length bound) = %v, want nil", err)
	}
	assertDoContextAndEffectRefusals(t, ledger, effect)
	if got := strings.Join(recordTypes(t, dir), " "); got != "effect.claimed effect.done" {
		t.Fatalf("records = %q, want only the one delivery that ran", got)
	}
	assertNewLedgerRefusals(t)
}

func assertDoContextAndEffectRefusals(t *testing.T, ledger *Ledger, effect func(context.Context) error) {
	t.Helper()
	if _, err := ledger.Do(testCtx(t), key, nil); err == nil || !strings.Contains(err.Error(), "nil effect") {
		t.Errorf("Do(nil effect) = %v, want it refused", err)
	}
	if _, err := ledger.Do(context.Background(), key, effect); err == nil || !strings.Contains(err.Error(), "effects: do requires a context deadline") {
		t.Errorf("Do(no deadline) = %v, want the ledger to refuse it", err)
	}
	if _, err := ledgerAt(t, t.TempDir()).Open(context.Background()); err == nil || !strings.Contains(err.Error(), "effects: replay requires a context deadline") {
		t.Errorf("Open(no deadline, empty log) = %v, want the ledger to refuse it", err)
	}
}

func assertNewLedgerRefusals(t *testing.T) {
	t.Helper()
	signer, err := corereceipt.NewSignerFromSeed(bytes.Repeat([]byte{5}, 32), clock.NewFake())
	if err != nil {
		t.Fatalf("NewSignerFromSeed() = %v, want nil", err)
	}
	if _, err = NewLedger("", signer, nil, eventlog.Limits{}); err == nil || !strings.Contains(err.Error(), "dir is required") {
		t.Errorf("NewLedger(no dir) = %v, want it refused", err)
	}
	if _, err = NewLedger(t.TempDir(), nil, nil, eventlog.Limits{}); err == nil || !strings.Contains(err.Error(), "signer is required") {
		t.Errorf("NewLedger(no signer) = %v, want it refused", err)
	}
}

// The reason of a failed attempt is kept, cut at the bound.
func TestFailedAttemptKeepsABoundedReason(t *testing.T) {
	ledger, _ := testLedger(t)
	long := errors.New(strings.Repeat("e", MaxReasonBytes+100))
	if _, err := ledger.Do(testCtx(t), key, func(context.Context) error { return long }); !errors.Is(err, long) {
		t.Fatalf("Do() = %v, want the effect's error", err)
	}
	if got := effectOf(t, ledger, key); got.State != StateFailed || len(got.Reason) != MaxReasonBytes {
		t.Fatalf("effect = state %q, reason of %d bytes; want failed with %d bytes", got.State, len(got.Reason), MaxReasonBytes)
	}
}

func rec(seq uint64, typ string, task string, body string) eventlog.Record {
	return eventlog.Record{Seq: seq, Type: typ, TaskID: task, Payload: json.RawMessage(body)}
}

// reduce applies records in order and fails the test on the first error.
func reduce(t *testing.T, recs ...eventlog.Record) eventlog.State {
	t.Helper()
	return eventlogtest.Reduce(t, Reducer, recs...)
}

// What two racing writers can append is not applied and is counted; the first claim in log
// order is the one that holds.
func TestReducerIgnoresAndCountsWhatARaceProduces(t *testing.T) {
	claim := `{"action_type":"file-issue"}`
	st := reduce(t,
		rec(1, Claimed, "task-1", claim),
		rec(2, Claimed, "task-1", claim),
		rec(3, Done, "task-1", `{"action_type":"file-issue","claim_seq":2}`),
		rec(4, Skipped, "task-1", claim),
		rec(5, Resolved, "task-2", `{"action_type":"file-issue","outcome":"done","by":"operator"}`),
	)
	got := st.Effects["task-1 file-issue"]
	if got.State != StateClaimed || got.ClaimSeq != 1 || got.Attempt != 1 || st.EffectRecordsIgnored != 4 {
		t.Fatalf("effect = %+v, ignored %d; want the first claim to hold and four records ignored", got, st.EffectRecordsIgnored)
	}
	st = reduce(t,
		rec(1, Claimed, "task-1", claim),
		rec(2, Done, "task-1", `{"action_type":"file-issue","claim_seq":1}`),
		rec(3, Done, "task-1", `{"action_type":"file-issue","claim_seq":1}`),
		rec(4, Failed, "task-1", `{"action_type":"file-issue","claim_seq":1,"reason":"late"}`),
		rec(5, Claimed, "task-1", claim),
		rec(6, Resolved, "task-1", `{"action_type":"file-issue","outcome":"retry","by":"operator"}`),
		rec(7, Skipped, "task-1", claim),
	)
	got = st.Effects["task-1 file-issue"]
	if got.State != StateDone || got.Skipped != 1 || st.EffectRecordsIgnored != 4 {
		t.Fatalf("effect = %+v, ignored %d; want done, skipped once and four records ignored", got, st.EffectRecordsIgnored)
	}
	assertReducerReplayCanonicalJSON(t, claim)
}

func assertReducerReplayCanonicalJSON(t *testing.T, claim string) {
	t.Helper()
	first, err := reduce(t, rec(1, Claimed, "task-1", claim)).CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON() = %v, want nil", err)
	}
	second, err := reduce(t, rec(1, Claimed, "task-1", claim)).CanonicalJSON()
	if err != nil || !bytes.Equal(first, second) || !bytes.Contains(first, []byte(`"effects":{"task-1 file-issue"`)) {
		t.Fatalf("two replays = %s / %s, %v; want the same bytes with the effect in them", first, second, err)
	}
}

// The ledger holds a bounded number of keys. A claim of one key more is not applied and is
// counted; a claim of a key the ledger already holds is not touched by the bound.
func TestReducerBoundsTheNumberOfKeys(t *testing.T) {
	full := eventlog.State{Effects: make(map[string]eventlog.Effect, MaxEffects)}
	for i := 0; i < MaxEffects; i++ {
		k := Key{TaskID: "task-" + itoa(i), ActionType: "file-issue"}
		full.Effects[k.String()] = eventlog.Effect{TaskID: k.TaskID, ActionType: k.ActionType, State: StateReleased, Attempt: 1}
	}
	st, err := Reducer(full, rec(1, Claimed, "one-more", `{"action_type":"file-issue"}`))
	if err != nil || len(st.Effects) != MaxEffects || st.EffectRecordsIgnored != 1 {
		t.Fatalf("claim beyond the bound = %v, %d keys, ignored %d; want it ignored and counted", err, len(st.Effects), st.EffectRecordsIgnored)
	}
	st, err = Reducer(st, rec(2, Claimed, "task-0", `{"action_type":"file-issue"}`))
	if err != nil || st.Effects["task-0 file-issue"].State != StateClaimed || st.EffectRecordsIgnored != 1 {
		t.Fatalf("claim of a held key at the bound = %v, %+v; want it applied", err, st.Effects["task-0 file-issue"])
	}
	delete(st.Effects, "task-1 file-issue")
	st, err = Reducer(st, rec(3, Claimed, "one-more", `{"action_type":"file-issue"}`))
	if err != nil || st.Effects["one-more file-issue"].State != StateClaimed {
		t.Fatalf("claim one below the bound = %v, want it applied", err)
	}
}

// A record no writer of this version produces stops the replay and names its position.
func TestReducerFailsOnARecordNoWriterProduces(t *testing.T) {
	cases := []struct {
		name string
		rec  eventlog.Record
		want string
	}{
		{"an unknown effect record", rec(4, "effect.retried", "task-1", `{"action_type":"file-issue"}`), `seq 4: unknown event type "effect.retried"`},
		{"an unknown domain", rec(4, "plan.created", "task-1", `{}`), `seq 4: unknown event type "plan.created"`},
		{"a payload that is no object", rec(4, Claimed, "task-1", `[]`), "seq 4: payload"},
		{"a field no writer sets", rec(4, Claimed, "task-1", `{"action_type":"file-issue","when":"now"}`), "seq 4: payload"},
		{"no action type", rec(4, Claimed, "task-1", `{}`), "seq 4: invalid effect key"},
		{"a task id with a space", rec(4, Claimed, "task 1", `{"action_type":"file-issue"}`), "seq 4: invalid effect key"},
		{"a resolution with an unknown outcome", rec(4, Resolved, "task-1", `{"action_type":"file-issue","outcome":"abandon","by":"operator"}`), `seq 4: unknown outcome "abandon"`},
		{"a resolution by nobody", rec(4, Resolved, "task-1", `{"action_type":"file-issue","outcome":"done"}`), "seq 4: a resolution names who made it"},
	}
	for i := 0; i < len(cases); i++ {
		t.Run(cases[i].name, func(t *testing.T) {
			eventlogtest.AssertRejects(t, Reducer, cases[i].rec, cases[i].want)
		})
	}
}

// One log serves every domain: the other reducers pass over an effect record, and this one
// passes over theirs.
func TestOtherReducersSkipEffectRecordsAndThisOneSkipsTheirs(t *testing.T) {
	claim := rec(1, Claimed, "task-1", `{"action_type":"file-issue"}`)
	others := []struct {
		name   string
		reduce eventlog.Reducer
	}{
		{"tasks", eventlog.TaskReducer},
		{"units", units.UnitReducer},
		{"jobs", supervisor.JobReducer},
		{"graph", graphlog.Reducer},
	}
	for i := 0; i < len(others); i++ {
		st, err := others[i].reduce(eventlog.State{}, claim)
		if err != nil || len(st.Effects) != 0 {
			t.Errorf("%s reducer on an effect record = %v, effects %v; want it skipped", others[i].name, err, st.Effects)
		}
	}
	theirs := []string{"task.created", "job.started", "unit.recorded", "graph.stage_committed"}
	for i := 0; i < len(theirs); i++ {
		st, err := Reducer(eventlog.State{}, rec(1, theirs[i], "x", `{}`))
		if err != nil || len(st.Effects) != 0 || st.EffectRecordsIgnored != 0 {
			t.Errorf("Reducer(%s) = %v, state %+v; want the record skipped", theirs[i], err, st)
		}
	}
}

// TestConcurrentResolveOnOpenClaim verifies that when multiple goroutines race to resolve
// an open claim, exactly one succeeds and the rest receive "has no open claim", with
// zero effect records ignored.
func TestConcurrentResolveOnOpenClaim(t *testing.T) {
	ledger, _ := testLedger(t)
	ctx := testCtx(t)
	k := Key{TaskID: "task-open", ActionType: "file-issue"}
	if _, err := ledger.append(ctx, Claimed, k, payload{}); err != nil {
		t.Fatalf("append(claim) = %v, want nil", err)
	}

	const total = 8
	var wg sync.WaitGroup
	errs := make(chan error, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs <- ledger.Resolve(ctx, k, OutcomeDone, "operator", "resolution "+itoa(idx))
		}(i)
	}
	wg.Wait()
	close(errs)

	successes := 0
	noOpenClaim := 0
	for err := range errs {
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "has no open claim") {
			noOpenClaim++
		} else {
			t.Fatalf("unexpected Resolve error: %v", err)
		}
	}
	if successes != 1 || noOpenClaim != 7 {
		t.Fatalf("successes = %d, noOpenClaim = %d; want 1 and 7", successes, noOpenClaim)
	}
	st, err := ledger.Replay(ctx)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if st.EffectRecordsIgnored != 0 {
		t.Fatalf("EffectRecordsIgnored = %d, want 0", st.EffectRecordsIgnored)
	}
}
