package catalog

import (
	"sort"
	"strings"
	"testing"
)

func assertCoverPlant(t *testing.T, want string, m contractManifest, folder []string, checkFolder bool) {
	t.Helper()
	p := firstCoverProblem(m, folder, checkFolder)
	if want == "" {
		if p != "" {
			t.Fatalf("%s problem %q, want clean", t.Name(), p)
		}
		return
	}
	if p == "" {
		t.Fatalf("%s passed cover checks, want problem containing %q", t.Name(), want)
	}
	if !strings.Contains(p, want) {
		t.Fatalf("%s problem %q, want substring %q", t.Name(), p, want)
	}
}

func TestSnapshotContractFixturesCoverRefusesAPlantedManifest(t *testing.T) {
	base := copyContractManifest(loadContractManifest(t))
	realFolder := sortedContractSnapshotNames(t)
	rows := coverPlantedManifestRows(realFolder)
	for i := 0; i < len(rows); i++ {
		r := rows[i]
		t.Run(r.name, func(t *testing.T) {
			assertCoverPlant(t, r.want, r.apply(base), r.folder, r.checkFolder)
		})
	}
}

func coverPlantedManifestRows(realFolder []string) []struct {
	name        string
	want        string
	apply       func(contractManifest) contractManifest
	folder      []string
	checkFolder bool
} {
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
		{"schema_version 2", "schema_version", plantManifestSchemaVersion2, nil, false},
		{"max_records 20000", "max_records", plantManifestMaxRecords20000, nil, false},
		{"duplicate case id", "duplicate case id", plantManifestDuplicateID, nil, false},
		{"file named by two cases", "named by cases", plantManifestDuplicateFile, nil, false},
		{"both file and generate", "exactly one of file or generate", plantManifestBothFileGenerate, nil, false},
		{"neither file nor generate", "exactly one of file or generate", plantManifestNeitherFileGenerate, nil, false},
		{"missing case file", "missing.snapshot.json", plantManifestMissingFile, nil, false},
		{"missing generate.from", "generate.from", plantManifestMissingGenerateFrom, nil, false},
		{"result maybe", "unknown result", plantManifestResultMaybe, nil, false},
		{"compliant refused", "direction", plantManifestCompliantRefused, nil, false},
		{"only compliant", "violation", plantManifestOnlyCompliant, nil, false},
		{"refused empty says", "empty says", plantManifestRefusedEmptySays, nil, false},
		{"accepted names says", "names says", plantManifestAcceptedSays, nil, false},
		{"refused names source_runs", "refused names source_runs", plantManifestRefusedSourceRuns, nil, false},
		{"refused names headroom", "refused names headroom", plantManifestRefusedHeadroom, nil, false},
		{"accepted no source_runs", "no source_runs", plantManifestAcceptedNoSourceRuns, nil, false},
		{"known without used_percent", "without used_percent", plantManifestKnownNoUsedPercent, nil, false},
		{"known with reason", "known with reason", plantManifestKnownWithReason, nil, false},
		{"unknown without reason", "without reason", plantManifestUnknownNoReason, nil, false},
		{"unknown with used_percent", "with used_percent", plantManifestUnknownWithUsedPercent, nil, false},
		{"reading state stale", "bad state", plantManifestReadingStateStale, nil, false},
		{"extra snapshot file", "named by cases", func(m contractManifest) contractManifest { return m }, extraFolder, true},
	}
}

func plantManifestSchemaVersion2(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	out.SchemaVersion = 2
	return out
}

func plantManifestMaxRecords20000(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	out.MaxRecords = 20000
	return out
}

func plantManifestDuplicateID(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	dup := copyContractCase(out.Cases[0])
	dup.ID = out.Cases[0].ID
	out.Cases = append(out.Cases, dup)
	return out
}

func plantManifestDuplicateFile(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	dup := copyContractCase(out.Cases[0])
	dup.ID = "valid-dup-file"
	out.Cases = append(out.Cases, dup)
	return out
}

func plantManifestBothFileGenerate(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	out.Cases[0].Generate = &generateSpec{From: "valid.snapshot.json", Records: 1}
	return out
}

func plantManifestNeitherFileGenerate(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	out.Cases[0].File = ""
	out.Cases[0].Generate = nil
	return out
}

func plantManifestMissingFile(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	out.Cases[0].File = "missing.snapshot.json"
	return out
}

func plantManifestMissingGenerateFrom(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	for i := 0; i < len(out.Cases); i++ {
		if out.Cases[i].Generate != nil {
			out.Cases[i].Generate.From = "missing.snapshot.json"
			break
		}
	}
	return out
}

func plantManifestResultMaybe(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	out.Cases[0].Expect.Result = "maybe"
	return out
}

func plantManifestCompliantRefused(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	out.Cases[0].Expect.Result = "refused"
	out.Cases[0].Expect.Says = "planted"
	return out
}

func plantManifestOnlyCompliant(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	kept := make([]contractCase, 0, len(out.Cases))
	for i := 0; i < len(out.Cases); i++ {
		if out.Cases[i].Direction == "compliant" {
			kept = append(kept, out.Cases[i])
		}
	}
	out.Cases = kept
	return out
}

func plantManifestRefusedEmptySays(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	for i := 0; i < len(out.Cases); i++ {
		if out.Cases[i].Expect.Result == "refused" {
			out.Cases[i].Expect.Says = ""
			break
		}
	}
	return out
}

func plantManifestAcceptedSays(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	out.Cases[0].Expect.Says = "should not be here"
	return out
}

func plantManifestRefusedSourceRuns(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	for i := 0; i < len(out.Cases); i++ {
		if out.Cases[i].Expect.Result == "refused" {
			out.Cases[i].Expect.SourceRuns = map[string]string{"x": "ok"}
			break
		}
	}
	return out
}

func plantManifestRefusedHeadroom(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	for i := 0; i < len(out.Cases); i++ {
		if out.Cases[i].Expect.Result == "refused" {
			out.Cases[i].Expect.Headroom = []headroomCheck{{Now: "2026-01-01T00:00:00Z", MaxAge: "1m", Readings: nil}}
			break
		}
	}
	return out
}

func plantManifestAcceptedNoSourceRuns(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	out.Cases[0].Expect.SourceRuns = nil
	return out
}

func plantManifestKnownNoUsedPercent(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	out.Cases[0].Expect.Headroom[0].Readings[0].UsedPercent = nil
	return out
}

func plantManifestKnownWithReason(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	out.Cases[0].Expect.Headroom[0].Readings[0].Reason = "stale:"
	return out
}

func plantManifestUnknownNoReason(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	for i := 0; i < len(out.Cases); i++ {
		if out.Cases[i].ID == "unknown-headroom" {
			out.Cases[i].Expect.Headroom[0].Readings[0].Reason = ""
			break
		}
	}
	return out
}

func plantManifestUnknownWithUsedPercent(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	for i := 0; i < len(out.Cases); i++ {
		if out.Cases[i].ID == "unknown-headroom" {
			out.Cases[i].Expect.Headroom[0].Readings[0].UsedPercent = float64Ptr(1)
			break
		}
	}
	return out
}

func plantManifestReadingStateStale(m contractManifest) contractManifest {
	out := copyContractManifest(m)
	out.Cases[0].Expect.Headroom[0].Readings[0].State = "stale"
	return out
}
