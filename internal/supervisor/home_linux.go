//go:build linux

package supervisor

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

const (
	// homeTokenMargin is how long a bound token has to stay valid beyond the job's latest
	// end. Codex refreshes an access token that expires within five minutes (codex-rs
	// login/src/auth/manager.rs, CHATGPT_ACCESS_TOKEN_REFRESH_WINDOW_MINUTES in 0.145.0).
	homeTokenMargin = 10 * time.Minute
	// maxHomeFileBytes bounds a file the shim reads from a home's source.
	maxHomeFileBytes = 1 << 20
)

// errHomeToken marks a start refused because a bound token would expire during the run.
var errHomeToken = errors.New("home: the bound login expires before the run can end")

// homeDir is where the job's home lies.
func homeDir(sandbox config.SandboxConfig) string {
	return filepath.Join(sandbox.Workspace, filepath.FromSlash(config.SandboxHomeDir))
}

// prepareHome makes the job's home before a start: it removes what an earlier start left,
// copies each copy file when it hashes to the configured value, and puts an empty file where
// each bind file is bound, so that the bind lands on a file of the shim's own making. A bind
// file whose token expires before until is refused: the client would try to refresh the
// token and write the file, which it cannot, and a refresh token the provider rotated
// without the new one being kept is lost for the login outside the slot too.
//
// No job of this sandbox runs while the shim prepares: the shim holds the job's lock.
func prepareHome(sandbox config.SandboxConfig, until time.Time) error {
	home := sandbox.Home
	if home == nil {
		return nil
	}
	if until.IsZero() {
		return fmt.Errorf("home: the run's latest end is unknown")
	}
	until = until.Add(homeTokenMargin)
	if err := requireDirectory(home.Source); err != nil {
		return fmt.Errorf("home: %w", err)
	}
	if err := requireUnchangedResolution("home source", home.Source); err != nil {
		return err
	}
	dir, err := freshHomeDir(sandbox.Workspace)
	if err != nil {
		return err
	}
	return fillHome(home, dir, until)
}

// fillHome copies each copy file into the fresh home and puts an empty file where each bind
// file is bound, after checking that its token outlasts until.
func fillHome(home *config.SandboxHome, dir string, until time.Time) error {
	for i := 0; i < len(home.Copy); i++ {
		if err := copyHomeFile(home.Source, dir, home.Copy[i]); err != nil {
			return err
		}
	}
	for i := 0; i < len(home.Bind); i++ {
		if err := checkHomeBind(home.Source, home.Bind[i], until); err != nil {
			return err
		}
		if err := writeHomeBindTarget(dir, home.Bind[i].File); err != nil {
			return err
		}
	}
	return nil
}

// freshHomeDir makes an empty home below the workspace. Each element on the way is a real
// directory, never a symlink the job left: the shim writes there with its own rights.
func freshHomeDir(workspace string) (string, error) {
	parts := strings.Split(config.SandboxHomeDir, "/")
	dir := workspace
	for i := 0; i < len(parts); i++ {
		dir = filepath.Join(dir, parts[i])
		if err := homeDirElement(dir, i == len(parts)-1); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// homeDirElement makes one directory on the way to the home, or checks that the one there is
// a real directory. The last element, the home itself, is removed and made again empty.
func homeDirElement(dir string, last bool) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.Mkdir(dir, 0o700); err != nil {
			return fmt.Errorf("home: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("home: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("home: %s is no directory", dir)
	}
	if !last {
		return nil
	}
	if err = os.RemoveAll(dir); err != nil {
		return fmt.Errorf("home: remove the home of an earlier start: %w", err)
	}
	if err = os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("home: %w", err)
	}
	return nil
}

// writeHomeBindTarget puts the empty file a bind lands on, making the folders a login file
// below a folder needs. The home is fresh and of the shim's making, so each folder is new.
func writeHomeBindTarget(dir string, file string) error {
	parts := strings.Split(file, "/")
	for i := 0; i < len(parts)-1; i++ {
		dir = filepath.Join(dir, parts[i])
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("home: %w", err)
		}
		if err := requireRealDirectory(dir); err != nil {
			return err
		}
	}
	return writeHomeFile(filepath.Join(dir, parts[len(parts)-1]), nil)
}

// requireRealDirectory refuses a path that is a symlink or no directory.
func requireRealDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("home: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("home: %s is no directory", dir)
	}
	return nil
}

func copyHomeFile(source string, dir string, entry config.SandboxHomeCopy) error {
	body, err := readHomeSource(source, entry.File)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != entry.SHA256 {
		return fmt.Errorf("home: copy %s hashes to %s, the configuration names %s", entry.File, got, entry.SHA256)
	}
	return writeHomeFile(filepath.Join(dir, entry.File), body)
}

// readHomeSource reads a regular file of the home's source. A symlink is refused, the file's
// and each folder's on the way to it: it could lead anywhere the configuration's checks
// never saw.
func readHomeSource(source string, name string) ([]byte, error) {
	parts := strings.Split(name, "/")
	path := source
	for i := 0; i < len(parts)-1; i++ {
		path = filepath.Join(path, parts[i])
		if err := requireRealDirectory(path); err != nil {
			return nil, err
		}
	}
	path = filepath.Join(path, parts[len(parts)-1])
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("home: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("home: %s is no regular file", path)
	}
	// #nosec G304 -- the path is a configured file name in the configured home source.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("home: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(f, maxHomeFileBytes+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("home: read %s: %w", path, err)
	}
	if len(body) > maxHomeFileBytes {
		return nil, fmt.Errorf("home: %s exceeds %d bytes", path, maxHomeFileBytes)
	}
	return body, nil
}

func writeHomeFile(path string, body []byte) error {
	// #nosec G304 -- the path lies in the home the shim just made.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("home: %w", err)
	}
	_, err = f.Write(body)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("home: write %s: %w", path, err)
	}
	return nil
}

// checkHomeBind refuses a bind file that is no regular file, one that names no expiry, and
// one whose token expires before until. The token itself never leaves this function, not in
// an error either.
func checkHomeBind(source string, entry config.SandboxHomeBind, until time.Time) error {
	body, err := readHomeSource(source, entry.File)
	if err != nil {
		return err
	}
	path, exp := entry.JWT, time.Time{}
	switch {
	case entry.JWT != "":
		exp, err = jwtExpiry(body, entry.JWT)
	case entry.ExpiresMS != "":
		path = entry.ExpiresMS
		exp, err = millisExpiry(body, entry.ExpiresMS)
	default:
		return fmt.Errorf("home: bind %s names no expiry; the configuration checks it", entry.File)
	}
	if err != nil {
		return fmt.Errorf("home: bind %s: the token's expiry at %s: %w", entry.File, path, err)
	}
	if !exp.After(until) {
		return fmt.Errorf("%w: bind %s: the token at %s expires at %s, the run may last until %s; refresh the login outside the slot, an operator act, and start the run again",
			errHomeToken, entry.File, path, exp.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339))
	}
	return nil
}

// jsonValueAt returns the value at the dotted path in a JSON document. Numbers stay
// json.Number.
func jsonValueAt(body []byte, path string) (any, error) {
	var doc any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		return nil, errors.New("the file is no JSON document")
	}
	names := strings.Split(path, ".")
	for i := 0; i < len(names); i++ {
		object, ok := doc.(map[string]any)
		if !ok {
			return nil, errors.New("the path leads through no object")
		}
		if doc, ok = object[names[i]]; !ok {
			return nil, errors.New("the path names no value")
		}
	}
	return doc, nil
}

// jwtExpiry reads the exp claim of the JSON Web Token at the dotted path in a JSON document.
// The signature is not checked: the token is the client's, and only its expiry is wanted.
func jwtExpiry(body []byte, path string) (time.Time, error) {
	doc, err := jsonValueAt(body, path)
	if err != nil {
		return time.Time{}, err
	}
	token, ok := doc.(string)
	if !ok {
		return time.Time{}, errors.New("the value is no string")
	}
	return jwtClaimExp(token)
}

// millisExpiry reads the number of Unix milliseconds at the dotted path in a JSON document.
func millisExpiry(body []byte, path string) (time.Time, error) {
	doc, err := jsonValueAt(body, path)
	if err != nil {
		return time.Time{}, err
	}
	number, ok := doc.(json.Number)
	if !ok {
		return time.Time{}, errors.New("the value is no number")
	}
	ms, err := number.Int64()
	if err != nil || ms <= 0 || ms >= math.MaxInt64/2 {
		return time.Time{}, errors.New("the value is no time in milliseconds")
	}
	return time.UnixMilli(ms), nil
}

func jwtClaimExp(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("the value is no JSON Web Token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, errors.New("the token's payload is no base64url")
	}
	var claims struct {
		Exp *json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err = decoder.Decode(&claims); err != nil || claims.Exp == nil {
		return time.Time{}, errors.New("the token has no exp claim")
	}
	exp, err := claims.Exp.Float64()
	if err != nil || exp <= 0 || exp >= math.MaxInt64/2 {
		return time.Time{}, errors.New("the token's exp claim is no time")
	}
	return time.Unix(int64(exp), 0), nil
}

// appendHomeArgs binds each bind file read-only over the empty file the shim made for it,
// and points the client at the home. Each folder between the home and a bind file is pinned
// first, as the read-only paths' are, so that the job cannot move it aside and put a
// writable file where the client looks for its login. A home that takes HOME is set where
// the sandbox sets HOME, once, so that no later setting replaces it.
func appendHomeArgs(args []string, sandbox config.SandboxConfig) []string {
	home := sandbox.Home
	if home == nil {
		return args
	}
	dir := homeDir(sandbox)
	targets := make([]string, 0, len(home.Bind))
	for i := 0; i < len(home.Bind); i++ {
		targets = append(targets, filepath.Join(dir, filepath.FromSlash(home.Bind[i].File)))
	}
	ancestors := readOnlyAncestors(dir, targets)
	for i := 0; i < len(ancestors); i++ {
		args = append(args, "--bind", ancestors[i], ancestors[i])
	}
	for i := 0; i < len(home.Bind); i++ {
		args = append(args, "--ro-bind", filepath.Join(home.Source, filepath.FromSlash(home.Bind[i].File)), targets[i])
	}
	if home.Env == "HOME" {
		return args
	}
	return append(args, "--setenv", home.Env, dir)
}

// jobHome is the job's HOME: its home when the home takes HOME, else its workspace.
func jobHome(sandbox config.SandboxConfig) string {
	if sandbox.Home != nil && sandbox.Home.Env == "HOME" {
		return homeDir(sandbox)
	}
	return sandbox.Workspace
}
