package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tekmora/epitrop/internal/eventlog"
)

// JobHistory counts one job's supervision records in the event log: the evidence the
// cutover's "supervision holds" check reads (docs/cutover-1.md). A compacted log holds no
// records up to its base, so the counts start after the base.
type JobHistory struct {
	Name      string
	Started   int // job.started and job.adopted
	Exited    int
	Unclean   int // exits without code 0: a stop's signal, a lost job and a deadline included
	Restarted int
	Stale     int
	Refused   int
	Stopped   int
	First     string // time of the first counted record, empty when none was counted
	Last      string // time of the last counted record
}

// History counts the supervision records of the named job, or of every configured job,
// written at or after since. A record whose time does not parse fails the count.
func (s *Supervisor) History(ctx context.Context, name string, since time.Time) ([]JobHistory, error) {
	if err := readyContext(ctx, "history"); err != nil {
		return nil, err
	}
	jobs, err := s.selectedJobs(name)
	if err != nil {
		return nil, err
	}
	counter := historyCounter{since: since, byName: map[string]JobHistory{}}
	if _, err = s.replayStateWithReducer(ctx, counter.reduce); err != nil {
		return nil, err
	}
	out := make([]JobHistory, 0, len(jobs))
	for i := 0; i < len(jobs); i++ {
		h := counter.byName[jobs[i].Name]
		h.Name = jobs[i].Name
		out = append(out, h)
	}
	return out, nil
}

// historyCounter counts records as the replay folds them. The replay retries a pass that an
// append or a compaction disturbed and hands the same records out again, so a record counts
// only when its seq is past the last one counted.
type historyCounter struct {
	since  time.Time
	last   uint64
	byName map[string]JobHistory
}

func (c *historyCounter) reduce(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	if rec.Seq > c.last {
		c.last = rec.Seq
		if err := c.count(rec); err != nil {
			return eventlog.State{}, err
		}
	}
	return JobReducer(st, rec)
}

func (c *historyCounter) count(rec eventlog.Record) error {
	at, err := time.Parse(time.RFC3339Nano, rec.Time)
	if err != nil {
		return fmt.Errorf("supervisor: history: seq %d time: %w", rec.Seq, err)
	}
	if at.Before(c.since) {
		return nil
	}
	h := c.byName[rec.TaskID]
	field := h.counterFor(rec.Type)
	if field == nil {
		return nil
	}
	*field++
	if rec.Type == "job.exited" {
		unclean, uErr := uncleanExit(rec)
		if uErr != nil {
			return uErr
		}
		if unclean {
			h.Unclean++
		}
	}
	if h.First == "" {
		h.First = rec.Time
	}
	h.Last = rec.Time
	c.byName[rec.TaskID] = h
	return nil
}

// counterFor is the count a record of type typ adds to, nil for a type history leaves out.
func (h *JobHistory) counterFor(typ string) *int {
	switch typ {
	case "job.started", "job.adopted":
		return &h.Started
	case "job.exited":
		return &h.Exited
	case "job.restarted":
		return &h.Restarted
	case "job.stale":
		return &h.Stale
	case "job.refused":
		return &h.Refused
	case "job.stopped":
		return &h.Stopped
	}
	return nil
}

func uncleanExit(rec eventlog.Record) (bool, error) {
	var payload struct {
		Code *int `json:"code"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		return false, fmt.Errorf("supervisor: history: seq %d payload: %w", rec.Seq, err)
	}
	return payload.Code == nil || *payload.Code != 0, nil
}
