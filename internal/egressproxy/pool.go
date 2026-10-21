package egressproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const (
	// MaxConnections is how many connections one listener accepts over its life: more than
	// one a second for the longest runtime a job may have. Past it the listener is closed,
	// so a further connection is refused at once and does not wait for an answer.
	MaxConnections = 1 << 25
	// maxTunnels is how many connections are served at a time. One more is answered as
	// busy.
	maxTunnels = 64
	// maxRefusing is how many refused connections may drain at a time. Matching maxTunnels
	// caps the goroutines for refused traffic at the same scale as served traffic, so a
	// connection flood cannot spawn unbounded goroutines.
	maxRefusing = maxTunnels
	// maxFaults is how many errors that belong to no request are kept for the caller.
	maxFaults = 8
	// drainDeadline bounds how long a refused connection waits to discard the client's
	// request before closing, so closing a socket with unread data sends no reset.
	drainDeadline = 1 * time.Second
)

// deadlineListener is a listener whose Accept can be given a deadline, as the TCP and Unix
// listeners can.
type deadlineListener interface {
	net.Listener
	SetDeadline(time.Time) error
}

// pool serves the connections of one listener: each in its own goroutine, at most
// maxTunnels at a time. It keeps the errors that belong to no single connection, such as
// a close that failed, for serve to return.
type pool struct {
	slots    chan struct{}
	refusing chan struct{}
	limit    int

	mu     sync.Mutex
	faults []error
}

func newPool() *pool {
	return &pool{
		slots:    make(chan struct{}, maxTunnels),
		refusing: make(chan struct{}, maxRefusing),
		limit:    MaxConnections,
	}
}

// serve accepts until ctx ends, the listener is closed or deadline has passed, and returns
// when the last connection has been served. Those three ends are no error. handle serves a
// connection that got a slot; busy answers one that found none. serve closes each
// connection after its function has returned.
func (p *pool) serve(ctx context.Context, listener deadlineListener, deadline time.Time, handle func(net.Conn), busy func(net.Conn)) error {
	if err := listener.SetDeadline(deadline); err != nil {
		return fmt.Errorf("accept deadline: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { p.fault(closed(listener)) })
	defer stop()
	var serving sync.WaitGroup
	err := p.accept(ctx, listener, &serving, handle, busy)
	serving.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return errors.Join(err, errors.Join(p.faults...))
}

func (p *pool) accept(ctx context.Context, listener net.Listener, serving *sync.WaitGroup, handle func(net.Conn), busy func(net.Conn)) error {
	for i := 0; i < p.limit; i++ {
		conn, err := listener.Accept()
		if err != nil {
			if ended(ctx) || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrDeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		p.serveOne(conn, serving, handle, busy)
	}
	return errors.Join(fmt.Errorf("accepted %d connections, the most one job may open", p.limit), closed(listener))
}

// ended reports whether ctx is done.
func ended(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// serveOne gives the connection a slot and a goroutine, or answers it as busy.
func (p *pool) serveOne(conn net.Conn, serving *sync.WaitGroup, handle func(net.Conn), busy func(net.Conn)) {
	select {
	case p.slots <- struct{}{}:
	default:
		select {
		case p.refusing <- struct{}{}:
			serving.Add(1)
			go func() {
				defer serving.Done()
				defer func() { <-p.refusing }()
				busy(conn)
				p.closeRefused(conn)
			}()
		default:
			busy(conn)
			p.fault(closed(conn))
		}
		return
	}
	serving.Add(1)
	go func() {
		defer serving.Done()
		defer func() { <-p.slots }()
		defer func() { p.fault(closed(conn)) }()
		handle(conn)
	}()
}

// closeRefused ends a refused connection cleanly: after the answer is written, it
// half-closes the write side if the connection supports it, reads and discards what the
// client sent under a read deadline so closing the socket sends no reset, and closes it.
func (p *pool) closeRefused(conn net.Conn) {
	var err error
	if half, ok := conn.(halfCloser); ok {
		err = errors.Join(err, endWrite(half))
	}
	if dlErr := conn.SetReadDeadline(time.Now().Add(drainDeadline)); dlErr != nil {
		err = errors.Join(err, ignoreClosed(dlErr))
	}
	_, drainErr := io.Copy(io.Discard, io.LimitReader(conn, maxRequestBytes))
	if drainErr != nil && !errors.Is(drainErr, os.ErrDeadlineExceeded) && !peerEnded(drainErr) {
		err = errors.Join(err, ignoreClosed(drainErr))
	}
	p.fault(errors.Join(err, closed(conn)))
}

// fault keeps an error that belongs to no single connection. nil is none; past maxFaults
// the further ones are dropped, the first ones say what went wrong.
func (p *pool) fault(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.faults) < maxFaults {
		p.faults = append(p.faults, err)
	}
}
