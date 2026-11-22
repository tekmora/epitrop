package catalog

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

func copyContractCase(c contractCase) contractCase {
	out := c
	out.Expect.SourceRuns = maps.Clone(c.Expect.SourceRuns)
	if c.Generate != nil {
		gen := *c.Generate
		out.Generate = &gen
	}
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

func copyContractManifest(m contractManifest) contractManifest {
	out := m
	out.Cases = make([]contractCase, len(m.Cases))
	for i := 0; i < len(m.Cases); i++ {
		out.Cases[i] = copyContractCase(m.Cases[i])
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
		{"valid known reading names reason", plantValidKnownReason(valid)},
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

func plantValidKnownReason(c contractCase) contractCase {
	out := copyContractCase(c)
	if len(out.Expect.Headroom) > 0 && len(out.Expect.Headroom[0].Readings) > 0 {
		out.Expect.Headroom[0].Readings[0].Reason = "stale:"
	}
	return out
}

func problemManifestSchemaVersion(m contractManifest) string {
	if m.SchemaVersion != SnapshotSchemaVersion {
		return fmt.Sprintf("schema_version %d, want %d", m.SchemaVersion, SnapshotSchemaVersion)
	}
	return ""
}

func problemManifestMaxRecords(m contractManifest) string {
	if m.MaxRecords != MaxSnapshotRecords {
		return fmt.Sprintf("max_records %d, want %d", m.MaxRecords, MaxSnapshotRecords)
	}
	return ""
}

func collectManifestFileCases(cases []contractCase) (map[string]string, string) {
	ids := map[string]struct{}{}
	fileCases := map[string]string{}
	for i := 0; i < len(cases); i++ {
		c := cases[i]
		if _, dup := ids[c.ID]; dup {
			return nil, fmt.Sprintf("duplicate case id %q", c.ID)
		}
		ids[c.ID] = struct{}{}
		if c.File == "" {
			continue
		}
		if prev := fileCases[c.File]; prev != "" {
			return nil, fmt.Sprintf("file %q named by cases %q and %q", c.File, prev, c.ID)
		}
		fileCases[c.File] = c.ID
	}
	return fileCases, ""
}

func problemCaseFileOrGenerate(c contractCase) string {
	hasFile := c.File != ""
	hasGen := c.Generate != nil
	if hasFile == hasGen {
		return fmt.Sprintf("case %q must name exactly one of file or generate", c.ID)
	}
	return ""
}

func problemCaseNamedFileExists(c contractCase) string {
	if c.File == "" {
		return ""
	}
	path := filepath.Join(contractDir, c.File)
	if _, err := os.Stat(path); err != nil {
		return fmt.Sprintf("case %q file %q: %v", c.ID, c.File, err)
	}
	return ""
}

func problemCaseGenerateFromExists(c contractCase) string {
	if c.Generate == nil {
		return ""
	}
	from := filepath.Join(contractDir, c.Generate.From)
	if _, err := os.Stat(from); err != nil {
		return fmt.Sprintf("case %q generate.from %q: %v", c.ID, c.Generate.From, err)
	}
	return ""
}

func problemCaseResult(c contractCase) string {
	if c.Expect.Result != "accepted" && c.Expect.Result != "refused" {
		return fmt.Sprintf("case %q unknown result %q", c.ID, c.Expect.Result)
	}
	return ""
}

func problemCaseDirectionVsResult(c contractCase) string {
	compliant := c.Expect.Result == "accepted"
	if (c.Direction == "compliant") != compliant {
		return fmt.Sprintf("case %q direction %q vs result %q", c.ID, c.Direction, c.Expect.Result)
	}
	return ""
}

func problemBothDirections(cases []contractCase) string {
	directions := map[string]int{}
	for i := 0; i < len(cases); i++ {
		directions[cases[i].Direction]++
	}
	if directions["compliant"] == 0 || directions["violation"] == 0 {
		return "directions missing compliant or violation"
	}
	return ""
}

func problemCaseExpectRules(c contractCase) string {
	if c.Expect.Result == "refused" {
		if c.Expect.Says == "" {
			return fmt.Sprintf("case %q refused with empty says", c.ID)
		}
		if len(c.Expect.SourceRuns) > 0 {
			return fmt.Sprintf("case %q refused names source_runs", c.ID)
		}
		if len(c.Expect.Headroom) > 0 {
			return fmt.Sprintf("case %q refused names headroom", c.ID)
		}
		return ""
	}
	if c.Expect.Says != "" {
		return fmt.Sprintf("case %q accepted names says", c.ID)
	}
	if len(c.Expect.SourceRuns) == 0 {
		return fmt.Sprintf("case %q accepted with no source_runs", c.ID)
	}
	for i := 0; i < len(c.Expect.Headroom); i++ {
		if p := problemReadingRules(c.ID, c.Expect.Headroom[i].Readings); p != "" {
			return p
		}
	}
	return ""
}

func problemReadingRules(caseID string, readings []readingExpect) string {
	for i := 0; i < len(readings); i++ {
		r := readings[i]
		switch r.State {
		case "known":
			if r.UsedPercent == nil {
				return fmt.Sprintf("case %q reading[%d] known without used_percent", caseID, i)
			}
			if r.Reason != "" {
				return fmt.Sprintf("case %q reading[%d] known with reason", caseID, i)
			}
		case "unknown":
			if r.Reason == "" {
				return fmt.Sprintf("case %q reading[%d] unknown without reason", caseID, i)
			}
			if r.UsedPercent != nil {
				return fmt.Sprintf("case %q reading[%d] unknown with used_percent", caseID, i)
			}
		default:
			return fmt.Sprintf("case %q reading[%d] bad state %q", caseID, i, r.State)
		}
	}
	return ""
}

func problemFolderSnapshotListing(names []string, fileCases map[string]string) string {
	for i := 0; i < len(names); i++ {
		name := names[i]
		if fileCases[name] == "" {
			return fmt.Sprintf("%s is not named by cases", name)
		}
	}
	return ""
}

func firstCoverCaseProblems(c contractCase) string {
	if p := problemCaseFileOrGenerate(c); p != "" {
		return p
	}
	if p := problemCaseNamedFileExists(c); p != "" {
		return p
	}
	if p := problemCaseGenerateFromExists(c); p != "" {
		return p
	}
	if p := problemCaseResult(c); p != "" {
		return p
	}
	if p := problemCaseDirectionVsResult(c); p != "" {
		return p
	}
	return problemCaseExpectRules(c)
}

func firstCoverProblem(m contractManifest, folderNames []string, checkFolder bool) string {
	if p := problemManifestSchemaVersion(m); p != "" {
		return p
	}
	if p := problemManifestMaxRecords(m); p != "" {
		return p
	}
	fileCases, p := collectManifestFileCases(m.Cases)
	if p != "" {
		return p
	}
	for i := 0; i < len(m.Cases); i++ {
		if p := firstCoverCaseProblems(m.Cases[i]); p != "" {
			return p
		}
	}
	if p := problemBothDirections(m.Cases); p != "" {
		return p
	}
	if !checkFolder {
		return ""
	}
	return problemFolderSnapshotListing(folderNames, fileCases)
}

func sortedContractSnapshotNames(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(contractDir, "*.snapshot.json"))
	if err != nil {
		t.Fatalf("Glob(contract) = %v", err)
	}
	sort.Strings(paths)
	names := make([]string, len(paths))
	for i := 0; i < len(paths); i++ {
		names[i] = filepath.Base(paths[i])
	}
	return names
}

func TestSnapshotContractFixturesCoverTheFolder(t *testing.T) {
	manifest := loadContractManifest(t)
	names := sortedContractSnapshotNames(t)
	if p := firstCoverProblem(manifest, names, true); p != "" {
		t.Fatal(p)
	}
}

func assertCoverPlant(t *testing.T, name, want string, m contractManifest, folder []string, checkFolder bool) {
	t.Helper()
	p := firstCoverProblem(m, folder, checkFolder)
	if want == "" {
		if p != "" {
			t.Fatalf("%s problem %q, want clean", name, p)
		}
		return
	}
	if p == "" {
		t.Fatalf("%s passed cover checks, want problem containing %q", name, want)
	}
	if !strings.Contains(p, want) {
		t.Fatalf("%s problem %q, want substring %q", name, p, want)
	}
}

func TestSnapshotContractFixturesCoverRefusesAPlantedManifest(t *testing.T) {
	base := copyContractManifest(loadContractManifest(t))
	realFolder := sortedContractSnapshotNames(t)
	rows := coverPlantedManifestRows(base, realFolder)
	for i := 0; i < len(rows); i++ {
		r := rows[i]
		t.Run(r.name, func(t *testing.T) {
			assertCoverPlant(t, r.name, r.want, r.apply(base), r.folder, r.checkFolder)
		})
	}
}

func coverPlantedManifestRows(base contractManifest, realFolder []string) []struct {
	name        string
	want        string
	apply       func(contractManifest) contractManifest
	folder      []string
	checkFolder bool
} {
	dupFile := copyContractCase(base.Cases[0])
	dupFile.ID = "valid-dup-file"
	extraFolder := append([]string{}, realFolder...)
	extraFolder = append(extraFolder, "extra.snapshot.json")
	sort.Strings(extraFolder)
	return []struct {
		name        string
		want        string
		apply       func(contractManifest) contractManifest
		folder      []string
		checkFolder bool
	}{
		{"control", "", func(m contractManifest) contractManifest { return m }, realFolder, true},
		{"schema_version 2", "schema_version", plantManifestSchemaVersion(base, 2), nil, false},
		{"max_records 20000", "max_records", plantManifestMaxRecords(base, 20000), nil, false},
		{"duplicate case id", "duplicate case id", plantManifestDuplicateID(base), nil, false},
		{"file named by two cases", "named by cases", plantManifestDuplicateFile(base, dupFile), nil, false},
		{"both file and generate", "exactly one of file or generate", plantManifestBothFileGenerate(base), nil, false},
		{"neither file nor generate", "exactly one of file or generate", plantManifestNeitherFileGenerate(base), nil, false},
		{"missing case file", "missing.snapshot.json", plantManifestMissingFile(base), nil, false},
		{"missing generate.from", "generate.from", plantManifestMissingGenerateFrom(base), nil, false},
		{"result maybe", "unknown result", plantManifestResultMaybe(base), nil, false},
		{"compliant refused", "direction", plantManifestCompliantRefused(base), nil, false},
		{"only compliant", "violation", plantManifestOnlyCompliant(base), nil, false},
		{"refused empty says", "empty says", plantManifestRefusedEmptySays(base), nil, false},
		{"accepted names says", "names says", plantManifestAcceptedSays(base), nil, false},
		{"refused names source_runs", "refused names source_runs", plantManifestRefusedSourceRuns(base), nil, false},
		{"refused names headroom", "refused names headroom", plantManifestRefusedHeadroom(base), nil, false},
		{"accepted no source_runs", "no source_runs", plantManifestAcceptedNoSourceRuns(base), nil, false},
		{"known without used_percent", "without used_percent", plantManifestKnownNoUsedPercent(base), nil, false},
		{"known with reason", "known with reason", plantManifestKnownWithReason(base), nil, false},
		{"unknown without reason", "without reason", plantManifestUnknownNoReason(base), nil, false},
		{"unknown with used_percent", "with used_percent", plantManifestUnknownWithUsedPercent(base), nil, false},
		{"reading state stale", "bad state", plantManifestReadingStateStale(base), nil, false},
		{"extra snapshot file", "named by cases", func(m contractManifest) contractManifest { return m }, extraFolder, true},
	}
}

func plantManifestSchemaVersion(base contractManifest, v int) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		out.SchemaVersion = v
		return out
	}
}

func plantManifestMaxRecords(base contractManifest, n int) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		out.MaxRecords = n
		return out
	}
}

func plantManifestDuplicateID(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		dup := copyContractCase(out.Cases[0])
		dup.ID = out.Cases[0].ID
		out.Cases = append(out.Cases, dup)
		return out
	}
}

func plantManifestDuplicateFile(base contractManifest, dup contractCase) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		out.Cases = append(out.Cases, dup)
		return out
	}
}

func plantManifestBothFileGenerate(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		out.Cases[0].Generate = &generateSpec{From: "valid.snapshot.json", Records: 1}
		return out
	}
}

func plantManifestNeitherFileGenerate(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		out.Cases[0].File = ""
		out.Cases[0].Generate = nil
		return out
	}
}

func plantManifestMissingFile(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		out.Cases[0].File = "missing.snapshot.json"
		return out
	}
}

func plantManifestMissingGenerateFrom(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		for i := 0; i < len(out.Cases); i++ {
			if out.Cases[i].Generate != nil {
				out.Cases[i].Generate.From = "missing.snapshot.json"
				break
			}
		}
		return out
	}
}

func plantManifestResultMaybe(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		out.Cases[0].Expect.Result = "maybe"
		return out
	}
}

func plantManifestCompliantRefused(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		out.Cases[0].Expect.Result = "refused"
		out.Cases[0].Expect.Says = "planted"
		return out
	}
}

func plantManifestOnlyCompliant(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		kept := make([]contractCase, 0, len(out.Cases))
		for i := 0; i < len(out.Cases); i++ {
			if out.Cases[i].Direction == "compliant" {
				kept = append(kept, out.Cases[i])
			}
		}
		out.Cases = kept
		return out
	}
}

func plantManifestRefusedEmptySays(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		for i := 0; i < len(out.Cases); i++ {
			if out.Cases[i].Expect.Result == "refused" {
				out.Cases[i].Expect.Says = ""
				break
			}
		}
		return out
	}
}

func plantManifestAcceptedSays(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		out.Cases[0].Expect.Says = "should not be here"
		return out
	}
}

func plantManifestRefusedSourceRuns(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		for i := 0; i < len(out.Cases); i++ {
			if out.Cases[i].Expect.Result == "refused" {
				out.Cases[i].Expect.SourceRuns = map[string]string{"x": "ok"}
				break
			}
		}
		return out
	}
}

func plantManifestRefusedHeadroom(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		for i := 0; i < len(out.Cases); i++ {
			if out.Cases[i].Expect.Result == "refused" {
				out.Cases[i].Expect.Headroom = []headroomCheck{{Now: "2026-01-01T00:00:00Z", MaxAge: "1m", Readings: nil}}
				break
			}
		}
		return out
	}
}

func plantManifestAcceptedNoSourceRuns(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		out.Cases[0].Expect.SourceRuns = nil
		return out
	}
}

func plantManifestKnownNoUsedPercent(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		out.Cases[0].Expect.Headroom[0].Readings[0].UsedPercent = nil
		return out
	}
}

func plantManifestKnownWithReason(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		out.Cases[0].Expect.Headroom[0].Readings[0].Reason = "stale:"
		return out
	}
}

func plantManifestUnknownNoReason(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		for i := 0; i < len(out.Cases); i++ {
			if out.Cases[i].ID == "unknown-headroom" {
				out.Cases[i].Expect.Headroom[0].Readings[0].Reason = ""
				break
			}
		}
		return out
	}
}

func plantManifestUnknownWithUsedPercent(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		for i := 0; i < len(out.Cases); i++ {
			if out.Cases[i].ID == "unknown-headroom" {
				out.Cases[i].Expect.Headroom[0].Readings[0].UsedPercent = float64Ptr(1)
				break
			}
		}
		return out
	}
}

func plantManifestReadingStateStale(base contractManifest) func(contractManifest) contractManifest {
	return func(m contractManifest) contractManifest {
		out := copyContractManifest(base)
		out.Cases[0].Expect.Headroom[0].Readings[0].State = "stale"
		return out
	}
}
