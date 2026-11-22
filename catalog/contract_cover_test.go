package catalog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func statContractFile(name string) error {
	root, err := os.OpenRoot(contractDir)
	if err != nil {
		return err
	}
	_, statErr := root.Stat(name)
	closeErr := root.Close()
	return errors.Join(statErr, closeErr)
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
	if err := statContractFile(c.File); err != nil {
		return fmt.Sprintf("case %q file %q: %v", c.ID, c.File, err)
	}
	return ""
}

func problemCaseGenerateFromExists(c contractCase) string {
	if c.Generate == nil {
		return ""
	}
	if err := statContractFile(c.Generate.From); err != nil {
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
