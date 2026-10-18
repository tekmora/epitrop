package catalog

import (
	"bytes"
	"math"
	"os"
	"strings"
	"testing"

	golusorisschema "github.com/golusoris/golusoris/jsonschema"
)

func readPlantedBurnSnapshot(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/usage-window-burn.snapshot.json")
	if err != nil {
		t.Fatalf("read planted snapshot: %v", err)
	}
	return data
}

func TestUsageWindowBurnFields_Positive(t *testing.T) {
	snap, err := ParseSnapshot(readPlantedBurnSnapshot(t))
	if err != nil {
		t.Fatalf("ParseSnapshot(planted) = %v, want nil", err)
	}
	measured := snap.Records[0].UsageWindow
	if measured.BurnPercentPerHour == nil || *measured.BurnPercentPerHour != 12.5 {
		t.Fatalf("burn_percent_per_hour = %v, want 12.5", measured.BurnPercentPerHour)
	}
	if measured.ActiveAgents == nil || *measured.ActiveAgents != 3 {
		t.Fatalf("active_agents = %v, want 3", measured.ActiveAgents)
	}
	absent := snap.Records[1].UsageWindow
	if absent.BurnPercentPerHour != nil || absent.ActiveAgents != nil {
		t.Fatalf("record without the fields read back burn=%v agents=%v, want both nil", absent.BurnPercentPerHour, absent.ActiveAgents)
	}
	assertWrittenBurnFields(t, snap)
}

func assertWrittenBurnFields(t *testing.T, snap Snapshot) {
	t.Helper()
	out, err := snap.Marshal()
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}
	if n := bytes.Count(out, []byte(`"burn_percent_per_hour": 12.5`)); n != 1 {
		t.Fatalf("written snapshot holds burn_percent_per_hour 12.5 %d times, want 1", n)
	}
	if !bytes.Contains(out, []byte(`"usage_window.burn_percent_per_hour": "key info reports spend, not its rate"`)) {
		t.Fatal("written snapshot lost the absent reason for burn_percent_per_hour")
	}
	if n := bytes.Count(out, []byte(`"active_agents": 3`)); n != 1 {
		t.Fatalf("written snapshot holds active_agents 3 %d times, want 1", n)
	}
	if bytes.Contains(out, []byte(`"active_agents": 0`)) || bytes.Contains(out, []byte(`"burn_percent_per_hour": 0`)) {
		t.Fatal("written snapshot zero-filled a field the source did not report")
	}
}

func TestUsageWindowBurnFields_Negative(t *testing.T) {
	cases := map[string]struct {
		from, to string
		want     string
	}{
		"negative burn":     {`"burn_percent_per_hour": 12.5`, `"burn_percent_per_hour": -0.1`, "burn_percent_per_hour"},
		"negative agents":   {`"active_agents": 3`, `"active_agents": -1`, "active_agents"},
		"fractional agents": {`"active_agents": 3`, `"active_agents": 1.5`, "active_agents"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			data := strings.Replace(string(readPlantedBurnSnapshot(t)), tc.from, tc.to, 1)
			if data == string(readPlantedBurnSnapshot(t)) {
				t.Fatalf("planted defect %q not applied", tc.to)
			}
			_, err := ParseSnapshot([]byte(data))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseSnapshot() error = %v, want one naming %s", err, tc.want)
			}
		})
	}
	for name, burn := range map[string]float64{"NaN": math.NaN(), "+Inf": math.Inf(1)} {
		r := validRecord()
		r.UsageWindow = &UsageWindow{BurnPercentPerHour: &burn}
		if err := r.Validate(); err == nil {
			t.Fatalf("Validate() with burn %s = nil, want an error", name)
		}
	}
}

func TestUsageWindowBurnFields_Boundary(t *testing.T) {
	zero, none := 0.0, 0
	r := validRecord()
	r.UsageWindow = &UsageWindow{BurnPercentPerHour: &zero, ActiveAgents: &none}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() with burn 0 and agents 0 = %v, want nil: zero is a measured value", err)
	}
	snap := Snapshot{SchemaVersion: SnapshotSchemaVersion, GeneratedAt: r.Provenance.FetchedAt, Records: []Record{r}}
	out, err := snap.Marshal()
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}
	for _, want := range []string{`"burn_percent_per_hour": 0`, `"active_agents": 0`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("written snapshot lacks %s: a measured zero must stay distinguishable from absent", want)
		}
	}
	schema := loadSnapshotSchemaForTest(t)
	props := objectMap(t, schemaObject(t, schema, "UsageWindow"), "properties")
	for _, field := range []string{"burn_percent_per_hour", "active_agents"} {
		prop, ok := props[field].(map[string]any)
		if !ok || prop["minimum"] != float64(0) {
			t.Fatalf("schema UsageWindow.%s = %v, want minimum 0", field, props[field])
		}
	}
}

// TestUsageWindowBurnFieldsSchema checks the planted snapshot against the
// shipped JSON Schema itself, the contract a reader in another language holds.
func TestUsageWindowBurnFieldsSchema(t *testing.T) {
	schema, err := golusorisschema.Compile("snapshot.schema.json", SnapshotSchema())
	if err != nil {
		t.Fatalf("compile snapshot schema: %v", err)
	}
	planted := string(readPlantedBurnSnapshot(t))
	cases := map[string]struct {
		from, to string
		valid    bool
	}{
		"planted":           {"", "", true},
		"zero burn":         {`"burn_percent_per_hour": 12.5`, `"burn_percent_per_hour": 0`, true},
		"zero agents":       {`"active_agents": 3`, `"active_agents": 0`, true},
		"negative burn":     {`"burn_percent_per_hour": 12.5`, `"burn_percent_per_hour": -0.1`, false},
		"negative agents":   {`"active_agents": 3`, `"active_agents": -1`, false},
		"fractional agents": {`"active_agents": 3`, `"active_agents": 1.5`, false},
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
