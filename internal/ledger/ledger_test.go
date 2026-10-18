package ledger

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// report builds a ledger report around one unit whose fields are unit and a
// summary whose fields are summary; every other field is the engine's own.
func report(unit, summary string) string {
	return `{"units":[{"pull_request_number":7,"metric_epoch":"2026-10-10",` + unit + `}],` +
		`"milestone_summary":{"metric_epoch":"2026-10-10",` + summary + `},` +
		`"sources_measured":{"forge":true,"transcripts":true,"spend_log":true}}`
}

const (
	measuredUnit = `"frontier_tokens":"120000","frontier_tokens_num":120000,"spend":"$1.50","spend_amount":1.5,` +
		`"wall_seconds":{"value":567,"provenance":"measured"}`
	notMeasuredUnit = `"frontier_tokens":"not measured","spend":"not measured","wall_seconds":null`
	measuredSummary = `"frontier_tokens":"120000","frontier_tokens_num":120000,"total_spend":"$1.50","total_spend_num":1.5`
	unmeasuredSum   = `"frontier_tokens":"not measured","total_spend":"not measured"`
)

func mustRead(t *testing.T, body string) Report {
	t.Helper()
	rep, err := Read(strings.NewReader(body))
	if err != nil {
		t.Fatalf("Read() = %v, want a report", err)
	}
	return rep
}

func wantValue(t *testing.T, name string, f Figure, want float64) {
	t.Helper()
	if !f.Known() || *f.Value != want || f.Reason != "" {
		t.Errorf("%s = %+v, want the value %v", name, f, want)
	}
}

func wantUnknown(t *testing.T, name string, f Figure, reason string) {
	t.Helper()
	if f.Known() || f.Value != nil || !strings.Contains(f.Reason, reason) {
		t.Errorf("%s = %+v, want unknown with %q", name, f, reason)
	}
}

// The fixture is the pinned engine's own output; it measured wall time from
// the forge and nothing else.
func TestReadTheEngineOutput(t *testing.T) {
	f, err := os.Open("testdata/upstream/efficiency-report.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // read-only test file
	rep, err := Read(f)
	if err != nil || len(rep.Units) != 3 || rep.Units[0].PullRequest != 240 {
		t.Fatalf("Read(fixture) = %+v, %v, want units 240, 239, 238", rep, err)
	}
	if rep.Sources != (Sources{Forge: true}) {
		t.Errorf("sources = %+v, want only the forge", rep.Sources)
	}
	wantUnknown(t, "summary frontier_tokens", rep.FrontierTokens, "not measured")
	wantUnknown(t, "summary total_spend", rep.TotalSpend, "not measured")
	wantUnknown(t, "unit 240 spend", rep.Units[0].Spend, "not measured")
	wantValue(t, "unit 240 wall_seconds", rep.Units[0].WallSeconds, 567)
	if rep.Units[0].WallSeconds.Provenance != "measured" {
		t.Errorf("unit 240 wall_seconds provenance = %q, want measured", rep.Units[0].WallSeconds.Provenance)
	}
}

func TestReadMeasuredFigures(t *testing.T) {
	rep := mustRead(t, report(measuredUnit, measuredSummary))
	wantValue(t, "summary frontier_tokens", rep.FrontierTokens, 120000)
	wantValue(t, "summary total_spend", rep.TotalSpend, 1.5)
	u := rep.Units[0]
	wantValue(t, "unit frontier_tokens", u.FrontierTokens, 120000)
	wantValue(t, "unit spend", u.Spend, 1.5)
	wantValue(t, "unit wall_seconds", u.WallSeconds, 567)
}

// A figure the ledger did not measure has no value, and its JSON form has no
// value key, so neither a Go caller nor a JSON reader can take it for zero.
func TestNotMeasuredIsNeverZero(t *testing.T) {
	rep := mustRead(t, report(notMeasuredUnit, unmeasuredSum))
	figures := map[string]Figure{
		"summary frontier_tokens": rep.FrontierTokens, "summary total_spend": rep.TotalSpend,
		"unit frontier_tokens": rep.Units[0].FrontierTokens, "unit spend": rep.Units[0].Spend,
		"unit wall_seconds": rep.Units[0].WallSeconds,
	}
	for name, f := range figures {
		wantUnknown(t, name, f, "not measured")
		body, err := json.Marshal(f)
		if err != nil || strings.Contains(string(body), `"value"`) {
			t.Errorf("%s json = %s, %v, want no value key", name, body, err)
		}
	}
	explained := report(notMeasuredUnit, `"frontier_tokens":"not measured (see #897)","total_spend":"not measured"`)
	wantUnknown(t, "summary frontier_tokens", mustRead(t, explained).FrontierTokens, "not measured (see #897)")
}

// Zero is a measured value; it stays known.
func TestReadBoundaries(t *testing.T) {
	zero := `"frontier_tokens":"0","frontier_tokens_num":0,"spend":"$0.00","spend_amount":0,"wall_seconds":{"value":0,"provenance":"modeled"}`
	rep := mustRead(t, report(zero, `"frontier_tokens":"0","frontier_tokens_num":0,"total_spend":"$0.00","total_spend_num":0`))
	wantValue(t, "summary frontier_tokens", rep.FrontierTokens, 0)
	wantValue(t, "unit spend", rep.Units[0].Spend, 0)
	wantValue(t, "unit wall_seconds", rep.Units[0].WallSeconds, 0)
	if rep.Units[0].WallSeconds.Provenance != "modeled" {
		t.Errorf("wall_seconds provenance = %q, want modeled kept", rep.Units[0].WallSeconds.Provenance)
	}
	empty := `{"units":[],"milestone_summary":{"metric_epoch":"2026-10-10",` + unmeasuredSum + `},"sources_measured":{}}`
	if rep := mustRead(t, empty); rep.Units == nil || len(rep.Units) != 0 {
		t.Errorf("Read(no units) = %#v, want an empty list", rep.Units)
	}
	atCap := strings.Repeat(" ", MaxReportBytes-len(empty)) + empty
	if _, err := Read(strings.NewReader(atCap)); err != nil {
		t.Errorf("Read(at the cap) = %v, want a report", err)
	}
	if _, err := Read(strings.NewReader(" " + atCap)); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("Read(one byte over) = %v, want it refused", err)
	}
}

func TestReadRefusesABrokenReport(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"summary epoch", `{"units":[],"milestone_summary":{"metric_epoch":"2027-01-01",` + measuredSummary + `},"sources_measured":{}}`, `milestone_summary has metric epoch "2027-01-01"`},
		{"unit epoch", strings.Replace(report(measuredUnit, measuredSummary), `"metric_epoch":"2026-10-10",`, `"metric_epoch":"",`, 1), `unit 7 has metric epoch ""`},
		{"no summary", `{"units":[],"sources_measured":{}}`, "no milestone_summary"},
		{"no sources", `{"units":[],"milestone_summary":{"metric_epoch":"2026-10-10"}}`, "no sources_measured"},
		{"figure missing", report(measuredUnit, `"total_spend":"not measured"`), "frontier_tokens is missing"},
		{"string without number", report(`"frontier_tokens":"5","spend":"not measured","wall_seconds":null`, unmeasuredSum), `says "5" but carries no number`},
		{"number without string", report(`"frontier_tokens":"not measured","frontier_tokens_num":5,"spend":"not measured","wall_seconds":null`, unmeasuredSum), "carries the number 5"},
		{"negative", report(`"frontier_tokens":"-1","frontier_tokens_num":-1,"spend":"not measured","wall_seconds":null`, unmeasuredSum), "out of range"},
		{"unknown provenance", report(`"frontier_tokens":"not measured","spend":"not measured","wall_seconds":{"value":5,"provenance":"guessed"}`, unmeasuredSum), `provenance "guessed"`},
		{"vector without value", report(`"frontier_tokens":"not measured","spend":"not measured","wall_seconds":{"provenance":"measured"}`, unmeasuredSum), "no value"},
		{"malformed", `{"units":`, "decode report"},
	}
	for _, tc := range cases {
		if _, err := Read(strings.NewReader(tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Read() = %v, want an error with %q", tc.name, err, tc.want)
		}
	}
}

func TestReadBoundsTheUnits(t *testing.T) {
	unit := `{"pull_request_number":1,"metric_epoch":"2026-10-10",` + notMeasuredUnit + `}`
	build := func(n int) string {
		return `{"units":[` + strings.TrimSuffix(strings.Repeat(unit+",", n), ",") +
			`],"milestone_summary":{"metric_epoch":"2026-10-10",` + unmeasuredSum + `},"sources_measured":{}}`
	}
	if rep, err := Read(strings.NewReader(build(MaxUnits))); err != nil || len(rep.Units) != MaxUnits {
		t.Errorf("Read(%d units) = %d units, %v, want all of them", MaxUnits, len(rep.Units), err)
	}
	if _, err := Read(strings.NewReader(build(MaxUnits + 1))); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("Read(%d units) = %v, want it refused", MaxUnits+1, err)
	}
}
