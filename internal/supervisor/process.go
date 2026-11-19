package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

func (s *Supervisor) shimCmd(ctx context.Context, job config.JobConfig) *exec.Cmd {
	return s.shimCmdFor(ctx, job, false)
}

func (s *Supervisor) shimCmdFor(ctx context.Context, job config.JobConfig, unlessStopped bool) *exec.Cmd {
	args := make([]string, 0, len(s.shimCommand)+len(job.Command)+19)
	args = append(args, s.shimCommand[1:]...)
	args = append(args,
		"__job-shim",
		"--event-log-dir", s.cfg.EventLog.Dir,
		"--event-log-lock-timeout-seconds", strconv.Itoa(s.cfg.EventLog.LockTimeoutSeconds),
		"--event-log-max-replay-records", strconv.Itoa(s.cfg.EventLog.MaxReplayRecords),
		"--signing-key", s.cfg.EventLog.SigningKeyPath,
		"--name", job.Name,
		"--lock", s.lockPath(job.Name),
		"--record", s.recordPath(job.Name),
		"--log", job.LogPath,
		"--max-runtime-seconds", strconv.Itoa(job.MaxRuntimeSeconds),
		"--stop-signal", job.Stop.Signal,
		"--stop-grace-seconds", strconv.Itoa(job.Stop.GraceSeconds),
	)
	if unlessStopped {
		args = append(args, "--unless-stopped")
	}
	args = append(args, sandboxFlagArgs(job.Sandbox)...)
	args = append(args, "--")
	args = append(args, job.Command...)
	// #nosec G204 -- the shim path is this executable by default, and job
	// command arguments are passed as argv after "--"; no shell is invoked.
	cmd := exec.CommandContext(ctx, s.shimCommand[0], args...)
	cmd.Cancel = nil
	return cmd
}

// waitStopTarget retries the lock while a starting shim publishes its record.
func (s *Supervisor) waitStopTarget(ctx context.Context, job config.JobConfig) (*heldLock, jobSignalTarget, error) {
	last := errors.New("job lock is held without a record")
	for i := 0; i < maxStatusPolls; i++ {
		lock, err := holdLock(s.lockPath(job.Name))
		if err == nil {
			return lock, jobSignalTarget{}, nil
		}
		if !errors.Is(err, errLockBusy) {
			return nil, jobSignalTarget{}, err
		}
		target, err := s.signalTarget(job)
		if err != nil {
			last = err
		} else if err = validateSignalTarget(target); err == nil {
			return nil, target, nil
		} else if errors.Is(err, errSignalRefused) {
			last = err
		} else {
			return nil, jobSignalTarget{}, fmt.Errorf("validate stop target: %w", err)
		}
		if !sleepContext(ctx, startPollInterval) {
			return nil, jobSignalTarget{}, errors.Join(last, ctx.Err())
		}
	}
	return nil, jobSignalTarget{}, last
}

func (s *Supervisor) isRunning(job config.JobConfig) (bool, error) {
	return lockHeld(s.lockPath(job.Name))
}

func (s *Supervisor) waitStopped(ctx context.Context, job config.JobConfig) bool {
	grace := job.Stop.GraceSeconds
	if grace < 0 {
		grace = 0
	}
	limit := grace*100 + 1
	for i := 0; i < limit; i++ {
		running, err := s.isRunning(job)
		if err == nil && !running {
			return true
		}
		if grace <= 0 {
			return false
		}
		if !sleepContext(ctx, 10*time.Millisecond) {
			return false
		}
	}
	return false
}

func (s *Supervisor) waitUnlocked(ctx context.Context, job config.JobConfig, limit int) bool {
	for i := 0; i < limit; i++ {
		running, err := s.isRunning(job)
		if err == nil && !running {
			return true
		}
		if !sleepContext(ctx, 10*time.Millisecond) {
			return false
		}
	}
	return false
}
