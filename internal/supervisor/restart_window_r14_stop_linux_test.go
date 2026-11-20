//go:build linux

package supervisor

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStopReresolvesReplacementAfterSignal(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	signals := stageStopReplacement(t, sup, false)
	if err := sup.Stop(testContext(t), "daemon"); err != nil {
		t.Fatalf("Stop with replacement = %v, signals %v", err, *signals)
	}
	if got := eventlogJob(t, sup, "daemon"); got.LastEvent != "job.stopped" || len(*signals) != 2 {
		t.Fatalf("job = %+v, signals %v; want stopped after signaling both runs", got, *signals)
	}
}

func TestStopWaitsForReplacementRecord(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	signals := stageStopReplacement(t, sup, true)
	if err := sup.Stop(testContext(t), "daemon"); err != nil {
		t.Fatalf("Stop with late replacement record = %v, signals %v", err, *signals)
	}
	if got := eventlogJob(t, sup, "daemon"); got.LastEvent != "job.stopped" || len(*signals) != 2 {
		t.Fatalf("job = %+v, signals %v; want stopped after signaling both runs", got, *signals)
	}
}

func stageStopReplacement(t *testing.T, sup *Supervisor, delayed bool) *[]recordedSignal {
	t.Helper()
	const pidA, shimA, shimB = 100001, 200001, 200002
	lock, err := holdLock(sup.lockPath("daemon"))
	if err != nil {
		t.Fatal(err)
	}
	held := true
	t.Cleanup(func() {
		if held {
			if closeErr := lock.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
	})
	writeStaleRecord(t, sup, "daemon", pidA, shimA)
	withProcessGroupID(t, nil)
	stageDelayedStopRecord(t, sup, delayed)
	withShimProbe(t, nil)
	oldStatus := procStatusReadFile
	procStatusReadFile = func(path string) ([]byte, error) {
		parent := shimB
		if strings.Contains(path, fmt.Sprint(pidA)) {
			parent = shimA
		}
		return fmt.Appendf(nil, "Name:\ttest\nPPid:\t%d\n", parent), nil
	}
	t.Cleanup(func() { procStatusReadFile = oldStatus })
	withProcCmdline(t, []string{"__job-shim", "--name", "daemon", "--lock", sup.lockPath("daemon"), "--record", sup.recordPath("daemon")})
	return stopReplacementSignaller(t, sup, delayed, lock, &held)
}

func stopReplacementSignaller(t *testing.T, sup *Supervisor, delayed bool, lock *heldLock, held *bool) *[]recordedSignal {
	t.Helper()
	return withStopSignaller(t, func(pid int, sig unix.Signal) error {
		if pid == -100001 && sig == unix.SIGTERM {
			if !delayed {
				writeStaleRecord(t, sup, "daemon", 100002, 200002)
			}
			return nil
		}
		if pid == -100002 && sig == unix.SIGTERM {
			*held = false
			return lock.Close()
		}
		return unix.EPERM
	})
}

func stageDelayedStopRecord(t *testing.T, sup *Supervisor, delayed bool) {
	t.Helper()
	if !delayed {
		return
	}
	old := processGroupID
	reads := 0
	processGroupID = func(pid int) (int, error) {
		if pid == 100001 {
			reads++
			if reads == 3 {
				writeStaleRecord(t, sup, "daemon", 100002, 200002)
				return 0, unix.ESRCH
			}
		}
		return old(pid)
	}
	t.Cleanup(func() { processGroupID = old })
}
