//go:build linux

package supervisor

import "testing"

func TestAutomaticShimRejectsSupersedingCompletedRun(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("on-failure", 1, 0, longJobCommand(t)))
	before := eventlogJob(t, sup, "daemon")
	stageSupersedingRun(t, sup)
	if err := sup.startUnlessStopped(testContext(t), "daemon", before); err != nil {
		t.Fatal(err)
	}
	if starts := countEvents(t, sup.cfg.EventLog.Dir, "job.started"); starts != 1 {
		t.Fatalf("job.started count = %d, want only superseding operator run", starts)
	}
}

func stageSupersedingRun(t *testing.T, sup *Supervisor) {
	t.Helper()
	var held *heldLock
	oldHook := automaticShimStart
	automaticShimStart = func(stage string) {
		if stage == "before" {
			var err error
			held, err = holdLock(sup.lockPath("daemon"))
			if err != nil {
				t.Fatal(err)
			}
		}
		if stage == "spawned" {
			appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "pid": 100, "shim_pid": 101, "since": "2026-11-18T08:00:00Z"})
			appendHistory(t, sup, "job.exited", "daemon", map[string]any{"state": "dead", "code": 0})
			if err := held.Close(); err != nil {
				t.Fatal(err)
			}
			held = nil
		}
	}
	t.Cleanup(func() {
		automaticShimStart = oldHook
		if held != nil {
			if err := held.Close(); err != nil {
				t.Error(err)
			}
		}
	})
}
