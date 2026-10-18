//go:build linux

package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/egressproxy"
)

const (
	legacyTIOCSTIPath = "/proc/sys/dev/tty/legacy_tiocsti"
	resolvedConfPath  = "/run/systemd/resolve/resolv.conf"

	// The egress namespace gets its own address from TEST-NET-1 (RFC 5737), which is never
	// routed on a real network, so it cannot shadow a real destination. With pasta's
	// default of copying the host's addresses, the host's own LAN address would point at
	// the namespace itself.
	sandboxNetAddress = "192.0.2.2"
	sandboxNetMask    = "24"
	sandboxNetGateway = "192.0.2.1"

	// With network=proxy the sandbox has no route out. The job's programs find a proxy on
	// the sandbox's own loopback: the relay, which leads to the socket of the job's proxy
	// outside. Both are bound under a directory nothing else uses.
	sandboxProxySocket = "/run/epitrop/proxy.sock"
	sandboxRelayPath   = "/run/epitrop/relay"
	sandboxProxyListen = "127.0.0.1:3128"
	sandboxProxyURL    = "http://" + sandboxProxyListen
	// relayProcsEnv tells the Go runtime of the relay its number of processors before it
	// starts a thread. The job's command starts without it.
	relayProcsEnv = "GOMAXPROCS"
)

// scopeNoExpand keeps the job's arguments as its config wrote them. With --scope, systemd-run
// rewrites the command from its own environment (by default from systemd 258 on): ${NAME}
// anywhere and an argument that is $NAME become the value, an unset $NAME argument is
// dropped, and $$ becomes $. That environment carries the job's env_allow values, so a value
// would land on the argv.
const scopeNoExpand = "--expand-environment=no"

// proxyEnvNames are the variables programs read their proxy from, in both spellings.
var proxyEnvNames = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"}

// busEnvNames are what systemd-run --user needs to reach the user manager. The shim passes
// them to systemd-run, and bwrap unsets them, with INVOCATION_ID that systemd adds for the
// scope, before the job starts.
var busEnvNames = []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"}

var (
	sandboxLookPath = exec.LookPath
	sandboxReadFile = os.ReadFile
)

func buildSandboxCommand(command []string, sandbox config.SandboxConfig, opts sandboxBuildOptions) (jobCommand, error) {
	tools, err := sandboxToolchain(sandbox, opts.Tools)
	if err != nil {
		return jobCommand{}, err
	}
	resolvedCommand, err := resolveExecutable(command[0])
	if err != nil {
		return jobCommand{}, err
	}
	if err = validateSandboxRuntimePaths(sandbox, opts.Paths); err != nil {
		return jobCommand{}, err
	}
	if err = validateSandboxProxy(sandbox, opts.Proxy); err != nil {
		return jobCommand{}, err
	}
	if err = prepareHome(sandbox, opts.Until); err != nil {
		return jobCommand{}, err
	}
	// Resolved again at every start: a path swapped for a symlink since the load must not
	// lead the read-only bind out of the workspace.
	readOnly, err := config.ResolveSandboxReadOnly(sandbox)
	if err != nil {
		return jobCommand{}, fmt.Errorf("sandbox: %w", err)
	}
	inner := bwrapArgs(resolvedCommand, command, sandbox, tools, opts.Proxy, readOnly)
	if sandbox.Network == "egress" {
		inner = append(pastaArgs(tools.Pasta), inner...)
	}
	// Stop sends SIGTERM to the whole process group, which includes bwrap and pasta. Both
	// would die on it, and bwrap's --die-with-parent would then SIGKILL the job before its
	// own TERM handling ran. They start with TERM ignored (inherited across exec), and the
	// job gets TERM back at its default just before it starts. SIGKILL, used for escalation
	// and on shim death, cannot be ignored.
	inner = append([]string{tools.Env, "--ignore-signal=TERM"}, inner...)
	env := sandboxEnv(sandbox, opts.Env)
	if !tools.Systemd {
		return jobCommand{Argv: inner, Env: env}, nil
	}
	scope, err := newScopeName()
	if err != nil {
		return jobCommand{}, err
	}
	return jobCommand{Argv: append(systemdRunArgs(tools.SystemdRun, scope, sandbox), inner...), Env: env, Scope: scope}, nil
}

// sandboxPreflight looks for the tools a sandboxed start needs, as the shim will.
func sandboxPreflight(sandbox config.SandboxConfig) error {
	_, err := sandboxToolchain(sandbox, sandboxTools{})
	return err
}

func sandboxToolchain(sandbox config.SandboxConfig, tools sandboxTools) (sandboxTools, error) {
	if tools.Bwrap != "" {
		return tools, nil
	}
	var err error
	tools.Systemd = true
	if tools.SystemdRun, err = resolveScopeRunner(); err != nil {
		return sandboxTools{}, err
	}
	tools.Bwrap, err = resolveExecutable("bwrap")
	if err != nil {
		return sandboxTools{}, fmt.Errorf("sandbox: bwrap: %w", err)
	}
	if tools.Env, err = resolveSignalEnv(); err != nil {
		return sandboxTools{}, err
	}
	if sandbox.Network == "egress" {
		tools.Pasta, err = resolvePasta()
		if err != nil {
			return sandboxTools{}, err
		}
		if err = requireReadable(resolvedConfPath); err != nil {
			return sandboxTools{}, err
		}
	}
	if err = requireTIOCSTIDisabled(); err != nil {
		return sandboxTools{}, err
	}
	return tools, nil
}

// resolveSignalEnv finds an env that supports --ignore-signal and --default-signal (GNU
// coreutils 8.31 or later) under /usr, the one tree every sandbox binds, so the same binary
// runs outside and inside bwrap.
func resolveSignalEnv() (string, error) {
	path, err := resolveExecutable("env")
	if err != nil {
		return "", fmt.Errorf("sandbox: env: %w", err)
	}
	if !strings.HasPrefix(path, "/usr/") {
		return "", fmt.Errorf("sandbox: env %s is outside /usr, so the sandbox cannot run it", path)
	}
	if out, err := probeTool(path, "--ignore-signal=TERM", "--default-signal=TERM", "true"); err != nil {
		return "", fmt.Errorf("sandbox: env %s lacks --ignore-signal/--default-signal: %w: %s", path, err, out)
	}
	return path, nil
}

// resolveScopeRunner finds a systemd-run that takes --expand-environment (systemd 254 or
// later). An older one would stop every job start at that switch, so the preflight names
// the missing piece first. The probe starts no unit: it parses the switch and prints the
// version.
func resolveScopeRunner() (string, error) {
	path, err := resolveExecutable("systemd-run")
	if err != nil {
		return "", fmt.Errorf("sandbox: systemd-run: %w", err)
	}
	if out, err := probeTool(path, scopeNoExpand, "--version"); err != nil {
		return "", fmt.Errorf("sandbox: systemd-run %s lacks --expand-environment (systemd 254 or later): %w: %s", path, err, out)
	}
	return path, nil
}

// probeTool runs a resolved tool with fixed arguments that change nothing, and returns what
// it printed.
func probeTool(path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput() // #nosec G204 -- fixed probe arguments of a resolved tool.
	return strings.TrimSpace(string(out)), err
}

// resolvePasta finds pasta by its own name. pasta and passt are one binary that picks its
// mode from the name it was started under, and distributions ship pasta as a symlink to
// passt; started under the resolved name it would run as passt and refuse --config-net.
func resolvePasta() (string, error) {
	path, err := sandboxLookPath("pasta")
	if err != nil {
		return "", fmt.Errorf("sandbox: pasta: %w", err)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("sandbox: pasta: %s is not absolute", path)
	}
	if _, err = filepath.EvalSymlinks(path); err != nil {
		return "", fmt.Errorf("sandbox: pasta: eval symlinks %s: %w", path, err)
	}
	return path, nil
}

func resolveExecutable(name string) (string, error) {
	path, err := sandboxLookPath(name)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("eval symlinks %s: %w", path, err)
	}
	return resolved, nil
}

func validateSandboxRuntimePaths(sandbox config.SandboxConfig, paths sandboxPaths) error {
	if !filepath.IsAbs(sandbox.Workspace) {
		return fmt.Errorf("sandbox: workspace %s is not absolute", sandbox.Workspace)
	}
	if err := requireDirectory(sandbox.Workspace); err != nil {
		return err
	}
	if err := requireUnchangedResolution("workspace", sandbox.Workspace); err != nil {
		return err
	}
	protected := []string{paths.EventLogDir, paths.SigningKey}
	for i := 0; i < len(protected); i++ {
		if protected[i] != "" && pathOverlapsSandbox(sandbox.Workspace, protected[i]) {
			return fmt.Errorf("sandbox: workspace overlaps protected path %s", protected[i])
		}
	}
	if sandbox.Home == nil {
		return validateSandboxInputs(sandbox.Inputs, protected)
	}
	if pathOverlapsSandbox(sandbox.Workspace, sandbox.Home.Source) {
		return fmt.Errorf("sandbox: home source %s overlaps the workspace", sandbox.Home.Source)
	}
	return validateSandboxInputs(append(slices.Clone(sandbox.Inputs), sandbox.Home.Source), protected)
}

func validateSandboxInputs(inputs []string, protected []string) error {
	for i := 0; i < len(inputs); i++ {
		if !filepath.IsAbs(inputs[i]) {
			return fmt.Errorf("sandbox: input %s is not absolute", inputs[i])
		}
		if err := requireExists(inputs[i]); err != nil {
			return err
		}
		if err := requireUnchangedResolution("input", inputs[i]); err != nil {
			return err
		}
		for j := 0; j < len(protected); j++ {
			if protected[j] != "" && pathOverlapsSandbox(inputs[i], protected[j]) {
				return fmt.Errorf("sandbox: input %s overlaps protected path %s", inputs[i], protected[j])
			}
		}
	}
	return nil
}

// requireUnchangedResolution refuses a path that no longer resolves to itself. Config load
// stores workspace and inputs fully resolved, so a component swapped for a symlink since
// then would make bwrap bind wherever the link points.
func requireUnchangedResolution(field string, path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("sandbox: resolve %s %s: %w", field, path, err)
	}
	if resolved != filepath.Clean(path) {
		return fmt.Errorf("sandbox: %s %s now resolves to %s; refusing a path changed since config load", field, path, resolved)
	}
	return nil
}

// validateSandboxProxy refuses network=proxy without a proxy to bind: such a sandbox would
// start with no way out and the job would fail later, with nothing that says why. It also
// refuses a proxy for any other mode, which would open a way out nobody declared.
func validateSandboxProxy(sandbox config.SandboxConfig, proxy sandboxProxy) error {
	if sandbox.Network != "proxy" {
		if proxy != (sandboxProxy{}) {
			return fmt.Errorf("sandbox: network=%s takes no proxy", sandbox.Network)
		}
		return nil
	}
	if !filepath.IsAbs(proxy.Socket) || !filepath.IsAbs(proxy.Relay) {
		return fmt.Errorf("sandbox: network=proxy needs the proxy socket and the relay program as absolute paths")
	}
	if err := requireExists(proxy.Socket); err != nil {
		return err
	}
	return requireExists(proxy.Relay)
}

func requireTIOCSTIDisabled() error {
	body, err := sandboxReadFile(legacyTIOCSTIPath)
	if err != nil {
		return fmt.Errorf("sandbox: read %s: %w", legacyTIOCSTIPath, err)
	}
	if strings.TrimSpace(string(body)) != "0" {
		return fmt.Errorf("sandbox: %s must be 0 before starting a job", legacyTIOCSTIPath)
	}
	return nil
}

func requireReadable(path string) error {
	f, err := os.Open(path) // #nosec G304 -- fixed platform preflight path.
	if err != nil {
		return fmt.Errorf("sandbox: %s readable: %w", path, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("sandbox: close %s: %w", path, err)
	}
	return nil
}

func requireExists(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("sandbox: stat %s: %w", path, err)
	}
	return nil
}

func requireDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("sandbox: stat workspace %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("sandbox: workspace %s is not a directory", path)
	}
	return nil
}

func pathOverlapsSandbox(a string, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || strings.HasPrefix(a, b+string(os.PathSeparator)) || strings.HasPrefix(b, a+string(os.PathSeparator))
}

func systemdRunArgs(path string, scope string, sandbox config.SandboxConfig) []string {
	args := []string{
		path, "--user", "--scope", "--quiet", "--collect", scopeNoExpand, "--unit=" + scope,
		"-p", "MemoryMax=" + sandbox.MemoryMax,
	}
	// The soft limit is optional: without it systemd leaves MemoryHigh at "infinity".
	if sandbox.MemoryHigh != "" {
		args = append(args, "-p", "MemoryHigh="+sandbox.MemoryHigh)
	}
	return append(args,
		"-p", "CPUWeight="+strconv.Itoa(sandbox.CPUWeight),
		"-p", "TasksMax="+strconv.Itoa(sandbox.TasksMax),
		"--",
	)
}

// pastaArgs gives the job egress without the host's loopback: every port forward off
// (pasta's default "auto" forwards host-listening ports into the namespace), no gateway
// mapping to host loopback, and an address of its own.
func pastaArgs(path string) []string {
	return []string{
		path, "--config-net",
		"--address", sandboxNetAddress, "--netmask", sandboxNetMask, "--gateway", sandboxNetGateway,
		"--map-host-loopback", "none",
		"-t", "none", "-u", "none", "-T", "none", "-U", "none",
		"-D", "none",
		"--quiet", "--",
	}
}

func bwrapArgs(commandPath string, command []string, sandbox config.SandboxConfig, tools sandboxTools, proxy sandboxProxy, readOnly []string) []string {
	args := bwrapBaseArgs(tools.Bwrap, sandbox)
	args = append(args, "--bind", sandbox.Workspace, sandbox.Workspace)
	args = appendReadOnlyBinds(args, sandbox.Workspace, readOnly)
	args = appendInputBinds(args, sandbox.Inputs)
	args = appendHomeArgs(args, sandbox)
	args = append(args, "--ro-bind", commandPath, commandPath, "--chdir", sandbox.Workspace)
	args = append(args, "--setenv", "PATH", "/usr/bin:/bin", "--setenv", "HOME", jobHome(sandbox), "--setenv", "TMPDIR", "/tmp")
	for i := 0; i < len(busEnvNames); i++ {
		args = append(args, "--unsetenv", busEnvNames[i])
	}
	args = append(args, "--unsetenv", "INVOCATION_ID")
	start := []string{tools.Env, "--default-signal=TERM"}
	if sandbox.Network == "proxy" {
		args = appendProxyArgs(args, proxy)
		// The relay's number of processors is its own: the command starts without it.
		start = append(relayArgs(), append(start, "--unset="+relayProcsEnv)...)
	}
	args = append(append(args, "--"), start...)
	return append(append(args, commandPath), command[1:]...)
}

// appendProxyArgs binds the proxy's socket and the relay program, points the job's
// programs at the relay, and gives the relay its number of processors.
func appendProxyArgs(args []string, proxy sandboxProxy) []string {
	args = append(args, "--ro-bind", proxy.Socket, sandboxProxySocket, "--ro-bind", proxy.Relay, sandboxRelayPath)
	for i := 0; i < len(proxyEnvNames); i++ {
		args = append(args, "--setenv", proxyEnvNames[i], sandboxProxyURL)
	}
	return append(args, "--setenv", relayProcsEnv, strconv.Itoa(egressproxy.RelayProcs))
}

// relayArgs start the relay, which listens and then runs the job's command as its child.
// The relay ignores TERM like the rest of the chain; the command still gets it back.
func relayArgs() []string {
	return []string{
		sandboxRelayPath, egressproxy.RelayCommand,
		"--socket", sandboxProxySocket, "--listen", sandboxProxyListen, "--",
	}
}

func bwrapBaseArgs(path string, sandbox config.SandboxConfig) []string {
	args := []string{
		path, "--die-with-parent", "--unshare-all",
	}
	if sandbox.Network == "egress" {
		args = append(args, "--share-net")
	}
	args = append(args,
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin",
		"--symlink", "usr/lib", "/lib",
		"--symlink", "usr/lib64", "/lib64",
		"--ro-bind", "/etc/ssl", "/etc/ssl",
		"--ro-bind", "/etc/ca-certificates", "/etc/ca-certificates",
	)
	if sandbox.Network == "egress" {
		args = append(args, "--ro-bind", resolvedConfPath, "/etc/resolv.conf")
	}
	return append(args, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--tmpfs", "/var/tmp")
}

// appendReadOnlyBinds binds each read-only path, resolved and below the workspace, over the
// writable workspace. Each directory between the workspace and such a path is first bound
// onto itself, writable as before: a mount point cannot be renamed, so the job cannot move a
// read-only path aside and make a writable one of the same name.
func appendReadOnlyBinds(args []string, workspace string, readOnly []string) []string {
	ancestors := readOnlyAncestors(workspace, readOnly)
	for i := 0; i < len(ancestors); i++ {
		args = append(args, "--bind", ancestors[i], ancestors[i])
	}
	for i := 0; i < len(readOnly); i++ {
		args = append(args, "--ro-bind", readOnly[i], readOnly[i])
	}
	return args
}

// readOnlyAncestors returns the directories strictly between the workspace and each
// read-only path, each once, parents before children.
func readOnlyAncestors(workspace string, readOnly []string) []string {
	var out []string
	seen := map[string]bool{}
	prefix := filepath.Clean(workspace) + string(os.PathSeparator)
	for i := 0; i < len(readOnly); i++ {
		rel := strings.TrimPrefix(filepath.Dir(readOnly[i]), prefix)
		if rel == filepath.Dir(readOnly[i]) {
			// The path lies directly in the workspace, or outside it, which the resolver refuses.
			continue
		}
		parts := strings.Split(rel, string(os.PathSeparator))
		dir := filepath.Clean(workspace)
		for k := 0; k < len(parts); k++ {
			dir = filepath.Join(dir, parts[k])
			if !seen[dir] {
				seen[dir] = true
				out = append(out, dir)
			}
		}
	}
	return out
}

func appendInputBinds(args []string, inputs []string) []string {
	for i := 0; i < len(inputs); i++ {
		args = append(args, "--ro-bind", inputs[i], inputs[i])
	}
	return args
}

// sandboxEnv is the whole environment of the sandbox chain: the fixed PATH, HOME and TMPDIR,
// the allowlisted variables that are set, and the bus variables systemd-run needs (bwrap
// unsets those before the job starts). Values travel here, never on argv.
func sandboxEnv(sandbox config.SandboxConfig, lookup func(string) (string, bool)) []string {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + sandbox.Workspace, "TMPDIR=/tmp"}
	for i := 0; i < len(sandbox.EnvAllow); i++ {
		if value, ok := lookup(sandbox.EnvAllow[i]); ok {
			env = append(env, sandbox.EnvAllow[i]+"="+value)
		}
	}
	for i := 0; i < len(busEnvNames); i++ {
		if value, ok := lookup(busEnvNames[i]); ok {
			env = append(env, busEnvNames[i]+"="+value)
		}
	}
	return env
}
