package catalog

import (
	"encoding/json"
	"fmt"
	"time"
)

// SnapshotSchemaVersion is the current JSON snapshot contract version.
const SnapshotSchemaVersion = 1

// MaxSnapshotRecords bounds how many records one snapshot may hold (HISS-02:
// every loop over Snapshot.Records carries this as its scalar upper bound).
const MaxSnapshotRecords = 25000

// SourceRun reports one sync source's outcome: whether it ran, how many
// records it produced, and why not when it did not. Sources fail
// independently, so a snapshot carries one SourceRun per attempted source.
type SourceRun struct {
	Source string `json:"source"`
	Status Status `json:"status"`
	Count  int    `json:"count"`
	Detail string `json:"detail,omitempty"`
}

// Status is a source run's outcome.
type Status string

const (
	StatusOK Status = "ok"
	// StatusDegraded means records were produced but upstream data was lost.
	StatusDegraded Status = "degraded"
	StatusSkip     Status = "skip"
	StatusFail     Status = "fail"
)

// Snapshot is the file-based catalog sync writes: every record gathered in
// one run, plus a per-source outcome line for each source attempted.
type Snapshot struct {
	SchemaVersion int         `json:"schema_version"`
	GeneratedAt   time.Time   `json:"generated_at"`
	Records       []Record    `json:"records"`
	SourceRuns    []SourceRun `json:"source_runs"`
}

// Validate reports the first structural problem with the snapshot: too many
// records, or any record that fails its own Validate.
func (s Snapshot) Validate() error {
	if s.SchemaVersion != SnapshotSchemaVersion {
		return fmt.Errorf("catalog: unsupported snapshot schema_version: found %d, supported %d", s.SchemaVersion, SnapshotSchemaVersion)
	}
	if len(s.Records) > MaxSnapshotRecords {
		return fmt.Errorf("catalog: snapshot has %d records, exceeds %d", len(s.Records), MaxSnapshotRecords)
	}
	for i, r := range s.Records {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("catalog: record %d: %w", i, err)
		}
	}
	return nil
}

// Marshal renders the snapshot as indented JSON. It validates first so a
// malformed snapshot never reaches disk.
func (s Snapshot) Marshal() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(s, "", "  ")
}

// ParseSnapshot decodes JSON bytes into a Snapshot and validates the result.
func ParseSnapshot(data []byte) (Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return Snapshot{}, fmt.Errorf("catalog: parse snapshot: %w", err)
	}
	if err := s.Validate(); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}
