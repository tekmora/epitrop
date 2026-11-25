// Package units keeps the run record: one entry per work unit with its status, written to
// the signed event log at each status change, so that after a crash the open units and
// what is safe to relaunch can be read back instead of reconstructed from transcripts.
package units

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/tekmora/epitrop/internal/eventlog"
	"github.com/tekmora/epitrop/internal/graph"
)

const (
	MaxUnreadNotes   = 256
	MaxUnits         = 10000
	MaxNoteBytes     = 4096
	MaxFieldBytes    = 512
	MaxIdentityBytes = 8192
	MaxPR            = 1 << 30
	MaxEvidenceLines = 1 << 30
)

var (
	unitIDRegex      = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}(#[0-9]{1,9}|:[a-z0-9-]{1,40})$`)
	senderRegex      = regexp.MustCompile(`^[A-Za-z0-9._@/:-]{1,64}$`)
	identityKeyRegex = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	evidenceDigest   = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
)

// RecordPayload is the payload of unit.recorded. A nil field keeps the unit's previous
// value: worktree, branch, pull request, lane and the identity fields are sticky.
type RecordPayload struct {
	Status        string          `json:"status"`
	Worktree      *string         `json:"worktree,omitempty"`
	Branch        *string         `json:"branch,omitempty"`
	PR            *int            `json:"pr,omitempty"`
	Lane          *string         `json:"lane,omitempty"`
	By            *string         `json:"by,omitempty"`
	Target        *string         `json:"target,omitempty"`
	ResolvedModel *string         `json:"resolved_model,omitempty"`
	Identity      json.RawMessage `json:"identity,omitempty"`
	IdentityKey   *string         `json:"identity_key,omitempty"`
	Reopen        bool            `json:"reopen,omitempty"`
	// Evidence is sticky like the fields above: nil keeps the unit's pointer, a new one
	// replaces it.
	Evidence *eventlog.UnitEvidence `json:"evidence,omitempty"`
}

type NotePayload struct {
	From string `json:"from"`
	Text string `json:"text"`
}

type NoteReadPayload struct {
	UptoSeq uint64 `json:"upto_seq"`
}

type ResumeRow struct {
	ID         string
	Status     string
	UpdatedAt  string
	Worktree   string
	Branch     string
	PR         int
	Lane       string
	Relaunch   string
	Evidence   string
	Node       string
	By         string
	Assignment *eventlog.UnitAssignment
}

// IsValidStatus reports whether s is a status of the task graph's vocabulary, the one
// vocabulary for work status in this repository (internal/graph).
func IsValidStatus(s string) bool {
	return graph.KnownStatus(graph.StatusName(s))
}

// IsTerminalStatus reports whether nothing follows s: landed or dropped.
func IsTerminalStatus(s string) bool {
	return graph.TerminalStatus(graph.StatusName(s))
}

func IsValidID(id string) bool {
	return unitIDRegex.MatchString(id)
}

// transitionRefusal says why a unit may not go from one status to another, or nil. A record
// that keeps the status only updates fields. Every other move follows the task graph's
// transition table. reopen is the explicit override for a landed or dropped unit, which the
// table never moves.
func transitionRefusal(from string, to string, reopen bool) error {
	if from == to {
		return nil
	}
	if reopen && IsTerminalStatus(from) {
		return nil
	}
	return graph.AllowTransition(graph.StatusName(from), graph.StatusName(to))
}

// moveAllowed is transitionRefusal as a yes or no, for the reducer, which does not report
// why it left a record unapplied.
func moveAllowed(from string, to string, reopen bool) bool {
	return transitionRefusal(from, to, reopen) == nil
}

// RelaunchRule says whether a crashed unit may simply be started again. Only work that
// has not reached a pull request is safe: anything later may already be under review or
// landed, and starting it again would redo or duplicate it.
func RelaunchRule(u eventlog.Unit) (bool, string) {
	status := graph.StatusName(u.Status)
	if status == graph.StatusBlocked {
		if u.Preempted != nil {
			return false, "preempted: " + u.Preempted.Reason
		}
		return false, "blocked"
	}
	if u.PR != 0 {
		return false, fmt.Sprintf("has PR #%d; check its state before relaunching", u.PR)
	}
	switch status {
	case graph.StatusReady, graph.StatusClaimed, graph.StatusImplementing:
		return true, ""
	case graph.StatusProposed:
		return false, "proposed; not ready to start"
	}
	return false, "in " + u.Status
}

// PreemptPayload is the payload of unit.preempted.
type PreemptPayload struct {
	Reason string `json:"reason"`
	By     string `json:"by,omitempty"`
}

func validatePreemptInput(id string, reason string, by string) error {
	if !IsValidID(id) {
		return fmt.Errorf("units: invalid unit id %q", id)
	}
	if len(strings.TrimSpace(reason)) < 1 || len(reason) > MaxNoteBytes {
		return fmt.Errorf("units: preempt reason must be 1..%d bytes", MaxNoteBytes)
	}
	if by != "" && !senderRegex.MatchString(by) {
		return fmt.Errorf("units: invalid sender %q", by)
	}
	return nil
}

// validateRecordPayload checks everything about a record that does not depend on the log.
func validateRecordPayload(payload RecordPayload) error {
	if !IsValidStatus(payload.Status) {
		return fmt.Errorf("units: unknown status %q", payload.Status)
	}
	fields := []struct {
		name  string
		value *string
	}{
		{"worktree", payload.Worktree}, {"branch", payload.Branch}, {"lane", payload.Lane},
		{"by", payload.By}, {"target", payload.Target}, {"resolved_model", payload.ResolvedModel},
	}
	for i := 0; i < len(fields); i++ {
		if fields[i].value == nil {
			continue
		}
		if err := checkPlainField(fields[i].name, *fields[i].value); err != nil {
			return err
		}
	}
	if payload.PR != nil && (*payload.PR < 0 || *payload.PR > MaxPR) {
		return fmt.Errorf("units: pr must be 0..%d", MaxPR)
	}
	if err := validateEvidence(payload.Evidence); err != nil {
		return err
	}
	return validateIdentity(payload.Identity, payload.IdentityKey)
}

// validateEvidence checks an evidence pointer: a path that prints on one line, a SHA-256
// digest or a prefix of at least 12 hex digits, and a line count.
func validateEvidence(evidence *eventlog.UnitEvidence) error {
	if evidence == nil {
		return nil
	}
	if evidence.Path == "" {
		return errors.New("units: evidence needs a path")
	}
	if err := checkPlainField("evidence path", evidence.Path); err != nil {
		return err
	}
	if !evidenceDigest.MatchString(evidence.SHA256) {
		return errors.New("units: evidence sha256 must be 12..64 lower-case hex digits")
	}
	if evidence.Lines < 0 || evidence.Lines > MaxEvidenceLines {
		return fmt.Errorf("units: evidence lines must be 0..%d", MaxEvidenceLines)
	}
	return nil
}

// ParseEvidence reads an evidence pointer in the form "<path> sha256:<hex> lines:<n>". The
// path is everything before the last two fields, so it may hold spaces.
func ParseEvidence(text string) (*eventlog.UnitEvidence, error) {
	fields := strings.Fields(text)
	if len(fields) < 3 {
		return nil, errors.New("units: evidence must be \"<path> sha256:<hex> lines:<n>\"")
	}
	digest, hasDigest := strings.CutPrefix(fields[len(fields)-2], "sha256:")
	count, hasLines := strings.CutPrefix(fields[len(fields)-1], "lines:")
	if !hasDigest || !hasLines {
		return nil, errors.New("units: evidence must end in \"sha256:<hex> lines:<n>\"")
	}
	lines, err := strconv.Atoi(count)
	if err != nil {
		return nil, fmt.Errorf("units: evidence lines: %w", err)
	}
	evidence := &eventlog.UnitEvidence{Path: strings.Join(fields[:len(fields)-2], " "), SHA256: digest, Lines: lines}
	if err = validateEvidence(evidence); err != nil {
		return nil, err
	}
	return evidence, nil
}

// checkPlainField keeps a value printable on one tab-separated line.
func checkPlainField(name string, value string) error {
	if len(value) > MaxFieldBytes {
		return fmt.Errorf("units: %s exceeds %d bytes", name, MaxFieldBytes)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("units: %s holds a control character", name)
		}
	}
	return nil
}

// validateIdentity accepts Praetor's run identity object as given. It checks only what
// Praetor itself refuses: an identity without the model that ran and the harness and
// version that ran it. The key belongs to the identity, so it is refused without one.
func validateIdentity(identity json.RawMessage, key *string) error {
	if len(identity) == 0 {
		if key != nil {
			return errors.New("units: identity_key needs the identity it was computed from")
		}
		return nil
	}
	if len(identity) > MaxIdentityBytes {
		return fmt.Errorf("units: identity exceeds %d bytes", MaxIdentityBytes)
	}
	var required struct {
		PhysicalModel  string `json:"physical_model"`
		Harness        string `json:"harness"`
		HarnessVersion string `json:"harness_version"`
	}
	if err := json.Unmarshal(identity, &required); err != nil {
		return fmt.Errorf("units: identity is not a JSON object: %w", err)
	}
	if required.PhysicalModel == "" || required.Harness == "" || required.HarnessVersion == "" {
		return errors.New("units: identity needs physical_model, harness and harness_version")
	}
	if key != nil && !identityKeyRegex.MatchString(*key) {
		return errors.New("units: identity_key must be sha256:<64 hex>")
	}
	return nil
}

func validateNoteInput(id string, from string, text string) error {
	if !IsValidID(id) {
		return fmt.Errorf("units: invalid unit id %q", id)
	}
	if !senderRegex.MatchString(from) {
		return fmt.Errorf("units: invalid sender %q", from)
	}
	if len(text) < 1 || len(text) > MaxNoteBytes {
		return fmt.Errorf("units: note text must be 1..%d bytes", MaxNoteBytes)
	}
	return nil
}
