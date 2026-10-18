package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/golusoris/golusoris/k8s/health"
	"github.com/golusoris/golusoris/observability/statuspage"
	"github.com/golusoris/golusoris/realtime/sse"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tekmora/epitrop/internal/eventlog"
)

// Paths the HTTP transport serves beside the health probes /livez, /readyz and /startupz.
// They are provisional until the first release that ships the server.
const (
	PathMCP    = "/mcp"
	PathEvents = "/events"
)

// Event names on the event stream.
const (
	EventUnit = "unit"
	EventJob  = "job"
)

// ReplayCheck is the readiness check that passes once the event log has replayed.
const ReplayCheck = "event-log-replay"

var errNotReplayed = errors.New("the event log has not replayed yet")

// HTTP serves the tools over streamable HTTP, the health probes, and an event stream of
// the units and jobs whose state changed. Watch drives readiness and the stream.
type HTTP struct {
	src    Sources
	log    *slog.Logger
	hub    *sse.Hub
	mux    *http.ServeMux
	replay atomic.Pointer[error]

	// Owned by Watch: the last summaries published, nil until the first replay.
	units map[string]UnitSummary
	jobs  map[string]Job
}

// NewHTTP returns the handler set over src. Readiness fails until Watch has replayed the
// event log; log takes the errors no client sees, and nil discards them.
func NewHTTP(version string, src Sources, log *slog.Logger) *HTTP {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	h := &HTTP{src: src, log: log, hub: sse.NewHub(log), mux: http.NewServeMux()}
	h.replay.Store(&errNotReplayed)
	srv := NewServer(version, src)
	h.mux.Handle(PathMCP, sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil))
	h.mux.Handle(PathEvents, h.hub.Handler())
	reg := statuspage.NewRegistry(nil)
	reg.Register(statuspage.Check{Name: ReplayCheck, Tags: []string{health.TagReadiness}, Fn: h.replayed})
	health.MountMux(h.mux, reg)
	return h
}

// Handler serves every path.
func (h *HTTP) Handler() http.Handler { return h.mux }

func (h *HTTP) replayed(context.Context) error {
	if err := *h.replay.Load(); err != nil {
		return err
	}
	return nil
}

// Watch replays the event log every interval until ctx ends. Readiness follows the last
// replay: it passes once one succeeds and fails again while replays fail. The first
// replay sets the state the stream starts from; each later one publishes an event for
// every unit and job whose summary changed. A client that misses an event reads the
// state back with the tools.
func (h *HTTP) Watch(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		h.Poll(ctx)
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

// Poll replays once and publishes what changed since the last replay.
func (h *HTTP) Poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	st, err := h.src.Units.Replay(ctx)
	if err != nil {
		err = fmt.Errorf("replay: %w", err)
		h.replay.Store(&err)
		h.log.WarnContext(ctx, "epitrop-server: event log replay failed", slog.String("error", err.Error()))
		return
	}
	var ok error
	h.replay.Store(&ok)
	h.units = publishChanged(ctx, h.hub, EventUnit, h.units, unitSummaries(st))
	statuses, err := h.src.Jobs.Status(ctx, "")
	if err != nil {
		h.log.DebugContext(ctx, "epitrop-server: job status unavailable", slog.String("error", err.Error()))
		return
	}
	h.jobs = publishChanged(ctx, h.hub, EventJob, h.jobs, jobsOf(statuses))
}

// publishChanged publishes every entry of next that differs from prev, in key order, and
// returns next. With prev nil it only returns next: the first state is the baseline.
func publishChanged[T comparable](ctx context.Context, hub *sse.Hub, event string, prev, next map[string]T) map[string]T {
	if prev == nil {
		return next
	}
	keys := make([]string, 0, len(next))
	for k, v := range next {
		if old, seen := prev[k]; !seen || old != v {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for i := 0; i < len(keys); i++ {
		hub.Publish(ctx, sse.Event{Event: event, Data: next[keys[i]]})
	}
	return next
}

func unitSummaries(st eventlog.State) map[string]UnitSummary {
	out := make(map[string]UnitSummary, len(st.Units))
	for id, u := range st.Units {
		out[id] = summarize(u)
	}
	return out
}
