package config

import (
	"fmt"
	"path"
	"path/filepath"
)

// validateSandboxReadOnly checks the read-only paths' shape: at most MaxSandboxReadOnly,
// each relative, clean and below the workspace, and only where the sandbox enforces them.
func validateSandboxReadOnly(sandbox SandboxConfig) error {
	paths := sandbox.ReadOnly
	if len(paths) == 0 {
		return nil
	}
	if sandbox.Mode != "enforce" {
		return fmt.Errorf("read_only: forbidden unless mode=enforce")
	}
	if len(paths) > MaxSandboxReadOnly {
		return fmt.Errorf("read_only: exceeds %d", MaxSandboxReadOnly)
	}
	for i := 0; i < len(paths); i++ {
		// The configuration separates with a slash on every platform: filepath.Clean would
		// rewrite it on Windows and refuse every nested path.
		if !filepath.IsLocal(paths[i]) || path.Clean(paths[i]) != paths[i] || paths[i] == "." {
			return fmt.Errorf("read_only/%d: %q is no clean relative path below the workspace", i, paths[i])
		}
	}
	return nil
}

// ResolveSandboxReadOnly resolves each read-only path below the workspace, which must be
// resolved already, and returns the results in order. A path that is missing, leaves the
// workspace through a symlink, resolves to the workspace itself, overlaps another
// read-only path, or overlaps the home the shim makes is refused: the sandbox binds each
// one read-only over the writable workspace, and an overlap would undo that.
func ResolveSandboxReadOnly(sandbox SandboxConfig) ([]string, error) {
	if err := validateSandboxReadOnly(sandbox); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(sandbox.ReadOnly))
	for i := 0; i < len(sandbox.ReadOnly); i++ {
		resolved, err := filepath.EvalSymlinks(filepath.Join(sandbox.Workspace, sandbox.ReadOnly[i]))
		if err != nil {
			return nil, fmt.Errorf("read_only/%d: %w", i, err)
		}
		if err = checkReadOnlyPlace(sandbox, resolved, out); err != nil {
			return nil, fmt.Errorf("read_only/%d: %w", i, err)
		}
		out = append(out, resolved)
	}
	return out, nil
}

func checkReadOnlyPlace(sandbox SandboxConfig, resolved string, earlier []string) error {
	if samePath(resolved, sandbox.Workspace) || !pathContains(sandbox.Workspace, resolved) {
		return fmt.Errorf("%s is not below the workspace %s", resolved, sandbox.Workspace)
	}
	for j := 0; j < len(earlier); j++ {
		if pathOverlaps(resolved, earlier[j]) {
			return fmt.Errorf("%s overlaps the read-only path %s", resolved, earlier[j])
		}
	}
	if sandbox.Home != nil && pathOverlaps(resolved, filepath.Join(sandbox.Workspace, SandboxHomeTop)) {
		return fmt.Errorf("%s overlaps the home's directory %s", resolved, SandboxHomeTop)
	}
	return nil
}
