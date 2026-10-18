package claudelocal

// Ported from cordanaLLM/praetor tribunus/internal/sources/claudelocal at 5d4acfb (EUPL-1.2).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"
)

// The ceiling store keeps each window's calibrated ceiling between fetches,
// so a ceiling survives a window reset and a restart even after the
// rejection that set it has left the scanned logs. One JSON file per
// account lives under the user's state folder.
const (
	// DefaultAccount names the store file when the caller names no account.
	DefaultAccount = "default"
	// DefaultMaxCeilingAge is how old a ceiling's rejection may be before the
	// ceiling counts as unknown again.
	DefaultMaxCeilingAge = 30 * 24 * time.Hour
	storeVersion         = 1
	maxStoreBytes        = 64 << 10
	maxStoredWindows     = 64
	maxSourceFileBytes   = 1024
	// pendingSuffix names the staged file a store write renames into place.
	pendingSuffix = ".new"
)

// accountName keeps an account usable as a file name on every platform.
var accountName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ceilingStore is the on-disk store of one account.
type ceilingStore struct {
	Version  int                      `json:"version"`
	Account  string                   `json:"account"`
	Ceilings map[string]storedCeiling `json:"ceilings"`
}

// storedCeiling is one window type's ceiling and where it came from: the
// rejected request that set it, the reset that request reported, and when
// this store recorded it.
type storedCeiling struct {
	Ceiling     float64   `json:"ceiling_usd_equivalent"`
	RejectedAt  time.Time `json:"rejected_at"`
	SourceFile  string    `json:"source_file"`
	WindowReset time.Time `json:"window_reset"`
	RecordedAt  time.Time `json:"recorded_at"`
}

// DefaultStateDir is the epitrop folder below the user's state folder:
// $XDG_STATE_HOME or ~/.local/state on Linux and the BSDs, %LOCALAPPDATA%
// on Windows, ~/Library/Application Support on macOS.
func DefaultStateDir() (string, error) {
	base, err := userStateDir()
	if err != nil {
		return "", fmt.Errorf("claude-local: resolve state folder: %w", err)
	}
	return filepath.Join(base, "epitrop"), nil
}

func userStateDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return dir, nil
		}
		return "", errors.New("LOCALAPPDATA is not set")
	case "darwin", "ios":
		return os.UserConfigDir()
	}
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state"), nil
}

func storePath(dir, account string) string {
	return filepath.Join(dir, "claude-ceilings", account+".json")
}

// loadStore reads an account's store. A missing file is an empty store; a
// file that does not decode or validate is refused, never repaired.
func loadStore(path, account string) (*ceilingStore, error) {
	// #nosec G304 -- path is storePath under the caller's state folder and a validated account name.
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &ceilingStore{Version: storeVersion, Account: account, Ceilings: map[string]storedCeiling{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read ceiling store %s: %w", path, err)
	}
	if len(data) > maxStoreBytes {
		return nil, fmt.Errorf("ceiling store %s exceeds %d bytes", path, maxStoreBytes)
	}
	var store ceilingStore
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&store); err != nil {
		return nil, fmt.Errorf("ceiling store %s is corrupt: %w", path, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("ceiling store %s is corrupt: data after the JSON value", path)
	}
	if err := store.validate(account); err != nil {
		return nil, fmt.Errorf("ceiling store %s is corrupt: %w", path, err)
	}
	return &store, nil
}

func (s *ceilingStore) validate(account string) error {
	if s.Version != storeVersion || s.Account != account {
		return fmt.Errorf("want version %d for account %s, found version %d for account %q", storeVersion, account, s.Version, s.Account)
	}
	if len(s.Ceilings) > maxStoredWindows {
		return fmt.Errorf("more than %d windows", maxStoredWindows)
	}
	for limitType, entry := range s.Ceilings {
		if err := entry.validate(limitType); err != nil {
			return err
		}
	}
	return nil
}

func (c storedCeiling) validate(limitType string) error {
	if !specFor(limitType).known {
		return fmt.Errorf("unknown window type %q", limitType)
	}
	if math.IsNaN(c.Ceiling) || math.IsInf(c.Ceiling, 0) || c.Ceiling <= 0 {
		return fmt.Errorf("window %s: ceiling must be a positive number", limitType)
	}
	if c.RejectedAt.IsZero() || c.WindowReset.IsZero() || c.RecordedAt.IsZero() {
		return fmt.Errorf("window %s: rejected_at, window_reset and recorded_at are required", limitType)
	}
	if c.SourceFile == "" || len(c.SourceFile) > maxSourceFileBytes {
		return fmt.Errorf("window %s: source_file must name the log file", limitType)
	}
	return nil
}

// saveStore writes the store atomically: the whole file is written and
// synced as path+".new", then renamed onto path, so a reader sees either the
// old store or the new one, never a partial file. A failed write removes
// the staged file and leaves path untouched.
func saveStore(path string, store *ceilingStore) error {
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("encode ceiling store: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create ceiling store folder: %w", err)
	}
	return replaceFile(path, append(data, '\n'), writeAndSync)
}

// writeAndSync writes data to f and flushes it to disk; tests substitute a
// writer that fails part-way to simulate an interrupted write.
func writeAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

func replaceFile(path string, data []byte, write func(*os.File, []byte) error) (err error) {
	pending := path + pendingSuffix
	// #nosec G304 -- pending is storePath plus a fixed suffix.
	f, err := os.OpenFile(pending, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("stage ceiling store: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeIfPresent(pending))
		}
	}()
	if writeErr := write(f, data); writeErr != nil {
		return errors.Join(fmt.Errorf("write ceiling store: %w", writeErr), f.Close())
	}
	if closeErr := f.Close(); closeErr != nil {
		return fmt.Errorf("close staged ceiling store: %w", closeErr)
	}
	if renameErr := os.Rename(pending, path); renameErr != nil {
		return fmt.Errorf("replace ceiling store: %w", renameErr)
	}
	return nil
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
