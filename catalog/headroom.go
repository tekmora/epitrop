package catalog

import (
	"fmt"
	"math"
	"time"
)

// HeadroomState says whether a consumer may act on a usage reading.
type HeadroomState string

const (
	// HeadroomKnown means the reading is fresh and carries a used share.
	HeadroomKnown HeadroomState = "known"
	// HeadroomUnknown means the reading is missing, stale or out of range.
	// A consumer treats it as unknown, never as zero usage.
	HeadroomUnknown HeadroomState = "unknown"
)

// Headroom is one usage window read from a snapshot for a consumer such as a
// router or a dispatch hook. UsedPercent is set only when State is known, so
// an unknown reading cannot be read as zero usage by accident.
type Headroom struct {
	ModelID       string        `json:"model_id"`
	Source        string        `json:"source"`
	State         HeadroomState `json:"state"`
	UsedPercent   *float64      `json:"used_percent,omitempty"`
	WindowMinutes *int          `json:"window_minutes,omitempty"`
	ResetsAt      *time.Time    `json:"resets_at,omitempty"`
	FetchedAt     time.Time     `json:"fetched_at"`
	Reason        string        `json:"reason,omitempty"`
}

// ReadHeadroom returns one reading per record in s that carries a usage
// window. A reading older than maxAge, fetched after now, without a used
// share, with a share out of range, or whose window has reset since is
// unknown and names why. A reading exactly maxAge old is still known.
func ReadHeadroom(s Snapshot, now time.Time, maxAge time.Duration) ([]Headroom, error) {
	if maxAge <= 0 {
		return nil, fmt.Errorf("catalog: headroom max age %s is not positive", maxAge)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	readings := []Headroom{}
	for _, r := range s.Records {
		if r.UsageWindow == nil {
			continue
		}
		readings = append(readings, readOneHeadroom(r, now, maxAge))
	}
	return readings, nil
}

func readOneHeadroom(r Record, now time.Time, maxAge time.Duration) Headroom {
	h := Headroom{
		ModelID:       r.ModelID,
		Source:        r.Provenance.Source,
		State:         HeadroomUnknown,
		WindowMinutes: r.UsageWindow.WindowMinutes,
		ResetsAt:      r.UsageWindow.ResetsAt,
		FetchedAt:     r.Provenance.FetchedAt,
	}
	if h.Reason = unknownHeadroomReason(r, now, maxAge); h.Reason != "" {
		return h
	}
	used := *r.UsageWindow.UsedPercent
	h.State = HeadroomKnown
	h.UsedPercent = &used
	return h
}

// unknownHeadroomReason names why r's usage window cannot be acted on, or
// returns "" when it can.
func unknownHeadroomReason(r Record, now time.Time, maxAge time.Duration) string {
	used, fetched, resets := r.UsageWindow.UsedPercent, r.Provenance.FetchedAt, r.UsageWindow.ResetsAt
	switch {
	case used == nil:
		return "the source reported no used_percent"
	case math.IsNaN(*used) || math.IsInf(*used, 0) || *used < 0:
		return fmt.Sprintf("used_percent %v is out of range", *used)
	case fetched.After(now):
		return fmt.Sprintf("fetched at %s, after now %s", fetched.Format(time.RFC3339), now.Format(time.RFC3339))
	case now.Sub(fetched) > maxAge:
		return fmt.Sprintf("stale: fetched %s ago, max age %s", now.Sub(fetched).Round(time.Second), maxAge)
	case resets != nil && !resets.After(now):
		return fmt.Sprintf("the window reset at %s; the reading no longer holds", resets.Format(time.RFC3339))
	default:
		return ""
	}
}
