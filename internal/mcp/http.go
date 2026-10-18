package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/golusoris/golusoris/k8s/health"
	"github.com/golusoris/golusoris/observability/statuspage"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tekmora/epitrop/internal/broker"
	"github.com/tekmora/epitrop/internal/eventlog"
)

// Paths the HTTP transport serves beside the health probes /livez, /readyz and /startupz.
// They are provisional until the first release that ships the server.
const (
	PathMCP    = "/mcp"
	PathEvents = "/events"
)

// ReplayCheck is the readiness check that passes once the event log has replayed.
const ReplayCheck = "event-log-replay"

const (
	// writeTimeout bounds each event write, so a stalled client is dropped.
	writeTimeout = 10 * time.Second
	// maxStreamEvents bounds one stream (HISS-02).
	maxStreamEvents = 1 << 31
)

var errNotReplayed = errors.New("the event log has not replayed yet")

// HTTP serves the tools over streamable HTTP, the health probes, and an event stream of
// the signed event log. Watch drives readiness and polls the event tail.
type HTTP struct {
	src    Sources
	log    *slog.Logger
	mux    *http.ServeMux
	replay atomic.Pointer[error]
}

// NewHTTP returns the handler set over src. Readiness fails until Watch has replayed the
// event log; log takes the errors no client sees, and nil discards them.
func NewHTTP(version string, src Sources, log *slog.Logger) *HTTP {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	h := &HTTP{src: src, log: log, mux: http.NewServeMux()}
	h.replay.Store(&errNotReplayed)
	srv := NewServer(version, src)
	h.mux.Handle(PathMCP, sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil))
	h.mux.HandleFunc(PathEvents, h.events)
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
// replay: it passes once one succeeds and fails again while replays fail. Each pass polls
// the event tail to publish new records.
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

// Poll replays once for readiness and polls the event tail, which publishes the records
// appended since its last poll.
func (h *HTTP) Poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	_, err := h.src.Units.Replay(ctx)
	if err != nil {
		err = fmt.Errorf("replay: %w", err)
		h.replay.Store(&err)
		h.log.WarnContext(ctx, "epitrop-server: event log replay failed", slog.String("error", err.Error()))
		return
	}
	// The tail polls before readiness passes, so a client that saw /readyz pass finds the
	// tail started and misses no record appended after it.
	if h.src.Events != nil {
		if err := h.src.Events.Poll(ctx); err != nil {
			h.log.WarnContext(ctx, "epitrop-server: event tail poll failed", slog.String("error", err.Error()))
		}
	}
	var ok error
	h.replay.Store(&ok)
}

// events streams one server-sent event per record: id is the record's seq, event its
// type, data the record. A Last-Event-ID header resumes after that seq. Refusals come
// before the first body byte with a status a browser EventSource does not retry.
func (h *HTTP) events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.src.Events == nil {
		http.Error(w, "events unavailable", http.StatusServiceUnavailable)
		return
	}
	after, err := parseLastEventID(r)
	if err != nil {
		http.Error(w, "bad Last-Event-ID", http.StatusBadRequest)
		return
	}
	resumed, cancel, err := h.src.Events.Resume(r.Context(), after)
	if err != nil {
		h.mapResumeError(r.Context(), w, err)
		return
	}
	defer cancel()
	h.streamEvents(r.Context(), w, resumed)
}

// parseLastEventID reads the Last-Event-ID header: nil when absent or empty, else a
// decimal seq without sign or spaces.
func parseLastEventID(r *http.Request) (*uint64, error) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		return nil, nil
	}
	val, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("mcp: parse Last-Event-ID: %w", err)
	}
	return &val, nil
}

// mapResumeError answers a refused resume: 503 before the tail started, 400 for a
// cursor past it, 410 for a cursor a compaction passed, and 500 for the rest.
func (h *HTTP) mapResumeError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, broker.ErrNotStarted):
		http.Error(w, "events not started", http.StatusServiceUnavailable)
	case errors.Is(err, broker.ErrCursorAhead):
		http.Error(w, "cursor ahead", http.StatusBadRequest)
	case errors.Is(err, eventlog.ErrCursorCompacted):
		http.Error(w, "cursor compacted", http.StatusGone)
	default:
		h.log.ErrorContext(ctx, "epitrop-server: resume events failed", slog.String("error", err.Error()))
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// streamEvents writes the records of resumed until ctx ends or a write fails. It returns
// after maxStreamEvents events; the client resumes with Last-Event-ID.
func (h *HTTP) streamEvents(ctx context.Context, w http.ResponseWriter, resumed *broker.Resumed) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return
	}
	for range maxStreamEvents {
		rec, err := resumed.Next(ctx)
		if err != nil {
			return
		}
		if err := writeEvent(ctx, w, rc, rec, h.log); err != nil {
			return
		}
	}
}

// writeEvent writes rec as one event under a write deadline, so a stalled client fails
// the write and frees its queue.
func writeEvent(ctx context.Context, w http.ResponseWriter, rc *http.ResponseController, rec eventlog.Record, log *slog.Logger) error {
	if strings.ContainsAny(rec.Type, "\r\n") {
		log.ErrorContext(ctx, "epitrop-server: invalid event type containing CR/LF", slog.String("type", rec.Type))
		return errors.New("mcp: invalid event type containing CR/LF")
	}
	data, err := json.Marshal(rec)
	if err != nil {
		log.ErrorContext(ctx, "epitrop-server: marshal event record", slog.String("error", err.Error()))
		return fmt.Errorf("mcp: marshal event: %w", err)
	}
	if err := rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("mcp: set write deadline: %w", err)
	}
	msg := fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", rec.Seq, rec.Type, data)
	if _, err := io.WriteString(w, msg); err != nil {
		return fmt.Errorf("mcp: write event: %w", err)
	}
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("mcp: flush event: %w", err)
	}
	return nil
}
