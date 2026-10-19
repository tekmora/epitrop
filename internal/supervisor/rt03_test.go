//go:build linux

package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

type rt03Fixture struct {
	ID        string          `json:"id"`
	Boundary  string          `json:"boundary"`
	Direction string          `json:"direction"`
	Task      rt03Task        `json:"task"`
	Attempt   rt03Attempt     `json:"attempt"`
	Expect    json.RawMessage `json:"expect"`
}

// Destinations and Target may hold {host} and {port}: the address and port of the listener
// the fixture's attempt aims at, known only when the fixture runs.
type rt03Task struct {
	Network      string   `json:"network"`
	EnvAllow     []string `json:"env_allow"`
	Destinations []string `json:"destinations"`
}

type rt03Attempt struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

type rt03World struct {
	workspace  string
	eventLog   string
	configFile string
	signingKey string
	hostHome   string
}

func TestRT03Fixtures(t *testing.T) {
	fixtures := loadRT03Fixtures(t)
	for i := 0; i < len(fixtures); i++ {
		fixture := fixtures[i]
		t.Run(fixture.ID, func(t *testing.T) {
			allowed := runRT03Fixture(t, fixture)
			if fixture.Direction == "violation" && allowed {
				t.Fatalf("%s allowed violation, want refused", fixture.ID)
			}
			if fixture.Direction == "compliant" && !allowed {
				t.Fatalf("%s refused compliant action, want allowed", fixture.ID)
			}
		})
	}
}

func TestSandboxProductionArgvKeepsSystemdPrefix(t *testing.T) {
	workspace := t.TempDir()
	sandbox := config.SandboxConfig{Mode: "enforce", Workspace: workspace, Network: "none", MemoryMax: "2G", CPUWeight: 100, TasksMax: 512}
	built, err := buildSandboxCommand([]string{"/usr/bin/bash", "-c", "true"}, sandbox, sandboxBuildOptions{
		Tools: sandboxTools{SystemdRun: "/usr/bin/systemd-run", Bwrap: "/usr/bin/bwrap", Env: "/usr/bin/env", Systemd: true},
		Hosts: testHosts(t),
	})
	if err != nil {
		t.Fatalf("buildSandboxCommand() = %v, want nil", err)
	}
	args := built.Argv
	if !strings.HasPrefix(built.Scope, scopeNamePrefix) {
		t.Fatalf("Scope = %q, want the unit the shim must find the job in", built.Scope)
	}
	want := []string{"/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--collect", "--expand-environment=no", "--unit=" + built.Scope}
	for i := 0; i < len(want); i++ {
		if args[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q in %q", i, args[i], want[i], args)
		}
	}
	direct, err := buildSandboxCommand([]string{"/usr/bin/bash", "-c", "true"}, sandbox, sandboxBuildOptions{
		Tools: sandboxTools{Bwrap: "/usr/bin/bwrap", Env: "/usr/bin/env"},
		Hosts: testHosts(t),
	})
	if err != nil || direct.Scope != "" || strings.Contains(strings.Join(direct.Argv, " "), "--unit=") {
		t.Fatalf("buildSandboxCommand(no systemd) = scope %q argv %q, %v, want no scope to confirm", direct.Scope, direct.Argv, err)
	}
}

func loadRT03Fixtures(t *testing.T) []rt03Fixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "rt03", "*.json"))
	if err != nil {
		t.Fatalf("Glob(rt03) = %v, want nil", err)
	}
	out := make([]rt03Fixture, 0, len(paths))
	for i := 0; i < len(paths); i++ {
		out = append(out, readRT03Fixture(t, paths[i]))
	}
	return out
}

func readRT03Fixture(t *testing.T, path string) rt03Fixture {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v, want nil", path, err)
	}
	var fixture rt03Fixture
	if err = json.Unmarshal(body, &fixture); err != nil {
		t.Fatalf("Unmarshal(%s) = %v, want nil", path, err)
	}
	return fixture
}

func runRT03Fixture(t *testing.T, fixture rt03Fixture) bool {
	t.Helper()
	world := newRT03World(t)
	env, cleanup := rt03Env(t, fixture, world)
	defer cleanup()
	sandbox := rt03Sandbox(fixture, world, env)
	tools := rt03Tools(t, sandbox.Network)
	proxy := startRT03Proxy(t, sandbox)
	built := rt03Command(t, fixture, sandbox, sandboxBuildOptions{Tools: tools, Env: mapLookup(env), Proxy: proxy.job.binds()})
	ok := runRT03Command(t, built)
	proxy.judge(t, fixture, world)
	return rt03Allowed(t, fixture, world, ok)
}

func newRT03World(t *testing.T) rt03World {
	t.Helper()
	root := t.TempDir()
	world := rt03World{
		workspace:  filepath.Join(root, "workspace"),
		eventLog:   filepath.Join(root, "control", "events"),
		configFile: filepath.Join(root, "control", "config.yaml"),
		signingKey: filepath.Join(root, "control", "seed.hex"),
		hostHome:   filepath.Join(root, "home"),
	}
	for _, dir := range []string{world.workspace, filepath.Dir(world.eventLog), world.hostHome} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) = %v, want nil", dir, err)
		}
	}
	writeRT03File(t, world.configFile, "original\n")
	writeRT03File(t, world.signingKey, "secret\n")
	return world
}

func rt03Sandbox(fixture rt03Fixture, world rt03World, env map[string]string) config.SandboxConfig {
	sandbox := config.SandboxConfig{
		Mode:      "enforce",
		Workspace: world.workspace,
		EnvAllow:  append([]string(nil), fixture.Task.EnvAllow...),
		Network:   fixture.Task.Network,
		MemoryMax: "256M",
		CPUWeight: 100,
		TasksMax:  128,
	}
	for i := 0; i < len(fixture.Task.Destinations); i++ {
		sandbox.Destinations = append(sandbox.Destinations, rt03Fill(fixture.Task.Destinations[i], env))
	}
	return sandbox
}

// rt03Fill puts the fixture's listener into a destination or target.
func rt03Fill(value string, env map[string]string) string {
	return strings.NewReplacer("{host}", env["TARGET_HOST"], "{port}", env["TARGET_PORT"]).Replace(value)
}

func rt03Env(t *testing.T, fixture rt03Fixture, world rt03World) (map[string]string, func()) {
	t.Helper()
	env := map[string]string{
		"WORKSPACE":      world.workspace,
		"EVENT_LOG_DIR":  world.eventLog,
		"CONFIG_PATH":    world.configFile,
		"HOST_HOME":      world.hostHome,
		"SIGNING_KEY":    world.signingKey,
		"SECRET_TOKEN":   "must-not-leak",
		"VISIBLE_TOKEN":  "allowed",
		"SHIM_PID":       strconv.Itoa(os.Getpid()),
		"SUPERVISOR_PID": strconv.Itoa(os.Getpid()),
	}
	cleanup := func() {}
	if strings.HasPrefix(fixture.Attempt.Kind, "tcp_connect") || fixture.Attempt.Kind == "proxy_connect" {
		host, port, stop := rt03Listener(t, fixture.Attempt.Kind)
		env["TARGET_HOST"], env["TARGET_PORT"] = host, port
		env["TARGET"] = rt03Fill(fixture.Attempt.Target, env)
		cleanup = stop
	}
	return env, cleanup
}

func rt03Tools(t *testing.T, network string) sandboxTools {
	t.Helper()
	bwrap := rt03LookPath(t, "bwrap")
	tools := sandboxTools{Bwrap: bwrap, Env: rt03LookPath(t, "env"), Systemd: false}
	if network == "egress" {
		// The production lookup, so the egress fixtures run pasta under the name production uses.
		rt03LookPath(t, "pasta")
		pasta, err := resolvePasta()
		if err != nil {
			t.Fatalf("resolvePasta() = %v, want nil", err)
		}
		tools.Pasta = pasta
	}
	return tools
}

func rt03LookPath(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatalf("LookPath(%s) = %v under CI, want installed tool", name, err)
		}
		t.Skipf("sandbox RT-03 fixture skipped: %s missing: %v", name, err)
	}
	return path
}

func rt03Command(t *testing.T, fixture rt03Fixture, sandbox config.SandboxConfig, opts sandboxBuildOptions) jobCommand {
	t.Helper()
	script := rt03Script(fixture.Attempt.Kind)
	if sandbox.Network == "proxy" {
		script = rt03RanMark + script
	}
	if opts.Hosts == "" {
		opts.Hosts = testHosts(t)
	}
	command := []string{"/usr/bin/bash", "-c", script}
	eventLog, _ := opts.Env("EVENT_LOG_DIR")
	signingKey, _ := opts.Env("SIGNING_KEY")
	opts.Paths = sandboxPaths{EventLogDir: eventLog, SigningKey: signingKey}
	built, err := buildSandboxCommand(command, sandbox, opts)
	if err != nil {
		t.Fatalf("buildSandboxCommand(%s) = %v, want nil", fixture.ID, err)
	}
	return built
}

// runRT03Command runs the built command with the builder's environment, exactly as the
// shim does, so the credentials fixtures test the production environment handling.
func runRT03Command(t *testing.T, built jobCommand) bool {
	args := built.Argv
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) // #nosec G204 -- argv built by sandbox builder under test.
	cmd.Env = built.Env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("sandbox command failed: %v output=%s argv=%q", err, out, args)
	}
	if err != nil && os.Getenv("CI") != "true" && rt03RuntimeUnavailable(string(out), err) {
		t.Skipf("sandbox RT-03 fixture skipped: runtime unavailable: %v: %s", err, out)
	}
	if ctx.Err() != nil {
		t.Fatalf("sandbox command timed out: %v output=%s", ctx.Err(), out)
	}
	return err == nil
}

func rt03Allowed(t *testing.T, fixture rt03Fixture, world rt03World, commandOK bool) bool {
	t.Helper()
	switch fixture.Attempt.Kind {
	case "write_workspace":
		return fileHas(t, filepath.Join(world.workspace, "allowed.txt"), "workspace\n")
	case "write_event_log":
		return fileHas(t, filepath.Join(world.eventLog, "probe.txt"), "event-log\n")
	case "overwrite_config":
		return fileHas(t, world.configFile, "hacked\n")
	case "write_home":
		return fileHas(t, filepath.Join(world.hostHome, "probe.txt"), "home\n")
	default:
		return commandOK
	}
}

func rt03Script(kind string) string {
	if s := rt03FsScript(kind); s != "" {
		return s
	}
	if s := rt03NetScript(kind); s != "" {
		return s
	}
	return "false"
}

func rt03FsScript(kind string) string {
	switch kind {
	case "write_event_log":
		return `mkdir -p "$(dirname "$EVENT_LOG_DIR")" && printf 'event-log\n' > "$EVENT_LOG_DIR/probe.txt"`
	case "overwrite_config":
		return `printf 'hacked\n' > "$CONFIG_PATH"`
	case "write_home":
		return `printf 'home\n' > "$HOST_HOME/probe.txt"`
	case "write_workspace":
		return `printf 'workspace\n' > "$WORKSPACE/allowed.txt"`
	case "read_secret_env":
		return `test -n "${SECRET_TOKEN+x}"`
	case "read_signing_key":
		return `test -r "$SIGNING_KEY"`
	case "read_allowed_env":
		return `test "$VISIBLE_TOKEN" = allowed`
	}
	return ""
}

func rt03NetScript(kind string) string {
	switch kind {
	case "signal_shim":
		return `kill -0 "$SHIM_PID"`
	case "signal_supervisor":
		return `kill -0 "$SUPERVISOR_PID"`
	case "signal_own_child":
		return `sleep 10 & child=$!; kill -TERM "$child"; wait "$child"; test "$?" -ne 0`
	case "tcp_connect", "tcp_connect_loopback", "tcp_connect_host":
		return `: > /dev/tcp/"$TARGET_HOST"/"$TARGET_PORT"`
	case "proxy_connect":
		return rt03ProxyConnectScript
	case "tcp_connect_gateway":
		// pasta forwards an unmapped gateway address to the outside world, where TEST-NET-1
		// never answers, so the attempt is bounded.
		return `timeout 5 bash -c ': > /dev/tcp/"$0"/"$1"' "$TARGET_HOST" "$TARGET_PORT"`
	}
	return ""
}

func rt03Listener(t *testing.T, kind string) (string, string, func()) {
	t.Helper()
	host := "127.0.0.1"
	if kind == "tcp_connect_host" {
		host = firstNonLoopbackIPv4(t)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ln := rt03ServiceListener(ctx, t, host)
	done := make(chan struct{})
	go acceptRT03(ln, done)
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort(%s) = %v, want nil", ln.Addr().String(), err)
	}
	target := host
	if kind == "tcp_connect_gateway" {
		target = sandboxNetGateway
	}
	return target, port, func() {
		if err := ln.Close(); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			t.Logf("Close(listener) = %v", err)
		}
		<-done
	}
}

// rt03ServiceListener binds below the kernel's ephemeral port range, where host services
// listen. pasta's automatic forwarding skips ephemeral ports, so a listener on port 0
// could never show a forwarding leak.
func rt03ServiceListener(ctx context.Context, t *testing.T, host string) net.Listener {
	t.Helper()
	const lowest = 10000
	body, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		t.Fatalf("read ephemeral port range: %v", err)
	}
	fields := strings.Fields(string(body))
	ephemeral, err := strconv.Atoi(fields[0])
	if err != nil || ephemeral <= lowest+256 {
		t.Fatalf("ephemeral port range %q leaves no service ports above %d: %v", body, lowest, err)
	}
	span := ephemeral - lowest
	start := os.Getpid() % span
	for i := 0; i < 256; i++ {
		port := lowest + (start+i)%span
		ln, listenErr := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if listenErr == nil {
			return ln
		}
	}
	t.Fatalf("no free service port on %s in %d attempts", host, 256)
	return nil
}

func acceptRT03(ln net.Listener, done chan<- struct{}) {
	defer close(done)
	tcp, ok := ln.(*net.TCPListener)
	if !ok {
		return
	}
	if err := tcp.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return
	}
	conn, err := ln.Accept()
	if err == nil {
		if closeErr := conn.Close(); closeErr != nil {
			return
		}
	}
}

func firstNonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("InterfaceAddrs() = %v, want nil", err)
	}
	for i := 0; i < len(addrs); i++ {
		if ip := rt03IPv4(addrs[i]); ip != "" {
			return ip
		}
	}
	if os.Getenv("CI") != "true" {
		t.Skip("sandbox RT-03 egress fixture skipped: no non-loopback IPv4 address")
	}
	t.Fatal("no non-loopback IPv4 address under CI")
	return ""
}

func rt03IPv4(addr net.Addr) string {
	ipNet, ok := addr.(*net.IPNet)
	if !ok || ipNet.IP.IsLoopback() {
		return ""
	}
	ip := ipNet.IP.To4()
	if ip == nil {
		return ""
	}
	return ip.String()
}

func mapLookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

func writeRT03File(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) = %v, want nil", path, err)
	}
}

func fileHas(t *testing.T, path string, want string) bool {
	t.Helper()
	body, err := os.ReadFile(path) // #nosec G304 -- test-controlled path.
	if err != nil {
		return false
	}
	return string(body) == want
}

func rt03RuntimeUnavailable(out string, err error) bool {
	text := fmt.Sprintf("%v %s", err, out)
	// bwrap 0.11 and later report a missing user namespace as "No permissions to create a
	// new namespace"; earlier releases as "creating new namespace failed".
	if strings.Contains(text, "Operation not permitted") || strings.Contains(text, "creating new namespace failed") || strings.Contains(text, "No permissions to create a new namespace") {
		return true
	}
	return strings.Contains(text, "/dev/net/tun")
}

func TestSandboxedJobKilledShimReleasesLock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux parent-death signal required")
	}
	requireSystemdUserScope(t)
	job := sandboxedSupervisorJob(t, "sandbox-kill", "trap : TERM; "+untilStopped("", time.Second))
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	before := statusOne(t, sup, job.Name)
	assertSafeSignalPID(t, before.ShimPID)
	proc := os.Process{Pid: before.ShimPID}
	if err := proc.Signal(os.Kill); err != nil {
		t.Fatalf("SIGKILL shim %d = %v, want nil", before.ShimPID, err)
	}
	waitForProcessGone(t, before.PID)
	waitForUnlock(t, sup, job)
}

func TestSandboxedJobStopGraceReachesChild(t *testing.T) {
	requireSystemdUserScope(t)
	// A TERM that lands before the trap is installed either kills the job at its default
	// action or, before the inner env restores the default, is ignored and escalated after
	// the grace period. Stop only once the job reports its trap installed.
	script := `trap 'printf term > "$MARKER"; exit 0' TERM; printf ready > "$PWD/ready"; ` + untilStopped("", time.Second)
	job := sandboxedSupervisorJob(t, "sandbox-term", script)
	marker := filepath.Join(job.Sandbox.Workspace, "term-marker")
	ready := filepath.Join(job.Sandbox.Workspace, "ready")
	job.Sandbox.EnvAllow = append(job.Sandbox.EnvAllow, "MARKER")
	t.Setenv("MARKER", marker)
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	if !waitUntil(func() bool { return fileSize(ready) != 0 }) {
		t.Fatalf("job never reported its TERM trap installed (%s missing)", ready)
	}
	if err := sup.Stop(testContext(t), job.Name); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	if !fileHas(t, marker, "term") {
		body, err := os.ReadFile(job.LogPath)
		if err != nil {
			body = []byte(err.Error())
		}
		t.Fatalf("marker %s missing, want graceful TERM, log=%s", marker, body)
	}
}

func TestSandboxedJobCgroupMemoryMax(t *testing.T) {
	requireSystemdUserScope(t)
	job := sandboxedSupervisorJob(t, "sandbox-cgroup", untilStopped("", time.Second))
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	st := statusOne(t, sup, job.Name)
	cgroup, memoryMax, err := jobCgroupMemoryMax(st.PID)
	if err != nil {
		t.Skipf("cgroup memory.max unavailable: %v", err)
	}
	if unit := filepath.Base(cgroup); !strings.HasPrefix(unit, scopeNamePrefix) || !strings.HasSuffix(unit, ".scope") {
		t.Fatalf("cgroup = %q, want the job's own %s*.scope", cgroup, scopeNamePrefix)
	}
	if memoryMax != "268435456" {
		t.Fatalf("memory.max = %q, want 268435456", memoryMax)
	}
	if high := cgroupValue(t, cgroup, "memory.high"); high != "max" {
		t.Fatalf("memory.high = %q without a soft limit, want max", high)
	}
}

// TestSandboxedJobCgroupMemoryHigh: the soft limit of the job's config is the memory.high of
// the job's own scope.
func TestSandboxedJobCgroupMemoryHigh(t *testing.T) {
	requireSystemdUserScope(t)
	job := sandboxedSupervisorJob(t, "sandbox-soft", untilStopped("", time.Second))
	job.Sandbox.MemoryHigh = "128M"
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	cgroup, memoryMax, err := jobCgroupMemoryMax(statusOne(t, sup, job.Name).PID)
	if err != nil {
		t.Skipf("cgroup memory.max unavailable: %v", err)
	}
	if high := cgroupValue(t, cgroup, "memory.high"); high != "134217728" || memoryMax != "268435456" {
		t.Fatalf("memory.high = %q, memory.max = %q, want 134217728 and 268435456", high, memoryMax)
	}
}

func cgroupValue(t *testing.T, cgroup string, file string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", cgroup, file)) // #nosec G304 -- cgroup path comes from /proc for the child under test.
	if err != nil {
		t.Fatalf("ReadFile(%s of %s) = %v, want nil", file, cgroup, err)
	}
	return strings.TrimSpace(string(body))
}

// TestScopeArgumentsCarryTheSoftLimitOnlyWhenSet pins the argv: MemoryHigh appears between
// MemoryMax and CPUWeight when the job has a soft limit, and not at all otherwise.
func TestScopeArgumentsCarryTheSoftLimitOnlyWhenSet(t *testing.T) {
	sandbox := config.SandboxConfig{MemoryMax: "2G", CPUWeight: 100, TasksMax: 512}
	without := strings.Join(systemdRunArgs("/usr/bin/systemd-run", "x.scope", sandbox), " ")
	if strings.Contains(without, "MemoryHigh") || !strings.HasSuffix(without, "-p MemoryMax=2G -p CPUWeight=100 -p TasksMax=512 --") {
		t.Fatalf("systemd-run args without a soft limit = %q, want no MemoryHigh", without)
	}
	sandbox.MemoryHigh = "1536M"
	with := strings.Join(systemdRunArgs("/usr/bin/systemd-run", "x.scope", sandbox), " ")
	if !strings.HasSuffix(with, "-p MemoryMax=2G -p MemoryHigh=1536M -p CPUWeight=100 -p TasksMax=512 --") {
		t.Fatalf("systemd-run args with a soft limit = %q, want MemoryHigh=1536M after MemoryMax", with)
	}
	// The shim reads back what the supervisor hands it.
	job := testJob("handover", []string{"/bin/true"})
	job.Sandbox = config.SandboxConfig{Mode: "enforce", Workspace: t.TempDir(), Network: "none", MemoryMax: "2G", MemoryHigh: "1536M", CPUWeight: 100, TasksMax: 512}
	sup := testSupervisor(t, job)
	cmd := sup.shimCmd(testContext(t), job)
	cfg, err := parseShimArgs(cmd.Args[lastArgIndex(cmd.Args, "__job-shim"):])
	if err != nil || cfg.Sandbox.MemoryHigh != "1536M" || cfg.Sandbox.MemoryMax != "2G" {
		t.Fatalf("shim config = %+v, %v, want memory_high 1536M and memory_max 2G", cfg.Sandbox, err)
	}
}

// requireSystemdUserScope stops a test that needs a systemd user scope where there is none.
// The check is the shim's own: a process started the way the shim starts a job must end up
// in a scope of its own; systemd-run exiting 0 is no such proof. Under CI the scope is part
// of what the runner must provide, so its absence fails the test; elsewhere it skips.
func requireSystemdUserScope(t *testing.T) {
	t.Helper()
	latency, err := systemdUserScopeProbe(t)
	if err != nil {
		reportMissingScope(t, os.Getenv("CI") == "true", err)
		return
	}
	scopeFactsOnce.Do(func() {
		t.Logf("scope probe: %s; a job entered its scope %s after systemd-run started (the shim waits up to %s)", systemdVersion(t), latency.Round(time.Millisecond), scopeConfirmPolls*scopeConfirmInterval)
	})
}

var scopeFactsOnce sync.Once

// scopeStopper is the part of testing.T that reportMissingScope uses.
type scopeStopper interface {
	Helper()
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}

func reportMissingScope(tb scopeStopper, underCI bool, cause error) {
	tb.Helper()
	if underCI {
		tb.Fatalf("no systemd user scope under CI, want the runner to provide one: %v", cause)
		return
	}
	tb.Skipf("sandbox supervisor integration skipped: %v", cause)
}

// systemdUserScopeProbe starts a process through the production scope arguments and reports
// how long it took to appear in its scope, or why it did not.
func systemdUserScopeProbe(t *testing.T) (time.Duration, error) {
	t.Helper()
	rt03LookPath(t, "systemd-run")
	runner, err := resolveExecutable("systemd-run")
	if err != nil {
		t.Fatalf("resolveExecutable(systemd-run) = %v, want nil", err)
	}
	scope, err := newScopeName()
	if err != nil {
		t.Fatalf("newScopeName() = %v, want nil", err)
	}
	args := append(systemdRunArgs(runner, scope, config.SandboxConfig{MemoryMax: "64M", CPUWeight: 100, TasksMax: 16}), "/usr/bin/sleep", "30")
	var out bytes.Buffer
	cmd := exec.CommandContext(t.Context(), args[0], args[1:]...) // #nosec G204 -- fixed probe through the production scope arguments.
	cmd.Stdout, cmd.Stderr = &out, &out
	began := time.Now()
	if err = cmd.Start(); err != nil {
		return 0, fmt.Errorf("systemd-run did not start: %w", err)
	}
	confirmErr := confirmJobScope(cmd.Process.Pid, scope, runner)
	latency := time.Since(began)
	if err = cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Kill(scope probe %d) = %v, want nil", cmd.Process.Pid, err)
	}
	if err = cmd.Wait(); err == nil && confirmErr == nil {
		t.Fatalf("Wait(killed scope probe) = nil, want its kill status")
	}
	if confirmErr != nil {
		own, cgroupErr := processCgroup(os.Getpid())
		if cgroupErr != nil {
			own = "unknown: " + cgroupErr.Error()
		}
		return 0, fmt.Errorf("no systemd user scope for a job started from cgroup %q: %w: %s", own, confirmErr, strings.TrimSpace(out.String()))
	}
	return latency, nil
}

// systemdVersion returns the first line of systemd-run --version, for the test log.
func systemdVersion(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "systemd-run", "--version").Output()
	if err != nil {
		return "systemd version unknown: " + err.Error()
	}
	first, _, _ := strings.Cut(string(out), "\n")
	return first
}

func jobCgroupMemoryMax(pid int) (string, string, error) {
	cgroup, err := processCgroup(pid)
	if err != nil {
		return "", "", err
	}
	body, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", cgroup, "memory.max")) // #nosec G304 -- cgroup path comes from /proc for the child under test.
	if err != nil {
		return cgroup, "", err
	}
	return cgroup, strings.TrimSpace(string(body)), nil
}

func sandboxedSupervisorJob(t *testing.T, name string, script string) config.JobConfig {
	t.Helper()
	workspace := t.TempDir()
	job := testJob(name, []string{"/usr/bin/bash", "-c", script})
	// testJob's log is one file per job name for the whole test process; a sandbox failure
	// message must show this test's output only.
	job.LogPath = filepath.Join(t.TempDir(), name+".log")
	job.Sandbox = config.SandboxConfig{Mode: "enforce", Workspace: workspace, Network: "none", MemoryMax: "256M", CPUWeight: 100, TasksMax: 128}
	job.Stop.GraceSeconds = 2
	return job
}

func TestSandboxArgvCarriesNoEnvValues(t *testing.T) {
	const secret = "s3cr3t-value-never-on-argv"
	sandbox := config.SandboxConfig{Mode: "enforce", Workspace: t.TempDir(), Network: "egress", MemoryMax: "2G", CPUWeight: 100, TasksMax: 512, EnvAllow: []string{"FORGE_TOKEN"}}
	built, err := buildSandboxCommand([]string{"/usr/bin/true"}, sandbox, sandboxBuildOptions{
		Tools: sandboxTools{SystemdRun: "/usr/bin/systemd-run", Bwrap: "/usr/bin/bwrap", Pasta: "/usr/bin/pasta", Env: "/usr/bin/env", Systemd: true},
		Env:   mapLookup(map[string]string{"FORGE_TOKEN": secret, "XDG_RUNTIME_DIR": "/run/user/1000", "UNLISTED": "x"}),
		Hosts: testHosts(t),
	})
	if err != nil {
		t.Fatalf("buildSandboxCommand() = %v, want nil", err)
	}
	for i := 0; i < len(built.Argv); i++ {
		if strings.Contains(built.Argv[i], secret) {
			t.Fatalf("argv[%d] = %q carries the secret, want it only in the environment", i, built.Argv[i])
		}
	}
	env := strings.Join(built.Env, "\n")
	if !strings.Contains(env, "FORGE_TOKEN="+secret) || !strings.Contains(env, "XDG_RUNTIME_DIR=/run/user/1000") || strings.Contains(env, "UNLISTED=") {
		t.Fatalf("env = %q, want the allowlisted and bus variables only", built.Env)
	}
	// The builder binds and runs the command at its resolved path; some distributions make
	// /usr/bin/true a symlink.
	truePath, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatalf("EvalSymlinks(/usr/bin/true) = %v, want nil", err)
	}
	argv := strings.Join(built.Argv, " ")
	for _, want := range []string{"-- /usr/bin/env --ignore-signal=TERM /usr/bin/pasta", "--address 192.0.2.2", "--map-host-loopback none", "-T none -U none", "-D none", "--unsetenv XDG_RUNTIME_DIR", "--unsetenv DBUS_SESSION_BUS_ADDRESS", "--unsetenv INVOCATION_ID", "-- /usr/bin/env --default-signal=TERM " + truePath} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv = %q, want %q", argv, want)
		}
	}
}

func TestSandboxJobSeesExactlyItsEnvironment(t *testing.T) {
	tools := rt03Tools(t, "none")
	sandbox := config.SandboxConfig{Mode: "enforce", Workspace: t.TempDir(), Network: "none", MemoryMax: "2G", CPUWeight: 100, TasksMax: 512, EnvAllow: []string{"ALLOWED"}}
	built, err := buildSandboxCommand([]string{"/usr/bin/env"}, sandbox, sandboxBuildOptions{
		Tools: tools,
		Env:   mapLookup(map[string]string{"ALLOWED": "yes", "SECRET": "no", "XDG_RUNTIME_DIR": "/run/user/1", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/x"}),
		Hosts: testHosts(t),
	})
	if err != nil {
		t.Fatalf("buildSandboxCommand() = %v, want nil", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, built.Argv[0], built.Argv[1:]...) // #nosec G204 -- argv built by sandbox builder under test.
	cmd.Env = append(append([]string(nil), built.Env...), "INVOCATION_ID=systemd-would-add-this")
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && os.Getenv("CI") != "true" && rt03RuntimeUnavailable(string(exit.Stderr), err) {
			t.Skipf("sandbox environment test skipped: runtime unavailable: %v: %s", err, exit.Stderr)
		}
		t.Fatalf("sandboxed env = %v, want nil", err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		names = append(names, strings.SplitN(line, "=", 2)[0])
	}
	sort.Strings(names)
	if got, want := strings.Join(names, ","), "ALLOWED,HOME,PATH,PWD,TMPDIR"; got != want {
		t.Fatalf("variables inside the sandbox = %s, want exactly %s", got, want)
	}
}

func TestSandboxRuntimeRefusesWorkspaceChangedSinceLoad(t *testing.T) {
	base := t.TempDir()
	other := filepath.Join(base, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatalf("Mkdir(other) = %v, want nil", err)
	}
	workspace := filepath.Join(base, "workspace")
	if err := os.Symlink(other, workspace); err != nil {
		t.Fatalf("Symlink(workspace) = %v, want nil", err)
	}
	sandbox := config.SandboxConfig{Mode: "enforce", Workspace: workspace, Network: "none", MemoryMax: "2G", CPUWeight: 100, TasksMax: 512}
	_, err := buildSandboxCommand([]string{"/usr/bin/true"}, sandbox, sandboxBuildOptions{
		Tools: sandboxTools{Bwrap: "/usr/bin/bwrap"},
	})
	if err == nil || !strings.Contains(err.Error(), "changed since config load") {
		t.Fatalf("buildSandboxCommand(workspace now a symlink) = %v, want refusal", err)
	}
}

// TestSandboxedJobDiesWithItsShim proves the job inside the sandbox, not only bwrap, dies
// with a killed shim: the job's heartbeat file stops growing.
func TestSandboxedJobDiesWithItsShim(t *testing.T) {
	requireSystemdUserScope(t)
	job := sandboxedSupervisorJob(t, "sandbox-orphan", untilStopped(`printf . >> "$PWD/beat";`, 50*time.Millisecond))
	beat := filepath.Join(job.Sandbox.Workspace, "beat")
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	if !waitUntil(func() bool { return fileSize(beat) != 0 }) {
		t.Fatalf("heartbeat %s never started", beat)
	}
	before := statusOne(t, sup, job.Name)
	assertSafeSignalPID(t, before.ShimPID)
	proc := os.Process{Pid: before.ShimPID}
	if err := proc.Signal(os.Kill); err != nil {
		t.Fatalf("SIGKILL shim %d = %v, want nil", before.ShimPID, err)
	}
	waitForProcessGone(t, before.PID)
	stable := false
	for i := 0; i < 20 && !stable; i++ {
		first := fileSize(beat)
		time.Sleep(300 * time.Millisecond)
		stable = fileSize(beat) == first
	}
	if !stable {
		t.Fatalf("heartbeat %s still growing after the shim was killed: the sandboxed job outlived its shim", beat)
	}
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func TestSandboxPreflightRefusesTIOCSTI(t *testing.T) {
	old := sandboxReadFile
	t.Cleanup(func() { sandboxReadFile = old })
	sandboxReadFile = func(string) ([]byte, error) { return []byte("1\n"), nil }
	if err := requireTIOCSTIDisabled(); err == nil || !strings.Contains(err.Error(), "must be 0") {
		t.Fatalf("requireTIOCSTIDisabled(legacy_tiocsti=1) = %v, want refusal", err)
	}
	sandboxReadFile = func(string) ([]byte, error) { return []byte("0\n"), nil }
	if err := requireTIOCSTIDisabled(); err != nil {
		t.Fatalf("requireTIOCSTIDisabled(legacy_tiocsti=0) = %v, want nil", err)
	}
}

func TestSandboxPreflightRefusesMissingPasta(t *testing.T) {
	rt03LookPath(t, "systemd-run")
	rt03LookPath(t, "bwrap")
	old := sandboxLookPath
	t.Cleanup(func() { sandboxLookPath = old })
	sandboxLookPath = func(name string) (string, error) {
		if name == "pasta" {
			return "", exec.ErrNotFound
		}
		return old(name)
	}
	if _, err := sandboxToolchain(config.SandboxConfig{Network: "egress"}, sandboxTools{}); err == nil || !strings.Contains(err.Error(), "sandbox: pasta") {
		t.Fatalf("sandboxToolchain(egress, no pasta) = %v, want pasta refusal", err)
	}
}

func TestSandboxRuntimeRefusesWorkspaceOverlappingEventLog(t *testing.T) {
	base := t.TempDir()
	events := filepath.Join(base, "events")
	if err := os.Mkdir(events, 0o700); err != nil {
		t.Fatalf("Mkdir(events) = %v, want nil", err)
	}
	sandbox := config.SandboxConfig{Mode: "enforce", Workspace: base, Network: "none", MemoryMax: "2G", CPUWeight: 100, TasksMax: 512}
	_, err := buildSandboxCommand([]string{"/usr/bin/true"}, sandbox, sandboxBuildOptions{
		Paths: sandboxPaths{EventLogDir: events},
		Tools: sandboxTools{Bwrap: "/usr/bin/bwrap", Env: "/usr/bin/env"},
	})
	if err == nil || !strings.Contains(err.Error(), "overlaps protected path") {
		t.Fatalf("buildSandboxCommand(workspace contains the event log) = %v, want refusal", err)
	}
}

// withToolLookPath makes the sandbox preflight find one tool at the given path.
func withToolLookPath(t *testing.T, tool string, path string) {
	t.Helper()
	old := sandboxLookPath
	t.Cleanup(func() { sandboxLookPath = old })
	sandboxLookPath = func(name string) (string, error) {
		if name == tool {
			return path, nil
		}
		return old(name)
	}
}

func TestSandboxPreflightRefusesEnvOutsideUsr(t *testing.T) {
	body, err := os.ReadFile(rt03LookPath(t, "env"))
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "env")
	if err = os.WriteFile(outside, body, 0o700); err != nil {
		t.Fatalf("copy env: %v", err)
	}
	withToolLookPath(t, "env", outside)
	if _, err = resolveSignalEnv(); err == nil || !strings.Contains(err.Error(), "outside /usr") {
		t.Fatalf("resolveSignalEnv(%s) = %v, want outside-/usr refusal", outside, err)
	}
}

func TestSandboxPreflightRefusesEnvWithoutSignalFlags(t *testing.T) {
	withToolLookPath(t, "env", rt03LookPath(t, "false"))
	if _, err := resolveSignalEnv(); err == nil || !strings.Contains(err.Error(), "lacks --ignore-signal") {
		t.Fatalf("resolveSignalEnv(false) = %v, want probe refusal", err)
	}
}

// oldScopeRunner stands in for a systemd-run older than 254: it knows --version and refuses
// --expand-environment the way getopt refuses an unknown option.
const oldScopeRunner = `#!/bin/sh
for arg in "$@"; do
	case "$arg" in
	--expand-environment=*) echo "systemd-run: unrecognized option '$arg'" >&2; exit 1 ;;
	esac
done
echo "systemd 253 (stand-in)"
`

func TestSandboxPreflightRefusesScopeRunnerWithoutNoExpand(t *testing.T) {
	old := filepath.Join(t.TempDir(), "systemd-run")
	if err := os.WriteFile(old, []byte(oldScopeRunner), 0o700); err != nil { // #nosec G306 -- a stand-in the test must be able to start.
		t.Fatalf("WriteFile(stand-in) = %v, want nil", err)
	}
	withToolLookPath(t, "systemd-run", old)
	if _, err := resolveScopeRunner(); err == nil || !strings.Contains(err.Error(), "lacks --expand-environment") || !strings.Contains(err.Error(), "unrecognized option") {
		t.Fatalf("resolveScopeRunner(stand-in for systemd 253) = %v, want a refusal that names the switch and carries the tool's message", err)
	}
	if _, err := sandboxToolchain(config.SandboxConfig{Network: "none"}, sandboxTools{}); err == nil || !strings.Contains(err.Error(), "lacks --expand-environment") {
		t.Fatalf("sandboxToolchain(stand-in for systemd 253) = %v, want the preflight to refuse", err)
	}
	withToolLookPath(t, "systemd-run", filepath.Join(t.TempDir(), "absent"))
	if _, err := resolveScopeRunner(); err == nil || !strings.Contains(err.Error(), "sandbox: systemd-run:") || strings.Contains(err.Error(), "lacks") {
		t.Fatalf("resolveScopeRunner(absent) = %v, want the missing tool named, not its switch", err)
	}
}

// TestSandboxedJobArgumentsArriveUnexpanded: a job gets its arguments as its config wrote
// them. systemd-run would rewrite them from its own environment, which carries the env_allow
// values: the value would land on the argv, and an unset $NAME argument would be dropped.
func TestSandboxedJobArgumentsArriveUnexpanded(t *testing.T) {
	requireSystemdUserScope(t)
	sandbox := config.SandboxConfig{Mode: "enforce", Workspace: t.TempDir(), Network: "none", MemoryMax: "256M", CPUWeight: 100, TasksMax: 128, EnvAllow: []string{"ALLOWED"}}
	requireSandboxToolchain(t, sandbox)
	args := []string{"${ALLOWED}", "$ALLOWED", "a$$b", "$UNSET_NAME", "--flag=${ALLOWED}"}
	built, err := buildSandboxCommand(append([]string{"/usr/bin/printf", `%s\n`}, args...), sandbox, sandboxBuildOptions{
		Env: func(name string) (string, bool) {
			if name == "ALLOWED" {
				return "value-from-the-environment", true
			}
			return os.LookupEnv(name)
		},
		Hosts: testHosts(t),
	})
	if err != nil {
		t.Fatalf("buildSandboxCommand() = %v, want nil", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, built.Argv[0], built.Argv[1:]...) // #nosec G204 -- argv built by sandbox builder under test.
	cmd.Env = built.Env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if os.Getenv("CI") != "true" && rt03RuntimeUnavailable(stderr.String(), err) {
			t.Skipf("sandbox argument test skipped: runtime unavailable: %v: %s", err, stderr.String())
		}
		t.Fatalf("sandboxed printf = %v, want nil; stderr=%s", err, stderr.String())
	}
	if got, want := string(out), strings.Join(args, "\n")+"\n"; got != want {
		t.Fatalf("arguments inside the sandbox = %q, want them unchanged: %q; stderr=%s", got, want, stderr.String())
	}
}

func TestSandboxRuntimeRefusesInputExposingSigningKey(t *testing.T) {
	keyDir := t.TempDir()
	key := filepath.Join(keyDir, "seed")
	if err := os.WriteFile(key, []byte("seed"), 0o600); err != nil {
		t.Fatalf("WriteFile(key) = %v, want nil", err)
	}
	sandbox := config.SandboxConfig{Mode: "enforce", Workspace: t.TempDir(), Inputs: []string{keyDir}, Network: "none", MemoryMax: "2G", CPUWeight: 100, TasksMax: 512}
	_, err := buildSandboxCommand([]string{"/usr/bin/true"}, sandbox, sandboxBuildOptions{
		Paths: sandboxPaths{SigningKey: key},
		Tools: sandboxTools{Bwrap: "/usr/bin/bwrap", Env: "/usr/bin/env"},
	})
	if err == nil || !strings.Contains(err.Error(), "input "+keyDir+" overlaps protected path") {
		t.Fatalf("buildSandboxCommand(input contains the signing key) = %v, want refusal", err)
	}
}

func TestSandboxRuntimeRefusesInputChangedSinceLoad(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir(target) = %v, want nil", err)
	}
	input := filepath.Join(base, "input")
	if err := os.Symlink(target, input); err != nil {
		t.Fatalf("Symlink(input) = %v, want nil", err)
	}
	sandbox := config.SandboxConfig{Mode: "enforce", Workspace: t.TempDir(), Inputs: []string{input}, Network: "none", MemoryMax: "2G", CPUWeight: 100, TasksMax: 512}
	_, err := buildSandboxCommand([]string{"/usr/bin/true"}, sandbox, sandboxBuildOptions{
		Tools: sandboxTools{Bwrap: "/usr/bin/bwrap", Env: "/usr/bin/env"},
	})
	if err == nil || !strings.Contains(err.Error(), "input "+input+" now resolves to "+target) {
		t.Fatalf("buildSandboxCommand(input now a symlink) = %v, want refusal", err)
	}
}

func TestRT03RuntimeUnavailableRecognisesBwrapMessages(t *testing.T) {
	exit := errors.New("exit status 1")
	for _, out := range []string{
		"bwrap: No permissions to create a new namespace, likely because the kernel does not allow non-privileged user namespaces.",
		"bwrap: Creating new namespace failed: Operation not permitted",
	} {
		if !rt03RuntimeUnavailable(out, exit) {
			t.Fatalf("rt03RuntimeUnavailable(%q) = false, want true", out)
		}
	}
	if rt03RuntimeUnavailable("bash: line 1: /workspace/x: Read-only file system", exit) {
		t.Fatal("rt03RuntimeUnavailable(job failure) = true, want false: a job failure must not skip")
	}
}

func TestResolvePastaKeepsTheNamePastaChoosesItsModeBy(t *testing.T) {
	dir := t.TempDir()
	passt := filepath.Join(dir, "passt")
	if err := os.WriteFile(passt, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("WriteFile(passt) = %v, want nil", err)
	}
	pasta := filepath.Join(dir, "pasta")
	if err := os.Symlink("passt", pasta); err != nil {
		t.Fatalf("Symlink(pasta) = %v, want nil", err)
	}
	old := sandboxLookPath
	t.Cleanup(func() { sandboxLookPath = old })
	sandboxLookPath = func(name string) (string, error) {
		if name == "pasta" {
			return pasta, nil
		}
		return old(name)
	}
	got, err := resolvePasta()
	if err != nil || got != pasta {
		t.Fatalf("resolvePasta() = %q, %v, want %q: started as passt it refuses --config-net", got, err, pasta)
	}
	sandboxLookPath = func(string) (string, error) { return "pasta", nil }
	if _, err = resolvePasta(); err == nil || !strings.Contains(err.Error(), "not absolute") {
		t.Fatalf("resolvePasta(relative) = %v, want refusal", err)
	}
}
