package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tekmora/epitrop/internal/broker"
	"github.com/tekmora/epitrop/internal/eventlog"
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

func assertHeader(t *testing.T, resp *http.Response, key, want string) {
	t.Helper()
	if got := resp.Header.Get(key); got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
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

func fakeTail(t *testing.T, replay broker.ReplayFunc, head broker.HeadFunc) *broker.Tail {
	t.Helper()
	b, err := broker.New(16)
	if err != nil {
		t.Fatalf("broker.New = %v", err)
	}
	tail, err := broker.NewTail(b, replay, head)
	if err != nil {
		t.Fatalf("broker.NewTail = %v", err)
	}
	return tail
}

func TestEventsRefusesNonGet(t *testing.T) {
	h := NewHTTP("test", Sources{}, nil)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(testContext(t), http.MethodPost, srv.URL+PathEvents, nil)
	if err != nil {
		t.Fatalf("NewRequest = %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /events = %d, want 405", resp.StatusCode)
	}
}

func TestEventsNilTailReturns503(t *testing.T) {
	h := NewHTTP("test", Sources{}, nil)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	if got := probe(t, srv.URL, PathEvents); got != http.StatusServiceUnavailable {
		t.Fatalf("GET /events with nil tail = %d, want 503", got)
	}
}

func TestEventsNotStartedReturns503(t *testing.T) {
	tail := fakeTail(t, func(context.Context, uint64) ([]eventlog.Record, error) { return nil, nil }, func(context.Context) (uint64, error) { return 0, nil })
	h := NewHTTP("test", Sources{Events: tail}, nil)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	if got := probe(t, srv.URL, PathEvents); got != http.StatusServiceUnavailable {
		t.Fatalf("GET /events before first Poll = %d, want 503", got)
	}
}

func TestEventsMalformedLastEventIDReturns400(t *testing.T) {
	tail := fakeTail(t, func(context.Context, uint64) ([]eventlog.Record, error) { return nil, nil }, func(context.Context) (uint64, error) { return 5, nil })
	if err := tail.Poll(testContext(t)); err != nil {
		t.Fatalf("Poll = %v", err)
	}
	h := NewHTTP("test", Sources{Events: tail}, nil)

	cases := []string{"abc", "-1", "1 ", "18446744073709551616"}
	for _, tc := range cases {
		req := httptest.NewRequestWithContext(testContext(t), http.MethodGet, PathEvents, nil)
		req.Header.Set("Last-Event-ID", tc)
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("Last-Event-ID %q = %d, want 400", tc, rec.Code)
		}
	}
}

func TestEventsCursorAheadReturns400(t *testing.T) {
	tail := fakeTail(t, func(context.Context, uint64) ([]eventlog.Record, error) { return nil, nil }, func(context.Context) (uint64, error) { return 5, nil })
	if err := tail.Poll(testContext(t)); err != nil {
		t.Fatalf("Poll = %v", err)
	}
	h := NewHTTP("test", Sources{Events: tail}, nil)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(testContext(t), http.MethodGet, srv.URL+PathEvents, nil)
	if err != nil {
		t.Fatalf("NewRequest = %v", err)
	}
	req.Header.Set("Last-Event-ID", "10")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Last-Event-ID 10 with pos 5 = %d, want 400", resp.StatusCode)
	}
}

func TestEventsCompactedReturns410(t *testing.T) {
	tail := fakeTail(t, func(context.Context, uint64) ([]eventlog.Record, error) {
		return nil, eventlog.ErrCursorCompacted
	}, func(context.Context) (uint64, error) { return 5, nil })
	if err := tail.Poll(testContext(t)); err != nil {
		t.Fatalf("Poll = %v", err)
	}
	h := NewHTTP("test", Sources{Events: tail}, nil)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(testContext(t), http.MethodGet, srv.URL+PathEvents, nil)
	if err != nil {
		t.Fatalf("NewRequest = %v", err)
	}
	req.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("compacted replay = %d, want 410", resp.StatusCode)
	}
}

func TestEventsWireFormatExactBytes(t *testing.T) {
	rec := eventlog.Record{
		Seq:     1,
		Prev:    "0000",
		Time:    "2026-10-18T10:00:00Z",
		Type:    "unit.recorded",
		TaskID:  "u1",
		Payload: []byte(`{"status":"implementing"}`),
	}
	tail := fakeTail(t, func(context.Context, uint64) ([]eventlog.Record, error) {
		return []eventlog.Record{rec}, nil
	}, func(context.Context) (uint64, error) { return 1, nil })
	if err := tail.Poll(testContext(t)); err != nil {
		t.Fatalf("Poll = %v", err)
	}
	h := NewHTTP("test", Sources{Events: tail}, nil)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+PathEvents, nil)
	if err != nil {
		t.Fatalf("NewRequest = %v", err)
	}
	req.Header.Set("Last-Event-ID", "0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("Close = %v", err)
		}
	})

	assertHeader(t, resp, "Content-Type", "text/event-stream")
	assertHeader(t, resp, "Cache-Control", "no-cache")
	assertHeader(t, resp, "X-Accel-Buffering", "no")

	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal = %v", err)
	}
	want := fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", rec.Seq, rec.Type, data)
	gotBytes := make([]byte, len(want))
	if _, err := io.ReadFull(resp.Body, gotBytes); err != nil {
		t.Fatalf("ReadFull = %v", err)
	}
	if string(gotBytes) != want {
		t.Fatalf("wire bytes = %q, want %q", string(gotBytes), want)
	}
}

func TestEventsRejectsCRLFInType(t *testing.T) {
	rec := eventlog.Record{
		Seq:    1,
		Type:   "unit\r\ninjected",
		TaskID: "u1",
	}
	tail := fakeTail(t, func(context.Context, uint64) ([]eventlog.Record, error) {
		return []eventlog.Record{rec}, nil
	}, func(context.Context) (uint64, error) { return 1, nil })
	if err := tail.Poll(testContext(t)); err != nil {
		t.Fatalf("Poll = %v", err)
	}
	h := NewHTTP("test", Sources{Events: tail}, nil)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(testContext(t), http.MethodGet, srv.URL+PathEvents, nil)
	if err != nil {
		t.Fatalf("NewRequest = %v", err)
	}
	req.Header.Set("Last-Event-ID", "0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("Close = %v", err)
		}
	})

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll = %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("body for CR/LF event type = %q, want empty", string(body))
	}
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
