package releasewatch

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/config"
)

func summaryService(t *testing.T, base string, cfg config.Config, stdout *bytes.Buffer) *Service {
	t.Helper()
	svc, err := NewService(cfg, ClientOptions{
		GitHubBase: base,
		Stdout:     stdout,
		LookupEnv:  func(string) (string, bool) { return "fake-token", true },
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// A pass that finds nothing new still ends with its summary line (#271).
func TestPassSummaryFiledThenQuiet(t *testing.T) {
	var postCount int32
	var title, body string
	server := newPlantedReleaseServer(&postCount, &title, &body)
	defer server.Close()
	var stdout bytes.Buffer
	svc := summaryService(t, server.URL, oneRouteConfig(t, 100), &stdout)

	if err := svc.RunPass(context.Background()); err != nil {
		t.Fatalf("RunPass 1: %v", err)
	}
	want := "release-watch: pass done: sources 1 polled, 0 failed; releases 1 new, 1 filed, 0 deferred"
	if got := lastLine(stdout.String()); got != want {
		t.Fatalf("pass 1 last line = %q, want %q", got, want)
	}
	stdout.Reset()
	if err := svc.RunPass(context.Background()); err != nil {
		t.Fatalf("RunPass 2: %v", err)
	}
	want = "release-watch: pass done: sources 1 polled, 0 failed; releases 0 new, 0 filed, 0 deferred"
	if got := stdout.String(); got != want+"\n" {
		t.Fatalf("quiet pass output = %q, want exactly %q", got, want)
	}
}

func TestPassSummaryCountsAFailedSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusInternalServerError)
	}))
	defer server.Close()
	var stdout bytes.Buffer
	svc := summaryService(t, server.URL, oneRouteConfig(t, 100), &stdout)

	if err := svc.RunPass(context.Background()); err == nil {
		t.Fatal("RunPass = nil, want the failed source as an error")
	}
	want := "release-watch: pass done: sources 1 polled, 1 failed; releases 0 new, 0 filed, 0 deferred"
	if got := lastLine(stdout.String()); got != want {
		t.Fatalf("last line = %q, want %q", got, want)
	}
}

func TestPassSummaryCountsCapDeferrals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/releases"):
			testWrite(w, `[{"tag_name":"v3"},{"tag_name":"v2"},{"tag_name":"v1"}]`)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)
		default:
			testWrite(w, `[]`)
		}
	}))
	defer server.Close()
	var stdout bytes.Buffer
	cfg := testGitHubNtfyConfig(t, "owner/upstream", 1)
	svc, err := NewService(cfg, ClientOptions{GitHubBase: server.URL, NtfyBase: server.URL, Stdout: &stdout})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if err = svc.RunPass(context.Background()); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	want := "release-watch: pass done: sources 1 polled, 0 failed; releases 3 new, 1 filed, 2 deferred"
	if got := lastLine(stdout.String()); got != want {
		t.Fatalf("last line = %q, want %q", got, want)
	}
}

func TestPassSummaryNamesTheThrottle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "42")
		w.Header().Set("X-RateLimit-Reset", "1789858494")
		w.Header().Set("Content-Type", "application/json")
		testWrite(w, `[]`)
	}))
	defer server.Close()
	var stdout bytes.Buffer
	cfg := oneRouteConfig(t, 100)
	cfg.ReleaseWatch.Routes = append(cfg.ReleaseWatch.Routes, cfg.ReleaseWatch.Routes[0])
	svc := summaryService(t, server.URL, cfg, &stdout)

	if err := svc.RunPass(context.Background()); err != nil {
		t.Fatalf("RunPass = %v, want nil on throttle", err)
	}
	// The first answer is already below the floor, so its source counts as not polled.
	want := "release-watch: pass done: sources 0 polled, 0 failed; releases 0 new, 0 filed, 0 deferred; stopped early by the rate-limit throttle"
	if got := lastLine(stdout.String()); got != want {
		t.Fatalf("last line = %q, want %q", got, want)
	}
}
