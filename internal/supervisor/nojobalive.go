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
	lostOnce bool
}

// findingEpisode tracks whether the active no-job-alive episode has emitted its
// recoverable finding line or still has an admitted write in flight.
type findingEpisode struct {
	reported bool
	pending  <-chan error
}

func (e *findingEpisode) reset() {
	*e = findingEpisode{}
}

// settlePending checks an admitted write that timed out on an earlier tick.
// It returns true when the write is still in flight.
func (e *findingEpisode) settlePending() bool {
	if e.pending == nil {
		return false
	}
	select {
	case err := <-e.pending:
		e.pending = nil
		e.reported = err == nil
		return false
	default:
		return true
	}
}

func (e *findingEpisode) shouldWrite(terminal bool) bool {
	if e.settlePending() && !terminal {
		return false
	}
	return terminal || !e.reported
}

var probeJobLock = lockHeld

// jobAlive checks running evidence first. Once jobs are alive only while their lock is held;
// An unreadable once lock defers the finding; cron/event jobs remain armed.
func (s *Supervisor) jobAlive(job config.JobConfig, st eventlog.Job, now time.Time) bool {
	if s.statusFor(job, st).State == StateRunning {
		return true
	}
	if job.Schedule == "once" {
		held, err := probeJobLock(s.lockPath(job.Name))
		return err != nil || held
	}
	if st.State == string(StateRunning) || job.Schedule != "always" {
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
// or a last end with exit code 0 (other than deadline) is excluded: supervision never starts
// it, or it completed. A stopped once job is excluded; every other once end is down with no
// recovery time, including a once job whose replay still says running after its lock is free.
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
			lostOnce := job.Schedule == "once" && st.State == string(StateRunning)
			down = append(down, downJob{job: job, st: st, restarts: restartsInWindow(job, st, now), due: due, lostOnce: lostOnce})
		}
	}
	return down
}

func excludedFromFinding(job config.JobConfig, st eventlog.Job) bool {
	if st.State == string(StateStopped) {
		return true
	}
	if job.Schedule != "once" || st.State == string(StateRunning) {
		return false
	}
	return st.LastEvent == "" || (st.LastExitCode != nil && *st.LastExitCode == 0 && st.LastReason != "deadline")
}

// checkNoJobAlive replays only a candidate finding, closing the stale-reconcile gap.
// An episode emits at most one non-terminal line; a terminal finding always attempts its line.
// The episode ends when the finding no longer holds.
func (s *Supervisor) checkNoJobAlive(ctx context.Context, state eventlog.State, episode *findingEpisode) error {
	if len(s.downJobsIfNoneAlive(state, s.clk.Now())) == 0 {
		episode.reset()
		return nil
	}
	fresh, err := s.replayState(ctx)
	if err != nil {
		return err
	}
	down := s.downJobsIfNoneAlive(fresh, s.clk.Now())
	if len(down) == 0 {
		episode.reset()
		return nil
	}
	down, confirmed, err := s.confirmLostOnce(ctx, down)
	if err != nil {
		return err
	}
	if !confirmed {
		return nil
	}
	if len(down) == 0 {
		episode.reset()
		return nil
	}
	return s.reportConfirmedDown(ctx, down, episode)
}

func (s *Supervisor) reportConfirmedDown(ctx context.Context, down []downJob, episode *findingEpisode) error {
	if ctx.Err() == nil {
		lost := make(map[string]string, len(down))
		for i := 0; i < len(down); i++ {
			if down[i].lostOnce {
				lost[down[i].job.Name] = down[i].st.Since
			}
		}
		s.rememberLostOnce(lost)
	}
	return s.reportNoJobAlive(ctx, down, episode)
}

// confirmLostOnce replays once more after a lock-free once candidate was seen, so a shim
// that recorded its exit just before releasing its lock is not reported as lost.
func (s *Supervisor) confirmLostOnce(ctx context.Context, down []downJob) ([]downJob, bool, error) {
	if !hasLostOnce(down) {
		return down, true, nil
	}
	if s.beforeConfirmReplay != nil {
		s.beforeConfirmReplay()
	}
	confirmed, err := s.replayState(ctx)
	if err != nil {
		return nil, false, err
	}
	confirmedDown := s.downJobsIfNoneAlive(confirmed, s.clk.Now())
	if len(confirmedDown) == 0 {
		return nil, true, nil
	}
	for i := 0; i < len(confirmedDown); i++ {
		if confirmedDown[i].lostOnce && !containsLostOnce(down, confirmedDown[i]) {
			return nil, false, nil
		}
	}
	if !sameLostOnceStarts(down, confirmed) {
		return nil, false, nil
	}
	return confirmedDown, true, nil
}

func sameLostOnceStarts(down []downJob, state eventlog.State) bool {
	for i := 0; i < len(down); i++ {
		if down[i].lostOnce {
			st := state.Jobs[down[i].job.Name]
			if st.State != string(StateRunning) || st.Since != down[i].st.Since {
				return false
			}
		}
	}
	return true
}

func containsLostOnce(down []downJob, candidate downJob) bool {
	for i := 0; i < len(down); i++ {
		if down[i].lostOnce && down[i].job.Name == candidate.job.Name && down[i].st.Since == candidate.st.Since {
			return true
		}
	}
	return false
}

func hasLostOnce(down []downJob) bool {
	for i := 0; i < len(down); i++ {
		if down[i].lostOnce {
			return true
		}
	}
	return false
}

// findingDetail preserves configuration order and marks recoverable jobs with their due time.
func findingDetail(down []downJob) (string, bool) {
	parts := make([]string, 0, len(down))
	terminal := true
	for i := 0; i < len(down); i++ {
		d := down[i]
		exit := exitText(d.st)
		if d.lostOnce {
			exit = "lost"
		}
		part := fmt.Sprintf("%s dead (exit %s, restarts %d/%d in %ds",
			foldWhitespace(d.job.Name), exit, d.restarts, d.job.Restart.MaxRestarts, restartWindowSeconds(d.job))
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
func (s *Supervisor) reportNoJobAlive(ctx context.Context, down []downJob, episode *findingEpisode) error {
	detail, terminal := findingDetail(down)
	if err := ctx.Err(); err != nil {
		return err
	}
	if episode.shouldWrite(terminal) {
		line := fmt.Sprintf("epitrop supervisor: finding: no job alive: %s; %s", detail, findingTail(terminal))
		pending, err := writeLine(ctx, s.errOut(), line, s.writeTimeout)
		episode.pending = pending
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if !terminal {
				return nil
			}
			return fmt.Errorf("%w: %s (%w: %w)", ErrNoJobAlive, detail, ErrFindingLineNotWritten, err)
		}
		episode.reported = true
	}
	if terminal {
		return fmt.Errorf("%w: %s", ErrNoJobAlive, detail)
	}
	return nil
}

func findingTail(terminal bool) string {
	if terminal {
		return fmt.Sprintf("supervise exits %d", NoJobAliveExitCode)
	}
	return "supervise keeps running"
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

// lostOnceCandidates samples only recorded once runs while supervision is active.
func (s *Supervisor) lostOnceCandidates(ctx context.Context, state eventlog.State) map[string]string {
	candidates := make(map[string]string, len(s.cfg.Jobs))
	for i := 0; i < len(s.cfg.Jobs); i++ {
		job := s.cfg.Jobs[i]
		st := state.Jobs[job.Name]
		if ctx.Err() != nil {
			break
		}
		if job.Schedule != "once" || st.State != string(StateRunning) || st.Since == "" {
			continue
		}
		held, err := probeJobLock(s.lockPath(job.Name))
		if err == nil && !held && ctx.Err() == nil {
			candidates[job.Name] = st.Since
		}
	}
	return candidates
}

// rememberLostOnce records observations that predate a completed poll or terminal finding.
func (s *Supervisor) rememberLostOnce(candidates map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lostOnceSince == nil {
		s.lostOnceSince = make(map[string]string, len(s.cfg.Jobs))
	}
	for name, since := range candidates {
		if s.observedSince[name] == since {
			s.lostOnceSince[name] = since
		}
	}
}
