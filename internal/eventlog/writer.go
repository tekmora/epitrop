package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
)

// AppendCheckedWithHash calls check under the event-log lock, then appends the event check returns.
// It returns the record and its content hash.
func (w *Writer) AppendCheckedWithHash(ctx context.Context, check func(context.Context) (Event, error)) (Record, string, error) {
	if err := readyContext(ctx, "append"); err != nil {
		return Record{}, "", err
	}
	if check == nil {
		return Record{}, "", errors.New("eventlog: check function is required")
	}
	if err := os.MkdirAll(w.eventsDir, 0o750); err != nil {
		return Record{}, "", fmt.Errorf("eventlog: mkdir %s: %w", w.eventsDir, err)
	}
	lock, err := acquireLock(ctx, w.eventsDir, w.limits.LockTimeout)
	if err != nil {
		return Record{}, "", err
	}
	current, err := w.currentHead()
	if err != nil {
		if closeErr := lock.Close(); closeErr != nil {
			return Record{}, "", fmt.Errorf("%w; close lock: %w", err, closeErr)
		}
		return Record{}, "", err
	}
	ev, err := check(ctx)
	if err != nil {
		if closeErr := lock.Close(); closeErr != nil {
			return Record{}, "", errors.Join(err, fmt.Errorf("eventlog: close lock: %w", closeErr))
		}
		return Record{}, "", err
	}
	rec, hash, err := w.writeAppend(current, ev)
	if closeErr := lock.Close(); closeErr != nil {
		return Record{}, "", errors.Join(err, fmt.Errorf("eventlog: close lock after append: %w", closeErr))
	}
	return rec, hash, err
}

// AppendChecked calls check under the event-log lock, then appends the event check returns.
// The check runs while the lock is held; it may replay the log (Replay takes no lock).
// Queued writers wait up to LockTimeout meanwhile, so check must stay short.
// A check error is returned as is and nothing is appended.
func (w *Writer) AppendChecked(ctx context.Context, check func(context.Context) (Event, error)) (Record, error) {
	rec, _, err := w.AppendCheckedWithHash(ctx, check)
	return rec, err
}

// Append appends ev. It is AppendChecked with a check that accepts ev.
func (w *Writer) Append(ctx context.Context, ev Event) (Record, error) {
	return w.AppendChecked(ctx, func(context.Context) (Event, error) {
		return ev, nil
	})
}

// RepairTail removes an uncommitted record past HEAD left by an append that
// crashed or failed before advancing HEAD, after verifying that it links to HEAD
// and carries a valid receipt from this writer's key. It takes the writer lock
// and reports whether an orphaned record was removed.
func (w *Writer) RepairTail(ctx context.Context) (bool, error) {
	if err := readyContext(ctx, "repair"); err != nil {
		return false, err
	}
	if err := os.MkdirAll(w.eventsDir, 0o750); err != nil {
		return false, fmt.Errorf("eventlog: mkdir %s: %w", w.eventsDir, err)
	}
	lock, err := acquireLock(ctx, w.eventsDir, w.limits.LockTimeout)
	if err != nil {
		return false, err
	}
	_, repaired, err := w.currentHeadWithRepair()
	if err != nil {
		if closeErr := lock.Close(); closeErr != nil {
			return false, fmt.Errorf("%w; close lock: %w", err, closeErr)
		}
		return false, err
	}
	if closeErr := lock.Close(); closeErr != nil {
		return false, fmt.Errorf("eventlog: close lock: %w", closeErr)
	}
	return repaired, nil
}

func (w *Writer) writeAppend(current unsignedHead, ev Event) (Record, string, error) {
	rec, full, err := w.buildRecord(current, ev)
	if err != nil {
		return Record{}, "", err
	}
	if err = writeNewRecord(w.eventsDir, rec.Seq, full); err != nil {
		return Record{}, "", err
	}
	hash := hashHex(full)
	if err = w.writeHead(rec.Seq, hash); err != nil {
		return Record{}, "", w.cleanupFailedAppend(rec.Seq, err)
	}
	return rec, hash, nil
}

func (w *Writer) buildRecord(current unsignedHead, ev Event) (Record, []byte, error) {
	payload, err := canonicalPayload(ev.Payload)
	if err != nil {
		return Record{}, nil, fmt.Errorf("eventlog: payload: %w", err)
	}
	rec := Record{
		Seq:     current.Seq + 1,
		Prev:    current.Hash,
		Time:    w.limits.Clock.Now().UTC().Format(time.RFC3339Nano),
		Type:    ev.Type,
		TaskID:  ev.TaskID,
		Payload: payload,
	}
	if rec.Seq == 1 {
		rec.Prev = zeroHash
	}
	unsigned, err := canonicalJSON(recordUnsigned(rec))
	if err != nil {
		return Record{}, nil, err
	}
	signed, err := w.signer.Sign(corereceipt.Run{Command: AppendCommand, Output: unsigned})
	if err != nil {
		return Record{}, nil, fmt.Errorf("eventlog: sign record %d: %w", rec.Seq, err)
	}
	rec.Receipt = signed
	full, err := canonicalJSON(rec)
	if err != nil {
		return Record{}, nil, err
	}
	if len(full) > w.limits.MaxRecordBytes {
		return Record{}, nil, fmt.Errorf("%w: seq %d is %d bytes", ErrRecordTooLarge, rec.Seq, len(full))
	}
	return rec, full, nil
}

func readDecodedHead(eventsDir string, pub string) (head, bool, error) {
	path := filepath.Join(eventsDir, "HEAD.json")
	// #nosec G304 -- the events directory comes from operator config and the
	// file name is fixed; HEAD.json is verified before it is trusted.
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return head{}, false, nil
	}
	if err != nil {
		return head{}, false, fmt.Errorf("eventlog: read %s: %w", path, err)
	}
	var h head
	if err = decodeHead(path, data, pub, &h); err != nil {
		return head{}, false, err
	}
	return h, true, nil
}

func (w *Writer) cleanupFailedAppend(seq uint64, cause error) error {
	h, exists, err := readDecodedHead(w.eventsDir, w.signer.PublicKey())
	if err != nil {
		return errors.Join(cause, err)
	}
	if exists && h.Seq >= seq {
		return cause
	}
	recPath := eventPath(w.eventsDir, seq)
	if rmErr := os.Remove(recPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		cause = errors.Join(cause, fmt.Errorf("eventlog: remove uncommitted record %s: %w", recPath, rmErr))
	}
	if syncErr := syncDirPath(w.eventsDir); syncErr != nil {
		cause = errors.Join(cause, fmt.Errorf("eventlog: sync directory %s: %w", w.eventsDir, syncErr))
	}
	return cause
}

func (w *Writer) currentHead() (unsignedHead, error) {
	h, _, err := w.currentHeadWithRepair()
	return h, err
}

func (w *Writer) currentHeadWithRepair() (unsignedHead, bool, error) {
	h, exists, err := readDecodedHead(w.eventsDir, w.signer.PublicKey())
	if err != nil {
		return unsignedHead{}, false, err
	}
	if !exists {
		return w.firstHead()
	}
	if err = verifyHeadTail(w.eventsDir, h); err != nil {
		return unsignedHead{}, false, err
	}
	repaired, err := w.checkAndRepairTail(h.Seq, h.Hash)
	if err != nil {
		return unsignedHead{}, false, err
	}
	return unsignedHead{Seq: h.Seq, Hash: h.Hash}, repaired, nil
}

func (w *Writer) firstHead() (unsignedHead, bool, error) {
	if base, baseErr := newestBase(baseDir(w.eventsDir)); baseErr != nil || base != 0 {
		return unsignedHead{}, false, errors.Join(fmt.Errorf("eventlog: missing HEAD with existing base at seq %d", base), baseErr)
	}
	segs, err := listSegments(w.eventsDir, w.limits.MaxReplayRecords, 0)
	if err != nil {
		return unsignedHead{}, false, err
	}
	if len(segs) > 0 {
		return unsignedHead{}, false, fmt.Errorf("eventlog: missing HEAD with existing segment %s", segs[0].name)
	}
	repaired, err := w.checkAndRepairTail(0, zeroHash)
	if err != nil {
		return unsignedHead{}, false, err
	}
	return unsignedHead{}, repaired, nil
}

func (w *Writer) recordsPastHead(headSeq uint64) ([]uint64, error) {
	entries, err := os.ReadDir(w.eventsDir)
	if err != nil {
		return nil, fmt.Errorf("eventlog: read %s: %w", w.eventsDir, err)
	}
	var pastHead []uint64
	for i := 0; i < len(entries); i++ {
		name := entries[i].Name()
		if entries[i].IsDir() || !isEventRecord(name) {
			continue
		}
		seq, parseErr := seqFromName(name)
		if parseErr != nil {
			return nil, parseErr
		}
		if seq > headSeq {
			pastHead = append(pastHead, seq)
		}
	}
	sort.Slice(pastHead, func(i, j int) bool { return pastHead[i] < pastHead[j] })
	return pastHead, nil
}

func (w *Writer) checkAndRepairTail(headSeq uint64, headHash string) (bool, error) {
	pastHead, err := w.recordsPastHead(headSeq)
	if err != nil {
		return false, err
	}
	if len(pastHead) == 0 {
		return false, nil
	}
	if len(pastHead) > 1 {
		return false, fmt.Errorf("eventlog: cannot repair tail: %d records past HEAD seq %d", len(pastHead), headSeq)
	}
	orphanSeq := pastHead[0]
	if orphanSeq != headSeq+1 {
		return false, fmt.Errorf("eventlog: cannot repair tail: gap past HEAD seq %d, found seq %d", headSeq, orphanSeq)
	}
	wantPrev := headHash
	if headSeq == 0 {
		wantPrev = zeroHash
	}
	orphanPath := eventPath(w.eventsDir, orphanSeq)
	data, err := readFileBounded(orphanPath, w.limits.MaxRecordBytes)
	if err != nil {
		return false, err
	}
	if _, err = decodeRecord(orphanPath, data, w.signer.PublicKey(), orphanSeq, wantPrev); err != nil {
		return false, fmt.Errorf("eventlog: cannot repair tail at seq %d: %w", orphanSeq, err)
	}
	if err = os.Remove(orphanPath); err != nil {
		return false, fmt.Errorf("eventlog: remove orphan %s: %w", orphanPath, err)
	}
	if err = syncDirPath(w.eventsDir); err != nil {
		return false, fmt.Errorf("eventlog: sync dir %s after orphan removal: %w", w.eventsDir, err)
	}
	return true, nil
}

func (w *Writer) writeHead(seq uint64, hash string) error {
	unsigned := unsignedHead{Seq: seq, Hash: hash}
	out, err := canonicalJSON(unsigned)
	if err != nil {
		return err
	}
	r, err := w.signer.Sign(corereceipt.Run{Command: AppendCommand, Output: out})
	if err != nil {
		return fmt.Errorf("eventlog: sign HEAD: %w", err)
	}
	body, err := canonicalJSON(head{Seq: seq, Hash: hash, Receipt: r})
	if err != nil {
		return err
	}
	return atomicReplace(w.eventsDir, "HEAD.json", body, 0o600)
}

func writeNewRecord(eventsDir string, seq uint64, body []byte) error {
	name := eventFileName(seq)
	path := filepath.Join(eventsDir, name)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("eventlog: %s already exists", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("eventlog: stat %s: %w", path, err)
	}
	return atomicReplace(eventsDir, name, body, 0o600)
}

func decodeHead(path string, data []byte, pub string, h *head) error {
	if err := verifyCanonical(path, data); err != nil {
		return err
	}
	if err := json.Unmarshal(data, h); err != nil {
		return fmt.Errorf("eventlog: decode %s: %w", path, err)
	}
	unsigned, err := canonicalJSON(unsignedHead{Seq: h.Seq, Hash: h.Hash})
	if err != nil {
		return err
	}
	if h.Receipt == nil {
		return fmt.Errorf("eventlog: %s: missing receipt", path)
	}
	if err = ensureExpectedPublicKey(h.Receipt.PublicKey, pub, path); err != nil {
		return err
	}
	if err = corereceipt.VerifyOutput(h.Receipt, unsigned); err != nil {
		return fmt.Errorf("eventlog: verify %s: %w", path, err)
	}
	return nil
}

func verifyHeadTail(eventsDir string, h head) error {
	data, err := os.ReadFile(eventPath(eventsDir, h.Seq))
	if err != nil {
		return fmt.Errorf("eventlog: HEAD tail seq %d: %w", h.Seq, err)
	}
	if got := hashHex(data); got != h.Hash {
		return fmt.Errorf("eventlog: HEAD tail seq %d hash mismatch", h.Seq)
	}
	return nil
}
