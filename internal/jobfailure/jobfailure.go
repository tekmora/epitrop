package jobfailure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/tekmora/epitrop/internal/broker"
	"github.com/tekmora/epitrop/internal/eventlog"
)

// ReplayFunc returns the records after seq after, oldest first.
type ReplayFunc func(ctx context.Context, after uint64) ([]eventlog.Record, error)

// HeadFunc returns the seq of the newest record.
type HeadFunc func(ctx context.Context) (uint64, error)

// AppendFunc appends the event check returns, with check run under the log lock (eventlog.Writer.AppendChecked).
type AppendFunc func(ctx context.Context, check func(context.Context) (eventlog.Event, error)) (eventlog.Record, error)

// OutputFunc returns the tail of the named job's output, at most MaxOutputBytes; "" when none.
type OutputFunc func(ctx context.Context, job string) (string, error)

const (
	// MaxOutputBytes is the maximum number of bytes read from a job's output log.
	MaxOutputBytes = 16 << 10

	// RecordType is the event type for dead-lettered job failures.
	RecordType = "job.dead_lettered"
)

var errAlreadyRecorded = errors.New("jobfailure: already recorded")

// Watcher watches the event log for failed job exits and records dead letters.
type Watcher struct {
	dl            *broker.DeadLetters
	replay        ReplayFunc
	head          HeadFunc
	appendFn      AppendFunc
	output        OutputFunc
	cursor        uint64
	attempts      map[string]int
	initialized   bool
	compactLogged bool
	mu            sync.Mutex
}

// New constructs a Watcher from the given dependencies.
func New(dl *broker.DeadLetters, replay ReplayFunc, head HeadFunc, appendFn AppendFunc, output OutputFunc) (*Watcher, error) {
	if dl == nil {
		return nil, errors.New("jobfailure: dead letters is required")
	}
	if replay == nil {
		return nil, errors.New("jobfailure: replay function is required")
	}
	if head == nil {
		return nil, errors.New("jobfailure: head function is required")
	}
	if appendFn == nil {
		return nil, errors.New("jobfailure: append function is required")
	}
	if output == nil {
		return nil, errors.New("jobfailure: output function is required")
	}
	return &Watcher{
		dl:       dl,
		replay:   replay,
		head:     head,
		appendFn: appendFn,
		output:   output,
		attempts: make(map[string]int),
	}, nil
}

// Poll handles the records after the cursor. First call: replay from 0 to find the cursor and attempt counts
// (see below); later calls replay after the cursor.
func (w *Watcher) Poll(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.initialized {
		return w.init(ctx)
	}
	return w.pollLater(ctx)
}

// Run polls every interval until ctx ends; a poll error is logged with slog and the loop goes on. Bounded: stops at ctx end.
func (w *Watcher) Run(ctx context.Context, interval time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := w.Poll(ctx); err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "jobfailure: poll failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Watcher) init(ctx context.Context) error {
	recs, err := w.replay(ctx, 0)
	if err != nil {
		if errors.Is(err, eventlog.ErrCursorCompacted) {
			return w.handleCompacted(ctx)
		}
		return fmt.Errorf("jobfailure: initial replay from 0: %w", err)
	}

	maxExitedSeq, hasDeadLettered := findMaxExitedSeq(recs)
	targetCursor := maxExitedSeq
	if !hasDeadLettered {
		head, hErr := w.head(ctx)
		if hErr != nil {
			return fmt.Errorf("jobfailure: initial head: %w", hErr)
		}
		targetCursor = head
	}

	w.cursor = targetCursor
	w.initialized = true
	return w.processInitialRecords(ctx, recs, targetCursor)
}

func (w *Watcher) handleCompacted(ctx context.Context) error {
	head, err := w.head(ctx)
	if err != nil {
		return fmt.Errorf("jobfailure: head after compaction: %w", err)
	}
	w.cursor = head
	w.attempts = make(map[string]int)
	w.initialized = true
	if !w.compactLogged {
		slog.WarnContext(ctx, "jobfailure: log compacted below base; cursor started at head", "head", head)
		w.compactLogged = true
	}
	return nil
}

func (w *Watcher) processInitialRecords(ctx context.Context, recs []eventlog.Record, targetCursor uint64) error {
	for _, rec := range recs {
		if rec.Seq <= targetCursor {
			if err := w.updateAttemptOnly(rec); err != nil {
				return err
			}
			continue
		}
		if err := w.handleRecord(ctx, rec); err != nil {
			return err
		}
		if rec.Seq > w.cursor {
			w.cursor = rec.Seq
		}
	}
	return nil
}

func (w *Watcher) pollLater(ctx context.Context) error {
	recs, err := w.replay(ctx, w.cursor)
	if err != nil {
		if errors.Is(err, eventlog.ErrCursorCompacted) {
			return w.handleCompacted(ctx)
		}
		return fmt.Errorf("jobfailure: replay after %d: %w", w.cursor, err)
	}

	for _, rec := range recs {
		if err := w.handleRecord(ctx, rec); err != nil {
			return err
		}
		if rec.Seq > w.cursor {
			w.cursor = rec.Seq
		}
	}
	return nil
}

func (w *Watcher) updateAttemptOnly(rec eventlog.Record) error {
	switch rec.Type {
	case "job.stopped":
		w.attempts[rec.TaskID] = 0
	case "job.exited":
		failed, _, _, err := parseExited(rec)
		if err != nil {
			return err
		}
		if failed {
			w.attempts[rec.TaskID]++
		} else {
			w.attempts[rec.TaskID] = 0
		}
	}
	return nil
}

func (w *Watcher) handleRecord(ctx context.Context, rec eventlog.Record) error {
	switch rec.Type {
	case "job.stopped":
		w.attempts[rec.TaskID] = 0
	case "job.exited":
		failed, code, reason, err := parseExited(rec)
		if err != nil {
			return err
		}
		if !failed {
			w.attempts[rec.TaskID] = 0
			return nil
		}
		// The count moves only once the verdict is in the log, so a retried record counts once.
		attempt := w.attempts[rec.TaskID] + 1
		if err := w.handleFailedExit(ctx, rec, attempt, code, reason); err != nil {
			return err
		}
		w.attempts[rec.TaskID] = attempt
	}
	return nil
}

func (w *Watcher) handleFailedExit(ctx context.Context, rec eventlog.Record, attempt int, code *int, reason string) error {
	job := rec.TaskID
	out, err := w.output(ctx, job)
	if err != nil {
		return fmt.Errorf("jobfailure: output for %s: %w", job, err)
	}
	text := buildFailureText(code, reason, out)

	_, appendErr := w.appendFn(ctx, func(cctx context.Context) (eventlog.Event, error) {
		return w.checkAndBuildEvent(cctx, rec, attempt, text, code, reason)
	})
	if appendErr != nil {
		if errors.Is(appendErr, errAlreadyRecorded) {
			return nil
		}
		return fmt.Errorf("jobfailure: append %s: %w", job, appendErr)
	}
	return nil
}

func (w *Watcher) checkAndBuildEvent(cctx context.Context, rec eventlog.Record, attempt int, text string, code *int, reason string) (eventlog.Event, error) {
	checkRecs, rErr := w.replay(cctx, rec.Seq)
	if rErr != nil {
		return eventlog.Event{}, fmt.Errorf("jobfailure: check replay: %w", rErr)
	}
	if isAlreadyRecorded(checkRecs, rec.Seq) {
		return eventlog.Event{}, errAlreadyRecorded
	}
	verdict, routeErr := w.dl.Route(cctx, broker.Failure{
		Unit:    rec.TaskID,
		Text:    text,
		Attempt: attempt,
	})
	if routeErr != nil && !errors.Is(routeErr, broker.ErrFull) {
		return eventlog.Event{}, fmt.Errorf("jobfailure: route: %w", routeErr)
	}
	parked := !verdict.Retry && routeErr == nil
	evPayload, mErr := buildDeadLetteredPayload(rec.Seq, verdict.Class, verdict.Retry, parked, attempt, code, reason)
	if mErr != nil {
		return eventlog.Event{}, mErr
	}
	return eventlog.Event{
		Type:    RecordType,
		TaskID:  rec.TaskID,
		Payload: evPayload,
	}, nil
}

func isAlreadyRecorded(recs []eventlog.Record, exitedSeq uint64) bool {
	for _, cr := range recs {
		if cr.Type != RecordType {
			continue
		}
		var p struct {
			ExitedSeq uint64 `json:"exited_seq"`
		}
		if err := json.Unmarshal(cr.Payload, &p); err == nil && p.ExitedSeq == exitedSeq {
			return true
		}
	}
	return false
}

func findMaxExitedSeq(recs []eventlog.Record) (uint64, bool) {
	var maxSeq uint64
	var found bool
	for _, rec := range recs {
		if rec.Type == RecordType {
			var p struct {
				ExitedSeq uint64 `json:"exited_seq"`
			}
			if err := json.Unmarshal(rec.Payload, &p); err == nil {
				if !found || p.ExitedSeq > maxSeq {
					maxSeq = p.ExitedSeq
					found = true
				}
			}
		}
	}
	return maxSeq, found
}

func parseExited(rec eventlog.Record) (bool, *int, string, error) {
	var payload struct {
		Code   *int   `json:"code"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		return false, nil, "", fmt.Errorf("jobfailure: unmarshal seq %d payload: %w", rec.Seq, err)
	}
	failed := payload.Reason != "" || payload.Code == nil || *payload.Code != 0
	return failed, payload.Code, payload.Reason, nil
}

func buildFailureText(code *int, reason string, out string) string {
	var b strings.Builder
	if code != nil {
		fmt.Fprintf(&b, "exit code %d\n", *code)
	} else {
		b.WriteString("exit code unknown\n")
	}
	if reason != "" {
		fmt.Fprintf(&b, "reason %s\n", reason)
	}
	b.WriteString(out)
	return b.String()
}

type deadLetteredPayload struct {
	ExitedSeq uint64 `json:"exited_seq"`
	Class     string `json:"class"`
	Retry     bool   `json:"retry"`
	Parked    bool   `json:"parked"`
	Attempt   int    `json:"attempt"`
	Code      *int   `json:"code,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func buildDeadLetteredPayload(exitedSeq uint64, class string, retry bool, parked bool, attempt int, code *int, reason string) ([]byte, error) {
	p := deadLetteredPayload{
		ExitedSeq: exitedSeq,
		Class:     class,
		Retry:     retry,
		Parked:    parked,
		Attempt:   attempt,
		Code:      code,
		Reason:    reason,
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("jobfailure: marshal dead_lettered payload: %w", err)
	}
	return b, nil
}
