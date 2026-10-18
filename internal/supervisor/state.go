package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
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

func JobReducer(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	if st.Jobs == nil {
		st.Jobs = map[string]eventlog.Job{}
	}
	switch rec.Type {
	case "job.started", "job.adopted":
		return reduceJobStarted(st, rec)
	case "job.exited":
		return reduceJobExited(st, rec)
	case "job.refused":
		return reduceJobRefused(st, rec)
	case "job.restarted":
		return reduceJobRestarted(st, rec)
	case "job.stopped":
		return reduceJobStopped(st, rec)
	case "job.armed":
		return reduceJobArmed(st, rec)
	case "job.triggered":
		return reduceJobTriggered(st, rec)
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
	job.LastReason, job.LastEvent = payload.Reason, rec.Type
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
	job.LastReason, job.LastExitCode = payload.Reason, nil
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
	job.Name, job.Restarts, job.LastEvent = rec.TaskID, job.Restarts+1, rec.Type
	job.RestartTimes = appendRestartTime(job.RestartTimes, rec.Time)
	st.Jobs[rec.TaskID] = job
	return st, nil
}

func reduceJobStopped(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	job := st.Jobs[rec.TaskID]
	job.Name, job.State, job.LastEvent = rec.TaskID, string(StateStopped), rec.Type
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

// appendEvent writes one event to the event log. It is a package seam replaced in tests
// to simulate disk errors (such as ENOSPC) during event logging.
var appendEvent = (*eventlog.Writer).Append

func (s *Supervisor) appendJobEvent(ctx context.Context, typ string, name string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("supervisor: marshal %s: %w", typ, err)
	}
	writer, err := eventlog.Open(s.cfg.EventLog.Dir, s.signer, s.eventLogLimits())
	if err != nil {
		return fmt.Errorf("supervisor: record %s for job %s in event log %s: %w", typ, name, s.cfg.EventLog.Dir, err)
	}
	if _, err = appendEvent(writer, ctx, eventlog.Event{Type: typ, TaskID: name, Payload: body}); err != nil {
		return fmt.Errorf("supervisor: record %s for job %s in event log %s: %w", typ, name, s.cfg.EventLog.Dir, err)
	}
	return nil
}

func (s *Supervisor) reconcileJob(ctx context.Context, job config.JobConfig, st eventlog.Job, matches []uint64, replayHead uint64) error {
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

func (s *Supervisor) reconcileAlwaysJob(ctx context.Context, job config.JobConfig, st eventlog.Job) error {
	status := s.statusFor(job, st)
	if status.State == StateRunning {
		return s.adoptIfFreshStart(ctx, job, st, status)
	}
	if st.State == string(StateRunning) {
		return s.recordLost(ctx, job)
	}
	if st.LastEvent == "" {
		return s.Start(ctx, job.Name)
	}
	if shouldRestart(job, st, s.clk.Now()) {
		return s.restartJob(ctx, job)
	}
	return nil
}

func (s *Supervisor) adoptIfFreshStart(ctx context.Context, job config.JobConfig, st eventlog.Job, status JobStatus) error {
	if st.LastEvent != "job.started" {
		return nil
	}
	payload := map[string]any{"state": string(StateRunning), "pid": status.PID, "shim_pid": status.ShimPID, "since": status.Since, "sandbox": status.Sandbox}
	return s.appendJobEvent(ctx, "job.adopted", job.Name, payload)
}

func (s *Supervisor) recordLost(ctx context.Context, job config.JobConfig) error {
	payload := map[string]any{"state": string(StateDead), "reason": "lost"}
	return s.appendJobEvent(ctx, "job.exited", job.Name, payload)
}

func (s *Supervisor) restartJob(ctx context.Context, job config.JobConfig) error {
	payload := map[string]any{"state": string(StateDead)}
	if err := s.appendJobEvent(ctx, "job.restarted", job.Name, payload); err != nil {
		return err
	}
	if job.Restart.BackoffSeconds > 0 && !sleepContext(ctx, time.Duration(job.Restart.BackoffSeconds)*time.Second) {
		return ctx.Err()
	}
	return s.Start(ctx, job.Name)
}

// restartsInWindow counts the restarts the cap bounds: every restart without a
// window, otherwise those newer than now minus the window. A time that does not
// parse counts, so an unreadable record never frees budget.
func restartsInWindow(job config.JobConfig, st eventlog.Job, now time.Time) int {
	if job.Restart.WindowSeconds <= 0 {
		return st.Restarts
	}
	cutoff := now.Add(-time.Duration(job.Restart.WindowSeconds) * time.Second)
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
	if restartsInWindow(job, st, now) >= job.Restart.MaxRestarts {
		return false
	}
	if job.Restart.Policy == "always" && st.State != string(StateStopped) {
		return true
	}
	if job.Restart.Policy != "on-failure" {
		return false
	}
	return st.LastExitCode == nil || *st.LastExitCode != 0 || st.LastReason == "lost"
}
