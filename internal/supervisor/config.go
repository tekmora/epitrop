package supervisor

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

func checkConfig(cfg config.Config) error {
	if cfg.EventLog.Dir == "" {
		return fmt.Errorf("supervisor: event_log.dir is required")
	}
	if cfg.EventLog.SigningKeyPath == "" {
		return fmt.Errorf("supervisor: event_log.signing_key_path is required")
	}
	if len(cfg.Jobs) > config.MaxJobs {
		return fmt.Errorf("supervisor: jobs exceeds %d", config.MaxJobs)
	}
	for i := 0; i < len(cfg.Jobs); i++ {
		if err := checkJob(cfg.Jobs[i]); err != nil {
			return fmt.Errorf("supervisor: jobs/%d: %w", i, err)
		}
	}
	return nil
}

func normalizeConfig(cfg config.Config) config.Config {
	for i := 0; i < len(cfg.Jobs); i++ {
		cfg.Jobs[i] = normalizeJob(cfg.Jobs[i])
	}
	return cfg
}

func checkJob(job config.JobConfig) error {
	if job.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(job.Command) == 0 || len(job.Command) > config.MaxJobArgs {
		return fmt.Errorf("command must have 1..%d args", config.MaxJobArgs)
	}
	if job.LogPath == "" {
		return fmt.Errorf("log_path is required")
	}
	if err := checkJobBounds(job); err != nil {
		return err
	}
	if err := config.ValidateJobTrigger(job); err != nil {
		return err
	}
	if err := config.ValidateSandbox(job.Sandbox); err != nil {
		return fmt.Errorf("sandbox.%w", err)
	}
	return nil
}

func checkJobBounds(job config.JobConfig) error {
	bounds := []struct {
		name       string
		value, max int
	}{
		{"restart.max_restarts", job.Restart.MaxRestarts, config.MaxJobRestarts},
		{"restart.backoff_seconds", job.Restart.BackoffSeconds, config.MaxRestartBackoff},
		{"stop.grace_seconds", job.Stop.GraceSeconds, config.MaxStopGraceSeconds},
		{"max_runtime_seconds", job.MaxRuntimeSeconds, config.MaxJobRuntimeSeconds},
		{"heartbeat.max_age_seconds", job.Heartbeat.MaxAgeSeconds, config.MaxHeartbeatSeconds},
	}
	for i := 0; i < len(bounds); i++ {
		if bounds[i].value < 0 || bounds[i].value > bounds[i].max {
			return fmt.Errorf("%s must be 0..%d", bounds[i].name, bounds[i].max)
		}
	}
	return nil
}

func (s *Supervisor) selectedJobs(name string) ([]config.JobConfig, error) {
	if name != "" {
		job, err := s.job(name)
		if err != nil {
			return nil, err
		}
		return []config.JobConfig{job}, nil
	}
	return s.cfg.Jobs, nil
}

func (s *Supervisor) job(name string) (config.JobConfig, error) {
	for i := 0; i < len(s.cfg.Jobs); i++ {
		if s.cfg.Jobs[i].Name == name {
			return normalizeJob(s.cfg.Jobs[i]), nil
		}
	}
	return config.JobConfig{}, fmt.Errorf("%w: %s", ErrUnknownJob, name)
}

func normalizeJob(job config.JobConfig) config.JobConfig {
	if job.Schedule == "" {
		job.Schedule = "always"
	}
	if job.Restart.Policy == "" {
		job.Restart.Policy = "never"
	}
	if job.Stop.Signal == "" {
		job.Stop.Signal = "TERM"
	}
	job.Sandbox = config.NormalizeSandbox(job.Sandbox)
	return job
}

func readyContext(ctx context.Context, op string) error {
	if ctx == nil {
		return fmt.Errorf("supervisor: %s requires context", op)
	}
	if _, ok := ctx.Deadline(); !ok {
		return fmt.Errorf("supervisor: %s requires context deadline", op)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("supervisor: %s context: %w", op, err)
	}
	return nil
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Supervisor) jobsDir() string {
	return filepath.Join(s.cfg.EventLog.Dir, "jobs")
}

func (s *Supervisor) lockPath(name string) string {
	return filepath.Join(s.jobsDir(), name+".lock")
}

func (s *Supervisor) recordPath(name string) string {
	return filepath.Join(s.jobsDir(), name+".json")
}
