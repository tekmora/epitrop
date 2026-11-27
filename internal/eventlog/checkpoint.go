package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
)

const (
	maxCheckpointsKept   = 3
	maxCheckpointEntries = 128
)

var validCheckpointNameRegex = regexp.MustCompile(`^[a-z]+$`)

// Checkpoint is one snapshot of reducer state after applying a specific record.
type Checkpoint struct {
	Name    string               `json:"name"`
	Seq     uint64               `json:"seq"`
	Hash    string               `json:"hash"`
	State   json.RawMessage      `json:"state"`
	Receipt *corereceipt.Receipt `json:"receipt"`
}

type unsignedCheckpoint struct {
	Name  string          `json:"name"`
	Seq   uint64          `json:"seq"`
	Hash  string          `json:"hash"`
	State json.RawMessage `json:"state"`
}

// CheckpointUse reports which checkpoint, if any, a replay started from.
type CheckpointUse struct {
	Seq      uint64      `json:"seq"`
	Skipped  []string    `json:"skipped,omitempty"`
	Diverged *Divergence `json:"diverged,omitempty"`
}

// Divergence describes a checkpoint that does not match the event log on disk.
type Divergence struct {
	Checkpoint DivergedCheckpoint `json:"checkpoint"`
	Log        DivergedLog        `json:"log"`
	File       string             `json:"-"`
}

// DivergedCheckpoint is the checkpoint that diverged from the log.
type DivergedCheckpoint struct {
	Name string `json:"name"`
	Seq  uint64 `json:"seq"`
	Hash string `json:"hash"`
}

// DivergedLog holds the log's state when divergence was detected.
type DivergedLog struct {
	Seq                 uint64 `json:"seq"`
	Hash                string `json:"hash"`
	HashAtCheckpointSeq string `json:"hash_at_checkpoint_seq"`
}

// WriteCheckpoint publishes a signed, canonical checkpoint under the log lock
// and keeps at most three checkpoints for name. An already published checkpoint
// for the same record takes precedence over a delayed ordinary write.
// Do not call WriteCheckpoint from an AppendChecked callback.
func (w *Writer) WriteCheckpoint(ctx context.Context, name string, seq uint64, hash string, st State) (err error) {
	if err := readyContext(ctx, "write checkpoint"); err != nil {
		return err
	}
	if !validCheckpointName(name) {
		return fmt.Errorf("eventlog: invalid checkpoint name %q", name)
	}
	if seq == 0 || hash == "" {
		return errors.New("eventlog: checkpoint seq and hash are required")
	}
	if err = os.MkdirAll(w.eventsDir, 0o750); err != nil {
		return fmt.Errorf("eventlog: mkdir %s: %w", w.eventsDir, err)
	}
	lock, err := acquireLock(ctx, w.eventsDir, w.limits.LockTimeout)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	if err = ctx.Err(); err != nil {
		return err
	}
	return w.writeCheckpointLocked(ctx, name, seq, hash, st)
}

// writeCheckpointLocked publishes an ordinary checkpoint without replacing an
// authenticated snapshot for the same record. The caller holds the log lock.
func (w *Writer) writeCheckpointLocked(ctx context.Context, name string, seq uint64, hash string, st State) error {
	cpDir := filepath.Join(w.dir, "checkpoints", name)
	fileName := checkpointFileName(seq)
	exists, err := w.hasMatchingCheckpoint(filepath.Join(cpDir, fileName), name, seq, hash)
	if err != nil || exists {
		return err
	}
	full, err := w.buildCheckpoint(name, seq, hash, st)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.MkdirAll(cpDir, 0o750); err != nil {
		return fmt.Errorf("eventlog: mkdir %s: %w", cpDir, err)
	}
	if err = atomicReplace(cpDir, fileName, full, 0o600); err != nil {
		return err
	}
	return pruneCheckpoints(cpDir)
}

// hasMatchingCheckpoint checks whether the existing snapshot is authenticated and
// names the same record. Missing, oversized, or invalid snapshots may be replaced.
func (w *Writer) hasMatchingCheckpoint(path, name string, seq uint64, hash string) (bool, error) {
	pub := strings.ToLower(w.signer.PublicKey())
	data, err := readFileBounded(path, w.limits.MaxRecordBytes)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrRecordTooLarge) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	cp, err := decodeCheckpoint(path, data, pub)
	return err == nil && cp.Name == name && cp.Seq == seq && cp.Hash == hash, nil
}

// RebuildCheckpoint replaces the newest usable checkpoint if accept rejects its
// state. It replays only through that checkpoint, from the authenticated base
// or genesis, and replaces it only if the reconstructed state is accepted.
// It never rewrites the base or consumes records after the checkpoint.
func (w *Writer) RebuildCheckpoint(ctx context.Context, name string, reducer Reducer, accept func(State) bool) (err error) {
	if err = readyContext(ctx, "rebuild checkpoint"); err != nil {
		return err
	}
	if !validCheckpointName(name) || reducer == nil || accept == nil {
		return errors.New("eventlog: rebuild checkpoint needs a fold name, reducer and state predicate")
	}
	if _, err = os.Stat(w.eventsDir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	lock, err := acquireLock(ctx, w.eventsDir, w.limits.LockTimeout)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	return w.rebuildCheckpointLocked(ctx, name, reducer, accept)
}

// rebuildCheckpointLocked selects and repairs a rejected snapshot without
// publishing at HEAD or pruning other snapshots. The caller holds the log lock.
func (w *Writer) rebuildCheckpointLocked(ctx context.Context, name string, reducer Reducer, accept func(State) bool) error {
	r := replayRun{
		eventsDir: w.eventsDir, checkpointsDir: filepath.Join(w.dir, "checkpoints"),
		pubKeyHex: strings.ToLower(w.signer.PublicKey()), limits: w.limits,
		name: name, fold: name,
		reducer: checkpointRebuildReducer(ctx, reducer),
	}
	b, hasBase, err := readBase(r.eventsDir, r.pubKeyHex, r.limits)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(r.eventsDir)
	if err != nil {
		return err
	}
	r.segs, err = listSegments(r.eventsDir, r.limits.MaxReplayRecords, b.Seq)
	if err != nil {
		return err
	}
	cp := r.findCheckpoint(b.Seq)
	if cp.seq == 0 {
		return nil
	}
	var old State
	if err = json.Unmarshal(cp.state, &old); err != nil {
		return fmt.Errorf("eventlog: rebuild checkpoint seq %d: %w", cp.seq, err)
	}
	if accept(old) {
		return nil
	}
	st, err := r.reconstructCheckpoint(b, hasBase, entries, cp, accept)
	if err != nil {
		return err
	}
	return w.replaceRebuiltCheckpoint(ctx, r.checkpointsDir, name, cp.seq, cp.hash, st)
}

// reconstructCheckpoint replays retained history from the base or genesis and
// returns state only if it reaches the authenticated boundary and passes accept.
func (r *replayRun) reconstructCheckpoint(b Base, hasBase bool, entries []os.DirEntry, cp checkpointResult, accept func(State) bool) (State, error) {
	if err := r.verifyCheckpointRebuildBoundary(b.Seq, cp.seq, cp.hash); err != nil {
		return State{}, err
	}
	st := State{Tasks: map[string]Task{}}
	c := cursor{state: &st}
	if err := r.start(b, hasBase, &c); err != nil {
		return State{}, err
	}
	if err := r.replayCheckpointBoundary(entries, &c, cp.seq, cp.hash); err != nil {
		return State{}, err
	}
	if !accept(st) {
		return State{}, fmt.Errorf("eventlog: retained history cannot rebuild compatible checkpoint state at seq %d", cp.seq)
	}
	return st, nil
}

// checkpointRebuildReducer checks cancellation before each reducer call.
func checkpointRebuildReducer(ctx context.Context, reducer Reducer) Reducer {
	return func(st State, rec Record) (State, error) {
		if err := ctx.Err(); err != nil {
			return State{}, err
		}
		return reducer(st, rec)
	}
}

// verifyCheckpointRebuildBoundary checks the replay budget and HEAD boundary.
func (r *replayRun) verifyCheckpointRebuildBoundary(baseSeq, seq uint64, hash string) error {
	maxRecords := r.limits.MaxReplayRecords
	if maxRecords < 0 {
		return fmt.Errorf("eventlog: negative max replay records %d rebuilding checkpoint seq %d", maxRecords, seq)
	}
	if seq < baseSeq {
		return fmt.Errorf("eventlog: checkpoint seq %d precedes base seq %d", seq, baseSeq)
	}
	if seq-baseSeq > uint64(maxRecords) {
		return fmt.Errorf("eventlog: max replay records %d exceeded rebuilding checkpoint seq %d", r.limits.MaxReplayRecords, seq)
	}
	h, hasHead, err := r.readHead()
	if err != nil {
		return err
	}
	if !hasHead || seq > h.Seq || (seq == h.Seq && hash != h.Hash) {
		return fmt.Errorf("eventlog: checkpoint seq %d does not match HEAD boundary", seq)
	}
	return nil
}

// replayCheckpointBoundary replays through the checkpoint and verifies the
// resulting sequence and hash.
func (r *replayRun) replayCheckpointBoundary(entries []os.DirEntry, c *cursor, seq uint64, hash string) error {
	r.checkpointEnd = seq
	if err := r.applySegments(looseRecords(entries), c); err != nil {
		return err
	}
	if err := r.applyRecords(entries, c); err != nil {
		return err
	}
	if c.last != seq || c.prev != hash {
		return fmt.Errorf("eventlog: rebuilt history does not match checkpoint seq %d", seq)
	}
	return nil
}

// replaceRebuiltCheckpoint signs and atomically replaces the selected snapshot.
// The caller holds the log lock.
func (w *Writer) replaceRebuiltCheckpoint(ctx context.Context, checkpointsDir, name string, seq uint64, hash string, st State) error {
	full, err := w.buildCheckpoint(name, seq, hash, st)
	if err != nil {
		return err
	}
	if len(full) > w.limits.MaxRecordBytes {
		return fmt.Errorf("%w: rebuilt checkpoint seq %d is %d bytes", ErrRecordTooLarge, seq, len(full))
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return atomicReplace(filepath.Join(checkpointsDir, name), checkpointFileName(seq), full, 0o600)
}

func (w *Writer) buildCheckpoint(name string, seq uint64, hash string, st State) ([]byte, error) {
	stateBytes, err := st.CanonicalJSON()
	if err != nil {
		return nil, fmt.Errorf("eventlog: checkpoint state: %w", err)
	}
	unsigned := unsignedCheckpoint{Name: name, Seq: seq, Hash: hash, State: stateBytes}
	unsignedBytes, err := canonicalJSON(unsigned)
	if err != nil {
		return nil, err
	}
	signed, err := w.signer.Sign(corereceipt.Run{Command: CheckpointCommand, Output: unsignedBytes})
	if err != nil {
		return nil, fmt.Errorf("eventlog: sign checkpoint %s seq %d: %w", name, seq, err)
	}
	cp := Checkpoint{Name: name, Seq: seq, Hash: hash, State: stateBytes, Receipt: signed}
	full, err := canonicalJSON(cp)
	if err != nil {
		return nil, err
	}
	return full, nil
}

// validCheckpointName reports whether name may name a checkpoint or a fold: lowercase
// letters, and never BaseName.
func validCheckpointName(name string) bool {
	return name != BaseName && validCheckpointNameRegex.MatchString(name)
}

func checkpointFileName(seq uint64) string {
	return fmt.Sprintf("%020d.json", seq)
}

func pruneCheckpoints(cpDir string) error {
	entries, err := os.ReadDir(cpDir)
	if err != nil {
		return fmt.Errorf("eventlog: read checkpoint dir %s: %w", cpDir, err)
	}
	var seqs []uint64
	for i := 0; i < len(entries) && i < maxCheckpointEntries; i++ {
		entry := entries[i]
		if !entry.IsDir() && isEventRecord(entry.Name()) {
			seq, seqErr := seqFromName(entry.Name())
			if seqErr == nil {
				seqs = append(seqs, seq)
			}
		}
	}
	sort.Slice(seqs, func(i int, j int) bool { return seqs[i] > seqs[j] })
	for i := maxCheckpointsKept; i < len(seqs); i++ {
		path := filepath.Join(cpDir, checkpointFileName(seqs[i]))
		if remErr := os.Remove(path); remErr != nil && !os.IsNotExist(remErr) {
			return fmt.Errorf("eventlog: prune checkpoint %s: %w", path, remErr)
		}
	}
	return nil
}

func verifyCheckpointReceipt(cp Checkpoint, pubKeyHex string, path string) error {
	if cp.Receipt == nil {
		return fmt.Errorf("checkpoint %s: missing receipt", path)
	}
	if cp.Receipt.Command != CheckpointCommand {
		return fmt.Errorf("checkpoint %s: receipt command %q, want %q", path, cp.Receipt.Command, CheckpointCommand)
	}
	if err := ensureExpectedPublicKey(cp.Receipt.PublicKey, pubKeyHex, path); err != nil {
		return err
	}
	unsigned := unsignedCheckpoint{Name: cp.Name, Seq: cp.Seq, Hash: cp.Hash, State: cp.State}
	unsignedBytes, err := canonicalJSON(unsigned)
	if err != nil {
		return fmt.Errorf("checkpoint %s: unsigned canonical JSON: %w", path, err)
	}
	if err = corereceipt.VerifyOutput(cp.Receipt, unsignedBytes); err != nil {
		return fmt.Errorf("checkpoint %s: verify receipt: %w", path, err)
	}
	return nil
}

func decodeCheckpoint(path string, data []byte, pubKeyHex string) (Checkpoint, error) {
	if err := verifyCanonical(path, data); err != nil {
		return Checkpoint{}, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return Checkpoint{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := verifyCheckpointReceipt(cp, pubKeyHex, path); err != nil {
		return Checkpoint{}, err
	}
	return cp, nil
}
