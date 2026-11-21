package catalog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSnapshotMarshalParse_Positive(t *testing.T) {
	snap := Snapshot{
		SchemaVersion: SnapshotSchemaVersion,
		GeneratedAt:   time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
		Records:       []Record{validRecord()},
		SourceRuns: []SourceRun{
			{Source: "litellm-gateway", Status: StatusOK, Count: 1},
		},
	}
	data, err := snap.Marshal()
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}
	got, err := ParseSnapshot(data)
	if err != nil {
		t.Fatalf("ParseSnapshot() = %v, want nil", err)
	}
	if len(got.Records) != 1 || got.Records[0].ModelID != "example/auto" {
		t.Fatalf("ParseSnapshot() round-trip = %+v", got)
	}
}

func TestSnapshotValidateRequiresSupportedSchemaVersion(t *testing.T) {
	cases := map[string]int{
		"missing":     0,
		"unsupported": 99,
	}
	for name, version := range cases {
		t.Run(name, func(t *testing.T) {
			snap := Snapshot{
				SchemaVersion: version,
				GeneratedAt:   time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
				Records:       []Record{validRecord()},
			}
			err := snap.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want an error for bad schema_version")
			}
			wantFound := "found " + strconv.Itoa(version)
			if msg := err.Error(); !strings.Contains(msg, "schema_version") ||
				!strings.Contains(msg, wantFound) || !strings.Contains(msg, "supported 1") {
				t.Fatalf("Validate() error = %q, want found and supported schema versions", msg)
			}
		})
	}
}

func TestSnapshotSchemaFileAgreesWithGoTypes(t *testing.T) {
	schema := loadSnapshotSchemaForTest(t)
	assertObjectMatchesType(t, schema, "Snapshot", reflect.TypeOf(Snapshot{}))
	assertObjectMatchesType(t, schema, "Record", reflect.TypeOf(Record{}))
	assertObjectMatchesType(t, schema, "Provenance", reflect.TypeOf(Provenance{}))
	assertObjectMatchesType(t, schema, "Limits", reflect.TypeOf(Limits{}))
	assertObjectMatchesType(t, schema, "UsageWindow", reflect.TypeOf(UsageWindow{}))
	assertObjectMatchesType(t, schema, "SourceRun", reflect.TypeOf(SourceRun{}))
	assertEnumMatches(t, schema, "Status", []string{string(StatusOK), string(StatusSkip), string(StatusFail), string(StatusDegraded)})
	assertEnumMatches(t, schema, "Kind", []string{string(KindMeasured), string(KindDeclared)})
	assertEnumMatches(t, schema, "AccessPath", []string{string(AccessAPI), string(AccessSubscriptionCLI), string(AccessGateway), string(AccessLocal)})
	if got := intFromSchema(t, schema, "version"); got != 1 {
		t.Fatalf("schema version = %d, want 1", got)
	}
}

func TestSnapshotMarshal_Negative(t *testing.T) {
	snap := Snapshot{
		SchemaVersion: SnapshotSchemaVersion,
		GeneratedAt:   time.Now(),
		Records:       []Record{{}}, // empty record fails Validate
	}
	if _, err := snap.Marshal(); err == nil {
		t.Fatal("Marshal() = nil error, want validation failure to propagate")
	}
}

func TestParseSnapshot_Negative(t *testing.T) {
	if _, err := ParseSnapshot([]byte("not json")); err == nil {
		t.Fatal("ParseSnapshot() = nil error, want a decode failure")
	}
}

// TestSnapshotValidate_Boundary confirms MaxSnapshotRecords is enforced at
// exactly one past the bound, not off by one in either direction.
func TestSnapshotValidate_Boundary(t *testing.T) {
	records := make([]Record, MaxSnapshotRecords+1)
	for i := range records {
		r := validRecord()
		r.ModelID = "m" + strconv.Itoa(i)
		records[i] = r
	}
	snap := Snapshot{SchemaVersion: SnapshotSchemaVersion, GeneratedAt: time.Now(), Records: records}
	if err := snap.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error one past MaxSnapshotRecords")
	}
}

func TestSnapshotSchemaEmbeddedMatchesFile(t *testing.T) {
	fileData, err := os.ReadFile(filepath.Join("snapshot.schema.json"))
	if err != nil {
		t.Fatalf("read schema file: %v", err)
	}
	if got := SnapshotSchema(); !bytes.Equal(got, fileData) {
		t.Fatalf("SnapshotSchema() differs from catalog/snapshot.schema.json")
	}
}

func loadSnapshotSchemaForTest(t *testing.T) map[string]any {
	t.Helper()
	path := os.Getenv("EPITROP_SNAPSHOT_SCHEMA")
	if path == "" {
		path = filepath.Join("snapshot.schema.json")
	}
	data, err := os.ReadFile(path) // #nosec G304 G703 -- schema path is an explicit test override or the local snapshot schema
	if err != nil {
		t.Fatalf("read snapshot schema %s: %v", path, err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse snapshot schema %s: %v", path, err)
	}
	return schema
}

func assertObjectMatchesType(t *testing.T, schema map[string]any, name string, typ reflect.Type) {
	t.Helper()
	object := schemaObject(t, schema, name)
	properties := stringMapKeys(t, objectMap(t, object, "properties"))
	required := stringValues(t, objectArray(t, object, "required"))
	wantProperties, wantRequired := jsonShape(t, typ)
	assertStringSet(t, name+" properties", properties, wantProperties)
	assertStringSet(t, name+" required", required, wantRequired)
}

func schemaObject(t *testing.T, schema map[string]any, name string) map[string]any {
	t.Helper()
	if name == "Snapshot" {
		return schema
	}
	defs := objectMap(t, schema, "$defs")
	value, ok := defs[name]
	if !ok {
		t.Fatalf("$defs missing %s", name)
	}
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("$defs.%s = %T, want object", name, value)
	}
	return object
}

func jsonShape(t *testing.T, typ reflect.Type) ([]string, []string) {
	t.Helper()
	properties := make([]string, 0, typ.NumField())
	required := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name, optional := jsonFieldName(field)
		if name == "" {
			continue
		}
		properties = append(properties, name)
		if !optional {
			required = append(required, name)
		}
	}
	return sortedStrings(properties), sortedStrings(required)
}

func jsonFieldName(field reflect.StructField) (string, bool) {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	parts := strings.Split(tag, ",")
	name := parts[0]
	if name == "" {
		name = field.Name
	}
	return name, slices.Contains(parts[1:], "omitempty")
}

func assertEnumMatches(t *testing.T, schema map[string]any, name string, want []string) {
	t.Helper()
	object := schemaObject(t, schema, name)
	got := stringValues(t, objectArray(t, object, "enum"))
	assertStringSet(t, name+" enum", got, sortedStrings(want))
}

func objectMap(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key]
	if !ok {
		t.Fatalf("schema object missing %q", key)
	}
	typed, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("schema %q = %T, want object", key, value)
	}
	return typed
}

func objectArray(t *testing.T, object map[string]any, key string) []any {
	t.Helper()
	value, ok := object[key]
	if !ok {
		t.Fatalf("schema object missing %q", key)
	}
	typed, ok := value.([]any)
	if !ok {
		t.Fatalf("schema %q = %T, want array", key, value)
	}
	return typed
}

func stringMapKeys(t *testing.T, values map[string]any) []string {
	t.Helper()
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return sortedStrings(keys)
}

func stringValues(t *testing.T, values []any) []string {
	t.Helper()
	out := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("schema array contains %T, want string", value)
		}
		out = append(out, text)
	}
	return sortedStrings(out)
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	slices.Sort(out)
	return out
}

func assertStringSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	if !slices.Equal(sortedStrings(got), sortedStrings(want)) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

func intFromSchema(t *testing.T, schema map[string]any, key string) int {
	t.Helper()
	value, ok := schema[key]
	if !ok {
		t.Fatalf("schema missing %q", key)
	}
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("schema %q = %T, want number", key, value)
	}
	return int(number)
}
