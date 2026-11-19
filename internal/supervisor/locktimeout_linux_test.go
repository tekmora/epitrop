//go:build linux

package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// The supervisor reads and writes the event log under the configured lock timeout. Zero is
// passed on as zero, which the log reads as its own default.
func TestEventLogLimitsCarryTheConfiguredLockTimeout(t *testing.T) {
	cases := []struct {
		seconds int
		want    time.Duration
	}{
		{0, 0},
		{1, time.Second},
		{5, 5 * time.Second},
		{300, 300 * time.Second},
	}
	sup := testSupervisor(t, testJob("limits", []string{"/bin/true"}))
	for i := 0; i < len(cases); i++ {
		sup.cfg.EventLog.LockTimeoutSeconds = cases[i].seconds
		got := sup.eventLogLimits()
		if got.LockTimeout != cases[i].want {
			t.Errorf("lock_timeout_seconds %d: limits.LockTimeout = %s, want %s", cases[i].seconds, got.LockTimeout, cases[i].want)
		}
		if got.Clock == nil || got.MaxReplayRecords != sup.cfg.EventLog.MaxReplayRecords {
			t.Errorf("lock_timeout_seconds %d: limits = %+v, want the supervisor's clock and replay bound", cases[i].seconds, got)
		}
	}
}

// The shim appends the job's events, so it gets the same timeout: the supervisor passes it
// on the command line and the shim opens its log with it.
func TestShimIsStartedWithTheConfiguredLockTimeout(t *testing.T) {
	job := testJob("timeout-arg", []string{"/bin/true"})
	sup := testSupervisor(t, job)
	sup.cfg.EventLog.LockTimeoutSeconds = 42
	args := sup.shimCmd(testContext(t), job).Args
	if !hasArgPair(args, "--event-log-lock-timeout-seconds", "42") {
		t.Fatalf("shim args = %q, want --event-log-lock-timeout-seconds 42", args)
	}
	cfg, err := parseShimArgs(args[lastArgIndex(args, "__job-shim"):])
	if err != nil {
		t.Fatalf("parseShimArgs() = %v, want nil", err)
	}
	if cfg.EventLogLockTimeoutSeconds != 42 {
		t.Fatalf("shim config lock timeout = %d, want 42", cfg.EventLogLockTimeoutSeconds)
	}
	if got := shimEventLogLimits(cfg, clockwork.NewFakeClock()).LockTimeout; got != 42*time.Second {
		t.Fatalf("shim limits.LockTimeout = %s, want 42s", got)
	}
}

func TestShimCarriesConfiguredReplayLimit(t *testing.T) {
	if got := shimEventLogLimits(shimConfig{}, clockwork.NewFakeClock()).MaxReplayRecords; got != 0 {
		t.Fatalf("default shim replay limit = %d, want event log default", got)
	}
	job := testJob("replay-limit", []string{"/bin/true"})
	sup := testSupervisor(t, job)
	sup.cfg.EventLog.MaxReplayRecords = 200000
	args := sup.shimCmd(testContext(t), job).Args
	if !hasArgPair(args, "--event-log-max-replay-records", "200000") {
		t.Fatalf("shim args = %q, want configured replay limit", args)
	}
	cfg, err := parseShimArgs(args[lastArgIndex(args, "__job-shim"):])
	if err != nil {
		t.Fatalf("parseShimArgs() = %v", err)
	}
	if got := shimEventLogLimits(cfg, clockwork.NewFakeClock()).MaxReplayRecords; got != 200000 {
		t.Fatalf("shim replay limit = %d, want 200000", got)
	}
	appendShimTestEvent(t, cfg, "job.started", `{"state":"running","pid":10,"shim_pid":9,"since":"t1"}`)
	appendShimTestEvent(t, cfg, "job.exited", `{"state":"dead","code":0}`)
	cfg.UnlessStopped = true
	if err := checkShimNotStopped(cfg); err != nil {
		t.Fatalf("replay within configured limit = %v", err)
	}
	cfg.EventLogMaxReplayRecords = 1
	if err := checkShimNotStopped(cfg); err == nil {
		t.Fatal("replay beyond configured limit succeeded")
	}
}

// A shim started without the flag keeps the log's default; one started with a negative value
// is refused by the log, not run with a wait nobody chose.
func TestShimLockTimeoutDefaultAndRefusal(t *testing.T) {
	job := testJob("timeout-default", []string{"/bin/true"})
	sup := testSupervisor(t, job)
	full := sup.shimCmd(testContext(t), job).Args
	args := make([]string, 0, len(full))
	for i := lastArgIndex(full, "__job-shim"); i < len(full); i++ {
		if full[i] == "--event-log-lock-timeout-seconds" {
			i++
			continue
		}
		args = append(args, full[i])
	}
	cfg, err := parseShimArgs(args)
	if err != nil {
		t.Fatalf("parseShimArgs(no timeout flag) = %v, want nil", err)
	}
	if got := shimEventLogLimits(cfg, clockwork.NewFakeClock()).LockTimeout; got != 0 {
		t.Fatalf("shim limits.LockTimeout without the flag = %s, want 0 (the log's default)", got)
	}
	rt := testRuntime(t)
	_, err = shimWriter(shimConfig{EventLogDir: rt.EventLogDir, SigningKey: rt.KeyPath, EventLogLockTimeoutSeconds: -1})
	if err == nil || !strings.Contains(err.Error(), "lock timeout must be non-negative") {
		t.Fatalf("shimWriter(lock timeout -1) = %v, want the log's refusal of a negative lock timeout", err)
	}
}
