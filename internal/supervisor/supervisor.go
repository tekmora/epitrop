package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/golusoris/golusoris/core/clock"
	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
	"github.com/jonboulle/clockwork"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/telemetry"
)

const (
	StateRunning JobRunState = "running"
	StateDead    JobRunState = "dead"
	StateStopped JobRunState = "stopped"

	defaultPollInterval = time.Second
	maxJobRuntime       = 365 * 24 * time.Hour
	maxStatusPolls      = 100
	// A start is awaited for 15 s (10 ms apart), longer than the shim may wait for the
	// job's scope, so a slow start is never reported as failed while it can still succeed.
	maxStartPolls     = 1500
	startPollInterval = 10 * time.Millisecond
	// Shims this supervisor started and has not collected yet. A job's lock allows one live
	// shim per job, so twice the number of jobs a config may hold is never reached in order.
	maxShimWaiters    = 2 * config.MaxJobs
	maxSuperviseTicks = 1 << 30
)

var (
	ErrUnknownJob = errors.New("supervisor: unknown job")
	// ErrRefused marks a start the shim refused; the error text carries the job and the reason.
	ErrRefused      = errors.New("supervisor: job refused")
	ErrNotSupported = errors.New("supervisor: not-supported")
	// ErrRunning marks a StartOnce of a job that runs already.
	ErrRunning = errors.New("supervisor: job is running")
)

type JobRunState string

// RecordedJob is a job as the event log tells it.
type RecordedJob = eventlog.Job

type JobStatus struct {
	Name     string
	State    JobRunState
	PID      int
	ShimPID  int
	Since    string
	Restarts int
	Sandbox  string
	// Stale is set for a running job whose last beat, LastBeat, is older than its heartbeat
	// allows. LastBeat is empty for a job without a heartbeat.
	Stale    bool
	LastBeat string
}

type jobSignalTarget struct {
	Name       string
	PID        int
	ShimPID    int
	RecordPath string
	LockPath   string
}

type Options struct {
	Clock        clock.Clock
	ShimCommand  []string
	PollInterval time.Duration
}

type Supervisor struct {
	cfg          config.Config
	signer       *corereceipt.Signer
	clk          clock.Clock
	shimCommand  []string
	pollInterval time.Duration
	shimWaiters  atomic.Int32
}

// reportShimError tells the operator about a shim that could not be collected. Nothing
// returns to a caller from the waiter, so it goes to stderr like the shim's own errors.
var reportShimError = func(name string, err error) {
	fmt.Fprintf(os.Stderr, "Error: collect shim of %s: %v\n", name, err)
}

func New(cfg config.Config, opts Options) (*Supervisor, error) {
	if err := platformSupported(); err != nil {
		return nil, err
	}
	if opts.Clock == nil {
		opts.Clock = clockwork.NewRealClock()
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = defaultPollInterval
	}
	if len(opts.ShimCommand) == 0 {
		path, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("supervisor: executable: %w", err)
		}
		opts.ShimCommand = []string{path}
	}
	if err := checkConfig(cfg); err != nil {
		return nil, err
	}
	cfg = normalizeConfig(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	signer, err := eventlog.NewSignerFromKeyFile(ctx, cfg.EventLog.SigningKeyPath, cfg.EventLog.Dir, opts.Clock)
	if err != nil {
		return nil, err
	}
	return &Supervisor{cfg: cfg, signer: signer, clk: opts.Clock, shimCommand: opts.ShimCommand, pollInterval: opts.PollInterval}, nil
}

func (s *Supervisor) PublicKey() string {
	return s.signer.PublicKey()
}

func (s *Supervisor) Start(ctx context.Context, name string) error {
	if err := readyContext(ctx, "start"); err != nil {
		return err
	}
	if name == "" {
		for i := 0; i < len(s.cfg.Jobs); i++ {
			if err := s.startOne(ctx, s.cfg.Jobs[i].Name); err != nil {
				return err
			}
		}
		return nil
	}
	return s.startOne(ctx, name)
}

// StartOnce starts one job like Start, with one difference: a job that runs already is an
// error wrapping ErrRunning. Start leaves a running job alone and reports success, which is
// what a daemon wants. A caller that needs a run of its own has to learn that it got none.
//
// Two callers that start the same idle job at the same moment both spawn a shim; the job's
// lock lets one of them run, and the other caller gets the shim's refusal (ErrRefused).
func (s *Supervisor) StartOnce(ctx context.Context, name string) error {
	if err := readyContext(ctx, "start"); err != nil {
		return err
	}
	job, err := s.job(name)
	if err != nil {
		return err
	}
	running, err := s.isRunning(job)
	if err != nil {
		return err
	}
	if running {
		return fmt.Errorf("%w: %s", ErrRunning, job.Name)
	}
	return s.startOne(ctx, name)
}

// Recorded returns the named job as the event log tells it: its last start, and the exit or
// the refusal that followed. Status says whether a job runs now; this says how it ended.
func (s *Supervisor) Recorded(ctx context.Context, name string) (RecordedJob, error) {
	if err := readyContext(ctx, "recorded"); err != nil {
		return eventlog.Job{}, err
	}
	job, err := s.job(name)
	if err != nil {
		return eventlog.Job{}, err
	}
	state, err := s.replayState(ctx)
	if err != nil {
		return eventlog.Job{}, err
	}
	return state.Jobs[job.Name], nil
}

// Preflight reports whether this host can start the named job's sandbox at all: the tools
// the shim would look for are looked for here. A job that runs without a sandbox needs none.
// It starts nothing; a job that passes can still be refused when it starts.
func (s *Supervisor) Preflight(name string) error {
	job, err := s.job(name)
	if err != nil {
		return err
	}
	if job.Sandbox.Mode == "off" {
		return nil
	}
	return sandboxPreflight(job.Sandbox)
}

func (s *Supervisor) startOne(ctx context.Context, name string) (err error) {
	ctx, span := telemetry.Default().StartSpan(ctx, "supervisor.job.start", attribute.String("epitrop.job", name))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()
	job, err := s.job(name)
	if err != nil {
		telemetry.Default().JobRun(ctx, "failed")
		return err
	}
	running, err := s.isRunning(job)
	if err != nil || running {
		if err != nil {
			telemetry.Default().JobRun(ctx, "failed")
		}
		return err
	}
	err = s.spawnAndAwait(ctx, job)
	s.recordJobRun(ctx, err)
	return err
}

func (s *Supervisor) recordJobRun(ctx context.Context, err error) {
	if err == nil {
		telemetry.Default().JobRun(ctx, "started")
		return
	}
	if errors.Is(err, ErrRefused) {
		telemetry.Default().JobRun(ctx, "refused")
		return
	}
	telemetry.Default().JobRun(ctx, "failed")
}

func (s *Supervisor) spawnAndAwait(ctx context.Context, job config.JobConfig) error {
	before, err := s.replayState(ctx)
	if err != nil {
		return err
	}
	if s.shimWaiters.Add(1) > maxShimWaiters {
		s.shimWaiters.Add(-1)
		return fmt.Errorf("supervisor: start shim for %s: %d shims are still uncollected", job.Name, maxShimWaiters)
	}
	cmd := s.shimCmd(ctx, job)
	if err = cmd.Start(); err != nil {
		s.shimWaiters.Add(-1)
		return fmt.Errorf("supervisor: start shim for %s: %w", job.Name, err)
	}
	shimPID := cmd.Process.Pid
	go s.collectShim(cmd, job.Name)
	return s.waitStarted(ctx, job.Name, before.Jobs[job.Name], shimPID)
}

// collectShim waits for a started shim and takes its exit status. Without this the shim
// stays in the process table as a zombie for as long as the supervisor runs, one per job
// start. The wait is not tied to the caller's context: the shim outlives it on purpose. An
// exit status other than 0 is no error here, the shim reports its own failures through the
// event log and the job's log.
func (s *Supervisor) collectShim(cmd *exec.Cmd, name string) {
	defer s.shimWaiters.Add(-1)
	err := cmd.Wait()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		reportShimError(name, err)
	}
}

func (s *Supervisor) Status(ctx context.Context, name string) ([]JobStatus, error) {
	if err := readyContext(ctx, "status"); err != nil {
		return nil, err
	}
	jobs, err := s.selectedJobs(name)
	if err != nil {
		return nil, err
	}
	state, err := s.replayState(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]JobStatus, 0, len(jobs))
	for i := 0; i < len(jobs); i++ {
		out = append(out, s.statusFor(jobs[i], state.Jobs[jobs[i].Name]))
	}
	return out, nil
}

func (s *Supervisor) Stop(ctx context.Context, name string) error {
	if err := readyContext(ctx, "stop"); err != nil {
		return err
	}
	if name == "" {
		for i := 0; i < len(s.cfg.Jobs); i++ {
			if err := s.stopOne(ctx, s.cfg.Jobs[i].Name); err != nil {
				return err
			}
		}
		return nil
	}
	return s.stopOne(ctx, name)
}

func (s *Supervisor) stopOne(ctx context.Context, name string) error {
	job, err := s.job(name)
	if err != nil {
		return err
	}
	if s.statusFor(job, eventlog.Job{}).State == StateRunning {
		if err = s.terminate(ctx, job); err != nil {
			return err
		}
	}
	return s.appendJobEvent(ctx, "job.stopped", job.Name, map[string]any{"state": string(StateStopped)})
}

// terminate ends a running job: its stop signal to the process group, its grace period,
// then a kill of the group. It records nothing; the shim records the exit.
func (s *Supervisor) terminate(ctx context.Context, job config.JobConfig) error {
	target, err := s.signalTarget(job)
	if err != nil {
		return err
	}
	if err = signalProcessGroup(target, job.Stop.Signal); err != nil {
		return s.refusalStands(job, err)
	}
	if !s.waitStopped(ctx, job) {
		if err = killProcess(target); err != nil {
			return s.refusalStands(job, err)
		}
		if !s.waitUnlocked(ctx, job, maxStatusPolls) {
			return fmt.Errorf("supervisor: stop %s: lock stayed held after SIGKILL", job.Name)
		}
	}
	return nil
}

// refusalStands is the result of a signal that was refused. A shim that exits releases the
// job's lock before the kernel gives its job another parent, so a stop in that moment finds
// the job and its shim alive and the lock free, and refuses. The job is then no longer
// running as Status counts it, and the shim's exit kills it (Pdeathsig): the job has ended,
// nothing is signalled, and the result is nil. While the lock is held, or cannot be read,
// the refusal stands.
func (s *Supervisor) refusalStands(job config.JobConfig, refusal error) error {
	running, err := s.isRunning(job)
	if err != nil || running {
		return refusal
	}
	return nil
}

// endIfStale ends a running job whose heartbeat is stale, after recording job.stale, and
// reports whether it did. No job.stopped is recorded: the shim records the exit, and the
// job's restart policy decides on the next reconcile.
func (s *Supervisor) endIfStale(ctx context.Context, job config.JobConfig, st eventlog.Job) (bool, error) {
	status := s.statusFor(job, st)
	if status.State != StateRunning || !status.Stale {
		return false, nil
	}
	payload := map[string]any{"pid": status.PID, "shim_pid": status.ShimPID, "last_beat": status.LastBeat, "max_age_seconds": job.Heartbeat.MaxAgeSeconds}
	if err := s.appendJobEvent(ctx, "job.stale", job.Name, payload); err != nil {
		return false, err
	}
	return true, s.terminate(ctx, job)
}

func (s *Supervisor) signalTarget(job config.JobConfig) (jobSignalTarget, error) {
	recordPath := s.recordPath(job.Name)
	rec, ok := readJobRecord(recordPath)
	if !ok {
		return jobSignalTarget{}, fmt.Errorf("supervisor: record %s: missing or invalid", recordPath)
	}
	return jobSignalTarget{
		Name:       job.Name,
		PID:        rec.PID,
		ShimPID:    rec.ShimPID,
		RecordPath: recordPath,
		LockPath:   s.lockPath(job.Name),
	}, nil
}

func (s *Supervisor) Supervise(ctx context.Context) error {
	if err := readyContext(ctx, "supervise"); err != nil {
		return err
	}
	writer, err := eventlog.Open(s.cfg.EventLog.Dir, s.signer, s.eventLogLimits())
	if err != nil {
		return fmt.Errorf("supervisor: open event log %s: %w", s.cfg.EventLog.Dir, err)
	}
	if _, err = writer.RepairTail(ctx); err != nil {
		return fmt.Errorf("supervisor: repair event log tail %s: %w", s.cfg.EventLog.Dir, err)
	}
	for tick := 0; tick < maxSuperviseTicks; tick++ {
		if err := s.Reconcile(ctx); err != nil {
			return err
		}
		if !sleepContext(ctx, s.pollInterval) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("supervisor: supervise exceeded %d ticks", maxSuperviseTicks)
}

func (s *Supervisor) Reconcile(ctx context.Context) error {
	if err := readyContext(ctx, "reconcile"); err != nil {
		return err
	}
	collector := &eventMatchCollector{jobs: s.cfg.Jobs, matches: map[string][]uint64{}}
	state, err := s.replayStateWithReducer(ctx, collector.reduce)
	if err != nil {
		return err
	}
	for i := 0; i < len(s.cfg.Jobs); i++ {
		job := s.cfg.Jobs[i]
		ended, err := s.endIfStale(ctx, job, state.Jobs[job.Name])
		if err != nil {
			return err
		}
		if ended {
			continue
		}
		if err = s.reconcileJob(ctx, job, state.Jobs[job.Name], collector.matches[job.Name], collector.head); err != nil {
			return err
		}
	}
	return nil
}

// waitStarted waits until the event log records a start by the shim just spawned. It
// does not require the job to still be running: a job that started and already exited
// did start, and polling for "running" would miss it between two polls. A shim that has
// exited records nothing more, so the wait ends one replay after it is seen gone.
func (s *Supervisor) waitStarted(ctx context.Context, name string, before eventlog.Job, shimPID int) error {
	var lastErr error
	shimGone := false
	for i := 0; i < maxStartPolls; i++ {
		state, err := s.replayState(ctx)
		if err == nil {
			if settled, startErr := startOutcome(name, state.Jobs[name], before, shimPID); settled {
				return startErr
			}
		}
		lastErr = lastReplayError(lastErr, err, ctx.Err())
		if shimGone {
			break
		}
		shimGone = shimExited(shimPID)
		if !shimGone && !sleepContext(ctx, startPollInterval) {
			return startWaitEnded(name, ctx.Err(), lastErr)
		}
	}
	if lastErr != nil {
		return fmt.Errorf("supervisor: %s did not start: %w", name, lastErr)
	}
	if shimGone {
		return fmt.Errorf("supervisor: %s did not start: its shim exited without recording a start; a refused job's log names the reason", name)
	}
	return fmt.Errorf("supervisor: %s did not start", name)
}

// lastReplayError keeps the result of the last replay that ran to its end. A replay the
// caller's context cut short says nothing about the log, so it leaves the previous result.
func lastReplayError(previous error, replayErr error, ctxErr error) error {
	if ctxErr != nil {
		return previous
	}
	return replayErr
}

// startWaitEnded is the error of a wait the caller's context ended. A replay that was still
// failing then is part of the answer: without it the caller sees only a deadline.
func startWaitEnded(name string, ctxErr error, lastErr error) error {
	if lastErr == nil {
		return fmt.Errorf("supervisor: %s did not start: %w", name, ctxErr)
	}
	return fmt.Errorf("supervisor: %s did not start: %w; last replay: %w", name, ctxErr, lastErr)
}

// shimExited reports whether the shim has exited. A shim whose state cannot be read is
// taken as alive; the caller's poll bound still holds.
func shimExited(pid int) bool {
	gone, err := processGone(pid)
	return err == nil && gone
}

// startOutcome reports whether the shim has settled this start, by recording it or by
// refusing it, and the error of a refusal.
func startOutcome(name string, job eventlog.Job, before eventlog.Job, shimPID int) (bool, error) {
	if startedBy(job, before, shimPID) {
		return true, nil
	}
	if refusedBy(job, before, shimPID) {
		return true, fmt.Errorf("%w: %s: %s", ErrRefused, name, job.LastReason)
	}
	return false, nil
}

// refusedBy reports whether job's last event is a refusal by shimPID recorded after before
// was read. As with a start, the changed time is what proves the refusal is new.
func refusedBy(job eventlog.Job, before eventlog.Job, shimPID int) bool {
	if job.LastEvent != "job.refused" || job.RefusedShimPID != shimPID || job.RefusedAt == "" {
		return false
	}
	return job.RefusedAt != before.RefusedAt
}

// startedBy reports whether job holds a start by shimPID recorded after before was read.
// Only job.started and job.adopted set ShimPID and Since (both from the shim's own record),
// and later events keep both, so a start followed by an exit still counts. A changed start
// time is what proves the start is new; a reused shim PID cannot pass on an old record.
func startedBy(job eventlog.Job, before eventlog.Job, shimPID int) bool {
	if job.ShimPID != shimPID || job.Since == "" {
		return false
	}
	return job.Since != before.Since
}
