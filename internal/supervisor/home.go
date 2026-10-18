package supervisor

import (
	"fmt"
	"strings"

	"github.com/tekmora/epitrop/internal/config"
)

// homeFlagArgs are the shim flags that carry a home.
func homeFlagArgs(home *config.SandboxHome) []string {
	if home == nil {
		return nil
	}
	args := []string{"--sandbox-home-source", home.Source, "--sandbox-home-env", home.Env}
	for i := 0; i < len(home.Copy); i++ {
		args = append(args, "--sandbox-home-copy", home.Copy[i].File+"="+home.Copy[i].SHA256)
	}
	for i := 0; i < len(home.Bind); i++ {
		args = append(args, "--sandbox-home-bind", home.Bind[i].File+"="+home.Bind[i].JWT+"="+home.Bind[i].ExpiresMS)
	}
	return args
}

// homeFlags collects the shim's home flags.
type homeFlags struct {
	source string
	env    string
	copies []string
	binds  []string
}

// home builds the home the flags describe, nil when they name none.
func (f homeFlags) home() (*config.SandboxHome, error) {
	if f.source == "" && f.env == "" && len(f.copies) == 0 && len(f.binds) == 0 {
		return nil, nil
	}
	home := &config.SandboxHome{Source: f.source, Env: f.env}
	for i := 0; i < len(f.copies); i++ {
		file, sum, ok := strings.Cut(f.copies[i], "=")
		if !ok {
			return nil, fmt.Errorf("job-shim: --sandbox-home-copy %q is no file=sha256", f.copies[i])
		}
		home.Copy = append(home.Copy, config.SandboxHomeCopy{File: file, SHA256: sum})
	}
	for i := 0; i < len(f.binds); i++ {
		file, expiry, ok := strings.Cut(f.binds[i], "=")
		if !ok {
			return nil, fmt.Errorf("job-shim: --sandbox-home-bind %q is no file=jwt=expires_ms", f.binds[i])
		}
		jwt, expiresMS, _ := strings.Cut(expiry, "=")
		home.Bind = append(home.Bind, config.SandboxHomeBind{File: file, JWT: jwt, ExpiresMS: expiresMS})
	}
	return home, nil
}
