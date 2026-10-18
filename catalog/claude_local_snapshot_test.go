package catalog

import (
	"bytes"
	"os"
	"strings"
	"testing"

	golusorisschema "github.com/golusoris/golusoris/jsonschema"
)

func readPlantedClaudeLocalSnapshot(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/claude-local.snapshot.json")
	if err != nil {
		t.Fatalf("read planted claude-local snapshot: %v", err)
	}
	return data
}

func TestClaudeLocalSnapshot_Positive(t *testing.T) {
	snap, err := ParseSnapshot(readPlantedClaudeLocalSnapshot(t))
	if err != nil {
		t.Fatalf("ParseSnapshot(planted) = %v, want nil", err)
	}
	if len(snap.Records) != 2 {
		t.Fatalf("len(Records) = %d, want 2", len(snap.Records))
	}

	assertMeasuredClaudeLocalRecord(t, snap.Records[0])
	assertAbsentClaudeLocalRecord(t, snap.Records[1])
	assertWrittenClaudeLocalFields(t, snap)
}

func assertMeasuredClaudeLocalRecord(t *testing.T, rec Record) {
	t.Helper()
	measured := rec.UsageWindow
	if measured == nil {
		t.Fatal("record 0 UsageWindow is nil")
	}
	if measured.UsedPercent == nil || *measured.UsedPercent != 35 {
		t.Fatalf("used_percent = %v, want 35", measured.UsedPercent)
	}
	if measured.BurnPercentPerHour == nil || *measured.BurnPercentPerHour != 40 {
		t.Fatalf("burn_percent_per_hour = %v, want 40", measured.BurnPercentPerHour)
	}
	if measured.ActiveAgents == nil || *measured.ActiveAgents != 2 {
		t.Fatalf("active_agents = %v, want 2", measured.ActiveAgents)
	}
}

func assertAbsentClaudeLocalRecord(t *testing.T, rec Record) {
	t.Helper()
	if rec.UsageWindow == nil {
		t.Fatal("record 1 UsageWindow is nil")
	}
	if rec.UsageWindow.UsedPercent != nil {
		t.Fatalf("record 1 used_percent = %v, want nil", rec.UsageWindow.UsedPercent)
	}
	if !strings.Contains(rec.Absent["usage_window.used_percent"], "ceiling is unknown") {
		t.Fatalf("record 1 absent used_percent = %q, want reason containing ceiling is unknown", rec.Absent["usage_window.used_percent"])
	}
}

func assertWrittenClaudeLocalFields(t *testing.T, snap Snapshot) {
	t.Helper()
	out, err := snap.Marshal()
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}
	if n := bytes.Count(out, []byte(`"used_percent": 35`)); n != 1 {
		t.Fatalf("written snapshot holds used_percent 35 %d times, want 1", n)
	}
	if n := bytes.Count(out, []byte(`"burn_percent_per_hour": 40`)); n != 1 {
		t.Fatalf("written snapshot holds burn_percent_per_hour 40 %d times, want 1", n)
	}
	if n := bytes.Count(out, []byte(`"active_agents": 2`)); n != 2 {
		t.Fatalf("written snapshot holds active_agents 2 %d times, want 2", n)
	}
	if !bytes.Contains(out, []byte(`"usage_window.used_percent": "no rejected seven_day request`)) {
		t.Fatal("written snapshot lost absent reason for seven_day used_percent")
	}
	if bytes.Contains(out, []byte(`"active_agents": 0`)) || bytes.Contains(out, []byte(`"burn_percent_per_hour": 0`)) {
		t.Fatal("written snapshot zero-filled a field the source did not report")
	}
}

func TestClaudeLocalSnapshot_Schema(t *testing.T) {
	schema, err := golusorisschema.Compile("snapshot.schema.json", SnapshotSchema())
	if err != nil {
		t.Fatalf("compile snapshot schema: %v", err)
	}
	planted := string(readPlantedClaudeLocalSnapshot(t))
	cases := map[string]struct {
		from, to string
		valid    bool
	}{
		"planted":           {"", "", true},
		"zero burn":         {`"burn_percent_per_hour": 40`, `"burn_percent_per_hour": 0`, true},
		"zero agents":       {`"active_agents": 2`, `"active_agents": 0`, true},
		"negative burn":     {`"burn_percent_per_hour": 40`, `"burn_percent_per_hour": -0.1`, false},
		"negative agents":   {`"active_agents": 2`, `"active_agents": -1`, false},
		"fractional agents": {`"active_agents": 2`, `"active_agents": 1.5`, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(planted, tc.from, tc.to, 1)
			if tc.from != "" && doc == planted {
				t.Fatalf("planted change %q not applied", tc.to)
			}
			err := schema.Validate([]byte(doc))
			if (err == nil) != tc.valid {
				t.Fatalf("schema.Validate() = %v, want valid=%v", err, tc.valid)
			}
		})
	}
}
