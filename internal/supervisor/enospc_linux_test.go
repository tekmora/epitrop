//go:build linux

package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
	"github.com/tekmora/epitrop/internal/eventlog"
)

func TestSuperviseAppendENOSPC_ErrorWrapped(t *testing.T) {
	sup, _ := historySupervisor(t)
	ctx := testContext(t)
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running"})

	realAppend := appendEvent
	appendEvent = func(w *eventlog.Writer, c context.Context, ev eventlog.Event) (eventlog.Record, error) {
		return eventlog.Record{}, &os.PathError{Op: "write", Path: sup.cfg.EventLog.Dir, Err: syscall.ENOSPC}
	}
	t.Cleanup(func() { appendEvent = realAppend })

	err := sup.Reconcile(ctx)
	if err == nil {
		t.Fatal("Reconcile() with ENOSPC = nil, want error")
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Reconcile() error = %v, want errors.Is ENOSPC", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "job daemon") {
		t.Fatalf("error %q does not contain job name 'job daemon'", msg)
	}
	if !strings.Contains(msg, "record job.exited") {
		t.Fatalf("error %q does not contain event type 'record job.exited'", msg)
	}
	if !strings.Contains(msg, sup.cfg.EventLog.Dir) {
		t.Fatalf("error %q does not contain event log dir %s", msg, sup.cfg.EventLog.Dir)
	}
}

func plantOrphanRecord(t *testing.T, rt testRuntimeConfig, signer *corereceipt.Signer, limits eventlog.Limits) {
	t.Helper()
	headPath := filepath.Join(rt.EventLogDir, "events", "HEAD.json")
	headData, err := os.ReadFile(headPath)
	if err != nil {
		t.Fatalf("read HEAD.json: %v", err)
	}
	w, err := eventlog.Open(rt.EventLogDir, signer, limits)
	if err != nil {
		t.Fatalf("Open eventlog: %v", err)
	}
	_, err = w.Append(testContext(t), eventlog.Event{Type: "log.info", TaskID: "test", Payload: []byte(`{"note":"orphan"}`)})
	if err != nil {
		t.Fatalf("Append orphan: %v", err)
	}
	if err = os.WriteFile(headPath, headData, 0o600); err != nil {
		t.Fatalf("restore HEAD.json: %v", err)
	}
}

func startDaemon(t *testing.T, sup *Supervisor, ctx context.Context) int {
	t.Helper()
	if err := sup.Start(ctx, "daemon"); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	st, err := sup.Status(ctx, "daemon")
	if err != nil || len(st) != 1 || st[0].State != StateRunning {
		t.Fatalf("Status() = %v, state = %v", err, st)
	}
	return st[0].PID
}

func superviseUntilAdopted(t *testing.T, sup *Supervisor, ctx context.Context) func() {
	t.Helper()
	supCtx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- sup.Supervise(supCtx)
	}()
	if !waitUntil(func() bool {
		recorded, rErr := sup.Recorded(ctx, "daemon")
		return rErr == nil && recorded.LastEvent == "job.adopted"
	}) {
		cancel()
		t.Fatal("timed out waiting for sup to adopt daemon after repair")
	}
	return func() {
		cancel()
		if supErr := <-errCh; supErr != nil && !errors.Is(supErr, context.Canceled) {
			t.Fatalf("Supervise() = %v, want context.Canceled", supErr)
		}
	}
}

func countJobStarts(t *testing.T, ctx context.Context, dir string, pub string, limits eventlog.Limits, name string) int {
	t.Helper()
	var count int
	_, err := eventlog.Replay(ctx, dir, pub, limits, func(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
		if rec.TaskID == name && rec.Type == "job.started" {
			count++
		}
		return st, nil
	})
	if err != nil {
		t.Fatalf("Replay() = %v", err)
	}
	return count
}

func verifyAdoptedRecord(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record 2: %v", err)
	}
	if strings.Contains(string(data), "orphan") {
		t.Fatal("orphan record was not removed by tail repair")
	}
	if !strings.Contains(string(data), "job.adopted") {
		t.Fatalf("record 2 = %s, want job.adopted", string(data))
	}
}

func TestSuperviseRestartAfterSpaceFreed(t *testing.T) {
	rt := testRuntime(t)
	job := testJob("daemon", []string{"sleep", "60"})
	sup1 := testSupervisorAt(t, rt, job)
	ctx := testContext(t)
	pid1 := startDaemon(t, sup1, ctx)

	plantOrphanRecord(t, rt, sup1.signer, sup1.eventLogLimits())
	orphanPath := filepath.Join(rt.EventLogDir, "events", "00000000000000000002.json")
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("orphan record file %s missing: %v", orphanPath, err)
	}

	sup2 := testSupervisorAt(t, rt, job)
	stopSupervise := superviseUntilAdopted(t, sup2, ctx)
	st2, err := sup2.Status(ctx, "daemon")
	if err != nil || len(st2) != 1 || st2[0].State != StateRunning {
		t.Fatalf("Status(sup2) = %v, state = %v", err, st2)
	}
	if st2[0].PID != pid1 {
		t.Fatalf("Status PID changed from %d to %d (wanted re-adopted)", pid1, st2[0].PID)
	}
	stopSupervise()

	if count := countJobStarts(t, ctx, rt.EventLogDir, sup2.cfg.EventLog.PublicKey, sup2.eventLogLimits(), "daemon"); count != 1 {
		t.Fatalf("job.started count = %d, want 1 (no second start)", count)
	}
	verifyAdoptedRecord(t, orphanPath)
}
