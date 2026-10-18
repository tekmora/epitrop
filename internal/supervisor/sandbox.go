package supervisor

import (
	"fmt"
	"strconv"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

type sandboxPaths struct {
	EventLogDir string
	SigningKey  string
}

type sandboxTools struct {
	SystemdRun string
	Bwrap      string
	Pasta      string
	Env        string
	Systemd    bool
}

// sandboxProxy is what a sandbox with network=proxy binds: the socket of the job's proxy,
// which runs outside the sandbox, and the program that runs as the relay inside it.
type sandboxProxy struct {
	Socket string
	Relay  string
}

type sandboxBuildOptions struct {
	Paths sandboxPaths
	Tools sandboxTools
	Env   func(string) (string, bool)
	Proxy sandboxProxy
	// Until is the latest time the job can still run, stop grace included; a home's bound
	// token has to outlast it.
	Until time.Time
}

func sandboxStatus(sandbox config.SandboxConfig) string {
	sandbox = config.NormalizeSandbox(sandbox)
	if sandbox.Mode == "off" {
		return "off: " + sandbox.Reason
	}
	return "enforce/" + sandbox.Network
}

func sandboxFlagArgs(sandbox config.SandboxConfig) []string {
	sandbox = config.NormalizeSandbox(sandbox)
	args := []string{
		"--sandbox-mode", sandbox.Mode,
		"--sandbox-reason", sandbox.Reason,
		"--sandbox-workspace", sandbox.Workspace,
		"--sandbox-network", sandbox.Network,
		"--sandbox-memory-max", sandbox.MemoryMax,
		"--sandbox-memory-high", sandbox.MemoryHigh,
		"--sandbox-cpu-weight", strconv.Itoa(sandbox.CPUWeight),
		"--sandbox-tasks-max", strconv.Itoa(sandbox.TasksMax),
	}
	for i := 0; i < len(sandbox.Inputs); i++ {
		args = append(args, "--sandbox-input", sandbox.Inputs[i])
	}
	for i := 0; i < len(sandbox.ReadOnly); i++ {
		args = append(args, "--sandbox-read-only", sandbox.ReadOnly[i])
	}
	for i := 0; i < len(sandbox.EnvAllow); i++ {
		args = append(args, "--sandbox-env-allow", sandbox.EnvAllow[i])
	}
	for i := 0; i < len(sandbox.Destinations); i++ {
		args = append(args, "--sandbox-destination", sandbox.Destinations[i])
	}
	return append(args, homeFlagArgs(sandbox.Home)...)
}

// jobCommand is the argv and environment the shim execs. Env nil means the shim's own
// environment is inherited (mode=off). A sandboxed job gets an explicit, minimal Env so
// that no secret travels on a command line, where /proc/<pid>/cmdline would show it, and
// nothing outside the allowlist reaches the job. Scope is the systemd scope unit the job
// must be found in before its start is recorded; empty means the job runs in none.
type jobCommand struct {
	Argv  []string
	Env   []string
	Scope string
}

func buildJobCommand(command []string, sandbox config.SandboxConfig, opts sandboxBuildOptions) (jobCommand, error) {
	if sandbox.Mode == "off" {
		return jobCommand{Argv: append([]string(nil), command...)}, nil
	}
	if len(command) == 0 {
		return jobCommand{}, fmt.Errorf("sandbox: command is required")
	}
	return buildSandboxCommand(command, sandbox, opts)
}
