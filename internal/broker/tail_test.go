package broker

import (
	"context"
	"errors"
	"testing"

	"github.com/golusoris/golusoris/realtime/pubsub"
	"github.com/tekmora/epitrop/internal/eventlog"
)

func makeRec(seq uint64, eventType string) eventlog.Record {
	return eventlog.Record{
		Seq:     seq,
		Type:    eventType,
		TaskID:  "u1",
		Payload: []byte("{}"),
	}
}

func nextSeq(t *testing.T, ctx context.Context, sub *Subscription) uint64 {
	t.Helper()
	m, err := sub.Next(ctx)
	if err != nil {
		t.Fatalf("sub.Next = %v", err)
	}
	r, ok := m.Data.(eventlog.Record)
	if !ok {
		t.Fatalf("sub.Next data type = %T, want eventlog.Record", m.Data)
	}
	return r.Seq
}

// wantSeqs reads the records from through to off resumed and fails unless they come in
// that order with nothing between.
func wantSeqs(ctx context.Context, t *testing.T, resumed *Resumed, from, to uint64) {
	t.Helper()
	for want := from; want <= to; want++ {
		rec, err := resumed.Next(ctx)
		if err != nil {
			t.Fatalf("Next for seq %d = %v", want, err)
		}
		if rec.Seq != want {
			t.Fatalf("got seq %d, want %d", rec.Seq, want)
		}
	}
}

func testTail(t *testing.T, capacity int, recs map[uint64]eventlog.Record, headSeq uint64) (*Broker, *Tail) {
	t.Helper()
	b, err := New(capacity)
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	tail, err := NewTail(
		b,
		func(_ context.Context, after uint64) ([]eventlog.Record, error) {
			var out []eventlog.Record
			for s := after + 1; s <= headSeq; s++ {
				if r, ok := recs[s]; ok {
					out = append(out, r)
				}
			}
			return out, nil
		},
		func(_ context.Context) (uint64, error) {
			return headSeq, nil
		},
	)
	if err != nil {
		t.Fatalf("NewTail = %v", err)
	}
	return b, tail
}

func TestNewTailValidatesInputs(t *testing.T) {
	b, err := New(4)
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	rep := func(context.Context, uint64) ([]eventlog.Record, error) { return nil, nil }
	hd := func(context.Context) (uint64, error) { return 0, nil }

	for _, tc := range []struct {
		b   *Broker
		rep ReplayFunc
		hd  HeadFunc
	}{
		{nil, rep, hd},
		{b, nil, hd},
		{b, rep, nil},
	} {
		if _, err := NewTail(tc.b, tc.rep, tc.hd); err == nil {
			t.Fatal("NewTail with nil input succeeded, want error")
		}
	}
}

func TestTailFirstPollPublishesNothing(t *testing.T) {
	recs := map[uint64]eventlog.Record{
		1: makeRec(1, "unit.recorded"),
		2: makeRec(2, "unit.recorded"),
	}
	b, tail := testTail(t, 4, recs, 2)
	sub, cancel := b.Subscribe(TopicEvents)
	defer cancel()

	if err := tail.Poll(testContext(t)); err != nil {
		t.Fatalf("first Poll = %v", err)
	}
	if sub.Len() != 0 {
		t.Fatalf("first Poll queued %d messages, want 0", sub.Len())
	}
}

func TestTailLaterPollPublishesInSeqOrder(t *testing.T) {
	recs := map[uint64]eventlog.Record{
		1: makeRec(1, "unit.recorded"),
		2: makeRec(2, "unit.recorded"),
	}
	b, tail := testTail(t, 4, recs, 0)
	sub, cancel := b.Subscribe(TopicEvents)
	defer cancel()

	ctx := testContext(t)
	if err := tail.Poll(ctx); err != nil {
		t.Fatalf("first Poll = %v", err)
	}

	tail.head = func(context.Context) (uint64, error) { return 2, nil }
	tail.replay = func(_ context.Context, after uint64) ([]eventlog.Record, error) {
		var out []eventlog.Record
		for s := after + 1; s <= 2; s++ {
			out = append(out, recs[s])
		}
		return out, nil
	}

	if err := tail.Poll(ctx); err != nil {
		t.Fatalf("second Poll = %v", err)
	}

	s1 := nextSeq(t, ctx, sub)
	s2 := nextSeq(t, ctx, sub)
	if s1 != 1 || s2 != 2 {
		t.Fatalf("got seqs %d, %d; want 1, 2", s1, s2)
	}
}

func TestTailErrFullStopsMidBatchAndResumes(t *testing.T) {
	recs := map[uint64]eventlog.Record{
		1: makeRec(1, "unit.recorded"),
		2: makeRec(2, "unit.recorded"),
		3: makeRec(3, "unit.recorded"),
	}
	b, tail := testTail(t, 2, recs, 0)
	sub, cancel := b.Subscribe(TopicEvents)
	defer cancel()

	ctx := testContext(t)
	if err := tail.Poll(ctx); err != nil {
		t.Fatalf("first Poll = %v", err)
	}

	tail.replay = func(_ context.Context, after uint64) ([]eventlog.Record, error) {
		var out []eventlog.Record
		for s := after + 1; s <= 3; s++ {
			out = append(out, recs[s])
		}
		return out, nil
	}

	// Queue capacity is 2. Publishing 1, 2, 3 fills queue on 2; 3 returns ErrFull.
	if err := tail.Poll(ctx); err != nil {
		t.Fatalf("Poll with full queue = %v, want nil", err)
	}
	if sub.Len() != 2 {
		t.Fatalf("sub.Len = %d, want 2", sub.Len())
	}

	// Drain messages 1 and 2.
	s1 := nextSeq(t, ctx, sub)
	s2 := nextSeq(t, ctx, sub)
	if s1 != 1 || s2 != 2 {
		t.Fatalf("got seqs %d, %d; want 1, 2", s1, s2)
	}

	// Next poll continues from pos=2, delivering 3.
	if err := tail.Poll(ctx); err != nil {
		t.Fatalf("Poll after drain = %v", err)
	}
	s3 := nextSeq(t, ctx, sub)
	if s3 != 3 {
		t.Fatalf("got seq %d, want 3", s3)
	}
}

func TestTailResumeBeforeFirstPollFails(t *testing.T) {
	_, tail := testTail(t, 4, nil, 0)
	after := uint64(0)
	_, _, err := tail.Resume(testContext(t), &after)
	if !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Resume before Poll = %v, want ErrNotStarted", err)
	}
}

func TestTailResumeAfterPastHeadFails(t *testing.T) {
	_, tail := testTail(t, 4, nil, 5)
	ctx := testContext(t)
	if err := tail.Poll(ctx); err != nil {
		t.Fatalf("Poll = %v", err)
	}
	after := uint64(6)
	_, _, err := tail.Resume(ctx, &after)
	if !errors.Is(err, ErrCursorAhead) {
		t.Fatalf("Resume past pos = %v, want ErrCursorAhead", err)
	}
}

func TestTailResumeCompactedFails(t *testing.T) {
	_, tail := testTail(t, 4, nil, 10)
	ctx := testContext(t)
	if err := tail.Poll(ctx); err != nil {
		t.Fatalf("Poll = %v", err)
	}
	tail.replay = func(context.Context, uint64) ([]eventlog.Record, error) {
		return nil, eventlog.ErrCursorCompacted
	}
	after := uint64(1)
	_, _, err := tail.Resume(ctx, &after)
	if !errors.Is(err, eventlog.ErrCursorCompacted) {
		t.Fatalf("Resume compacted = %v, want ErrCursorCompacted", err)
	}
}

func TestTailResumeHandoverArrivesExactlyOnce(t *testing.T) {
	recs := map[uint64]eventlog.Record{
		1: makeRec(1, "unit.recorded"),
		2: makeRec(2, "unit.recorded"),
		3: makeRec(3, "unit.recorded"),
		4: makeRec(4, "unit.recorded"),
		5: makeRec(5, "unit.recorded"),
	}
	b, err := New(10)
	if err != nil {
		t.Fatalf("New = %v", err)
	}

	var tail *Tail
	ctx := testContext(t)
	tail, err = NewTail(
		b,
		func(ctx context.Context, after uint64) ([]eventlog.Record, error) {
			// During replay, publish records 4 and 5 live.
			pErr := b.Publish(ctx, pubsub.Message{Topic: TopicEvents, Data: recs[4]})
			if pErr != nil {
				return nil, pErr
			}
			pErr = b.Publish(ctx, pubsub.Message{Topic: TopicEvents, Data: recs[5]})
			if pErr != nil {
				return nil, pErr
			}
			// Replay returns records 2, 3, 4, 5. Seq 4 and 5 are > pos=3, so filter keeps 2, 3.
			return []eventlog.Record{recs[2], recs[3], recs[4], recs[5]}, nil
		},
		func(context.Context) (uint64, error) { return 3, nil },
	)
	if err != nil {
		t.Fatalf("NewTail = %v", err)
	}
	if err := tail.Poll(ctx); err != nil {
		t.Fatalf("Poll = %v", err)
	}

	after := uint64(1)
	resumed, cancel, err := tail.Resume(ctx, &after)
	if err != nil {
		t.Fatalf("Resume = %v", err)
	}
	defer cancel()

	wantSeqs(ctx, t, resumed, 2, 5)
	// Records 4 and 5 came once: the next record is the next live one, not a copy.
	if err := b.Publish(ctx, pubsub.Message{Topic: TopicEvents, Data: makeRec(6, "unit.recorded")}); err != nil {
		t.Fatalf("Publish(6) = %v", err)
	}
	wantSeqs(ctx, t, resumed, 6, 6)
}

func TestTailResumeLagGreaterThanQueueCapacity(t *testing.T) {
	recs := map[uint64]eventlog.Record{}
	for i := uint64(1); i <= 10; i++ {
		recs[i] = makeRec(i, "unit.recorded")
	}
	// Broker queue capacity is only 2, but backlog has 8 records.
	_, tail := testTail(t, 2, recs, 10)
	ctx := testContext(t)
	if err := tail.Poll(ctx); err != nil {
		t.Fatalf("Poll = %v", err)
	}

	after := uint64(2)
	resumed, cancel, err := tail.Resume(ctx, &after)
	if err != nil {
		t.Fatalf("Resume = %v", err)
	}
	defer cancel()

	wantSeqs(ctx, t, resumed, 3, 10)
}

func TestTailResumeLiveOnlyWhenAfterNil(t *testing.T) {
	recs := map[uint64]eventlog.Record{
		1: makeRec(1, "unit.recorded"),
	}
	b, tail := testTail(t, 4, recs, 1)
	ctx := testContext(t)
	if err := tail.Poll(ctx); err != nil {
		t.Fatalf("Poll = %v", err)
	}

	resumed, cancel, err := tail.Resume(ctx, nil)
	if err != nil {
		t.Fatalf("Resume = %v", err)
	}
	defer cancel()

	// Live publish of seq 2.
	rec2 := makeRec(2, "unit.recorded")
	if err := b.Publish(ctx, pubsub.Message{Topic: TopicEvents, Data: rec2}); err != nil {
		t.Fatalf("Publish = %v", err)
	}

	got, err := resumed.Next(ctx)
	if err != nil {
		t.Fatalf("Next = %v", err)
	}
	if got.Seq != 2 {
		t.Fatalf("got seq %d, want 2", got.Seq)
	}
}
