package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/egressproxy"
	"github.com/tekmora/epitrop/internal/eventlog"
)

type shimConfig struct {
	EventLogDir string
	// EventLogLockTimeoutSeconds is the configured wait for the event log's lock, 0 for the
	// log's own default.
	EventLogLockTimeoutSeconds int
	SigningKey                 string
	Name                       string
	LockPath                   string
	RecordPath                 string
	LogPath                    string
	Command                    []string
	Sandbox                    config.SandboxConfig
	// MaxRuntimeSeconds is the job's deadline, 0 for the longest any job may run. At the
	// deadline the job gets StopSignal, then StopGraceSeconds to exit, then the kill.
	MaxRuntimeSeconds int
	StopSignal        string
	StopGraceSeconds  int
}

func RunShim(args []string) int {
	cfg, err := parseShimArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 2
	}
	if err = runShim(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

func parseShimArgs(args []string) (shimConfig, error) {
	if len(args) > 0 && args[0] == "__job-shim" {
		args = args[1:]
	}
	var cfg shimConfig
	fs := flag.NewFlagSet("job-shim", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.EventLogDir, "event-log-dir", "", "")
	fs.IntVar(&cfg.EventLogLockTimeoutSeconds, "event-log-lock-timeout-seconds", 0, "")
	fs.StringVar(&cfg.SigningKey, "signing-key", "", "")
	fs.StringVar(&cfg.Name, "name", "", "")
	fs.StringVar(&cfg.LockPath, "lock", "", "")
	fs.StringVar(&cfg.RecordPath, "record", "", "")
	fs.StringVar(&cfg.LogPath, "log", "", "")
	fs.IntVar(&cfg.MaxRuntimeSeconds, "max-runtime-seconds", 0, "")
	fs.StringVar(&cfg.StopSignal, "stop-signal", "TERM", "")
	fs.IntVar(&cfg.StopGraceSeconds, "stop-grace-seconds", 0, "")
	fs.StringVar(&cfg.Sandbox.Mode, "sandbox-mode", "", "")
	fs.StringVar(&cfg.Sandbox.Reason, "sandbox-reason", "", "")
	fs.StringVar(&cfg.Sandbox.Workspace, "sandbox-workspace", "", "")
	fs.Var((*stringListFlag)(&cfg.Sandbox.Inputs), "sandbox-input", "")
	fs.Var((*stringListFlag)(&cfg.Sandbox.ReadOnly), "sandbox-read-only", "")
	fs.Var((*stringListFlag)(&cfg.Sandbox.EnvAllow), "sandbox-env-allow", "")
	fs.StringVar(&cfg.Sandbox.Network, "sandbox-network", "", "")
	fs.Var((*stringListFlag)(&cfg.Sandbox.Destinations), "sandbox-destination", "")
	fs.StringVar(&cfg.Sandbox.MemoryMax, "sandbox-memory-max", "", "")
	fs.StringVar(&cfg.Sandbox.MemoryHigh, "sandbox-memory-high", "", "")
	fs.IntVar(&cfg.Sandbox.CPUWeight, "sandbox-cpu-weight", 0, "")
	fs.IntVar(&cfg.Sandbox.TasksMax, "sandbox-tasks-max", 0, "")
	var home homeFlags
	fs.StringVar(&home.source, "sandbox-home-source", "", "")
	fs.StringVar(&home.env, "sandbox-home-env", "", "")
	fs.Var((*stringListFlag)(&home.copies), "sandbox-home-copy", "")
	fs.Var((*stringListFlag)(&home.binds), "sandbox-home-bind", "")
	if err := fs.Parse(args); err != nil {
		return shimConfig{}, fmt.Errorf("job-shim: parse: %w", err)
	}
	var err error
	if cfg.Sandbox.Home, err = home.home(); err != nil {
		return shimConfig{}, err
	}
	cfg.Command = fs.Args()
	if len(cfg.Command) > 0 && cfg.Command[0] == "--" {
		cfg.Command = cfg.Command[1:]
	}
	if err = checkShimConfig(cfg); err != nil {
		return shimConfig{}, err
	}
	return cfg, nil
}

func checkShimConfig(cfg shimConfig) error {
	if cfg.EventLogDir == "" || cfg.SigningKey == "" || cfg.Name == "" {
		return fmt.Errorf("job-shim: event log, signing key and name are required")
	}
	if cfg.LockPath == "" || cfg.RecordPath == "" || cfg.LogPath == "" {
		return fmt.Errorf("job-shim: lock, record and log paths are required")
	}
	if err := checkShimLimits(cfg); err != nil {
		return err
	}
	cfg.Sandbox = config.NormalizeSandbox(cfg.Sandbox)
	if err := config.ValidateSandbox(cfg.Sandbox); err != nil {
		return fmt.Errorf("job-shim: sandbox.%w", err)
	}
	return nil
}

// checkShimLimits checks the command's length, the run's deadline and how the job is stopped.
func checkShimLimits(cfg shimConfig) error {
	if len(cfg.Command) == 0 || len(cfg.Command) > config.MaxJobArgs {
		return fmt.Errorf("job-shim: command must have 1..%d args", config.MaxJobArgs)
	}
	if cfg.MaxRuntimeSeconds < 0 || cfg.MaxRuntimeSeconds > config.MaxJobRuntimeSeconds {
		return fmt.Errorf("job-shim: max runtime must be 0..%d seconds", config.MaxJobRuntimeSeconds)
	}
	if cfg.StopGraceSeconds < 0 || cfg.StopGraceSeconds > config.MaxStopGraceSeconds {
		return fmt.Errorf("job-shim: stop grace must be 0..%d seconds", config.MaxStopGraceSeconds)
	}
	if _, err := signalFromName(cfg.StopSignal); cfg.StopSignal != "" && err != nil {
		return fmt.Errorf("job-shim: stop signal: %w", err)
	}
	return nil
}

func runShim(cfg shimConfig) error {
	lock, err := holdLock(cfg.LockPath)
	if err != nil {
		return err
	}
	defer closeLockForShim(lock)
	logFile, err := openAppendLog(cfg.LogPath)
	if err != nil {
		return err
	}
	defer closeLogForShim(logFile)
	// Linux Pdeathsig is tied to the creating thread, so keep the start thread
	// alive until the child exits or is killed on a shim error path.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	runCtx, cancel := context.WithTimeout(context.Background(), jobDeadline(cfg))
	defer cancel()
	proxy, err := startJobProxy(runCtx, cfg, logFile)
	if err != nil {
		return refuseJob(cfg, logFile, err)
	}
	defer stopProxyForShim(proxy, logFile)
	cmd, started, err := startConfirmedJob(runCtx, cfg, proxy.binds(), logFile)
	if err != nil {
		return refuseJob(cfg, logFile, err)
	}
	writer, err := shimWriter(cfg)
	if err != nil {
		return killAndCollect(cmd, err)
	}
	if err = appendStarted(writer, cfg, cmd.Process.Pid, started); err != nil {
		return killAndCollect(cmd, err)
	}
	return waitAndRecordExit(runCtx, writer, cfg, cmd)
}

// jobDeadline is how long the job may run: its own deadline, or the longest any job may.
func jobDeadline(cfg shimConfig) time.Duration {
	if cfg.MaxRuntimeSeconds > 0 {
		return time.Duration(cfg.MaxRuntimeSeconds) * time.Second
	}
	return maxJobRuntime
}

// stopAtDeadline is what the job's context does when it ends: the job's whole process group
// gets the stop signal, and with no grace period the kill at once. With a grace period the
// command's wait delay follows, and waitAndRecordExit kills what is left of the group.
func stopAtDeadline(cfg shimConfig, pid int) error {
	if cfg.StopGraceSeconds == 0 {
		return killStartedProcessGroup(pid)
	}
	return signalStartedProcessGroup(pid, cfg.StopSignal)
}

// startConfirmedJob builds the job's command, starts it, writes its record and confirms its
// scope. After any error the job is not running: one that had started is killed with its
// process group.
func startConfirmedJob(ctx context.Context, cfg shimConfig, proxy sandboxProxy, logFile *os.File) (*exec.Cmd, string, error) {
	command, err := buildJobCommand(cfg.Command, cfg.Sandbox, shimBuildOptions(cfg, proxy, time.Now()))
	if err != nil {
		return nil, "", fmt.Errorf("job-shim: sandbox %s: %w", cfg.Name, err)
	}
	// #nosec G204 -- job command comes from validated operator config and is
	// executed as argv without a shell.
	cmd := exec.CommandContext(ctx, command.Argv[0], command.Argv[1:]...)
	cmd.Env = command.Env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Cancel = func() error { return stopAtDeadline(cfg, cmd.Process.Pid) }
	cmd.WaitDelay = time.Duration(cfg.StopGraceSeconds) * time.Second
	prepareProcess(cmd)
	if err = cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("job-shim: start %s: %w", cfg.Command[0], err)
	}
	started := time.Now().UTC().Format(time.RFC3339Nano)
	if err = writeRecord(cfg, cmd.Process.Pid, started); err != nil {
		return nil, "", killAndCollect(cmd, err)
	}
	// The record comes first so that a stop during the wait can reach the job; job.started
	// is appended only for a job confirmed inside its scope.
	if err = confirmJobScope(cmd.Process.Pid, command.Scope, command.Argv[0]); err != nil {
		return nil, "", killAndCollect(cmd, err)
	}
	return cmd, started, nil
}

// shimBuildOptions are the options the shim builds a job's command with, for a start at now.
func shimBuildOptions(cfg shimConfig, proxy sandboxProxy, now time.Time) sandboxBuildOptions {
	return sandboxBuildOptions{
		Paths: sandboxPaths{EventLogDir: cfg.EventLogDir, SigningKey: cfg.SigningKey},
		Env:   os.LookupEnv,
		Proxy: proxy,
		Until: latestEnd(cfg, now),
	}
}

// latestEnd is the latest time a job started at now can still run: its deadline, or the
// longest any job may run, plus its stop grace.
func latestEnd(cfg shimConfig, now time.Time) time.Time {
	runtime := cfg.MaxRuntimeSeconds
	if runtime == 0 {
		runtime = config.MaxJobRuntimeSeconds
	}
	return now.Add(time.Duration(runtime+cfg.StopGraceSeconds) * time.Second)
}

func openAppendLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("job-shim: mkdir log dir %s: %w", filepath.Dir(path), err)
	}
	// #nosec G304 -- log path comes from validated operator config for this job.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("job-shim: open log %s: %w", path, err)
	}
	return f, nil
}

func writeRecord(cfg shimConfig, pid int, started string) error {
	if err := os.MkdirAll(filepath.Dir(cfg.RecordPath), 0o750); err != nil {
		return fmt.Errorf("job-shim: mkdir state dir: %w", err)
	}
	rec := jobRecord{Name: cfg.Name, PID: pid, ShimPID: os.Getpid(), StartedAt: started, LogPath: cfg.LogPath}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("job-shim: marshal record: %w", err)
	}
	// #nosec G304 -- record path is derived from configured state dir plus checked job name.
	return os.WriteFile(cfg.RecordPath, append(body, '\n'), 0o600)
}

func shimWriter(cfg shimConfig) (*eventlog.Writer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clk := clockwork.NewRealClock()
	signer, err := eventlog.NewSignerFromKeyFile(ctx, cfg.SigningKey, cfg.EventLogDir, clk)
	if err != nil {
		return nil, err
	}
	return eventlog.Open(cfg.EventLogDir, signer, shimEventLogLimits(cfg, clk))
}

// shimEventLogLimits are the limits under which the shim appends its job's events: the lock
// timeout its supervisor was configured with.
func shimEventLogLimits(cfg shimConfig, clk clockwork.Clock) eventlog.Limits {
	return eventlog.Limits{Clock: clk, LockTimeout: time.Duration(cfg.EventLogLockTimeoutSeconds) * time.Second}
}

func appendStarted(writer *eventlog.Writer, cfg shimConfig, pid int, started string) error {
	payload := map[string]any{"state": string(StateRunning), "pid": pid, "shim_pid": os.Getpid(), "since": started, "sandbox": sandboxStatus(cfg.Sandbox)}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("job-shim: marshal started: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = writer.Append(ctx, eventlog.Event{Type: "job.started", TaskID: cfg.Name, Payload: body})
	return err
}

type stringListFlag []string

func (flag *stringListFlag) String() string {
	return strings.Join(*flag, ",")
}

func (flag *stringListFlag) Set(value string) error {
	*flag = append(*flag, value)
	return nil
}

// waitAndRecordExit waits for the job and records how it ended. A job that was still
// running at its deadline is recorded with reason "deadline", and whatever is left of its
// process group is killed: the stop signal may have ended only the first process, and the
// wait delay kills only that one. The kill comes before the first process is collected.
// Once collected, its process id no longer names the group for certain, and the shim signals
// no group it cannot check.
func waitAndRecordExit(ctx context.Context, writer *eventlog.Writer, cfg shimConfig, cmd *exec.Cmd) error {
	waitErr := awaitExitUncollected(cmd.Process.Pid)
	payload := map[string]any{"state": string(StateDead)}
	var killErr error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		payload["reason"] = "deadline"
		killErr = killStartedProcessGroup(cmd.Process.Pid)
	}
	payload["code"] = collectedExitCode(cmd)
	body, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return errors.Join(fmt.Errorf("job-shim: marshal exited: %w", marshalErr), waitErr, killErr)
	}
	appendCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, appendErr := writer.Append(appendCtx, eventlog.Event{Type: "job.exited", TaskID: cfg.Name, Payload: body}); appendErr != nil {
		return errors.Join(appendErr, waitErr, killErr)
	}
	return errors.Join(waitErr, killErr)
}

// collectedExitCode collects the job and returns its own exit code: the code it exited
// with, or -1 when a signal ended it. The error of Wait is not the source: for a job that
// exits by itself after its deadline, Wait returns the context's error, not the job's.
func collectedExitCode(cmd *exec.Cmd) int {
	err := cmd.Wait()
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	return exitCode(err)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// maxRefusalReasonBytes bounds the reason a job.refused event carries.
const maxRefusalReasonBytes = 2048

// refuseJob records why the shim did not start a job, or killed it before recording a start:
// a line in the job's log, where its own output is read, and a job.refused event, which a
// caller can act on without reading a log. The shim's stderr is not kept.
func refuseJob(cfg shimConfig, logFile io.Writer, cause error) error {
	err := fmt.Errorf("job-shim: %s refused: %w", cfg.Name, cause)
	if _, writeErr := fmt.Fprintf(logFile, "epitrop: %v\n", err); writeErr != nil {
		err = errors.Join(err, fmt.Errorf("job-shim: write refusal to the job log: %w", writeErr))
	}
	if appendErr := appendRefused(cfg, cause); appendErr != nil {
		err = errors.Join(err, fmt.Errorf("job-shim: record refusal: %w", appendErr))
	}
	return err
}

func appendRefused(cfg shimConfig, cause error) error {
	writer, err := shimWriter(cfg)
	if err != nil {
		return err
	}
	reason := cause.Error()
	if len(reason) > maxRefusalReasonBytes {
		reason = reason[:maxRefusalReasonBytes]
	}
	payload := map[string]any{"state": string(StateDead), "shim_pid": os.Getpid(), "at": time.Now().UTC().Format(time.RFC3339Nano), "reason": reason, "sandbox": sandboxStatus(cfg.Sandbox)}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("job-shim: marshal refused: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = writer.Append(ctx, eventlog.Event{Type: "job.refused", TaskID: cfg.Name, Payload: body})
	return err
}

// killAndCollect ends a job the shim started and will not run: the job's process group is
// killed and the job is collected. Collecting also ends the watcher of the job's context,
// which would otherwise outlive this call and stop the job once more when the context ends.
// A job that could not be killed is not waited for.
func killAndCollect(cmd *exec.Cmd, cause error) error {
	if err := killStartedProcessGroup(cmd.Process.Pid); err != nil {
		return errors.Join(cause, err)
	}
	// The exit status of a job that was just killed says nothing; Wait is called to collect.
	var exit *exec.ExitError
	if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
		return errors.Join(cause, fmt.Errorf("job-shim: collect the killed job: %w", err))
	}
	return cause
}

func closeLockForShim(lock *heldLock) {
	if err := lock.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: close job lock: %v\n", err)
	}
}

// stopProxyForShim ends the job's proxy. What went wrong goes to the job's log, which is
// kept; the shim's stderr is not.
func stopProxyForShim(proxy jobProxy, logFile io.Writer) {
	err := proxy.stop()
	if err == nil {
		return
	}
	if _, writeErr := fmt.Fprintf(logFile, "epitrop: stop the egress proxy: %s\n", egressproxy.ErrorLine(err)); writeErr != nil {
		fmt.Fprintf(os.Stderr, "Error: stop the egress proxy: %s; write the job log: %v\n", egressproxy.ErrorLine(err), writeErr)
	}
}

func closeLogForShim(logFile *os.File) {
	if err := logFile.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: close job log: %v\n", err)
	}
}
