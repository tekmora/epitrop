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
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// probeCommand makes the test binary a client of the relay. See TestMain.
const (
	probeCommand = "__test-probe"
	probeFailed  = 3
	// resetCommand makes the test binary a client that resets its connection.
	resetCommand = "__test-reset"
)

// TestMain lets the test binary be the two programs the relay tests start: the relay itself,
// as the epitrop program is inside a sandbox, and a client that asks the relay for a tunnel.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == RelayCommand {
		os.Exit(RunRelay(os.Args[1:], os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == probeCommand {
		os.Exit(runProbe(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == resetCommand {
		os.Exit(runReset(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// runProbe is: <proxy address> <target> <payload>. It asks the proxy for a tunnel to the
// target and prints the status line; through an open tunnel it sends the payload, ends its
// writing side and prints what comes back.
func runProbe(args []string) int {
	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "probe: want <proxy address> <target> <payload>")
		return probeFailed
	}
	if err := probe(args[0], args[1], args[2], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "probe: %v\n", err)
		return probeFailed
	}
	return 0
}

func probe(proxy string, target string, payload string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxy)
	if err != nil {
		return fmt.Errorf("connect to the relay: %w", err)
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return errors.Join(errors.New("the relay connection is no TCP connection"), conn.Close())
	}
	err = probeTunnel(tcp, target, payload, out)
	return errors.Join(err, ignoreClosed(tcp.Close()))
}

func probeTunnel(conn *net.TCPConn, target string, payload string, out io.Writer) error {
	if err := conn.SetDeadline(time.Now().Add(testWait)); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, connectRequest(target)); err != nil {
		return fmt.Errorf("send the request: %w", err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read the status line: %w", err)
	}
	if _, err = fmt.Fprintln(out, strings.TrimRight(status, "\r\n")); err != nil {
		return err
	}
	if !strings.Contains(status, " 200 ") {
		return nil
	}
	if _, err = io.WriteString(conn, payload); err != nil {
		return fmt.Errorf("send the payload: %w", err)
	}
	if err = conn.CloseWrite(); err != nil {
		return err
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read the tunnel: %w", err)
	}
	_, err = fmt.Fprintln(out, strings.TrimLeft(string(rest), "\r\n"))
	return err
}

// runReset is: <proxy address> <target>. It asks the proxy for a tunnel, reads the first byte
// of the answer and resets the connection, as a program does that closes a connection with
// bytes unread.
func runReset(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "reset: want <proxy address> <target>")
		return probeFailed
	}
	if err := resetAfterOneByte(args[0], args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "reset: %v\n", err)
		return probeFailed
	}
	return 0
}

func resetAfterOneByte(proxy string, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxy)
	if err != nil {
		return fmt.Errorf("connect to the relay: %w", err)
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return errors.Join(errors.New("the relay connection is no TCP connection"), conn.Close())
	}
	if err = tcp.SetDeadline(time.Now().Add(testWait)); err != nil {
		return errors.Join(err, tcp.Close())
	}
	if _, err = io.WriteString(tcp, connectRequest(target)); err != nil {
		return errors.Join(fmt.Errorf("send the request: %w", err), tcp.Close())
	}
	if _, err = io.ReadFull(tcp, make([]byte, 1)); err != nil {
		return errors.Join(fmt.Errorf("read the first byte: %w", err), tcp.Close())
	}
	// With no linger time, closing sends a reset and not the ordinary end.
	return errors.Join(tcp.SetLinger(0), tcp.Close())
}

// freeLoopback is a loopback address nothing listens on at the time of the call.
func freeLoopback(t *testing.T) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	return address
}

type relayRun struct {
	stdout string
	stderr string
	code   int
}

// relayCommand is the test binary as the relay, in front of command.
func relayCommand(ctx context.Context, t *testing.T, socket string, listen string, command ...string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() = %v, want nil", err)
	}
	args := append([]string{RelayCommand, "--socket", socket, "--listen", listen, "--"}, command...)
	// #nosec G204 -- the test binary and a command the test wrote.
	cmd := exec.CommandContext(ctx, self, args...)
	// As the sandbox starts it: the number of processors is in the relay's environment.
	cmd.Env = append(os.Environ(), "GOMAXPROCS="+strconv.Itoa(RelayProcs))
	return cmd
}

// runRelayProcess runs the relay to its end.
func runRelayProcess(t *testing.T, socket string, listen string, command ...string) relayRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*testWait)
	defer cancel()
	cmd := relayCommand(ctx, t, socket, listen, command...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var exit *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exit) {
		t.Fatalf("run the relay = %v, want it to start and end", err)
	}
	return relayRun{stdout: stdout.String(), stderr: stderr.String(), code: cmd.ProcessState.ExitCode()}
}

func probeArgs(t *testing.T, listen string, target string, payload string) []string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() = %v, want nil", err)
	}
	return []string{self, probeCommand, listen, target, payload}
}

func TestParseRelayArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"with the command word", []string{RelayCommand, "--socket", "/s", "--listen", "127.0.0.1:3128", "--", "true"}, ""},
		{"without the command word", []string{"--socket", "/s", "--listen", "127.0.0.1:3128", "--", "env", "-u", "X", "true"}, ""},
		{"listen on ::1", []string{"--socket", "/s", "--listen", "[::1]:3128", "--", "true"}, ""},
		{"listen on the last loopback address", []string{"--socket", "/s", "--listen", "127.255.255.254:1", "--", "true"}, ""},
		{"no socket", []string{"--listen", "127.0.0.1:3128", "--", "true"}, "a socket and a command are required"},
		{"no command", []string{"--socket", "/s", "--listen", "127.0.0.1:3128", "--"}, "a socket and a command are required"},
		{"no listen address", []string{"--socket", "/s", "--", "true"}, "want address:port"},
		{"listen without a port", []string{"--socket", "/s", "--listen", "127.0.0.1", "--", "true"}, "want address:port"},
		{"listen on every address", []string{"--socket", "/s", "--listen", "0.0.0.0:3128", "--", "true"}, "loopback address only"},
		{"listen on the first address past loopback", []string{"--socket", "/s", "--listen", "128.0.0.0:3128", "--", "true"}, "loopback address only"},
		{"listen on a name", []string{"--socket", "/s", "--listen", "localhost:3128", "--", "true"}, "loopback address only"},
		{"unknown flag", []string{"--socket", "/s", "--listen", "127.0.0.1:3128", "--other", "--", "true"}, "parse:"},
	}
	for i := 0; i < len(tests); i++ {
		cfg, err := parseRelayArgs(tests[i].args)
		if tests[i].want == "" {
			if err != nil || cfg.Socket != "/s" || cfg.Command[len(cfg.Command)-1] != "true" {
				t.Errorf("%s: parseRelayArgs() = %+v, %v, want the socket and the command", tests[i].name, cfg, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tests[i].want) {
			t.Errorf("%s: parseRelayArgs() = %+v, %v, want an error with %q", tests[i].name, cfg, err, tests[i].want)
		}
	}
}

func TestParseRelayArgsKeepsTheCommandWhole(t *testing.T) {
	cfg, err := parseRelayArgs([]string{RelayCommand, "--socket", "/s", "--listen", "127.0.0.1:1", "--", "env", "--default-signal=TERM", "sh", "-c", "exit 0"})
	want := []string{"env", "--default-signal=TERM", "sh", "-c", "exit 0"}
	if err != nil || !slices.Equal(cfg.Command, want) || cfg.Listen != "127.0.0.1:1" {
		t.Fatalf("parseRelayArgs() = %+v, %v, want the command %q untouched", cfg, err, want)
	}
}

// TestRelayExitCodes: the relay ends with the command's code, so the supervisor above it
// reads the job's result and not the relay's.
func TestRelayExitCodes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the relay runs inside the sandbox, which does not run on Windows")
	}
	socket := shortDir(t) + "/none"
	tests := []struct {
		name    string
		command []string
		code    int
		stderr  string
	}{
		{"success", []string{"sh", "-c", "exit 0"}, 0, ""},
		{"the command's code", []string{"sh", "-c", "exit 7"}, 7, ""},
		{"the highest code", []string{"sh", "-c", "exit 255"}, 255, ""},
		{"ended by a signal", []string{"sh", "-c", "kill -KILL $$"}, 137, ""},
		{"command that cannot start", []string{"/nonexistent/command"}, RelayFailed, "start /nonexistent/command"},
	}
	for i := 0; i < len(tests); i++ {
		run := runRelayProcess(t, socket, freeLoopback(t), tests[i].command...)
		if run.code != tests[i].code || !strings.Contains(run.stderr, tests[i].stderr) {
			t.Errorf("%s: exit %d, stderr %q, want exit %d and stderr with %q", tests[i].name, run.code, run.stderr, tests[i].code, tests[i].stderr)
		}
		if tests[i].stderr == "" && run.stderr != "" {
			t.Errorf("%s: stderr = %q, want a relay that worked to say nothing", tests[i].name, run.stderr)
		}
	}
}

func TestRelayPassesOutputAndInputThrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*testWait)
	defer cancel()
	cmd := relayCommand(ctx, t, shortDir(t)+"/none", freeLoopback(t), "sh", "-c", "read line; echo out:$line; echo err:$line >&2")
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader("hello\n"), &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run the relay = %v, want exit 0", err)
	}
	if stdout.String() != "out:hello\n" || stderr.String() != "err:hello\n" {
		t.Fatalf("stdout %q, stderr %q, want the command's own lines and nothing else", stdout.String(), stderr.String())
	}
}

func TestRelayFailsWhenItCannotListen(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Errorf("Close() = %v, want nil", err)
		}
	}()
	run := runRelayProcess(t, shortDir(t)+"/none", listener.Addr().String(), "sh", "-c", "echo started")
	if run.code != RelayFailed || !strings.Contains(run.stderr, "listen on") || run.stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q, want exit %d before the command starts", run.code, run.stdout, run.stderr, RelayFailed)
	}
}

func TestRelayFailsOnBadArguments(t *testing.T) {
	run := runRelayProcess(t, shortDir(t)+"/none", "192.0.2.1:3128", "sh", "-c", "echo started")
	if run.code != RelayFailed || !strings.Contains(run.stderr, "loopback address only") || run.stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q, want exit %d and no command", run.code, run.stdout, run.stderr, RelayFailed)
	}
}

// TestRelayReachesADeclaredDestination is the whole way a job's request takes: client, relay
// on loopback, socket, server, destination.
func TestRelayReachesADeclaredDestination(t *testing.T) {
	skipWithoutUnixHalfClose(t)
	target := upstream(t)
	server := startServer(t, Options{Destinations: []string{target}})
	listen := freeLoopback(t)
	run := runRelayProcess(t, server.SocketPath(), listen, probeArgs(t, listen, target, "ping")...)
	if run.code != 0 || run.stdout != "HTTP/1.1 200 Connection established\ngot:ping\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q, want the tunnel and the destination's answer", run.code, run.stdout, run.stderr)
	}
	if run.stderr != "" || server.Refused() != 0 {
		t.Fatalf("stderr %q, Refused() = %d, want neither", run.stderr, server.Refused())
	}
}

// TestRelayDecidesNothing: a target that is not declared is refused by the server, and the
// client behind the relay sees that answer.
func TestRelayDecidesNothing(t *testing.T) {
	target := upstream(t)
	other := upstream(t)
	server := startServer(t, Options{Destinations: []string{target}})
	listen := freeLoopback(t)
	run := runRelayProcess(t, server.SocketPath(), listen, probeArgs(t, listen, other, "ping")...)
	if run.code != 0 || run.stdout != "HTTP/1.1 403 Forbidden\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q, want the server's refusal", run.code, run.stdout, run.stderr)
	}
	if !waitFor(func() bool { return server.Refused() == 1 }) {
		t.Fatalf("Refused() = %d, want 1", server.Refused())
	}
}

// TestRelayWithoutAServerGivesNoTunnel: with nothing behind the socket the client gets no
// answer, the relay says why, and the command's result still stands.
func TestRelayWithoutAServerGivesNoTunnel(t *testing.T) {
	listen := freeLoopback(t)
	socket := shortDir(t) + "/none"
	run := runRelayProcess(t, socket, listen, probeArgs(t, listen, "gateway.example:443", "ping")...)
	if run.code != probeFailed || run.stdout != "" {
		t.Fatalf("exit %d, stdout %q, want the client to fail with exit %d", run.code, run.stdout, probeFailed)
	}
	if !strings.Contains(run.stderr, "connect to the proxy socket "+socket) {
		t.Fatalf("stderr = %q, want the relay to name the socket it could not reach", run.stderr)
	}
}

// TestRelaySaysNothingWhenItsClientResets: a program that closes a connection with bytes
// unread resets it. That is the program's way to end a tunnel, open or refused, and the
// relay writes nothing about it into the job's output.
func TestRelaySaysNothingWhenItsClientResets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the relay runs inside the sandbox, which does not run on Windows")
	}
	target := upstream(t)
	server := startServer(t, Options{Destinations: []string{target}})
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() = %v, want nil", err)
	}
	targets := []string{target, "other.example:443"}
	for i := 0; i < len(targets); i++ {
		listen := freeLoopback(t)
		run := runRelayProcess(t, server.SocketPath(), listen, self, resetCommand, listen, targets[i])
		if run.code != 0 || run.stderr != "" {
			t.Errorf("client reset its tunnel to %s: exit %d, stderr %q, want exit 0 and nothing said", targets[i], run.code, run.stderr)
		}
	}
	if !waitFor(func() bool { return server.Refused() == 1 }) {
		t.Fatalf("Refused() = %d, want the one undeclared target", server.Refused())
	}
}

// TestRelayNamesEveryFaultOnOneLine: what the relay says goes into the job's log, where a
// line without the relay's name would read as the job's own.
func TestRelayNamesEveryFaultOnOneLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the relay runs inside the sandbox, which does not run on Windows")
	}
	listen := freeLoopback(t)
	socket := shortDir(t) + "/none"
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() = %v, want nil", err)
	}
	script := fmt.Sprintf("%s %s %s gateway.example:443 ping; %s %s %s gateway.example:443 ping; exit 0", self, probeCommand, listen, self, probeCommand, listen)
	run := runRelayProcess(t, socket, listen, "sh", "-c", script)
	lines := strings.Split(strings.TrimRight(run.stderr, "\n"), "\n")
	relayLines := 0
	for i := 0; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "probe: ") {
			continue
		}
		relayLines++
		if !strings.HasPrefix(lines[i], "epitrop: egress relay: ") || strings.Count(lines[i], "connect to the proxy socket "+socket) != 2 {
			t.Errorf("stderr line %q, want one line of the relay that names both failed connections", lines[i])
		}
	}
	if run.code != 0 || relayLines != 1 {
		t.Fatalf("exit %d, %d relay lines in %q, want exit 0 and one line", run.code, relayLines, run.stderr)
	}
}

func TestReportedBothWays(t *testing.T) {
	fault := errors.Join(errors.New("first"), errors.New("second"))
	tests := []struct {
		name     string
		code     int
		err      error
		failing  bool
		wantCode int
		wantText string
	}{
		{"nothing to report", 0, nil, false, 0, ""},
		{"nothing to report and no way to write", 0, nil, true, 0, ""},
		{"a fault beside a clean run", 0, fault, false, 0, "epitrop: egress relay: first; second\n"},
		{"a fault beside a failed run", 3, fault, false, 3, "epitrop: egress relay: first; second\n"},
		{"an unwritten fault beside a clean run", 0, fault, true, RelayFailed, ""},
		{"an unwritten fault beside a failed run", 3, fault, true, 3, ""},
	}
	for i := 0; i < len(tests); i++ {
		var text bytes.Buffer
		var out io.Writer = &text
		if tests[i].failing {
			out = failingWriter{}
		}
		if got := reported(out, tests[i].code, tests[i].err); got != tests[i].wantCode || text.String() != tests[i].wantText {
			t.Errorf("%s: reported() = %d and %q, want %d and %q", tests[i].name, got, text.String(), tests[i].wantCode, tests[i].wantText)
		}
	}
}

func TestErrorLineIsOneLine(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{errors.New("one"), "one"},
		{errors.Join(errors.New("one"), errors.New("two")), "one; two"},
		{fmt.Errorf("outer: %w", errors.Join(errors.New("one"), errors.Join(errors.New("two"), errors.New("three")))), "outer: one; two; three"},
	}
	for i := 0; i < len(tests); i++ {
		if got := ErrorLine(tests[i].err); got != tests[i].want {
			t.Errorf("ErrorLine(%q) = %q, want %q", tests[i].err, got, tests[i].want)
		}
	}
}

func TestPeerEndedBothWays(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{io.EOF, false},
		{net.ErrClosed, false},
		{syscall.ENOTCONN, false},
		{os.ErrDeadlineExceeded, false},
		{syscall.ECONNRESET, true},
		{syscall.EPIPE, true},
		{&net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("splice", syscall.ECONNRESET)}, true},
		{errors.Join(io.ErrUnexpectedEOF, fmt.Errorf("write: %w", syscall.EPIPE)), true},
	}
	for i := 0; i < len(tests); i++ {
		if got := peerEnded(tests[i].err); got != tests[i].want {
			t.Errorf("peerEnded(%v) = %v, want %v", tests[i].err, got, tests[i].want)
		}
	}
}
