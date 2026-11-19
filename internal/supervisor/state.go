package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
)

const (
	// reasonSupervisorStop is the reason of a job.interrupted record: the job's supervisor
	// stopped while the job was recorded running.
	reasonSupervisorStop = "supervisor-stop"
	// eventSupervisorStopped is the marker a stopping supervisor appends for each job it left
	// recorded running.
	eventSupervisorStopped = "job.supervisor_stopped"
	// eventInterrupted records the end of a job whose stopping supervisor marked it running.
	eventInterrupted = "job.interrupted"
)

type jobRecord struct {
	Name      string `json:"name"`
	PID       int    `json:"pid"`
	ShimPID   int    `json:"shim_pid"`
	StartedAt string `json:"started_at"`
	LogPath   string `json:"log_path"`
}

var jobRecordReadFile = os.ReadFile

func (s *Supervisor) replayState(ctx context.Context) (eventlog.State, error) {
	return s.replayStateWithReducer(ctx, JobReducer)
}

func (s *Supervisor) replayStateWithReducer(ctx context.Context, reducer eventlog.Reducer) (eventlog.State, error) {
	events := filepath.Join(s.cfg.EventLog.Dir, "events")
	if _, err := os.Stat(events); os.IsNotExist(err) {
		return eventlog.State{Jobs: map[string]eventlog.Job{}}, nil
	} else if err != nil {
		return eventlog.State{}, fmt.Errorf("supervisor: stat %s: %w", events, err)
	}
	return eventlog.ReplayFoldStable(ctx, s.cfg.EventLog.Dir, s.signer.PublicKey(), s.eventLogLimits(), FoldName, reducer)
}

// FoldName names the jobs state in the base of a compacted log.
const FoldName = "jobs"

// Fold is the jobs state as a compacted log's base holds it. Reconcile reads, beside the
// state, the records an event job's trigger matches after its cursor (see
// reconcileEventJob), so the base stays at or below the lowest cursor of the armed event
// jobs of jobs.
func Fold(jobs []config.JobConfig) eventlog.Fold {
	return eventlog.Fold{Name: FoldName, Reduce: JobReducer, Floor: func(st eventlog.State) uint64 {
		return eventJobsFloor(jobs, st)
	}}
}

// eventJobsFloor is the lowest seq past which an armed event job of jobs still reads the
// records its trigger matches.
func eventJobsFloor(jobs []config.JobConfig, st eventlog.State) uint64 {
	floor := uint64(math.MaxUint64)
	for i := 0; i < len(jobs); i++ {
		job, ok := st.Jobs[jobs[i].Name]
		if jobs[i].Schedule != "event" || !ok || job.ArmedTrigger == "" {
			continue
		}
		floor = min(floor, max(job.ArmedSeq, job.LastTriggerSeq))
	}
	return floor
}

// eventLogLimits are the limits under which the supervisor reads and writes the event log.
// The lock timeout is the configured one. Left out, the log falls back to its own default,
// and an operator who raised event_log.lock_timeout_seconds for a slow disk would see no
// effect.
func (s *Supervisor) eventLogLimits() eventlog.Limits {
	return eventlog.Limits{
		Clock:            s.clk,
		LockTimeout:      time.Duration(s.cfg.EventLog.LockTimeoutSeconds) * time.Second,
		MaxReplayRecords: s.cfg.EventLog.MaxReplayRecords,
	}
}

var jobReducers = map[string]eventlog.Reducer{
	"job.started":          reduceJobStarted,
	"job.adopted":          reduceJobStarted,
	"job.exited":           reduceJobExited,
	eventInterrupted:       reduceJobInterrupted,
	"job.refused":          reduceJobRefused,
	"job.restarted":        reduceJobRestarted,
	eventSupervisorStopped: reduceJobSupervisorStopped,
	"job.stopped":          reduceJobStopped,
	"job.armed":            reduceJobArmed,
	"job.triggered":        reduceJobTriggered,
}

func JobReducer(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	if st.Jobs == nil {
		st.Jobs = map[string]eventlog.Job{}
	}
	if fn, ok := jobReducers[rec.Type]; ok {
		return fn(st, rec)
	}
	return st, nil
}

func reduceJobStarted(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	var payload struct {
		State   string `json:"state"`
		PID     int    `json:"pid"`
		ShimPID int    `json:"shim_pid"`
		Since   string `json:"since"`
		Sandbox string `json:"sandbox"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		return eventlog.State{}, fmt.Errorf("job payload: %w", err)
	}
	job := st.Jobs[rec.TaskID]
	job.Name, job.State, job.PID = rec.TaskID, payload.State, payload.PID
	job.ShimPID, job.Since, job.LastEvent = payload.ShimPID, payload.Since, rec.Type
	job.Sandbox = payload.Sandbox
	job.SupervisorStopped = false
	st.Jobs[rec.TaskID] = job
	return st, nil
}

// reduceJobSupervisorStopped marks a job the stopping supervisor left recorded running, when
// the marker's since matches the job's current run. It changes neither State, LastEvent nor
// the restart counts: the marker explains the end that follows, it is no event of the job.
func reduceJobSupervisorStopped(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	var payload struct {
		Since string `json:"since"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		return eventlog.State{}, fmt.Errorf("job payload: %w", err)
	}
	job := st.Jobs[rec.TaskID]
	if job.State != string(StateRunning) || payload.Since == "" || payload.Since != job.Since {
		return st, nil
	}
	job.Name, job.SupervisorStopped = rec.TaskID, true
	st.Jobs[rec.TaskID] = job
	return st, nil
}

// reduceJobInterrupted records the end of a job that was running when its supervisor stopped:
// the job is dead with reason supervisor-stop and no exit code, the marker is cleared, and
// its restart counts stay as they were.
func reduceJobInterrupted(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	job := st.Jobs[rec.TaskID]
	job.Name, job.State, job.LastEvent = rec.TaskID, string(StateDead), rec.Type
	job.LastReason, job.LastExitCode, job.SupervisorStopped = reasonSupervisorStop, nil, false
	st.Jobs[rec.TaskID] = job
	return st, nil
}

func reduceJobExited(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	var payload struct {
		State  string `json:"state"`
		Code   *int   `json:"code,omitempty"`
		Reason string `json:"reason,omitempty"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		return eventlog.State{}, fmt.Errorf("job payload: %w", err)
	}
	job := st.Jobs[rec.TaskID]
	job.Name, job.State, job.LastExitCode = rec.TaskID, payload.State, payload.Code
	job.LastReason, job.LastEvent, job.SupervisorStopped = payload.Reason, rec.Type, false
	st.Jobs[rec.TaskID] = job
	return st, nil
}

// reduceJobRefused records a start the shim refused: the job is not running, and the reason
// is kept. ShimPID and Since stay as they are; they belong to the last recorded start, and
// startedBy reads them.
func reduceJobRefused(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	var payload struct {
		State   string `json:"state"`
		ShimPID int    `json:"shim_pid"`
		At      string `json:"at"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		return eventlog.State{}, fmt.Errorf("job payload: %w", err)
	}
	job := st.Jobs[rec.TaskID]
	job.Name, job.State, job.LastEvent = rec.TaskID, payload.State, rec.Type
	job.LastReason, job.LastExitCode, job.SupervisorStopped = payload.Reason, nil, false
	job.RefusedShimPID, job.RefusedAt = payload.ShimPID, payload.At
	st.Jobs[rec.TaskID] = job
	return st, nil
}

// restartTimesKept bounds the restart times a job keeps: the cap never exceeds
// config.MaxJobRestarts, so an older time never decides a restart.
const restartTimesKept = config.MaxJobRestarts

// appendRestartTime returns a new slice with t after the newest kept times;
// the slice of an earlier State is never written.
func appendRestartTime(times []string, t string) []string {
	n := len(times)
	start := 0
	if n >= restartTimesKept {
		start = n - restartTimesKept + 1
	}
	kept := times[start:]
	out := make([]string, len(kept)+1)
	copy(out, kept)
	out[len(kept)] = t
	return out
}

func reduceJobRestarted(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	job := st.Jobs[rec.TaskID]
	job.Name, job.LastEvent = rec.TaskID, rec.Type
	job.Restarts++
	job.RestartTimes = appendRestartTime(job.RestartTimes, rec.Time)
	st.Jobs[rec.TaskID] = job
	return st, nil
}

func reduceJobStopped(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	job := st.Jobs[rec.TaskID]
	job.Name, job.State, job.LastEvent = rec.TaskID, string(StateStopped), rec.Type
	job.SupervisorStopped = false
	st.Jobs[rec.TaskID] = job
	return st, nil
}

func reduceJobArmed(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	var payload struct {
		Trigger string `json:"trigger"`
		At      string `json:"at"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		return eventlog.State{}, fmt.Errorf("job payload: %w", err)
	}
	job := st.Jobs[rec.TaskID]
	job.Name, job.LastEvent = rec.TaskID, rec.Type
	// The armed record's own seq is the anchor: every record before it is history for this
	// trigger, including records appended in the same reconcile pass after the replay that
	// decided to arm. A HEAD read before that pass would leave those records to fire the job.
	job.ArmedTrigger, job.ArmedAt, job.ArmedSeq = payload.Trigger, payload.At, rec.Seq
	job.LastDue, job.LastTriggerSeq = "", 0
	st.Jobs[rec.TaskID] = job
	return st, nil
}

func reduceJobTriggered(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	var payload struct {
		Trigger string `json:"trigger"`
		Outcome string `json:"outcome"`
		Reason  string `json:"reason,omitempty"`
		Due     string `json:"due,omitempty"`
		Seq     uint64 `json:"seq,omitempty"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		return eventlog.State{}, fmt.Errorf("job payload: %w", err)
	}
	job := st.Jobs[rec.TaskID]
	job.Name, job.LastEvent = rec.TaskID, rec.Type
	if payload.Due != "" {
		job.LastDue = payload.Due
	}
	if payload.Seq != 0 {
		job.LastTriggerSeq = payload.Seq
	}
	st.Jobs[rec.TaskID] = job
	return st, nil
}

func (s *Supervisor) statusFor(job config.JobConfig, replayed eventlog.Job) JobStatus {
	rec, hasRecord := readJobRecord(s.recordPath(job.Name))
	running := false
	if hasRecord {
		held, err := lockHeld(s.lockPath(job.Name))
		running = err == nil && held
	}
	if running {
		status := JobStatus{Name: job.Name, State: StateRunning, PID: rec.PID, ShimPID: rec.ShimPID, Since: rec.StartedAt, Restarts: replayed.Restarts, Sandbox: runningSandbox(rec, replayed)}
		s.checkHeartbeat(job, rec, &status)
		return status
	}
	if replayed.State == string(StateStopped) {
		return JobStatus{Name: job.Name, State: StateStopped, Restarts: replayed.Restarts, Sandbox: sandboxStatus(job.Sandbox)}
	}
	return JobStatus{Name: job.Name, State: StateDead, Restarts: replayed.Restarts, Sandbox: sandboxStatus(job.Sandbox)}
}

// checkHeartbeat sets LastBeat and Stale of a running job that has a heartbeat. The last
// beat is the newer of the job's start and the last write to its log. Liveness stays the
// held lock; the beat only tells a hung job from a working one. A record whose start cannot
// be read counts the log alone, and a job with neither has its last beat at the zero time,
// so it is stale.
func (s *Supervisor) checkHeartbeat(job config.JobConfig, rec jobRecord, status *JobStatus) {
	if job.Heartbeat.MaxAgeSeconds == 0 {
		return
	}
	beat, err := time.Parse(time.RFC3339Nano, rec.StartedAt)
	if err != nil {
		beat = time.Time{}
	}
	if info, statErr := os.Stat(rec.LogPath); statErr == nil && info.ModTime().After(beat) {
		beat = info.ModTime()
	}
	status.LastBeat = beat.UTC().Format(time.RFC3339Nano)
	status.Stale = s.clk.Now().Sub(beat) > time.Duration(job.Heartbeat.MaxAgeSeconds)*time.Second
}

// runningSandbox names the sandbox a running job was started under, as its shim recorded
// it in job.started, never the current config, which may have changed since. Until the
// shim's event for this start is replayed, the answer is unknown.
func runningSandbox(rec jobRecord, replayed eventlog.Job) string {
	if replayed.Since != rec.StartedAt || replayed.Sandbox == "" {
		return "unknown"
	}
	return replayed.Sandbox
}

func readJobRecord(path string) (jobRecord, bool) {
	body, err := jobRecordReadFile(path) // #nosec G304 -- path is derived from configured state dir plus checked job name.
	if err != nil {
		return jobRecord{}, false
	}
	var rec jobRecord
	if err = json.Unmarshal(body, &rec); err != nil {
		return jobRecord{}, false
	}
	return rec, true
}

// errSkipJobEvent stops an AppendChecked call without writing a record when the replayed
// state under the event-log lock no longer matches the snapshot that decided the append.
var errSkipJobEvent = errors.New("supervisor: skip job event")

// appendEvent writes one event to the event log after check has accepted it under the log's
// lock. It is a package seam replaced in tests to simulate disk or deadline errors.
var appendEvent = (*eventlog.Writer).AppendChecked

func (s *Supervisor) appendJobEvent(ctx context.Context, typ string, name string, payload map[string]any) error {
	_, err := s.appendJobEventIf(ctx, typ, name, payload, nil)
	return err
}

func (s *Supervisor) appendJobEventIf(ctx context.Context, typ string, name string, payload map[string]any, want func(eventlog.Job) bool) (bool, error) {
	var check func(context.Context, eventlog.Job) (bool, error)
	if want != nil {
		check = func(_ context.Context, st eventlog.Job) (bool, error) { return want(st), nil }
	}
	return s.appendCheckedJobEvent(ctx, typ, name, func(eventlog.Job) map[string]any { return payload }, check)
}

func (s *Supervisor) appendCheckedJobEvent(ctx context.Context, typ string, name string, payload func(eventlog.Job) map[string]any, check func(context.Context, eventlog.Job) (bool, error)) (bool, error) {
	writer, err := eventlog.Open(s.cfg.EventLog.Dir, s.signer, s.eventLogLimits())
	if err != nil {
		return false, fmt.Errorf("supervisor: record %s for job %s in event log %s: %w", typ, name, s.cfg.EventLog.Dir, err)
	}
	_, err = appendEvent(writer, ctx, func(cctx context.Context) (eventlog.Event, error) {
		st, err := s.checkJobEvent(cctx, name, check)
		if err != nil {
			return eventlog.Event{}, err
		}
		body, err := json.Marshal(payload(st))
		if err != nil {
			return eventlog.Event{}, fmt.Errorf("supervisor: marshal %s: %w", typ, err)
		}
		return eventlog.Event{Type: typ, TaskID: name, Payload: body}, nil
	})
	if errors.Is(err, errSkipJobEvent) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("supervisor: record %s for job %s in event log %s: %w", typ, name, s.cfg.EventLog.Dir, err)
	}
	return true, nil
}

func (s *Supervisor) checkJobEvent(ctx context.Context, name string, check func(context.Context, eventlog.Job) (bool, error)) (eventlog.Job, error) {
	if check == nil {
		return eventlog.Job{}, nil
	}
	st, err := s.replayState(ctx)
	if err != nil {
		return eventlog.Job{}, err
	}
	job := st.Jobs[name]
	ok, err := check(ctx, job)
	if err != nil {
		return eventlog.Job{}, err
	}
	if !ok {
		return eventlog.Job{}, errSkipJobEvent
	}
	return job, nil
}

func (s *Supervisor) reconcileJob(ctx context.Context, job config.JobConfig, st eventlog.Job, matches []uint64, replayHead uint64) error {
	if st.State == string(StateRunning) && st.SupervisorStopped {
		status := s.statusFor(job, st)
		if status.State != StateRunning {
			return s.recordInterruptedIfUnlocked(ctx, job, st)
		}
		if job.Schedule != "always" {
			return s.adoptIfFreshStart(ctx, job, st, status)
		}
	}
	switch job.Schedule {
	case "always":
		return s.reconcileAlwaysJob(ctx, job, st)
	case "once":
		return nil
	}
	// Every other schedule is "event" or a cron expression; config validation refuses the rest.
	armedState, err := s.ensureArmed(ctx, job, st, replayHead)
	if err != nil {
		return err
	}
	if job.Schedule == "event" {
		return s.reconcileEventJob(ctx, job, armedState, matches)
	}
	return s.reconcileCronJob(ctx, job, armedState)
}

// recordInterruptedIfUnlocked records the interruption of a marked job that statusFor did
// not find running. statusFor needs a readable record before it probes the lock, so a held
// lock behind an unreadable record reads as dead; the lock alone decides here, and a held
// lock defers the interruption to a later pass.
func (s *Supervisor) recordInterruptedIfUnlocked(ctx context.Context, job config.JobConfig, st eventlog.Job) error {
	held, err := lockHeld(s.lockPath(job.Name))
	if err != nil {
		return fmt.Errorf("supervisor: check lock for %s: %w", job.Name, err)
	}
	if held {
		return nil
	}
	return s.recordInterrupted(ctx, job, st)
}

func (s *Supervisor) reconcileAlwaysJob(ctx context.Context, job config.JobConfig, st eventlog.Job) error {
	status := s.statusFor(job, st)
	if status.State == StateRunning {
		return s.adoptIfFreshStart(ctx, job, st, status)
	}
	if st.State == string(StateRunning) {
		return s.recordExit(ctx, job, st, "lost")
	}
	if st.LastEvent == "" {
		return s.startUnlessStopped(ctx, job.Name, st)
	}
	if shouldRestart(job, st, s.clk.Now()) {
		if endedBySupervisorStop(st) {
			return s.startUnlessStopped(ctx, job.Name, st)
		}
		return s.restartJob(ctx, job)
	}
	return nil
}

func (s *Supervisor) adoptIfFreshStart(ctx context.Context, job config.JobConfig, st eventlog.Job, status JobStatus) error {
	// A job the stopping supervisor marked survived the stop (a supervisor run without its own
	// pid namespace): its adoption clears the marker, so a later end is not read as a
	// supervisor stop.
	markedRunning := st.State == string(StateRunning) && st.SupervisorStopped
	if st.LastEvent != "job.started" && !markedRunning {
		return nil
	}
	payload := map[string]any{"state": string(StateRunning), "pid": status.PID, "shim_pid": status.ShimPID, "since": status.Since, "sandbox": status.Sandbox}
	_, err := s.appendJobEventIf(ctx, "job.adopted", job.Name, payload, func(curr eventlog.Job) bool {
		return curr.LastEvent == st.LastEvent && curr.State == st.State && curr.Since == st.Since && curr.SupervisorStopped == st.SupervisorStopped
	})
	return err
}

// recordInterrupted records the end of a job that was recorded running, holds no lock, and
// was marked by its stopping supervisor: the stop ended it, nothing failed.
func (s *Supervisor) recordInterrupted(ctx context.Context, job config.JobConfig, st eventlog.Job) error {
	payload := map[string]any{"state": string(StateDead), "reason": reasonSupervisorStop}
	_, err := s.appendJobEventIf(ctx, eventInterrupted, job.Name, payload, func(curr eventlog.Job) bool {
		return curr.State == string(StateRunning) && curr.SupervisorStopped && curr.Since == st.Since
	})
	return err
}

// recordExit records the exit of a job that was recorded running and holds no lock: nobody
// else will record it.
func (s *Supervisor) recordExit(ctx context.Context, job config.JobConfig, st eventlog.Job, reason string) error {
	payload := map[string]any{"state": string(StateDead), "reason": reason}
	_, err := s.appendJobEventIf(ctx, "job.exited", job.Name, payload, func(curr eventlog.Job) bool {
		return curr.State == string(StateRunning) && !curr.SupervisorStopped && curr.Since == st.Since
	})
	return err
}

// endedBySupervisorStop reports whether the job's last event was an interruption by the stop
// of its supervisor, and the job was not stopped on purpose since.
func endedBySupervisorStop(st eventlog.Job) bool {
	return st.LastEvent == eventInterrupted && st.State != string(StateStopped)
}

func (s *Supervisor) restartJob(ctx context.Context, job config.JobConfig) error {
	before, err := s.replayState(ctx)
	if err != nil {
		return err
	}
	payload := map[string]any{"state": string(StateDead)}
	if err = s.appendJobEvent(ctx, "job.restarted", job.Name, payload); err != nil {
		return err
	}
	if job.Restart.BackoffSeconds > 0 && !sleepContext(ctx, time.Duration(job.Restart.BackoffSeconds)*time.Second) {
		return ctx.Err()
	}
	return s.startUnlessStopped(ctx, job.Name, before.Jobs[job.Name])
}

// restartsInWindow counts the restarts the cap bounds: those newer than now minus the
// window. A window of 0 is the default window, never the job's whole life. A time that does
// not parse counts, so an unreadable record never frees budget.
func restartsInWindow(job config.JobConfig, st eventlog.Job, now time.Time) int {
	cutoff := now.Add(-time.Duration(restartWindowSeconds(job)) * time.Second)
	count := 0
	for _, raw := range st.RestartTimes {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil || t.After(cutoff) {
			count++
		}
	}
	return count
}

func shouldRestart(job config.JobConfig, st eventlog.Job, now time.Time) bool {
	if st.State == string(StateStopped) {
		return false
	}
	// The stop of the supervisor is no failure of the job: it resumes whatever the budget
	// holds, unless its policy is never to restart.
	if endedBySupervisorStop(st) && job.Restart.Policy != "never" {
		return true
	}
	if restartsInWindow(job, st, now) >= job.Restart.MaxRestarts {
		return false
	}
	if job.Restart.Policy == "always" {
		return true
	}
	if job.Restart.Policy != "on-failure" {
		return false
	}
	return st.LastExitCode == nil || *st.LastExitCode != 0 || st.LastReason == "lost"
}

// restartWindowSeconds is the job's restart window; 0 or unset is the default window.
func restartWindowSeconds(job config.JobConfig) int {
	if job.Restart.WindowSeconds <= 0 {
		return config.DefaultRestartWindowSeconds
	}
	return job.Restart.WindowSeconds
}

// restartDueAt predicts when a spent budget frees. Unreadable times never expire.
// The policy must still permit a restart after enough parsed times have aged out.
func restartDueAt(job config.JobConfig, st eventlog.Job, now time.Time) (time.Time, bool) {
	window := time.Duration(restartWindowSeconds(job)) * time.Second
	cutoff := now.Add(-window)
	parsed := make([]time.Time, 0, len(st.RestartTimes))
	for i := 0; i < len(st.RestartTimes); i++ {
		t, err := time.Parse(time.RFC3339Nano, st.RestartTimes[i])
		if err == nil && t.After(cutoff) {
			parsed = append(parsed, t)
		}
	}
	need := restartsInWindow(job, st, now) - job.Restart.MaxRestarts + 1
	// Unparseable times never age out: only parsed times can free budget.
	if need <= 0 || need > len(parsed) {
		return time.Time{}, false
	}
	slices.SortFunc(parsed, time.Time.Compare)
	due := parsed[need-1].Add(window)
	return due, shouldRestart(job, st, due.Add(time.Nanosecond))
}
