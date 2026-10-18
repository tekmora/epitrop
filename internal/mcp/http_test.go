package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/supervisor"
	"github.com/tekmora/epitrop/internal/units"
)

// gatedUnits fails every replay with err while err is set.
type gatedUnits struct {
	UnitReader
	err error
}

func (g *gatedUnits) Replay(ctx context.Context) (eventlog.State, error) {
	if g.err != nil {
		return eventlog.State{}, g.err
	}
	return g.UnitReader.Replay(ctx)
}

func probe(t *testing.T, base, path string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(testContext(t), http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("NewRequest(%s) = %v", path, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s = %v", path, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("Close(%s) = %v", path, err)
	}
	return resp.StatusCode
}

func TestReadyzFollowsTheReplay(t *testing.T) {
	gate := &gatedUnits{UnitReader: testUnits(t)}
	h := NewHTTP("test", Sources{Jobs: &fakeJobs{}, Units: gate}, nil)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	steps := []struct {
		name    string
		poll    bool
		failing error
		want    int
	}{
		{"before any replay", false, nil, http.StatusServiceUnavailable},
		{"replay failing", true, errors.New("planted replay failure"), http.StatusServiceUnavailable},
		{"replay succeeded", true, nil, http.StatusOK},
		{"replay failing again", true, errors.New("planted replay failure"), http.StatusServiceUnavailable},
	}
	for _, st := range steps {
		gate.err = st.failing
		if st.poll {
			h.Poll(testContext(t))
		}
		if got := probe(t, srv.URL, "/readyz"); got != st.want {
			t.Fatalf("%s: /readyz = %d, want %d", st.name, got, st.want)
		}
	}
	if got := probe(t, srv.URL, "/livez"); got != http.StatusOK {
		t.Fatalf("/livez = %d, want 200 while replays fail", got)
	}
}

// eventStream subscribes to the event stream and returns a function that reads the next
// event's name and data.
func eventStream(t *testing.T, base string) func() (string, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(testContext(t))
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+PathEvents, nil)
	if err != nil {
		t.Fatalf("NewRequest = %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s = %v", PathEvents, err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("Close(%s) = %v", PathEvents, err)
		}
	})
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	lines := bufio.NewScanner(resp.Body)
	return func() (string, string) {
		t.Helper()
		var name, data string
		for lines.Scan() {
			line := lines.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "" && name != "":
				return name, data
			}
		}
		t.Fatalf("event stream ended: %v", lines.Err())
		return "", ""
	}
}

func TestEventsPublishOnlyWhatChanged(t *testing.T) {
	svc := testUnits(t)
	jobs := &fakeJobs{statuses: []supervisor.JobStatus{{Name: "idle", State: "dead"}}}
	h := NewHTTP("test", Sources{Jobs: jobs, Units: svc}, nil)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	next := eventStream(t, srv.URL)
	ctx := testContext(t)
	record(t, svc, "tekmora/epitrop#1", "implementing")

	h.Poll(ctx) // baseline: publishes nothing
	h.Poll(ctx) // nothing changed: publishes nothing
	record(t, svc, "tekmora/epitrop#2", "implementing")
	h.Poll(ctx)
	if unit := nextOf[UnitSummary](t, next, EventUnit); unit.ID != "tekmora/epitrop#2" {
		t.Fatalf("first event = %+v, want only the new unit #2", unit)
	}

	jobs.statuses = []supervisor.JobStatus{{Name: "idle", State: "running", PID: 42}}
	record(t, svc, "tekmora/epitrop#1", "review")
	h.Poll(ctx)
	if unit := nextOf[UnitSummary](t, next, EventUnit); unit.ID != "tekmora/epitrop#1" || unit.Status != "review" {
		t.Fatalf("second event = %+v, want unit #1 in review", unit)
	}
	if job := nextOf[Job](t, next, EventJob); job.State != "running" || job.PID != 42 {
		t.Fatalf("third event = %+v, want the idle job running", job)
	}
}

// nextOf reads the next event, which must be named want, and decodes its data.
func nextOf[T any](t *testing.T, next func() (string, string), want string) T {
	t.Helper()
	var out T
	name, data := next()
	if name != want {
		t.Fatalf("event %s %s, want a %s event", name, data, want)
	}
	if err := json.Unmarshal([]byte(data), &out); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return out
}

func record(t *testing.T, svc *units.Service, id, status string) {
	t.Helper()
	if _, err := svc.Record(testContext(t), id, units.RecordPayload{Status: status}); err != nil {
		t.Fatalf("Record(%s) = %v", id, err)
	}
}

func TestWatchEndsWithContext(t *testing.T) {
	h := NewHTTP("test", Sources{Jobs: &fakeJobs{}, Units: testUnits(t)}, nil)
	ctx, cancel := context.WithCancel(testContext(t))
	done := make(chan struct{})
	go func() { h.Watch(ctx, time.Millisecond); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Watch did not return after its context ended")
	}
}

func TestStreamableHTTPReadsBack(t *testing.T) {
	svc := testUnits(t)
	record(t, svc, "tekmora/epitrop#35", "implementing")
	srv := httptest.NewServer(NewHTTP("test", Sources{Jobs: &fakeJobs{}, Units: svc}, nil).Handler())
	t.Cleanup(srv.Close)
	ctx := testContext(t)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: srv.URL + PathMCP}, nil)
	if err != nil {
		t.Fatalf("Connect() = %v", err)
	}
	t.Cleanup(func() { closeSession(t, "client", cs.Close) })
	var out UnitStatusOut
	call(t, cs, ToolUnitStatus, map[string]any{"id": "tekmora/epitrop#35"}, &out)
	if out.ID != "tekmora/epitrop#35" || out.Status != "implementing" {
		t.Fatalf("unit_status over HTTP = %+v, want the recorded unit", out)
	}
}

func TestStreamableHTTPRefusesAForeignHost(t *testing.T) {
	srv := httptest.NewServer(NewHTTP("test", Sources{Jobs: &fakeJobs{}, Units: testUnits(t)}, nil).Handler())
	t.Cleanup(srv.Close)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"t"}}}`
	req, err := http.NewRequestWithContext(testContext(t), http.MethodPost, srv.URL+PathMCP, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest = %v", err)
	}
	req.Host = "attacker.example"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST with Host attacker.example = %d, want 403", resp.StatusCode)
	}
}
