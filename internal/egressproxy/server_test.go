package egressproxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const testWait = 10 * time.Second

// syncBuffer is a log the server's goroutines and the test read and write at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runningServer is a server that serves until stop.
type runningServer struct {
	*Server
	log    *syncBuffer
	cancel context.CancelFunc
	served chan error
	once   sync.Once
	err    error
	// judged says that the test took the result of stop and judges it itself.
	judged bool
}

// stop ends the server and returns what Serve and Close returned. The test that calls it
// judges the result; the cleanup then leaves it alone.
func (r *runningServer) stop() error {
	r.judged = true
	return r.end()
}

func (r *runningServer) end() error {
	r.once.Do(func() {
		r.cancel()
		select {
		case err := <-r.served:
			r.err = errors.Join(err, r.Close())
		case <-time.After(testWait):
			r.err = errors.Join(fmt.Errorf("Serve still runs %s after its context ended", testWait), r.Close())
		}
	})
	return r.err
}

// skipWithoutUnixHalfClose skips a test that needs the peer to see a half-close over a Unix
// socket: on Windows it is lost now and then (issue #320), and the egress proxy runs only on
// Linux (ADR-0006).
func skipWithoutUnixHalfClose(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a half-close over a Unix socket is not reliably seen by the peer on Windows (issue #320); the egress proxy runs only on Linux (ADR-0006)")
	}
}

// shortDir is a directory with a short path: a socket address holds 108 bytes, and the
// temporary directory of a test with a long name leaves too few.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ep")
	if err != nil {
		t.Fatalf("MkdirTemp() = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("RemoveAll(%s) = %v, want nil", dir, err)
		}
	})
	return dir
}

// startServer serves opts until the test ends. A test that expects Serve to fail calls
// stop itself; every other test fails when Serve or Close returns an error.
func startServer(t *testing.T, opts Options) *runningServer {
	t.Helper()
	log := &syncBuffer{}
	if opts.Log == nil {
		opts.Log = log
	}
	if opts.Dir == "" {
		opts.Dir = shortDir(t)
	}
	server, err := Listen(opts)
	if err != nil {
		t.Fatalf("Listen(%v) = %v, want nil", opts.Destinations, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	running := &runningServer{Server: server, log: log, cancel: cancel, served: make(chan error, 1)}
	go func() { running.served <- server.Serve(ctx) }()
	t.Cleanup(func() {
		if err := running.end(); err != nil && !running.judged {
			t.Errorf("server stopped with %v, want nil", err)
		}
	})
	return running
}

// upstream is a TCP service on loopback. It answers every connection with what it read
// until the client ended its writing side, prefixed with "got:".
func upstream(t *testing.T) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen(upstream) = %v, want nil", err)
	}
	var serving sync.WaitGroup
	serving.Add(1)
	go func() {
		defer serving.Done()
		for i := 0; i < 4*maxTunnels; i++ {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			serving.Add(1)
			go func() {
				defer serving.Done()
				answerUpstream(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("Close(upstream) = %v, want nil", err)
		}
		serving.Wait()
	})
	return listener.Addr().String()
}

func answerUpstream(conn net.Conn) {
	// A failed read, write or close only ends this connection: the client's side of the test
	// then misses the answer and fails there.
	readErr := conn.SetDeadline(time.Now().Add(testWait))
	var body []byte
	if readErr == nil {
		body, readErr = io.ReadAll(conn)
	}
	if readErr == nil {
		if _, err := fmt.Fprintf(conn, "got:%s", body); err != nil {
			readErr = err
		}
	}
	if err := conn.Close(); err != nil || readErr != nil {
		return
	}
}

// tunnel is a client's connection to the server and the reader that holds what was read
// ahead of the status line.
type tunnel struct {
	conn   *net.UnixConn
	reader *bufio.Reader
}

// dialServer connects to the server's socket and closes the connection when the test ends.
func dialServer(t *testing.T, server *Server) tunnel {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: server.SocketPath(), Net: "unix"})
	if err != nil {
		t.Fatalf("DialUnix(%s) = %v, want nil", server.SocketPath(), err)
	}
	if err = conn.SetDeadline(time.Now().Add(testWait)); err != nil {
		t.Fatalf("SetDeadline() = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("Close(client) = %v, want nil", err)
		}
	})
	return tunnel{conn: conn, reader: bufio.NewReader(conn)}
}

// ask sends the request text and returns the status line of the answer.
func ask(t *testing.T, server *Server, request string) (tunnel, string) {
	t.Helper()
	tun := dialServer(t, server)
	if _, err := io.WriteString(tun.conn, request); err != nil {
		t.Fatalf("write request = %v, want nil", err)
	}
	status, err := tun.reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read status of %.40q = %v, want a status line", request, err)
	}
	return tun, strings.TrimRight(status, "\r\n")
}

// askMayClose is ask for a server that may answer and close before it has read the whole
// request: at its tunnel limit it answers 503 as it accepts, and a refusal can come while the
// rest of the request is still being written. The write may then find the socket closed
// (darwin reports a broken pipe). The answer is still there to read: a write error fails the
// test only when no status line follows it.
func askMayClose(t *testing.T, server *Server, request string) string {
	t.Helper()
	tun := dialServer(t, server)
	_, writeErr := io.WriteString(tun.conn, request)
	status, err := tun.reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read status of %.40q = %v (write: %v), want a status line", request, err, writeErr)
	}
	return strings.TrimRight(status, "\r\n")
}

func connectRequest(target string) string {
	return "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"
}

// finish ends the client's writing side and returns everything the tunnel still sends,
// the rest of the answer's header included.
func finish(t *testing.T, tun tunnel) string {
	t.Helper()
	if err := tun.conn.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite() = %v, want nil", err)
	}
	rest, err := io.ReadAll(tun.reader)
	if err != nil {
		t.Fatalf("read the tunnel to its end = %v, want nil", err)
	}
	return string(rest)
}

func TestServerTunnelsToDeclaredAddress(t *testing.T) {
	skipWithoutUnixHalfClose(t)
	target := upstream(t)
	server := startServer(t, Options{Destinations: []string{target}})
	tun, status := ask(t, server.Server, connectRequest(target))
	if status != "HTTP/1.1 200 Connection established" {
		t.Fatalf("status = %q, want 200", status)
	}
	if _, err := io.WriteString(tun.conn, "ping"); err != nil {
		t.Fatalf("write through the tunnel = %v, want nil", err)
	}
	// The upstream answers only after it saw the end of what the client sent, so this also
	// shows that the end of one direction passes through while the other stays open.
	if rest := finish(t, tun); rest != "\r\ngot:ping" {
		t.Fatalf("tunnel returned %q, want the upstream's answer", rest)
	}
	if server.Refused() != 0 || server.log.String() != "" {
		t.Fatalf("Refused() = %d, log %q, want a clean tunnel to leave neither", server.Refused(), server.log.String())
	}
}

func TestServerPassesBytesSentWithTheRequest(t *testing.T) {
	skipWithoutUnixHalfClose(t)
	target := upstream(t)
	server := startServer(t, Options{Destinations: []string{target}})
	tun, status := ask(t, server.Server, connectRequest(target)+"early")
	if !strings.Contains(status, " 200 ") {
		t.Fatalf("status = %q, want 200", status)
	}
	if rest := finish(t, tun); rest != "\r\ngot:early" {
		t.Fatalf("tunnel returned %q, want the bytes that followed the request", rest)
	}
}

func TestServerRefusals(t *testing.T) {
	target := upstream(t)
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatalf("SplitHostPort(%s) = %v, want nil", target, err)
	}
	tests := []struct {
		name    string
		request string
		status  string
		logged  string
	}{
		{"undeclared host", connectRequest("other.example:" + port), "403 Forbidden", "is not a declared destination"},
		{"declared host, other port", connectRequest(host + ":1"), "403 Forbidden", "is not a declared destination"},
		{"wildcard", connectRequest("*:" + port), "403 Forbidden", "host name label"},
		{"no port", connectRequest(host), "403 Forbidden", "want host:port"},
		{"other method", "GET http://" + target + "/ HTTP/1.1\r\n\r\n", "405 Method Not Allowed", "serves CONNECT only"},
		{"lower-case method", "connect " + target + " HTTP/1.1\r\n\r\n", "405 Method Not Allowed", "serves CONNECT only"},
		{"no version", "CONNECT " + target + "\r\n\r\n", "400 Bad Request", "not of the form"},
		{"a fourth field", "CONNECT " + target + " HTTP/1.1 more\r\n\r\n", "400 Bad Request", "not of the form"},
		{"other protocol", "CONNECT " + target + " SPDY/3\r\n\r\n", "400 Bad Request", "not of the form"},
		{"empty line first", "\r\n\r\n", "400 Bad Request", "not of the form"},
		{"line longer than the buffer", "CONNECT " + strings.Repeat("a", maxRequestBytes+1) + ":443 HTTP/1.1\r\n\r\n", "400 Bad Request", "read the request"},
		{"too many header bytes", "CONNECT " + target + " HTTP/1.1\r\n" + strings.Repeat("X-Fill: "+strings.Repeat("b", 1000)+"\r\n", 9) + "\r\n", "431 Request Header Fields Too Large", "header lines"},
	}
	server := startServer(t, Options{Destinations: []string{target}})
	for i := 0; i < len(tests); i++ {
		status := askMayClose(t, server.Server, tests[i].request)
		if status != "HTTP/1.1 "+tests[i].status {
			t.Errorf("%s: status = %q, want %s", tests[i].name, status, tests[i].status)
		}
		// The answer goes out before the count and the log line are written.
		if !waitFor(func() bool { return strings.Count(server.log.String(), "\n") == i+1 }) {
			t.Fatalf("%s: log = %q, want %d lines", tests[i].name, server.log.String(), i+1)
		}
		if server.Refused() != i+1 {
			t.Errorf("%s: Refused() = %d, want %d", tests[i].name, server.Refused(), i+1)
		}
		if line := lastLine(server.log.String()); !strings.Contains(line, tests[i].logged) {
			t.Errorf("%s: log line = %q, want it to say %q", tests[i].name, line, tests[i].logged)
		}
	}
}

// TestServerHeaderLineBoundary: maxHeaderLines header lines are served, one more is refused.
func TestServerHeaderLineBoundary(t *testing.T) {
	target := upstream(t)
	server := startServer(t, Options{Destinations: []string{target}})
	head := "CONNECT " + target + " HTTP/1.1\r\n"
	_, atLimit := ask(t, server.Server, head+strings.Repeat("X-A: b\r\n", maxHeaderLines)+"\r\n")
	if !strings.Contains(atLimit, " 200 ") {
		t.Errorf("%d header lines: status = %q, want 200", maxHeaderLines, atLimit)
	}
	_, over := ask(t, server.Server, head+strings.Repeat("X-A: b\r\n", maxHeaderLines+1)+"\r\n")
	if !strings.Contains(over, " 431 ") {
		t.Errorf("%d header lines: status = %q, want 431", maxHeaderLines+1, over)
	}
}

// resolver is a Lookup that answers every name with the same addresses.
func resolver(addresses ...string) func(context.Context, string) ([]net.IPAddr, error) {
	return func(context.Context, string) ([]net.IPAddr, error) {
		out := make([]net.IPAddr, 0, len(addresses))
		for i := 0; i < len(addresses); i++ {
			out = append(out, net.IPAddr{IP: net.ParseIP(addresses[i])})
		}
		return out, nil
	}
}

// dialLog is a Dial that records what it was asked for and connects to one fixed place.
type dialLog struct {
	mu    sync.Mutex
	asked []string
	to    string
}

func (d *dialLog) dial(ctx context.Context, network string, address string) (net.Conn, error) {
	d.mu.Lock()
	d.asked = append(d.asked, address)
	d.mu.Unlock()
	if d.to == "" {
		return nil, errors.New("no route in this test")
	}
	return (&net.Dialer{}).DialContext(ctx, network, d.to)
}

func (d *dialLog) calls() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.asked, " ")
}

func TestServerNameNeverLeadsToTheHost(t *testing.T) {
	dials := &dialLog{to: upstream(t)}
	server := startServer(t, Options{
		Destinations: []string{"gateway.example:8443"},
		Lookup:       resolver("127.0.0.1", "::1", "127.8.9.10", "0.0.0.0", "::", "169.254.169.254", "fe80::1", "224.0.0.1", "ff02::1"),
		Dial:         dials.dial,
	})
	_, status := ask(t, server.Server, connectRequest("gateway.example:8443"))
	if status != "HTTP/1.1 502 Bad Gateway" {
		t.Fatalf("status = %q, want 502 for a name with local addresses only", status)
	}
	if got := dials.calls(); got != "" {
		t.Fatalf("dialled %q, want no connection at all", got)
	}
	// The answer goes out before the log line: the line says whether the answer arrived.
	if !waitFor(func() bool { return strings.Contains(server.log.String(), "resolves to no address a name may lead to") }) {
		t.Fatalf("log = %q, want the reason", server.log.String())
	}
}

func TestServerNameSkipsLocalAddresses(t *testing.T) {
	skipWithoutUnixHalfClose(t)
	dials := &dialLog{to: upstream(t)}
	server := startServer(t, Options{
		Destinations: []string{"gateway.example:8443"},
		Lookup:       resolver("127.0.0.1", "192.0.2.7", "::1", "2001:db8::7"),
		Dial:         dials.dial,
	})
	// Case and the trailing dot are the client's spelling; the declared one is canonical.
	tun, status := ask(t, server.Server, connectRequest("GATEWAY.example.:8443"))
	if !strings.Contains(status, " 200 ") {
		t.Fatalf("status = %q, want 200", status)
	}
	if rest := finish(t, tun); rest != "\r\ngot:" {
		t.Fatalf("tunnel returned %q, want the upstream's answer", rest)
	}
	if got := dials.calls(); got != "192.0.2.7:8443" {
		t.Fatalf("dialled %q, want the first address that is not local and nothing else", got)
	}
}

func TestServerTriesAFixedNumberOfAddresses(t *testing.T) {
	addresses := make([]string, 0, maxAddresses+4)
	for i := 0; i < maxAddresses+4; i++ {
		addresses = append(addresses, fmt.Sprintf("192.0.2.%d", i+1))
	}
	dials := &dialLog{}
	server := startServer(t, Options{Destinations: []string{"gateway.example:8443"}, Lookup: resolver(addresses...), Dial: dials.dial})
	_, status := ask(t, server.Server, connectRequest("gateway.example:8443"))
	if !strings.Contains(status, " 502 ") {
		t.Fatalf("status = %q, want 502 when no address answers", status)
	}
	if got := len(strings.Fields(dials.calls())); got != maxAddresses {
		t.Fatalf("dialled %d addresses, want %d", got, maxAddresses)
	}
}

func TestServerLookupFailureIsRefused(t *testing.T) {
	dials := &dialLog{}
	server := startServer(t, Options{
		Destinations: []string{"gateway.example:8443"},
		Lookup: func(context.Context, string) ([]net.IPAddr, error) {
			// An answer that comes with an error is not used.
			return []net.IPAddr{{IP: net.ParseIP("192.0.2.7")}}, errors.New("no such host in this test")
		},
		Dial: dials.dial,
	})
	_, status := ask(t, server.Server, connectRequest("gateway.example:8443"))
	if !strings.Contains(status, " 502 ") || dials.calls() != "" {
		t.Fatalf("status = %q, dialled %q, want 502 and no connection", status, dials.calls())
	}
	want := `egress proxy: refused "gateway.example:8443": resolve gateway.example: no such host in this test` + "\n"
	if !waitFor(func() bool { return server.log.String() == want }) {
		t.Fatalf("log = %q, want %q", server.log.String(), want)
	}
}

// TestServerAddressIsReachedAsDeclared: an address is not looked up, loopback included. A
// gateway on the host is declared this way.
func TestServerAddressIsReachedAsDeclared(t *testing.T) {
	dials := &dialLog{to: upstream(t)}
	server := startServer(t, Options{
		Destinations: []string{"127.0.0.1:4000"},
		Lookup: func(context.Context, string) ([]net.IPAddr, error) {
			return nil, errors.New("an address must not be looked up")
		},
		Dial: dials.dial,
	})
	_, status := ask(t, server.Server, connectRequest("127.0.0.1:4000"))
	if !strings.Contains(status, " 200 ") || dials.calls() != "127.0.0.1:4000" {
		t.Fatalf("status = %q, dialled %q, want 200 and the declared address", status, dials.calls())
	}
}

func TestServerRefusesMoreThanMaxTunnels(t *testing.T) {
	skipWithoutUnixHalfClose(t)
	target := upstream(t)
	server := startServer(t, Options{Destinations: []string{target}})
	open := make([]tunnel, 0, maxTunnels)
	for i := 0; i < maxTunnels; i++ {
		tun, status := ask(t, server.Server, connectRequest(target))
		if !strings.Contains(status, " 200 ") {
			t.Fatalf("tunnel %d: status = %q, want 200", i, status)
		}
		open = append(open, tun)
	}
	if status := askMayClose(t, server.Server, connectRequest(target)); status != "HTTP/1.1 503 Service Unavailable" {
		t.Fatalf("tunnel %d: status = %q, want 503", maxTunnels, status)
	}
	if rest := finish(t, open[0]); rest != "\r\ngot:" {
		t.Fatalf("first tunnel returned %q, want the upstream's answer", rest)
	}
	// The slot is free once the server has seen the first tunnel end.
	freed := waitFor(func() bool {
		return strings.Contains(askMayClose(t, server.Server, connectRequest(target)), " 200 ")
	})
	if !freed {
		t.Fatalf("no tunnel after one of %d ended, want its slot to be free", maxTunnels)
	}
}

func TestServerLogIsBounded(t *testing.T) {
	server := startServer(t, Options{Destinations: []string{"gateway.example:443"}})
	const extra = 5
	for i := 0; i < maxLoggedLines+extra; i++ {
		tun, status := ask(t, server.Server, connectRequest("other.example:443"))
		if !strings.Contains(status, " 403 ") {
			t.Fatalf("request %d: status = %q, want 403", i, status)
		}
		if err := tun.conn.Close(); err != nil {
			t.Fatalf("request %d: Close() = %v, want nil", i, err)
		}
		// A refusal keeps its slot until it has been logged, after the client has read the
		// answer. The client closes so the drain ends. Without this wait, a slow runner piles up
		// more than maxTunnels of them and the next request is answered as busy.
		if !waitFor(func() bool { return len(server.pool.slots) == 0 }) {
			t.Fatalf("request %d: %d slots still held, want the refusal to free its slot", i, len(server.pool.slots))
		}
	}
	if !waitFor(func() bool { return server.Refused() == maxLoggedLines+extra }) {
		t.Fatalf("Refused() = %d, want every refusal counted (%d)", server.Refused(), maxLoggedLines+extra)
	}
	lines := strings.Split(strings.TrimSuffix(server.log.String(), "\n"), "\n")
	if len(lines) != maxLoggedLines+1 {
		t.Fatalf("log has %d lines, want %d and the closing one", len(lines), maxLoggedLines)
	}
	if !strings.Contains(lines[maxLoggedLines], "further ones are not logged") {
		t.Fatalf("last line = %q, want the note that logging stopped", lines[maxLoggedLines])
	}
}

// TestServerLogsTheTargetCut: what a job sends is printed quoted and cut, so it can neither
// forge a log line nor fill the log with one request.
func TestServerLogsTheTargetCut(t *testing.T) {
	server := startServer(t, Options{Destinations: []string{"gateway.example:443"}})
	long := strings.Repeat("a", 2*maxLoggedLine) + ":443"
	if _, status := ask(t, server.Server, connectRequest(long)); !strings.Contains(status, " 403 ") {
		t.Fatalf("status = %q, want 403", status)
	}
	if !waitFor(func() bool { return strings.HasSuffix(server.log.String(), "\n") }) {
		t.Fatalf("log = %q, want one line", server.log.String())
	}
	log := server.log.String()
	if strings.Count(log, "\n") != 1 || !strings.HasPrefix(log, `egress proxy: refused "`+strings.Repeat("a", maxLoggedTarget)+`": `) {
		t.Fatalf("log = %q, want one line with the target cut at %d characters", log, maxLoggedTarget)
	}
	// The reason names the target again, in full.
	if len(log) != maxLoggedLine+1 {
		t.Fatalf("the log line has %d characters, want it cut at %d", len(log)-1, maxLoggedLine)
	}
}

// TestServerLogsARefusalOnOneLine: the errors of several addresses print one to a line, and
// in the log of a job every line after the first would read as the job's own output.
func TestServerLogsARefusalOnOneLine(t *testing.T) {
	server := startServer(t, Options{
		Destinations: []string{"gateway.example:443"},
		Lookup:       resolver("192.0.2.7", "192.0.2.8"),
		Dial:         func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no route") },
	})
	if _, status := ask(t, server.Server, connectRequest("gateway.example:443")); !strings.Contains(status, " 502 ") {
		t.Fatalf("status = %q, want 502", status)
	}
	want := `egress proxy: refused "gateway.example:443": connect to gateway.example:443: no route; no route` + "\n"
	if !waitFor(func() bool { return server.log.String() == want }) {
		t.Fatalf("log = %q, want %q", server.log.String(), want)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the log is gone") }

func TestServeReportsALogThatCannotBeWritten(t *testing.T) {
	server := startServer(t, Options{Destinations: []string{"gateway.example:443"}, Log: failingWriter{}})
	if _, status := ask(t, server.Server, connectRequest("other.example:443")); !strings.Contains(status, " 403 ") {
		t.Fatalf("status = %q, want the refusal to stand without a log", status)
	}
	if !waitFor(func() bool { return server.Refused() == 1 }) {
		t.Fatalf("Refused() = %d, want 1", server.Refused())
	}
	err := server.stop()
	if err == nil || !strings.Contains(err.Error(), "write the log: the log is gone") {
		t.Fatalf("stop() = %v, want the log's error", err)
	}
}

func TestListenRefusals(t *testing.T) {
	tooMany := make([]string, 0, MaxDestinations+1)
	for i := 0; i < MaxDestinations+1; i++ {
		tooMany = append(tooMany, fmt.Sprintf("gateway%d.example:443", i))
	}
	dir := shortDir(t)
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"no destination", Options{Dir: dir}, "want 1..16 destinations, got 0"},
		{"too many destinations", Options{Dir: dir, Destinations: tooMany}, "want 1..16 destinations, got 17"},
		{"bad destination", Options{Dir: dir, Destinations: []string{"*.example:443"}}, "host name label"},
		{"missing directory", Options{Dir: dir + "/missing", Destinations: []string{"gateway.example:443"}}, "socket directory"},
	}
	for i := 0; i < len(tests); i++ {
		server, err := Listen(tests[i].opts)
		if err == nil || !strings.Contains(err.Error(), tests[i].want) {
			t.Errorf("Listen(%s) = %v, %v, want an error with %q", tests[i].name, server, err, tests[i].want)
		}
	}
	assertEmptyDir(t, dir)
}

func TestListenBoundaryDestinationsAndSocketPath(t *testing.T) {
	most := make([]string, 0, MaxDestinations)
	for i := 0; i < MaxDestinations; i++ {
		most = append(most, fmt.Sprintf("gateway%d.example:443", i))
	}
	dir := shortDir(t)
	server, err := Listen(Options{Dir: dir, Destinations: most})
	if err != nil {
		t.Fatalf("Listen(%d destinations) = %v, want nil", MaxDestinations, err)
	}
	if len(server.SocketPath()) > maxSocketPath {
		t.Errorf("SocketPath() has %d bytes, want at most %d", len(server.SocketPath()), maxSocketPath)
	}
	if err = server.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	assertEmptyDir(t, dir)
	// A directory whose path alone is at the limit leaves no room for the socket's name.
	long := dir + "/" + strings.Repeat("d", maxSocketPath-len(dir)-1)
	if err = os.Mkdir(long, 0o700); err != nil {
		t.Fatalf("Mkdir(%s) = %v, want nil", long, err)
	}
	if _, err = Listen(Options{Dir: long, Destinations: most}); err == nil || !strings.Contains(err.Error(), "is longer than") {
		t.Fatalf("Listen(long directory) = %v, want the socket path refused", err)
	}
	assertEmptyDir(t, long)
}

func TestListenMakesAPrivateSocketDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the egress proxy does not run on Windows")
	}
	server := startServer(t, Options{Destinations: []string{"gateway.example:443"}})
	info, err := os.Stat(server.dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("Stat(%s) = %v, %v, want a directory only its owner can enter", server.dir, info, err)
	}
	if err = server.stop(); err != nil {
		t.Fatalf("stop() = %v, want nil", err)
	}
	if _, err = os.Stat(server.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(%s) after Close = %v, want it removed", server.dir, err)
	}
}

func TestServeEndsOpenTunnelsWhenItsContextEnds(t *testing.T) {
	target := upstream(t)
	server := startServer(t, Options{Destinations: []string{target}})
	tun, status := ask(t, server.Server, connectRequest(target))
	if !strings.Contains(status, " 200 ") {
		t.Fatalf("status = %q, want 200", status)
	}
	if err := server.stop(); err != nil {
		t.Fatalf("stop() with an open tunnel = %v, want nil", err)
	}
	if _, err := io.ReadAll(tun.reader); err != nil {
		t.Fatalf("read after the server stopped = %v, want the plain end of the tunnel", err)
	}
}

// TestServeEndsAtItsDeadline: the deadline of the context is the deadline of the listener
// and of every tunnel, and reaching it is no error.
func TestServeEndsAtItsDeadline(t *testing.T) {
	target := upstream(t)
	server, err := Listen(Options{Dir: shortDir(t), Destinations: []string{target}})
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()
	tun, status := ask(t, server, connectRequest(target))
	if !strings.Contains(status, " 200 ") {
		t.Fatalf("status = %q, want 200", status)
	}
	select {
	case err = <-served:
	case <-time.After(testWait):
		t.Fatalf("Serve still runs %s after its deadline", testWait)
	}
	if err != nil {
		t.Fatalf("Serve() = %v, want nil at the deadline", err)
	}
	if _, err = io.ReadAll(tun.reader); err != nil {
		t.Fatalf("read after the deadline = %v, want the plain end of the tunnel", err)
	}
	if err = server.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("ReadDir(%s) = %d entries, %v, want none left behind", dir, len(entries), err)
	}
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	return lines[len(lines)-1]
}

// waitFor polls ok for up to testWait.
func waitFor(ok func() bool) bool {
	const step = 10 * time.Millisecond
	for i := 0; i < int(testWait/step); i++ {
		if ok() {
			return true
		}
		time.Sleep(step)
	}
	return false
}
