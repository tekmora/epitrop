package broker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golusoris/golusoris/realtime/pubsub"
)

const (
	topicA = "unit.result"
	topicB = "watch.release"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newBroker(t *testing.T, capacity int) *Broker {
	t.Helper()
	b, err := New(capacity)
	if err != nil {
		t.Fatalf("New(%d): %v", capacity, err)
	}
	return b
}

func subscribe(t *testing.T, b *Broker, topic string) *Subscription {
	t.Helper()
	sub, cancel := b.Subscribe(topic)
	t.Cleanup(cancel)
	return sub
}

func next(t *testing.T, sub *Subscription) pubsub.Message {
	t.Helper()
	msg, err := sub.Next(testContext(t))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return msg
}

func publish(t *testing.T, b *Broker, topic string, data any) {
	t.Helper()
	if err := b.Publish(testContext(t), pubsub.Message{Topic: topic, Data: data}); err != nil {
		t.Fatalf("Publish(%s, %v): %v", topic, data, err)
	}
}

func TestPublishReachesEverySubscriberOfTheTopic(t *testing.T) {
	b := newBroker(t, 4)
	first, second := subscribe(t, b, topicA), subscribe(t, b, topicA)
	other := subscribe(t, b, topicB)
	publish(t, b, topicA, 1)
	publish(t, b, topicA, 2)
	for _, sub := range []*Subscription{first, second} {
		for _, want := range []int{1, 2} {
			if got := next(t, sub); got.Topic != topicA || got.Data != want {
				t.Fatalf("got %+v, want %s %d", got, topicA, want)
			}
		}
	}
	if got := other.Len(); got != 0 {
		t.Fatalf("subscriber of %s holds %d messages, want 0", topicB, got)
	}
}

func TestAFullQueueRefusesWithARetryableError(t *testing.T) {
	b := newBroker(t, 2)
	sub := subscribe(t, b, topicA)
	publish(t, b, topicA, 1)
	publish(t, b, topicA, 2)
	err := b.Publish(testContext(t), pubsub.Message{Topic: topicA, Data: 3})
	if !errors.Is(err, ErrFull) || !Retryable(err) {
		t.Fatalf("Publish on a full queue = %v, want ErrFull and retryable", err)
	}
	if got := next(t, sub); got.Data != 1 {
		t.Fatalf("first message = %v, want 1", got.Data)
	}
	publish(t, b, topicA, 3)
	for _, want := range []int{2, 3} {
		if got := next(t, sub); got.Data != want {
			t.Fatalf("message = %v, want %d", got.Data, want)
		}
	}
}

func TestARefusedPublishReachesNoSubscriber(t *testing.T) {
	b := newBroker(t, 1)
	drained, full := subscribe(t, b, topicA), subscribe(t, b, topicA)
	publish(t, b, topicA, 1)
	next(t, drained)
	if err := b.Publish(testContext(t), pubsub.Message{Topic: topicA, Data: 2}); !errors.Is(err, ErrFull) {
		t.Fatalf("Publish with one full queue = %v, want ErrFull", err)
	}
	if got := drained.Len(); got != 0 {
		t.Fatalf("drained subscriber got %d messages from a refused publish, want 0", got)
	}
	if got := next(t, full); got.Data != 1 {
		t.Fatalf("full subscriber's message = %v, want 1", got.Data)
	}
}

func TestACancelledSubscriptionNoLongerHoldsBackPublishing(t *testing.T) {
	b := newBroker(t, 1)
	sub, cancel := b.Subscribe(topicA)
	publish(t, b, topicA, 1)
	cancel()
	publish(t, b, topicA, 2)
	if got := sub.Len(); got != 1 {
		t.Fatalf("cancelled subscriber holds %d messages, want 1", got)
	}
}

func TestCapacityBounds(t *testing.T) {
	for _, capacity := range []int{-1, 0, MaxCapacity + 1} {
		if _, err := New(capacity); !errors.Is(err, ErrCapacity) {
			t.Fatalf("New(%d) = %v, want ErrCapacity", capacity, err)
		}
	}
	for _, capacity := range []int{1, MaxCapacity} {
		if _, err := New(capacity); err != nil {
			t.Fatalf("New(%d): %v", capacity, err)
		}
	}
}

func TestNextStopsWhenTheContextEnds(t *testing.T) {
	b := newBroker(t, 1)
	sub := subscribe(t, b, topicA)
	ctx, cancel := context.WithCancel(testContext(t))
	cancel()
	if _, err := sub.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next on an empty queue with an ended context = %v, want context.Canceled", err)
	}
}

func TestPublishWithAnEndedContextDeliversNothing(t *testing.T) {
	b := newBroker(t, 1)
	sub := subscribe(t, b, topicA)
	ctx, cancel := context.WithCancel(testContext(t))
	cancel()
	err := b.Publish(ctx, pubsub.Message{Topic: topicA, Data: 1})
	if !errors.Is(err, context.Canceled) || Retryable(err) {
		t.Fatalf("Publish with an ended context = %v, want context.Canceled, not retryable", err)
	}
	if got := sub.Len(); got != 0 {
		t.Fatalf("subscriber holds %d messages, want 0", got)
	}
}

func TestOnlyAFullQueueIsRetryable(t *testing.T) {
	for _, err := range []error{nil, ErrCapacity, context.DeadlineExceeded, errors.New("other")} {
		if Retryable(err) {
			t.Fatalf("Retryable(%v) = true, want false", err)
		}
	}
}

// publishAll publishes n messages and returns how many the broker accepted. A refusal other
// than a full queue fails the test.
func publishAll(ctx context.Context, t *testing.T, b *Broker, n int) int {
	accepted := 0
	for i := 0; i < n; i++ {
		err := b.Publish(ctx, pubsub.Message{Topic: topicA, Data: i})
		if err == nil {
			accepted++
		} else if !Retryable(err) {
			t.Errorf("Publish: %v", err)
			return accepted
		}
	}
	return accepted
}

// drain reads sub until ctx ends, then counts what is left in its queue.
func drain(ctx context.Context, t *testing.T, sub *Subscription, limit int) int {
	received := 0
	for ; received <= limit; received++ {
		if _, err := sub.Next(ctx); err != nil {
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Next: %v", err)
			}
			break
		}
	}
	return received + sub.Len()
}

func TestConcurrentPublishersEitherDeliverOrAreRefused(t *testing.T) {
	const publishers, perPublisher = 4, 200
	b := newBroker(t, 8)
	sub := subscribe(t, b, topicA)
	ctx := testContext(t)
	var wg sync.WaitGroup
	var accepted atomic.Int64
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			accepted.Add(int64(publishAll(ctx, t, b, perPublisher)))
		}()
	}
	readCtx, stopReading := context.WithCancel(ctx)
	go func() { wg.Wait(); stopReading() }()
	received := drain(readCtx, t, sub, publishers*perPublisher)
	wg.Wait()
	if int64(received) != accepted.Load() {
		t.Fatalf("received %d messages, %d publishes were accepted", received, accepted.Load())
	}
}
