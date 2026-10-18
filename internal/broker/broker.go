// Package broker is the in-process broker of the control plane: fan-out through golusoris
// realtime/pubsub, with a bounded queue per subscriber (HISS-06). A publish that would
// overflow any subscriber's queue is refused as a whole with a retryable error, so a slow
// consumer pushes back on its producers instead of growing memory or losing messages.
package broker

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/golusoris/golusoris/realtime/pubsub"
)

// MaxCapacity is the largest queue a subscriber may get.
const MaxCapacity = 4096

var (
	// ErrFull says a subscriber's queue had no room, so no subscriber got the message. It
	// is retryable: the publish can succeed once the subscriber has taken messages.
	ErrFull = errors.New("broker: a subscriber queue is full")
	// ErrCapacity says the queue capacity is outside 1 to MaxCapacity.
	ErrCapacity = errors.New("broker: queue capacity out of range")
)

// Retryable reports whether err refused a publish that may succeed when tried again later.
func Retryable(err error) bool {
	return errors.Is(err, ErrFull)
}

// Broker fans messages out to the subscribers of their topic. Each subscriber has its own
// queue of a fixed capacity.
type Broker struct {
	capacity int
	bus      pubsub.Bus
	// mu orders publishes against each other and against subscription changes, so the
	// room seen by Publish is still there when the bus delivers: queues only fill under mu.
	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{}
}

// Subscription is one subscriber's queue. Its messages arrive in publish order.
type Subscription struct {
	queue chan pubsub.Message
}

// New returns a broker whose subscribers each get a queue of capacity messages.
func New(capacity int) (*Broker, error) {
	if capacity < 1 || capacity > MaxCapacity {
		return nil, fmt.Errorf("%w: %d, want 1 to %d", ErrCapacity, capacity, MaxCapacity)
	}
	return &Broker{capacity: capacity, bus: pubsub.New(), subs: make(map[string]map[*Subscription]struct{})}, nil
}

// Subscribe adds a subscriber to topic. The returned function removes it; messages already
// in its queue stay readable.
func (b *Broker) Subscribe(topic string) (*Subscription, func()) {
	sub := &Subscription{queue: make(chan pubsub.Message, b.capacity)}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[topic] == nil {
		b.subs[topic] = make(map[*Subscription]struct{})
	}
	b.subs[topic][sub] = struct{}{}
	stop := b.bus.Subscribe(topic, func(msg pubsub.Message) { sub.queue <- msg })
	var once sync.Once
	return sub, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			stop()
			delete(b.subs[topic], sub)
			if len(b.subs[topic]) == 0 {
				delete(b.subs, topic)
			}
		})
	}
}

// Publish delivers msg to every subscriber of its topic, or to none: when any of their
// queues is full it returns ErrFull. A topic without subscribers takes the message and
// delivers it nowhere.
func (b *Broker) Publish(ctx context.Context, msg pubsub.Message) error {
	return b.publishAfter(ctx, msg.Topic, func(context.Context) (any, error) { return msg.Data, nil })
}

// publishAfter calls first once every subscriber of topic has room, and delivers what it
// returns to all of them. first runs under mu, so the room it was given is still there; an
// error from first delivers nothing.
func (b *Broker) publishAfter(ctx context.Context, topic string, first func(context.Context) (any, error)) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("broker: publish to %s: %w", topic, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs[topic] {
		if len(sub.queue) == cap(sub.queue) {
			return fmt.Errorf("%w: topic %s, capacity %d", ErrFull, topic, b.capacity)
		}
	}
	data, err := first(ctx)
	if err != nil {
		return err
	}
	b.bus.Publish(ctx, pubsub.Message{Topic: topic, Data: data})
	return nil
}

// Next returns the oldest message in the queue, waiting for one until ctx ends.
func (s *Subscription) Next(ctx context.Context) (pubsub.Message, error) {
	select {
	case msg := <-s.queue:
		return msg, nil
	case <-ctx.Done():
		return pubsub.Message{}, fmt.Errorf("broker: wait for a message: %w", ctx.Err())
	}
}

// Len returns the number of messages waiting in the queue.
func (s *Subscription) Len() int {
	return len(s.queue)
}
