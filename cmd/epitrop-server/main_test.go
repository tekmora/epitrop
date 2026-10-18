package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/egressproxy"
	"github.com/tekmora/epitrop/internal/logsetup"
	"github.com/tekmora/epitrop/internal/mcp"
	"github.com/tekmora/epitrop/internal/supervisor"
	"github.com/tekmora/epitrop/internal/units"
)

const unitID = "tekmora/epitrop#35"

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// writeConfig writes a signing key and a config with one job that never runs, and returns
// the config path.
func writeConfig(t *testing.T) string {
	t.Helper()
	return writeConfigServing(t, `{"transport":"stdio"}`)
}

// writeConfigServing is writeConfig with server set to the JSON object server.
func writeConfigServing(t *testing.T, server string) string {
	t.Helper()
	base := t.TempDir()
	keyPath := filepath.Join(base, "seed.hex")
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(bytes.Repeat([]byte{9}, 32))), 0o600); err != nil {
		t.Fatalf("WriteFile(key) = %v", err)
	}
	cfgPath := filepath.Join(base, "config.json")
	body := fmt.Sprintf(`{"server":%s,"event_log":{"dir":%s,"signing_key_path":%s},"jobs":[{"name":"idle","command":["sleep","30"],"log_path":%s,"schedule":"always","sandbox":{"mode":"off","reason":"server test"}}]}`,
		server, jsonQuote(filepath.Join(base, "state")), jsonQuote(keyPath), jsonQuote(filepath.Join(base, "idle.log")))
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(config) = %v", err)
	}
	return cfgPath
}

// recordUnit writes one unit through the same event log the server reads.
func recordUnit(t *testing.T, cfgPath string) {
	t.Helper()
	cfg, err := config.Load(testContext(t), cfgPath)
	if err != nil {
		t.Fatalf("config.Load() = %v", err)
	}
	svc, err := logsetup.UnitsService(cfg)
	if err != nil {
		t.Fatalf("UnitsService() = %v", err)
	}
	branch := "feat/35-mcp-readonly"
	if _, err := svc.Record(testContext(t), unitID, units.RecordPayload{Status: "implementing", Branch: &branch}); err != nil {
		t.Fatalf("Record() = %v", err)
	}
}

// stdioClient runs serve on pipes and connects a client over the same line framing a
// client process uses on the server's stdin and stdout. It returns the session and a
// function that ends the client and returns serve's error.
func stdioClient(t *testing.T, args []string) (*sdkmcp.ClientSession, func() error) {
	t.Helper()
	ctx := testContext(t)
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, args, serverIn, serverOut) }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := client.Connect(ctx, &sdkmcp.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
	if err != nil {
		t.Fatalf("client Connect() = %v", err)
	}
	return cs, func() error {
		if err := cs.Close(); err != nil {
			return fmt.Errorf("client Close: %w", err)
		}
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func structured(t *testing.T, res *sdkmcp.CallToolResult, out any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error %+v, want a result", res.Content)
	}
	body, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

func TestServeReadsBackOverStdio(t *testing.T) {
	cfgPath := writeConfig(t)
	recordUnit(t, cfgPath)
	cs, stop := stdioClient(t, []string{"--config=" + cfgPath})
	ctx := testContext(t)

	res, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: mcp.ToolUnitStatus, Arguments: map[string]any{"id": unitID}})
	if err != nil {
		t.Fatalf("CallTool(unit_status) = %v", err)
	}
	var unit mcp.UnitStatusOut
	structured(t, res, &unit)
	if unit.ID != unitID || unit.Status != "implementing" || unit.Branch != "feat/35-mcp-readonly" {
		t.Fatalf("unit_status = %+v, want the recorded unit", unit)
	}

	res, err = cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: mcp.ToolJobsStatus, Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool(jobs_status) = %v", err)
	}
	checkJobs(t, res)

	if err := stop(); err != nil {
		t.Fatalf("serve after client close = %v, want nil", err)
	}
}

// checkJobs reads the configured job back where the platform has a supervisor, and the
// reason where it has none.
func checkJobs(t *testing.T, res *sdkmcp.CallToolResult) {
	t.Helper()
	if runtime.GOOS != "linux" {
		if !res.IsError {
			t.Fatalf("jobs_status on %s = %+v, want the not-supported tool error", runtime.GOOS, res)
		}
		return
	}
	var jobs mcp.JobsStatusOut
	structured(t, res, &jobs)
	if len(jobs.Jobs) != 1 || jobs.Jobs[0].Name != "idle" || jobs.Jobs[0].State != "dead" || jobs.Jobs[0].Sandbox != "off: server test" {
		t.Fatalf("jobs_status = %+v, want the never-started idle job dead with its sandbox", jobs)
	}
}

func TestServeRefusesBadArguments(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no config", nil, "--config is required"},
		{"extra argument", []string{"--config=" + missing, "extra"}, `unexpected argument "extra"`},
		{"unknown flag", []string{"--listen=:1"}, "flag provided but not defined"},
		{"absent config", []string{"--config=" + missing}, "absent.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := load(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("load(%q) = %v, want an error containing %q", tc.args, err, tc.want)
			}
		})
	}
}

func TestServeEndsWithContext(t *testing.T) {
	cfgPath := writeConfig(t)
	ctx, cancel := context.WithCancel(testContext(t))
	serverIn, clientOut := io.Pipe()
	_, serverOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, []string{"--config=" + cfgPath}, serverIn, serverOut) }()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("serve after cancel = %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not return after its context ended")
	}
	if err := clientOut.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
}

func TestOpenJobsFallsBackOnlyWhenNotSupported(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantStart bool
	}{
		{"not supported", fmt.Errorf("%w: no flock", supervisor.ErrNotSupported), true},
		{"other error", errors.New("supervisor: job \"idle\": bad stop signal"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := newSupervisor
			t.Cleanup(func() { newSupervisor = orig })
			newSupervisor = func(config.Config, supervisor.Options) (*supervisor.Supervisor, error) { return nil, tc.err }
			jobs, err := openJobs(config.Default())
			if !tc.wantStart {
				if err == nil || !errors.Is(err, tc.err) {
					t.Fatalf("openJobs() = %v, %v; want the error %v", jobs, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("openJobs() = %v, want nil", err)
			}
			assertNotSupported(t, jobs)
		})
	}
}

// assertNotSupported checks that every job call answers with ErrNotSupported.
func assertNotSupported(t *testing.T, jobs jobService) {
	t.Helper()
	if _, statusErr := jobs.Status(testContext(t), ""); !errors.Is(statusErr, supervisor.ErrNotSupported) {
		t.Fatalf("Status() = %v, want ErrNotSupported", statusErr)
	}
	if startErr, stopErr := jobs.Start(testContext(t), "idle"), jobs.Stop(testContext(t), "idle"); !errors.Is(startErr, supervisor.ErrNotSupported) || !errors.Is(stopErr, supervisor.ErrNotSupported) {
		t.Fatalf("Start(), Stop() = %v, %v; want ErrNotSupported", startErr, stopErr)
	}
}

// serveHTTPConfig runs serve with transport http on a free loopback port and returns the
// base address it prints, the cancel that ends it, and serve's result.
func serveHTTPConfig(t *testing.T, cfgPath string) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(testContext(t))
	t.Cleanup(cancel)
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := serve(ctx, []string{"--config=" + cfgPath}, io.NopCloser(strings.NewReader("")), outW)
		outW.CloseWithError(fmt.Errorf("serve returned %w", err))
		done <- err
	}()
	lines := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(outR).ReadString('\n')
		if err != nil {
			line = err.Error()
		}
		lines <- line
	}()
	var line string
	select {
	case line = <-lines:
	case <-time.After(30 * time.Second):
		t.Fatal("serve printed no address")
	}
	endpoint := strings.TrimSpace(strings.TrimPrefix(line, "epitrop-server: serving "))
	if !strings.HasPrefix(endpoint, "http://127.0.0.1:") || !strings.HasSuffix(endpoint, mcp.PathMCP) {
		t.Fatalf("served address line %q, want http://127.0.0.1:<port>%s", line, mcp.PathMCP)
	}
	return strings.TrimSuffix(endpoint, mcp.PathMCP), cancel, done
}

func waitReady(t *testing.T, base string) {
	t.Helper()
	for i := 0; i < 300; i++ {
		req, err := http.NewRequestWithContext(testContext(t), http.MethodGet, base+"/readyz", nil)
		if err != nil {
			t.Fatalf("NewRequest = %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /readyz = %v", err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("Close = %v", err)
		}
		if resp.StatusCode == http.StatusOK {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("/readyz never passed")
}

func TestServeHTTPReadsBackAndStops(t *testing.T) {
	cfgPath := writeConfigServing(t, `{"transport":"http","listen":"127.0.0.1:0"}`)
	recordUnit(t, cfgPath)
	base, cancel, done := serveHTTPConfig(t, cfgPath)
	waitReady(t, base)

	readBackOverHTTP(t, base)

	// An open event stream must not hold the shutdown.
	streamReq, err := http.NewRequestWithContext(testContext(t), http.MethodGet, base+mcp.PathEvents, nil)
	if err != nil {
		t.Fatalf("NewRequest = %v", err)
	}
	stream, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("GET %s = %v", mcp.PathEvents, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve after cancel = %v, want nil", err)
		}
	case <-time.After(shutdownTimeout / 2):
		t.Fatal("serve did not stop with an event stream open")
	}
	if err := stream.Body.Close(); err != nil {
		t.Fatalf("Close(stream) = %v", err)
	}
}

// silentConnStopWait is below the five seconds net/http's Shutdown waits for a connection
// that has sent no request (#247), so the test tells the fix from that grace.
const silentConnStopWait = 4 * time.Second

func TestServeHTTPStopsWithASilentConnection(t *testing.T) {
	cfgPath := writeConfigServing(t, `{"transport":"http","listen":"127.0.0.1:0"}`)
	recordUnit(t, cfgPath)
	base, cancel, done := serveHTTPConfig(t, cfgPath)
	waitReady(t, base)

	// A connection that sends nothing must not hold the stop.
	var d net.Dialer
	conn, err := d.DialContext(testContext(t), "tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatalf("Dial = %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("Close(conn) = %v", err)
		}
	})
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve after cancel = %v, want nil", err)
		}
	case <-time.After(silentConnStopWait):
		t.Fatalf("serve did not stop within %s with a connection that sent no request", silentConnStopWait)
	}
	t.Logf("serve stopped %s after cancel", time.Since(start).Round(time.Millisecond))
	if err := conn.SetReadDeadline(time.Now().Add(silentConnStopWait)); err != nil {
		t.Fatalf("SetReadDeadline = %v", err)
	}
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("Read(silent conn) after stop = %v, want io.EOF: the server must close it", err)
	}
}

func readBackOverHTTP(t *testing.T, base string) {
	t.Helper()
	ctx := testContext(t)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: base + mcp.PathMCP}, nil)
	if err != nil {
		t.Fatalf("Connect() = %v", err)
	}
	res, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: mcp.ToolUnitStatus, Arguments: map[string]any{"id": unitID}})
	if err != nil {
		t.Fatalf("CallTool(unit_status) = %v", err)
	}
	var unit mcp.UnitStatusOut
	structured(t, res, &unit)
	if unit.ID != unitID || unit.Status != "implementing" {
		t.Fatalf("unit_status over HTTP = %+v, want the recorded unit", unit)
	}
	if err := cs.Close(); err != nil {
		t.Fatalf("client Close = %v", err)
	}

}

func TestServeHTTPRefusesABusyAddress(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(testContext(t), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen = %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("Close = %v", err)
		}
	})
	cfgPath := writeConfigServing(t, fmt.Sprintf(`{"transport":"http","listen":%s}`, jsonQuote(ln.Addr().String())))
	err = serve(testContext(t), []string{"--config=" + cfgPath}, io.NopCloser(strings.NewReader("")), nopWriteCloser{io.Discard})
	// The bind error's text differs between platforms; its operation does not.
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "listen" {
		t.Fatalf("serve on a busy address = %v, want the listen error", err)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestHelperRunsTheShimAndTheRelay(t *testing.T) {
	for _, args := range [][]string{{jobShimCommand}, {egressproxy.RelayCommand}} {
		code, ok := helper(args)
		if !ok || code == 0 {
			t.Fatalf("helper(%v) = %d, %v; want the helper run and refuse its missing flags", args, code, ok)
		}
	}
	for _, args := range [][]string{nil, {"--config=x"}, {"__job-shim-extra"}} {
		if _, ok := helper(args); ok {
			t.Fatalf("helper(%v) ran a helper, want serve", args)
		}
	}
}

// toolsOf lists the tool names a session serves.
func toolsOf(t *testing.T, cs *sdkmcp.ClientSession) map[string]bool {
	t.Helper()
	res, err := cs.ListTools(testContext(t), nil)
	if err != nil {
		t.Fatalf("ListTools() = %v", err)
	}
	out := make(map[string]bool, len(res.Tools))
	for i := 0; i < len(res.Tools); i++ {
		out[res.Tools[i].Name] = true
	}
	return out
}

func TestServeMutatesOnlyWhenAllowed(t *testing.T) {
	for _, allow := range []bool{false, true} {
		cs, stop := stdioClient(t, []string{"--config=" + writeConfigServing(t, fmt.Sprintf(`{"allow_mutations":%v}`, allow))})
		if got := toolsOf(t, cs)[mcp.ToolUnitSet]; got != allow {
			t.Fatalf("allow_mutations %v: unit_set listed = %v", allow, got)
		}
		if err := stop(); err != nil {
			t.Fatalf("serve after client close = %v, want nil", err)
		}
	}
}

func TestServeUnitSetRunsOncePerKey(t *testing.T) {
	cs, stop := stdioClient(t, []string{"--config=" + writeConfigServing(t, `{"allow_mutations":true}`)})
	args := map[string]any{"key": "req-1", "id": unitID, "status": "implementing"}
	for _, want := range []mcp.MutationOut{{Executed: true}, {Skipped: true}} {
		res, err := cs.CallTool(testContext(t), &sdkmcp.CallToolParams{Name: mcp.ToolUnitSet, Arguments: args})
		if err != nil {
			t.Fatalf("CallTool(unit_set) = %v", err)
		}
		var got mcp.MutationOut
		structured(t, res, &got)
		if got != want {
			t.Fatalf("unit_set = %+v, want %+v", got, want)
		}
	}
	res, err := cs.CallTool(testContext(t), &sdkmcp.CallToolParams{Name: mcp.ToolUnitStatus, Arguments: map[string]any{"id": unitID}})
	if err != nil {
		t.Fatalf("CallTool(unit_status) = %v", err)
	}
	var unit mcp.UnitStatusOut
	structured(t, res, &unit)
	if unit.Status != "implementing" {
		t.Fatalf("unit_status = %+v, want implementing", unit)
	}
	if err := stop(); err != nil {
		t.Fatalf("serve after client close = %v, want nil", err)
	}
}
