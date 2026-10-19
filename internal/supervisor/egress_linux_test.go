//go:build linux

package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/egressproxy"
)

func proxyTestSandbox(workspace string, destinations ...string) config.SandboxConfig {
	return config.SandboxConfig{
		Mode: "enforce", Workspace: workspace, Network: "proxy", Destinations: destinations,
		MemoryMax: "256M", CPUWeight: 100, TasksMax: 128,
	}
}

// proxyTestBinds makes the two files a sandbox with network=proxy binds.
func proxyTestBinds(t *testing.T) sandboxProxy {
	t.Helper()
	dir := t.TempDir()
	proxy := sandboxProxy{Socket: filepath.Join(dir, "s"), Relay: filepath.Join(dir, "relay")}
	writeRT03File(t, proxy.Socket, "")
	writeRT03File(t, proxy.Relay, "")
	return proxy
}

func proxyTestArgv(t *testing.T, sandbox config.SandboxConfig, proxy sandboxProxy) string {
	t.Helper()
	built, err := buildSandboxCommand([]string{"/usr/bin/true", "x"}, sandbox, sandboxBuildOptions{
		Tools: sandboxTools{SystemdRun: "/usr/bin/systemd-run", Bwrap: "/usr/bin/bwrap", Pasta: "/usr/bin/pasta", Env: "/usr/bin/env", Systemd: true},
		Proxy: proxy,
		Hosts: testHosts(t),
	})
	if err != nil {
		t.Fatalf("buildSandboxCommand(network=%s) = %v, want nil", sandbox.Network, err)
	}
	return strings.Join(built.Argv, " ")
}

func TestProxySandboxArgvBindsTheProxyAndStartsTheRelay(t *testing.T) {
	proxy := proxyTestBinds(t)
	argv := proxyTestArgv(t, proxyTestSandbox(t.TempDir(), "gateway.example:443"), proxy)
	truePath, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatalf("EvalSymlinks(/usr/bin/true) = %v, want nil", err)
	}
	wanted := []string{
		"--ro-bind " + proxy.Socket + " /run/epitrop/proxy.sock",
		"--ro-bind " + proxy.Relay + " /run/epitrop/relay",
		"--setenv HTTPS_PROXY http://127.0.0.1:3128", "--setenv https_proxy http://127.0.0.1:3128",
		"--setenv HTTP_PROXY http://127.0.0.1:3128", "--setenv http_proxy http://127.0.0.1:3128",
		"--setenv GOMAXPROCS 2 --",
		"/usr/bin/env --ignore-signal=TERM /usr/bin/bwrap --die-with-parent --unshare-all --ro-bind /usr /usr",
		"-- /run/epitrop/relay __egress-relay --socket /run/epitrop/proxy.sock --listen 127.0.0.1:3128 -- /usr/bin/env --default-signal=TERM --unset=GOMAXPROCS " + truePath + " x",
	}
	for i := 0; i < len(wanted); i++ {
		if !strings.Contains(argv, wanted[i]) {
			t.Errorf("argv = %q, want %q", argv, wanted[i])
		}
	}
	// The job has no route out: no shared network, no resolver, no pasta.
	refused := []string{"--share-net", "resolv.conf", "/usr/bin/pasta"}
	for i := 0; i < len(refused); i++ {
		if strings.Contains(argv, refused[i]) {
			t.Errorf("argv = %q, want no %q with network=proxy", argv, refused[i])
		}
	}
}

func TestSandboxArgvWithoutProxyNamesNoneOfIt(t *testing.T) {
	networks := []string{"none", "egress"}
	for i := 0; i < len(networks); i++ {
		sandbox := proxyTestSandbox(t.TempDir())
		sandbox.Network = networks[i]
		argv := proxyTestArgv(t, sandbox, sandboxProxy{})
		refused := []string{"/run/epitrop", "__egress-relay", "_PROXY", "_proxy", sandboxProxyListen, sandboxProxyURL, "GOMAXPROCS", "--unset="}
		for j := 0; j < len(refused); j++ {
			if strings.Contains(argv, refused[j]) {
				t.Errorf("argv(network=%s) = %q, want no %q", networks[i], argv, refused[j])
			}
		}
	}
}

func TestEgressSandboxArgvDisablesDNS(t *testing.T) {
	sandbox := proxyTestSandbox(t.TempDir())
	sandbox.Network = "egress"
	argv := proxyTestArgv(t, sandbox, sandboxProxy{})
	wanted := []string{"--map-host-loopback none", "-t none -u none -T none -U none", "-D none", "--quiet --"}
	for i := 0; i < len(wanted); i++ {
		if !strings.Contains(argv, wanted[i]) {
			t.Errorf("argv(network=egress) = %q, want %q", argv, wanted[i])
		}
	}
}

func TestValidateSandboxProxyBothWays(t *testing.T) {
	binds := proxyTestBinds(t)
	missing := filepath.Join(t.TempDir(), "missing")
	tests := []struct {
		name    string
		network string
		proxy   sandboxProxy
		want    string
	}{
		{"proxy with both files", "proxy", binds, ""},
		{"proxy with nothing", "proxy", sandboxProxy{}, "needs the proxy socket and the relay program"},
		{"proxy with the socket only", "proxy", sandboxProxy{Socket: binds.Socket}, "needs the proxy socket and the relay program"},
		{"proxy with the relay only", "proxy", sandboxProxy{Relay: binds.Relay}, "needs the proxy socket and the relay program"},
		{"proxy with a relative socket", "proxy", sandboxProxy{Socket: "s", Relay: binds.Relay}, "as absolute paths"},
		{"proxy with a relative relay", "proxy", sandboxProxy{Socket: binds.Socket, Relay: "relay"}, "as absolute paths"},
		{"proxy with a socket that is gone", "proxy", sandboxProxy{Socket: missing, Relay: binds.Relay}, "sandbox: stat " + missing},
		{"proxy with a relay that is gone", "proxy", sandboxProxy{Socket: binds.Socket, Relay: missing}, "sandbox: stat " + missing},
		{"none with nothing", "none", sandboxProxy{}, ""},
		{"egress with nothing", "egress", sandboxProxy{}, ""},
		{"none with both files", "none", binds, "network=none takes no proxy"},
		{"egress with both files", "egress", binds, "network=egress takes no proxy"},
		{"none with the socket only", "none", sandboxProxy{Socket: binds.Socket}, "network=none takes no proxy"},
		{"egress with the relay only", "egress", sandboxProxy{Relay: binds.Relay}, "network=egress takes no proxy"},
	}
	for i := 0; i < len(tests); i++ {
		err := validateSandboxProxy(config.SandboxConfig{Network: tests[i].network}, tests[i].proxy)
		if tests[i].want == "" && err != nil {
			t.Errorf("validateSandboxProxy(%s) = %v, want nil", tests[i].name, err)
		}
		if tests[i].want != "" && (err == nil || !strings.Contains(err.Error(), tests[i].want)) {
			t.Errorf("validateSandboxProxy(%s) = %v, want an error with %q", tests[i].name, err, tests[i].want)
		}
	}
}

// The builder is where the rule holds for a real start: a sandbox that names the proxy mode
// and gets no proxy is not built, and neither is another mode that gets one.
func TestBuildSandboxCommandRefusesAProxyThatDoesNotFitTheMode(t *testing.T) {
	tools := sandboxTools{Bwrap: "/usr/bin/bwrap", Pasta: "/usr/bin/pasta", Env: "/usr/bin/env"}
	_, err := buildSandboxCommand([]string{"/usr/bin/true"}, proxyTestSandbox(t.TempDir(), "gateway.example:443"), sandboxBuildOptions{Tools: tools})
	if err == nil || !strings.Contains(err.Error(), "network=proxy needs the proxy socket") {
		t.Fatalf("buildSandboxCommand(proxy, no binds) = %v, want the refusal", err)
	}
	none := proxyTestSandbox(t.TempDir())
	none.Network = "none"
	_, err = buildSandboxCommand([]string{"/usr/bin/true"}, none, sandboxBuildOptions{Tools: tools, Proxy: proxyTestBinds(t)})
	if err == nil || !strings.Contains(err.Error(), "network=none takes no proxy") {
		t.Fatalf("buildSandboxCommand(none, binds) = %v, want the refusal", err)
	}
}

func TestShimArgsCarryTheDestinations(t *testing.T) {
	tests := [][]string{
		{"gateway.example:443"},
		{"gateway.example:443", "127.0.0.1:4000", "[::1]:4000"},
		proxyTestDestinations(egressproxy.MaxDestinations),
	}
	for i := 0; i < len(tests); i++ {
		cfg := withShimSandbox(validShimConfig(t, "proxy-args"), proxyTestSandbox(t.TempDir(), tests[i]...))
		parsed, err := parseShimArgs(shimArgs(t, cfg))
		if err != nil || !slices.Equal(parsed.Sandbox.Destinations, tests[i]) || parsed.Sandbox.Network != "proxy" {
			t.Errorf("parseShimArgs(%d destinations) = %v network %q, %v, want them in order", len(tests[i]), parsed.Sandbox.Destinations, parsed.Sandbox.Network, err)
		}
	}
	none := proxyTestSandbox(t.TempDir())
	none.Network = "none"
	if args := sandboxFlagArgs(none); slices.Contains(args, "--sandbox-destination") {
		t.Errorf("sandboxFlagArgs(no destinations) = %q, want no --sandbox-destination", args)
	}
}

// The shim checks what it is given, whoever started it: the rules of the configuration hold
// on its command line too.
func TestShimRefusesDestinationsTheConfigurationRefuses(t *testing.T) {
	tests := []struct {
		name         string
		network      string
		destinations []string
		want         string
	}{
		{"one more than the limit", "proxy", proxyTestDestinations(egressproxy.MaxDestinations + 1), "destinations"},
		{"none with proxy", "proxy", nil, "destinations"},
		{"a wildcard", "proxy", []string{"*.example:443"}, "host name label"},
		{"a second spelling", "proxy", []string{"Gateway.example:443"}, `write it as "gateway.example:443"`},
		{"with network none", "none", []string{"gateway.example:443"}, "destinations"},
		{"with network egress", "egress", []string{"gateway.example:443"}, "destinations"},
	}
	for i := 0; i < len(tests); i++ {
		sandbox := proxyTestSandbox(t.TempDir(), tests[i].destinations...)
		sandbox.Network = tests[i].network
		_, err := parseShimArgs(shimArgs(t, withShimSandbox(validShimConfig(t, "proxy-refused"), sandbox)))
		if err == nil || !strings.Contains(err.Error(), "job-shim: sandbox.") || !strings.Contains(err.Error(), tests[i].want) {
			t.Errorf("parseShimArgs(%s) = %v, want a sandbox refusal with %q", tests[i].name, err, tests[i].want)
		}
	}
}

func proxyTestDestinations(count int) []string {
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, fmt.Sprintf("gateway%d.example:443", i))
	}
	return out
}

func TestStartJobProxyOnlyForTheProxyMode(t *testing.T) {
	tests := []config.SandboxConfig{
		{Mode: "off", Reason: "unit test"},
		{Mode: "enforce", Network: "none"},
		{Mode: "enforce", Network: "egress"},
		// Normalising gives a sandbox without a network the mode none.
		{Mode: "enforce"},
		// Only an enforced sandbox binds a proxy's socket; without one a proxy serves nobody.
		{Mode: "off", Reason: "unit test", Network: "proxy", Destinations: []string{"gateway.example:443"}},
	}
	for i := 0; i < len(tests); i++ {
		proxy, err := startJobProxy(t.Context(), shimConfig{Sandbox: tests[i]}, &bytes.Buffer{})
		if err != nil || proxy.server != nil || proxy.binds() != (sandboxProxy{}) {
			t.Errorf("startJobProxy(%s/%s) = %+v, %v, want the zero value", tests[i].Mode, tests[i].Network, proxy.binds(), err)
		}
		if err = proxy.stop(); err != nil {
			t.Errorf("stop(no proxy, %s/%s) = %v, want nil", tests[i].Mode, tests[i].Network, err)
		}
	}
}

func TestStartJobProxyListensUntilStopped(t *testing.T) {
	var log bytes.Buffer
	proxy, err := startJobProxy(t.Context(), shimConfig{Sandbox: proxyTestSandbox(t.TempDir(), "gateway.example:443")}, &log)
	if err != nil {
		t.Fatalf("startJobProxy() = %v, want nil", err)
	}
	binds := proxy.binds()
	checkJobProxyBinds(t, binds)
	if err = proxy.stop(); err != nil {
		t.Fatalf("stop() = %v, want nil", err)
	}
	if _, err = os.Stat(filepath.Dir(binds.Socket)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(socket directory after stop) = %v, want it gone", err)
	}
	if log.Len() != 0 {
		t.Errorf("job log = %q, want nothing from a proxy nobody asked", log.String())
	}
}

func checkJobProxyBinds(t *testing.T, binds sandboxProxy) {
	t.Helper()
	info, err := os.Stat(binds.Socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Errorf("Stat(%s) = %v, %v, want a socket", binds.Socket, info, err)
	}
	own, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() = %v, want nil", err)
	}
	if own, err = filepath.EvalSymlinks(own); err != nil || binds.Relay != own {
		t.Errorf("relay = %q, want this program %q (%v)", binds.Relay, own, err)
	}
	if err = validateSandboxProxy(config.SandboxConfig{Network: "proxy"}, binds); err != nil {
		t.Errorf("validateSandboxProxy(binds of a started proxy) = %v, want nil", err)
	}
}

func TestStartJobProxyRefusesDestinationsItCannotServe(t *testing.T) {
	tests := []struct {
		name         string
		destinations []string
		want         string
	}{
		{"none", nil, "want 1..16 destinations, got 0"},
		{"seventeen", proxyTestDestinations(17), "want 1..16 destinations, got 17"},
		{"a wildcard", []string{"*.example:443"}, "host name label"},
		{"no port", []string{"gateway.example"}, "want host:port"},
	}
	for i := 0; i < len(tests); i++ {
		sandbox := proxyTestSandbox(t.TempDir(), tests[i].destinations...)
		proxy, err := startJobProxy(t.Context(), shimConfig{Sandbox: sandbox}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), tests[i].want) || proxy.server != nil {
			t.Errorf("startJobProxy(%s) = %v, want an error with %q and no proxy", tests[i].name, err, tests[i].want)
		}
	}
	proxy, err := startJobProxy(t.Context(), shimConfig{Sandbox: proxyTestSandbox(t.TempDir(), proxyTestDestinations(16)...)}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("startJobProxy(sixteen) = %v, want nil", err)
	}
	if err = proxy.stop(); err != nil {
		t.Errorf("stop(sixteen) = %v, want nil", err)
	}
}

// askProxy sends one CONNECT request to the socket of a job's proxy and returns the status
// line of the answer.
func askProxy(t *testing.T, socket string, target string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		t.Fatalf("dial %s = %v, want nil", socket, err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			t.Errorf("Close(proxy connection) = %v, want nil", closeErr)
		}
	}()
	if err = conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline() = %v, want nil", err)
	}
	if _, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("write the request for %s = %v, want nil", target, err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read the answer for %s = %v, want a status line", target, err)
	}
	return strings.TrimRight(line, "\r\n")
}

func TestStopWritesTheNumberOfRefusedRequests(t *testing.T) {
	var log bytes.Buffer
	proxy, err := startJobProxy(t.Context(), shimConfig{Sandbox: proxyTestSandbox(t.TempDir(), "gateway.example:443")}, &log)
	if err != nil {
		t.Fatalf("startJobProxy() = %v, want nil", err)
	}
	targets := []string{"other.example:443", "gateway.example:8443"}
	for i := 0; i < len(targets); i++ {
		if got := askProxy(t, proxy.binds().Socket, targets[i]); got != "HTTP/1.1 403 Forbidden" {
			t.Errorf("CONNECT %s = %q, want 403", targets[i], got)
		}
	}
	if err = proxy.stop(); err != nil {
		t.Fatalf("stop() = %v, want nil", err)
	}
	wanted := []string{
		`egress proxy: refused "other.example:443": other.example:443 is not a declared destination` + "\n",
		`egress proxy: refused "gateway.example:8443": gateway.example:8443 is not a declared destination` + "\n",
	}
	for i := 0; i < len(wanted); i++ {
		if !strings.Contains(log.String(), wanted[i]) {
			t.Errorf("job log = %q, want %q", log.String(), wanted[i])
		}
	}
	if !strings.HasSuffix(log.String(), "epitrop: egress proxy: 2 requests refused\n") {
		t.Errorf("job log = %q, want the number of refused requests as its last line", log.String())
	}
}

func TestStopReportsAJobLogThatCannotBeWritten(t *testing.T) {
	proxy, err := startJobProxy(t.Context(), shimConfig{Sandbox: proxyTestSandbox(t.TempDir(), "gateway.example:443")}, failingWriter{})
	if err != nil {
		t.Fatalf("startJobProxy() = %v, want nil", err)
	}
	if got := askProxy(t, proxy.binds().Socket, "other.example:443"); got != "HTTP/1.1 403 Forbidden" {
		t.Errorf("CONNECT other.example:443 = %q, want 403", got)
	}
	err = proxy.stop()
	if err == nil || !strings.Contains(err.Error(), "egress proxy: write the job log: log is gone") {
		t.Fatalf("stop() = %v, want the write error of the job log", err)
	}
	var log bytes.Buffer
	stopProxyForShim(jobProxy{}, &log)
	if log.Len() != 0 {
		t.Errorf("stopProxyForShim(no proxy) wrote %q, want nothing", log.String())
	}
}

// Two things go wrong when the job log of a refused request cannot be written: the proxy's
// own line and the count at the end. The shim writes both on its one line.
func TestShimWritesWhatStoppingTheProxyReportsOnOneLine(t *testing.T) {
	proxy, err := startJobProxy(t.Context(), shimConfig{Sandbox: proxyTestSandbox(t.TempDir(), "gateway.example:443")}, failingWriter{})
	if err != nil {
		t.Fatalf("startJobProxy() = %v, want nil", err)
	}
	if got := askProxy(t, proxy.binds().Socket, "other.example:443"); got != "HTTP/1.1 403 Forbidden" {
		t.Errorf("CONNECT other.example:443 = %q, want 403", got)
	}
	var log bytes.Buffer
	stopProxyForShim(proxy, &log)
	got := log.String()
	if !strings.HasPrefix(got, "epitrop: stop the egress proxy: ") || strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("job log = %q, want one line of the shim", got)
	}
	wanted := []string{"egress proxy: write the log: log is gone", "; egress proxy: write the job log: log is gone"}
	for i := 0; i < len(wanted); i++ {
		if !strings.Contains(got, wanted[i]) {
			t.Errorf("job log = %q, want %q in it", got, wanted[i])
		}
	}
}

// The whole chain as a job runs it: shim, proxy outside, scope, bwrap, relay inside, job.
func TestSandboxedJobReachesOnlyItsDeclaredDestination(t *testing.T) {
	requireSystemdUserScope(t)
	host, port, closeListener := rt03Listener(t, "tcp_connect_loopback")
	defer closeListener()
	declared := net.JoinHostPort(host, port)
	script := "ask() (\n" + rt03ProxyConnectScript + "\n)\n" +
		`TARGET=$DECLARED ask > declared.txt; printf '%s\n' "$?" >> declared.txt; ` +
		`TARGET=$UNDECLARED ask > undeclared.txt; printf '%s\n' "$?" >> undeclared.txt; ` +
		`tr '\0' '\n' < /proc/$PPID/environ | grep '^GOMAXPROCS=' > procs.txt; printenv GOMAXPROCS >> procs.txt || echo unset >> procs.txt; ` +
		`mv undeclared.txt done.txt; ` + untilStopped("", time.Second)
	job := sandboxedSupervisorJob(t, "sandbox-proxy", script)
	job.Sandbox.Network = "proxy"
	job.Sandbox.Destinations = []string{declared}
	job.Sandbox.EnvAllow = []string{"DECLARED", "UNDECLARED"}
	t.Setenv("DECLARED", declared)
	t.Setenv("UNDECLARED", "192.0.2.7:443")
	requireSandboxToolchain(t, job.Sandbox)
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	done := filepath.Join(job.Sandbox.Workspace, "done.txt")
	finished := waitUntil(func() bool { return fileSize(done) != 0 })
	if got := statusOne(t, sup, job.Name).Sandbox; got != "enforce/proxy" {
		t.Errorf("Sandbox = %q, want enforce/proxy", got)
	}
	if err := sup.Stop(testContext(t), job.Name); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	logged := waitUntil(func() bool {
		return jobLogHasLines(job.LogPath,
			`egress proxy: refused "192.0.2.7:443": 192.0.2.7:443 is not a declared destination`,
			"epitrop: egress proxy: 1 requests refused")
	})
	body, err := os.ReadFile(job.LogPath)
	if err != nil {
		t.Fatalf("ReadFile(job log) = %v, want nil", err)
	}
	if !finished || !logged {
		t.Fatalf("job finished its requests: %v, log has the refusal: %v, log = %q", finished, logged, body)
	}
	if !fileHas(t, filepath.Join(job.Sandbox.Workspace, "declared.txt"), "HTTP/1.1 200 Connection established\r\n0\n") {
		t.Errorf("declared destination was not reached, log = %q", body)
	}
	if !fileHas(t, done, "HTTP/1.1 403 Forbidden\r\n1\n") {
		t.Errorf("undeclared destination was not refused with 403, log = %q", body)
	}
	// The job's parent is the relay. It has its number of processors, the job has none.
	if !fileHas(t, filepath.Join(job.Sandbox.Workspace, "procs.txt"), "GOMAXPROCS=2\nunset\n") {
		t.Errorf("the relay does not start with GOMAXPROCS=2, or the job inherits it, log = %q", body)
	}
}

func TestJobLogHasLines(t *testing.T) {
	refused := `egress proxy: refused "192.0.2.7:443": 192.0.2.7:443 is not a declared destination`
	summary := "epitrop: egress proxy: 1 requests refused"
	cases := []struct {
		name string
		log  string
		want bool
	}{
		{"adjacent", refused + "\n" + summary + "\n", true},
		{"coverage warning between", refused + "\nwarning: GOCOVERDIR not set, no coverage data emitted\n" + summary + "\n", true},
		{"out of order", summary + "\n" + refused + "\n", false},
		{"summary missing", refused + "\n", false},
		{"refusal missing", summary + "\n", false},
		{"last line not ended", refused + "\n" + summary, false},
		{"line with a prefix", refused + "\nx " + summary + "\n", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		path := filepath.Join(t.TempDir(), "job.log")
		if err := os.WriteFile(path, []byte(tc.log), 0o600); err != nil {
			t.Fatalf("WriteFile() = %v", err)
		}
		if got := jobLogHasLines(path, refused, summary); got != tc.want {
			t.Errorf("%s: jobLogHasLines() = %v, want %v", tc.name, got, tc.want)
		}
	}
	if jobLogHasLines(filepath.Join(t.TempDir(), "missing.log"), refused) {
		t.Error("missing log: jobLogHasLines() = true, want false")
	}
}

// jobLogHasLines reports whether the job log holds each wanted line, whole, ended by a newline
// and in order. Other lines may come between them: a coverage build of the helper process adds
// its own warning line to the log.
func jobLogHasLines(path string, want ...string) bool {
	body, err := os.ReadFile(path) // #nosec G304 -- test-controlled path.
	if err != nil {
		return false
	}
	lines := strings.Split(string(body), "\n")
	lines = lines[:len(lines)-1] // the text after the last newline is not a whole line yet
	next := 0
	for i := 0; i < len(lines) && next < len(want); i++ {
		if lines[i] == want[next] {
			next++
		}
	}
	return next == len(want)
}

func TestRequireReadable(t *testing.T) {
	t.Run("readable file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "readable.txt")
		if err := os.WriteFile(f, []byte("data"), 0o600); err != nil {
			t.Fatalf("WriteFile() = %v", err)
		}
		if err := requireReadable(f); err != nil {
			t.Fatalf("requireReadable(%s) = %v, want nil", f, err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "missing.txt")
		err := requireReadable(f)
		if err == nil || !strings.Contains(err.Error(), "readable:") {
			t.Fatalf("requireReadable(%s) = %v, want error with 'readable:'", f, err)
		}
	})

	t.Run("unreadable file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("skipping unreadable file test under root (euid 0)")
		}
		f := filepath.Join(t.TempDir(), "unreadable.txt")
		if err := os.WriteFile(f, []byte("secret"), 0o000); err != nil {
			t.Fatalf("WriteFile() = %v", err)
		}
		err := requireReadable(f)
		if err == nil || !strings.Contains(err.Error(), "readable:") {
			t.Fatalf("requireReadable(%s) = %v, want error with 'readable:'", f, err)
		}
	})
}

func TestSandboxedJobEgressLogHasNoNameserverWarning(t *testing.T) {
	requireSystemdUserScope(t)
	job := sandboxedSupervisorJob(t, "sandbox-egress-dns", untilStopped("", time.Second))
	job.Sandbox.Network = "egress"
	requireSandboxToolchain(t, job.Sandbox)
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	if got := statusOne(t, sup, job.Name).Sandbox; got != "enforce/egress" {
		t.Errorf("Sandbox = %q, want enforce/egress", got)
	}
	if err := sup.Stop(testContext(t), job.Name); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	body, err := os.ReadFile(job.LogPath)
	if err != nil {
		t.Fatalf("ReadFile(job log) = %v, want nil", err)
	}
	const warning = "Couldn't get any nameserver address"
	if strings.Contains(string(body), warning) {
		t.Fatalf("job log contains %q: %s", warning, body)
	}
}
