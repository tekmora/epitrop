package jobfailure

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/broker"
	"github.com/tekmora/epitrop/internal/eventlog"
)

type fakeLog struct {
	mu      sync.Mutex
	records []eventlog.Record
	outputs map[string]string
}

func (f *fakeLog) appendRecord(t *testing.T, typ, taskID string, payload any) eventlog.Record {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	seq := uint64(1)
	if len(f.records) > 0 {
		seq = f.records[len(f.records)-1].Seq + 1
	}
	rec := eventlog.Record{
		Seq:     seq,
		Type:    typ,
		TaskID:  taskID,
		Payload: b,
	}
	f.records = append(f.records, rec)
	return rec
}

func (f *fakeLog) replay(_ context.Context, after uint64) ([]eventlog.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []eventlog.Record
	for _, r := range f.records {
		if r.Seq > after {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeLog) head(_ context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.records) == 0 {
		return 0, nil
	}
	return f.records[len(f.records)-1].Seq, nil
}

func (f *fakeLog) appendFn(ctx context.Context, check func(context.Context) (eventlog.Event, error)) (eventlog.Record, error) {
	ev, err := check(ctx)
	if err != nil {
		return eventlog.Record{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := uint64(1)
	if len(f.records) > 0 {
		seq = f.records[len(f.records)-1].Seq + 1
	}
	rec := eventlog.Record{
		Seq:     seq,
		Type:    ev.Type,
		TaskID:  ev.TaskID,
		Payload: ev.Payload,
	}
	f.records = append(f.records, rec)
	return rec, nil
}

func (f *fakeLog) output(_ context.Context, job string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.outputs[job], nil
}

func testDeadLetters(t *testing.T, specs []broker.ClassSpec, fallback string, cap int) *broker.DeadLetters {
	t.Helper()
	dl, err := broker.NewDeadLetters(specs, fallback, cap)
	if err != nil {
		t.Fatalf("NewDeadLetters: %v", err)
	}
	return dl
}

func defaultDeadLetters(t *testing.T) *broker.DeadLetters {
	t.Helper()
	specs := []broker.ClassSpec{
		{Name: "known-flake", Signatures: []string{`flake-sig`}, MaxRetries: 1},
		{Name: "real-failure"},
	}
	return testDeadLetters(t, specs, "real-failure", 10)
}

func assertFailedExitPayload(t *testing.T, rec eventlog.Record, exitedSeq uint64, class string, code int) {
	t.Helper()
	var p deadLetteredPayload
	if err := json.Unmarshal(rec.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.ExitedSeq != exitedSeq {
		t.Fatalf("ExitedSeq = %d, want %d", p.ExitedSeq, exitedSeq)
	}
	if p.Class != class {
		t.Fatalf("Class = %q, want %q", p.Class, class)
	}
	if p.Retry || !p.Parked {
		t.Fatalf("retry=%v parked=%v, want retry=false parked=true", p.Retry, p.Parked)
	}
	if p.Attempt != 1 {
		t.Fatalf("Attempt = %d, want 1", p.Attempt)
	}
	if p.Code == nil || *p.Code != code {
		t.Fatalf("Code = %v, want %d", p.Code, code)
	}
}

func assertCodeAndReason(t *testing.T, rec eventlog.Record, code int, reason string) {
	t.Helper()
	var p deadLetteredPayload
	if err := json.Unmarshal(rec.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Code == nil || *p.Code != code {
		t.Fatalf("Code = %v, want %d", p.Code, code)
	}
	if p.Reason != reason {
		t.Fatalf("Reason = %q, want %q", p.Reason, reason)
	}
}

func TestAFailedExitIsRecordedOnce(t *testing.T) {
	f := &fakeLog{outputs: make(map[string]string)}
	dl := defaultDeadLetters(t)
	w, err := New(dl, f.replay, f.head, f.appendFn, f.output)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("init Poll: %v", err)
	}
	exitRec := f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 1})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("first Poll: %v", err)
	}
	if len(f.records) != 2 {
		t.Fatalf("records count = %d, want 2", len(f.records))
	}
	dlRec := f.records[1]
	if dlRec.Type != RecordType {
		t.Fatalf("record type = %q, want %q", dlRec.Type, RecordType)
	}
	if dlRec.TaskID != "job-a" {
		t.Fatalf("task_id = %q, want job-a", dlRec.TaskID)
	}
	assertFailedExitPayload(t, dlRec, exitRec.Seq, "real-failure", 1)
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if len(f.records) != 2 {
		t.Fatalf("records count after second Poll = %d, want 2", len(f.records))
	}
}

func TestACleanExitIsNotRecorded(t *testing.T) {
	f := &fakeLog{outputs: make(map[string]string)}
	dl := defaultDeadLetters(t)
	w, err := New(dl, f.replay, f.head, f.appendFn, f.output)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("init Poll: %v", err)
	}
	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 0})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll 1: %v", err)
	}
	if len(f.records) != 1 {
		t.Fatalf("records count = %d, want 1", len(f.records))
	}
	zero := 0
	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": zero, "reason": "deadline"})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll 2: %v", err)
	}
	if len(f.records) != 3 {
		t.Fatalf("records count = %d, want 3", len(f.records))
	}
	assertCodeAndReason(t, f.records[2], 0, "deadline")
}

func TestALostJobIsRecorded(t *testing.T) {
	f := &fakeLog{outputs: make(map[string]string)}
	dl := defaultDeadLetters(t)
	w, err := New(dl, f.replay, f.head, f.appendFn, f.output)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("init Poll: %v", err)
	}
	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "reason": "lost"})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(f.records) != 2 {
		t.Fatalf("records count = %d, want 2", len(f.records))
	}
	dlRec := f.records[1]
	var raw map[string]any
	if err := json.Unmarshal(dlRec.Payload, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["code"]; ok {
		t.Fatalf("code field must be omitted, got %v", raw["code"])
	}
	if raw["reason"] != "lost" {
		t.Fatalf("reason = %v, want lost", raw["reason"])
	}
}

func assertAttemptPayload(t *testing.T, rec eventlog.Record, attempt int, retry, parked bool) {
	t.Helper()
	var p deadLetteredPayload
	if err := json.Unmarshal(rec.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.Attempt != attempt || p.Retry != retry || p.Parked != parked {
		t.Fatalf("payload attempt=%d retry=%v parked=%v, want attempt=%d retry=%v parked=%v",
			p.Attempt, p.Retry, p.Parked, attempt, retry, parked)
	}
}

func TestAttemptsCountConsecutiveFailures(t *testing.T) {
	f := &fakeLog{outputs: make(map[string]string)}
	specs := []broker.ClassSpec{
		{Name: "retry-once", Signatures: []string{`.*`}, MaxRetries: 1},
		{Name: "fallback-class"},
	}
	dl := testDeadLetters(t, specs, "fallback-class", 10)
	w, err := New(dl, f.replay, f.head, f.appendFn, f.output)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("init Poll: %v", err)
	}

	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 1})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll 1: %v", err)
	}
	assertAttemptPayload(t, f.records[1], 1, true, false)

	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 1})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll 2: %v", err)
	}
	assertAttemptPayload(t, f.records[3], 2, false, true)

	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 0})
	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 1})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll 3: %v", err)
	}
	assertAttemptPayload(t, f.records[6], 1, true, false)

	f.appendRecord(t, "job.stopped", "job-a", map[string]any{})
	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 1})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll 4: %v", err)
	}
	assertAttemptPayload(t, f.records[9], 1, true, false)

	f.appendRecord(t, "job.exited", "job-b", map[string]any{"state": "dead", "code": 1})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll 5: %v", err)
	}
	assertAttemptPayload(t, f.records[11], 1, true, false)
}

func TestTheOutputPicksTheClass(t *testing.T) {
	f := &fakeLog{outputs: make(map[string]string)}
	specs := []broker.ClassSpec{
		{Name: "match-class", Signatures: []string{`panic: runtime error`}, MaxRetries: 0},
		{Name: "fallback-class"},
	}
	dl := testDeadLetters(t, specs, "fallback-class", 10)
	w, err := New(dl, f.replay, f.head, f.appendFn, f.output)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("init Poll: %v", err)
	}

	f.outputs["job-match"] = "goroutine 1:\npanic: runtime error: index out of range\n"
	f.appendRecord(t, "job.exited", "job-match", map[string]any{"state": "dead", "code": 2})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll 1: %v", err)
	}
	var p1 deadLetteredPayload
	if err := json.Unmarshal(f.records[1].Payload, &p1); err != nil {
		t.Fatalf("unmarshal 1: %v", err)
	}
	if p1.Class != "match-class" {
		t.Fatalf("p1.Class = %q, want match-class", p1.Class)
	}

	f.outputs["job-nomatch"] = "all tests passed\n"
	f.appendRecord(t, "job.exited", "job-nomatch", map[string]any{"state": "dead", "code": 1})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll 2: %v", err)
	}
	var p2 deadLetteredPayload
	if err := json.Unmarshal(f.records[3].Payload, &p2); err != nil {
		t.Fatalf("unmarshal 2: %v", err)
	}
	if p2.Class != "fallback-class" {
		t.Fatalf("p2.Class = %q, want fallback-class", p2.Class)
	}
}

func TestAFullQueueStillRecords(t *testing.T) {
	f := &fakeLog{outputs: make(map[string]string)}
	specs := []broker.ClassSpec{
		{Name: "fallback-class"},
	}
	dl := testDeadLetters(t, specs, "fallback-class", 1)
	w, err := New(dl, f.replay, f.head, f.appendFn, f.output)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("init Poll: %v", err)
	}

	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 1})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll 1: %v", err)
	}
	var p1 deadLetteredPayload
	if err := json.Unmarshal(f.records[1].Payload, &p1); err != nil {
		t.Fatalf("unmarshal 1: %v", err)
	}
	if !p1.Parked {
		t.Fatalf("p1.Parked = false, want true")
	}

	f.appendRecord(t, "job.exited", "job-b", map[string]any{"state": "dead", "code": 1})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll 2: %v", err)
	}
	var p2 deadLetteredPayload
	if err := json.Unmarshal(f.records[3].Payload, &p2); err != nil {
		t.Fatalf("unmarshal 2: %v", err)
	}
	if p2.Parked {
		t.Fatalf("p2.Parked = true, want false")
	}
}

func TestAnotherWriterWins(t *testing.T) {
	f := &fakeLog{outputs: make(map[string]string)}
	dl := defaultDeadLetters(t)

	w, err := New(dl, f.replay, f.head, func(ctx context.Context, check func(context.Context) (eventlog.Event, error)) (eventlog.Record, error) {
		f.appendRecord(t, RecordType, "job-a", deadLetteredPayload{
			ExitedSeq: 1,
			Class:     "real-failure",
			Attempt:   1,
		})
		return f.appendFn(ctx, check)
	}, f.output)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("init Poll: %v", err)
	}

	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 1})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(f.records) != 2 {
		t.Fatalf("records count = %d, want 2", len(f.records))
	}
}

func TestTheFirstPollStartsAtTheLastVerdict(t *testing.T) {
	t.Run("with prior dead_lettered", testFirstPollWithPriorDeadLettered)
	t.Run("without prior dead_lettered", testFirstPollWithoutPriorDeadLettered)
}

func testFirstPollWithPriorDeadLettered(t *testing.T) {
	ctx := context.Background()
	f := &fakeLog{outputs: make(map[string]string)}
	dl := defaultDeadLetters(t)
	for i := 1; i <= 4; i++ {
		f.appendRecord(t, "job.started", "job-a", map[string]any{})
	}
	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 1})
	f.appendRecord(t, RecordType, "job-a", deadLetteredPayload{
		ExitedSeq: 5,
		Class:     "real-failure",
		Attempt:   1,
	})
	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 1})

	w, err := New(dl, f.replay, f.head, f.appendFn, f.output)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(f.records) != 8 {
		t.Fatalf("records count = %d, want 8", len(f.records))
	}
	var p deadLetteredPayload
	if err := json.Unmarshal(f.records[7].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.ExitedSeq != 7 {
		t.Fatalf("ExitedSeq = %d, want 7", p.ExitedSeq)
	}
}

func testFirstPollWithoutPriorDeadLettered(t *testing.T) {
	ctx := context.Background()
	f := &fakeLog{outputs: make(map[string]string)}
	dl := defaultDeadLetters(t)
	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 1})

	w, err := New(dl, f.replay, f.head, f.appendFn, f.output)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll 1: %v", err)
	}
	if len(f.records) != 1 {
		t.Fatalf("records count = %d, want 1", len(f.records))
	}

	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 1})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("Poll 2: %v", err)
	}
	if len(f.records) != 3 {
		t.Fatalf("records count = %d, want 3", len(f.records))
	}
}

func TestACompactedLogStartsAtHead(t *testing.T) {
	f := &fakeLog{outputs: make(map[string]string)}
	dl := defaultDeadLetters(t)
	f.appendRecord(t, "job.started", "job-a", map[string]any{})
	f.appendRecord(t, "job.started", "job-a", map[string]any{})

	compactedReplay := func(ctx context.Context, after uint64) ([]eventlog.Record, error) {
		if after == 0 {
			return nil, eventlog.ErrCursorCompacted
		}
		return f.replay(ctx, after)
	}

	w, err := New(dl, compactedReplay, f.head, f.appendFn, f.output)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("first Poll: %v", err)
	}
	w.mu.Lock()
	cursor := w.cursor
	w.mu.Unlock()
	if cursor != 2 {
		t.Fatalf("cursor = %d, want 2 (head)", cursor)
	}

	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 1})
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if len(f.records) != 4 {
		t.Fatalf("records count = %d, want 4", len(f.records))
	}
}

func TestNewNilArgs(t *testing.T) {
	f := &fakeLog{}
	dl := defaultDeadLetters(t)
	if _, err := New(nil, f.replay, f.head, f.appendFn, f.output); err == nil {
		t.Fatal("New(nil dl) = nil, want error")
	}
	if _, err := New(dl, nil, f.head, f.appendFn, f.output); err == nil {
		t.Fatal("New(nil replay) = nil, want error")
	}
	if _, err := New(dl, f.replay, nil, f.appendFn, f.output); err == nil {
		t.Fatal("New(nil head) = nil, want error")
	}
	if _, err := New(dl, f.replay, f.head, nil, f.output); err == nil {
		t.Fatal("New(nil appendFn) = nil, want error")
	}
	if _, err := New(dl, f.replay, f.head, f.appendFn, nil); err == nil {
		t.Fatal("New(nil output) = nil, want error")
	}
}

func TestWatcherRunExitsOnContextCancel(t *testing.T) {
	f := &fakeLog{outputs: make(map[string]string)}
	dl := defaultDeadLetters(t)
	w, err := New(dl, f.replay, f.head, f.appendFn, f.output)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx, 10*time.Millisecond, nil)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on ctx cancel")
	}
}

func TestAFailedAppendCountsTheAttemptOnce(t *testing.T) {
	f := &fakeLog{outputs: make(map[string]string)}
	failNext := true
	appendOnce := func(ctx context.Context, check func(context.Context) (eventlog.Event, error)) (eventlog.Record, error) {
		if failNext {
			failNext = false
			return eventlog.Record{}, errors.New("disk full")
		}
		return f.appendFn(ctx, check)
	}
	w, err := New(defaultDeadLetters(t), f.replay, f.head, appendOnce, f.output)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("init Poll: %v", err)
	}
	f.appendRecord(t, "job.exited", "job-a", map[string]any{"state": "dead", "code": 1})
	if err := w.Poll(ctx); err == nil {
		t.Fatal("Poll with a failing append: want an error")
	}
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("retry Poll: %v", err)
	}
	if len(f.records) != 2 {
		t.Fatalf("records count = %d, want 2", len(f.records))
	}
	assertFailedExitPayload(t, f.records[1], 1, "real-failure", 1)
}
