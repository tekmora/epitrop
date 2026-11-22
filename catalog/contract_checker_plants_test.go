package catalog

import (
	"maps"
	"testing"
)

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
