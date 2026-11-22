package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const contractDir = "testdata/contract"

// Generated snapshots copy the first record of "from" records times, suffixing
// each model_id as <model_id>-<i> for i from 0; all other fields stay as in from.
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
	ModelID     string  `json:"model_id"`
	State       string  `json:"state"`
	UsedPercent float64 `json:"used_percent,omitempty"`
	Reason      string  `json:"reason,omitempty"`
}

func loadContractManifest(t *testing.T) contractManifest {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(contractDir, "cases.json"))
	if err != nil {
		t.Fatalf("ReadFile(cases.json) = %v, want nil", err)
	}
	var manifest contractManifest
	if err = json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("cases.json: %v", err)
	}
	return manifest
}

func loadContractCaseBytes(t *testing.T, c contractCase) []byte {
	t.Helper()
	if c.Generate != nil {
		return generateCaseSnapshot(t, c.Generate)
	}
	body, err := os.ReadFile(filepath.Join(contractDir, c.File))
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v, want nil", c.File, err)
	}
	return body
}

func generateCaseSnapshot(t *testing.T, spec *generateSpec) []byte {
	t.Helper()
	from, err := os.ReadFile(filepath.Join(contractDir, spec.From))
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v, want nil", spec.From, err)
	}
	var snap Snapshot
	if err = json.Unmarshal(from, &snap); err != nil {
		t.Fatalf("json.Unmarshal(%s) = %v, want nil", spec.From, err)
	}
	if len(snap.Records) == 0 {
		t.Fatalf("%s has no records to copy", spec.From)
	}
	base := snap.Records[0]
	records := make([]Record, spec.Records)
	for i := 0; i < spec.Records; i++ {
		rec := base
		rec.ModelID = base.ModelID + "-" + strconv.Itoa(i)
		records[i] = rec
	}
	snap.Records = records
	out, err := json.Marshal(snap)
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
	if !strings.Contains(err.Error(), c.Expect.Says) {
		return "refusal does not contain " + c.Expect.Says + ": " + err.Error()
	}
	return ""
}

func checkSourceRuns(snap Snapshot, want map[string]string) string {
	if len(want) == 0 {
		return ""
	}
	got := map[string]Status{}
	for i := 0; i < len(snap.SourceRuns); i++ {
		run := snap.SourceRuns[i]
		got[run.Source] = run.Status
	}
	if len(got) != len(want) {
		return fmt.Sprintf("source_runs count %d, want %d", len(got), len(want))
	}
	for source, status := range want {
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
	switch want.State {
	case "known":
		if want.UsedPercent != 0 {
			if got.UsedPercent == nil || *got.UsedPercent != want.UsedPercent {
				return fmt.Sprintf("used_percent %v, want %v", got.UsedPercent, want.UsedPercent)
			}
		}
		if got.Reason != "" {
			return "known reading has reason: " + got.Reason
		}
	case "unknown":
		if want.Reason != "" && !strings.Contains(got.Reason, want.Reason) {
			return fmt.Sprintf("reason %q, want substring %q", got.Reason, want.Reason)
		}
		if got.Reason == "" {
			return "unknown reading has empty reason"
		}
	}
	return ""
}

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

func TestSnapshotContractCheckerRefusesAWrongExpectation(t *testing.T) {
	manifest := loadContractManifest(t)
	byID := map[string]contractCase{}
	for i := 0; i < len(manifest.Cases); i++ {
		byID[manifest.Cases[i].ID] = manifest.Cases[i]
	}
	valid := byID["valid"]
	stale := byID["stale"]
	bad := byID["incompatible-version"]
	plants := []struct {
		name string
		c    contractCase
	}{
		{"valid planted refused", plantRefused(valid)},
		{"incompatible-version planted accepted", plantAccepted(bad)},
		{"stale second reading planted known", plantStaleKnown(stale)},
		{"valid wrong source run", plantWrongSource(valid)},
	}
	for i := 0; i < len(plants); i++ {
		data := loadContractCaseBytes(t, plants[i].c)
		if wrong := checkContractCase(t, plants[i].c, data); wrong == "" {
			t.Errorf("%s passed the checker, want a problem", plants[i].name)
		}
	}
}

func plantRefused(c contractCase) contractCase {
	c.Expect.Result = "refused"
	c.Expect.Says = "nothing matches"
	return c
}

func plantAccepted(c contractCase) contractCase {
	c.Expect.Result = "accepted"
	c.Expect.Says = ""
	return c
}

func plantStaleKnown(c contractCase) contractCase {
	if len(c.Expect.Headroom) > 1 {
		c.Expect.Headroom[1].Readings[0].State = "known"
		c.Expect.Headroom[1].Readings[0].UsedPercent = 99
		c.Expect.Headroom[1].Readings[0].Reason = ""
	}
	return c
}

func plantWrongSource(c contractCase) contractCase {
	c.Expect.SourceRuns["codex-local"] = "fail"
	return c
}

func TestSnapshotContractFixturesCoverTheFolder(t *testing.T) {
	manifest := loadContractManifest(t)
	if manifest.SchemaVersion != SnapshotSchemaVersion {
		t.Fatalf("manifest schema_version %d, want %d", manifest.SchemaVersion, SnapshotSchemaVersion)
	}
	if manifest.MaxRecords != MaxSnapshotRecords {
		t.Fatalf("manifest max_records %d, want %d", manifest.MaxRecords, MaxSnapshotRecords)
	}
	ids := map[string]struct{}{}
	fileCases := map[string]string{}
	directions := map[string]int{}
	for i := 0; i < len(manifest.Cases); i++ {
		c := manifest.Cases[i]
		if _, dup := ids[c.ID]; dup {
			t.Fatalf("duplicate case id %q", c.ID)
		}
		ids[c.ID] = struct{}{}
		hasFile := c.File != ""
		hasGen := c.Generate != nil
		if hasFile == hasGen {
			t.Fatalf("case %q must name exactly one of file or generate", c.ID)
		}
		if hasFile {
			fileCases[c.File] = c.ID
		}
		directions[c.Direction]++
		compliant := c.Expect.Result == "accepted"
		if (c.Direction == "compliant") != compliant {
			t.Fatalf("case %q direction %q vs result %q", c.ID, c.Direction, c.Expect.Result)
		}
	}
	paths, err := filepath.Glob(filepath.Join(contractDir, "*.snapshot.json"))
	if err != nil {
		t.Fatalf("Glob(contract) = %v", err)
	}
	sort.Strings(paths)
	for i := 0; i < len(paths); i++ {
		base := filepath.Base(paths[i])
		if fileCases[base] == "" {
			t.Fatalf("%s is not named by any case", base)
		}
	}
	if directions["compliant"] == 0 || directions["violation"] == 0 {
		t.Fatalf("directions = %v, want compliant and violation", directions)
	}
}
