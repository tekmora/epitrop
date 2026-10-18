package slot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/supervisor"
)

const templateJob = `"name": "slot-1", "command": ["/usr/bin/true"], "max_runtime_seconds": 600, "stop": {"signal": "TERM", "grace_seconds": 5}`

// testRoot is the directory every temporary directory of the test lies in, the run's
// workspace among them: the sandbox_input_roots entry of a test's template.
func testRoot(t *testing.T) string {
	t.Helper()
	return filepath.Dir(t.TempDir())
}

// writeTemplate writes a slot's template: the given document, with the test's directory as
// its sandbox_input_roots entry.
func writeTemplate(t *testing.T, document string) string {
	t.Helper()
	root, err := json.Marshal([]string{testRoot(t)})
	if err != nil {
		t.Fatalf("marshal the roots: %v", err)
	}
	rest := strings.TrimPrefix(document, "{")
	if !strings.HasPrefix(rest, "}") {
		rest = ", " + rest
	}
	document = `{"sandbox_input_roots": ` + string(root) + rest
	path := filepath.Join(t.TempDir(), "slot.json")
	if err = os.WriteFile(path, []byte(document+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(template) = %v, want nil", err)
	}
	return path
}

// runDirs makes a workspace and an output directory as Run has them when it asks for the job.
func runDirs(t *testing.T) (Workspace, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks() = %v, want nil", err)
	}
	return Workspace{Dir: dir, Branch: Branch(testUnit), Base: strings.Repeat("0a", 20)}, t.TempDir()
}

func TestRunConfigCompletesTheTemplateForOneRun(t *testing.T) {
	ws, out := runDirs(t)
	template := writeTemplate(t, `{"jobs": [{`+templateJob+`, "sandbox": {"network": "none", "memory_max": "1G"}}]}`)
	cfg, path, err := RunConfig(slotCtx(t), template, ws, out)
	if err != nil {
		t.Fatalf("RunConfig() = %v, want nil", err)
	}
	if path != filepath.Join(out, ConfigFile) || len(cfg.Jobs) != 1 {
		t.Fatalf("RunConfig() = %d jobs at %s, want one job at the run's config file", len(cfg.Jobs), path)
	}
	job := cfg.Jobs[0]
	assertCompletedJobValues(t, job, ws.Dir, out)
	assertCompletedConfigFileWritten(t, path)
	assertCompletedConfigReload(t, path, ws.Dir, job.LogPath)
}

func assertCompletedJobValues(t *testing.T, job config.JobConfig, wsDir, out string) {
	t.Helper()
	if job.Name != "slot-1" || job.LogPath != filepath.Join(out, LogFile) || job.Schedule != "once" || job.Restart.Policy != "never" || job.Restart.MaxRestarts != 0 {
		t.Fatalf("job = %+v, want the template's job with the run's log, run once and never restarted", job)
	}
	if job.Sandbox.Workspace != wsDir || job.Sandbox.Mode != "enforce" || job.Sandbox.MemoryMax != "1G" || job.MaxRuntimeSeconds != 600 {
		t.Fatalf("sandbox = %+v, want the run's workspace with the template's limits", job.Sandbox)
	}
}

func assertCompletedConfigFileWritten(t *testing.T, path string) {
	t.Helper()
	var written struct {
		Jobs []struct {
			Schedule string         `json:"schedule"`
			Restart  map[string]any `json:"restart"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &written); err != nil || len(written.Jobs) != 1 {
		t.Fatalf("the run's config file = %+v, %v; want one job", written, err)
	}
	if restart := written.Jobs[0].Restart; written.Jobs[0].Schedule != "once" || restart["policy"] != "never" || restart["max_restarts"] != float64(0) || len(restart) != 2 {
		t.Fatalf("the run's config file has schedule %q and restart %v, want once and never, written out", written.Jobs[0].Schedule, restart)
	}
}

func assertCompletedConfigReload(t *testing.T, path, wsDir, logPath string) {
	t.Helper()
	again, err := config.Load(slotCtx(t), path)
	if err != nil || len(again.Jobs) != 1 || again.Jobs[0].Sandbox.Workspace != wsDir || again.Jobs[0].LogPath != logPath {
		t.Fatalf("Load(run's config) = %+v, %v; want the same job", again.Jobs, err)
	}
}

// A template may be written in YAML, like any configuration.
func TestRunConfigTakesAYAMLTemplate(t *testing.T) {
	ws, out := runDirs(t)
	template := filepath.Join(t.TempDir(), "slot.yaml")
	body := "sandbox_input_roots: [" + testRoot(t) + "]\njobs:\n  - name: slot-1\n    command: [/usr/bin/true]\n    max_runtime_seconds: 600\n    sandbox:\n      network: none\n"
	if err := os.WriteFile(template, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(template) = %v, want nil", err)
	}
	cfg, _, err := RunConfig(slotCtx(t), template, ws, out)
	if err != nil || len(cfg.Jobs) != 1 || cfg.Jobs[0].Name != "slot-1" || cfg.Jobs[0].MaxRuntimeSeconds != 600 || cfg.Jobs[0].Sandbox.Workspace != ws.Dir {
		t.Fatalf("RunConfig(YAML template) = %+v, %v; want the template's job in the run's workspace", cfg.Jobs, err)
	}
}

func TestRunConfigRefusesATemplateThatIsNoSlotsTemplate(t *testing.T) {
	cases := []struct {
		name     string
		template string
		want     string
	}{
		{"no job", `{}`, "holds 0 jobs"},
		{"two jobs", `{"jobs": [{` + templateJob + `}, {"name": "other", "command": ["/usr/bin/true"]}]}`, "holds 2 jobs"},
		{"jobs that are no list", `{"jobs": {"name": "slot-1"}}`, "its jobs are no list"},
		{"a job that is no object", `{"jobs": ["slot-1"]}`, "its job is no object"},
		{"a sandbox that is no object", `{"jobs": [{` + templateJob + `, "sandbox": "none"}]}`, "sandbox is no object"},
		{"a log path of its own", `{"jobs": [{` + templateJob + `, "log_path": "/var/tmp/slot.log"}]}`, "sets log_path"},
		{"a schedule of its own", `{"jobs": [{` + templateJob + `, "schedule": "always"}]}`, "sets schedule"},
		{"a restart policy of its own", `{"jobs": [{` + templateJob + `, "restart": {"policy": "always"}}]}`, "sets restart"},
		{"a workspace of its own", `{"jobs": [{` + templateJob + `, "sandbox": {"workspace": "/var/tmp"}}]}`, "sets sandbox.workspace"},
		{"no sandbox", `{"jobs": [{` + templateJob + `, "sandbox": {"mode": "off", "reason": "trusted"}}]}`, "runs sandboxed"},
		{"the open internet", `{"jobs": [{` + templateJob + `, "sandbox": {"network": "egress"}}]}`, "reaches its gateway or nothing"},
		{"no deadline", `{"jobs": [{"name": "slot-1", "command": ["/usr/bin/true"]}]}`, "has a deadline of 1.."},
		{"a deadline above the bound", `{"jobs": [{"name": "slot-1", "command": ["/usr/bin/true"], "max_runtime_seconds": 86401}]}`, "has a deadline of 1.."},
		{"a key the configuration does not know", `{"jobs": [{` + templateJob + `, "harness": "x"}]}`, "gives no valid configuration"},
		{"no command", `{"jobs": [{"name": "slot-1", "max_runtime_seconds": 600}]}`, "gives no valid configuration"},
	}
	for i := 0; i < len(cases); i++ {
		ws, out := runDirs(t)
		_, _, err := RunConfig(slotCtx(t), writeTemplate(t, cases[i].template), ws, out)
		if err == nil || !strings.Contains(err.Error(), cases[i].want) {
			t.Errorf("%s: RunConfig() = %v, want an error holding %q", cases[i].name, err, cases[i].want)
		}
	}
	ws, out := runDirs(t)
	if _, _, err := RunConfig(slotCtx(t), filepath.Join(t.TempDir(), "missing.json"), ws, out); err == nil || !strings.Contains(err.Error(), "stat") {
		t.Fatalf("RunConfig(missing template) = %v, want it refused", err)
	}
	if _, _, err := RunConfig(slotCtx(t), writeTemplate(t, `{"jobs": [{`+templateJob+`}]}`), ws, filepath.Join(out, "missing")); err == nil {
		t.Fatal("RunConfig(no output directory) = nil, want it refused")
	}
	// The bound itself is allowed.
	ws, out = runDirs(t)
	template := writeTemplate(t, `{"jobs": [{"name": "slot-1", "command": ["/usr/bin/true"], "max_runtime_seconds": 86400}]}`)
	if cfg, _, err := RunConfig(slotCtx(t), template, ws, out); err != nil || cfg.Jobs[0].MaxRuntimeSeconds != MaxRunSeconds {
		t.Fatalf("RunConfig(deadline at the bound) = %v, want nil", err)
	}
}

// A job that reaches only its declared gateway is allowed, and its run keeps that gateway
// as its only destination.
func TestRunConfigKeepsTheDeclaredGatewayAsTheOnlyDestination(t *testing.T) {
	ws, out := runDirs(t)
	template := writeTemplate(t, `{"jobs": [{`+templateJob+`, "sandbox": {"network": "proxy", "destinations": ["127.0.0.1:4000"]}}]}`)
	cfg, _, err := RunConfig(slotCtx(t), template, ws, out)
	if err != nil {
		t.Fatalf("RunConfig(network proxy) = %v, want nil", err)
	}
	if got := cfg.Jobs[0].Sandbox; got.Network != "proxy" || len(got.Destinations) != 1 || got.Destinations[0] != "127.0.0.1:4000" {
		t.Fatalf("RunConfig(network proxy) sandbox = %+v, want network proxy to 127.0.0.1:4000 only", got)
	}
}

// A slot resumes a run from its saved configuration through the same check, so a saved
// configuration that reaches the open internet is refused there too.
func TestCheckRunJobLetsAJobReachItsGatewayOrNothing(t *testing.T) {
	cases := []struct {
		network string
		want    string
	}{
		{"none", ""},
		{"proxy", ""},
		{"egress", "reaches its gateway or nothing"},
		{"", "reaches its gateway or nothing"},
	}
	for i := 0; i < len(cases); i++ {
		job := config.JobConfig{Name: "slot-1", MaxRuntimeSeconds: 600}
		job.Sandbox.Mode = "enforce"
		job.Sandbox.Network = cases[i].network
		err := CheckRunJob(job)
		if cases[i].want == "" && err != nil {
			t.Errorf("CheckRunJob(network %q) = %v, want nil", cases[i].network, err)
		}
		if cases[i].want != "" && (err == nil || !strings.Contains(err.Error(), cases[i].want)) {
			t.Errorf("CheckRunJob(network %q) = %v, want an error holding %q", cases[i].network, err, cases[i].want)
		}
	}
}

// The run's configuration is loaded like any other, so the loader's guard holds for the
// run's workspace: a workspace that would hold the event log is refused.
func TestRunConfigKeepsTheEventLogOutOfTheWorkspace(t *testing.T) {
	ws, out := runDirs(t)
	template := writeTemplate(t, fmt.Sprintf(`{"event_log": {"dir": %q}, "jobs": [{`+templateJob+`}]}`, filepath.Join(ws.Dir, "log")))
	_, _, err := RunConfig(slotCtx(t), template, ws, out)
	if err == nil || !strings.Contains(err.Error(), "gives no valid configuration") || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("RunConfig(event log inside the workspace) = %v, want the loader's refusal of the workspace", err)
	}
}

// fakeSupervisor answers a Supervised job from a script.
type fakeSupervisor struct {
	preflightErr error
	startErr     error
	statusErr    error
	recordedErr  error
	running      []bool // one answer per Status call; the last one repeats
	recorded     supervisor.RecordedJob
	statusCalls  int
	started      int
}

func (f *fakeSupervisor) Preflight(string) error { return f.preflightErr }

func (f *fakeSupervisor) StartOnce(context.Context, string) error {
	f.started++
	return f.startErr
}

func (f *fakeSupervisor) Status(_ context.Context, name string) ([]supervisor.JobStatus, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	i := min(f.statusCalls, len(f.running)-1)
	f.statusCalls++
	state := supervisor.StateDead
	if len(f.running) > 0 && f.running[i] {
		state = supervisor.StateRunning
	}
	return []supervisor.JobStatus{{Name: name, State: state}}, nil
}

func (f *fakeSupervisor) Recorded(context.Context, string) (supervisor.RecordedJob, error) {
	return f.recorded, f.recordedErr
}

func supervised(f *fakeSupervisor, polls int) *Supervised {
	return &Supervised{sup: f, name: "slot-1", polls: polls, interval: time.Millisecond}
}

func TestSupervisedPreflightRefusesABusySlotAndAHostWithoutTheSandbox(t *testing.T) {
	if err := supervised(&fakeSupervisor{}, 1).Preflight(slotCtx(t)); err != nil {
		t.Fatalf("Preflight(idle slot) = %v, want nil", err)
	}
	if err := supervised(&fakeSupervisor{preflightErr: errors.New("sandbox: bwrap: not found")}, 1).Preflight(slotCtx(t)); err == nil || !strings.Contains(err.Error(), "bwrap") {
		t.Fatalf("Preflight(no sandbox tools) = %v, want the supervisor's refusal", err)
	}
	if err := supervised(&fakeSupervisor{running: []bool{true}}, 1).Preflight(slotCtx(t)); err == nil || !strings.Contains(err.Error(), "runs already") {
		t.Fatalf("Preflight(busy slot) = %v, want it refused", err)
	}
	if err := supervised(&fakeSupervisor{statusErr: errors.New("planted: no replay")}, 1).Preflight(slotCtx(t)); err == nil || !strings.Contains(err.Error(), "planted: no replay") {
		t.Fatalf("Preflight(status fails) = %v, want the error", err)
	}
}

// Only a start after which no job of this run exists counts as not started. Every other
// failed start leaves the job's fate open, and Run then records nothing.
func TestSupervisedStartSaysWhenNothingWasStarted(t *testing.T) {
	planted := errors.New("planted: the wait for the start ended")
	cases := []struct {
		name       string
		sup        fakeSupervisor
		notStarted bool
	}{
		{"a refused start", fakeSupervisor{startErr: fmt.Errorf("%w: slot-1: no scope", supervisor.ErrRefused), running: []bool{true}}, true},
		{"a job that runs already", fakeSupervisor{startErr: fmt.Errorf("%w: slot-1", supervisor.ErrRunning), running: []bool{true}}, true},
		{"a failed start and no job running", fakeSupervisor{startErr: planted, running: []bool{false}}, true},
		{"a failed start and a job running", fakeSupervisor{startErr: planted, running: []bool{true}}, false},
		{"a failed start and no answer about the job", fakeSupervisor{startErr: planted, statusErr: errors.New("planted: no replay")}, false},
	}
	for i := 0; i < len(cases); i++ {
		err := supervised(&cases[i].sup, 1).Start(slotCtx(t))
		if err == nil || errors.Is(err, ErrNotStarted) != cases[i].notStarted {
			t.Errorf("%s: Start() = %v, want an error; not started = %t", cases[i].name, err, cases[i].notStarted)
		}
	}
	started := fakeSupervisor{recorded: supervisor.RecordedJob{Since: "2026-10-11T10:00:00Z", LastEvent: "job.started"}}
	job := supervised(&started, 1)
	if err := job.Start(slotCtx(t)); err != nil || job.since != "2026-10-11T10:00:00Z" || started.started != 1 {
		t.Fatalf("Start() = %v with start time %q after %d starts; want nil, the recorded start time and one start", err, job.since, started.started)
	}
	if err := supervised(&fakeSupervisor{recordedErr: errors.New("planted: no replay")}, 1).Start(slotCtx(t)); err == nil || errors.Is(err, ErrNotStarted) {
		t.Fatalf("Start(started, record unreadable) = %v, want an error that is not a refusal", err)
	}
}

func TestSupervisedWaitReadsTheEndOfItsOwnStart(t *testing.T) {
	code := 3
	exited := supervisor.RecordedJob{Since: "t1", LastEvent: "job.exited", LastExitCode: &code, LastReason: "deadline"}
	sup := &fakeSupervisor{running: []bool{true, true, false}, recorded: exited}
	job := supervised(sup, 5)
	job.since = "t1"
	end, err := job.Wait(slotCtx(t))
	if err != nil || !end.Recorded || end.Code == nil || *end.Code != 3 || end.Reason != "deadline" || sup.statusCalls != 3 {
		t.Fatalf("Wait() = %+v, %v after %d looks; want the recorded exit after 3", end, err, sup.statusCalls)
	}
	assertSupervisedWaitUnrecordedExits(t, exited)
}

func assertSupervisedWaitUnrecordedExits(t *testing.T, exited supervisor.RecordedJob) {
	t.Helper()
	// An exit that belongs to another start of the slot's job is not this run's.
	job := supervised(&fakeSupervisor{running: []bool{false}, recorded: exited}, 5)
	job.since = "t0"
	end, err := job.Wait(slotCtx(t))
	if err != nil || end.Recorded || end.Code != nil || !strings.Contains(end.Reason, "without its exit") {
		t.Fatalf("Wait(exit of another start) = %+v, %v; want an end that is not recorded", end, err)
	}
	// A job that stopped running with no exit on record.
	job = supervised(&fakeSupervisor{running: []bool{false}, recorded: supervisor.RecordedJob{Since: "t1", LastEvent: "job.started"}}, 5)
	job.since = "t1"
	if end, err = job.Wait(slotCtx(t)); err != nil || end.Recorded {
		t.Fatalf("Wait(no exit on record) = %+v, %v; want an end that is not recorded", end, err)
	}
}

func TestSupervisedWaitIsBounded(t *testing.T) {
	sup := &fakeSupervisor{running: []bool{true}}
	if _, err := supervised(sup, 4).Wait(slotCtx(t)); err == nil || !strings.Contains(err.Error(), "still runs after its deadline") || sup.statusCalls != 4 {
		t.Fatalf("Wait(job that never ends) = %v after %d looks, want it to give up after 4", err, sup.statusCalls)
	}
	ended, cancel := context.WithCancel(slotCtx(t))
	cancel()
	slow := &Supervised{sup: &fakeSupervisor{running: []bool{true}}, name: "slot-1", polls: 4, interval: time.Hour}
	if _, err := slow.Wait(ended); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait(ended context) = %v, want the context's error", err)
	}
	if _, err := supervised(&fakeSupervisor{statusErr: errors.New("planted: no replay")}, 4).Wait(slotCtx(t)); err == nil || !strings.Contains(err.Error(), "planted: no replay") {
		t.Fatalf("Wait(status fails) = %v, want the error", err)
	}
	if _, err := supervised(&fakeSupervisor{running: []bool{false}, recordedErr: errors.New("planted: no replay")}, 4).Wait(slotCtx(t)); err == nil {
		t.Fatal("Wait(record unreadable) = nil, want the error")
	}
}

// The wait reaches a little beyond the job's own deadline and stop grace, and no further.
func TestNewSupervisedWaitsJustBeyondTheJobsDeadline(t *testing.T) {
	job := config.JobConfig{Name: "slot-1", MaxRuntimeSeconds: 600, Stop: config.JobStopConfig{GraceSeconds: 20}}
	got := NewSupervised(&fakeSupervisor{}, job, 0)
	if want := (600+20+60)/2 + 1; got.polls != want || got.interval != WaitInterval || got.name != "slot-1" {
		t.Fatalf("NewSupervised() = %d looks every %s for %q, want %d every %s for slot-1", got.polls, got.interval, got.name, want, WaitInterval)
	}
	quick := NewSupervised(&fakeSupervisor{}, job, 500*time.Millisecond)
	if want := (600+20+60)*2 + 1; quick.polls != want || quick.interval != 500*time.Millisecond {
		t.Fatalf("NewSupervised(every 500ms) = %d looks every %s, want %d: the same wait in shorter steps", quick.polls, quick.interval, want)
	}
}

func TestSupervisedAdoptBoundary(t *testing.T) {
	prepTime := "2026-10-14T10:00:00.000000000Z"

	earlier := supervisor.RecordedJob{Since: "2026-10-14T09:59:59.999999999Z", LastEvent: "job.exited"}
	supEarlier := &fakeSupervisor{running: []bool{false}, recorded: earlier}
	jobEarlier := supervised(supEarlier, 1)
	if err := jobEarlier.Adopt(slotCtx(t), prepTime); err != nil {
		t.Fatalf("Adopt(earlier) = %v, want nil", err)
	}
	if jobEarlier.since != "" {
		t.Fatalf("jobEarlier.since = %q, want empty", jobEarlier.since)
	}
	endEarlier, err := jobEarlier.Wait(slotCtx(t))
	if err != nil || endEarlier.Recorded {
		t.Fatalf("Wait(earlier) = %+v, %v; want not recorded", endEarlier, err)
	}

	equal := supervisor.RecordedJob{Since: prepTime, LastEvent: "job.exited"}
	supEqual := &fakeSupervisor{running: []bool{false}, recorded: equal}
	jobEqual := supervised(supEqual, 1)
	if err := jobEqual.Adopt(slotCtx(t), prepTime); err != nil {
		t.Fatalf("Adopt(equal) = %v, want nil", err)
	}
	if jobEqual.since != prepTime {
		t.Fatalf("jobEqual.since = %q, want %q", jobEqual.since, prepTime)
	}

	laterTime := "2026-10-14T10:00:00.000000001Z"
	later := supervisor.RecordedJob{Since: laterTime, LastEvent: "job.exited"}
	supLater := &fakeSupervisor{running: []bool{false}, recorded: later}
	jobLater := supervised(supLater, 1)
	if err := jobLater.Adopt(slotCtx(t), prepTime); err != nil {
		t.Fatalf("Adopt(later) = %v, want nil", err)
	}
	if jobLater.since != laterTime {
		t.Fatalf("jobLater.since = %q, want %q", jobLater.since, laterTime)
	}
}
