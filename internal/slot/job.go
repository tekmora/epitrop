package slot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/supervisor"
)

const (
	// ConfigFile is the configuration of one run, in the run's output directory. It is what
	// `jobs status` and `jobs stop` take to reach the run's job.
	ConfigFile = "config.json"
	// LogFile is the log of the run's job, in the run's output directory.
	LogFile = "job.log"
	// MaxRunSeconds bounds the deadline a slot's template may give its job.
	MaxRunSeconds = 24 * 60 * 60
	// runSchedule is the schedule of a run's job. Any value but "always" keeps the
	// supervisor from starting the job again by itself.
	runSchedule = "once"
	// waitMarginSeconds is how long the runner waits beyond the job's deadline and stop
	// grace before it gives up: the supervisor ends the job, the runner only watches.
	waitMarginSeconds = 60
	// WaitInterval is how often a runner looks whether its job still runs.
	WaitInterval = 2 * time.Second
)

// RunConfig writes and loads the configuration of one run: the slot's template with the
// fields that belong to the run.
//
// A template is a configuration with exactly one job, sandboxed and with a deadline, that
// leaves out what only a run can know. The runner sets the job's log path, its sandbox
// workspace, its schedule and its restart policy; a template that sets one of them is
// refused, so that nobody reads a value there that is not used. The result is written to
// ConfigFile in the output directory and loaded like any configuration, so every check of
// the loader, the protected paths among them, holds for the run's own workspace.
func RunConfig(ctx context.Context, template string, ws Workspace, out string) (config.Config, string, error) {
	doc, err := config.ReadDocument(ctx, template)
	if err != nil {
		return config.Config{}, "", err
	}
	if err = completeTemplate(doc, ws, out); err != nil {
		return config.Config{}, "", fmt.Errorf("slot: template %s: %w", template, err)
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return config.Config{}, "", fmt.Errorf("slot: marshal the run's configuration: %w", err)
	}
	path := filepath.Join(out, ConfigFile)
	if err = writeNew(path, append(body, '\n')); err != nil {
		return config.Config{}, "", err
	}
	cfg, err := config.Load(ctx, path)
	if err != nil {
		return config.Config{}, "", fmt.Errorf("slot: template %s gives no valid configuration: %w", template, err)
	}
	if err = CheckRunJob(cfg.Jobs[0]); err != nil {
		return config.Config{}, "", fmt.Errorf("slot: template %s: %w", template, err)
	}
	return cfg, path, nil
}

// completeTemplate sets the run's fields in the template's one job.
func completeTemplate(doc map[string]any, ws Workspace, out string) error {
	jobs, isList := doc["jobs"].([]any)
	if _, set := doc["jobs"]; set && !isList {
		return errors.New("its jobs are no list")
	}
	if len(jobs) != 1 {
		return fmt.Errorf("it holds %d jobs, a slot's template holds exactly one", len(jobs))
	}
	job, ok := jobs[0].(map[string]any)
	if !ok {
		return errors.New("its job is no object")
	}
	sandbox, ok := job["sandbox"].(map[string]any)
	if !ok {
		if _, set := job["sandbox"]; set {
			return errors.New("its job's sandbox is no object")
		}
		sandbox = map[string]any{}
		job["sandbox"] = sandbox
	}
	for _, key := range []string{"log_path", "schedule", "restart"} {
		if _, set := job[key]; set {
			return fmt.Errorf("its job sets %s, which the runner sets for each run", key)
		}
	}
	if _, set := sandbox["workspace"]; set {
		return errors.New("its job sets sandbox.workspace, which the runner sets for each run")
	}
	job["log_path"] = filepath.Join(out, LogFile)
	job["schedule"] = runSchedule
	job["restart"] = map[string]any{"policy": "never", "max_restarts": 0}
	sandbox["workspace"] = ws.Dir
	return nil
}

// CheckRunJob refuses a job that a slot may not run: one without a sandbox, because a run
// is not trusted, and one without a deadline of its own, because nothing else ends a run
// that never ends.
func CheckRunJob(job config.JobConfig) error {
	if job.Sandbox.Mode != "enforce" {
		return fmt.Errorf("its job has sandbox.mode %q, a slot's job runs sandboxed", job.Sandbox.Mode)
	}
	if job.MaxRuntimeSeconds < 1 || job.MaxRuntimeSeconds > MaxRunSeconds {
		return fmt.Errorf("its job has max_runtime_seconds %d, a slot's job has a deadline of 1..%d", job.MaxRuntimeSeconds, MaxRunSeconds)
	}
	return nil
}

// Supervisor is what a run needs of the supervisor.
type Supervisor interface {
	Preflight(name string) error
	StartOnce(ctx context.Context, name string) error
	Status(ctx context.Context, name string) ([]supervisor.JobStatus, error)
	Recorded(ctx context.Context, name string) (supervisor.RecordedJob, error)
}

// Supervised is the job of one run under the supervisor.
type Supervised struct {
	sup      Supervisor
	name     string
	polls    int
	interval time.Duration
	since    string
	sandbox  config.SandboxConfig
}

// Name returns the job's name.
func (j *Supervised) Name() string { return j.name }

// Adopt makes the job the one an earlier runner prepared at preparedAt, so that Wait
// reports its exit without a second start. The supervisor keeps only the job's last
// recorded start; one from before preparedAt belongs to an earlier run of the same job, and
// its exit is not counted as this run's.
func (j *Supervised) Adopt(ctx context.Context, preparedAt string) error {
	rec, err := j.sup.Recorded(ctx, j.name)
	if err != nil {
		return err
	}
	prepTime, err := time.Parse(time.RFC3339Nano, preparedAt)
	if err != nil {
		return fmt.Errorf("slot: parse prepared_at: %w", err)
	}
	sinceTime, err := time.Parse(time.RFC3339Nano, rec.Since)
	if err == nil && sinceTime.Compare(prepTime) >= 0 {
		j.since = rec.Since
	}
	return nil
}

// NewSupervised returns the job of one run. The job is the one job of the run's
// configuration; sup is a supervisor made from that configuration. The runner looks every
// interval whether the job still runs, WaitInterval when interval is not positive, and for
// no longer than the job's deadline, its stop grace and a margin together.
func NewSupervised(sup Supervisor, job config.JobConfig, interval time.Duration) *Supervised {
	if interval <= 0 {
		interval = WaitInterval
	}
	wait := time.Duration(job.MaxRuntimeSeconds+job.Stop.GraceSeconds+waitMarginSeconds) * time.Second
	return &Supervised{sup: sup, name: job.Name, polls: int(wait/interval) + 1, interval: interval, sandbox: job.Sandbox}
}

// Binds returns what the job's sandbox makes visible to the job: the workspace, the inputs
// and the files of its home. A job without a sandbox sees what its user sees; it has none.
func (j *Supervised) Binds() []Bind {
	sandbox := j.sandbox
	if sandbox.Mode != "enforce" {
		return nil
	}
	binds := []Bind{{Source: sandbox.Workspace, Target: sandbox.Workspace, Mode: BindWrite}}
	for i := 0; i < len(sandbox.Inputs); i++ {
		binds = append(binds, Bind{Source: sandbox.Inputs[i], Target: sandbox.Inputs[i], Mode: BindRead})
	}
	home := sandbox.Home
	if home == nil {
		return binds
	}
	dir := filepath.Join(sandbox.Workspace, filepath.FromSlash(config.SandboxHomeDir))
	for i := 0; i < len(home.Copy); i++ {
		file := home.Copy[i].File
		binds = append(binds, Bind{Source: filepath.Join(home.Source, file), Target: filepath.Join(dir, file), Mode: BindCopy, SHA256: home.Copy[i].SHA256})
	}
	for i := 0; i < len(home.Bind); i++ {
		file := filepath.FromSlash(home.Bind[i].File)
		binds = append(binds, Bind{Source: filepath.Join(home.Source, file), Target: filepath.Join(dir, file), Mode: BindRead})
	}
	return binds
}

// Preflight refuses a slot whose job runs already, and a host that cannot start the job's
// sandbox.
func (j *Supervised) Preflight(ctx context.Context) error {
	if err := j.sup.Preflight(j.name); err != nil {
		return err
	}
	running, err := j.running(ctx)
	if err != nil {
		return err
	}
	if running {
		return fmt.Errorf("slot: the job %s runs already; one slot runs one unit at a time", j.name)
	}
	return nil
}

func (j *Supervised) running(ctx context.Context) (bool, error) {
	status, err := j.sup.Status(ctx, j.name)
	if err != nil {
		return false, err
	}
	return len(status) == 1 && status[0].State == supervisor.StateRunning, nil
}

// Start starts the job. A start that the supervisor refused, a job that runs already and a
// failed start after which no job of the slot runs all wrap ErrNotStarted.
func (j *Supervised) Start(ctx context.Context) error {
	err := j.sup.StartOnce(ctx, j.name)
	if err == nil {
		rec, recErr := j.sup.Recorded(ctx, j.name)
		if recErr != nil {
			return recErr
		}
		j.since = rec.Since
		return nil
	}
	if errors.Is(err, supervisor.ErrRefused) || errors.Is(err, supervisor.ErrRunning) {
		return fmt.Errorf("%w: %w", ErrNotStarted, err)
	}
	if running, statusErr := j.running(ctx); statusErr == nil && !running {
		return fmt.Errorf("%w: %w", ErrNotStarted, err)
	}
	return err
}

// Wait watches the job until it no longer runs, then reads its end from the event log. The
// supervisor's shim ends the job at its deadline; the wait is bounded a little beyond it.
func (j *Supervised) Wait(ctx context.Context) (JobEnd, error) {
	for i := 0; i < j.polls; i++ {
		running, err := j.running(ctx)
		if err != nil {
			return JobEnd{}, err
		}
		if !running {
			return j.end(ctx)
		}
		timer := time.NewTimer(j.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return JobEnd{}, fmt.Errorf("slot: wait for the job %s: %w", j.name, ctx.Err())
		case <-timer.C:
		}
	}
	return JobEnd{}, fmt.Errorf("slot: the job %s still runs after its deadline and stop grace; stop it with jobs stop", j.name)
}

// end reads how the job ended. The exit counts only when it belongs to the start this run
// made: the start time is what tells one start of the slot's job from another.
func (j *Supervised) end(ctx context.Context) (JobEnd, error) {
	rec, err := j.sup.Recorded(ctx, j.name)
	if err != nil {
		return JobEnd{}, err
	}
	if rec.LastEvent != "job.exited" || rec.Since != j.since {
		return JobEnd{Reason: "the job stopped running without its exit in the event log"}, nil
	}
	return JobEnd{Recorded: true, Code: rec.LastExitCode, Reason: rec.LastReason}, nil
}
