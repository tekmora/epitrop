package supervisor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/eventlog"
)

func historyRecord(seq uint64, typ string, at string, payload string) eventlog.Record {
	return eventlog.Record{Seq: seq, Type: typ, TaskID: "daemon", Time: at, Payload: json.RawMessage(payload)}
}

func TestHistoryCountsARetriedRecordOnce(t *testing.T) {
	c := historyCounter{byName: map[string]JobHistory{}}
	at := "2026-10-17T12:00:00Z"
	recs := []eventlog.Record{
		historyRecord(1, "job.started", at, `{"state":"running"}`),
		historyRecord(2, "job.exited", at, `{"state":"dead","code":1}`),
	}
	st := eventlog.State{}
	// The replay retried: the same two records come again, then one more.
	for _, rec := range append(append(recs, recs...), historyRecord(3, "job.restarted", at, `{}`)) {
		var err error
		if st, err = c.reduce(st, rec); err != nil {
			t.Fatalf("reduce(seq %d) = %v, want nil", rec.Seq, err)
		}
	}
	h := c.byName["daemon"]
	if h.Started != 1 || h.Exited != 1 || h.Unclean != 1 || h.Restarted != 1 {
		t.Fatalf("counts after a retried pass = %+v, want each record once", h)
	}
	if st.Jobs["daemon"].LastEvent != "job.restarted" || st.Jobs["daemon"].Restarts != 1 {
		t.Fatalf("state after the pass = %+v, want the job reducer's fold", st.Jobs["daemon"])
	}
}

func TestHistoryRefusesARecordItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		rec  eventlog.Record
		want string
	}{
		{historyRecord(1, "job.started", "yesterday", `{}`), "seq 1 time"},
		{historyRecord(2, "job.exited", "2026-10-17T12:00:00Z", `{"code":"x"}`), "seq 2 payload"},
	} {
		c := historyCounter{byName: map[string]JobHistory{}}
		_, err := c.reduce(eventlog.State{}, tc.rec)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("reduce(%s) = %v, want an error naming %q", tc.rec.Type, err, tc.want)
		}
	}
}
