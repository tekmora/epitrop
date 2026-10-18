package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golusoris/golusoris/core/clock"
	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"

	"github.com/tekmora/epitrop/internal/eventlog"
)

type logFixture struct {
	log    *Log
	dir    string
	pub    string
	limits eventlog.Limits
}

func newLog(t *testing.T, capacity int) logFixture {
	t.Helper()
	clk := clock.NewFake()
	signer, err := corereceipt.NewSignerFromSeed(bytes.Repeat([]byte{7}, 32), clk)
	if err != nil {
		t.Fatalf("NewSignerFromSeed: %v", err)
	}
	f := logFixture{dir: t.TempDir(), pub: signer.PublicKey(), limits: eventlog.Limits{Clock: clk, LockTimeout: time.Minute, MaxReplayRecords: 100}}
	w, err := eventlog.Open(f.dir, signer, f.limits)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	if f.log, err = NewLog(newBroker(t, capacity), w); err != nil {
		t.Fatalf("NewLog: %v", err)
	}
	return f
}

// records replays the event log and returns every record it holds, in order.
func (f logFixture) records(t *testing.T) []eventlog.Record {
	t.Helper()
	var recs []eventlog.Record
	_, err := eventlog.Replay(testContext(t), f.dir, f.pub, f.limits, func(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
		recs = append(recs, rec)
		return st, nil
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return recs
}

func event(n int) eventlog.Event {
	return eventlog.Event{Type: topicA, TaskID: "u1", Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, n))}
}

func TestLogPublishDeliversTheRecordTheLogHolds(t *testing.T) {
	f := newLog(t, 4)
	sub := subscribe(t, f.log.broker, topicA)
	var returned []eventlog.Record
	for n := 1; n <= 2; n++ {
		rec, err := f.log.Publish(testContext(t), event(n))
		if err != nil {
			t.Fatalf("Publish(%d): %v", n, err)
		}
		returned = append(returned, rec)
	}
	logged := f.records(t)
	if len(logged) != 2 {
		t.Fatalf("log holds %d records, want 2", len(logged))
	}
	for i, want := range logged {
		got := next(t, sub)
		if got.Topic != topicA || !reflect.DeepEqual(got.Data, want) || !reflect.DeepEqual(returned[i], want) {
			t.Fatalf("message %d = %+v, returned %+v; want the logged record %+v", i, got, returned[i], want)
		}
		if want.Seq != uint64(i+1) {
			t.Fatalf("record %d has seq %d", i, want.Seq)
		}
	}
}

func TestLogPublishOnAFullQueueAppendsNothing(t *testing.T) {
	f := newLog(t, 1)
	subscribe(t, f.log.broker, topicA)
	if _, err := f.log.Publish(testContext(t), event(1)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	_, err := f.log.Publish(testContext(t), event(2))
	if !errors.Is(err, ErrFull) || !Retryable(err) {
		t.Fatalf("Publish on a full queue = %v, want ErrFull", err)
	}
	if got := len(f.records(t)); got != 1 {
		t.Fatalf("log holds %d records after a refused publish, want 1", got)
	}
}

func TestLogPublishWhoseAppendFailsDeliversNothing(t *testing.T) {
	f := newLog(t, 4)
	sub := subscribe(t, f.log.broker, topicA)
	big := eventlog.Event{Type: topicA, TaskID: "u1", Payload: json.RawMessage(`"` + strings.Repeat("a", eventlog.MaxRecordBytes) + `"`)}
	_, err := f.log.Publish(testContext(t), big)
	if !errors.Is(err, eventlog.ErrRecordTooLarge) || Retryable(err) {
		t.Fatalf("Publish of an oversized event = %v, want ErrRecordTooLarge, not retryable", err)
	}
	if got := sub.Len(); got != 0 {
		t.Fatalf("subscriber got %d messages from a failed append, want 0", got)
	}
	if _, err := f.log.Publish(testContext(t), event(1)); err != nil {
		t.Fatalf("Publish after a failed append: %v", err)
	}
	if rec, ok := next(t, sub).Data.(eventlog.Record); !ok || rec.Seq != 1 {
		t.Fatalf("first delivered record = %+v, %v; want seq 1", rec, ok)
	}
}

func TestLogPublishWithoutSubscribersStillAppends(t *testing.T) {
	f := newLog(t, 1)
	other := subscribe(t, f.log.broker, topicB)
	for n := 1; n <= 2; n++ {
		if _, err := f.log.Publish(testContext(t), event(n)); err != nil {
			t.Fatalf("Publish(%d): %v", n, err)
		}
	}
	if got := len(f.records(t)); got != 2 {
		t.Fatalf("log holds %d records, want 2", got)
	}
	if got := other.Len(); got != 0 {
		t.Fatalf("subscriber of %s holds %d messages, want 0", topicB, got)
	}
}

func TestLogSubscribeIsTheBrokers(t *testing.T) {
	f := newLog(t, 1)
	sub, cancel := f.log.Subscribe(topicA)
	t.Cleanup(cancel)
	if _, err := f.log.Publish(testContext(t), event(1)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := sub.Len(); got != 1 {
		t.Fatalf("subscriber holds %d messages, want 1", got)
	}
}

func TestNewLogNeedsBothParts(t *testing.T) {
	f := newLog(t, 1)
	if _, err := NewLog(nil, f.log.writer); err == nil {
		t.Fatal("NewLog(nil, w) = nil error")
	}
	if _, err := NewLog(f.log.broker, nil); err == nil {
		t.Fatal("NewLog(b, nil) = nil error")
	}
}
