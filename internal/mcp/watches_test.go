package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/releasewatch"
)

// testWatches configures two routes, one with a GitHub issue sink and an ntfy sink, and
// writes a seen-set holding releases of both sources of the first and of a neighbour.
func testWatches(t *testing.T) releasewatch.Watches {
	t.Helper()
	dir := t.TempDir()
	seen := `{"version":1,"seen":{
		"github:o/r@v1":"2026-10-17T01:00:00Z",
		"github:o/r@v2":"2026-10-17T02:00:00Z",
		"github:o/r@v3":"2026-10-17T02:00:00Z",
		"github:o/r2@v9":"2026-10-17T09:00:00Z",
		"hf:org/m1":"2026-10-16T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, releasewatch.SeenFileName), []byte(seen), 0o600); err != nil {
		t.Fatal(err)
	}
	return releasewatch.NewWatches(config.ReleaseWatchConfig{StateDir: dir, Routes: []config.ReleaseWatchRoute{
		{
			Name:    "tools",
			Sources: []config.ReleaseWatchSource{{GitHub: "o/r"}, {HuggingFaceOrg: "org"}},
			Sinks: []config.ReleaseWatchSink{
				{GitHubIssue: &config.GitHubIssueSink{Repo: "o/tracker", Labels: []string{"release"}}},
				{Ntfy: &config.NtfySink{Topic: "secret-topic", Priority: 3}},
			},
		},
		{Name: "quiet", Sources: []config.ReleaseWatchSource{{GitHub: "o/q"}}, Sinks: []config.ReleaseWatchSink{{Ntfy: &config.NtfySink{Topic: "other-topic"}}}},
	}})
}

func TestWatchesListReadsTheRoutesInOrder(t *testing.T) {
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: testUnits(t), Watches: testWatches(t)})
	var out WatchesListOut
	res := call(t, cs, ToolWatchesList, map[string]any{}, &out)
	want := WatchesListOut{Watches: []WatchSummary{
		{Name: "tools", Sources: []string{"github:o/r", "huggingface:org"}, Sinks: []string{"github_issue", "ntfy"}},
		{Name: "quiet", Sources: []string{"github:o/q"}, Sinks: []string{"ntfy"}},
	}}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("watches_list = %+v, want %+v", out, want)
	}
	assertNoTopic(t, res.StructuredContent)
}

func TestWatchStatusCountsTheSeenReleases(t *testing.T) {
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: testUnits(t), Watches: testWatches(t)})
	var out WatchStatusOut
	res := call(t, cs, ToolWatchStatus, map[string]any{"name": "tools"}, &out)
	want := WatchStatusOut{
		Name: "tools",
		Sources: []WatchSource{
			// v2 and v3 were seen at one time; the greater id is the last.
			{Kind: "github", Name: "o/r", Seen: 3, LastSeen: "github:o/r@v3", LastSeenAt: "2026-10-17T02:00:00Z"},
			{Kind: "huggingface", Name: "org", Seen: 1, LastSeen: "hf:org/m1", LastSeenAt: "2026-10-16T00:00:00Z"},
		},
		Sinks: []WatchSink{{Kind: "github_issue", Repo: "o/tracker", Labels: []string{"release"}}, {Kind: "ntfy"}},
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("watch_status = %+v, want %+v", out, want)
	}
	assertNoTopic(t, res.StructuredContent)

	var quiet WatchStatusOut
	call(t, cs, ToolWatchStatus, map[string]any{"name": "quiet"}, &quiet)
	if len(quiet.Sources) != 1 || quiet.Sources[0].Seen != 0 || quiet.Sources[0].LastSeen != "" {
		t.Fatalf("watch_status quiet = %+v, want one source that has seen nothing", quiet)
	}
}

func TestSourceOfBreaksATieByID(t *testing.T) {
	seen := map[string]string{
		"github:o/r@v2": "2026-10-17T02:00:00Z",
		"github:o/r@v3": "2026-10-17T02:00:00Z",
		"github:o/r@v1": "2026-10-17T01:00:00Z",
	}
	// Each range over seen starts at a random entry; 64 passes leave a tie broken by map
	// order a chance of 2^-64 to go unnoticed.
	for i := 0; i < 64; i++ {
		if got := sourceOf(config.ReleaseWatchSource{GitHub: "o/r"}, seen); got.LastSeen != "github:o/r@v3" {
			t.Fatalf("sourceOf pass %d last seen = %q, want github:o/r@v3", i, got.LastSeen)
		}
	}
}

func TestWatchToolRefusals(t *testing.T) {
	cs := connect(t, Sources{Jobs: &fakeJobs{}, Units: testUnits(t), Watches: testWatches(t)})
	res := call(t, cs, ToolWatchStatus, map[string]any{"name": "nope"}, nil)
	if !res.IsError || !strings.Contains(errorText(res), `no release watch route "nope"`) {
		t.Fatalf("watch_status nope = %v %q, want a tool error naming the route", res.IsError, errorText(res))
	}

	bare := connect(t, Sources{Jobs: &fakeJobs{}, Units: testUnits(t)})
	for name, args := range map[string]map[string]any{ToolWatchesList: {}, ToolWatchStatus: {"name": "tools"}} {
		res = call(t, bare, name, args, nil)
		if !res.IsError || !strings.Contains(errorText(res), "reads no release watches") {
			t.Errorf("%s without watches = %v %q, want a tool error", name, res.IsError, errorText(res))
		}
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, releasewatch.SeenFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := releasewatch.NewWatches(config.ReleaseWatchConfig{StateDir: dir, Routes: []config.ReleaseWatchRoute{{Name: "tools", Sources: []config.ReleaseWatchSource{{GitHub: "o/r"}}}}})
	cs = connect(t, Sources{Jobs: &fakeJobs{}, Units: testUnits(t), Watches: broken})
	res = call(t, cs, ToolWatchStatus, map[string]any{"name": "tools"}, nil)
	if !res.IsError || !strings.Contains(errorText(res), "parse seen-set") {
		t.Fatalf("watch_status over a broken seen-set = %v %q, want a tool error", res.IsError, errorText(res))
	}
}

// assertNoTopic fails when an ntfy topic reaches the client.
func assertNoTopic(t *testing.T, content any) {
	t.Helper()
	body, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "topic") {
		t.Fatalf("result carries an ntfy topic: %s", body)
	}
}
