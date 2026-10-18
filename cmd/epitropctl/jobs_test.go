package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golusoris/golusoris/core/clock"
	"github.com/jonboulle/clockwork"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/supervisor"
)

func TestHelperProcessShim(t *testing.T) {
	if os.Getenv("EPITROP_TEST_SHIM") != "1" {
		return
	}
	os.Exit(runJobShim(argsAfterDash(os.Args)))
}

func TestRunJobsRejectsMissingAndUnknownCommand(t *testing.T) {
	if err := runJobs(nil); err == nil || !strings.Contains(err.Error(), "jobs command required") {
		t.Fatalf("runJobs(nil) = %v, want command required", err)
	}
	if err := runJobs([]string{"bogus"}); err == nil || !strings.Contains(err.Error(), "unknown jobs command: bogus") {
		t.Fatalf("runJobs(bogus) = %v, want unknown command", err)
	}
}

func TestRunJobsRequiresConfig(t *testing.T) {
	err := runJobs([]string{"status"})
	if err == nil || !strings.Contains(err.Error(), "jobs status: --config is required") {
		t.Fatalf("runJobs(status) = %v, want config required", err)
	}
}

func TestRunJobsAcceptsAllKnownCommands(t *testing.T) {
	for _, action := range []string{"start", "stop", "supervise", "history"} {
		t.Run(action, func(t *testing.T) {
			err := runJobs([]string{action})
			if err == nil || !strings.Contains(err.Error(), "--config is required") {
				t.Fatalf("runJobs(%s) = %v, want config-required action path", action, err)
			}
		})
	}
}

func TestRunJobsActionRejectsParseNameLoadAndNewErrors(t *testing.T) {
	if err := runJobsAction("status", []string{"-bad"}); err == nil || !strings.Contains(err.Error(), "flag provided") {
		t.Fatalf("runJobsAction(bad flag) = %v, want parse error", err)
	}
	cfgPath := writeJobsConfig(t, "missing-key")
	if err := runJobsAction("status", []string{"--config", cfgPath, "one", "two"}); err == nil || !strings.Contains(err.Error(), "expected at most one name") {
		t.Fatalf("runJobsAction(two names) = %v, want name error", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	if err := runJobsAction("status", []string{"--config", missing}); err == nil || !strings.Contains(err.Error(), "stat") {
		t.Fatalf("runJobsAction(missing config) = %v, want stat error", err)
	}
	if err := runJobsAction("status", []string{"--config", cfgPath}); err == nil {
		t.Fatal("runJobsAction(missing key) = nil, want New signer error")
	}
}

func TestRunJobsActionStatusWithLoadedConfig(t *testing.T) {
	cfgPath := writeJobsConfig(t, "valid-key")
	if runtime.GOOS != "linux" {
		// The supervisor runs on Linux only; elsewhere status must say so instead of printing rows.
		if err := runJobsAction("status", []string{"--config", cfgPath, "idle"}); !errors.Is(err, supervisor.ErrNotSupported) {
			t.Fatalf("runJobsAction(status loaded) on %s = %v, want supervisor.ErrNotSupported", runtime.GOOS, err)
		}
		return
	}
	out := captureStdout(t, func() {
		if err := runJobsAction("status", []string{"--config", cfgPath, "idle"}); err != nil {
			t.Fatalf("runJobsAction(status loaded) = %v, want nil", err)
		}
	})
	if !strings.HasPrefix(out, "idle\tdead\t0\t\t0\t") {
		t.Fatalf("status output = %q, want idle row", out)
	}
}

func TestRunJobsActionPassesPollInterval(t *testing.T) {
	cfgPath := writeJobsConfig(t, "valid-key")
	wantErr := errors.New("captured supervisor options")
	var got time.Duration
	old := newJobsSupervisor
	newJobsSupervisor = func(cfg config.Config, opts supervisor.Options) (*supervisor.Supervisor, error) {
		got = opts.PollInterval
		return nil, wantErr
	}
	t.Cleanup(func() { newJobsSupervisor = old })
	err := runJobsAction("status", []string{"--config", cfgPath, "--poll-interval-seconds", "7"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("runJobsAction(poll capture) = %v, want sentinel", err)
	}
	if got != 7*time.Second {
		t.Fatalf("PollInterval = %s, want 7s", got)
	}
}

// Supervise runs until stopped: its context outlives the 30 second bound of the other jobs
// commands, which once ended every supervise after 30 seconds with a deadline error.
func TestJobsActionContextLetsSuperviseOutliveTheCommandTimeout(t *testing.T) {
	ctx, cancel := jobsActionContext("supervise")
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) < 364*24*time.Hour {
		t.Fatalf("jobsActionContext(supervise) deadline = %v (set %v), want about %s", deadline, ok, maxSuperviseRuntime)
	}
	cancel()
	if ctx.Err() == nil {
		t.Fatal("jobsActionContext(supervise) after cancel: Err() = nil, want canceled")
	}
	for _, action := range []string{"start", "status", "stop"} {
		actionCtx, actionCancel := jobsActionContext(action)
		deadline, ok = actionCtx.Deadline()
		actionCancel()
		if !ok || time.Until(deadline) > jobsCommandTimeout {
			t.Fatalf("jobsActionContext(%s) deadline = %v (set %v), want within %s", action, deadline, ok, jobsCommandTimeout)
		}
	}
}

func TestJobsNameArgBounds(t *testing.T) {
	if name, err := jobsNameArg("supervise", nil); err != nil || name != "" {
		t.Fatalf("jobsNameArg(supervise) = %q, %v, want empty nil", name, err)
	}
	if name, err := jobsNameArg("status", []string{"one"}); err != nil || name != "one" {
		t.Fatalf("jobsNameArg(status one) = %q, %v, want one nil", name, err)
	}
	if _, err := jobsNameArg("start", []string{"one", "two"}); err == nil {
		t.Fatal("jobsNameArg(two names) = nil error, want bounds failure")
	}
}

func TestPrintJobsStatusWritesTabRows(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("status formatting test needs Linux supervisor support")
	}
	sup := testJobsSupervisor(t)
	out := captureStdout(t, func() {
		if err := printJobsStatus(testContext(t), sup, "idle"); err != nil {
			t.Fatalf("printJobsStatus() = %v, want nil", err)
		}
	})
	if !strings.HasPrefix(out, "idle\tdead\t0\t\t0\t") {
		t.Fatalf("status output = %q, want tab row", out)
	}
}

func TestRunLoadedJobsActionDispatchesStatus(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("status dispatch test needs Linux supervisor support")
	}
	sup := testJobsSupervisor(t)
	out := captureStdout(t, func() {
		if err := runLoadedJobsAction(testContext(t), sup, "status", "idle"); err != nil {
			t.Fatalf("runLoadedJobsAction(status) = %v, want nil", err)
		}
	})
	if !strings.HasPrefix(out, "idle\tdead\t0\t\t0\t") {
		t.Fatalf("status output = %q, want tab row", out)
	}
}

func TestRunLoadedJobsActionDispatchesStartStopSuperviseAndDefault(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("dispatch test needs Linux supervisor support")
	}
	sup, base := testJobsSupervisorIn(t)
	if err := runLoadedJobsAction(testContext(t), sup, "start", "idle"); err != nil {
		t.Fatalf("runLoadedJobsAction(start) = %v, want nil", err)
	}
	if st, err := sup.Status(testContext(t), "idle"); err != nil || st[0].State != supervisor.StateRunning {
		t.Fatalf("Status(after start) = %+v, %v, want running", st, err)
	}
	if err := runLoadedJobsAction(testContext(t), sup, "stop", "idle"); err != nil {
		logJobState(t, sup, base, "idle")
		t.Fatalf("runLoadedJobsAction(stop) = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(testContext(t))
	cancel()
	if err := runLoadedJobsAction(ctx, sup, "supervise", ""); err != nil {
		t.Fatalf("runLoadedJobsAction(supervise stopped) = %v, want nil: a stop signal is the normal end", err)
	}
	expired, cancelExpired := context.WithDeadline(testContext(t), time.Now().Add(-time.Second))
	defer cancelExpired()
	if err := runLoadedJobsAction(expired, sup, "supervise", ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runLoadedJobsAction(supervise past its bound) = %v, want context.DeadlineExceeded", err)
	}
	if err := runLoadedJobsAction(testContext(t), sup, "bogus", ""); err == nil {
		t.Fatal("runLoadedJobsAction(bogus) = nil, want unknown command")
	}
}

func TestPrintJobsStatusReturnsSupervisorError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("status error test needs Linux supervisor support")
	}
	sup := testJobsSupervisor(t)
	err := printJobsStatus(testContext(t), sup, "missing")
	if err == nil || !strings.Contains(err.Error(), "unknown job") {
		t.Fatalf("printJobsStatus(missing) = %v, want unknown job", err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() = %v, want nil", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = old
	}()
	fn()
	if err = writer.Close(); err != nil {
		t.Fatalf("Close(writer) = %v, want nil", err)
	}
	var buf bytes.Buffer
	if _, err = io.Copy(&buf, reader); err != nil {
		t.Fatalf("Copy(stdout) = %v, want nil", err)
	}
	if err = reader.Close(); err != nil {
		t.Fatalf("Close(reader) = %v, want nil", err)
	}
	return buf.String()
}

func testJobsSupervisor(t *testing.T) *supervisor.Supervisor {
	t.Helper()
	sup, _ := testJobsSupervisorIn(t)
	return sup
}

// testJobsSupervisorIn is testJobsSupervisor that also returns the folder holding the job's
// state and log.
func testJobsSupervisorIn(t *testing.T) (*supervisor.Supervisor, string) {
	t.Helper()
	base := jobsTempDir(t)
	keyPath := filepath.Join(base, "seed.hex")
	seed := bytes.Repeat([]byte{4}, 32)
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
		t.Fatalf("WriteFile(key) = %v, want nil", err)
	}
	cfg := config.Default()
	cfg.EventLog.Dir = filepath.Join(base, "state")
	cfg.EventLog.SigningKeyPath = keyPath
	cfg.Jobs = []config.JobConfig{{
		Name:     "idle",
		Command:  []string{"sleep", "30"},
		LogPath:  filepath.Join(base, "idle.log"),
		Schedule: "always",
		Sandbox:  config.SandboxConfig{Mode: "off", Reason: "command dispatch unit test"},
	}}
	if err := os.MkdirAll(filepath.Join(cfg.EventLog.Dir, ".git"), 0o700); err != nil {
		t.Fatalf("MkdirAll(state/.git) = %v, want nil", err)
	}
	sup, err := supervisor.New(cfg, supervisor.Options{
		Clock:       clock.NewFake(),
		ShimCommand: []string{os.Args[0], "-test.run=TestHelperProcessShim", "--"},
	})
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	t.Setenv("EPITROP_TEST_SHIM", "1")
	return sup, base
}

func writeJobsConfig(t *testing.T, keyMode string) string {
	t.Helper()
	base := jobsTempDir(t)
	keyPath := filepath.Join(base, "seed.hex")
	if keyMode == "valid-key" {
		seed := bytes.Repeat([]byte{5}, 32)
		if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
			t.Fatalf("WriteFile(key) = %v, want nil", err)
		}
	}
	path := filepath.Join(base, "config.json")
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(filepath.Join(stateDir, ".git"), 0o700); err != nil {
		t.Fatalf("MkdirAll(state/.git) = %v, want nil", err)
	}
	body := `{"event_log":{"dir":` + jsonQuote(stateDir) + `,"signing_key_path":` + jsonQuote(keyPath) + `},"jobs":[{"name":"idle","command":["/bin/true"],"log_path":` + jsonQuote(filepath.Join(base, "idle.log")) + `,"sandbox":{"mode":"off","reason":"jobs cli unit test"}}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(config) = %v, want nil", err)
	}
	return path
}

// jsonQuote returns the JSON-encoded string representation of s, ensuring
// backslashes in Windows paths are properly escaped.
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func jobsTempDir(t *testing.T) string {
	t.Helper()
	root := os.Getenv("GOCACHE")
	if root == "" {
		root = t.TempDir()
	}
	base, err := os.MkdirTemp(root, "epitrop-jobs-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp(%s) = %v, want nil", root, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Fatalf("RemoveAll(%s) = %v, want nil", base, err)
		}
	})
	return base
}

func argsAfterDash(args []string) []string {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return args[i+1:]
		}
	}
	return args
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// logJobState logs what a failed stop needs explained: the job's status, the record its shim
// wrote, the process state of the shim and of the job, and the job's log.
func logJobState(t *testing.T, sup *supervisor.Supervisor, base, name string) {
	t.Helper()
	st, err := sup.Status(testContext(t), name)
	t.Logf("status: %+v, %v", st, err)
	body, err := os.ReadFile(filepath.Join(base, "state", "jobs", name+".json"))
	t.Logf("record: %q, %v", body, err)
	var rec struct {
		PID     int `json:"pid"`
		ShimPID int `json:"shim_pid"`
	}
	if json.Unmarshal(body, &rec) == nil {
		for _, pid := range []int{rec.ShimPID, rec.PID} {
			stat, statErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			t.Logf("pid %d: %q, %v", pid, stat, statErr)
		}
	}
	logBody, err := os.ReadFile(filepath.Join(base, name+".log"))
	t.Logf("job log: %q, %v", logBody, err)
}

func TestJobsStatusLineNamesAStaleHeartbeat(t *testing.T) {
	st := supervisor.JobStatus{Name: "hung", State: supervisor.StateRunning, PID: 42, Since: "2026-10-17T10:00:00Z", Sandbox: "enforce", LastBeat: "2026-10-17T10:00:05Z"}
	if got, want := jobsStatusLine(st), "hung\trunning\t42\t2026-10-17T10:00:00Z\t0\tenforce"; got != want {
		t.Fatalf("jobsStatusLine(fresh) = %q, want %q", got, want)
	}
	st.Stale = true
	if got, want := jobsStatusLine(st), "hung\trunning\t42\t2026-10-17T10:00:00Z\t0\tenforce\tstale since 2026-10-17T10:00:05Z"; got != want {
		t.Fatalf("jobsStatusLine(stale) = %q, want %q", got, want)
	}
}

func TestRunJobsActionHistoryWithLoadedConfig(t *testing.T) {
	cfgPath := writeJobsConfig(t, "valid-key")
	args := []string{"--config", cfgPath, "--since", "2026-10-17T14:13:00Z", "idle"}
	if runtime.GOOS != "linux" {
		if err := runJobsAction("history", args); !errors.Is(err, supervisor.ErrNotSupported) {
			t.Fatalf("runJobsAction(history loaded) on %s = %v, want supervisor.ErrNotSupported", runtime.GOOS, err)
		}
		return
	}
	out := captureStdout(t, func() {
		if err := runJobsAction("history", args); err != nil {
			t.Fatalf("runJobsAction(history loaded) = %v, want nil", err)
		}
	})
	if out != "idle\t0\t0\t0\t0\t0\t0\t0\t\t\n" {
		t.Fatalf("history output = %q, want an idle row with nothing counted", out)
	}
}

func TestRunJobsActionHistoryCountsFromSince(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("history test needs Linux supervisor support")
	}
	cfgPath := writeJobsConfig(t, "valid-key")
	appendJobsRecord(t, cfgPath, "job.started", "idle")
	for _, tc := range []struct{ since, want string }{
		{"2000-01-01T00:00:00Z", "idle\t1\t"},
		{"2999-01-01T00:00:00Z", "idle\t0\t"},
	} {
		out := captureStdout(t, func() {
			if err := runJobsAction("history", []string{"--config", cfgPath, "--since", tc.since}); err != nil {
				t.Fatalf("runJobsAction(history --since %s) = %v, want nil", tc.since, err)
			}
		})
		if !strings.HasPrefix(out, tc.want) {
			t.Fatalf("history --since %s = %q, want prefix %q", tc.since, out, tc.want)
		}
	}
}

// appendJobsRecord signs one record of type typ for job name into the event log of the
// configuration at cfgPath.
func appendJobsRecord(t *testing.T, cfgPath string, typ string, name string) {
	t.Helper()
	cfg, err := config.Load(testContext(t), cfgPath)
	if err != nil {
		t.Fatalf("config.Load() = %v, want nil", err)
	}
	clk := clockwork.NewRealClock()
	signer, err := eventlog.NewSignerFromKeyFile(testContext(t), cfg.EventLog.SigningKeyPath, cfg.EventLog.Dir, clk)
	if err != nil {
		t.Fatalf("NewSignerFromKeyFile() = %v, want nil", err)
	}
	w, err := eventlog.Open(cfg.EventLog.Dir, signer, eventlog.Limits{Clock: clk})
	if err != nil {
		t.Fatalf("eventlog.Open() = %v, want nil", err)
	}
	if _, err = w.Append(testContext(t), eventlog.Event{Type: typ, TaskID: name, Payload: json.RawMessage(`{"state":"running"}`)}); err != nil {
		t.Fatalf("Append(%s) = %v, want nil", typ, err)
	}
}

func TestRunJobsActionRefusesABadSince(t *testing.T) {
	cfgPath := writeJobsConfig(t, "valid-key")
	err := runJobsAction("history", []string{"--config", cfgPath, "--since", "yesterday"})
	if err == nil || !strings.Contains(err.Error(), "jobs history: --since") {
		t.Fatalf("runJobsAction(history bad since) = %v, want a --since parse error", err)
	}
	err = runJobsAction("status", []string{"--config", cfgPath, "--since", "2026-10-17T14:13:00Z"})
	if err == nil || !strings.Contains(err.Error(), "jobs status: --since is a jobs history flag") {
		t.Fatalf("runJobsAction(status --since) = %v, want the flag refused", err)
	}
}

func TestJobsHistoryLineOrdersItsColumns(t *testing.T) {
	h := supervisor.JobHistory{Name: "d", Started: 1, Exited: 2, Unclean: 3, Restarted: 4, Stale: 5, Refused: 6, Stopped: 7, First: "f", Last: "l"}
	if got, want := jobsHistoryLine(h), "d\t1\t2\t3\t4\t5\t6\t7\tf\tl"; got != want {
		t.Fatalf("jobsHistoryLine() = %q, want %q", got, want)
	}
}

func TestPrintJobsHistoryRefusesAnUnknownJob(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("history test needs Linux supervisor support")
	}
	sup := testJobsSupervisor(t)
	if err := printJobsHistory(testContext(t), sup, "missing", time.Time{}); !errors.Is(err, supervisor.ErrUnknownJob) {
		t.Fatalf("printJobsHistory(missing) = %v, want supervisor.ErrUnknownJob", err)
	}
}
