package broker

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/tekmora/epitrop/internal/eventlog"
)

// TopicEvents is the topic where the tail publishes verified event log records.
const TopicEvents = "events"

// ReplayFunc replays records after a sequence number.
type ReplayFunc func(ctx context.Context, after uint64) ([]eventlog.Record, error)

// HeadFunc returns the sequence number of verified HEAD.json.
type HeadFunc func(ctx context.Context) (uint64, error)

// Tail polls an external event log and publishes its records to subscribers in seq order.
type Tail struct {
	broker  *Broker
	topic   string
	replay  ReplayFunc
	head    HeadFunc
	poll    sync.Mutex
	pos     uint64
	started bool
}

// NewTail returns a tail over b that reads from the event log via replay and head.
func NewTail(b *Broker, replay ReplayFunc, head HeadFunc) (*Tail, error) {
	if b == nil || replay == nil || head == nil {
		return nil, errors.New("broker: a tail needs a broker, a replay function, and a head function")
	}
	return &Tail{
		broker: b,
		topic:  TopicEvents,
		replay: replay,
		head:   head,
	}, nil
}

// Poll replays and publishes new records from the event log. The first call sets the tail's
// starting position to HEAD and publishes nothing; later calls replay from that position and
// deliver each new record in seq order.
func (t *Tail) Poll(ctx context.Context) error {
	t.poll.Lock()
	defer t.poll.Unlock()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("broker: tail poll: %w", err)
	}

	t.broker.mu.Lock()
	started := t.started
	pos := t.pos
	t.broker.mu.Unlock()

	if !started {
		headSeq, err := t.head(ctx)
		if err != nil {
			return fmt.Errorf("broker: tail head: %w", err)
		}
		t.broker.mu.Lock()
		t.pos = headSeq
		t.started = true
		t.broker.mu.Unlock()
		return nil
	}

	recs, err := t.replay(ctx, pos)
	if err != nil {
		return fmt.Errorf("broker: tail replay after seq %d: %w", pos, err)
	}

	for _, rec := range recs {
		err := t.broker.publishAfter(ctx, t.topic, func(context.Context) (any, error) {
			t.pos = rec.Seq
			return rec, nil
		})
		if err != nil {
			if errors.Is(err, ErrFull) {
				return nil
			}
			return fmt.Errorf("broker: tail publish seq %d: %w", rec.Seq, err)
		}
	}
	return nil
}

// Resume subscribes to the tail's events and resumes after cursor after. When after is nil,
// it receives live events starting after the current tail position. When after is set, it
// replays records from after up to the tail position, then continues with live records.
func (t *Tail) Resume(ctx context.Context, after *uint64) (*Resumed, func(), error) {
	var (
		started bool
		pos     uint64
	)
	live, cancel := t.broker.subscribeWith(t.topic, func() {
		started = t.started
		pos = t.pos
	})
	if !started {
		cancel()
		return nil, nil, ErrNotStarted
	}
	if after == nil {
		return newResumedFilter(live, t.topic, pos, nil, nil), cancel, nil
	}
	if *after > pos {
		cancel()
		return nil, nil, fmt.Errorf("%w: cursor %d > tail position %d", ErrCursorAhead, *after, pos)
	}
	recs, err := t.replay(ctx, *after)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("broker: resume after seq %d: %w", *after, err)
	}
	r := newResumedFilter(live, t.topic, pos, recs, func(rec eventlog.Record) bool {
		return rec.Seq <= pos
	})
	return r, cancel, nil
}
