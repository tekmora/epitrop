package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tekmora/epitrop/internal/config"
)

const (
	// sandboxHostsContent is the fixed /etc/hosts content for every sandbox: localhost
	// only, never the host's /etc/hosts, so a job can resolve localhost without learning
	// the host's names.
	sandboxHostsContent = "127.0.0.1 localhost\n::1 localhost\n"
)

// jobHosts is the private per-job hosts file bound read-only into the sandbox.
// The shim creates it in a private directory owned by the shim outside the workspace
// and removes the directory when the job ends.
type jobHosts struct {
	dir  string
	path string
}

func (h jobHosts) stop() error {
	if h.dir == "" {
		return nil
	}
	return os.RemoveAll(h.dir)
}

// startJobHosts writes the fixed hosts file for a sandboxed job into a private directory
// the shim owns, outside the workspace, and returns the zero value for every other job.
// An error means the job must not start.
func startJobHosts(sandbox config.SandboxConfig) (jobHosts, error) {
	sandbox = config.NormalizeSandbox(sandbox)
	if sandbox.Mode != "enforce" {
		return jobHosts{}, nil
	}
	dir, err := os.MkdirTemp("", "epitrop-hosts-")
	if err != nil {
		return jobHosts{}, fmt.Errorf("job-shim: hosts directory: %w", err)
	}
	path := filepath.Join(dir, "hosts")
	// #nosec G304 -- the path lies in the freshly created private hosts directory.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return jobHosts{}, errors.Join(fmt.Errorf("job-shim: create hosts file %s: %w", path, err), os.RemoveAll(dir))
	}
	_, err = f.WriteString(sandboxHostsContent)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return jobHosts{}, errors.Join(fmt.Errorf("job-shim: write hosts file %s: %w", path, err), os.RemoveAll(dir))
	}
	// #nosec G302 -- hosts file in sandbox must be readable by all inside the container.
	if err = os.Chmod(path, 0o444); err != nil {
		return jobHosts{}, errors.Join(fmt.Errorf("job-shim: chmod hosts file %s: %w", path, err), os.RemoveAll(dir))
	}
	return jobHosts{dir: dir, path: path}, nil
}

// stopHostsForShim removes the job's private hosts directory. What went wrong goes to the
// job's log, which is kept; the shim's stderr is not.
func stopHostsForShim(hosts jobHosts, logFile io.Writer) {
	err := hosts.stop()
	if err == nil {
		return
	}
	if _, writeErr := fmt.Fprintf(logFile, "epitrop: remove the hosts directory: %v\n", err); writeErr != nil {
		fmt.Fprintf(os.Stderr, "Error: remove the hosts directory: %v; write the job log: %v\n", err, writeErr)
	}
}
