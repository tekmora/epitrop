package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
)

// NoJobAliveExitCode is the exit status `epitropctl jobs supervise` ends with when
// Supervise returns ErrNoJobAlive. A host unit can name it in RestartPreventExitStatus.
const NoJobAliveExitCode = 3

// ErrNoJobAlive ends Supervise only for a terminal finding: jobs are down, none is alive,
// and none can regain restart eligibility. Its text carries the jobs, even if output fails.
var ErrNoJobAlive = errors.New("supervisor: no job alive")

// ErrFindingLineNotWritten marks a terminal finding whose output failed or timed out.
// A recoverable finding's failed write leaves the episode unreported for the next tick.
var ErrFindingLineNotWritten = errors.New("supervisor: finding line not written")

// downJob is a job that is down, with the restart count its budget was judged on.
type downJob struct {
	job      config.JobConfig
	st       eventlog.Job
	restarts int
	due      time.Time
}

// jobAlive checks running evidence first. Once jobs have no future supervised run;
// cron/event jobs remain armed, and an intentional stop excludes an always job.
func (s *Supervisor) jobAlive(job config.JobConfig, st eventlog.Job, now time.Time) bool {
	if s.statusFor(job, st).State == StateRunning || st.State == string(StateRunning) {
		return true
	}
	if job.Schedule == "once" {
		return false
	}
	if job.Schedule != "always" {
		return true
	}
	if st.State == string(StateStopped) {
		return false
	}
	if st.LastEvent == "" {
		return true
	}
	return shouldRestart(job, st, now)
}

// downJobsIfNoneAlive returns the down jobs, in config order, when at least one configured
// job is down and none is alive; otherwise nil. A job an operator stopped is neither alive
// nor down: a stop is intentional, not a failure. A non-running once job with no record
// or a last end with exit code 0 is excluded: supervision never starts it, or it completed.
// A stopped once job is excluded; every other once end is down with no recovery time.
func (s *Supervisor) downJobsIfNoneAlive(state eventlog.State, now time.Time) []downJob {
	var down []downJob
	for i := 0; i < len(s.cfg.Jobs); i++ {
		job := s.cfg.Jobs[i]
		st := state.Jobs[job.Name]
		if s.jobAlive(job, st, now) {
			return nil
		}
		if !excludedFromFinding(job, st) {
			var due time.Time
			if job.Schedule != "once" {
				if at, recoverable := restartDueAt(job, st, now); recoverable {
					due = at
				}
			}
			down = append(down, downJob{job: job, st: st, restarts: restartsInWindow(job, st, now), due: due})
		}
	}
	return down
}

func excludedFromFinding(job config.JobConfig, st eventlog.Job) bool {
	if st.State == string(StateStopped) {
		return true
	}
	return job.Schedule == "once" && (st.LastEvent == "" || (st.LastExitCode != nil && *st.LastExitCode == 0))
}

// checkNoJobAlive replays only a candidate finding, closing the stale-reconcile gap.
// An episode emits at most one non-terminal line; a terminal finding always emits its line.
// The episode ends when the finding no longer holds.
// A concurrent start remains invisible until its shim holds the lock or records its start;
// this gap ends at that evidence. The start caller's wait has a context and poll bound,
// but it does not serialize other processes' starts with this judgment.
func (s *Supervisor) checkNoJobAlive(ctx context.Context, state eventlog.State, reported *bool) error {
	if len(s.downJobsIfNoneAlive(state, s.clk.Now())) == 0 {
		*reported = false
		return nil
	}
	fresh, err := s.replayState(ctx)
	if err != nil {
		return err
	}
	down := s.downJobsIfNoneAlive(fresh, s.clk.Now())
	if len(down) == 0 {
		*reported = false
		return nil
	}
	return s.reportNoJobAlive(ctx, down, reported)
}

// findingDetail preserves configuration order and marks recoverable jobs with their due time.
func findingDetail(down []downJob) (string, bool) {
	parts := make([]string, 0, len(down))
	terminal := true
	for i := 0; i < len(down); i++ {
		d := down[i]
		part := fmt.Sprintf("%s dead (exit %s, restarts %d/%d in %ds",
			foldWhitespace(d.job.Name), exitText(d.st), d.restarts, d.job.Restart.MaxRestarts, restartWindowSeconds(d.job))
		if !d.due.IsZero() {
			terminal = false
			part += ", restart due " + d.due.UTC().Format(time.RFC3339Nano)
		}
		parts = append(parts, part+")")
	}
	return strings.Join(parts, ", "), terminal
}

// reportNoJobAlive emits once per episode plus any terminal finding.
// Recoverable budgets keep supervision active.
func (s *Supervisor) reportNoJobAlive(ctx context.Context, down []downJob, reported *bool) error {
	detail, terminal := findingDetail(down)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !*reported || terminal {
		tail := "supervise keeps running"
		if terminal {
			tail = fmt.Sprintf("supervise exits %d", NoJobAliveExitCode)
		}
		line := fmt.Sprintf("epitrop supervisor: finding: no job alive: %s; %s", detail, tail)
		if err := writeLine(s.errOut(), line, s.writeTimeout); err != nil {
			if !terminal {
				return nil
			}
			return fmt.Errorf("%w: %s (%w: %w)", ErrNoJobAlive, detail, ErrFindingLineNotWritten, err)
		}
		*reported = true
	}
	if terminal {
		return fmt.Errorf("%w: %s", ErrNoJobAlive, detail)
	}
	return nil
}

// exitText is how a down job's last end reads: its exit code, otherwise its reason (lost,
// or a refusal), otherwise unknown. Whitespace is folded so the finding stays one line.
func exitText(st eventlog.Job) string {
	if st.LastExitCode != nil {
		return strconv.Itoa(*st.LastExitCode)
	}
	if reason := foldWhitespace(st.LastReason); reason != "" {
		return reason
	}
	return "unknown"
}

func foldWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
