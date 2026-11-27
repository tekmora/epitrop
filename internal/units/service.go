package units

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/golusoris/golusoris/core/clock"
	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
	"github.com/jonboulle/clockwork"
	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graph"
	"github.com/tekmora/epitrop/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type Service struct {
	dir    string
	signer *corereceipt.Signer
	limits eventlog.Limits
	git    *eventlog.GitRecord
	folds  []eventlog.Fold
}

func NewService(dir string, signer *corereceipt.Signer, clk clock.Clock, limits eventlog.Limits) (*Service, error) {
	if dir == "" {
		return nil, errors.New("units: dir is required")
	}
	if signer == nil {
		return nil, errors.New("units: signer is required")
	}
	if clk == nil {
		clk = clockwork.NewRealClock()
	}
	limits.Clock = clk
	return &Service{dir: dir, signer: signer, limits: limits}, nil
}

// RecordInGit makes every checkpoint commit the log onto g.Ref (event_log.git). Call it
// before the service is used.
func (s *Service) RecordInGit(g eventlog.GitRecord) {
	s.git = &g
}

// CompactWith makes every commit RecordInGit enables compact the log to a base of folds
// (event_log.git.compact). folds must name every reader of the log, since a replay of a fold
// the base lacks fails. Call it before the service is used.
func (s *Service) CompactWith(folds []eventlog.Fold) {
	s.folds = folds
}

// Replay rebuilds the run record. A log directory that holds no events yet is an empty
// record, not an error.
func (s *Service) Replay(ctx context.Context) (eventlog.State, error) {
	st, _, err := s.ReplayCheckpointed(ctx)
	return st, err
}

// ReplayCheckpointed rebuilds the run record, using checkpoints when available.
func (s *Service) ReplayCheckpointed(ctx context.Context) (eventlog.State, eventlog.CheckpointUse, error) {
	if err := readyContext(ctx, "replay"); err != nil {
		return eventlog.State{}, eventlog.CheckpointUse{}, err
	}
	events := filepath.Join(s.dir, "events")
	if _, err := os.Stat(events); os.IsNotExist(err) {
		return eventlog.State{Units: map[string]eventlog.Unit{}}, eventlog.CheckpointUse{}, nil
	} else if err != nil {
		return eventlog.State{}, eventlog.CheckpointUse{}, fmt.Errorf("units: stat %s: %w", events, err)
	}
	st, use, err := s.replayUnits(ctx)
	if err != nil {
		return eventlog.State{}, eventlog.CheckpointUse{}, err
	}
	if use.Diverged != nil {
		if dErr := s.handleDivergence(ctx, use.Diverged); dErr != nil {
			return eventlog.State{}, eventlog.CheckpointUse{}, dErr
		}
	}
	return st, use, nil
}

func (s *Service) replayRaw(ctx context.Context) (eventlog.State, error) {
	events := filepath.Join(s.dir, "events")
	if _, err := os.Stat(events); os.IsNotExist(err) {
		return eventlog.State{Units: map[string]eventlog.Unit{}}, nil
	} else if err != nil {
		return eventlog.State{}, fmt.Errorf("units: stat %s: %w", events, err)
	}
	st, _, err := s.replayUnits(ctx)
	return st, err
}

// replayUnits uses the same checkpoint policy for public reads and checked appends.
func (s *Service) replayUnits(ctx context.Context) (eventlog.State, eventlog.CheckpointUse, error) {
	return eventlog.ReplayCheckpointedStableAccepting(ctx, s.dir, s.signer.PublicKey(), s.limits, FoldName, UnitReducer, hasAssignmentEvidence)
}

// hasAssignmentEvidence rejects snapshots that kept the assigned node but not the
// original assignment. Manual units have no node, and unranked assignments are valid.
// Replay can recover evidence only from retained records or a base that preserved it.
// If old compaction discarded the records and the base lacks the evidence, local
// replay cannot recover it. The current lane and actor may have been edited, so do
// not use them to guess the original assignment.
func hasAssignmentEvidence(st eventlog.State) bool {
	for _, u := range st.Units {
		if u.Node != "" && u.Assignment == nil {
			return false
		}
	}
	return true
}

func (s *Service) handleDivergence(ctx context.Context, div *eventlog.Divergence) error {
	body, err := json.Marshal(div)
	if err != nil {
		return fmt.Errorf("units: marshal log.diverged: %w", err)
	}
	_, _, _, err = s.appendCheckedRecord(ctx, func(eventlog.State) (eventlog.Event, error) {
		// The rename runs under the log's lock. A second replay that saw the same checkpoint
		// before the first one renamed it finds the file gone and appends nothing, so the
		// divergence is logged once.
		refused := div.File + ".refused"
		if renErr := os.Rename(div.File, refused); os.IsNotExist(renErr) {
			return eventlog.Event{}, errDivergenceLogged
		} else if renErr != nil {
			return eventlog.Event{}, fmt.Errorf("units: rename %s to %s: %w", div.File, refused, renErr)
		}
		return eventlog.Event{Type: "log.diverged", TaskID: "log", Payload: body}, nil
	})
	if errors.Is(err, errDivergenceLogged) {
		return nil
	}
	return err
}

// errDivergenceLogged says another writer has logged and renamed the diverged checkpoint.
var errDivergenceLogged = errors.New("units: diverged checkpoint already logged")

func (s *Service) appendCheckedRecord(ctx context.Context, build func(eventlog.State) (eventlog.Event, error)) (eventlog.State, eventlog.Record, string, error) {
	writer, err := eventlog.Open(s.dir, s.signer, s.limits)
	if err != nil {
		return eventlog.State{}, eventlog.Record{}, "", err
	}
	var capturedSt eventlog.State
	rec, recHash, err := writer.AppendCheckedWithHash(ctx, func(cctx context.Context) (eventlog.Event, error) {
		st, replayErr := s.replayRaw(cctx)
		if replayErr != nil {
			return eventlog.Event{}, replayErr
		}
		ev, buildErr := build(st)
		if buildErr != nil {
			return eventlog.Event{}, buildErr
		}
		capturedSt = st
		return ev, nil
	})
	if err != nil {
		return eventlog.State{}, eventlog.Record{}, "", err
	}
	nextSt, err := UnitReducer(capturedSt, rec)
	return nextSt, rec, recHash, err
}

// appendChecked opens an eventlog writer and appends the event returned by build.
// The check runs under the log's lock (#108).
func (s *Service) appendChecked(ctx context.Context, build func(eventlog.State) (eventlog.Event, error)) (eventlog.State, error) {
	st, _, _, err := s.appendCheckedRecord(ctx, build)
	return st, err
}

// Record appends one status record and returns the unit as it stands after it.
func (s *Service) Record(ctx context.Context, id string, payload RecordPayload) (u eventlog.Unit, err error) {
	ctx, span := telemetry.Default().StartSpan(ctx, "units.record", attribute.String("epitrop.unit", id))
	defer func() { finishSpan(span, err) }()
	if err = readyContext(ctx, "record"); err != nil {
		return eventlog.Unit{}, err
	}
	if !IsValidID(id) {
		return eventlog.Unit{}, fmt.Errorf("units: invalid unit id %q", id)
	}
	if err = validateRecordPayload(payload); err != nil {
		return eventlog.Unit{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return eventlog.Unit{}, fmt.Errorf("units: marshal payload: %w", err)
	}
	return s.appendUnitEvent(ctx, span, id, func(st eventlog.State) (eventlog.Event, bool, error) {
		existing, exists := st.Units[id]
		if valErr := validateRecordState(st, id, payload.Status, payload.Reopen); valErr != nil {
			return eventlog.Event{}, false, valErr
		}
		changed := !exists || existing.Status != payload.Status
		return eventlog.Event{Type: "unit.recorded", TaskID: id, Payload: body}, changed, nil
	})
}

// finishSpan records a failed call's error on its span and ends the span.
func finishSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// appendUnitEvent appends the event build returns and returns the unit as it stands after
// it. A checkpoint follows when build reports that the unit's status changed.
func (s *Service) appendUnitEvent(ctx context.Context, span trace.Span, id string, build func(eventlog.State) (eventlog.Event, bool, error)) (eventlog.Unit, error) {
	var statusChanged bool
	st, rec, recHash, err := s.appendCheckedRecord(ctx, func(st eventlog.State) (eventlog.Event, error) {
		event, changed, buildErr := build(st)
		statusChanged = changed
		return event, buildErr
	})
	if err != nil {
		return eventlog.Unit{}, err
	}
	if statusChanged {
		s.writeCheckpointSafe(ctx, span, rec.Seq, recHash, st)
	}
	return st.Units[id], nil
}

func (s *Service) writeCheckpointSafe(ctx context.Context, span trace.Span, seq uint64, hash string, st eventlog.State) {
	writer, err := eventlog.Open(s.dir, s.signer, s.limits)
	if err != nil {
		span.RecordError(err)
		telemetry.Default().CheckpointWrite(ctx, "failed")
		return
	}
	if err = writer.WriteCheckpoint(ctx, FoldName, seq, hash, st); err != nil {
		span.RecordError(err)
		telemetry.Default().CheckpointWrite(ctx, "failed")
		return
	}
	telemetry.Default().CheckpointWrite(ctx, "written")
	// A status change is a quiet point: closed ranges of records move into segments here,
	// off the append path. A failed rotation leaves the log as it was and is retried at the
	// next checkpoint.
	if _, err = writer.Rotate(ctx); err != nil {
		span.RecordError(err)
	}
	// The same quiet point commits the log to git when event_log.git is enabled. A failed
	// commit leaves the log and the ref as they were and is tried again at the next checkpoint.
	if s.git == nil {
		return
	}
	if _, err = writer.RecordInGit(ctx, *s.git); err != nil {
		span.RecordError(err)
		return
	}
	// Compaction removes only segments the commit just made holds; a failed one leaves the
	// log replayable and is tried again at the next checkpoint.
	if len(s.folds) == 0 {
		return
	}
	if _, err = writer.Compact(ctx, s.folds, *s.git); err != nil {
		span.RecordError(err)
	}
}

// Preempt marks an active unit as preempted with a reason and actor.
func (s *Service) Preempt(ctx context.Context, id string, reason string, by string) (u eventlog.Unit, err error) {
	ctx, span := telemetry.Default().StartSpan(ctx, "units.preempt", attribute.String("epitrop.unit", id))
	defer func() { finishSpan(span, err) }()
	if err = readyContext(ctx, "preempt"); err != nil {
		return eventlog.Unit{}, err
	}
	if err = validatePreemptInput(id, reason, by); err != nil {
		return eventlog.Unit{}, err
	}
	body, err := json.Marshal(PreemptPayload{Reason: reason, By: by})
	if err != nil {
		return eventlog.Unit{}, fmt.Errorf("units: marshal preempt payload: %w", err)
	}
	return s.appendUnitEvent(ctx, span, id, func(st eventlog.State) (eventlog.Event, bool, error) {
		return preemptEvent(st, id, body)
	})
}

// preemptEvent builds the preemption event of an active unit and reports whether it changes
// the unit's status.
func preemptEvent(st eventlog.State, id string, body []byte) (eventlog.Event, bool, error) {
	existing, exists := st.Units[id]
	if !exists {
		return eventlog.Event{}, false, fmt.Errorf("units: unit %q does not exist", id)
	}
	if valErr := transitionRefusal(existing.Status, string(graph.StatusBlocked), false); valErr != nil {
		if IsTerminalStatus(existing.Status) {
			return eventlog.Event{}, false, fmt.Errorf("units: %q is %s; cannot preempt", id, existing.Status)
		}
		return eventlog.Event{}, false, fmt.Errorf("units: %q: %w", id, valErr)
	}
	changed := existing.Status != string(graph.StatusBlocked)
	return eventlog.Event{Type: "unit.preempted", TaskID: id, Payload: body}, changed, nil
}

func validateRecordState(st eventlog.State, id string, status string, reopen bool) error {
	existing, exists := st.Units[id]
	if !exists {
		if len(st.Units) >= MaxUnits {
			return fmt.Errorf("units: the run record already holds %d units", MaxUnits)
		}
		return nil
	}
	if err := transitionRefusal(existing.Status, status, reopen); err != nil {
		if IsTerminalStatus(existing.Status) {
			return fmt.Errorf("units: %q is %s; pass --reopen to move it again", id, existing.Status)
		}
		return fmt.Errorf("units: %q: %w", id, err)
	}
	return nil
}

func validateNoteState(st eventlog.State, id string) error {
	unit, exists := st.Units[id]
	if !exists {
		return fmt.Errorf("units: unit %q does not exist", id)
	}
	if len(UnreadNotes(unit)) >= MaxUnreadNotes {
		return fmt.Errorf("units: %q already has %d unread notes", id, MaxUnreadNotes)
	}
	return nil
}

// Note appends a note to a unit's inbox. The unit reads it at its next status record.
func (s *Service) Note(ctx context.Context, id string, from string, text string) (eventlog.UnitNote, error) {
	if err := readyContext(ctx, "note"); err != nil {
		return eventlog.UnitNote{}, err
	}
	if err := validateNoteInput(id, from, text); err != nil {
		return eventlog.UnitNote{}, err
	}
	body, err := json.Marshal(NotePayload{From: from, Text: text})
	if err != nil {
		return eventlog.UnitNote{}, fmt.Errorf("units: marshal note payload: %w", err)
	}
	st, err := s.appendChecked(ctx, func(st eventlog.State) (eventlog.Event, error) {
		if validateErr := validateNoteState(st, id); validateErr != nil {
			return eventlog.Event{}, validateErr
		}
		return eventlog.Event{Type: "unit.note", TaskID: id, Payload: body}, nil
	})
	if err != nil {
		return eventlog.UnitNote{}, err
	}
	notes := st.Units[id].Notes
	if len(notes) == 0 {
		return eventlog.UnitNote{}, fmt.Errorf("units: note for %q was not recorded", id)
	}
	return notes[len(notes)-1], nil
}

var errNothingUnread = errors.New("units: nothing unread")

// Inbox returns the unread notes of a unit and marks them read. Nothing unread appends
// nothing.
func (s *Service) Inbox(ctx context.Context, id string) ([]eventlog.UnitNote, error) {
	if err := readyContext(ctx, "inbox"); err != nil {
		return nil, err
	}
	if !IsValidID(id) {
		return nil, fmt.Errorf("units: invalid unit id %q", id)
	}
	var unread []eventlog.UnitNote
	_, err := s.appendChecked(ctx, func(st eventlog.State) (eventlog.Event, error) {
		unit, exists := st.Units[id]
		if !exists {
			return eventlog.Event{}, fmt.Errorf("units: unit %q does not exist", id)
		}
		notes := UnreadNotes(unit)
		if len(notes) == 0 {
			return eventlog.Event{}, errNothingUnread
		}
		body, marshalErr := json.Marshal(NoteReadPayload{UptoSeq: notes[len(notes)-1].Seq})
		if marshalErr != nil {
			return eventlog.Event{}, fmt.Errorf("units: marshal note read payload: %w", marshalErr)
		}
		unread = notes
		return eventlog.Event{Type: "unit.note_read", TaskID: id, Payload: body}, nil
	})
	if errors.Is(err, errNothingUnread) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return unread, nil
}

// Show returns one unit as the log records it.
func (s *Service) Show(ctx context.Context, id string) (eventlog.Unit, error) {
	if err := readyContext(ctx, "show"); err != nil {
		return eventlog.Unit{}, err
	}
	if !IsValidID(id) {
		return eventlog.Unit{}, fmt.Errorf("units: invalid unit id %q", id)
	}
	st, err := s.Replay(ctx)
	if err != nil {
		return eventlog.Unit{}, err
	}
	unit, exists := st.Units[id]
	if !exists {
		return eventlog.Unit{}, fmt.Errorf("units: unit %q does not exist", id)
	}
	return unit, nil
}

// Resume lists the open units, sorted by id, each with whether it is safe to relaunch,
// and the number of unit records replay did not apply.
func (s *Service) Resume(ctx context.Context) ([]ResumeRow, int, error) {
	if err := readyContext(ctx, "resume"); err != nil {
		return nil, 0, err
	}
	st, err := s.Replay(ctx)
	if err != nil {
		return nil, 0, err
	}
	openIDs := make([]string, 0, len(st.Units))
	for id, u := range st.Units {
		if !IsTerminalStatus(u.Status) {
			openIDs = append(openIDs, id)
		}
	}
	sort.Strings(openIDs)
	rows := make([]ResumeRow, 0, len(openIDs))
	for i := 0; i < len(openIDs); i++ {
		u := st.Units[openIDs[i]]
		relaunch := "safe"
		if safe, reason := RelaunchRule(u); !safe {
			relaunch = "no: " + reason
		}
		evidence := ""
		if u.Evidence != nil {
			evidence = u.Evidence.Path
		}
		rows = append(rows, ResumeRow{ID: u.ID, Status: u.Status, UpdatedAt: u.UpdatedAt, Worktree: u.Worktree,
			Branch: u.Branch, PR: u.PR, Lane: u.Lane, Relaunch: relaunch, Evidence: evidence,
			Node: u.Node, By: u.By, Assignment: u.Assignment})
	}
	return rows, st.UnitRecordsIgnored, nil
}

func readyContext(ctx context.Context, op string) error {
	if ctx == nil {
		return fmt.Errorf("units: %s: nil context", op)
	}
	if _, ok := ctx.Deadline(); !ok {
		return fmt.Errorf("units: %s requires a context deadline", op)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("units: %s context: %w", op, err)
	}
	return nil
}
