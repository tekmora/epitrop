package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/adhocore/gronx"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
)

func canonicalTrigger(job config.JobConfig) string {
	if job.Schedule == "event" && job.OnEvent != nil {
		return fmt.Sprintf("event %s task_id=%s status=%s", job.OnEvent.Type, job.OnEvent.TaskID, job.OnEvent.Status)
	}
	return "schedule " + job.Schedule
}

func matchEventTrigger(t *config.JobEventTrigger, rec eventlog.Record) bool {
	if rec.Type != t.Type {
		return false
	}
	if t.TaskID != "" && rec.TaskID != t.TaskID {
		return false
	}
	if t.Status != "" {
		var p struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rec.Payload, &p); err != nil || p.Status != t.Status {
			return false
		}
	}
	return true
}

// eventMatchCollector wraps JobReducer for one reconcile pass. It keeps the log's last seq and,
// for every event job, the seqs of the records its trigger matches, because the reduced state
// alone cannot say which records arrived. job.armed and job.triggered never match a trigger.
type eventMatchCollector struct {
	jobs    []config.JobConfig
	matches map[string][]uint64
	head    uint64
}

func (c *eventMatchCollector) reduce(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	if rec.Seq > c.head {
		c.head = rec.Seq
	}
	if rec.Type != "job.armed" && rec.Type != "job.triggered" {
		for i := 0; i < len(c.jobs); i++ {
			c.add(c.jobs[i], rec)
		}
	}
	return JobReducer(st, rec)
}

// add keeps a matching seq once. ReplayStable runs the reducer again from the first record when
// the log moved under a replay, so a seq at or below the last one kept for the job is a repeat.
func (c *eventMatchCollector) add(job config.JobConfig, rec eventlog.Record) {
	if job.Schedule != "event" || job.OnEvent == nil || !matchEventTrigger(job.OnEvent, rec) {
		return
	}
	seqs := c.matches[job.Name]
	if len(seqs) > 0 && rec.Seq <= seqs[len(seqs)-1] {
		return
	}
	c.matches[job.Name] = append(seqs, rec.Seq)
}

// ensureArmed appends job.armed when the job has no armed trigger or its configured trigger
// changed. The armed record's own seq anchors event matching once it is replayed (see
// reduceJobArmed); for the rest of this pass replayHead stands in, since every match this pass
// knows of comes from the replay that ended there.
func (s *Supervisor) ensureArmed(ctx context.Context, job config.JobConfig, st eventlog.Job, replayHead uint64) (eventlog.Job, error) {
	currTrigger := canonicalTrigger(job)
	if st.ArmedTrigger != "" && st.ArmedTrigger == currTrigger {
		return st, nil
	}
	now := s.clk.Now().UTC().Format(time.RFC3339)
	payload := map[string]any{
		"trigger": currTrigger,
		"at":      now,
	}
	if err := s.appendJobEvent(ctx, "job.armed", job.Name, payload); err != nil {
		return st, err
	}
	st.ArmedTrigger = currTrigger
	st.ArmedAt = now
	st.ArmedSeq = replayHead
	st.LastDue = ""
	st.LastTriggerSeq = 0
	return st, nil
}

func (s *Supervisor) reconcileCronJob(ctx context.Context, job config.JobConfig, st eventlog.Job) error {
	now := s.clk.Now().UTC()
	// Five-field schedules have minute precision; gronx v1.20.7 can return an in-slot reference second.
	due, err := gronx.PrevTickBefore(job.Schedule, now.Truncate(time.Minute), true)
	if err != nil {
		return fmt.Errorf("supervisor: job %s: prev tick for %q: %w", job.Name, job.Schedule, err)
	}
	due = due.UTC()
	base, err := time.Parse(time.RFC3339, st.ArmedAt)
	if err != nil {
		return fmt.Errorf("supervisor: job %s: parse armed_at %q: %w", job.Name, st.ArmedAt, err)
	}
	if st.LastDue != "" {
		lastDue, err := time.Parse(time.RFC3339, st.LastDue)
		if err != nil {
			return fmt.Errorf("supervisor: job %s: parse last_due %q: %w", job.Name, st.LastDue, err)
		}
		if lastDue.After(base) {
			base = lastDue
		}
	}
	if !due.After(base) {
		return nil
	}
	return s.triggerCronJob(ctx, job, st, due)
}

func (s *Supervisor) triggerCronJob(ctx context.Context, job config.JobConfig, st eventlog.Job, due time.Time) error {
	dueStr := due.Format(time.RFC3339)
	triggerStr := canonicalTrigger(job)
	status := s.statusFor(job, st)
	if status.State == StateRunning {
		payload := map[string]any{
			"trigger": triggerStr,
			"outcome": "skipped",
			"reason":  "running",
			"due":     dueStr,
		}
		return s.appendJobEvent(ctx, "job.triggered", job.Name, payload)
	}
	payload := map[string]any{
		"trigger": triggerStr,
		"outcome": "started",
		"due":     dueStr,
	}
	// Appended BEFORE Start (at-most-once: a crash between record and start loses that start but never doubles it).
	if err := s.appendJobEvent(ctx, "job.triggered", job.Name, payload); err != nil {
		return err
	}
	return s.Start(ctx, job.Name)
}

func (s *Supervisor) reconcileEventJob(ctx context.Context, job config.JobConfig, st eventlog.Job, matches []uint64) error {
	cursor := st.ArmedSeq
	if st.LastTriggerSeq > cursor {
		cursor = st.LastTriggerSeq
	}
	var pending []uint64
	for i := 0; i < len(matches); i++ {
		if matches[i] > cursor {
			pending = append(pending, matches[i])
		}
	}
	if len(pending) == 0 {
		return nil
	}
	status := s.statusFor(job, st)
	if status.State == StateRunning {
		// Nothing is recorded and the match waits (queued: the cursor does not move).
		return nil
	}
	return s.triggerEventJob(ctx, job, pending[0])
}

func (s *Supervisor) triggerEventJob(ctx context.Context, job config.JobConfig, seq uint64) error {
	triggerStr := canonicalTrigger(job)
	payload := map[string]any{
		"trigger": triggerStr,
		"outcome": "started",
		"seq":     seq,
	}
	// Appended BEFORE Start (at-most-once: a crash between record and start loses that start but never doubles it).
	if err := s.appendJobEvent(ctx, "job.triggered", job.Name, payload); err != nil {
		return err
	}
	return s.Start(ctx, job.Name)
}
