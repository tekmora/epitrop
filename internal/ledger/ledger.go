// Package ledger reads the Praetor efficiency ledger, the JSON that
// `praetorctl efficiency --json` prints, for the orchestration and platform
// budget layers (#6). A figure the ledger reports as not measured stays
// unknown here: it carries no value, so no caller can read it as zero.
//
// The fields read are the ones the pinned engine declares in
// internal/efficiency/types.go; testdata/upstream/efficiency-report.json is
// that engine's output, captured unchanged.
package ledger

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
)

const (
	// MaxReportBytes bounds the ledger read (HISS-02).
	MaxReportBytes = 16 << 20
	// MetricEpoch is the ledger metric epoch this reader understands. The
	// engine starts a new epoch when a metric changes meaning, so a report of
	// another epoch is refused rather than read with the old meanings.
	MetricEpoch = "2026-10-10"
	// MaxUnits bounds the units read from one report (HISS-02).
	MaxUnits = 10000
	// notMeasured starts every display string of a figure the ledger did not
	// measure, for example "not measured (see #897)".
	notMeasured = "not measured"
)

// provenances are the labels the engine gives a vector value
// (efficiency.ValidProvenance); any other label is refused.
var provenances = map[string]struct{}{"measured": {}, "modeled": {}, "cited": {}, "interval": {}}

// Figure is one ledger number. Value is set only when the ledger reports one;
// otherwise Reason says why there is none.
type Figure struct {
	Value      *float64 `json:"value,omitempty"`
	Provenance string   `json:"provenance,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

// Known reports whether f carries a value.
func (f Figure) Known() bool { return f.Value != nil }

// Sources says which inputs the engine read for the report.
type Sources struct {
	Forge       bool `json:"forge"`
	Transcripts bool `json:"transcripts"`
	SpendLog    bool `json:"spend_log"`
}

// Unit is one pull request unit of the ledger.
type Unit struct {
	PullRequest    int    `json:"pull_request_number"`
	FrontierTokens Figure `json:"frontier_tokens"`
	Spend          Figure `json:"spend"`
	WallSeconds    Figure `json:"wall_seconds"`
}

// Report is the part of a ledger report the budget layers read.
type Report struct {
	Sources        Sources `json:"sources_measured"`
	FrontierTokens Figure  `json:"frontier_tokens"`
	TotalSpend     Figure  `json:"total_spend"`
	Units          []Unit  `json:"units"`
}

type wireVector struct {
	Value      *float64 `json:"value"`
	Provenance string   `json:"provenance"`
}

type wireUnit struct {
	PullRequest       int         `json:"pull_request_number"`
	MetricEpoch       string      `json:"metric_epoch"`
	FrontierTokens    *string     `json:"frontier_tokens"`
	FrontierTokensNum *float64    `json:"frontier_tokens_num"`
	Spend             *string     `json:"spend"`
	SpendAmount       *float64    `json:"spend_amount"`
	WallSeconds       *wireVector `json:"wall_seconds"`
}

type wireSummary struct {
	MetricEpoch       string   `json:"metric_epoch"`
	FrontierTokens    *string  `json:"frontier_tokens"`
	FrontierTokensNum *float64 `json:"frontier_tokens_num"`
	TotalSpend        *string  `json:"total_spend"`
	TotalSpendNum     *float64 `json:"total_spend_num"`
}

type wireReport struct {
	Units   []wireUnit   `json:"units"`
	Summary *wireSummary `json:"milestone_summary"`
	Sources *Sources     `json:"sources_measured"`
}

// Read decodes one ledger report from r. A report of another metric epoch,
// one without its summary or sources, or a figure whose display string and
// number disagree is refused.
func Read(r io.Reader) (Report, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxReportBytes+1))
	if err != nil {
		return Report{}, fmt.Errorf("ledger: read report: %w", err)
	}
	if len(body) > MaxReportBytes {
		return Report{}, fmt.Errorf("ledger: report is larger than %d bytes", MaxReportBytes)
	}
	var wire wireReport
	if err := json.Unmarshal(body, &wire); err != nil {
		return Report{}, fmt.Errorf("ledger: decode report: %w", err)
	}
	return fromWire(wire)
}

func fromWire(wire wireReport) (Report, error) {
	switch {
	case wire.Summary == nil:
		return Report{}, fmt.Errorf("ledger: report carries no milestone_summary")
	case wire.Sources == nil:
		return Report{}, fmt.Errorf("ledger: report carries no sources_measured")
	case len(wire.Units) > MaxUnits:
		return Report{}, fmt.Errorf("ledger: report carries %d units, more than %d", len(wire.Units), MaxUnits)
	}
	if err := checkEpoch("milestone_summary", wire.Summary.MetricEpoch); err != nil {
		return Report{}, err
	}
	rep := Report{Sources: *wire.Sources, Units: make([]Unit, 0, len(wire.Units))}
	var err error
	if rep.FrontierTokens, err = pairedFigure("milestone_summary.frontier_tokens", wire.Summary.FrontierTokens, wire.Summary.FrontierTokensNum); err != nil {
		return Report{}, err
	}
	if rep.TotalSpend, err = pairedFigure("milestone_summary.total_spend", wire.Summary.TotalSpend, wire.Summary.TotalSpendNum); err != nil {
		return Report{}, err
	}
	for _, wu := range wire.Units {
		u, err := unitFromWire(wu)
		if err != nil {
			return Report{}, err
		}
		rep.Units = append(rep.Units, u)
	}
	return rep, nil
}

func unitFromWire(wu wireUnit) (Unit, error) {
	where := fmt.Sprintf("unit %d", wu.PullRequest)
	if err := checkEpoch(where, wu.MetricEpoch); err != nil {
		return Unit{}, err
	}
	u := Unit{PullRequest: wu.PullRequest}
	var err error
	if u.FrontierTokens, err = pairedFigure(where+" frontier_tokens", wu.FrontierTokens, wu.FrontierTokensNum); err != nil {
		return Unit{}, err
	}
	if u.Spend, err = pairedFigure(where+" spend", wu.Spend, wu.SpendAmount); err != nil {
		return Unit{}, err
	}
	if u.WallSeconds, err = vectorFigure(where+" wall_seconds", wu.WallSeconds); err != nil {
		return Unit{}, err
	}
	return u, nil
}

func checkEpoch(where, epoch string) error {
	if epoch != MetricEpoch {
		return fmt.Errorf("ledger: %s has metric epoch %q, this reader understands %q", where, epoch, MetricEpoch)
	}
	return nil
}

// pairedFigure reads a figure the engine prints twice: a display string,
// which is "not measured ..." when there is no value, and a number, which is
// left out then. Both must tell the same story.
func pairedFigure(where string, display *string, num *float64) (Figure, error) {
	if display == nil {
		return Figure{}, fmt.Errorf("ledger: %s is missing", where)
	}
	if strings.HasPrefix(*display, notMeasured) {
		if num != nil {
			return Figure{}, fmt.Errorf("ledger: %s says %q but carries the number %v", where, *display, *num)
		}
		return Figure{Reason: *display}, nil
	}
	if num == nil {
		return Figure{}, fmt.Errorf("ledger: %s says %q but carries no number", where, *display)
	}
	if err := checkValue(where, *num); err != nil {
		return Figure{}, err
	}
	value := *num
	return Figure{Value: &value, Provenance: "measured"}, nil
}

// vectorFigure reads a {value, provenance} field, which the engine writes as
// null when it has no value.
func vectorFigure(where string, v *wireVector) (Figure, error) {
	if v == nil {
		return Figure{Reason: notMeasured}, nil
	}
	if _, ok := provenances[v.Provenance]; !ok {
		return Figure{}, fmt.Errorf("ledger: %s has provenance %q, not one of measured, modeled, cited, interval", where, v.Provenance)
	}
	if v.Value == nil {
		return Figure{}, fmt.Errorf("ledger: %s has provenance %q but no value", where, v.Provenance)
	}
	if err := checkValue(where, *v.Value); err != nil {
		return Figure{}, err
	}
	value := *v.Value
	return Figure{Value: &value, Provenance: v.Provenance}, nil
}

func checkValue(where string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return fmt.Errorf("ledger: %s value %v is out of range", where, v)
	}
	return nil
}
