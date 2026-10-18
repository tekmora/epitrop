package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/tekmora/epitrop/internal/eventlog"
)

// Log publishes log-first: an event is appended to the event log before any subscriber
// sees it, and subscribers receive the signed eventlog.Record the log holds. A subscriber
// can therefore read back from the log whatever it was given (ADR-0004).
type Log struct {
	broker *Broker
	writer *eventlog.Writer
}

// NewLog returns a log-first publisher over b and the event log w appends to.
func NewLog(b *Broker, w *eventlog.Writer) (*Log, error) {
	if b == nil || w == nil {
		return nil, errors.New("broker: a log-first publisher needs a broker and an event-log writer")
	}
	return &Log{broker: b, writer: w}, nil
}

// Publish appends ev and delivers its record to every subscriber of the topic ev.Type, so a
// record's topic can be read back from the log. When a queue is full it returns ErrFull and
// appends nothing; when the append fails nobody gets a message. The append runs under the
// broker's lock, so publishes wait for it, up to the event log's lock timeout.
func (l *Log) Publish(ctx context.Context, ev eventlog.Event) (eventlog.Record, error) {
	topic := ev.Type
	var rec eventlog.Record
	err := l.broker.publishAfter(ctx, topic, func(ctx context.Context) (any, error) {
		appended, err := l.writer.Append(ctx, ev)
		if err != nil {
			return nil, fmt.Errorf("broker: append to the event log before %s: %w", topic, err)
		}
		rec = appended
		return appended, nil
	})
	if err != nil {
		return eventlog.Record{}, err
	}
	return rec, nil
}

// Subscribe adds a subscriber to topic; see Broker.Subscribe.
func (l *Log) Subscribe(topic string) (*Subscription, func()) {
	return l.broker.Subscribe(topic)
}

// Resumed is a subscription that starts after a cursor: it hands out the records of its
// topic that the log holds after the cursor, then the live ones, each once and in seq order.
type Resumed struct {
	live    *Subscription
	topic   string
	backlog []eventlog.Record
	// seen is the last seq the replay read; a live record at or below it is a copy.
	seen uint64
}

// SubscribeFrom subscribes to topic and resumes after seq after, the last seq the caller
// has seen. It subscribes before it reads the log, so a record appended meanwhile lies in
// the log, in the live queue, or in both, and Next drops the copy. The replay is
// eventlog.ReplayAfter, with its bound and its errors; meanwhile the live queue fills, and a
// publish that finds it full gets ErrFull as with any subscriber. The returned function
// ends the subscription.
func (l *Log) SubscribeFrom(ctx context.Context, topic string, after uint64) (*Resumed, func(), error) {
	return l.resume(ctx, topic, after, l.writer.ReplayAfter)
}

// resume is SubscribeFrom with the replay passed in.
func (l *Log) resume(ctx context.Context, topic string, after uint64, replay func(context.Context, uint64) ([]eventlog.Record, error)) (*Resumed, func(), error) {
	live, cancel := l.broker.Subscribe(topic)
	recs, err := replay(ctx, after)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("broker: resume %s after seq %d: %w", topic, after, err)
	}
	return newResumed(live, topic, after, recs), cancel, nil
}

// newResumed keeps the records of topic among recs, the replay after seq after.
func newResumed(live *Subscription, topic string, after uint64, recs []eventlog.Record) *Resumed {
	r := &Resumed{live: live, topic: topic, seen: after}
	for _, rec := range recs {
		if rec.Type == topic {
			r.backlog = append(r.backlog, rec)
		}
		r.seen = rec.Seq
	}
	return r
}

// Next returns the next record: the replayed ones first, then the live ones past them. A
// live copy of a replayed record was queued before the replay ended, so at most a queue's
// worth of them is dropped. The queue holds records in seq order, so every copy comes
// before the first record past seen and seen need not move.
func (r *Resumed) Next(ctx context.Context) (eventlog.Record, error) {
	if len(r.backlog) > 0 {
		rec := r.backlog[0]
		r.backlog = r.backlog[1:]
		return rec, nil
	}
	for range cap(r.live.queue) + 1 {
		msg, err := r.live.Next(ctx)
		if err != nil {
			return eventlog.Record{}, err
		}
		rec, ok := msg.Data.(eventlog.Record)
		if !ok {
			return eventlog.Record{}, fmt.Errorf("broker: topic %s carried a %T, not an event-log record", r.topic, msg.Data)
		}
		if rec.Seq > r.seen {
			return rec, nil
		}
	}
	return eventlog.Record{}, fmt.Errorf("broker: topic %s queued more replayed copies than its capacity %d", r.topic, cap(r.live.queue))
}
