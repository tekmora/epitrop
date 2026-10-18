package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/eventlog"
)

func publishLog(t *testing.T, f logFixture, evs ...eventlog.Event) {
	t.Helper()
	for _, ev := range evs {
		if _, err := f.log.Publish(testContext(t), ev); err != nil {
			t.Fatalf("Publish(%s): %v", ev.Payload, err)
		}
	}
}

func eventOn(topic string, n int) eventlog.Event {
	return eventlog.Event{Type: topic, TaskID: "u1", Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, n))}
}

func resume(t *testing.T, f logFixture, topic string, after uint64) *Resumed {
	t.Helper()
	r, cancel, err := f.log.SubscribeFrom(testContext(t), topic, after)
	if err != nil {
		t.Fatalf("SubscribeFrom(%s, %d): %v", topic, after, err)
	}
	t.Cleanup(cancel)
	return r
}

// takeSeqs reads n records from r and returns their seqs.
func takeSeqs(t *testing.T, r *Resumed, n int) []uint64 {
	t.Helper()
	seqs := make([]uint64, 0, n)
	for range n {
		rec, err := r.Next(testContext(t))
		if err != nil {
			t.Fatalf("Next after seqs %v: %v", seqs, err)
		}
		seqs = append(seqs, rec.Seq)
	}
	return seqs
}

func assertSeqList(t *testing.T, got []uint64, want ...uint64) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("seqs = %v, want %v", got, want)
	}
}

func assertNothingWaiting(t *testing.T, r *Resumed) {
	t.Helper()
	ctx, cancel := context.WithCancel(testContext(t))
	cancel()
	if rec, err := r.Next(ctx); err == nil {
		t.Fatalf("Next = seq %d, want nothing waiting", rec.Seq)
	}
}

func TestResumeReplaysTheLogThenGoesLive(t *testing.T) {
	f := newLog(t, 4)
	publishLog(t, f, event(1), event(2), event(3))
	r := resume(t, f, topicA, 1)
	assertSeqList(t, takeSeqs(t, r, 2), 2, 3)
	publishLog(t, f, event(4), event(5))
	assertSeqList(t, takeSeqs(t, r, 2), 4, 5)
	assertNothingWaiting(t, r)
}

func TestResumeHandsTheLiveCopyOfAReplayedRecordOutOnce(t *testing.T) {
	f := newLog(t, 4)
	live := subscribe(t, f.log.broker, topicA)
	publishLog(t, f, event(1), event(2), event(3))
	recs, err := f.log.writer.ReplayAfter(testContext(t), 1)
	if err != nil {
		t.Fatalf("ReplayAfter: %v", err)
	}
	r := newResumed(live, topicA, 1, recs)
	assertSeqList(t, takeSeqs(t, r, 2), 2, 3)
	publishLog(t, f, event(4))
	assertSeqList(t, takeSeqs(t, r, 1), 4)
	assertNothingWaiting(t, r)
}

func TestResumeSubscribesBeforeItReplays(t *testing.T) {
	f := newLog(t, 4)
	publishLog(t, f, event(1))
	// A record appended after the replay read the log reaches only the live queue.
	replay := func(ctx context.Context, after uint64) ([]eventlog.Record, error) {
		recs, err := f.log.writer.ReplayAfter(ctx, after)
		publishLog(t, f, event(2))
		return recs, err
	}
	r, cancel, err := f.log.resume(testContext(t), topicA, 0, replay)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	t.Cleanup(cancel)
	assertSeqList(t, takeSeqs(t, r, 2), 1, 2)
	assertNothingWaiting(t, r)
}

func TestResumeKeepsOnlyItsTopic(t *testing.T) {
	f := newLog(t, 4)
	publishLog(t, f, eventOn(topicA, 1), eventOn(topicB, 2), eventOn(topicA, 3), eventOn(topicB, 4))
	r := resume(t, f, topicA, 0)
	assertSeqList(t, takeSeqs(t, r, 2), 1, 3)
	publishLog(t, f, eventOn(topicB, 5), eventOn(topicA, 6))
	assertSeqList(t, takeSeqs(t, r, 1), 6)
	assertNothingWaiting(t, r)
}

func TestResumeOnAnEmptyLogIsLive(t *testing.T) {
	f := newLog(t, 2)
	r := resume(t, f, topicA, 0)
	assertNothingWaiting(t, r)
	publishLog(t, f, event(1))
	assertSeqList(t, takeSeqs(t, r, 1), 1)
}

func TestResumeAtTheHeadIsLive(t *testing.T) {
	f := newLog(t, 2)
	publishLog(t, f, event(1), event(2))
	r := resume(t, f, topicA, 2)
	assertNothingWaiting(t, r)
	publishLog(t, f, event(3))
	assertSeqList(t, takeSeqs(t, r, 1), 3)
}

func TestResumePastTheHeadFailsAndLeavesNoSubscriber(t *testing.T) {
	f := newLog(t, 1)
	publishLog(t, f, event(1))
	_, cancel, err := f.log.SubscribeFrom(testContext(t), topicA, 5)
	if err == nil || cancel != nil {
		t.Fatalf("SubscribeFrom past the head = %v, cancel set %v; want an error", err, cancel != nil)
	}
	assertErrHas(t, err, "resume "+topicA+" after seq 5", "no such record")
	f.log.broker.mu.Lock()
	n := len(f.log.broker.subs[topicA])
	f.log.broker.mu.Unlock()
	if n != 0 {
		t.Fatalf("subscribers of %s after a failed resume = %d, want 0", topicA, n)
	}
	publishLog(t, f, event(2))
}

func TestResumeRefusesAMessageThatIsNoRecord(t *testing.T) {
	f := newLog(t, 2)
	r := resume(t, f, topicA, 0)
	publish(t, f.log.broker, topicA, "plain")
	_, err := r.Next(testContext(t))
	assertErrHas(t, err, "topic "+topicA, "string", "not an event-log record")
}

func TestResumeBoundsTheCopiesItDrops(t *testing.T) {
	f := newLog(t, 2)
	live := subscribe(t, f.log.broker, topicA)
	publishLog(t, f, event(1), event(2))
	r := newResumed(live, topicA, 10, nil)
	done := make(chan error, 1)
	go func() {
		_, err := r.Next(testContext(t))
		done <- err
	}()
	// A third copy reaches the queue once Next has taken the first two.
	for range 1000 {
		if _, err := f.log.Publish(testContext(t), event(3)); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	assertErrHas(t, <-done, "more replayed copies than its capacity 2")
}

func assertErrHas(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want failure")
	}
	for _, part := range parts {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("error %q does not contain %q", err, part)
		}
	}
}
