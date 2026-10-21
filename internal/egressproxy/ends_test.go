package egressproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countedConn counts how often it is closed, in a counter its test owns.
type countedConn struct {
	net.Conn
	closes *atomic.Int64
}

func (c countedConn) Close() error {
	c.closes.Add(1)
	return nil
}

func (countedConn) SetDeadline(time.Time) error     { return nil }
func (countedConn) SetReadDeadline(time.Time) error { return nil }
func (countedConn) Read([]byte) (int, error)        { return 0, io.EOF }

// TestPoolClosesEveryConnectionOnce: a served connection and one answered as busy are both
// closed by the pool, each exactly once.
func TestPoolClosesEveryConnectionOnce(t *testing.T) {
	tests := []struct {
		name        string
		connections int
	}{
		{"one", 1},
		{"as many as are served at a time", maxTunnels},
		{"one more than are served at a time", maxTunnels + 1},
		{"twice as many", 2 * maxTunnels},
	}
	for i := 0; i < len(tests); i++ {
		var closes, served, busy atomic.Int64
		release := make(chan struct{})
		listener := &scriptedListener{accept: func(call int) (net.Conn, error) {
			if call > tests[i].connections {
				close(release)
				return nil, net.ErrClosed
			}
			return countedConn{closes: &closes}, nil
		}}
		// Every served connection holds its slot until the last one was accepted, so the
		// ones past maxTunnels find none.
		err := newPool().serve(context.Background(), listener, time.Now().Add(testWait),
			func(net.Conn) { served.Add(1); <-release },
			func(net.Conn) { busy.Add(1) })
		if err != nil {
			t.Errorf("%s: serve() = %v, want nil", tests[i].name, err)
		}
		wantBusy := int64(max(tests[i].connections-maxTunnels, 0))
		if served.Load() != int64(tests[i].connections)-wantBusy || busy.Load() != wantBusy {
			t.Errorf("%s: %d served and %d answered as busy, want %d and %d", tests[i].name,
				served.Load(), busy.Load(), int64(tests[i].connections)-wantBusy, wantBusy)
		}
		if closes.Load() != int64(tests[i].connections) {
			t.Errorf("%s: %d closes for %d connections, want one each", tests[i].name, closes.Load(), tests[i].connections)
		}
	}
}

// endConn is a connection end that records how it was ended.
type endConn struct {
	net.Conn
	sink       strings.Builder
	closes     int
	halfCloses int
}

func (c *endConn) Write(p []byte) (int, error) { return c.sink.Write(p) }

func (c *endConn) Close() error {
	c.closes++
	return nil
}

// halfEndConn is an endConn that can end its writing side alone.
type halfEndConn struct{ endConn }

func (c *halfEndConn) CloseWrite() error {
	c.halfCloses++
	return nil
}

// failingReader gives its text and then err instead of the end.
type failingReader struct {
	text io.Reader
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	n, err := r.text.Read(p)
	if errors.Is(err, io.EOF) {
		return n, r.err
	}
	return n, err
}

// TestCopyHalfEndsOfOneDirection: only a clean end towards a connection that can end one
// side alone leaves the tunnel open for the other direction. Every other end closes both
// connections, and an error is returned, not lost.
func TestCopyHalfEndsOfOneDirection(t *testing.T) {
	broken := errors.New("the read broke")
	tests := []struct {
		name          string
		canHalfClose  bool
		readErr       error
		wantErr       error
		wantHalfClose int
		wantCloses    int
	}{
		{"clean end, one side can end alone", true, nil, nil, 1, 0},
		{"error, one side can end alone", true, broken, broken, 0, 1},
		{"clean end, only the whole connection ends", false, nil, nil, 0, 1},
		{"error, only the whole connection ends", false, broken, broken, 0, 1},
		{"closed by this side, one side can end alone", true, net.ErrClosed, nil, 0, 1},
	}
	for i := 0; i < len(tests); i++ {
		runCopyHalfEndsCase(t, tests[i])
	}
}

func runCopyHalfEndsCase(t *testing.T, tc struct {
	name          string
	canHalfClose  bool
	readErr       error
	wantErr       error
	wantHalfClose int
	wantCloses    int
}) {
	t.Helper()
	src := &endConn{}
	half := &halfEndConn{}
	whole := &endConn{}
	var dst net.Conn = whole
	got := whole
	if tc.canHalfClose {
		dst, got = half, &half.endConn
	}
	var reader io.Reader = strings.NewReader("ping")
	if tc.readErr != nil {
		reader = &failingReader{text: reader, err: tc.readErr}
	}
	err := copyHalf(dst, reader, src)
	if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
		t.Errorf("%s: copyHalf() = %v, want %v", tc.name, err, tc.wantErr)
	}
	if got.sink.String() != "ping" {
		t.Errorf("%s: copied %q, want ping", tc.name, got.sink.String())
	}
	if half.halfCloses != tc.wantHalfClose {
		t.Errorf("%s: %d ends of the writing side, want %d", tc.name, half.halfCloses, tc.wantHalfClose)
	}
	if got.closes != tc.wantCloses || src.closes != tc.wantCloses {
		t.Errorf("%s: %d and %d closes of the two connections, want %d each", tc.name, got.closes, src.closes, tc.wantCloses)
	}
}

// waitingListener accepts nothing: its Accept returns when the listener is closed.
type waitingListener struct {
	scriptedListener
	end    chan struct{}
	closes atomic.Int64
}

func (l *waitingListener) Accept() (net.Conn, error) {
	select {
	case <-l.end:
		return nil, net.ErrClosed
	case <-time.After(2 * testWait):
		return nil, os.ErrDeadlineExceeded
	}
}

func (l *waitingListener) Close() error {
	if l.closes.Add(1) == 1 {
		close(l.end)
	}
	return nil
}

// TestPoolClosesItsListenerWhenItsContextEnds: the end of the context ends an accept that
// waits, by closing the listener once, and serving does not wait for its deadline.
func TestPoolClosesItsListenerWhenItsContextEnds(t *testing.T) {
	listener := &waitingListener{end: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- newPool().serve(ctx, listener, time.Now().Add(2*testWait), func(net.Conn) {}, func(net.Conn) {})
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve() = %v, want nil after the context ended", err)
		}
	case <-time.After(testWait):
		t.Errorf("serve() still runs %s after its context ended", testWait)
	}
	if got := listener.closes.Load(); got != 1 {
		t.Errorf("%d closes of the listener, want 1", got)
	}
	if err := listener.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}

// TestPipeEndsWhenItsContextEnds: the end of the context closes both connections, so both
// directions end long before their deadline, and that end is a clean one.
func TestPipeEndsWhenItsContextEnds(t *testing.T) {
	clientNear, _ := loopbackPair(t)
	upstreamNear, _ := loopbackPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pipe(ctx, clientNear, clientNear, upstreamNear) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("pipe() = %v, want nil: an end this side closed is a clean end", err)
		}
	case <-time.After(testWait / 2):
		t.Fatalf("pipe() still runs %s after its context ended", testWait/2)
	}
	ends := []*net.TCPConn{clientNear, upstreamNear}
	for i := 0; i < len(ends); i++ {
		if _, err := ends[i].Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
			t.Errorf("write on end %d of the tunnel = %v, want it closed", i, err)
		}
	}
}

// TestRelayOneSetsTheTunnelDeadline: a tunnel of the relay carries the deadline of the
// relay, and a connection that cannot be given one gets no tunnel.
func TestRelayOneSetsTheTunnelDeadline(t *testing.T) {
	// One windows/amd64 run of about 36 timed out here (main d593720); about 1,000 probe trials
	// per Windows architecture, half-close and relayOne alike, lost nothing.
	if runtime.GOOS == "windows" {
		skipOnWindowsUntil(t, windowsRelaySkipExpires, time.Now(),
			"exception category D (120 days from 2026-10-16): the relay runs inside the sandbox, which does not run on Windows")
	}
	tests := []struct {
		name        string
		deadlineErr error
		want        string
	}{
		{"deadline set", nil, ""},
		{"no deadline to set", errors.New("no deadline"), "tunnel deadline: no deadline"},
	}
	for i := 0; i < len(tests); i++ {
		server := startServer(t, Options{Destinations: []string{"gateway.example:443"}})
		deadline := time.Now().Add(testWait)
		conn := &scriptedConn{in: strings.NewReader(""), deadlineErrs: []error{tests[i].deadlineErr}}
		err := relayOne(t.Context(), conn, server.SocketPath(), deadline)
		if (err == nil) != (tests[i].want == "") || (err != nil && err.Error() != tests[i].want) {
			t.Errorf("%s: relayOne() = %v, want %q", tests[i].name, err, tests[i].want)
		}
		if len(conn.set) != 1 || !conn.set[0].Equal(deadline) {
			t.Errorf("%s: deadlines set %v, want one, %s", tests[i].name, conn.set, deadline)
		}
	}
}

// TestServerHandleDeadlines: a request has handshakeTimeout to arrive, and the tunnel it
// opens ends when the server does.
func TestServerHandleDeadlines(t *testing.T) {
	target := upstream(t)
	server := startServer(t, Options{Destinations: []string{target}})
	conn := &scriptedConn{in: strings.NewReader(connectRequest(target))}
	tunnelEnd := time.Now().Add(time.Hour)
	before := time.Now()
	server.handle(t.Context(), conn, tunnelEnd)
	after := time.Now()
	if !strings.HasPrefix(conn.out.String(), "HTTP/1.1 200 ") || len(conn.set) != 2 {
		t.Fatalf("answer %q with deadlines %v, want a tunnel and two deadlines", conn.out.String(), conn.set)
	}
	if conn.set[0].Before(before.Add(handshakeTimeout)) || conn.set[0].After(after.Add(handshakeTimeout)) {
		t.Errorf("request deadline %s, want %s after the request came (%s..%s)", conn.set[0], handshakeTimeout, before, after)
	}
	if !conn.set[1].Equal(tunnelEnd) {
		t.Errorf("tunnel deadline %s, want %s", conn.set[1], tunnelEnd)
	}
}

// TestSocketPathBoundary: a path of maxSocketPath bytes is bound, one byte more is refused.
func TestSocketPathBoundary(t *testing.T) {
	atLimit := "/" + strings.Repeat("d", maxSocketPath-len(socketName)-2)
	path, err := socketPath(atLimit)
	if err != nil || len(path) != maxSocketPath {
		t.Errorf("socketPath(%d bytes) = %d bytes, %v, want %d and nil", len(atLimit), len(path), err, maxSocketPath)
	}
	path, err = socketPath(atLimit + "d")
	if err == nil || path != "" || !strings.Contains(err.Error(), "is longer than 100 bytes") {
		t.Errorf("socketPath(%d bytes) = %q, %v, want it refused", len(atLimit)+1, path, err)
	}
}

// TestServerClosesAnUpstreamItOpensNoTunnelTo: a connection to the destination that is not
// handed to a tunnel is closed by the server, once.
func TestServerClosesAnUpstreamItOpensNoTunnelTo(t *testing.T) {
	var closes atomic.Int64
	server := startServer(t, Options{
		Destinations: []string{"192.0.2.7:443"},
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return countedConn{closes: &closes}, nil
		},
	})
	conn := &scriptedConn{
		in:           strings.NewReader(connectRequest("192.0.2.7:443")),
		deadlineErrs: []error{nil, errors.New("no deadline")},
	}
	server.handle(t.Context(), conn, time.Now().Add(testWait))
	if !strings.HasPrefix(conn.out.String(), "HTTP/1.1 500 ") {
		t.Errorf("answer %q, want 500", conn.out.String())
	}
	if closes.Load() != 1 {
		t.Errorf("%d closes of the upstream connection, want 1", closes.Load())
	}
}

// TestTunnelDeadlineBothWays: the deadline of the context is the deadline of the tunnels;
// without one they end after maxTunnelLife.
func TestTunnelDeadlineBothWays(t *testing.T) {
	want := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), want)
	defer cancel()
	if got := tunnelDeadline(ctx); !got.Equal(want) {
		t.Errorf("tunnelDeadline(context with a deadline) = %s, want %s", got, want)
	}
	before := time.Now()
	got := tunnelDeadline(context.Background())
	if got.Before(before.Add(maxTunnelLife)) || got.After(time.Now().Add(maxTunnelLife)) {
		t.Errorf("tunnelDeadline(context without one) = %s, want %s after now", got, maxTunnelLife)
	}
}

// TestServerLogsATunnelThatEndsWithAnError: a tunnel that did not end cleanly is one line in
// the log and no refusal.
func TestServerLogsATunnelThatEndsWithAnError(t *testing.T) {
	target := upstream(t)
	server := startServer(t, Options{Destinations: []string{target}})
	// The answer to the request is written; what the destination sends then is not.
	conn := &scriptedConn{in: strings.NewReader(connectRequest(target)), writeErr: errors.New("the client is gone"), goodWrites: 1}
	server.handle(t.Context(), conn, time.Now().Add(testWait))
	if got := conn.out.String(); got != "HTTP/1.1 200 Connection established\r\n\r\n" {
		t.Errorf("answer %q, want 200 and nothing after it", got)
	}
	log := server.log.String()
	if !strings.HasPrefix(log, "egress proxy: tunnel to "+target+" ended: ") || !strings.HasSuffix(log, "the client is gone\n") || strings.Count(log, "\n") != 1 {
		t.Errorf("log %q, want one line that names the tunnel and what ended it", log)
	}
	if server.Refused() != 0 {
		t.Errorf("Refused() = %d, want 0", server.Refused())
	}
}
