package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/catalog"
	"github.com/tekmora/epitrop/internal/sources/httpfetch"
	"github.com/tekmora/epitrop/internal/sources/litellmgateway"
	"github.com/tekmora/epitrop/internal/sources/ollamalocal"
)

const (
	liveHTTPTimeout  = 10 * time.Second
	liveMaxBodyBytes = 8 << 20
	liveMaxModels    = 5000
)

var liveSyncEnvNames = [5]string{
	"EPITROP_LIVE_OLLAMA",
	"EPITROP_LIVE_OLLAMA_MODEL",
	"EPITROP_LIVE_LITELLM_BASE",
	"EPITROP_LIVE_LITELLM_TOKEN_FILE",
	"EPITROP_LIVE_LITELLM_MODEL",
}

type liveSyncEnv struct {
	ollama           string
	ollamaModel      string
	litellmBase      string
	litellmTokenFile string
	litellmModel     string
}

// TestSystemSyncLive runs epitropctl sync against real Ollama and LiteLLM
// servers when all five EPITROP_LIVE_* environment variables are set, and skips
// during normal go test runs when any of them is unset.
func TestSystemSyncLive(t *testing.T) {
	cfg := requireLiveSyncEnv(t)
	runLiveSyncSuite(t, cfg)
}

func runLiveSyncSuite(t *testing.T, cfg liveSyncEnv) {
	t.Helper()
	ollamaVer, err := fetchOllamaVersion(t.Context(), cfg.ollama)
	if err != nil {
		t.Fatalf("ollama version: %v", err)
	}
	litellmVer, err := fetchLiteLLMVersion(t.Context(), cfg.litellmBase)
	if err != nil {
		t.Fatalf("litellm version: %v", err)
	}
	t.Logf("live server versions: ollama=%s litellm=%s", ollamaVer, litellmVer)

	wantOllama, err := fetchOllamaTagCount(t.Context(), cfg.ollama)
	if err != nil {
		t.Fatalf("ollama direct model count: %v", err)
	}
	wantGateway, err := fetchLiteLLMModelCount(t.Context(), cfg.litellmBase, cfg.litellmTokenFile)
	if err != nil {
		t.Fatalf("litellm direct model count: %v", err)
	}

	bin := buildCLI(t)
	t.Run("positive_and_boundary", func(t *testing.T) {
		runLiveSyncPositiveAndBoundary(t, bin, cfg, wantOllama, wantGateway)
	})
	t.Run("negative_wrong_token", func(t *testing.T) {
		runLiveSyncNegativeWrongToken(t, bin, cfg, wantOllama)
	})
}

func requireLiveSyncEnv(t *testing.T) liveSyncEnv {
	t.Helper()
	missing := make([]string, 0, len(liveSyncEnvNames))
	values := make([]string, len(liveSyncEnvNames))
	for i := 0; i < len(liveSyncEnvNames); i++ {
		v := strings.TrimSpace(os.Getenv(liveSyncEnvNames[i]))
		if v == "" {
			missing = append(missing, liveSyncEnvNames[i])
		}
		values[i] = v
	}
	if len(missing) > 0 {
		t.Skipf("live sync skipped: unset %s (set by .github/workflows/live-sync.yml)", strings.Join(missing, ", "))
	}
	return liveSyncEnv{
		ollama:           strings.TrimRight(values[0], "/"),
		ollamaModel:      values[1],
		litellmBase:      strings.TrimRight(values[2], "/"),
		litellmTokenFile: values[3],
		litellmModel:     values[4],
	}
}

func fetchOllamaVersion(ctx context.Context, baseURL string) (string, error) {
	body, err := httpfetch.Get(ctx, baseURL+"/api/version", liveHTTPTimeout, liveMaxBodyBytes)
	if err != nil {
		return "", fmt.Errorf("request ollama /api/version: %w", err)
	}
	var parsed struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode ollama /api/version: %w", err)
	}
	if strings.TrimSpace(parsed.Version) == "" {
		return "", fmt.Errorf("ollama %s/api/version returned an empty version", baseURL)
	}
	return parsed.Version, nil
}

// In LiteLLM v1.109.1, unauthenticated GET /health/readiness omits
// litellm_version unless general_settings.allow_public_health_readiness_details
// is true (litellm/proxy/health_endpoints/_health_endpoints.py:2326-2346), and
// GET /health/readiness/details (:2349-2358) requires proxy-admin auth
// (litellm/proxy/auth/route_checks.py:423). GET /public/model_hub/info
// (litellm/proxy/public_endpoints/public_endpoints.py:417-439, mounted at
// litellm/proxy/proxy_server.py:21832, listed in public_routes at
// litellm/proxy/_types.py:750) returns litellm_version unconditionally.
func fetchLiteLLMVersion(ctx context.Context, baseURL string) (string, error) {
	body, err := httpfetch.Get(ctx, baseURL+"/public/model_hub/info", liveHTTPTimeout, liveMaxBodyBytes)
	if err != nil {
		return "", fmt.Errorf("request litellm /public/model_hub/info: %w", err)
	}
	var parsed struct {
		Version string `json:"litellm_version"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode litellm /public/model_hub/info: %w", err)
	}
	if strings.TrimSpace(parsed.Version) == "" {
		return "", fmt.Errorf("litellm %s/public/model_hub/info returned an empty litellm_version", baseURL)
	}
	return parsed.Version, nil
}

func fetchOllamaTagCount(ctx context.Context, baseURL string) (int, error) {
	body, err := httpfetch.Get(ctx, baseURL+"/api/tags", liveHTTPTimeout, liveMaxBodyBytes)
	if err != nil {
		return 0, fmt.Errorf("request ollama /api/tags: %w", err)
	}
	var parsed struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("decode ollama /api/tags: %w", err)
	}
	if len(parsed.Models) == 0 || len(parsed.Models) > liveMaxModels {
		return 0, fmt.Errorf("ollama /api/tags count = %d, want 1..%d", len(parsed.Models), liveMaxModels)
	}
	return len(parsed.Models), nil
}

func fetchLiteLLMModelCount(ctx context.Context, baseURL, tokenFile string) (int, error) {
	token, err := litellmgateway.ReadToken(tokenFile)
	if err != nil {
		return 0, fmt.Errorf("read litellm token: %w", err)
	}
	if token == "" {
		return 0, fmt.Errorf("litellm token file %s is empty", tokenFile)
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	body, err := httpfetch.GetWithHeader(ctx, baseURL+"/v1/models", header, liveHTTPTimeout, liveMaxBodyBytes)
	if err != nil {
		return 0, fmt.Errorf("request litellm /v1/models: %w", err)
	}
	var parsed struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("decode litellm /v1/models: %w", err)
	}
	if len(parsed.Data) == 0 || len(parsed.Data) > liveMaxModels {
		return 0, fmt.Errorf("litellm /v1/models count = %d, want 1..%d", len(parsed.Data), liveMaxModels)
	}
	return len(parsed.Data), nil
}

func runLiveSyncPositiveAndBoundary(t *testing.T, bin string, cfg liveSyncEnv, wantOllama, wantGateway int) {
	t.Helper()
	snapshotPath := filepath.Join(t.TempDir(), "snapshot.json")
	args := []string{
		"sync",
		"--sources=ollama-local,litellm-gateway",
		"--ollama=" + cfg.ollama,
		"--litellm-base=" + cfg.litellmBase,
		"--litellm-token-file=" + cfg.litellmTokenFile,
		"--out=" + snapshotPath,
	}
	stdout, stderr, code, err := runSystemCLI(t, bin, args...)
	if err != nil || code != 0 {
		t.Fatalf("sync exited with code %d, err %v: %s\n%s", code, err, stdout, stderr)
	}
	t.Logf("sync stdout:\n%s", strings.TrimRight(stdout, "\r\n"))

	wantStdout := strings.Join([]string{
		fmt.Sprintf("ollama-local     ok   count=%d", wantOllama),
		fmt.Sprintf("litellm-gateway  ok   count=%d", wantGateway),
		fmt.Sprintf("wrote %d records to %s", wantOllama+wantGateway, snapshotPath),
		"",
	}, "\n")
	if strings.ReplaceAll(stdout, "\r\n", "\n") != wantStdout {
		t.Fatalf("sync stdout =\n%s\nwant:\n%s", stdout, wantStdout)
	}
	assertLivePositiveSnapshot(t, snapshotPath, cfg, wantOllama, wantGateway)
}

func assertLivePositiveSnapshot(t *testing.T, snapshotPath string, cfg liveSyncEnv, wantOllama, wantGateway int) {
	t.Helper()
	snap, err := readSnapshotFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if snap.SchemaVersion != catalog.SnapshotSchemaVersion {
		t.Fatalf("schema_version = %d, want %d", snap.SchemaVersion, catalog.SnapshotSchemaVersion)
	}
	if len(snap.Records) != wantOllama+wantGateway {
		t.Fatalf("len(records) = %d, want %d", len(snap.Records), wantOllama+wantGateway)
	}
	assertLiveSourceRuns(t, snap.SourceRuns, catalog.StatusOK, wantOllama, catalog.StatusOK, wantGateway)
	if got := countLiveRecords(snap.Records, cfg.ollamaModel, catalog.AccessLocal, ollamalocal.SourceName); got != 1 {
		t.Fatalf("local record count for %q = %d, want 1", cfg.ollamaModel, got)
	}
	if got := countLiveRecords(snap.Records, cfg.litellmModel, catalog.AccessGateway, litellmgateway.SourceName); got != 1 {
		t.Fatalf("gateway record count for %q = %d, want 1", cfg.litellmModel, got)
	}
}

func runLiveSyncNegativeWrongToken(t *testing.T, bin string, cfg liveSyncEnv, wantOllama int) {
	t.Helper()
	const wrongKey = "sk-wrong-live-sync-token-0000000000000000"
	wrongTokenFile := filepath.Join(t.TempDir(), "wrong-token")
	if err := os.WriteFile(wrongTokenFile, []byte(wrongKey+"\n"), 0o600); err != nil {
		t.Fatalf("write wrong token file: %v", err)
	}
	snapshotPath := filepath.Join(t.TempDir(), "snapshot-negative.json")
	args := []string{
		"sync",
		"--sources=ollama-local,litellm-gateway",
		"--ollama=" + cfg.ollama,
		"--litellm-base=" + cfg.litellmBase,
		"--litellm-token-file=" + wrongTokenFile,
		"--out=" + snapshotPath,
	}
	stdout, stderr, code, err := runSystemCLI(t, bin, args...)
	if err != nil || code != 0 {
		t.Fatalf("sync with wrong token exited with code %d, err %v: %s\n%s", code, err, stdout, stderr)
	}
	if strings.Contains(stdout, wrongKey) || strings.Contains(stderr, wrongKey) {
		t.Fatal("sync leaked the wrong bearer token")
	}
	assertNegativeStdout(t, stdout, snapshotPath, wantOllama)
	snap, err := readSnapshotFile(snapshotPath)
	if err != nil {
		t.Fatalf("read negative snapshot: %v", err)
	}
	assertLiveSourceRuns(t, snap.SourceRuns, catalog.StatusOK, wantOllama, catalog.StatusFail, 0)
	if got := countLiveRecords(snap.Records, cfg.ollamaModel, catalog.AccessLocal, ollamalocal.SourceName); got != 1 {
		t.Fatalf("negative run local record count for %q = %d, want 1", cfg.ollamaModel, got)
	}
	if got := countLiveRecords(snap.Records, cfg.litellmModel, catalog.AccessGateway, litellmgateway.SourceName); got != 0 {
		t.Fatalf("negative run gateway record count = %d, want 0", got)
	}
}

func assertNegativeStdout(t *testing.T, stdout, snapshotPath string, wantOllama int) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(stdout, "\r\n", "\n"), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("negative sync stdout lines = %d (%q), want 3", len(lines), stdout)
	}
	wantFirst := fmt.Sprintf("ollama-local     ok   count=%d", wantOllama)
	if lines[0] != wantFirst {
		t.Fatalf("line[0] = %q, want %q", lines[0], wantFirst)
	}
	if !strings.HasPrefix(lines[1], "litellm-gateway  fail count=0 ") {
		t.Fatalf("line[1] = %q, want prefix %q", lines[1], "litellm-gateway  fail count=0 ")
	}
	wantLast := fmt.Sprintf("wrote %d records to %s", wantOllama, snapshotPath)
	if lines[2] != wantLast {
		t.Fatalf("line[2] = %q, want %q", lines[2], wantLast)
	}
}

func assertLiveSourceRuns(t *testing.T, runs []catalog.SourceRun, wantOllama catalog.Status, ollamaCount int, wantGateway catalog.Status, gatewayCount int) {
	t.Helper()
	if len(runs) != 2 {
		t.Fatalf("len(source_runs) = %d, want 2", len(runs))
	}
	if runs[0].Source != ollamalocal.SourceName || runs[0].Status != wantOllama || runs[0].Count != ollamaCount {
		t.Fatalf("source_runs[0] = %+v, want %s %s count=%d", runs[0], ollamalocal.SourceName, wantOllama, ollamaCount)
	}
	if runs[1].Source != litellmgateway.SourceName || runs[1].Status != wantGateway || runs[1].Count != gatewayCount {
		t.Fatalf("source_runs[1] = %+v, want %s %s count=%d", runs[1], litellmgateway.SourceName, wantGateway, gatewayCount)
	}
}

func countLiveRecords(records []catalog.Record, modelID string, access catalog.AccessPath, source string) int {
	matched := 0
	for i := 0; i < len(records) && i < catalog.MaxSnapshotRecords; i++ {
		r := records[i]
		if r.ModelID == modelID && r.AccessPath == access && r.Provenance.Source == source && r.Provenance.Kind == catalog.KindMeasured {
			matched++
		}
	}
	return matched
}

func TestLiveSyncContract(t *testing.T) {
	const goodKey = "sk-live-contract-key-0123456789abcdef"
	ollamaSrv, litellmSrv := newContractLiveServers(t, goodKey)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(goodKey+"\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	cfg := liveSyncEnv{
		ollama:           ollamaSrv.URL,
		ollamaModel:      "all-minilm:22m",
		litellmBase:      litellmSrv.URL,
		litellmTokenFile: tokenFile,
		litellmModel:     "live/all-minilm",
	}
	runLiveSyncSuite(t, cfg)

	t.Run("rejects_versionless_endpoint", func(t *testing.T) {
		emptySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeContractJSON(t, w, http.StatusOK, `{}`)
		}))
		t.Cleanup(emptySrv.Close)
		if _, err := fetchOllamaVersion(t.Context(), emptySrv.URL); err == nil {
			t.Fatal("fetchOllamaVersion() = nil error on empty version, want error")
		}
		if _, err := fetchLiteLLMVersion(t.Context(), emptySrv.URL); err == nil {
			t.Fatal("fetchLiteLLMVersion() = nil error on empty litellm_version, want error")
		}
	})
}

func newContractLiveServers(t *testing.T, validKey string) (*httptest.Server, *httptest.Server) {
	t.Helper()
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			writeContractJSON(t, w, http.StatusOK, `{"version":"0.40.16"}`)
		case "/api/tags":
			writeContractJSON(t, w, http.StatusOK, `{"models":[{"name":"all-minilm:22m","details":{"parameter_size":"22M","quantization_level":"F16","context_length":512}}]}`)
		case "/api/ps":
			writeContractJSON(t, w, http.StatusOK, `{"models":[]}`)
		default:
			writeContractJSON(t, w, http.StatusNotFound, `{"error":"not found"}`)
		}
	}))
	litellmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health/readiness":
			// v1.109.1 public readiness payload omits litellm_version by default.
			writeContractJSON(t, w, http.StatusOK, `{"status":"healthy","db":"Not connected"}`)
		case "/public/model_hub/info":
			writeContractJSON(t, w, http.StatusOK, `{"docs_title":"LiteLLM API","litellm_version":"1.109.1"}`)
		case "/v1/models":
			if r.Header.Get("Authorization") != "Bearer "+validKey {
				writeContractJSON(t, w, http.StatusUnauthorized, `{"error":{"message":"Authentication Error"}}`)
				return
			}
			writeContractJSON(t, w, http.StatusOK, `{"data":[{"id":"live/all-minilm","owned_by":"openai"}],"object":"list"}`)
		default:
			writeContractJSON(t, w, http.StatusNotFound, `{"error":"not found"}`)
		}
	}))
	t.Cleanup(func() {
		ollamaSrv.Close()
		litellmSrv.Close()
	})
	return ollamaSrv, litellmSrv
}

func writeContractJSON(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("write contract response: %v", err)
	}
}
