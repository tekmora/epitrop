package egressproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxRequestBytes = 8 << 10
	maxHeaderLines  = 64
	maxAddresses    = 8
	maxLoggedLines  = 128
	maxLoggedTarget = 128
	// maxLoggedLine is the length of one log line: room for the cut target and the errors
	// of every address of a name.
	maxLoggedLine = 1024

	handshakeTimeout = 10 * time.Second
	dialTimeout      = 10 * time.Second
	// maxTunnelLife bounds a tunnel when the server's context carries no deadline.
	maxTunnelLife = 24 * time.Hour

	// maxSocketPath is below the 108 bytes a Unix socket address holds, the ending zero
	// byte included.
	maxSocketPath = 100
	socketName    = "s"
)

// Options configure a Server.
type Options struct {
	// Destinations are the "host:port" values a job may reach, each in its one spelling.
	Destinations []string
	// Dir is where the server makes the directory of its socket. Empty means the
	// temporary directory of the process.
	Dir string
	// Log receives one line for a refused request and one for a tunnel that ended with an
	// error, up to a fixed number of lines. Nil means none.
	Log io.Writer
	// Lookup and Dial replace the resolver and the dialer in tests. Nil means the
	// system's.
	Lookup func(ctx context.Context, host string) ([]net.IPAddr, error)
	Dial   func(ctx context.Context, network string, address string) (net.Conn, error)
}

// Server is the proxy outside the sandbox. It answers one request per connection,
// "CONNECT host:port", and opens the tunnel only when host:port is a declared destination.
// A declared name that resolves to a loopback, link-local, multicast or unspecified address
// is not reached through that address: a name must not lead to the services of the host
// the control plane runs on. A destination on loopback is declared as an address.
type Server struct {
	listener     *net.UnixListener
	dir          string
	destinations []Destination
	pool         *pool
	lookup       func(ctx context.Context, host string) ([]net.IPAddr, error)
	dial         func(ctx context.Context, network string, address string) (net.Conn, error)

	mu      sync.Mutex
	log     io.Writer
	lines   int
	refused int
}

// Listen makes a directory only its owner can enter, and in it the socket the server
// accepts on. Close removes both.
func Listen(opts Options) (*Server, error) {
	destinations, err := parseDestinations(opts.Destinations)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(opts.Dir, "epitrop-proxy-")
	if err != nil {
		return nil, fmt.Errorf("egress proxy: socket directory: %w", err)
	}
	path, err := socketPath(dir)
	if err != nil {
		return nil, errors.Join(err, os.Remove(dir))
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("egress proxy: listen on %s: %w", path, err), os.Remove(dir))
	}
	server := &Server{
		listener: listener, dir: dir, destinations: destinations,
		pool: newPool(), log: opts.Log,
		lookup: opts.Lookup, dial: opts.Dial,
	}
	if server.lookup == nil {
		server.lookup = net.DefaultResolver.LookupIPAddr
	}
	if server.dial == nil {
		server.dial = (&net.Dialer{}).DialContext
	}
	return server, nil
}

// socketPath is the path of the socket in dir. A path a socket address may not hold is an
// error.
func socketPath(dir string) (string, error) {
	path := filepath.Join(dir, socketName)
	if len(path) > maxSocketPath {
		return "", fmt.Errorf("egress proxy: socket path %s is longer than %d bytes", path, maxSocketPath)
	}
	return path, nil
}

func parseDestinations(values []string) ([]Destination, error) {
	if len(values) == 0 || len(values) > MaxDestinations {
		return nil, fmt.Errorf("egress proxy: want 1..%d destinations, got %d", MaxDestinations, len(values))
	}
	out := make([]Destination, 0, len(values))
	for i := 0; i < len(values); i++ {
		dest, err := ParseDestination(values[i])
		if err != nil {
			return nil, fmt.Errorf("egress proxy: %w", err)
		}
		out = append(out, dest)
	}
	return out, nil
}

// SocketPath is the socket a sandbox binds.
func (s *Server) SocketPath() string {
	return filepath.Join(s.dir, socketName)
}

// Close stops accepting and removes the socket and its directory. Open tunnels end when
// the context of Serve ends.
func (s *Server) Close() error {
	return errors.Join(closed(s.listener), os.RemoveAll(s.dir))
}

// Serve accepts connections until ctx ends or the server is closed, and returns when the
// last tunnel has ended. Both ends are no error; what Serve returns is what went wrong
// beside the tunnels, such as a log that could not be written.
func (s *Server) Serve(ctx context.Context) error {
	deadline := tunnelDeadline(ctx)
	err := s.pool.serve(ctx, s.listener, deadline,
		func(conn net.Conn) { s.handle(ctx, conn, deadline) },
		func(conn net.Conn) {
			s.refuse(conn, "503 Service Unavailable", "", fmt.Errorf("%d tunnels are open, the most at a time", maxTunnels))
		})
	if err != nil {
		return fmt.Errorf("egress proxy: %w", err)
	}
	return nil
}

func (s *Server) handle(ctx context.Context, client net.Conn, deadline time.Time) {
	if err := client.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		s.pool.fault(fmt.Errorf("request deadline: %w", err))
		return
	}
	reader := bufio.NewReaderSize(client, maxRequestBytes)
	target, status, err := readConnect(reader)
	if err != nil {
		s.refuseAndDrain(client, status, target, err)
		return
	}
	dest, err := s.declared(target)
	if err != nil {
		s.refuseAndDrain(client, "403 Forbidden", target, err)
		return
	}
	upstream, err := s.connect(ctx, dest)
	if err != nil {
		s.refuseAndDrain(client, "502 Bad Gateway", target, err)
		return
	}
	defer func() { s.pool.fault(closed(upstream)) }()
	if err = errors.Join(client.SetDeadline(deadline), upstream.SetDeadline(deadline)); err != nil {
		s.refuseAndDrain(client, "500 Internal Server Error", target, err)
		return
	}
	if _, err = io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		s.logLine(fmt.Sprintf("egress proxy: tunnel to %s: answer the request: %v", dest, err))
		return
	}
	if err = pipe(ctx, client, reader, upstream); err != nil {
		s.logLine(fmt.Sprintf("egress proxy: tunnel to %s ended: %v", dest, err))
	}
}

// readConnect reads the request line and skips the header lines. It returns the target as
// the client wrote it, and for a refusal the status to answer with.
func readConnect(reader *bufio.Reader) (string, string, error) {
	line, err := readLine(reader)
	if err != nil {
		return "", "400 Bad Request", err
	}
	fields := strings.Fields(line)
	if len(fields) != 3 || !strings.HasPrefix(fields[2], "HTTP/1.") {
		return "", "400 Bad Request", fmt.Errorf("the request line is not of the form CONNECT host:port HTTP/1.1")
	}
	if fields[0] != "CONNECT" {
		return fields[1], "405 Method Not Allowed", fmt.Errorf("method %.16q: this proxy serves CONNECT only", fields[0])
	}
	total := len(line)
	// One more read than header lines: the empty line that ends them.
	for i := 0; i <= maxHeaderLines; i++ {
		header, readErr := readLine(reader)
		if readErr != nil {
			return fields[1], "400 Bad Request", readErr
		}
		if header == "" {
			return fields[1], "", nil
		}
		if total += len(header); total > maxRequestBytes {
			break
		}
	}
	return fields[1], "431 Request Header Fields Too Large", fmt.Errorf("the request has more than %d header lines or %d bytes", maxHeaderLines, maxRequestBytes)
}

// readLine returns one line without its ending. A line longer than the reader's buffer is
// an error.
func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return "", fmt.Errorf("read the request: %w", err)
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// declared returns the declared destination the target names.
func (s *Server) declared(target string) (Destination, error) {
	want, err := ParseDestination(target)
	if err != nil {
		return Destination{}, err
	}
	for i := 0; i < len(s.destinations); i++ {
		if s.destinations[i] == want {
			return want, nil
		}
	}
	return Destination{}, fmt.Errorf("%s is not a declared destination", want)
}

// connect opens the connection to a declared destination, to the first of its addresses
// that answers.
func (s *Server) connect(ctx context.Context, dest Destination) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	addresses, err := s.addresses(ctx, dest)
	if err != nil {
		return nil, err
	}
	var failed error
	for i := 0; i < len(addresses); i++ {
		conn, dialErr := s.dial(ctx, "tcp", addresses[i])
		if dialErr == nil {
			return conn, nil
		}
		failed = errors.Join(failed, dialErr)
	}
	return nil, fmt.Errorf("connect to %s: %w", dest, failed)
}

// addresses are where a destination is reached: an address literal as declared, a name at
// up to maxAddresses of the addresses it resolves to and may lead to.
func (s *Server) addresses(ctx context.Context, dest Destination) ([]string, error) {
	if dest.Literal {
		return []string{dest.String()}, nil
	}
	found, err := s.lookup(ctx, dest.Host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dest.Host, err)
	}
	out := make([]string, 0, maxAddresses)
	for i := 0; i < len(found) && len(out) < maxAddresses; i++ {
		ip := found[i].IP
		if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
			continue
		}
		out = append(out, Destination{Host: ip.String(), Port: dest.Port}.String())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s resolves to no address a name may lead to (loopback and link-local are declared as addresses)", dest.Host)
	}
	return out, nil
}

// refuse answers a request that gets no tunnel and writes the reason to the log. target is
// what the client sent and is printed quoted and cut.
func (s *Server) refuse(client net.Conn, status string, target string, cause error) {
	if err := answer(client, status); err != nil {
		// The client may be gone already; the log line is the record either way.
		cause = fmt.Errorf("%w (the answer did not reach the client: %w)", cause, err)
	}
	s.mu.Lock()
	s.refused++
	s.mu.Unlock()
	s.logLine(fmt.Sprintf("egress proxy: refused %.*q: %v", maxLoggedTarget, target, cause))
}

// refuseAndDrain answers a refused request, logs the refusal, and drains unread bytes
// from the client under a read deadline before handle returns.
func (s *Server) refuseAndDrain(client net.Conn, status string, target string, cause error) {
	s.refuse(client, status, target, cause)
	s.pool.fault(s.pool.drain(client))
}

func answer(client net.Conn, status string) error {
	if err := client.SetWriteDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return err
	}
	_, err := fmt.Fprintf(client, "HTTP/1.1 %s\r\nConnection: close\r\nContent-Length: 0\r\n\r\n", status)
	return err
}

// logLine writes one line to the log, up to maxLoggedLines of them. It stays one line
// whatever the error in it prints, and is cut at maxLoggedLine characters. A job can fill
// the log neither by asking again and again nor with one long request, and no part of a
// line reads as the job's own output.
func (s *Server) logLine(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines++
	if s.log == nil || s.lines > maxLoggedLines+1 {
		return
	}
	if s.lines == maxLoggedLines+1 {
		line = fmt.Sprintf("egress proxy: %d lines written; further ones are not logged", maxLoggedLines)
	}
	if _, err := fmt.Fprintf(s.log, "%.*s\n", maxLoggedLine, oneLine(line)); err != nil {
		s.pool.fault(fmt.Errorf("write the log: %w", err))
	}
}

// Refused is how many requests got no tunnel.
func (s *Server) Refused() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

func tunnelDeadline(ctx context.Context) time.Time {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return time.Now().Add(maxTunnelLife)
}
