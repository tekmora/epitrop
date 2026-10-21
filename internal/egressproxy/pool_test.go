package egressproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// scriptedListener is a listener whose Accept answers what the test says for each call.
type scriptedListener struct {
	deadlineErr error
	accept      func(call int) (net.Conn, error)
	calls       int
	closes      atomic.Int64
}

func (l *scriptedListener) Accept() (net.Conn, error) {
	l.calls++
	return l.accept(l.calls)
}

func (l *scriptedListener) Close() error                { l.closes.Add(1); return nil }
func (l *scriptedListener) Addr() net.Addr              { return &net.UnixAddr{Name: "scripted", Net: "unix"} }
func (l *scriptedListener) SetDeadline(time.Time) error { return l.deadlineErr }

// idleConn is a connection nothing is read from or written to; closing it is all a pool
// does with a connection itself.
type idleConn struct{ net.Conn }

func (idleConn) Close() error                    { return nil }
func (idleConn) SetReadDeadline(time.Time) error { return nil }
func (idleConn) Read([]byte) (int, error)        { return 0, io.EOF }

func TestPoolKeepsAFixedNumberOfFaults(t *testing.T) {
	tests := []struct {
		faults int
		kept   int
	}{
		{0, 0},
		{1, 1},
		{maxFaults - 1, maxFaults - 1},
		{maxFaults, maxFaults},
		{maxFaults + 1, maxFaults},
		{4 * maxFaults, maxFaults},
	}
	for i := 0; i < len(tests); i++ {
		p := newPool()
		p.fault(nil)
		for n := 0; n < tests[i].faults; n++ {
			p.fault(fmt.Errorf("fault %d", n))
			p.fault(nil)
		}
		if len(p.faults) != tests[i].kept {
			t.Errorf("%d faults: %d kept, want %d", tests[i].faults, len(p.faults), tests[i].kept)
		}
		if tests[i].kept > 0 && (p.faults[0].Error() != "fault 0" || p.faults[tests[i].kept-1].Error() != fmt.Sprintf("fault %d", tests[i].kept-1)) {
			t.Errorf("%d faults: kept %v, want the first %d", tests[i].faults, p.faults, tests[i].kept)
		}
	}
}

func TestPoolServeEndsOfAccept(t *testing.T) {
	broken := errors.New("the listener broke")
	tests := []struct {
		name        string
		deadlineErr error
		acceptErr   error
		want        string
		wantCalls   int
	}{
		{"closed listener", nil, net.ErrClosed, "", 1},
		{"wrapped closed listener", nil, &net.OpError{Op: "accept", Net: "unix", Err: net.ErrClosed}, "", 1},
		{"deadline passed", nil, os.ErrDeadlineExceeded, "", 1},
		{"broken listener", nil, broken, "accept: the listener broke", 1},
		{"no deadline to set", errors.New("no deadline"), broken, "accept deadline: no deadline", 0},
	}
	for i := 0; i < len(tests); i++ {
		listener := &scriptedListener{
			deadlineErr: tests[i].deadlineErr,
			accept:      func(int) (net.Conn, error) { return nil, tests[i].acceptErr },
		}
		err := newPool().serve(context.Background(), listener, time.Now().Add(testWait), func(net.Conn) {}, func(net.Conn) {})
		if (err == nil) != (tests[i].want == "") || (err != nil && err.Error() != tests[i].want) {
			t.Errorf("%s: serve() = %v, want %q", tests[i].name, err, tests[i].want)
		}
		if listener.calls != tests[i].wantCalls {
			t.Errorf("%s: %d calls of Accept, want %d", tests[i].name, listener.calls, tests[i].wantCalls)
		}
	}
}

// A context that has ended makes every accept error the end of serving, whatever it says.
func TestPoolServeAfterItsContextEnded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	listener := &scriptedListener{accept: func(int) (net.Conn, error) { return nil, errors.New("the listener broke") }}
	if err := newPool().serve(ctx, listener, time.Now().Add(testWait), func(net.Conn) {}, func(net.Conn) {}); err != nil {
		t.Fatalf("serve() = %v, want nil after the context ended", err)
	}
}

func TestPoolAcceptsAFixedNumberOfConnections(t *testing.T) {
	const limit = 1000
	var handled, refused atomic.Int64
	listener := &scriptedListener{accept: func(int) (net.Conn, error) { return idleConn{}, nil }}
	p := newPool()
	if p.limit != MaxConnections {
		t.Fatalf("a new pool accepts %d connections, want MaxConnections (%d)", p.limit, MaxConnections)
	}
	p.limit = limit
	err := p.serve(context.Background(), listener, time.Now().Add(testWait),
		func(net.Conn) { handled.Add(1) },
		func(net.Conn) { refused.Add(1) })
	want := fmt.Sprintf("accepted %d connections, the most one job may open", limit)
	if err == nil || err.Error() != want {
		t.Fatalf("serve() = %v, want %q", err, want)
	}
	if listener.calls != limit {
		t.Errorf("%d calls of Accept, want exactly %d", listener.calls, limit)
	}
	if got := handled.Load() + refused.Load(); got != limit || handled.Load() == 0 {
		t.Errorf("%d served and %d answered as busy, want %d together and some served", handled.Load(), refused.Load(), limit)
	}
	// A listener left open would take the next connection and never answer it.
	if got := listener.closes.Load(); got != 1 {
		t.Errorf("%d closes of the listener at the bound, want 1", got)
	}
}

// halfEnd is a connection end whose CloseWrite answers what the test says.
type halfEnd struct{ err error }

func (h halfEnd) CloseWrite() error { return h.err }

func TestEndWriteBothWays(t *testing.T) {
	other := errors.New("no space left")
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"ended", nil, nil},
		{"peer gone already", syscall.ENOTCONN, nil},
		{"peer gone already, wrapped", &net.OpError{Op: "shutdown", Net: "tcp", Err: os.NewSyscallError("shutdown", syscall.ENOTCONN)}, nil},
		{"closed by this side", net.ErrClosed, nil},
		{"anything else", other, other},
		{"a reset is not a peer that is gone", syscall.ECONNRESET, syscall.ECONNRESET},
	}
	for i := 0; i < len(tests); i++ {
		if got := endWrite(halfEnd{tests[i].err}); !errors.Is(got, tests[i].want) || (tests[i].want == nil && got != nil) {
			t.Errorf("%s: endWrite() = %v, want %v", tests[i].name, got, tests[i].want)
		}
	}
}

// wholeConn is a connection that cannot end its writing side alone.
type wholeConn struct{ net.Conn }

// loopbackPair is a TCP connection on loopback, both ends.
func loopbackPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	defer func() {
		if closeErr := listener.Close(); closeErr != nil {
			t.Errorf("close the listener = %v, want nil", closeErr)
		}
	}()
	near, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial() = %v, want nil", err)
	}
	far, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept() = %v, want nil", err)
	}
	nearTCP, nearOK := near.(*net.TCPConn)
	farTCP, farOK := far.(*net.TCPConn)
	if !nearOK || !farOK {
		t.Fatalf("connections are %T and %T, want TCP", near, far)
	}
	deadline := time.Now().Add(testWait)
	if err = errors.Join(nearTCP.SetDeadline(deadline), farTCP.SetDeadline(deadline)); err != nil {
		t.Fatalf("SetDeadline() = %v, want nil", err)
	}
	t.Cleanup(func() {
		if closeErr := errors.Join(closed(nearTCP), closed(farTCP)); closeErr != nil {
			t.Errorf("close the pair = %v, want nil", closeErr)
		}
	})
	return nearTCP, farTCP
}

// TestPipeEndsBothDirectionsWhenAnEndCannotHalfClose: the end of one direction cannot be
// passed on alone, so the whole tunnel ends and neither copy waits for its deadline.
func TestPipeEndsBothDirectionsWhenAnEndCannotHalfClose(t *testing.T) {
	clientNear, clientFar := loopbackPair(t)
	upstreamNear, upstreamFar := loopbackPair(t)
	done := make(chan error, 1)
	go func() {
		done <- pipe(context.Background(), wholeConn{clientNear}, clientNear, wholeConn{upstreamNear})
	}()
	if _, err := io.WriteString(clientFar, "ping"); err != nil {
		t.Fatalf("write = %v, want nil", err)
	}
	if err := clientFar.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite() = %v, want nil", err)
	}
	got, err := io.ReadAll(upstreamFar)
	if err != nil || string(got) != "ping" {
		t.Fatalf("the far end read %q, %v, want ping and the end", got, err)
	}
	select {
	case err = <-done:
	case <-time.After(testWait):
		t.Fatal("pipe() still runs, want it to end with the direction that ended")
	}
	if err != nil {
		t.Errorf("pipe() = %v, want nil: an end this side closed is a clean end", err)
	}
	if _, err = upstreamNear.Write([]byte("x")); err == nil || !strings.Contains(err.Error(), "use of closed network connection") {
		t.Errorf("write on the tunnel's end = %v, want it closed", err)
	}
}

// orderRecordConn records the order of CloseWrite, Read, Write, and Close calls.
type orderRecordConn struct {
	mu         sync.Mutex
	calls      []string
	request    *bytes.Buffer
	drainBytes int
	closed     bool
	halfClosed bool
}

func (c *orderRecordConn) CloseWrite() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.halfClosed = true
	c.calls = append(c.calls, "CloseWrite")
	return nil
}

func (c *orderRecordConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "Read")
	n, err := c.request.Read(p)
	c.drainBytes += n
	return n, err
}

func (c *orderRecordConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "Write")
	return len(p), nil
}

func (c *orderRecordConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.calls = append(c.calls, "Close")
	return nil
}

func (c *orderRecordConn) SetDeadline(time.Time) error      { return nil }
func (c *orderRecordConn) SetReadDeadline(time.Time) error  { return nil }
func (c *orderRecordConn) SetWriteDeadline(time.Time) error { return nil }
func (c *orderRecordConn) LocalAddr() net.Addr              { return &net.UnixAddr{Name: "order", Net: "unix"} }
func (c *orderRecordConn) RemoteAddr() net.Addr             { return &net.UnixAddr{Name: "order", Net: "unix"} }

func checkOrder(calls []string, first, second string) bool {
	f := slices.Index(calls, first)
	s := slices.Index(calls, second)
	return f >= 0 && s >= 0 && f < s
}

// TestPoolRefusedConnDrainsBeforeClose verifies call order on refused connections:
// request bytes are drained before Close, and CloseWrite is called when available.
func TestPoolRefusedConnDrainsBeforeClose(t *testing.T) {
	p := newPool()
	for i := 0; i < maxTunnels; i++ {
		p.slots <- struct{}{}
	}
	req := "CONNECT target.example:443 HTTP/1.1\r\nHost: target.example:443\r\n\r\n"
	conn := &orderRecordConn{request: bytes.NewBufferString(req)}
	var serving sync.WaitGroup
	p.serveOne(conn, &serving, func(net.Conn) {}, func(c net.Conn) {
		if _, err := io.WriteString(c, "HTTP/1.1 503 Service Unavailable\r\n\r\n"); err != nil {
			t.Errorf("write response: %v", err)
		}
	})
	serving.Wait()

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if !conn.halfClosed {
		t.Fatalf("CloseWrite was not called on connection that supports it")
	}
	if !conn.closed {
		t.Fatalf("Close was not called")
	}
	if conn.drainBytes != len(req) {
		t.Fatalf("drained %d bytes, want %d", conn.drainBytes, len(req))
	}
	if !checkOrder(conn.calls, "Read", "Close") {
		t.Errorf("calls = %v: want Read before Close", conn.calls)
	}
	if !checkOrder(conn.calls, "CloseWrite", "Close") {
		t.Errorf("calls = %v: want CloseWrite before Close", conn.calls)
	}
}

// streamConn produces bytes indefinitely until closed.
type streamConn struct {
	mu     sync.Mutex
	read   int
	closed bool
}

func (c *streamConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	c.read += len(p)
	return len(p), nil
}

func (c *streamConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *streamConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }
func (c *streamConn) LocalAddr() net.Addr              { return &net.UnixAddr{Name: "stream", Net: "unix"} }
func (c *streamConn) RemoteAddr() net.Addr             { return &net.UnixAddr{Name: "stream", Net: "unix"} }

// TestPoolRefusedConnBoundaryMaxRequestBytes verifies that a client sending more than
// maxRequestBytes is capped: the drain stops at maxRequestBytes and close still happens.
func TestPoolRefusedConnBoundaryMaxRequestBytes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	p := newPool()
	for i := 0; i < maxTunnels; i++ {
		p.slots <- struct{}{}
	}
	conn := &streamConn{}
	var serving sync.WaitGroup
	p.serveOne(conn, &serving, func(net.Conn) {}, func(net.Conn) {})

	done := make(chan struct{})
	go func() {
		serving.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("drain hung reading past maxRequestBytes")
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if !conn.closed {
		t.Errorf("Close() was not called")
	}
	if conn.read != maxRequestBytes {
		t.Errorf("read %d bytes, want exactly maxRequestBytes (%d)", conn.read, maxRequestBytes)
	}
}

// silentConn blocks reads until its read deadline or until closed.
type silentConn struct {
	deadline atomic.Pointer[time.Time]
	closed   chan struct{}
}

func newSilentConn() *silentConn {
	return &silentConn{closed: make(chan struct{})}
}

func (c *silentConn) SetReadDeadline(t time.Time) error {
	c.deadline.Store(&t)
	return nil
}

func (c *silentConn) Read([]byte) (int, error) {
	dl := c.deadline.Load()
	if dl == nil || dl.IsZero() {
		<-c.closed
		return 0, net.ErrClosed
	}
	wait := time.Until(*dl)
	if wait <= 0 {
		return 0, os.ErrDeadlineExceeded
	}
	select {
	case <-time.After(wait):
		return 0, os.ErrDeadlineExceeded
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func (c *silentConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func (c *silentConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *silentConn) SetDeadline(time.Time) error      { return nil }
func (c *silentConn) SetWriteDeadline(time.Time) error { return nil }
func (c *silentConn) LocalAddr() net.Addr              { return &net.UnixAddr{Name: "silent", Net: "unix"} }
func (c *silentConn) RemoteAddr() net.Addr             { return &net.UnixAddr{Name: "silent", Net: "unix"} }

// TestPoolRefusedConnNegativeNeverSends verifies that a client sending nothing and never
// closing is bounded by the drain deadline before closing.
func TestPoolRefusedConnNegativeNeverSends(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	p := newPool()
	for i := 0; i < maxTunnels; i++ {
		p.slots <- struct{}{}
	}
	conn := newSilentConn()
	start := time.Now()
	var serving sync.WaitGroup
	p.serveOne(conn, &serving, func(net.Conn) {}, func(net.Conn) {})

	done := make(chan struct{})
	go func() {
		serving.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("drain hung on silent client")
	}

	elapsed := time.Since(start)
	if elapsed < drainDeadline {
		t.Errorf("closed after %s, want at least drainDeadline (%s)", elapsed, drainDeadline)
	}
	if elapsed > drainDeadline+2*time.Second {
		t.Errorf("closed after %s, want bounded close near drainDeadline (%s)", elapsed, drainDeadline)
	}
	select {
	case <-conn.closed:
	default:
		t.Errorf("Close() was not called on the connection")
	}
}

// TestPoolRefusedConnBoundaryRefusingFull verifies that when the refusing semaphore is
// at capacity, serveOne answers synchronously: busy is called, Close is called before
// serveOne returns, and no drain read is attempted.
func TestPoolRefusedConnBoundaryRefusingFull(t *testing.T) {
	p := newPool()
	for i := 0; i < maxTunnels; i++ {
		p.slots <- struct{}{}
	}
	for i := 0; i < maxRefusing; i++ {
		p.refusing <- struct{}{}
	}
	req := "CONNECT target.example:443 HTTP/1.1\r\nHost: target.example:443\r\n\r\n"
	conn := &orderRecordConn{request: bytes.NewBufferString(req)}
	var busyCalled bool
	var serving sync.WaitGroup
	p.serveOne(conn, &serving, func(net.Conn) {}, func(c net.Conn) {
		busyCalled = true
	})

	conn.mu.Lock()
	closedImmediately := conn.closed
	conn.mu.Unlock()

	serving.Wait()

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if !busyCalled {
		t.Errorf("busy was not called synchronously")
	}
	if !closedImmediately {
		t.Errorf("Close was not called synchronously before serveOne returned")
	}
	if slices.Contains(conn.calls, "Read") {
		t.Errorf("Read was called (%d bytes drained), want Read never called", conn.drainBytes)
	}
}
