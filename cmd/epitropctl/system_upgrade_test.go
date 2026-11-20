package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fixtureSourceDir is the v0.1.0 fixture set; testdata/v0.1.0/generate.sh wrote it with
// the published v0.1.0 binary. Tests copy it and never change it in place.
func fixtureSourceDir(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "testdata", "v0.1.0"))
	if err != nil {
		t.Fatalf("filepath.Abs(testdata/v0.1.0) = %v", err)
	}
	return abs
}

func copyUpgradeDir(t *testing.T, src, dst string) {
	t.Helper()
	root, err := os.OpenRoot(src)
	if err != nil {
		t.Fatalf("open fixture source %s: %v", src, err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Errorf("close fixture source %s: %v", src, err)
		}
	}()
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		target := filepath.Join(dst, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		data, err := root.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600) // #nosec G703 -- target is within the temporary upgrade fixture destination
	})
	if err != nil {
		t.Fatalf("copy directory from %s to %s: %v", src, dst, err)
	}
}

type upgradeFixture struct {
	baseDir      string
	cfgPath      string
	eventLogDir  string
	watchDir     string
	snapshotPath string
}

func writeUpgradeConfig(t *testing.T, base, eventLogDir, keyPath, watchDir string) string {
	t.Helper()
	cfg := map[string]any{
		"event_log": map[string]any{
			"dir":              eventLogDir,
			"signing_key_path": keyPath,
		},
		"release_watch": map[string]any{
			"state_dir": watchDir,
			"routes": []any{
				map[string]any{
					"name": "fixture",
					"sources": []any{
						map[string]any{"github": "tekmora/epitrop"},
					},
					"sinks": []any{
						map[string]any{
							"ntfy": map[string]any{"topic": "fixture-topic"},
						},
					},
				},
			},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal upgrade config: %v", err)
	}
	cfgPath := filepath.Join(base, "config.json")
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatalf("write upgrade config %s: %v", cfgPath, err)
	}
	return cfgPath
}

func prepareUpgradeFixture(t *testing.T) upgradeFixture {
	t.Helper()
	src := fixtureSourceDir(t)
	base := jobsTempDir(t)
	copyUpgradeDir(t, src, base)

	eventLogDir := filepath.Join(base, "eventlog")
	if err := os.MkdirAll(filepath.Join(eventLogDir, ".git"), 0o700); err != nil {
		t.Fatalf("MkdirAll(eventlog/.git) = %v", err)
	}

	keyPath := filepath.Join(base, "seed.hex")
	seed := bytes.Repeat([]byte{7}, 32)
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
		t.Fatalf("WriteFile(seed.hex) = %v", err)
	}

	watchDir := filepath.Join(base, "release-watch")
	cfgPath := writeUpgradeConfig(t, base, eventLogDir, keyPath, watchDir)

	return upgradeFixture{
		baseDir:      base,
		cfgPath:      cfgPath,
		eventLogDir:  eventLogDir,
		watchDir:     watchDir,
		snapshotPath: filepath.Join(base, "snapshot.json"),
	}
}

func newUpgradeWatchServers(t *testing.T, ntfyDeliveries *atomic.Int32) (*httptest.Server, *httptest.Server) {
	t.Helper()
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/repos/tekmora/epitrop/releases") {
			releaseJSON := `[{"tag_name":"v0.1.0","html_url":"https://github.com/tekmora/epitrop/releases/tag/v0.1.0","body":"v0.1.0 release notes","published_at":"2026-10-16T12:00:00Z"}]`
			if _, err := w.Write([]byte(releaseJSON)); err != nil {
				t.Logf("write release json: %v", err)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "fixture-topic") {
			ntfyDeliveries.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(func() {
		ghServer.Close()
		ntfyServer.Close()
	})
	return ghServer, ntfyServer
}

func assertCLIExitZero(t *testing.T, action, stdout, stderr string, code int, err error) {
	t.Helper()
	if err != nil || code != 0 {
		t.Fatalf("%s code = %d, err = %v: %s\n%s", action, code, err, stdout, stderr)
	}
}

func TestSystemUpgrade_Positive(t *testing.T) {
	bin := buildCLI(t)
	fix := prepareUpgradeFixture(t)

	checkUpgradeUnitsShow(t, bin, fix.cfgPath)
	checkUpgradeUnitsResume(t, bin, fix.cfgPath)
	checkUpgradeGraphShow(t, bin, fix.cfgPath)
	checkUpgradeAppend(t, bin, fix.cfgPath, fix.eventLogDir)
	checkUpgradeSnapshotShow(t, bin, fix.snapshotPath)
	checkUpgradeWatchRun(t, bin, fix.cfgPath)
}

// Without the v0.1.0 seen-set the same release is new and delivered once, so the zero
// deliveries of checkUpgradeWatchRun come from the seen-set, not from a release the fake
// server failed to serve.
func TestSystemUpgrade_WatchWithoutSeenSetDelivers(t *testing.T) {
	bin := buildCLI(t)
	fix := prepareUpgradeFixture(t)
	if err := os.Remove(filepath.Join(fix.watchDir, "release-watch-seen.json")); err != nil {
		t.Fatalf("Remove(seen-set) = %v", err)
	}
	var ntfyDeliveries atomic.Int32
	ghServer, ntfyServer := newUpgradeWatchServers(t, &ntfyDeliveries)
	stdout, stderr, code, err := runSystemCLI(t, bin, "watch", "run",
		"--config="+fix.cfgPath, "--once",
		"--github-base="+ghServer.URL, "--ntfy-base="+ntfyServer.URL)
	assertCLIExitZero(t, "watch run", stdout, stderr, code, err)
	if got := ntfyDeliveries.Load(); got != 1 {
		t.Fatalf("ntfy deliveries = %d, want 1 for the unseen v0.1.0 release", got)
	}
}

func checkUpgradeUnitsShow(t *testing.T, bin, cfgPath string) {
	t.Helper()
	stdout, stderr, code, err := runSystemCLI(t, bin, "units", "show", "--config="+cfgPath, "epitrop:unit-1")
	assertCLIExitZero(t, "units show", stdout, stderr, code, err)
	for _, want := range []string{
		`"id": "epitrop:unit-1"`,
		`"status": "ready"`,
		`"fixture note"`,
		`"from": "fixture"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("units show output missing %q:\n%s", want, stdout)
		}
	}
}

func checkUpgradeUnitsResume(t *testing.T, bin, cfgPath string) {
	t.Helper()
	stdout, stderr, code, err := runSystemCLI(t, bin, "units", "resume", "--config="+cfgPath)
	assertCLIExitZero(t, "units resume", stdout, stderr, code, err)
	wantHeader := "id\tstatus\tupdated_at\tworktree\tbranch\tpr\tlane\trelaunch\n"
	if !strings.HasPrefix(stdout, wantHeader) {
		t.Fatalf("units resume header missing: %s", stdout)
	}
	if !strings.Contains(stdout, "epitrop:unit-1\tready\t") {
		t.Fatalf("units resume missing unit row: %s", stdout)
	}
}

func checkUpgradeGraphShow(t *testing.T, bin, cfgPath string) {
	t.Helper()
	stdout, stderr, code, err := runSystemCLI(t, bin, "graph", "show", "--config="+cfgPath)
	assertCLIExitZero(t, "graph show", stdout, stderr, code, err)
	nodes := shownNodes(t, stdout)
	if len(nodes) != 2 {
		t.Fatalf("graph show nodes count = %d, want 2", len(nodes))
	}
	if nodes[0].ID != "task-1" || nodes[1].ID != "task-2" {
		t.Fatalf("graph show unexpected nodes: %+v", nodes)
	}
	if len(nodes[1].DependsOn) != 1 || nodes[1].DependsOn[0] != "task-1" {
		t.Fatalf("graph show task-2 missing edge to task-1: %+v", nodes[1].DependsOn)
	}
}

func checkUpgradeAppend(t *testing.T, bin, cfgPath, eventLogDir string) {
	t.Helper()
	checkUpgradeAppendUnit(t, bin, cfgPath, eventLogDir)
	checkUpgradeAppendGraph(t, bin, cfgPath)
}

func checkUpgradeAppendUnit(t *testing.T, bin, cfgPath, eventLogDir string) {
	t.Helper()
	stdout, stderr, code, err := runSystemCLI(t, bin, "units", "set", "--config="+cfgPath, "--status=claimed", "epitrop:unit-1")
	assertCLIExitZero(t, "units set", stdout, stderr, code, err)

	seq5Path := filepath.Join(eventLogDir, "events", "00000000000000000005.json")
	info, err := os.Stat(seq5Path)
	if err != nil || info.IsDir() {
		t.Fatalf("new record seq 5 file missing at %s: err=%v", seq5Path, err)
	}

	stdoutShow, stderrShow, codeShow, errShow := runSystemCLI(t, bin, "units", "show", "--config="+cfgPath, "epitrop:unit-1")
	assertCLIExitZero(t, "units show", stdoutShow, stderrShow, codeShow, errShow)
	if !strings.Contains(stdoutShow, `"status": "claimed"`) {
		t.Fatalf("units show missing claimed status: %s", stdoutShow)
	}
}

func checkUpgradeAppendGraph(t *testing.T, bin, cfgPath string) {
	t.Helper()
	stage := stageFile(t, fmt.Sprintf(`{"ops":[%s]}`, nodeOp("task-3")))
	stdoutApply, stderrApply, codeApply, errApply := runSystemCLI(t, bin, "graph", "apply", "--config="+cfgPath, "--file="+stage)
	assertCLIExitZero(t, "graph apply", stdoutApply, stderrApply, codeApply, errApply)

	stdoutGraph, stderrGraph, codeGraph, errGraph := runSystemCLI(t, bin, "graph", "show", "--config="+cfgPath)
	assertCLIExitZero(t, "graph show", stdoutGraph, stderrGraph, codeGraph, errGraph)
	nodes := shownNodes(t, stdoutGraph)
	if len(nodes) != 3 {
		t.Fatalf("graph show nodes count = %d, want 3: %s", len(nodes), stdoutGraph)
	}
}

func checkUpgradeSnapshotShow(t *testing.T, bin, snapshotPath string) {
	t.Helper()
	stdout, stderr, code, err := runSystemCLI(t, bin, "show", "--in="+snapshotPath)
	assertCLIExitZero(t, "show", stdout, stderr, code, err)
	for _, want := range []string{"fixture-model:7b", "ollama-local"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("show output missing %q:\n%s", want, stdout)
		}
	}
}

func checkUpgradeWatchRun(t *testing.T, bin, cfgPath string) {
	t.Helper()
	var ntfyDeliveries atomic.Int32
	ghServer, ntfyServer := newUpgradeWatchServers(t, &ntfyDeliveries)

	stdout, stderr, code, err := runSystemCLI(t, bin, "watch", "run",
		"--config="+cfgPath, "--once",
		"--github-base="+ghServer.URL, "--ntfy-base="+ntfyServer.URL)
	assertCLIExitZero(t, "watch run", stdout, stderr, code, err)
	if ntfyDeliveries.Load() != 0 {
		t.Fatalf("ntfy deliveries = %d, want 0 (v0.1.0 release already seen)", ntfyDeliveries.Load())
	}
}

func TestSystemUpgrade_Negative(t *testing.T) {
	bin := buildCLI(t)

	t.Run("tampered record without resigning", func(t *testing.T) {
		checkUpgradeNegativeTamperedRecord(t, bin)
	})

	t.Run("seen file unknown version", func(t *testing.T) {
		checkUpgradeNegativeSeenVersion(t, bin)
	})
}

func assertTamperedFailure(t *testing.T, cmdName, stderr string, code int, err error) {
	t.Helper()
	if err == nil || code == 0 {
		t.Fatalf("%s on tampered record succeeded (code = %d), want failure", cmdName, code)
	}
	if !strings.Contains(stderr, "hash mismatch") && !strings.Contains(stderr, "receipt") {
		t.Errorf("%s stderr missing hash/receipt error: %s", cmdName, stderr)
	}
}

func checkUpgradeNegativeTamperedRecord(t *testing.T, bin string) {
	t.Helper()
	fix := prepareUpgradeFixture(t)
	event2Path := filepath.Join(fix.eventLogDir, "events", "00000000000000000002.json")
	// #nosec G304 -- event2Path is in the prepared upgrade fixture
	data, err := os.ReadFile(event2Path)
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v", event2Path, err)
	}
	tampered := strings.Replace(string(data), `"status":"ready"`, `"status":"claimed"`, 1)
	if tampered == string(data) {
		t.Fatalf("record 2 holds no \"status\":\"ready\" to change: %s", data)
	}
	if err := os.WriteFile(event2Path, []byte(tampered), 0o600); err != nil { // #nosec G703 -- event2Path is in the prepared upgrade fixture
		t.Fatalf("WriteFile(%s) = %v", event2Path, err)
	}

	_, stderrShow, codeShow, errShow := runSystemCLI(t, bin, "units", "show", "--config="+fix.cfgPath, "epitrop:unit-1")
	assertTamperedFailure(t, "units show", stderrShow, codeShow, errShow)

	_, stderrResume, codeResume, errResume := runSystemCLI(t, bin, "units", "resume", "--config="+fix.cfgPath)
	assertTamperedFailure(t, "units resume", stderrResume, codeResume, errResume)
}

func checkUpgradeNegativeSeenVersion(t *testing.T, bin string) {
	t.Helper()
	fix := prepareUpgradeFixture(t)
	seenPath := filepath.Join(fix.watchDir, "release-watch-seen.json")
	// #nosec G304 -- seenPath is in the prepared upgrade fixture
	data, err := os.ReadFile(seenPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v", seenPath, err)
	}
	v2Seen := strings.Replace(string(data), `"version": 1`, `"version": 2`, 1)
	if v2Seen == string(data) {
		t.Fatalf("seen file holds no \"version\": 1 to change: %s", data)
	}
	if err := os.WriteFile(seenPath, []byte(v2Seen), 0o600); err != nil { // #nosec G703 -- seenPath is in the prepared upgrade fixture
		t.Fatalf("WriteFile(%s) = %v", seenPath, err)
	}

	var ntfyDeliveries atomic.Int32
	ghServer, ntfyServer := newUpgradeWatchServers(t, &ntfyDeliveries)

	_, stderr, code, err := runSystemCLI(t, bin, "watch", "run",
		"--config="+fix.cfgPath, "--once",
		"--github-base="+ghServer.URL, "--ntfy-base="+ntfyServer.URL)

	if code == 0 {
		t.Fatalf("watch run on a version 2 seen file: code = 0, err = %v, want a refusal", err)
	}
	if !strings.Contains(stderr, "has version 2, want 1") {
		t.Errorf("watch run stderr = %q, want the version refusal", stderr)
	}
	if got := ntfyDeliveries.Load(); got != 0 {
		t.Errorf("ntfy deliveries = %d, want 0 when the state is refused", got)
	}
}

// v010JobReason is the sandbox reason fixture-job had under v0.1.0; the config here keeps it,
// so the status row reads the same before and after the upgrade.
const v010JobReason = "fixture: writes one line and exits"

// prepareUpgradeJobs copies the v0.1.0 jobs fixture (one finished run of fixture-job) and
// writes a config that declares the same job.
func prepareUpgradeJobs(t *testing.T) (cfgPath, eventLogDir string) {
	t.Helper()
	base := jobsTempDir(t)
	eventLogDir = filepath.Join(base, "jobs")
	copyUpgradeDir(t, filepath.Join(fixtureSourceDir(t), "jobs"), eventLogDir)
	if err := os.MkdirAll(filepath.Join(eventLogDir, ".git"), 0o700); err != nil {
		t.Fatalf("MkdirAll(jobs/.git) = %v", err)
	}
	keyPath := filepath.Join(base, "seed.hex")
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(bytes.Repeat([]byte{7}, 32))), 0o600); err != nil {
		t.Fatalf("WriteFile(seed.hex) = %v", err)
	}
	body := `{"event_log":{"dir":` + jsonQuote(eventLogDir) + `,"signing_key_path":` + jsonQuote(keyPath) +
		`},"jobs":[{"name":"fixture-job","command":["/bin/true"],"log_path":` + jsonQuote(filepath.Join(base, "fixture-job.log")) +
		`,"sandbox":{"mode":"off","reason":` + jsonQuote(v010JobReason) + `}}]}`
	cfgPath = filepath.Join(base, "config.json")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) = %v", cfgPath, err)
	}
	return cfgPath, eventLogDir
}

// waitForFile polls for path for up to 10 s and fails the test when it does not appear; the
// restarted job writes its job.exited record after jobs start has returned.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; i < 200 && time.Now().Before(deadline); i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s did not appear: %v", path, err)
	}
}

// TestSystemUpgrade_Jobs: the current binary reports the job run v0.1.0 recorded, and starts
// the job again on top of that state: the shim appends after the v0.1.0 records and replaces
// the v0.1.0 job record.
func TestSystemUpgrade_Jobs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the supervisor runs on Linux only")
	}
	bin := buildCLI(t)
	cfgPath, eventLogDir := prepareUpgradeJobs(t)
	wantRow := "fixture-job\tdead\t0\t\t0\toff: " + v010JobReason + "\n"
	stdout, stderr, code, err := runSystemCLI(t, bin, "jobs", "status", "--config="+cfgPath, "fixture-job")
	assertCLIExitZero(t, "jobs status", stdout, stderr, code, err)
	if stdout != wantRow {
		t.Fatalf("jobs status on v0.1.0 state = %q, want %q", stdout, wantRow)
	}

	recordPath := filepath.Join(eventLogDir, "jobs", "fixture-job.json")
	before, err := os.ReadFile(recordPath) // #nosec G304 -- path is the copied test fixture.
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v", recordPath, err)
	}
	stdout, stderr, code, err = runSystemCLI(t, bin, "jobs", "start", "--config="+cfgPath, "fixture-job")
	assertCLIExitZero(t, "jobs start", stdout, stderr, code, err)
	waitForFile(t, filepath.Join(eventLogDir, "events", "00000000000000000004.json"))
	after, err := os.ReadFile(recordPath) // #nosec G304 -- path is the copied test fixture.
	if err != nil || bytes.Equal(after, before) {
		t.Fatalf("job record after the restart = %s, err = %v, want a new record", after, err)
	}
	stdout, stderr, code, err = runSystemCLI(t, bin, "jobs", "status", "--config="+cfgPath, "fixture-job")
	assertCLIExitZero(t, "jobs status", stdout, stderr, code, err)
	if stdout != wantRow {
		t.Fatalf("jobs status after the restart = %q, want %q", stdout, wantRow)
	}
}
