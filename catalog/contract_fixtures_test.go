package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const contractDir = "testdata/contract"

type contractManifest struct {
	Rule          string         `json:"rule"`
	SchemaVersion int            `json:"schema_version"`
	MaxRecords    int            `json:"max_records"`
	Cases         []contractCase `json:"cases"`
}

type contractCase struct {
	ID        string        `json:"id"`
	File      string        `json:"file,omitempty"`
	Generate  *generateSpec `json:"generate,omitempty"`
	Direction string        `json:"direction"`
	Expect    expectSpec    `json:"expect"`
}

type generateSpec struct {
	From    string `json:"from"`
	Records int    `json:"records"`
}

type expectSpec struct {
	Result     string            `json:"result"`
	Says       string            `json:"says,omitempty"`
	SourceRuns map[string]string `json:"source_runs,omitempty"`
	Headroom   []headroomCheck   `json:"headroom,omitempty"`
}

type headroomCheck struct {
	Now      string          `json:"now"`
	MaxAge   string          `json:"max_age"`
	Readings []readingExpect `json:"readings"`
}

type readingExpect struct {
	ModelID     string   `json:"model_id"`
	State       string   `json:"state"`
	UsedPercent *float64 `json:"used_percent,omitempty"`
	Reason      string   `json:"reason,omitempty"`
}

func openContractFile(name string) ([]byte, error) {
	root, err := os.OpenRoot(contractDir)
	if err != nil {
		return nil, err
	}
	body, readErr := root.ReadFile(name)
	closeErr := root.Close()
	return body, errors.Join(readErr, closeErr)
}

func readContractFile(t *testing.T, name string) []byte {
	t.Helper()
	body, err := openContractFile(name)
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v, want nil", name, err)
	}
	return body
}

func checkContractNameErr(t *testing.T, fn, name, want string, err error) {
	t.Helper()
	switch want {
	case "ok":
		if err != nil {
			t.Fatalf("%s(%q) = %v, want %s", fn, name, err, want)
		}
	case "escapes":
		if err == nil {
			t.Fatalf("%s(%q) = %v, want %s", fn, name, err, want)
		}
		if !strings.Contains(err.Error(), "path escapes from parent") {
			t.Fatalf("%s(%q) = %v, want %s", fn, name, err, want)
		}
	case "missing":
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s(%q) = %v, want %s", fn, name, err, want)
		}
	default:
		t.Fatalf("unknown want %q", want)
	}
}

func TestSnapshotContractFixturesReadRefusesAnEscapingName(t *testing.T) {
	rows := []struct {
		name string
		want string
	}{
		{name: "valid.snapshot.json", want: "ok"},
		{name: "../contract/valid.snapshot.json", want: "escapes"},
		{name: "/etc/hostname", want: "escapes"},
		{name: "missing.snapshot.json", want: "missing"},
	}
	for i := 0; i < len(rows); i++ {
		row := rows[i]
		t.Run(row.name, func(t *testing.T) {
			body, err := openContractFile(row.name)
			checkContractNameErr(t, "openContractFile", row.name, row.want, err)
			if row.want == "ok" && len(body) == 0 {
				t.Fatalf("openContractFile(%q) body length 0, want > 0", row.name)
			}
			statErr := statContractFile(row.name)
			checkContractNameErr(t, "statContractFile", row.name, row.want, statErr)
		})
	}
}

func loadContractManifest(t *testing.T) contractManifest {
	t.Helper()
	body := readContractFile(t, "cases.json")
	var manifest contractManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("cases.json: %v", err)
	}
	return manifest
}

func loadContractCaseBytes(t *testing.T, c contractCase) []byte {
	t.Helper()
	if c.Generate != nil {
		return generateCaseSnapshot(t, c.Generate)
	}
	return readContractFile(t, c.File)
}

func generateParseFromFile(t *testing.T, fromName string) (map[string]json.RawMessage, map[string]json.RawMessage, string) {
	t.Helper()
	from := readContractFile(t, fromName)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(from, &top); err != nil {
		t.Fatalf("json.Unmarshal(%s) = %v, want nil", fromName, err)
	}
	rawRecords, ok := top["records"]
	if !ok {
		t.Fatalf("%s has no records field", fromName)
	}
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(rawRecords, &records); err != nil {
		t.Fatalf("json.Unmarshal(%s records) = %v", fromName, err)
	}
	if len(records) == 0 {
		t.Fatalf("%s has no records to copy", fromName)
	}
	base := records[0]
	rawModel, ok := base["model_id"]
	if !ok {
		t.Fatalf("%s first record has no model_id", fromName)
	}
	var modelID string
	if err := json.Unmarshal(rawModel, &modelID); err != nil {
		t.Fatalf("model_id in %s: %v", fromName, err)
	}
	return top, base, modelID
}

func generateDuplicateRecords(t *testing.T, base map[string]json.RawMessage, modelID string, n int) []byte {
	t.Helper()
	outRecords := make([]map[string]json.RawMessage, n)
	for i := 0; i < n; i++ {
		rec := maps.Clone(base)
		suffix, err := json.Marshal(modelID + "-" + strconv.Itoa(i))
		if err != nil {
			t.Fatalf("json.Marshal model_id suffix: %v", err)
		}
		rec["model_id"] = suffix
		outRecords[i] = rec
	}
	marshaledRecords, err := json.Marshal(outRecords)
	if err != nil {
		t.Fatalf("json.Marshal records: %v", err)
	}
	return marshaledRecords
}

func generateCaseSnapshot(t *testing.T, spec *generateSpec) []byte {
	t.Helper()
	top, base, modelID := generateParseFromFile(t, spec.From)
	top["records"] = generateDuplicateRecords(t, base, modelID, spec.Records)
	out, err := json.Marshal(top)
	if err != nil {
		t.Fatalf("json.Marshal generated snapshot = %v, want nil", err)
	}
	return out
}

func checkContractCase(t *testing.T, c contractCase, data []byte) string {
	t.Helper()
	snap, err := ParseSnapshot(data)
	wantAccept := c.Expect.Result == "accepted"
	if wantAccept != (err == nil) {
		if wantAccept {
			return "expected accepted, refused: " + err.Error()
		}
		return "expected refused, accepted"
	}
	if err != nil {
		return checkRefusal(c, err)
	}
	if wrong := checkSourceRuns(snap, c.Expect.SourceRuns); wrong != "" {
		return wrong
	}
	return checkHeadroomCases(t, snap, c.Expect.Headroom)
}

func checkRefusal(c contractCase, err error) string {
	if c.Expect.Says == "" {
		return "refusal case missing says"
	}
	if !strings.Contains(err.Error(), c.Expect.Says) {
		return "refusal does not contain " + c.Expect.Says + ": " + err.Error()
	}
	return ""
}

func checkSourceRuns(snap Snapshot, want map[string]string) string {
	got := map[string]Status{}
	for i := 0; i < len(snap.SourceRuns); i++ {
		run := snap.SourceRuns[i]
		got[run.Source] = run.Status
	}
	if len(want) == 0 {
		if len(got) > 0 {
			return fmt.Sprintf("source_runs expectation empty, snapshot has %d", len(got))
		}
		return ""
	}
	if len(got) != len(want) {
		return fmt.Sprintf("source_runs count %d, want %d", len(got), len(want))
	}
	keys := slices.Sorted(maps.Keys(want))
	for i := 0; i < len(keys); i++ {
		source := keys[i]
		status := want[source]
		if got[source] != Status(status) {
			return fmt.Sprintf("source %s status %q, want %q", source, got[source], status)
		}
	}
	return ""
}

func checkHeadroomCases(t *testing.T, snap Snapshot, checks []headroomCheck) string {
	t.Helper()
	for i := 0; i < len(checks); i++ {
		if wrong := checkOneHeadroomCase(snap, checks[i]); wrong != "" {
			return fmt.Sprintf("headroom[%d]: %s", i, wrong)
		}
	}
	return ""
}

func checkOneHeadroomCase(snap Snapshot, check headroomCheck) string {
	now, err := time.Parse(time.RFC3339, check.Now)
	if err != nil {
		return "bad now: " + err.Error()
	}
	maxAge, err := time.ParseDuration(check.MaxAge)
	if err != nil {
		return "bad max_age: " + err.Error()
	}
	got, err := ReadHeadroom(snap, now, maxAge)
	if err != nil {
		return "ReadHeadroom: " + err.Error()
	}
	if len(got) != len(check.Readings) {
		return fmt.Sprintf("%d readings, want %d", len(got), len(check.Readings))
	}
	for i := 0; i < len(check.Readings); i++ {
		if wrong := compareReading(got[i], check.Readings[i]); wrong != "" {
			return fmt.Sprintf("reading[%d]: %s", i, wrong)
		}
	}
	return ""
}

func compareReading(got Headroom, want readingExpect) string {
	if got.ModelID != want.ModelID {
		return fmt.Sprintf("model_id %q, want %q", got.ModelID, want.ModelID)
	}
	if string(got.State) != want.State {
		return fmt.Sprintf("state %q, want %q", got.State, want.State)
	}
	if want.State == "known" {
		return compareKnownReading(got, want)
	}
	if want.State == "unknown" {
		return compareUnknownReading(got, want)
	}
	return ""
}

func compareKnownReading(got Headroom, want readingExpect) string {
	if want.Reason != "" {
		return "known reading expectation names reason"
	}
	if want.UsedPercent == nil {
		return "known reading expectation missing used_percent"
	}
	if got.UsedPercent == nil {
		return fmt.Sprintf("used_percent nil, want %v", *want.UsedPercent)
	}
	if *got.UsedPercent != *want.UsedPercent {
		return fmt.Sprintf("used_percent %v, want %v", *got.UsedPercent, *want.UsedPercent)
	}
	if got.Reason != "" {
		return "known reading has reason: " + got.Reason
	}
	return ""
}

func compareUnknownReading(got Headroom, want readingExpect) string {
	if want.Reason == "" {
		return "unknown reading expectation missing reason"
	}
	if want.UsedPercent != nil {
		return "unknown reading expectation names used_percent"
	}
	if got.UsedPercent != nil {
		return fmt.Sprintf("unknown reading has used_percent %v", *got.UsedPercent)
	}
	if got.Reason == "" {
		return "unknown reading has empty reason"
	}
	if !strings.Contains(got.Reason, want.Reason) {
		return fmt.Sprintf("reason %q, want substring %q", got.Reason, want.Reason)
	}
	return ""
}

// TestSnapshotContractFixtures replays each case in cases.json. A generated case
// takes the snapshot named in from and replaces its records with records copies of
// its first record, copy i (from 0) with model_id <model_id>-<i>, everything else
// unchanged; generated snapshots are built in memory, never written to the folder.
func TestSnapshotContractFixtures(t *testing.T) {
	manifest := loadContractManifest(t)
	for i := 0; i < len(manifest.Cases); i++ {
		c := manifest.Cases[i]
		t.Run(c.ID, func(t *testing.T) {
			data := loadContractCaseBytes(t, c)
			if wrong := checkContractCase(t, c, data); wrong != "" {
				t.Error(wrong)
			}
		})
	}
}
