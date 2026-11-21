//go:build linux

package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/jonboulle/clockwork"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
)

// TestRefusedStartIsAnEvent: whatever stops the shim before it records a start, the caller
// learns it from the event log and from Start's error, without reading the job's log.
func TestRefusedStartIsAnEvent(t *testing.T) {
	t.Run("command cannot start", func(t *testing.T) {
		job := testJob("nocommand", []string{filepath.Join(t.TempDir(), "missing-command")})
		job.LogPath = filepath.Join(t.TempDir(), "nocommand.log")
		sup := testSupervisor(t, job)
		assertRefusedStart(t, sup, job.Name, job.LogPath, "job-shim: start ")
	})
	t.Run("sandbox cannot be built", func(t *testing.T) {
		job := sandboxedSupervisorJob(t, "noworkspace", `true`)
		requireSandboxToolchain(t, job.Sandbox)
		sup := testSupervisor(t, job)
		if err := os.Remove(job.Sandbox.Workspace); err != nil {
			t.Fatalf("Remove(workspace) = %v, want nil", err)
		}
		assertRefusedStart(t, sup, job.Name, job.LogPath, "job-shim: sandbox noworkspace: ")
	})
	t.Run("proxy cannot start", func(t *testing.T) {
		job := sandboxedSupervisorJob(t, "noproxy", `true`)
		job.Sandbox.Network, job.Sandbox.Destinations = "proxy", []string{"gateway.example:443"}
		requireSandboxToolchain(t, job.Sandbox)
		sup := testSupervisor(t, job)
		// The shim makes the directory of the proxy's socket where TMPDIR says; here that is nowhere.
		t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
		assertRefusedStart(t, sup, job.Name, job.LogPath, "egress proxy: socket directory: ")
	})
}

func assertRefusedStart(t *testing.T, sup *Supervisor, name string, logPath string, wantReason string) {
	t.Helper()
	err := sup.Start(testContext(t), name)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), name+": "+wantReason) {
		t.Fatalf("Start(%s) = %v, want ErrRefused with reason %q", name, err, wantReason)
	}
	job := eventlogJob(t, sup, name)
	checkRefusedJobEventLog(t, job, wantReason)
	log, err := os.ReadFile(logPath) // #nosec G304 -- logPath is the refused test job log
	if err != nil || !strings.Contains(string(log), "epitrop: job-shim: "+name+" refused: "+wantReason) {
		t.Fatalf("job log = %q, %v, want the refusal line", log, err)
	}
	waitForUnlock(t, sup, mustJob(t, sup, name))
}

func checkRefusedJobEventLog(t *testing.T, job eventlog.Job, wantReason string) {
	t.Helper()
	if job.LastEvent != "job.refused" || job.State != string(StateDead) || !strings.HasPrefix(job.LastReason, wantReason) {
		t.Fatalf("event log holds %+v, want job.refused, dead, reason %q", job, wantReason)
	}
	if job.Since != "" || job.ShimPID != 0 || job.RefusedShimPID == 0 || job.RefusedAt == "" {
		t.Fatalf("event log holds %+v, want the refusal's shim and time and no start", job)
	}
}

func mustJob(t *testing.T, sup *Supervisor, name string) config.JobConfig {
	t.Helper()
	found, err := sup.job(name)
	if err != nil {
		t.Fatalf("job(%s) = %v, want nil", name, err)
	}
	return found
}

func shimPublicKey(t *testing.T, cfg shimConfig) string {
	t.Helper()
	signer, err := eventlog.NewSignerFromKeyFile(testContext(t), cfg.SigningKey, cfg.EventLogDir, clockwork.NewRealClock())
	if err != nil {
		t.Fatalf("NewSignerFromKeyFile() = %v, want nil", err)
	}
	return signer.PublicKey()
}

func TestRefusedByNeedsThisShimAndANewTime(t *testing.T) {
	refused := eventlog.Job{State: string(StateDead), LastEvent: "job.refused", LastReason: "why", RefusedShimPID: 19, RefusedAt: "t2"}
	exited := refused
	exited.LastEvent = "job.exited"
	noTime := refused
	noTime.RefusedAt = ""
	cases := []struct {
		name   string
		job    eventlog.Job
		before eventlog.Job
		shim   int
		want   bool
	}{
		{"this shim, first refusal", refused, eventlog.Job{}, 19, true},
		{"this shim, refusal after an older one", refused, eventlog.Job{RefusedShimPID: 7, RefusedAt: "t1"}, 19, true},
		{"reused shim pid, refusal already seen", refused, eventlog.Job{RefusedShimPID: 19, RefusedAt: "t2"}, 19, false},
		{"another shim's refusal", refused, eventlog.Job{}, 20, false},
		{"a later event follows the refusal", exited, eventlog.Job{}, 19, false},
		{"refusal without a time", noTime, eventlog.Job{}, 19, false},
		{"refusal without a time after an older one", noTime, eventlog.Job{RefusedShimPID: 7, RefusedAt: "t1"}, 19, false},
		{"no refusal at all", eventlog.Job{}, eventlog.Job{}, 0, false},
	}
	for i := 0; i < len(cases); i++ {
		if got := refusedBy(cases[i].job, cases[i].before, cases[i].shim); got != cases[i].want {
			t.Errorf("refusedBy(%s) = %v, want %v", cases[i].name, got, cases[i].want)
		}
	}
	settled, err := startOutcome("probe", refused, eventlog.Job{}, 19)
	if !settled || !errors.Is(err, ErrRefused) || err.Error() != "supervisor: job refused: probe: why" {
		t.Fatalf("startOutcome(refused) = %v, %v, want settled with ErrRefused naming job and reason", settled, err)
	}
	started := eventlog.Job{ShimPID: 19, Since: "t2", LastEvent: "job.started"}
	if settled, err = startOutcome("probe", started, eventlog.Job{}, 19); !settled || err != nil {
		t.Fatalf("startOutcome(started) = %v, %v, want settled without error", settled, err)
	}
	if settled, err = startOutcome("probe", eventlog.Job{}, eventlog.Job{}, 19); settled || err != nil {
		t.Fatalf("startOutcome(nothing recorded) = %v, %v, want not settled", settled, err)
	}
}

// TestJobRefusedKeepsTheLastRecordedStart: a refusal says the job is not running and why.
// The shim and time of the last recorded start stay, so an old start is never read as new.
func TestJobRefusedKeepsTheLastRecordedStart(t *testing.T) {
	st := eventlog.State{}
	events := []eventlog.Record{
		{Seq: 1, Type: "job.started", TaskID: "daemon", Payload: json.RawMessage(`{"state":"running","pid":21,"shim_pid":20,"since":"t1","sandbox":"enforce/none"}`)},
		{Seq: 2, Type: "job.exited", TaskID: "daemon", Payload: json.RawMessage(`{"state":"dead","code":3}`)},
		{Seq: 3, Type: "job.refused", TaskID: "daemon", Payload: json.RawMessage(`{"state":"dead","shim_pid":30,"at":"t3","reason":"no scope","sandbox":"enforce/none"}`)},
	}
	var err error
	for i := 0; i < len(events); i++ {
		if st, err = JobReducer(st, events[i]); err != nil {
			t.Fatalf("JobReducer(%s) = %v, want nil", events[i].Type, err)
		}
	}
	job := st.Jobs["daemon"]
	checkRefusedJobKeptStart(t, job)
	checkRefusedJobRefusalFields(t, job)
	if _, err = JobReducer(st, eventlog.Record{Seq: 4, Type: "job.refused", TaskID: "daemon", Payload: json.RawMessage(`{`)}); err == nil {
		t.Fatalf("JobReducer(job.refused, broken payload) = nil, want an error")
	}
}

func checkRefusedJobKeptStart(t *testing.T, job eventlog.Job) {
	t.Helper()
	if job.ShimPID != 20 || job.Since != "t1" || job.PID != 21 {
		t.Fatalf("after job.refused the last start is %+v, want shim 20 since t1 kept", job)
	}
	if startedBy(job, eventlog.Job{}, 30) {
		t.Fatalf("startedBy(refusing shim) = true, want a refusal never read as a start")
	}
}

func checkRefusedJobRefusalFields(t *testing.T, job eventlog.Job) {
	t.Helper()
	if job.State != "dead" || job.LastEvent != "job.refused" || job.LastReason != "no scope" || job.LastExitCode != nil || job.RefusedShimPID != 30 || job.RefusedAt != "t3" {
		t.Fatalf("after job.refused the job is %+v, want dead, the reason, no exit code, shim 30 at t3", job)
	}
}

func TestRefuseJobReportsWhatItCouldNotRecord(t *testing.T) {
	cfg := validShimConfig(t, "probe")
	cause := errors.New("job-shim: sandbox probe: " + strings.Repeat("x", 3*maxRefusalReasonBytes))
	var log bytes.Buffer
	err := refuseJob(cfg, &log, cause)
	if err == nil || !errors.Is(err, cause) || strings.Contains(err.Error(), "record refusal") {
		t.Fatalf("refuseJob() = %.120v, want the cause and no recording error", err)
	}
	if !strings.HasPrefix(log.String(), "epitrop: job-shim: probe refused: job-shim: sandbox probe: ") {
		t.Fatalf("job log = %.80q, want the refusal line", log.String())
	}
	state, err := eventlog.Replay(testContext(t), cfg.EventLogDir, shimPublicKey(t, cfg), eventlog.Limits{Clock: clockwork.NewRealClock()}, JobReducer)
	if err != nil {
		t.Fatalf("Replay() = %v, want nil", err)
	}
	if got := state.Jobs["probe"]; got.LastEvent != "job.refused" || len(got.LastReason) != maxRefusalReasonBytes || got.RefusedShimPID != os.Getpid() {
		t.Fatalf("recorded refusal = %s shim %d reason of %d bytes, want job.refused by this process with the reason cut at %d", got.LastEvent, got.RefusedShimPID, len(got.LastReason), maxRefusalReasonBytes)
	}

	checkRefuseJobEventLogUnwritable(t, cause)
	checkRefuseJobJobLogUnwritable(t, cause)
}

func checkRefuseJobEventLogUnwritable(t *testing.T, cause error) {
	t.Helper()
	broken := validShimConfig(t, "broken")
	if err := os.WriteFile(filepath.Join(broken.EventLogDir, "events"), []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile(events file) = %v, want nil", err)
	}
	var log bytes.Buffer
	err := refuseJob(broken, &log, cause)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "job-shim: record refusal: ") {
		t.Fatalf("refuseJob(event log unwritable) = %.200v, want the cause joined with the recording error", err)
	}
	if log.Len() == 0 {
		t.Fatalf("job log is empty after a refusal that could not be recorded, want the refusal line")
	}
}

func checkRefuseJobJobLogUnwritable(t *testing.T, cause error) {
	t.Helper()
	deaf := validShimConfig(t, "deaf")
	err := refuseJob(deaf, failingWriter{}, cause)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "job-shim: write refusal to the job log: ") {
		t.Fatalf("refuseJob(job log unwritable) = %.200v, want the cause joined with the write error", err)
	}
	state, err := eventlog.Replay(testContext(t), deaf.EventLogDir, shimPublicKey(t, deaf), eventlog.Limits{Clock: clockwork.NewRealClock()}, JobReducer)
	if err != nil || state.Jobs["deaf"].LastEvent != "job.refused" {
		t.Fatalf("after a refusal with an unwritable job log: %+v, %v, want job.refused recorded", state.Jobs["deaf"], err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("log is gone") }

// TestRefusedJobThatHadStartedIsKilled: a job the shim started and then refused, here because
// its record cannot be written, does not keep running, and neither does anything it started.
// The shim's context ends with the shim and kills the job's first process on its own; only
// the kill of the process group reaches the child, which is the one this test looks for.
// Both are found by an argument only they carry, not by timing.
func TestRefusedJobThatHadStartedIsKilled(t *testing.T) {
	cfg := validShimConfig(t, "norecord")
	marker := filepath.Join(t.TempDir(), "refused-job-marker")
	cfg.Command = []string{"/bin/sh", "-c", `/bin/sh -c "sleep 30; :" "$0" & wait`, marker}
	blocker := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(state-file) = %v, want nil", err)
	}
	cfg.RecordPath = filepath.Join(blocker, "norecord.json")
	before := zombieChildren(t)
	err := errorWithoutPanic(t, "runShim", func() error { return runShim(cfg) })
	if err == nil || !strings.Contains(err.Error(), "norecord refused: ") || !strings.Contains(err.Error(), "mkdir state dir") {
		t.Fatalf("runShim(record unwritable) = %v, want the refusal naming the record error", err)
	}
	var alive []int
	waitUntil(func() bool {
		alive = liveProcessesWithArg(t, marker)
		return len(alive) == 0
	})
	if len(alive) > 0 {
		for i := 0; i < len(alive); i++ {
			assertSafeSignalPID(t, alive[i])
			if killErr := syscall.Kill(alive[i], syscall.SIGKILL); killErr != nil {
				t.Logf("cleanup kill %d = %v", alive[i], killErr)
			}
		}
		t.Fatalf("job processes %v still run after the shim refused the job, want them killed", alive)
	}
	// The shim also collects the job. Left uncollected, the watcher of the job's context would
	// outlive the shim's work on it and stop the job a second time when the context ends.
	after := zombieChildren(t)
	for pid := range after {
		if !before[pid] {
			comm, ppid, state := zombieChildDiagnostic(pid)
			t.Fatalf("pid %d (%s) is a new zombie child after the refusal (PPid %s, State %s), want the refused job collected", pid, comm, ppid, state)
		}
	}
}

// zombieChildDiagnostic returns comm, ppid and state from /proc/<pid>/status for error messages.
func zombieChildDiagnostic(pid int) (comm, ppid, state string) {
	body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return "unknown", "?", "?"
	}
	status := string(body)
	comm = procStatusField(status, "Name:")
	if comm == "" {
		comm = "unknown"
	}
	ppid = procStatusField(status, "PPid:")
	if ppid == "" {
		ppid = "?"
	}
	state = procStatusField(status, "State:")
	if state == "" {
		state = "?"
	}
	return comm, ppid, state
}

// zombieChildren returns the uncollected children of the test process.
func zombieChildren(t *testing.T) map[int]bool {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("ReadDir(/proc) = %v, want nil", err)
	}
	zombies := map[int]bool{}
	for i := 0; i < len(entries); i++ {
		pid, convErr := strconv.Atoi(entries[i].Name())
		if convErr == nil && zombieChildOfThisProcess(pid) {
			zombies[pid] = true
		}
	}
	return zombies
}

// liveProcessesWithArg returns the processes, zombies excepted, whose command line holds arg.
func liveProcessesWithArg(t *testing.T, arg string) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("ReadDir(/proc) = %v, want nil", err)
	}
	var pids []int
	for i := 0; i < len(entries); i++ {
		pid, convErr := strconv.Atoi(entries[i].Name())
		if convErr != nil || pid == os.Getpid() {
			continue
		}
		cmdline, readErr := os.ReadFile(filepath.Join("/proc", entries[i].Name(), "cmdline"))
		if readErr != nil || !bytes.Contains(cmdline, []byte(arg)) {
			continue
		}
		if processExists(pid) {
			pids = append(pids, pid)
		}
	}
	return pids
}
