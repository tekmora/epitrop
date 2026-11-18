package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
)

// NoJobAliveExitCode is the exit status `epitropctl jobs supervise` ends with when
// Supervise returns ErrNoJobAlive. A host unit can name it in RestartPreventExitStatus.
const NoJobAliveExitCode = 3

// ErrNoJobAlive ends Supervise when at least one configured job is down and none is alive:
// nothing is left to supervise, and a supervisor that kept ticking would leave the host unit
// active for weeks. The error text carries the down jobs.
var ErrNoJobAlive = errors.New("supervisor: no job alive")

// downJob is a job that is down, with the restart count its budget was judged on.
type downJob struct {
	job      config.JobConfig
	st       eventlog.Job
	restarts int
}

// errOut is the writer for what the supervisor tells its operator.
func (s *Supervisor) errOut() io.Writer {
	if s.stderr == nil {
		return os.Stderr
	}
	return s.stderr
}

// jobAlive reports whether job may still run, judged on the state a reconcile tick
// replayed. A job that is not `always` is armed between runs. A running job lives, and one
// whose record still says running has its exit recorded by this tick. A job with no record
// is about to start, and one that shouldRestart is about to restart.
func (s *Supervisor) jobAlive(job config.JobConfig, st eventlog.Job, now time.Time) bool {
	if job.Schedule != "always" {
		return true
	}
	if s.statusFor(job, st).State == StateRunning || st.State == string(StateRunning) {
		return true
	}
	if st.LastEvent == "" {
		return true
	}
	return shouldRestart(job, st, now)
}

// downJobsIfNoneAlive returns the down jobs, in config order, when at least one configured
// job is down and none is alive; otherwise nil. A job an operator stopped is neither alive
// nor down: a stop is intentional, not a failure.
func (s *Supervisor) downJobsIfNoneAlive(state eventlog.State, now time.Time) []downJob {
	var down []downJob
	for i := 0; i < len(s.cfg.Jobs); i++ {
		job := s.cfg.Jobs[i]
		st := state.Jobs[job.Name]
		if s.jobAlive(job, st, now) {
			return nil
		}
		if st.State != string(StateStopped) {
			down = append(down, downJob{job: job, st: st, restarts: restartsInWindow(job, st, now)})
		}
	}
	return down
}

// reportNoJobAlive writes the finding line to the supervisor's stderr, which is the unit
// journal under a host unit, and returns the error that ends Supervise.
func (s *Supervisor) reportNoJobAlive(down []downJob) error {
	parts := make([]string, 0, len(down))
	for i := 0; i < len(down); i++ {
		d := down[i]
		parts = append(parts, fmt.Sprintf("%s dead (exit %s, restarts %d/%d in %ds)",
			d.job.Name, exitText(d.st), d.restarts, d.job.Restart.MaxRestarts, d.job.Restart.WindowSeconds))
	}
	detail := strings.Join(parts, ", ")
	line := fmt.Sprintf("epitrop supervisor: finding: no job alive: %s; supervise exits %d", detail, NoJobAliveExitCode)
	if _, err := fmt.Fprintln(s.errOut(), line); err != nil {
		return fmt.Errorf("%w: %s (finding line not written: %w)", ErrNoJobAlive, detail, err)
	}
	return fmt.Errorf("%w: %s", ErrNoJobAlive, detail)
}

// exitText is how a down job's last end reads: its exit code, otherwise its reason (lost,
// or a refusal), otherwise unknown. Whitespace is folded so the finding stays one line.
func exitText(st eventlog.Job) string {
	if st.LastExitCode != nil {
		return strconv.Itoa(*st.LastExitCode)
	}
	if reason := strings.Join(strings.Fields(st.LastReason), " "); reason != "" {
		return reason
	}
	return "unknown"
}
