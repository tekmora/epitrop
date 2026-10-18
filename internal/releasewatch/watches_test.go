package releasewatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/config"
)

func TestSeenPrefixMatchesFetchedIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			testWrite(w, `[{"tag_name":"v1.2.0","published_at":"2026-10-17T00:00:00Z"}]`)
		case "/api/models":
			testWrite(w, `[{"id":"org/m1"},{"id":"m2"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client, err := NewClient(ClientOptions{GitHubBase: srv.URL, HuggingFaceBase: srv.URL, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sources := []config.ReleaseWatchSource{{GitHub: "o/r"}, {HuggingFaceOrg: "org"}}
	for i := 0; i < len(sources); i++ {
		items, fetchErr := FetchSource(context.Background(), client, sources[i])
		if fetchErr != nil || len(items) == 0 {
			t.Fatalf("FetchSource(%+v) = %d items, %v; want items", sources[i], len(items), fetchErr)
		}
		for j := 0; j < len(items); j++ {
			if !strings.HasPrefix(items[j].ID, SeenPrefix(sources[i])) {
				t.Errorf("id %q does not start with SeenPrefix %q", items[j].ID, SeenPrefix(sources[i]))
			}
		}
	}
}

func TestSeenPrefixDoesNotMatchANeighbour(t *testing.T) {
	cases := []struct {
		src config.ReleaseWatchSource
		id  string
	}{
		{config.ReleaseWatchSource{GitHub: "o/r"}, "github:o/r2@v1"},
		{config.ReleaseWatchSource{HuggingFaceOrg: "org"}, "hf:org2/m"},
	}
	for i := 0; i < len(cases); i++ {
		if strings.HasPrefix(cases[i].id, SeenPrefix(cases[i].src)) {
			t.Errorf("SeenPrefix(%+v) = %q matches %q", cases[i].src, SeenPrefix(cases[i].src), cases[i].id)
		}
	}
}

func TestWatchesSeenReadsTheSeenSet(t *testing.T) {
	dir := t.TempDir()
	body := `{"version":1,"seen":{"github:o/r@v1":"2026-10-17T01:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, SeenFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	route := config.ReleaseWatchRoute{Name: "r", Sources: []config.ReleaseWatchSource{{GitHub: "o/r"}}}
	w := NewWatches(config.ReleaseWatchConfig{StateDir: dir, Routes: []config.ReleaseWatchRoute{route}})
	seen, err := w.Seen(context.Background())
	if err != nil {
		t.Fatalf("Seen() = %v, want nil", err)
	}
	if want := map[string]string{"github:o/r@v1": "2026-10-17T01:00:00Z"}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("Seen() = %v, want %v", seen, want)
	}
	if got := w.Routes(); !reflect.DeepEqual(got, []config.ReleaseWatchRoute{route}) {
		t.Fatalf("Routes() = %+v, want the configured route", got)
	}
}

func TestWatchesSeenBoundaries(t *testing.T) {
	empty, err := NewWatches(config.ReleaseWatchConfig{}).Seen(context.Background())
	if err != nil || len(empty) != 0 || empty == nil {
		t.Fatalf("Seen() without a state dir = %v, %v; want an empty set", empty, err)
	}
	missing, err := NewWatches(config.ReleaseWatchConfig{StateDir: t.TempDir()}).Seen(context.Background())
	if err != nil || len(missing) != 0 {
		t.Fatalf("Seen() before the first save = %v, %v; want an empty set", missing, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = NewWatches(config.ReleaseWatchConfig{StateDir: t.TempDir()}).Seen(ctx); err == nil {
		t.Fatal("Seen() on a cancelled context = nil, want an error")
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, SeenFileName), []byte(`{"version":9,"seen":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = NewWatches(config.ReleaseWatchConfig{StateDir: dir}).Seen(context.Background()); err == nil || !strings.Contains(err.Error(), "version 9") {
		t.Fatalf("Seen() of a version 9 seen-set = %v, want a version error", err)
	}
}
