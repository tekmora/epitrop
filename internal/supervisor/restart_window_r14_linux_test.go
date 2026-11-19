//go:build linux

package supervisor

import (
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestStopMarkerWaitsForShimExitingAfterCollectorCheck(t *testing.T) {
	t.Run("current shim", func(t *testing.T) { stopMarkerAfterCollectorCheck(t, false) })
	t.Run("prior shim after unpublished replacement", func(t *testing.T) { stopMarkerAfterCollectorCheck(t, true) })
}

// stopMarkerAfterCollectorCheck SIGTERMs the run's shim after the marker's first collector
// check and holds its collector at the seam. With replaced, a replacement shim is spawned
// before the stop, so the pending collector belongs to a prior shim and the marker must wait
// on that prior shim's collector.
func stopMarkerAfterCollectorCheck(t *testing.T, replaced bool) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	cmd := exec.CommandContext(t.Context(), "/usr/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	sup.recordSpawnedShim("daemon", pid)
	done := sup.spawnedShim["daemon"].done
	sup.shimWaiters.Add(1)
	const since = "2026-11-18T08:00:00Z"
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "pid": pid + 1, "shim_pid": pid, "since": since})
	sup.observeRunning("daemon", since)
	if replaced {
		sup.recordSpawnedShim("daemon", pid+100000)
	}
	atCollector, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	oldCollector, oldCheck := beforeRecordCollectedShim, stopCollectorCheck
	beforeRecordCollectedShim = func(string, int) { close(atCollector); <-release }
	stopCollectorCheck = func(stage string) {
		if stage == "checked" {
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Error(err)
			}
			select {
			case <-atCollector:
			case <-time.After(testWaitBound):
				t.Error("collector did not reach seam")
			}
		}
		if stage == "waiting" {
			unblock()
		}
	}
	go sup.collectShim(cmd, "daemon")
	t.Cleanup(func() { unblock(); <-done; beforeRecordCollectedShim, stopCollectorCheck = oldCollector, oldCheck })
	if err := sup.markSupervisorStopped(); err != nil {
		t.Fatal(err)
	}
	if got := historyOne(t, sup, "daemon").SupervisorStops; got != 1 {
		t.Fatalf("supervisor stop markers = %d, want 1 after SIGTERM collection", got)
	}
}
