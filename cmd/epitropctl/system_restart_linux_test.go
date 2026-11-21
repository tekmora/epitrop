//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/releasewatch"
	"github.com/tekmora/epitrop/internal/units"
)

const (
	restartJobName       = "daemon"
	restartSandboxReason = "restart system test"
	restartOpenUnitID    = "tekmora/epitrop#399"
	restartDoneUnitID    = "tekmora/epitrop#398"
	restartBranch        = "feat/restart-399"
	restartUnitNote      = "keep unit across supervisor restart"
	restartPollCap       = 300
	restartPollInterval  = 50 * time.Millisecond
	restartPollBound     = 15 * time.Second
	superviseRunBound    = 50 * time.Second
)

type restartFixture struct {
	bin            string
	baseDir        string
	cfgPath        string
	keyPath        string
	eventLogDir    string
	watchDir       string
	worktreeDir    string
	ghURL          string
	ntfyURL        string
	addRelease     *atomic.Int32
	ntfyDeliveries *atomic.Int32
}

type restartSnapshot struct {
	jobsStatus   string
	daemonPID    int
	unitsResume  string
	unitsShow    string
	watchSeenIDs []string
}

type superviseProc struct {
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	stopped bool
}

func writeRestartConfig(t *testing.T, cfgPath, eventLogDir, keyPath, logPath, watchDir string) {
	t.Helper()
	cfg := map[string]any{
		"event_log": map[string]any{
			"dir":              eventLogDir,
			"signing_key_path": keyPath,
		},
		"jobs": []any{
			map[string]any{
				"name":     restartJobName,
				"schedule": "always",
				"command":  []string{"/bin/sleep", "600"},
				"log_path": logPath,
				"sandbox": map[string]any{
					"mode":   "off",
					"reason": restartSandboxReason,
				},
				"restart": map[string]any{
					"policy":       "always",
					"max_restarts": 5,
				},
			},
		},
		"release_watch": map[string]any{
			"state_dir":        watchDir,
			"rate_limit_floor": 10,
			"routes": []any{
				map[string]any{
					"name":    "system-test-route",
					"sources": []any{map[string]any{"github": "owner/upstream"}},
					"sinks":   []any{map[string]any{"ntfy": map[string]any{"topic": "system-test-topic"}}},
				},
			},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal restart config: %v", err)
	}
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil { // #nosec G703 -- cfgPath is inside the temporary test directory
		t.Fatalf("write restart config %s: %v", cfgPath, err)
	}
}

func stopDaemonCleanup(t *testing.T, bin, cfgPath string, env []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "jobs", "stop", "--config="+cfgPath, restartJobName) // #nosec G204 -- bin is built by buildCLI and cfgPath is a temporary fixture
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("cleanup jobs stop (%s): %v: %s", cfgPath, err, out)
	}
}

func newRestartFixture(t *testing.T) *restartFixture {
	t.Helper()
	bin := buildCLI(t)
	base := jobsTempDir(t)
	keyPath := filepath.Join(base, "seed.hex")
	seed := bytes.Repeat([]byte{7}, 32)
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
		t.Fatalf("write signing key: %v", err)
	}
	eventLogDir := filepath.Join(base, "state")
	if err := os.MkdirAll(filepath.Join(eventLogDir, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir state/.git: %v", err)
	}
	watchDir := filepath.Join(base, "watch-state")
	worktreeDir := filepath.Join(base, "wt-399")
	if err := os.MkdirAll(worktreeDir, 0o750); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	cfgPath := filepath.Join(base, "config.json")
	writeRestartConfig(t, cfgPath, eventLogDir, keyPath, filepath.Join(base, "daemon.log"), watchDir)
	cleanEnv := isolatedCLIEnv(t)
	t.Cleanup(func() { stopDaemonCleanup(t, bin, cfgPath, cleanEnv) })

	var addRelease atomic.Int32
	var ntfyDeliveries atomic.Int32
	ghServer, ntfyServer := newSystemWatchStandIns(t, &addRelease, &ntfyDeliveries)
	return &restartFixture{
		bin:            bin,
		baseDir:        base,
		cfgPath:        cfgPath,
		keyPath:        keyPath,
		eventLogDir:    eventLogDir,
		watchDir:       watchDir,
		worktreeDir:    worktreeDir,
		ghURL:          ghServer.URL,
		ntfyURL:        ntfyServer.URL,
		addRelease:     &addRelease,
		ntfyDeliveries: &ntfyDeliveries,
	}
}

func openSuperviseLogs(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	dir := t.TempDir()
	outFile, err := os.CreateTemp(dir, "supervise-out-*")
	if err != nil {
		t.Fatalf("create supervise stdout log: %v", err)
	}
	errFile, err := os.CreateTemp(dir, "supervise-err-*")
	if err != nil {
		if closeErr := outFile.Close(); closeErr != nil {
			t.Logf("close supervise stdout log: %v", closeErr)
		}
		t.Fatalf("create supervise stderr log: %v", err)
	}
	return outFile, errFile
}

func closeSuperviseLogs(t *testing.T, outFile, errFile *os.File) {
	t.Helper()
	if err := outFile.Close(); err != nil {
		t.Fatalf("close supervise stdout fd: %v", err)
	}
	if err := errFile.Close(); err != nil {
		t.Fatalf("close supervise stderr fd: %v", err)
	}
}

func startSupervise(t *testing.T, bin, cfgPath string) *superviseProc {
	t.Helper()
	outFile, errFile := openSuperviseLogs(t)
	ctx, cancel := context.WithTimeout(t.Context(), superviseRunBound)
	cmd := exec.CommandContext(ctx, bin, "jobs", "supervise", "--config="+cfgPath) // #nosec G204 -- bin is built by buildCLI and cfgPath is a temporary fixture
	cmd.Env = isolatedCLIEnv(t)
	cmd.Stdout = outFile
	cmd.Stderr = errFile
	startErr := cmd.Start()
	closeSuperviseLogs(t, outFile, errFile)
	if startErr != nil {
		cancel()
		t.Fatalf("start jobs supervise: %v", startErr)
	}
	proc := &superviseProc{cmd: cmd, cancel: cancel}
	t.Cleanup(func() { proc.stop(t) })
	return proc
}

func (p *superviseProc) stop(t *testing.T) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	defer p.cancel()
	if killErr := p.cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		t.Fatalf("kill supervise pid %d: %v", p.cmd.Process.Pid, killErr)
	}
	waitErr := p.cmd.Wait()
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		t.Fatalf("wait supervise pid %d: %v", p.cmd.Process.Pid, waitErr)
	}
}

func restartSupervise(t *testing.T, prev *superviseProc, bin, cfgPath string) *superviseProc {
	t.Helper()
	prev.stop(t)
	return startSupervise(t, bin, cfgPath)
}

func parseDaemonStatus(stdout string) (string, int, int, string, bool) {
	fields := strings.Split(strings.TrimSuffix(stdout, "\n"), "\t")
	if len(fields) < 6 || fields[0] != restartJobName {
		return "", 0, 0, "", false
	}
	pid, pidErr := strconv.Atoi(fields[2])
	restarts, restartErr := strconv.Atoi(fields[4])
	if pidErr != nil || restartErr != nil {
		return "", 0, 0, "", false
	}
	return fields[1], pid, restarts, fields[5], true
}

func waitDaemonRunning(t *testing.T, bin, cfgPath string) (string, int, int) {
	t.Helper()
	deadline := time.Now().Add(restartPollBound)
	var lastOut, lastErr string
	for i := 0; i < restartPollCap && time.Now().Before(deadline); i++ {
		stdout, stderr, code, err := runSystemCLIWithDeadline(t, bin, 5*time.Second, nil, "jobs", "status", "--config="+cfgPath, restartJobName)
		lastOut, lastErr = stdout, stderr
		if err == nil && code == 0 {
			state, pid, restarts, sandbox, ok := parseDaemonStatus(stdout)
			if ok && state == "running" && pid > 0 && sandbox != "unknown" {
				return stdout, pid, restarts
			}
		}
		time.Sleep(restartPollInterval)
	}
	t.Fatalf("daemon did not report running within %s; stdout=%q stderr=%q", restartPollBound, lastOut, lastErr)
	return "", 0, 0
}

func countRestartEvents(t *testing.T, eventLogDir, typ string) int {
	t.Helper()
	eventsDir := filepath.Join(eventLogDir, "events")
	root, err := os.OpenRoot(eventsDir)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("open events root %s: %v", eventsDir, err)
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			t.Errorf("close events root %s: %v", eventsDir, closeErr)
		}
	}()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		t.Fatalf("read events dir %s: %v", eventsDir, err)
	}
	needle := `"type":"` + typ + `"`
	count := 0
	for i := 0; i < len(entries); i++ {
		if entries[i].IsDir() || !strings.HasSuffix(entries[i].Name(), ".json") {
			continue
		}
		body, readErr := root.ReadFile(entries[i].Name())
		if readErr != nil {
			t.Fatalf("read event %s: %v", entries[i].Name(), readErr)
		}
		if strings.Contains(string(body), needle) {
			count++
		}
	}
	return count
}

func waitEventCountAtLeast(t *testing.T, eventLogDir, typ string, wantMin int) int {
	t.Helper()
	deadline := time.Now().Add(restartPollBound)
	got := 0
	for i := 0; i < restartPollCap && time.Now().Before(deadline); i++ {
		got = countRestartEvents(t, eventLogDir, typ)
		if got >= wantMin {
			return got
		}
		time.Sleep(restartPollInterval)
	}
	t.Fatalf("event %q count = %d after %s, want >= %d", typ, got, restartPollBound, wantMin)
	return 0
}

func mustRunUnitsCLI(t *testing.T, bin string, args ...string) string {
	t.Helper()
	stdout, stderr, code, err := runSystemCLI(t, bin, args...)
	assertCLIExitZero(t, strings.Join(args[:2], " "), stdout, stderr, code, err)
	return stdout
}

func populateRestartUnits(t *testing.T, fix *restartFixture) (string, string) {
	t.Helper()
	if !units.IsTerminalStatus("landed") {
		t.Fatal("landed must be a terminal status")
	}
	doneSteps := []string{"ready", "claimed", "implementing", "review", "landing", "landed"}
	for i := 0; i < len(doneSteps); i++ {
		mustRunUnitsCLI(t, fix.bin, "units", "set", "--config="+fix.cfgPath, "--status="+doneSteps[i], restartDoneUnitID)
	}
	openSteps := []string{"ready", "claimed", "implementing"}
	for i := 0; i < len(openSteps); i++ {
		mustRunUnitsCLI(t, fix.bin, "units", "set", "--config="+fix.cfgPath, "--status="+openSteps[i], restartOpenUnitID)
	}
	mustRunUnitsCLI(t, fix.bin, "units", "set", "--config="+fix.cfgPath,
		"--status=implementing", "--worktree="+fix.worktreeDir, "--branch="+restartBranch, restartOpenUnitID)
	mustRunUnitsCLI(t, fix.bin, "units", "note", "--config="+fix.cfgPath, "--from=operator", restartOpenUnitID, restartUnitNote)

	resumeSnap := mustRunUnitsCLI(t, fix.bin, "units", "resume", "--config="+fix.cfgPath)
	showSnap := mustRunUnitsCLI(t, fix.bin, "units", "show", "--config="+fix.cfgPath, restartOpenUnitID)
	checkInitialUnitsSnapshots(t, fix, resumeSnap, showSnap)
	return resumeSnap, showSnap
}

func checkInitialUnitsSnapshots(t *testing.T, fix *restartFixture, resumeSnap, showSnap string) {
	t.Helper()
	if strings.Contains(resumeSnap, restartDoneUnitID) {
		t.Fatalf("units resume includes terminal unit %s:\n%s", restartDoneUnitID, resumeSnap)
	}
	for _, want := range []string{restartOpenUnitID + "\timplementing\t", fix.worktreeDir, restartBranch, "\tsafe\n"} {
		if !strings.Contains(resumeSnap, want) {
			t.Fatalf("units resume snapshot missing %q:\n%s", want, resumeSnap)
		}
	}
	for _, want := range []string{`"status": "implementing"`, restartBranch, restartUnitNote} {
		if !strings.Contains(showSnap, want) {
			t.Fatalf("units show snapshot missing %q:\n%s", want, showSnap)
		}
	}
}

func readWatchSeenIDs(t *testing.T, watchDir string) []string {
	t.Helper()
	state, err := releasewatch.LoadState(watchDir)
	if err != nil {
		t.Fatalf("LoadState(%s) = %v", watchDir, err)
	}
	root, err := os.OpenRoot(watchDir)
	if err != nil {
		t.Fatalf("open watch root %s: %v", watchDir, err)
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			t.Errorf("close watch root %s: %v", watchDir, closeErr)
		}
	}()
	raw, err := root.ReadFile(releasewatch.SeenFileName)
	if err != nil {
		t.Fatalf("read %s: %v", releasewatch.SeenFileName, err)
	}
	var sf releasewatch.SeenFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("unmarshal %s: %v", releasewatch.SeenFileName, err)
	}
	ids := slices.Sorted(maps.Keys(sf.Seen))
	if state.Count() != len(ids) {
		t.Fatalf("LoadState.Count() = %d, seen file keys = %d", state.Count(), len(ids))
	}
	for i := 0; i < len(ids); i++ {
		if !state.Has(ids[i]) {
			t.Fatalf("LoadState missing seen id %q", ids[i])
		}
	}
	return ids
}

func prepareAndSnapshotState(t *testing.T, fix *restartFixture) (*superviseProc, restartSnapshot) {
	t.Helper()
	sup := startSupervise(t, fix.bin, fix.cfgPath)
	jobsStatus, daemonPID, _ := waitDaemonRunning(t, fix.bin, fix.cfgPath)
	unitsResume, unitsShow := populateRestartUnits(t, fix)

	systemWatchSeed(t, fix.bin, fix.cfgPath, fix.ghURL, fix.ntfyURL, fix.watchDir, fix.ntfyDeliveries)
	fix.addRelease.Store(1)
	systemWatchRunOnce(t, fix.bin, fix.cfgPath, fix.ghURL, fix.ntfyURL, fix.watchDir, fix.ntfyDeliveries)
	seenIDs := readWatchSeenIDs(t, fix.watchDir)

	return sup, restartSnapshot{
		jobsStatus:   jobsStatus,
		daemonPID:    daemonPID,
		unitsResume:  unitsResume,
		unitsShow:    unitsShow,
		watchSeenIDs: seenIDs,
	}
}

// assertPostRestartJobs checks the measured restart path after killing only the supervise
// process with os.Process.Kill: SIGKILL ends supervise before markSupervisorStopped runs,
// and shimCmdFor sets no Pdeathsig on the __job-shim child (prepareProcess sets Pdeathsig
// only between the shim and its job process). The shim therefore keeps holding
// jobs/daemon.lock, and the next supervisor's reconcileAlwaysJob sees statusFor == StateRunning
// and calls adoptIfFreshStart, appending one job.adopted record while keeping the same
// daemon PID and zero job.restarted records.
func assertPostRestartJobs(t *testing.T, fix *restartFixture, snap restartSnapshot) {
	t.Helper()
	adopted := waitEventCountAtLeast(t, fix.eventLogDir, "job.adopted", 1)
	statusAfter, pidAfter, restartsShown := waitDaemonRunning(t, fix.bin, fix.cfgPath)
	if pidAfter != snap.daemonPID {
		t.Fatalf("daemon pid after restart = %d, want adopted pid %d", pidAfter, snap.daemonPID)
	}
	if statusAfter != snap.jobsStatus {
		t.Fatalf("jobs status after restart = %q, want %q", statusAfter, snap.jobsStatus)
	}
	if adopted != 1 {
		t.Fatalf("job.adopted events = %d, want 1", adopted)
	}
	if started := countRestartEvents(t, fix.eventLogDir, "job.started"); started != 1 {
		t.Fatalf("job.started events = %d, want 1", started)
	}
	restarted := countRestartEvents(t, fix.eventLogDir, "job.restarted")
	if restarted != 0 {
		t.Fatalf("job.restarted events = %d, want 0", restarted)
	}
	if restartsShown != restarted {
		t.Fatalf("jobs status restarts = %d, want event log restart count %d", restartsShown, restarted)
	}
}

func assertPostRestartUnitsAndWatch(t *testing.T, fix *restartFixture, snap restartSnapshot) {
	t.Helper()
	resumeAfter := mustRunUnitsCLI(t, fix.bin, "units", "resume", "--config="+fix.cfgPath)
	if resumeAfter != snap.unitsResume {
		t.Fatalf("units resume after restart =\n%s\nwant exact snapshot:\n%s", resumeAfter, snap.unitsResume)
	}
	if strings.Contains(resumeAfter, restartDoneUnitID) {
		t.Fatalf("terminal unit %s appeared in units resume:\n%s", restartDoneUnitID, resumeAfter)
	}
	showAfter := mustRunUnitsCLI(t, fix.bin, "units", "show", "--config="+fix.cfgPath, restartOpenUnitID)
	if showAfter != snap.unitsShow {
		t.Fatalf("units show after restart =\n%s\nwant exact snapshot:\n%s", showAfter, snap.unitsShow)
	}

	beforeDeliveries := fix.ntfyDeliveries.Load()
	stdout, stderr, code, err := runSystemCLI(t, fix.bin, "watch", "run",
		"--config="+fix.cfgPath, "--once", "--github-base="+fix.ghURL, "--ntfy-base="+fix.ntfyURL)
	assertCLIExitZero(t, "watch run", stdout, stderr, code, err)
	if got := fix.ntfyDeliveries.Load() - beforeDeliveries; got != 0 {
		t.Fatalf("ntfy deliveries after restart = %d, want 0", got)
	}
	gotSeen := readWatchSeenIDs(t, fix.watchDir)
	if !slices.Equal(gotSeen, snap.watchSeenIDs) {
		t.Fatalf("seen ids after restart = %v, want %v", gotSeen, snap.watchSeenIDs)
	}
}

func TestSystemRestart_Positive(t *testing.T) {
	fix := newRestartFixture(t)
	sup, snap := prepareAndSnapshotState(t, fix)

	restartSupervise(t, sup, fix.bin, fix.cfgPath)

	assertPostRestartJobs(t, fix, snap)
	assertPostRestartUnitsAndWatch(t, fix, snap)
}

func TestSystemRestart_Boundary(t *testing.T) {
	fix := newRestartFixture(t)
	sup1, snap := prepareAndSnapshotState(t, fix)

	sup2 := restartSupervise(t, sup1, fix.bin, fix.cfgPath)
	waitDaemonRunning(t, fix.bin, fix.cfgPath)
	restartSupervise(t, sup2, fix.bin, fix.cfgPath)

	assertPostRestartJobs(t, fix, snap)
	assertPostRestartUnitsAndWatch(t, fix, snap)
}

func TestSystemRestart_Negative(t *testing.T) {
	fix := newRestartFixture(t)
	sup, _ := prepareAndSnapshotState(t, fix)

	emptyWatchDir := filepath.Join(fix.baseDir, "empty-watch")
	if err := os.MkdirAll(emptyWatchDir, 0o750); err != nil {
		t.Fatalf("mkdir empty watch dir: %v", err)
	}
	watchLostCfg := filepath.Join(fix.baseDir, "config-empty-watch.json")
	writeRestartConfig(t, watchLostCfg, fix.eventLogDir, fix.keyPath, filepath.Join(fix.baseDir, "daemon.log"), emptyWatchDir)

	restartSupervise(t, sup, fix.bin, watchLostCfg)
	waitDaemonRunning(t, fix.bin, watchLostCfg)

	beforeDeliveries := fix.ntfyDeliveries.Load()
	stdoutWatch, stderrWatch, codeWatch, errWatch := runSystemCLI(t, fix.bin, "watch", "run",
		"--config="+watchLostCfg, "--once", "--github-base="+fix.ghURL, "--ntfy-base="+fix.ntfyURL)
	assertCLIExitZero(t, "watch run empty state_dir", stdoutWatch, stderrWatch, codeWatch, errWatch)
	if delivered := fix.ntfyDeliveries.Load() - beforeDeliveries; delivered <= 0 {
		t.Fatalf("watch deliveries on empty state_dir = %d, want > 0", delivered)
	}

	emptyEventDir := filepath.Join(fix.baseDir, "empty-events")
	if err := os.MkdirAll(filepath.Join(emptyEventDir, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir empty events/.git: %v", err)
	}
	unitsLostCfg := filepath.Join(fix.baseDir, "config-empty-events.json")
	writeRestartConfig(t, unitsLostCfg, emptyEventDir, fix.keyPath, filepath.Join(fix.baseDir, "daemon-empty.log"), fix.watchDir)

	resumeEmpty := mustRunUnitsCLI(t, fix.bin, "units", "resume", "--config="+unitsLostCfg)
	if strings.Contains(resumeEmpty, restartOpenUnitID) {
		t.Fatalf("units resume on empty event_log.dir still contained %s:\n%s", restartOpenUnitID, resumeEmpty)
	}
}
