package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
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
	maxStopAttempts     = 3
	// A start is awaited for 15 s (10 ms apart), longer than the shim may wait for the
	// job's scope, so a slow start is never reported as failed while it can still succeed.
	maxStartPolls     = 1500
	startPollInterval = 10 * time.Millisecond
	// Shims this supervisor started and has not collected yet. A job's lock allows one live
	// shim per job, so twice the number of jobs a config may hold is never reached in order.
	maxShimWaiters    = 2 * config.MaxJobs
	maxSuperviseTicks = 1 << 30
	// supervisorStopMarkTimeout bounds each stop-marker append.
	supervisorStopMarkTimeout = 5 * time.Second
)

// stopMarkTimeout is how long each job's stop-marker append may wait. It is a package
// variable so tests can shrink the bound without sleeping for seconds.
var stopMarkTimeout = supervisorStopMarkTimeout

var (
	ErrUnknownJob = errors.New("supervisor: unknown job")
	// ErrRefused marks a start the shim refused; the error text carries the job and the reason.
	ErrRefused      = errors.New("supervisor: job refused")
	ErrNotSupported = errors.New("supervisor: not-supported")
	// ErrRunning marks a StartOnce of a job that runs already.
	ErrRunning = errors.New("supervisor: job is running")
	// ErrStoppedBeforeStart marks an automatic start whose job record turned stopped before the
	// shim started the job. Reconcile treats it as no error.
	ErrStoppedBeforeStart = errors.New("supervisor: job stopped before start")
	// ErrStopMarkersNotWritten marks a Supervise return whose stop markers could not all be
	// written to the event log.
	ErrStopMarkersNotWritten = errors.New("supervisor: stop markers not written")
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

type spawnedShim struct {
	pid  int
	done chan struct{}
}

type collectedShim struct {
	pid     int
	sigterm bool
}

type Supervisor struct {
	cfg           config.Config
	signer        *corereceipt.Signer
	clk           clock.Clock
	shimCommand   []string
	pollInterval  time.Duration
	shimWaiters   atomic.Int32
	mu            sync.Mutex
	observedSince map[string]string
	spawnedShim   map[string]spawnedShim
	collectedShim map[string]collectedShim
}

// beforeRecordCollectedShim is a test seam called in collectShim after cmd.Wait returns and
// before the collected status is recorded.
var beforeRecordCollectedShim = func(string, int) {}

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

func (s *Supervisor) startOne(ctx context.Context, name string) error {
	return s.startJob(ctx, name, eventlog.Job{}, false)
}

func (s *Supervisor) startUnlessStopped(ctx context.Context, name string, before eventlog.Job) error {
	err := s.startJob(ctx, name, before, true)
	if errors.Is(err, ErrStoppedBeforeStart) {
		return nil
	}
	return err
}

func (s *Supervisor) startJob(ctx context.Context, name string, before eventlog.Job, unlessStopped bool) (err error) {
	ctx, span := telemetry.Default().StartSpan(ctx, "supervisor.job.start", attribute.String("epitrop.job", name))
	defer func() {
		if err != nil && !errors.Is(err, ErrStoppedBeforeStart) {
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
	err = s.spawnAndAwait(ctx, job, before, unlessStopped)
	if !errors.Is(err, ErrStoppedBeforeStart) {
		s.recordJobRun(ctx, err)
	}
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

func (s *Supervisor) spawnAndAwait(ctx context.Context, job config.JobConfig, before eventlog.Job, unlessStopped bool) error {
	state, err := s.replayState(ctx)
	if err != nil {
		return err
	}
	prior := state.Jobs[job.Name]
	if unlessStopped {
		prior.State = before.State
	}
	if s.shimWaiters.Add(1) > maxShimWaiters {
		s.shimWaiters.Add(-1)
		return fmt.Errorf("supervisor: start shim for %s: %d shims are still uncollected", job.Name, maxShimWaiters)
	}
	cmd := s.shimCmdFor(ctx, job, unlessStopped)
	if err = cmd.Start(); err != nil {
		s.shimWaiters.Add(-1)
		return fmt.Errorf("supervisor: start shim for %s: %w", job.Name, err)
	}
	shimPID := cmd.Process.Pid
	s.recordSpawnedShim(job.Name, shimPID)
	go s.collectShim(cmd, job.Name)
	return s.waitStarted(ctx, job.Name, prior, shimPID)
}

func (s *Supervisor) recordSpawnedShim(name string, pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spawnedShim == nil {
		s.spawnedShim = make(map[string]spawnedShim, len(s.cfg.Jobs))
	}
	s.spawnedShim[name] = spawnedShim{pid: pid, done: make(chan struct{})}
	delete(s.collectedShim, name)
}

// collectShim waits for a started shim and takes its exit status. Without this the shim
// stays in the process table as a zombie for as long as the supervisor runs, one per job
// start. The wait is not tied to the caller's context: the shim outlives it on purpose. An
// exit status other than 0 is no error here, the shim reports its own failures through the
// event log and the job's log.
func (s *Supervisor) collectShim(cmd *exec.Cmd, name string) {
	var pid int
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	defer s.finishShimCollector(name, pid)
	err := cmd.Wait()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		reportShimError(name, err)
		return
	}
	if cmd.ProcessState != nil {
		beforeRecordCollectedShim(name, cmd.ProcessState.Pid())
	}
	s.recordCollectedShim(name, cmd.ProcessState)
}

func (s *Supervisor) finishShimCollector(name string, pid int) {
	s.mu.Lock()
	if cur, ok := s.spawnedShim[name]; ok && cur.pid == pid && cur.done != nil {
		close(cur.done)
		cur.done = nil
		s.spawnedShim[name] = cur
	}
	s.mu.Unlock()
	s.shimWaiters.Add(-1)
}

func (s *Supervisor) recordCollectedShim(name string, state *os.ProcessState) {
	if state == nil || state.Pid() <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spawnedShim[name].pid != state.Pid() {
		return
	}
	if s.collectedShim == nil {
		s.collectedShim = make(map[string]collectedShim, len(s.cfg.Jobs))
	}
	s.collectedShim[name] = collectedShim{pid: state.Pid(), sigterm: endedBySIGTERM(state)}
}

func endedBySIGTERM(state *os.ProcessState) bool {
	ws, ok := state.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGTERM
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
	for attempt := 0; attempt < maxStopAttempts; attempt++ {
		lock, target, waitErr := s.waitStopTarget(ctx, job)
		if waitErr != nil {
			return fmt.Errorf("supervisor: stop %s: %w", name, waitErr)
		}
		if lock != nil {
			appendErr := s.appendJobEvent(ctx, "job.stopped", name, map[string]any{"state": string(StateStopped)})
			return errors.Join(appendErr, lock.Close())
		}
		if err = s.terminateTarget(ctx, job, target); err != nil {
			return err
		}
	}
	return fmt.Errorf("supervisor: stop %s: lock remained busy after %d attempts", name, maxStopAttempts)
}

// terminate ends a running job: its stop signal to the process group, its grace period,
// then a kill of the group. It records nothing; the shim records the exit.
func (s *Supervisor) terminate(ctx context.Context, job config.JobConfig) error {
	target, err := s.signalTarget(job)
	if err != nil {
		return err
	}
	return s.terminateTarget(ctx, job, target)
}

func (s *Supervisor) terminateTarget(ctx context.Context, job config.JobConfig, target jobSignalTarget) error {
	if err := signalProcessGroup(target, job.Stop.Signal); err != nil {
		return s.refusalStands(job, err)
	}
	if !s.waitStopped(ctx, job) {
		if err := killProcess(target); err != nil {
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

// Supervise reconciles every poll interval until ctx ends, and on every return marks the
// jobs it leaves recorded running (see markSupervisorStopped). A marker that cannot be
// written is returned joined with the reason Supervise ended for.
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
	err = s.superviseLoop(ctx)
	if markErr := s.markSupervisorStopped(); markErr != nil {
		return errors.Join(err, markErr)
	}
	return err
}

func (s *Supervisor) superviseLoop(ctx context.Context) error {
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

// markSupervisorStopped appends job.supervisor_stopped for each configured job whose run this
// supervisor observed (on Reconcile or Start) and that the event log still records running
// with that start time, unless this supervisor started and collected that run's shim with an
// exit other than SIGTERM. It does not read the job's lock: a stop of the supervisor's unit
// sends SIGTERM to the shims in the same cgroup at the same time, so each shim exits and frees
// its lock before the marker is written. A job whose exit the shim recorded before the marker
// is no longer recorded running and gets none, and a shim this supervisor started that died on
// its own (exit code, SIGKILL or crash) is collected with a non-SIGTERM status and gets none;
// only a run this supervisor adopted rather than started (no wait status) whose shim died
// unrecorded between the last reconcile and the stop reads as interrupted rather than lost, a
// window of one poll interval. Whatever ended Supervise, its context has ended too, so each
// job's checked append runs under a supervisorStopMarkTimeout deadline of its own: a slow or
// failing append for one job does not shorten the next. Because the event log appends one
// record at a time, a supervisor killed in the middle of marking still leaves the remaining
// jobs without a marker, and the next supervisor records them lost like a crash. The unit
// that runs the supervisor ends its jobs with it, and nothing else records their exit: the
// next supervisor reads the marker to tell this stop from a loss (reconcileJob).
func (s *Supervisor) markSupervisorStopped() error {
	pid := os.Getpid()
	var errs []error
	for i := 0; i < len(s.cfg.Jobs); i++ {
		if err := s.markOneJobStopped(s.cfg.Jobs[i], pid); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrStopMarkersNotWritten, errors.Join(errs...))
	}
	return nil
}

func (s *Supervisor) observeRunning(name string, since string) {
	if since == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.observedSince == nil {
		s.observedSince = make(map[string]string, len(s.cfg.Jobs))
	}
	s.observedSince[name] = since
}

func (s *Supervisor) observeReconciled(state eventlog.State) {
	for i := 0; i < len(s.cfg.Jobs); i++ {
		name := s.cfg.Jobs[i].Name
		if job := state.Jobs[name]; job.State == string(StateRunning) {
			s.observeRunning(name, job.Since)
		}
	}
}

// markOneJobStopped appends job.supervisor_stopped for job when the replay under the
// event-log lock still records the run this supervisor observed running, and any exit status
// collected from that run's shim was SIGTERM. The job's lock is not read because the stop's
// SIGTERM reaches the shims at the same time; only an adopted run (no wait status) whose shim
// died unrecorded between the last reconcile and the stop reads as interrupted rather than
// lost (a window of one poll interval).
func (s *Supervisor) markOneJobStopped(job config.JobConfig, pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), stopMarkTimeout)
	defer cancel()
	_, err := s.appendCheckedJobEvent(ctx, eventSupervisorStopped, job.Name, func(st eventlog.Job) map[string]any {
		return map[string]any{"supervisor_pid": pid, "since": st.Since}
	}, func(cctx context.Context, st eventlog.Job) (bool, error) {
		if err := s.waitPendingShimCollector(cctx, job.Name, st); err != nil {
			return false, err
		}
		return s.canMarkJobStopped(job.Name, st), nil
	})
	return err
}

func (s *Supervisor) waitPendingShimCollector(ctx context.Context, name string, st eventlog.Job) error {
	if st.State != string(StateRunning) || st.Since == "" {
		return nil
	}
	s.mu.Lock()
	since := s.observedSince[name]
	spawned, hasSpawned := s.spawnedShim[name]
	s.mu.Unlock()
	if !hasSpawned || spawned.done == nil || st.Since != since || st.ShimPID != spawned.pid || !shimExited(spawned.pid) {
		return nil
	}
	select {
	case <-spawned.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait shim collector: %w", ctx.Err())
	}
}

func (s *Supervisor) canMarkJobStopped(name string, st eventlog.Job) bool {
	if st.State != string(StateRunning) || st.Since == "" {
		return false
	}
	s.mu.Lock()
	since := s.observedSince[name]
	spawned := s.spawnedShim[name]
	end, hasEnd := s.collectedShim[name]
	s.mu.Unlock()
	if st.Since != since {
		return false
	}
	return shimAllowsStopMarker(st.ShimPID, spawned, end, hasEnd)
}

func shimAllowsStopMarker(shimPID int, spawned spawnedShim, end collectedShim, hasEnd bool) bool {
	if hasEnd && end.pid == shimPID {
		return end.sigterm
	}
	return spawned.pid != shimPID || !shimExited(spawned.pid)
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
	s.observeReconciled(state)
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
			if settled, startErr := s.settleStart(name, state.Jobs[name], before, shimPID, shimGone); settled {
				return startErr
			}
		}
		if ctx.Err() != nil {
			return startWaitEnded(name, ctx.Err(), errors.Join(lastErr, s.observeStartedOnCancel(name, before, shimPID)))
		}
		lastErr = lastReplayError(lastErr, err, ctx.Err())
		if shimGone {
			break
		}
		shimGone = shimExited(shimPID)
		if !shimGone && !sleepContext(ctx, startPollInterval) {
			return startWaitEnded(name, ctx.Err(), errors.Join(lastErr, s.observeStartedOnCancel(name, before, shimPID)))
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

func (s *Supervisor) settleStart(name string, job eventlog.Job, before eventlog.Job, shimPID int, shimGone bool) (bool, error) {
	settled, err := startOutcome(name, job, before, shimPID)
	if settled && errors.Is(err, ErrStoppedBeforeStart) && !shimGone {
		return false, nil
	}
	if settled && err == nil {
		s.observeRunning(name, job.Since)
	}
	return settled, err
}

// observeStartedOnCancel replays once more when a stop ends the start wait, so a start the
// shim recorded just before the stop is observed and can still get its stop marker.
func (s *Supervisor) observeStartedOnCancel(name string, before eventlog.Job, shimPID int) error {
	ctx, cancel := context.WithTimeout(context.Background(), stopMarkTimeout)
	defer cancel()
	state, err := s.replayState(ctx)
	if err != nil {
		return err
	}
	job := state.Jobs[name]
	if settled, startErr := startOutcome(name, job, before, shimPID); settled && startErr == nil {
		s.observeRunning(name, job.Since)
	}
	return nil
}

// startOutcome reports whether the shim has settled this start, by recording it, refusing
// it, or leaving a job record that turned stopped after before.
func startOutcome(name string, job eventlog.Job, before eventlog.Job, shimPID int) (bool, error) {
	if startedBy(job, before, shimPID) {
		return true, nil
	}
	if refusedBy(job, before, shimPID) {
		return true, fmt.Errorf("%w: %s: %s", ErrRefused, name, job.LastReason)
	}
	if stoppedAfter(job, before) {
		return true, fmt.Errorf("%w: %s", ErrStoppedBeforeStart, name)
	}
	return false, nil
}

func stoppedAfter(job eventlog.Job, before eventlog.Job) bool {
	return job.State == string(StateStopped) && before.State != string(StateStopped)
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
