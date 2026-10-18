package catalog

import "errors"

var (
	errEmptyModelID          = errors.New("catalog: record model_id is empty")
	errBadAccessPath         = errors.New("catalog: record access_path is not one of api|subscription_cli|gateway|local")
	errEmptyProvenanceSource = errors.New("catalog: record provenance.source is empty")
	errBadProvenanceKind     = errors.New("catalog: record provenance.kind is not measured|declared")
	errZeroFetchedAt         = errors.New("catalog: record provenance.fetched_at is zero")
	errBadBurnPercentPerHour = errors.New("catalog: record usage_window.burn_percent_per_hour is not a finite number of at least zero")
	errNegativeActiveAgents  = errors.New("catalog: record usage_window.active_agents is negative")
)
