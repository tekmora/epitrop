package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
)

const (
	// SandboxHomeTop is the workspace's top directory a job's home lies in; no commit of a
	// run may touch it.
	SandboxHomeTop = ".slot"
	// SandboxHomeDir is where a job's home lies in its workspace. The shim makes it before
	// each start; nothing below it belongs to the job's work.
	SandboxHomeDir = SandboxHomeTop + "/home"
	// MaxSandboxHomeFiles bounds the files a home copies and the files it binds, each.
	MaxSandboxHomeFiles = 8
)

var (
	homeFileName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	homeSHA256   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	homeJWTPath  = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}(\.[A-Za-z0-9_]{1,64}){0,7}$`)
)

// homeCredentialFiles are the login files a bind may name although the plain name rule
// refuses them: each is the file a client keeps its login in, measured in the client
// itself, relative to the folder home.env points at, slash-separated. Nothing else may lead
// with a dot or name a file below a folder.
//
//   - .credentials.json: Claude Code 2.1.302 below CLAUDE_CONFIG_DIR; the token's expiry
//     is claudeAiOauth.expiresAt, in Unix milliseconds.
var homeCredentialFiles = []string{".credentials.json"}

// SandboxHome is a home a coding client keeps its settings and login in, made in the job's
// workspace at SandboxHomeDir before each start. Copy files come from Source and must hash to
// the value named here; Bind files are bound from Source read-only, one by one, so the job
// can read them and cannot change them. Env names the variable that points the client at
// the home.
type SandboxHome struct {
	Source string            `koanf:"source" json:"source"`
	Env    string            `koanf:"env" json:"env"`
	Copy   []SandboxHomeCopy `koanf:"copy" json:"copy"`
	Bind   []SandboxHomeBind `koanf:"bind" json:"bind"`
}

// SandboxHomeCopy is a file copied into the home when it hashes to SHA256.
type SandboxHomeCopy struct {
	File   string `koanf:"file" json:"file"`
	SHA256 string `koanf:"sha256" json:"sha256"`
}

// SandboxHomeBind is a file bound into the home read-only. It names its token's expiry in
// one of two ways, and the expiry must lie beyond the job's end: a client that found its
// token expiring would try to refresh it and write the file, which it cannot. JWT is the
// dotted path of a JSON Web Token in the file, whose exp claim is read; ExpiresMS is the
// dotted path of a number in the file, the expiry in Unix milliseconds.
type SandboxHomeBind struct {
	File      string `koanf:"file" json:"file"`
	JWT       string `koanf:"jwt" json:"jwt"`
	ExpiresMS string `koanf:"expires_ms" json:"expires_ms"`
}

// validateSandboxHome checks a home's shape: an absolute source, a variable the sandbox does
// not set otherwise, file names that are one path element each or a measured login file,
// each named once, and an expiry for each bound file.
func validateSandboxHome(sandbox SandboxConfig) error {
	home := sandbox.Home
	if home == nil {
		return nil
	}
	if sandbox.Mode != "enforce" {
		return fmt.Errorf("home: forbidden unless mode=enforce")
	}
	if !filepath.IsAbs(home.Source) {
		return fmt.Errorf("home.source: %q is not absolute", home.Source)
	}
	if err := validateHomeEnv(home.Env, sandbox.EnvAllow); err != nil {
		return err
	}
	if len(home.Copy) > MaxSandboxHomeFiles || len(home.Bind) > MaxSandboxHomeFiles {
		return fmt.Errorf("home: copy and bind hold at most %d files each", MaxSandboxHomeFiles)
	}
	if len(home.Bind) == 0 && len(home.Copy) == 0 {
		return fmt.Errorf("home: names no file to copy or bind")
	}
	names, err := homeFileNames(home)
	if err != nil {
		return err
	}
	return validateHomeNames(names)
}

// homeFileNames checks each copy's digest and name and each bind's name and expiry, and
// returns the file names of both lists, copies first.
func homeFileNames(home *SandboxHome) ([]string, error) {
	names := make([]string, 0, len(home.Copy)+len(home.Bind))
	for i := 0; i < len(home.Copy); i++ {
		if !homeSHA256.MatchString(home.Copy[i].SHA256) {
			return nil, fmt.Errorf("home.copy/%d.sha256: not 64 lowercase hex digits", i)
		}
		if !homeFileName.MatchString(home.Copy[i].File) {
			return nil, fmt.Errorf("home: file %q is not one plain path element", home.Copy[i].File)
		}
		names = append(names, home.Copy[i].File)
	}
	for i := 0; i < len(home.Bind); i++ {
		if err := validateHomeBind(i, home.Bind[i]); err != nil {
			return nil, err
		}
		names = append(names, home.Bind[i].File)
	}
	return names, nil
}

// validateHomeBind checks a bind's name and that it names its token's expiry exactly once.
func validateHomeBind(i int, bind SandboxHomeBind) error {
	if !homeFileName.MatchString(bind.File) && !slices.Contains(homeCredentialFiles, bind.File) {
		return fmt.Errorf("home: file %q is not one plain path element nor a known login file", bind.File)
	}
	return validateHomeExpiry(i, bind)
}

func validateHomeExpiry(i int, bind SandboxHomeBind) error {
	switch {
	case bind.JWT == "" && bind.ExpiresMS == "":
		return fmt.Errorf("home.bind/%d: names neither jwt nor expires_ms; a bound login's expiry is checked before each start", i)
	case bind.JWT != "" && bind.ExpiresMS != "":
		return fmt.Errorf("home.bind/%d: names both jwt and expires_ms; name the one the file holds", i)
	case bind.JWT != "" && !homeJWTPath.MatchString(bind.JWT):
		return fmt.Errorf("home.bind/%d.jwt: %q is no dotted path of 1..8 names", i, bind.JWT)
	case bind.ExpiresMS != "" && !homeJWTPath.MatchString(bind.ExpiresMS):
		return fmt.Errorf("home.bind/%d.expires_ms: %q is no dotted path of 1..8 names", i, bind.ExpiresMS)
	}
	return nil
}

// validateHomeEnv checks the variable that points the client at its home. HOME is the one
// variable of the sandbox's own a home may take: a client that keeps its login below HOME
// then sees the home in place of the workspace there.
func validateHomeEnv(name string, allowed []string) error {
	if !sandboxEnvName.MatchString(name) {
		return fmt.Errorf("home.env: invalid name %q", name)
	}
	if name == "HOME" {
		return nil
	}
	if slices.Contains(reservedSandboxEnv, name) || slices.Contains(proxySandboxEnv, name) {
		return fmt.Errorf("home.env: %s is set by the sandbox", name)
	}
	if slices.Contains(allowed, name) {
		return fmt.Errorf("home.env: %s is also in env_allow; the home sets it", name)
	}
	return nil
}

func validateHomeNames(names []string) error {
	for i := 0; i < len(names); i++ {
		if slices.Contains(names[:i], names[i]) {
			return fmt.Errorf("home: file %s is named twice", names[i])
		}
	}
	return nil
}

// resolveSandboxHome resolves the home's source like an input: it has to exist, lie in a
// root and stay clear of the login locations and the protected paths. It must not overlap the
// workspace, which the job writes: the job could then change what the next start copies.
func resolveSandboxHome(sandbox *SandboxConfig, guard protectedPaths) error {
	if sandbox.Home == nil {
		return nil
	}
	resolved, err := resolveExistingAbs(sandbox.Home.Source)
	if err != nil {
		return fmt.Errorf("home.source: %w", err)
	}
	if err = guard.check("home.source", resolved); err != nil {
		return err
	}
	if pathOverlaps(resolved, sandbox.Workspace) {
		return fmt.Errorf("home.source: %s overlaps the workspace %s", resolved, sandbox.Workspace)
	}
	// A root that names a login location opens it to inputs, never to a home: a home's login
	// is one made for the slot, and the operator's own stays where no job reaches it.
	for i := 0; i < len(loginLocations); i++ {
		login := filepath.Join(guard.home, filepath.FromSlash(loginLocations[i]))
		if guard.home != "" && pathOverlaps(resolved, login) {
			return fmt.Errorf("home.source: %s overlaps the login location %s; a slot home holds a login of its own", resolved, login)
		}
	}
	sandbox.Home.Source = resolved
	return nil
}
