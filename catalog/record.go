// Package catalog defines the record and snapshot shape of the model data the
// Epitrop graph router routes on.
//
// This package holds no I/O and no source-specific logic; it is the boundary
// epitrop/internal/sources packages write into and epitrop/cmd/epitropctl
// serializes. It is also the only epitrop package praetor may import once
// the router itself is built, so it stays free of praetor internal
// dependencies and free of os/net calls.
package catalog

import (
	"math"
	"time"
)

// Kind marks whether a field's value was observed live (measured) or copied
// from a published catalog / config (declared). Never guessed: a field this
// source cannot support is left nil and explained in Record.Absent instead.
type Kind string

const (
	KindMeasured Kind = "measured"
	KindDeclared Kind = "declared"
)

// AccessPath names how a model is reached.
type AccessPath string

const (
	AccessAPI             AccessPath = "api"
	AccessSubscriptionCLI AccessPath = "subscription_cli"
	AccessGateway         AccessPath = "gateway"
	AccessLocal           AccessPath = "local"
)

// Provenance records who produced a Record's fields and when.
type Provenance struct {
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetched_at"`
	Kind      Kind      `json:"kind"`
}

// Limits holds request/token ceilings, when a source reports them.
type Limits struct {
	RPM   *int `json:"rpm,omitempty"`
	TPM   *int `json:"tpm,omitempty"`
	Daily *int `json:"daily,omitempty"`
}

// UsageWindow holds a subscription/plan usage window, when a source reports one.
type UsageWindow struct {
	UsedPercent   *float64   `json:"used_percent,omitempty"`
	WindowMinutes *int       `json:"window_minutes,omitempty"`
	ResetsAt      *time.Time `json:"resets_at,omitempty"`
	// BurnPercentPerHour is the recent burn of the window: how fast used_percent
	// rose over a short sliding window before the read (not the average since
	// the window opened), across every session on the account.
	BurnPercentPerHour *float64 `json:"burn_percent_per_hour,omitempty"`
	// ActiveAgents counts the sessions and subagents that drew on the window
	// in that sliding window.
	ActiveAgents *int `json:"active_agents,omitempty"`
}

// Record is one catalog entry: a model or a subscription usage window,
// depending on the source. Fields the source did not report are left nil
// (or empty) and named in Absent with a reason; slice 1 never guesses a value.
type Record struct {
	ModelID       string       `json:"model_id"`
	Provider      string       `json:"provider,omitempty"`
	AccessPath    AccessPath   `json:"access_path"`
	ContextWindow *int64       `json:"context_window,omitempty"`
	PriceInPerM   *float64     `json:"price_in_per_m,omitempty"`
	PriceOutPerM  *float64     `json:"price_out_per_m,omitempty"`
	Capabilities  []string     `json:"capabilities,omitempty"`
	Limits        *Limits      `json:"limits,omitempty"`
	UsageWindow   *UsageWindow `json:"usage_window,omitempty"`
	Provenance    Provenance   `json:"provenance"`
	// Absent maps a field name (e.g. "price_in_per_m") this record has no
	// value for to the reason, so a missing field always reads as a stated
	// gap rather than a silent zero value.
	Absent map[string]string `json:"absent,omitempty"`
}

// Validate reports the first structural problem with r, or nil if r is a
// well-formed catalog entry. It does not judge whether the values are
// correct, only whether the shape is sound enough to serialize and read back.
func (r Record) Validate() error {
	if r.ModelID == "" {
		return errEmptyModelID
	}
	switch r.AccessPath {
	case AccessAPI, AccessSubscriptionCLI, AccessGateway, AccessLocal:
	default:
		return errBadAccessPath
	}
	if r.Provenance.Source == "" {
		return errEmptyProvenanceSource
	}
	switch r.Provenance.Kind {
	case KindMeasured, KindDeclared:
	default:
		return errBadProvenanceKind
	}
	if r.Provenance.FetchedAt.IsZero() {
		return errZeroFetchedAt
	}
	return r.UsageWindow.validate()
}

// validate checks the fields a reader acts on without a further range check:
// a burn rate is finite and at least zero, an agent count at least zero. A nil
// window or field is valid; it means the source did not report it.
func (w *UsageWindow) validate() error {
	if w == nil {
		return nil
	}
	if b := w.BurnPercentPerHour; b != nil && (math.IsNaN(*b) || math.IsInf(*b, 0) || *b < 0) {
		return errBadBurnPercentPerHour
	}
	if w.ActiveAgents != nil && *w.ActiveAgents < 0 {
		return errNegativeActiveAgents
	}
	return nil
}
