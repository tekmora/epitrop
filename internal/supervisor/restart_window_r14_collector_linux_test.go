//go:build linux

package supervisor

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestUnpublishedReplacementPreservesFailedShimStatus(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	const since = "2026-11-18T08:00:00Z"
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "pid": pid + 1, "shim_pid": pid, "since": since})
	sup.observeRunning("daemon", since)
	sup.recordSpawnedShim("daemon", pid)
	sup.shimWaiters.Add(1)
	oldCollector := beforeRecordCollectedShim
	beforeRecordCollectedShim = func(string, int) { sup.recordSpawnedShim("daemon", pid+100000) }
	t.Cleanup(func() { beforeRecordCollectedShim = oldCollector })
	sup.collectShim(cmd, "daemon")
	if err := sup.markSupervisorStopped(); err != nil {
		t.Fatal(err)
	}
	if got := historyOne(t, sup, "daemon").SupervisorStops; got != 0 {
		t.Fatalf("supervisor stop markers = %d, want 0 for failed run before replacement start", got)
	}
}

func TestUnpublishedReplacementKeepsSigtermStopMarker(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	cmd := exec.CommandContext(t.Context(), "/usr/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	const since = "2026-11-18T08:00:00Z"
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "pid": pid + 1, "shim_pid": pid, "since": since})
	sup.observeRunning("daemon", since)
	sup.recordSpawnedShim("daemon", pid)
	sup.shimWaiters.Add(1)
	oldCollector := beforeRecordCollectedShim
	beforeRecordCollectedShim = func(string, int) { sup.recordSpawnedShim("daemon", pid+100000) }
	t.Cleanup(func() { beforeRecordCollectedShim = oldCollector })
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	sup.collectShim(cmd, "daemon")
	if err := sup.markSupervisorStopped(); err != nil {
		t.Fatal(err)
	}
	if got := historyOne(t, sup, "daemon").SupervisorStops; got != 1 {
		t.Fatalf("supervisor stop markers = %d, want 1 for a SIGTERM run before replacement start", got)
	}
}

func TestStartRefusesWhenPriorShimTrackingIsFull(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	sup.priorShims = map[string]map[int]priorShim{"daemon": {}}
	for i := 0; i < maxShimWaiters; i++ {
		pid := i + 100000
		sup.priorShims["daemon"][pid] = priorShim{spawned: spawnedShim{pid: pid, done: make(chan struct{})}}
	}
	before := eventlogJob(t, sup, "daemon")
	err := sup.startUnlessStopped(testContext(t), "daemon", before)
	if err == nil || !strings.Contains(err.Error(), "prior shim tracking is full") {
		t.Fatalf("start with full prior shim tracking = %v, want prior shim tracking is full", err)
	}
	if after := eventlogJob(t, sup, "daemon"); after.LastEvent != before.LastEvent || after.Since != before.Since {
		t.Fatalf("job after refused start = %+v, want unchanged %+v", after, before)
	}
}
