package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

func copyStringKeyedMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	rv := reflect.ValueOf(m)
	keys := rv.MapKeys()
	for i := 0; i < len(keys); i++ {
		k := keys[i].String()
		out[k] = m[k]
	}
	return out
}

func copyRawRecordMap(m map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m))
	rv := reflect.ValueOf(m)
	keys := rv.MapKeys()
	for i := 0; i < len(keys); i++ {
		k := keys[i].String()
		out[k] = m[k]
	}
	return out
}

func generateParseFromFile(t *testing.T, fromName string) (map[string]json.RawMessage, map[string]json.RawMessage, string) {
	t.Helper()
	from, err := os.ReadFile(filepath.Join(contractDir, fromName))
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v, want nil", fromName, err)
	}
	var top map[string]json.RawMessage
	if err = json.Unmarshal(from, &top); err != nil {
		t.Fatalf("json.Unmarshal(%s) = %v, want nil", fromName, err)
	}
	rawRecords, ok := top["records"]
	if !ok {
		t.Fatalf("%s has no records field", fromName)
	}
	var records []map[string]json.RawMessage
	if err = json.Unmarshal(rawRecords, &records); err != nil {
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
	if err = json.Unmarshal(rawModel, &modelID); err != nil {
		t.Fatalf("model_id in %s: %v", fromName, err)
	}
	return top, base, modelID
}

func generateDuplicateRecords(t *testing.T, base map[string]json.RawMessage, modelID string, n int) []byte {
	t.Helper()
	outRecords := make([]map[string]json.RawMessage, n)
	for i := 0; i < n; i++ {
		rec := copyRawRecordMap(base)
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

func sourceRunKeys(want map[string]string) []string {
	keys := make([]string, 0, len(want))
	rv := reflect.ValueOf(want)
	mapKeys := rv.MapKeys()
	for i := 0; i < len(mapKeys); i++ {
		keys = append(keys, mapKeys[i].String())
	}
	sort.Strings(keys)
	return keys
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
	keys := sourceRunKeys(want)
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

func copyContractCase(c contractCase) contractCase {
	out := c
	out.Expect.SourceRuns = copyStringKeyedMap(c.Expect.SourceRuns)
	if len(c.Expect.Headroom) == 0 {
		return out
	}
	out.Expect.Headroom = make([]headroomCheck, len(c.Expect.Headroom))
	for i := 0; i < len(c.Expect.Headroom); i++ {
		out.Expect.Headroom[i] = c.Expect.Headroom[i]
		out.Expect.Headroom[i].Readings = make([]readingExpect, len(c.Expect.Headroom[i].Readings))
		copy(out.Expect.Headroom[i].Readings, c.Expect.Headroom[i].Readings)
	}
	return out
}

func float64Ptr(v float64) *float64 {
	return &v
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
	unknown := byID["unknown-headroom"]
	plants := []struct {
		name string
		c    contractCase
	}{
		{"valid planted refused", plantRefused(valid)},
		{"incompatible-version planted accepted", plantAccepted(bad)},
		{"stale second reading planted known", plantStaleKnown(stale)},
		{"valid wrong source run", plantWrongSource(valid)},
		{"valid wrong used_percent", plantWrongUsedPercent(valid)},
		{"incompatible-version wrong says", plantWrongSays(bad)},
		{"unknown-headroom wrong reason", plantWrongUnknownReason(unknown)},
		{"valid missing source_runs expect", plantNoSourceRunsExpect(valid)},
		{"valid missing used_percent expect", plantNoUsedPercentExpect(valid)},
	}
	for i := 0; i < len(plants); i++ {
		data := loadContractCaseBytes(t, plants[i].c)
		if wrong := checkContractCase(t, plants[i].c, data); wrong == "" {
			t.Errorf("%s passed the checker, want a problem", plants[i].name)
		}
	}
}

func plantRefused(c contractCase) contractCase {
	out := copyContractCase(c)
	out.Expect.Result = "refused"
	out.Expect.Says = "nothing matches"
	return out
}

func plantAccepted(c contractCase) contractCase {
	out := copyContractCase(c)
	out.Expect.Result = "accepted"
	out.Expect.Says = ""
	return out
}

func plantStaleKnown(c contractCase) contractCase {
	out := copyContractCase(c)
	if len(out.Expect.Headroom) > 1 {
		out.Expect.Headroom[1].Readings[0].State = "known"
		out.Expect.Headroom[1].Readings[0].UsedPercent = float64Ptr(99)
		out.Expect.Headroom[1].Readings[0].Reason = ""
	}
	return out
}

func plantWrongSource(c contractCase) contractCase {
	out := copyContractCase(c)
	out.Expect.SourceRuns["codex-local"] = "fail"
	return out
}

func plantWrongUsedPercent(c contractCase) contractCase {
	out := copyContractCase(c)
	if len(out.Expect.Headroom) > 0 && len(out.Expect.Headroom[0].Readings) > 0 {
		out.Expect.Headroom[0].Readings[0].UsedPercent = float64Ptr(41)
	}
	return out
}

func plantWrongSays(c contractCase) contractCase {
	out := copyContractCase(c)
	out.Expect.Says = "found 3"
	return out
}

func plantWrongUnknownReason(c contractCase) contractCase {
	out := copyContractCase(c)
	if len(out.Expect.Headroom) > 0 && len(out.Expect.Headroom[0].Readings) > 0 {
		out.Expect.Headroom[0].Readings[0].Reason = "stale:"
	}
	return out
}

func plantNoSourceRunsExpect(c contractCase) contractCase {
	out := copyContractCase(c)
	out.Expect.SourceRuns = nil
	return out
}

func plantNoUsedPercentExpect(c contractCase) contractCase {
	out := copyContractCase(c)
	if len(out.Expect.Headroom) > 0 && len(out.Expect.Headroom[0].Readings) > 0 {
		out.Expect.Headroom[0].Readings[0].UsedPercent = nil
	}
	return out
}

func TestSnapshotContractFixturesCoverTheFolder(t *testing.T) {
	manifest := loadContractManifest(t)
	coverManifestSchema(t, manifest)
	fileCases := coverCollectCases(t, manifest)
	coverSnapshotFiles(t, fileCases)
}

func coverManifestSchema(t *testing.T, manifest contractManifest) {
	t.Helper()
	if manifest.SchemaVersion != SnapshotSchemaVersion {
		t.Fatalf("manifest schema_version %d, want %d", manifest.SchemaVersion, SnapshotSchemaVersion)
	}
	if manifest.MaxRecords != MaxSnapshotRecords {
		t.Fatalf("manifest max_records %d, want %d", manifest.MaxRecords, MaxSnapshotRecords)
	}
}

func coverCollectCases(t *testing.T, manifest contractManifest) map[string]string {
	t.Helper()
	ids := map[string]struct{}{}
	fileCases := map[string]string{}
	directions := map[string]int{}
	for i := 0; i < len(manifest.Cases); i++ {
		c := manifest.Cases[i]
		if _, dup := ids[c.ID]; dup {
			t.Fatalf("duplicate case id %q", c.ID)
		}
		ids[c.ID] = struct{}{}
		coverValidateCaseShape(t, c)
		coverValidateCaseExpect(t, c)
		if c.File != "" {
			if prev := fileCases[c.File]; prev != "" {
				t.Fatalf("file %q named by cases %q and %q", c.File, prev, c.ID)
			}
			fileCases[c.File] = c.ID
		}
		directions[c.Direction]++
		compliant := c.Expect.Result == "accepted"
		if (c.Direction == "compliant") != compliant {
			t.Fatalf("case %q direction %q vs result %q", c.ID, c.Direction, c.Expect.Result)
		}
	}
	if directions["compliant"] == 0 || directions["violation"] == 0 {
		t.Fatalf("directions = %v, want compliant and violation", directions)
	}
	return fileCases
}

func coverValidateCaseShape(t *testing.T, c contractCase) {
	t.Helper()
	hasFile := c.File != ""
	hasGen := c.Generate != nil
	if hasFile == hasGen {
		t.Fatalf("case %q must name exactly one of file or generate", c.ID)
	}
	if hasFile {
		path := filepath.Join(contractDir, c.File)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("case %q file %q: %v", c.ID, c.File, err)
		}
		return
	}
	from := filepath.Join(contractDir, c.Generate.From)
	if _, err := os.Stat(from); err != nil {
		t.Fatalf("case %q generate.from %q: %v", c.ID, c.Generate.From, err)
	}
}

func coverValidateCaseExpect(t *testing.T, c contractCase) {
	t.Helper()
	if c.Expect.Result == "refused" {
		if c.Expect.Says == "" {
			t.Fatalf("case %q refused with empty says", c.ID)
		}
		return
	}
	if c.Expect.Result != "accepted" {
		t.Fatalf("case %q unknown result %q", c.ID, c.Expect.Result)
	}
	if len(c.Expect.SourceRuns) == 0 {
		t.Fatalf("case %q accepted with no source_runs", c.ID)
	}
	for i := 0; i < len(c.Expect.Headroom); i++ {
		coverValidateReadings(t, c.ID, c.Expect.Headroom[i].Readings)
	}
}

func coverValidateReadings(t *testing.T, caseID string, readings []readingExpect) {
	t.Helper()
	for i := 0; i < len(readings); i++ {
		r := readings[i]
		switch r.State {
		case "known":
			if r.UsedPercent == nil {
				t.Fatalf("case %q reading[%d] known without used_percent", caseID, i)
			}
		case "unknown":
			if r.Reason == "" {
				t.Fatalf("case %q reading[%d] unknown without reason", caseID, i)
			}
			if r.UsedPercent != nil {
				t.Fatalf("case %q reading[%d] unknown with used_percent", caseID, i)
			}
		default:
			t.Fatalf("case %q reading[%d] bad state %q", caseID, i, r.State)
		}
	}
}

func coverSnapshotFiles(t *testing.T, fileCases map[string]string) {
	t.Helper()
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
}
