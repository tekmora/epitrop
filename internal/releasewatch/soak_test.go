package releasewatch

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	soakCycles = 300
	// Each pass the stand-in publishes one new release and still lists the two before it,
	// so every pass meets two releases it has already filed.
	soakWindow  = 3
	soakTimeout = 2 * time.Minute
	// A seen-set entry is an id plus an RFC 3339 time in indented JSON, under 96 bytes.
	soakEntryBytes     = 96
	soakHeapGrowthCap  = 4 << 20
	soakWarmupPasses   = 20
	soakStateFileSlack = 64
)

// soakUpstream is a stand-in GitHub API: release list, an empty marker search, and an
// issue endpoint that counts how often each title is filed.
type soakUpstream struct {
	mu     sync.Mutex
	newest int
	filed  map[string]int
}

func (u *soakUpstream) publish() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.newest++
}

func (u *soakUpstream) releases() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	items := make([]string, 0, soakWindow)
	for n := u.newest; n > 0 && n > u.newest-soakWindow; n-- {
		items = append(items, fmt.Sprintf(`{"tag_name":"v1.0.%d","html_url":"https://github.com/owner/upstream/releases/tag/v1.0.%d","body":"notes","published_at":"2026-10-01T12:00:00Z"}`, n, n))
	}
	return "[" + strings.Join(items, ",") + "]"
}

func (u *soakUpstream) record(title string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.filed[title]++
}

func (u *soakUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/releases"):
			w.Header().Set("Content-Type", "application/json")
			testWrite(w, u.releases())
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues"):
			w.Header().Set("Content-Type", "application/json")
			testWrite(w, `[]`)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/issues"):
			payload, ok := readIssuePayload(w, r)
			if !ok {
				return
			}
			u.record(payload.Title)
			w.WriteHeader(http.StatusCreated)
			testWrite(w, `{"number":1,"title":"title","body":"body","state":"open"}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestSoakReleaseWatch runs the release watch for a few hundred passes against a stand-in
// GitHub API that publishes one release each pass. Opt-in: EPITROP_SOAK=1.
func TestSoakReleaseWatch(t *testing.T) {
	if os.Getenv("EPITROP_SOAK") != "1" {
		t.Skip("soak test: set EPITROP_SOAK=1 to run it")
	}
	up := &soakUpstream{filed: map[string]int{}}
	gh := up.server()
	defer gh.Close()
	cfg := oneRouteConfig(t, 100)
	svc, err := NewService(cfg, ClientOptions{GitHubBase: gh.URL, Stdout: &bytes.Buffer{}, LookupEnv: func(string) (string, bool) { return "token", true }})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	start := time.Now()
	heapStart, heapEnd := runSoakPasses(t, svc, up)
	elapsed := time.Since(start)
	t.Logf("soak: %d passes took %s", soakCycles, elapsed)
	requireEachReleaseFiledOnce(t, up)
	requireStateFileBounded(t, cfg.ReleaseWatch.StateDir)
	requireHeapFlat(t, heapStart, heapEnd)
}

// runSoakPasses returns the live heap after the warm-up passes and after the last pass.
func runSoakPasses(t *testing.T, svc *Service, up *soakUpstream) (uint64, uint64) {
	t.Helper()
	ctx := t.Context()
	var heapStart uint64
	for i := 0; i < soakCycles; i++ {
		if i == soakWarmupPasses {
			heapStart = liveHeap()
		}
		up.publish()
		if err := svc.RunPass(ctx); err != nil {
			t.Fatalf("pass %d: RunPass() = %v, want nil", i, err)
		}
	}
	return heapStart, liveHeap()
}

func liveHeap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func requireEachReleaseFiledOnce(t *testing.T, up *soakUpstream) {
	t.Helper()
	up.mu.Lock()
	defer up.mu.Unlock()
	t.Logf("soak: %d issues filed for %d releases", len(up.filed), soakCycles)
	if len(up.filed) != soakCycles {
		t.Errorf("filed %d distinct issues, want %d", len(up.filed), soakCycles)
	}
	for title, n := range up.filed {
		if n != 1 {
			t.Errorf("issue %q filed %d times, want 1", title, n)
		}
	}
}

func requireStateFileBounded(t *testing.T, stateDir string) {
	t.Helper()
	fi, err := os.Stat(filepath.Join(stateDir, SeenFileName))
	if err != nil {
		t.Fatalf("stat seen-set: %v", err)
	}
	limit := int64(soakCycles*soakEntryBytes + soakStateFileSlack)
	t.Logf("soak: seen-set file %d bytes, bound %d", fi.Size(), limit)
	if fi.Size() > limit {
		t.Errorf("seen-set file = %d bytes, want <= %d", fi.Size(), limit)
	}
}

func requireHeapFlat(t *testing.T, start, end uint64) {
	t.Helper()
	if start > math.MaxInt64 || end > math.MaxInt64 {
		t.Fatal("heap size exceeds int64")
	}
	endBytes := int64(end)     // #nosec G115 -- end is checked against MaxInt64 above
	startBytes := int64(start) // #nosec G115 -- start is checked against MaxInt64 above
	growth := endBytes - startBytes
	t.Logf("soak: live heap %d -> %d bytes (%+d), bound %+d", start, end, growth, soakHeapGrowthCap)
	if growth > soakHeapGrowthCap {
		t.Errorf("live heap grew %d bytes, want <= %d", growth, soakHeapGrowthCap)
	}
}
