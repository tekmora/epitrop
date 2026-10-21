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
	"testing"
	"time"
)

// scriptedConn is a client connection that sends one request and fails where the test says.
type scriptedConn struct {
	net.Conn
	in  *strings.Reader
	out bytes.Buffer
	// deadlineErrs are the answers of SetDeadline, call by call; past its end the answer is nil.
	deadlineErrs []error
	// set are the deadlines it was given, in order.
	set              []time.Time
	writeDeadlineErr error
	// writeErr is the answer of every Write after the first goodWrites.
	writeErr   error
	goodWrites int
	writes     int
}

func (c *scriptedConn) Read(p []byte) (int, error) { return c.in.Read(p) }

func (c *scriptedConn) Write(p []byte) (int, error) {
	c.writes++
	if c.writeErr != nil && c.writes > c.goodWrites {
		return 0, c.writeErr
	}
	return c.out.Write(p)
}

func (c *scriptedConn) Close() error { return nil }

func (c *scriptedConn) SetDeadline(deadline time.Time) error {
	c.set = append(c.set, deadline)
	if len(c.set) <= len(c.deadlineErrs) {
		return c.deadlineErrs[len(c.set)-1]
	}
	return nil
}

func (c *scriptedConn) SetWriteDeadline(time.Time) error { return c.writeDeadlineErr }

func (c *scriptedConn) SetReadDeadline(time.Time) error { return nil }

// TestServerHandleFaultsOfTheClientConnection: what the server does when the connection of
// the client itself fails, at each place it can.
func TestServerHandleFaultsOfTheClientConnection(t *testing.T) {
	target := upstream(t)
	gone := errors.New("the client is gone")
	noDeadline := errors.New("no deadline")
	tests := []struct {
		name        string
		conn        scriptedConn
		target      string
		wantAnswer  string
		wantLog     string
		wantRefused int
		wantFault   string
	}{
		{"refusal that cannot be written", scriptedConn{writeErr: gone}, "other.example:443", "",
			`egress proxy: refused "other.example:443": other.example:443 is not a declared destination (the answer did not reach the client: the client is gone)` + "\n", 1, ""},
		{"refusal without a write deadline", scriptedConn{writeDeadlineErr: noDeadline}, "other.example:443", "",
			`egress proxy: refused "other.example:443": other.example:443 is not a declared destination (the answer did not reach the client: no deadline)` + "\n", 1, ""},
		{"no request deadline", scriptedConn{deadlineErrs: []error{noDeadline}}, target, "", "", 0, "egress proxy: request deadline: no deadline"},
		{"no tunnel deadline", scriptedConn{deadlineErrs: []error{nil, noDeadline}}, target, "HTTP/1.1 500 Internal Server Error\r\n",
			`egress proxy: refused "` + target + `": no deadline` + "\n", 1, ""},
		{"answer that cannot be written", scriptedConn{writeErr: gone}, target, "",
			"egress proxy: tunnel to " + target + ": answer the request: the client is gone\n", 0, ""},
	}
	for i := 0; i < len(tests); i++ {
		server := startServer(t, Options{Destinations: []string{target}})
		conn := &tests[i].conn
		conn.in = strings.NewReader(connectRequest(tests[i].target))
		server.handle(t.Context(), conn, time.Now().Add(testWait))
		if got := conn.out.String(); !strings.HasPrefix(got, tests[i].wantAnswer) || (tests[i].wantAnswer == "" && got != "") {
			t.Errorf("%s: answer %q, want %q", tests[i].name, got, tests[i].wantAnswer)
		}
		if got := server.log.String(); got != tests[i].wantLog {
			t.Errorf("%s: log %q, want %q", tests[i].name, got, tests[i].wantLog)
		}
		if got := server.Refused(); got != tests[i].wantRefused {
			t.Errorf("%s: Refused() = %d, want %d", tests[i].name, got, tests[i].wantRefused)
		}
		err := server.stop()
		if (err == nil) != (tests[i].wantFault == "") || (err != nil && err.Error() != tests[i].wantFault) {
			t.Errorf("%s: the server stopped with %v, want %q", tests[i].name, err, tests[i].wantFault)
		}
	}
}

// A request that ends inside its header lines is refused, not waited for.
func TestServerRefusesARequestThatEndsInItsHeader(t *testing.T) {
	server := startServer(t, Options{Destinations: []string{"gateway.example:443"}})
	tun := dialServer(t, server.Server)
	if _, err := io.WriteString(tun.conn, "CONNECT gateway.example:443 HTTP/1.1\r\nHost: gateway.example:443\r\n"); err != nil {
		t.Fatalf("write request = %v, want nil", err)
	}
	if got := finish(t, tun); !strings.HasPrefix(got, "HTTP/1.1 400 Bad Request\r\n") {
		t.Fatalf("answer = %q, want 400", got)
	}
	want := `egress proxy: refused "gateway.example:443": read the request: EOF` + "\n"
	if !waitFor(func() bool { return server.log.String() == want }) {
		t.Fatalf("log = %q, want %q", server.log.String(), want)
	}
	if server.Refused() != 1 {
		t.Errorf("Refused() = %d, want 1", server.Refused())
	}
}

// drainRecordConn records calls to Read, Write, CloseWrite, and SetReadDeadline.
type drainRecordConn struct {
	mu           sync.Mutex
	calls        []string
	request      *bytes.Buffer
	drainBytes   int
	halfClosed   bool
	readDeadline time.Time
	out          bytes.Buffer
}

func (c *drainRecordConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "Read")
	n, err := c.request.Read(p)
	c.drainBytes += n
	return n, err
}

func (c *drainRecordConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "Write")
	return c.out.Write(p)
}

func (c *drainRecordConn) CloseWrite() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.halfClosed = true
	c.calls = append(c.calls, "CloseWrite")
	return nil
}

func (c *drainRecordConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "Close")
	return nil
}

func (c *drainRecordConn) SetDeadline(time.Time) error      { return nil }
func (c *drainRecordConn) SetWriteDeadline(time.Time) error { return nil }
func (c *drainRecordConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	c.calls = append(c.calls, "SetReadDeadline")
	return nil
}

func (c *drainRecordConn) LocalAddr() net.Addr  { return &net.UnixAddr{Name: "record", Net: "unix"} }
func (c *drainRecordConn) RemoteAddr() net.Addr { return &net.UnixAddr{Name: "record", Net: "unix"} }

// TestServerHandleRefusalDrainsLongRequestLine verifies that when a request line exceeds
// maxRequestBytes followed by more bytes, the server answers 400 and drains the remaining
// request bytes under a read deadline before handle returns.
func TestServerHandleRefusalDrainsLongRequestLine(t *testing.T) {
	server := startServer(t, Options{Destinations: []string{"declared.example:443"}})
	req := strings.Repeat("A", maxRequestBytes+100) + "\r\n" + "extra bytes after request line"
	conn := &drainRecordConn{request: bytes.NewBufferString(req)}
	server.handle(t.Context(), conn, time.Now().Add(testWait))

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if !strings.HasPrefix(conn.out.String(), "HTTP/1.1 400 Bad Request\r\n") {
		t.Errorf("answer = %q, want 400 Bad Request", conn.out.String())
	}
	if !conn.halfClosed {
		t.Errorf("CloseWrite was not called on connection that supports it")
	}
	if conn.drainBytes != len(req) {
		t.Errorf("drained %d bytes, want %d", conn.drainBytes, len(req))
	}
	closeWriteIdx := slices.Index(conn.calls, "CloseWrite")
	if closeWriteIdx < 0 || !slices.Contains(conn.calls[closeWriteIdx:], "Read") {
		t.Errorf("calls = %v: want CloseWrite before drain Read", conn.calls)
	}
	if !slices.Contains(conn.calls, "SetReadDeadline") {
		t.Errorf("calls = %v: want SetReadDeadline called", conn.calls)
	}
}

// TestServerRefusalClientSeesCleanEOF verifies that a client sending an oversized request
// followed by more data sees a clean EOF rather than a reset after closing its write side.
func TestServerRefusalClientSeesCleanEOF(t *testing.T) {
	server := startServer(t, Options{Destinations: []string{"declared.example:443"}})
	tun := dialServer(t, server.Server)
	req := strings.Repeat("A", maxRequestBytes+100) + "\r\n" + "extra bytes after request line"
	if _, err := io.WriteString(tun.conn, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := tun.conn.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	resp, err := io.ReadAll(tun.conn)
	if err != nil {
		t.Fatalf("ReadAll: %v, want clean EOF", err)
	}
	if !strings.HasPrefix(string(resp), "HTTP/1.1 400 Bad Request\r\n") {
		t.Fatalf("response = %q, want 400 Bad Request", string(resp))
	}
}

// headerCapStreamConn produces a 431 header-exceeding request and then streams bytes
// indefinitely until closed.
type headerCapStreamConn struct {
	mu           sync.Mutex
	prefix       *bytes.Buffer
	drainRead    int
	closed       bool
	readDeadline atomic.Pointer[time.Time]
	out          bytes.Buffer
}

func newHeaderCapStreamConn() *headerCapStreamConn {
	var buf bytes.Buffer
	buf.WriteString("CONNECT declared.example:443 HTTP/1.1\r\n")
	for i := 0; i <= maxHeaderLines; i++ {
		fmt.Fprintf(&buf, "X-Header-%02d: %s\r\n", i, strings.Repeat("v", 100))
	}
	return &headerCapStreamConn{prefix: &buf}
}

func (c *headerCapStreamConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	if c.prefix.Len() > 0 {
		return c.prefix.Read(p)
	}
	c.drainRead += len(p)
	return len(p), nil
}

func (c *headerCapStreamConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.Write(p)
}

func (c *headerCapStreamConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *headerCapStreamConn) CloseWrite() error                { return nil }
func (c *headerCapStreamConn) SetDeadline(time.Time) error      { return nil }
func (c *headerCapStreamConn) SetWriteDeadline(time.Time) error { return nil }
func (c *headerCapStreamConn) SetReadDeadline(t time.Time) error {
	c.readDeadline.Store(&t)
	return nil
}
func (c *headerCapStreamConn) LocalAddr() net.Addr {
	return &net.UnixAddr{Name: "headercap", Net: "unix"}
}
func (c *headerCapStreamConn) RemoteAddr() net.Addr {
	return &net.UnixAddr{Name: "headercap", Net: "unix"}
}

// TestServerHandleRefusalBoundaryHeaderCapExceeded verifies that for a 431 refusal with
// continuous bytes after the header cap, the drain is capped at maxRequestBytes and the
// connection closes within drainDeadline + margin without hanging.
func TestServerHandleRefusalBoundaryHeaderCapExceeded(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	server := startServer(t, Options{Destinations: []string{"declared.example:443"}})
	conn := newHeaderCapStreamConn()
	start := time.Now()

	var serving sync.WaitGroup
	server.pool.serveOne(conn, &serving,
		func(c net.Conn) { server.handle(ctx, c, time.Now().Add(testWait)) },
		func(net.Conn) {})

	done := make(chan struct{})
	go func() {
		serving.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("handle refusal drain hung past 5 s test timer")
	}

	elapsed := time.Since(start)
	if elapsed > drainDeadline+2*time.Second {
		t.Errorf("connection closed after %s, want within drainDeadline + margin (%s)", elapsed, drainDeadline)
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if !conn.closed {
		t.Errorf("Close() was not called on the connection")
	}
	if !strings.HasPrefix(conn.out.String(), "HTTP/1.1 431 Request Header Fields Too Large\r\n") {
		t.Errorf("answer = %q, want 431 Request Header Fields Too Large", conn.out.String())
	}
	if conn.drainRead > maxRequestBytes {
		t.Errorf("drained %d bytes past headers, want at most maxRequestBytes (%d)", conn.drainRead, maxRequestBytes)
	}
}

// silentAfterConn sends an initial request and then blocks reads until its read deadline
// or until closed, never sending EOF.
type silentAfterConn struct {
	mu       sync.Mutex
	request  *bytes.Buffer
	deadline atomic.Pointer[time.Time]
	closed   chan struct{}
	out      bytes.Buffer
}

func newSilentAfterConn(request string) *silentAfterConn {
	return &silentAfterConn{
		request: bytes.NewBufferString(request),
		closed:  make(chan struct{}),
	}
}

func (c *silentAfterConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if c.request.Len() > 0 {
		n, err := c.request.Read(p)
		c.mu.Unlock()
		return n, err
	}
	c.mu.Unlock()

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

func (c *silentAfterConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.Write(p)
}

func (c *silentAfterConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func (c *silentAfterConn) CloseWrite() error                { return nil }
func (c *silentAfterConn) SetDeadline(time.Time) error      { return nil }
func (c *silentAfterConn) SetWriteDeadline(time.Time) error { return nil }
func (c *silentAfterConn) SetReadDeadline(t time.Time) error {
	c.deadline.Store(&t)
	return nil
}
func (c *silentAfterConn) LocalAddr() net.Addr  { return &net.UnixAddr{Name: "silent", Net: "unix"} }
func (c *silentAfterConn) RemoteAddr() net.Addr { return &net.UnixAddr{Name: "silent", Net: "unix"} }

// TestServerHandleRefusalNegativeSilentClientAfter403 verifies that when a client is refused
// with 403 and never closes, handle waits for drainDeadline and returns after it, not later.
func TestServerHandleRefusalNegativeSilentClientAfter403(t *testing.T) {
	server := startServer(t, Options{Destinations: []string{"declared.example:443"}})
	conn := newSilentAfterConn("CONNECT forbidden.example:443 HTTP/1.1\r\nHost: forbidden.example:443\r\n\r\n")
	start := time.Now()
	server.handle(t.Context(), conn, time.Now().Add(testWait))
	elapsed := time.Since(start)

	if elapsed < drainDeadline {
		t.Errorf("handle returned after %s, want at least drainDeadline (%s)", elapsed, drainDeadline)
	}
	if elapsed > drainDeadline+2*time.Second {
		t.Errorf("handle returned after %s, want bounded near drainDeadline (%s)", elapsed, drainDeadline)
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if !strings.HasPrefix(conn.out.String(), "HTTP/1.1 403 Forbidden\r\n") {
		t.Errorf("answer = %q, want 403 Forbidden", conn.out.String())
	}
	if server.Refused() != 1 {
		t.Errorf("Refused() = %d, want 1", server.Refused())
	}
}
