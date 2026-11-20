package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/releasewatch"
)

func TestRunWatchRejectsMissingAndUnknownCommand(t *testing.T) {
	if err := runWatch(nil); err == nil || !strings.Contains(err.Error(), "watch command required") {
		t.Fatalf("runWatch(nil) = %v, want command required", err)
	}
	if err := runWatch([]string{"bogus"}); err == nil || !strings.Contains(err.Error(), "unknown watch command: bogus") {
		t.Fatalf("runWatch(bogus) = %v, want unknown command", err)
	}
}

func TestRunWatchRequiresConfig(t *testing.T) {
	for _, action := range []string{"run", "seed"} {
		t.Run(action, func(t *testing.T) {
			err := runWatch([]string{action})
			if err == nil || !strings.Contains(err.Error(), "--config is required") {
				t.Fatalf("runWatch(%s) = %v, want config required", action, err)
			}
		})
	}
}

func TestRunWatchRejectsInvalidBaseURLs(t *testing.T) {
	for _, action := range []string{"run", "seed"} {
		for _, flagName := range []string{"--github-base", "--huggingface-base", "--ntfy-base"} {
			for _, badURL := range []string{"not-a-url", "ftp://example.com", "/relative/path"} {
				t.Run(action+"/"+flagName+"/"+badURL, func(t *testing.T) {
					err := runWatchAction(action, []string{"--config=dummy.yaml", flagName + "=" + badURL})
					if err == nil || !strings.Contains(err.Error(), "must be an absolute http(s) URL") {
						t.Fatalf("runWatchAction(%s, %s=%s) = %v, want invalid URL error", action, flagName, badURL, err)
					}
				})
			}
		}
	}
}

func TestRunWatchActionBadFlagsExit2(t *testing.T) {
	for _, action := range []string{"run", "seed"} {
		t.Run(action, func(t *testing.T) {
			err := runWatchAction(action, []string{"-bad-flag"})
			if err == nil || !strings.Contains(err.Error(), "flag provided") {
				t.Fatalf("runWatchAction(%s, -bad-flag) = %v, want flag error", action, err)
			}
			var ec interface{ ExitCode() int }
			if !errors.As(err, &ec) || ec.ExitCode() != 2 {
				t.Fatalf("runWatchAction(%s) error exit code = %v, want 2", action, ec)
			}
		})
	}
}

func TestCLIWatchBadFlagsExit2Subprocess(t *testing.T) {
	bin := buildCLI(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "watch", "run", "--bad-flag") // #nosec G204 -- bin is the CLI built by the test
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatal("epitropctl watch run --bad-flag = nil error, want exit 2")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("exit code = %v, want 2; stderr = %s", err, stderr.String())
	}
}

// watchStandIn answers the three GitHub calls one release watch pass makes for the route that
// writeWatchConfig declares, and counts the issues it is asked to file. Any other request fails
// the test.
func watchStandIn(t *testing.T, posts *int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/upstream/releases":
			if _, err := w.Write([]byte(`[{"tag_name":"v1.0.0","html_url":"https://github.com/owner/upstream/releases/tag/v1.0.0","body":"notes","published_at":"2026-10-01T12:00:00Z"}]`)); err != nil {
				t.Errorf("write stand-in response: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/consumer/issues":
			if _, err := w.Write([]byte(`[]`)); err != nil {
				t.Errorf("write stand-in response: %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/consumer/issues":
			atomic.AddInt32(posts, 1)
			w.WriteHeader(http.StatusCreated)
			if _, err := w.Write([]byte(`{"number":1,"html_url":"https://github.com/owner/consumer/issues/1"}`)); err != nil {
				t.Errorf("write stand-in response: %v", err)
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// writeWatchConfig writes a config with one route from owner/upstream releases to issues in
// owner/consumer, and points the watch service at server.
func writeWatchConfig(t *testing.T, server *httptest.Server) (cfgPath, stateDir string) {
	t.Helper()
	stateDir = t.TempDir()
	cfg := map[string]any{"release_watch": map[string]any{
		"state_dir": stateDir,
		"routes": []any{map[string]any{
			"name":    "rt",
			"sources": []any{map[string]any{"github": "owner/upstream"}},
			"sinks":   []any{map[string]any{"github_issue": map[string]any{"repo": "owner/consumer"}}},
		}},
	}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal(config) = %v", err)
	}
	cfgPath = filepath.Join(stateDir, "config.json")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	oldNewService := newWatchService
	t.Cleanup(func() { newWatchService = oldNewService })
	newWatchService = func(cfg config.Config, opts releasewatch.ClientOptions) (*releasewatch.Service, error) {
		opts.GitHubBase = server.URL
		opts.LookupEnv = func(string) (string, bool) { return "token", true }
		return releasewatch.NewService(cfg, opts)
	}
	return cfgPath, stateDir
}

func TestRunWatchSeedRecordsWithoutFiling(t *testing.T) {
	var posts int32
	cfgPath, stateDir := writeWatchConfig(t, watchStandIn(t, &posts))

	if err := runWatch([]string{"seed", "--config", cfgPath}); err != nil {
		t.Fatalf("runWatch(seed) = %v, want nil", err)
	}
	if got := atomic.LoadInt32(&posts); got != 0 {
		t.Fatalf("issues filed by seed = %d, want 0", got)
	}
	state, err := releasewatch.LoadState(stateDir)
	if err != nil || !state.Has("github:owner/upstream@v1.0.0") {
		t.Fatalf("LoadState() = %v, want the seeded release recorded", err)
	}
}

func TestRunWatchRunFilesOnce(t *testing.T) {
	var posts int32
	cfgPath, stateDir := writeWatchConfig(t, watchStandIn(t, &posts))

	if err := runWatch([]string{"run", "--config", cfgPath, "--once"}); err != nil {
		t.Fatalf("runWatch(run, pass 1) = %v, want nil", err)
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("issues filed after pass 1 = %d, want 1", got)
	}
	state, err := releasewatch.LoadState(stateDir)
	if err != nil || !state.Has("github:owner/upstream@v1.0.0") {
		t.Fatalf("LoadState() = %v, want the release recorded after the run", err)
	}

	if err := runWatch([]string{"run", "--config", cfgPath, "--once"}); err != nil {
		t.Fatalf("runWatch(run, pass 2) = %v, want nil", err)
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("issues filed after pass 2 = %d, want still 1", got)
	}
}

func TestRunWatchMissingConfigFile(t *testing.T) {
	err := runWatch([]string{"run", "--config", filepath.Join(t.TempDir(), "missing.json"), "--once"})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("runWatch(missing config) = %v, want fs.ErrNotExist", err)
	}
}

type watchCustomSentinelErr struct {
	msg string
}

func (e *watchCustomSentinelErr) Error() string {
	return e.msg
}

func TestFlagErrUnwrap(t *testing.T) {
	sentinel := &watchCustomSentinelErr{msg: "inner error"}
	wrapped := flagErr{err: sentinel}

	t.Run("errors.Is matches wrapped sentinel", func(t *testing.T) {
		if !errors.Is(wrapped, sentinel) {
			t.Fatalf("errors.Is(wrapped, sentinel) = false, want true")
		}
		other := errors.New("other error")
		if errors.Is(wrapped, other) {
			t.Fatalf("errors.Is(wrapped, other) = true, want false")
		}
	})

	t.Run("errors.As extracts wrapped target pointer", func(t *testing.T) {
		var target *watchCustomSentinelErr
		if !errors.As(wrapped, &target) {
			t.Fatalf("errors.As(wrapped, &target) = false, want true")
		}
		if target != sentinel {
			t.Fatalf("target = %v, want %v", target, sentinel)
		}
	})

	t.Run("Error and ExitCode pass-through", func(t *testing.T) {
		if wrapped.Error() != "inner error" {
			t.Fatalf("wrapped.Error() = %q, want 'inner error'", wrapped.Error())
		}
		if wrapped.ExitCode() != 2 {
			t.Fatalf("wrapped.ExitCode() = %d, want 2", wrapped.ExitCode())
		}
	})
}
