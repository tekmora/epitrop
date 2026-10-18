package config

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/broker"
)

const fleetDeadLetter = `{"broker": {"queue_capacity": 128, "dead_letter": {"capacity": 16, "fallback": "real-failure", "classes": [
  {"name": "known-flake", "signatures": ["TestSelfTest.*SIGSEGV"], "max_retries": 1},
  {"name": "time-limit", "signatures": ["context deadline exceeded"], "max_retries": 1},
  {"name": "rebase-conflict", "signatures": ["^CONFLICT \\("]},
  {"name": "generated-drift", "signatures": ["go mod tidy -diff"]}
]}}}`

func TestBrokerDefaults(t *testing.T) {
	cfg, err := Load(testContext(t), writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("Load({}) = %v", err)
	}
	b := cfg.Broker
	if b.QueueCapacity != 64 || b.DeadLetter.Capacity != 64 || b.DeadLetter.Fallback != "real-failure" || len(b.DeadLetter.Classes) != 0 {
		t.Fatalf("broker defaults = %+v", b)
	}
	specs := b.DeadLetter.Specs()
	if len(specs) != 1 || specs[0].Name != "real-failure" || len(specs[0].Signatures) != 0 {
		t.Fatalf("Specs() of the defaults = %+v, want the fallback class alone", specs)
	}
}

func TestBrokerClassSetLoadsAndRoutes(t *testing.T) {
	cfg, err := Load(testContext(t), writeConfig(t, fleetDeadLetter))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	assertFleetBroker(t, cfg.Broker)
	dl := cfg.Broker.DeadLetter
	set, err := broker.NewDeadLetters(dl.Specs(), dl.Fallback, dl.Capacity)
	if err != nil {
		t.Fatalf("NewDeadLetters(Specs()) = %v", err)
	}
	cases := []struct {
		text string
		want broker.Verdict
	}{
		{"Auto-merging x\nCONFLICT (content): y", broker.Verdict{Class: "rebase-conflict"}},
		{"CONFLICT (content): y\ncontext deadline exceeded", broker.Verdict{Class: "time-limit", Retry: true}},
		{"want 2, got 3", broker.Verdict{Class: "real-failure"}},
	}
	for _, c := range cases {
		v, err := set.Route(testContext(t), broker.Failure{Unit: "u", Text: c.text, Attempt: 1})
		if err != nil || v != c.want {
			t.Fatalf("Route(%q) = %+v, %v; want %+v", c.text, v, err, c.want)
		}
	}
}

func assertFleetBroker(t *testing.T, b BrokerConfig) {
	t.Helper()
	dl := b.DeadLetter
	if b.QueueCapacity != 128 || dl.Capacity != 16 || len(dl.Classes) != 4 {
		t.Fatalf("broker = %+v", b)
	}
	if dl.Classes[0].MaxRetries != 1 || dl.Classes[2].MaxRetries != 0 {
		t.Fatalf("broker classes = %+v", dl.Classes)
	}
}

// TestBrokerRefusals: each document breaks one rule of the broker block.
func TestBrokerRefusals(t *testing.T) {
	class := func(name, sig string) string {
		return fmt.Sprintf(`{"name": %q, "signatures": [%q]}`, name, sig)
	}
	dl := func(body string) string { return `{"broker": {"dead_letter": ` + body + `}}` }
	// want names the barrier that refuses: the schema, or the broker's own class set check.
	const schema, classSet = "jsonschema", "config: broker/dead_letter"
	cases := map[string]struct{ body, want string }{
		"queue capacity 0":        {`{"broker": {"queue_capacity": 0}}`, schema},
		"class capacity 0":        {dl(`{"capacity": 0}`), schema},
		"unknown key":             {`{"broker": {"queue": 1}}`, schema},
		"unknown class key":       {dl(`{"classes": [{"name": "a", "signatures": ["x"], "retries": 1}]}`), schema},
		"fallback name":           {dl(`{"fallback": "Real Failure"}`), schema},
		"class name":              {dl(`{"classes": [` + class("Known Flake", "x") + `]}`), schema},
		"class without signature": {dl(`{"classes": [{"name": "a", "signatures": []}]}`), schema},
		"signatures missing":      {dl(`{"classes": [{"name": "a"}]}`), schema},
		"empty signature":         {dl(`{"classes": [` + class("a", "") + `]}`), schema},
		"two retries":             {dl(`{"classes": [{"name": "a", "signatures": ["x"], "max_retries": 2}]}`), schema},
		"long signature":          {dl(`{"classes": [` + class("a", strings.Repeat("a", broker.MaxSignatureLen+1)) + `]}`), schema},
		"bad expression":          {dl(`{"classes": [` + class("a", "(") + `]}`), classSet},
		"duplicate class":         {dl(`{"classes": [` + class("a", "x") + `, ` + class("a", "y") + `]}`), classSet},
		"class named as fallback": {dl(`{"classes": [` + class("real-failure", "x") + `]}`), classSet},
	}
	for name, c := range cases {
		_, err := Load(testContext(t), writeConfig(t, c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: Load = %v, want a refusal by %s", name, err, c.want)
		}
	}
}

func classList(n, signatures int) string {
	sigs := make([]string, signatures)
	for i := range sigs {
		sigs[i] = fmt.Sprintf("%q", fmt.Sprintf("s%d", i))
	}
	classes := make([]string, n)
	for i := range classes {
		classes[i] = fmt.Sprintf(`{"name": "c%d", "signatures": [%s]}`, i, strings.Join(sigs, ", "))
	}
	return `{"broker": {"dead_letter": {"classes": [` + strings.Join(classes, ", ") + `]}}}`
}

// TestBrokerBoundsAreTheBrokerBounds: the schema's bounds are the broker package's, and a
// document at each bound loads.
func TestBrokerBoundsAreTheBrokerBounds(t *testing.T) {
	accepted := map[string]string{
		"queue capacity at the bound": fmt.Sprintf(`{"broker": {"queue_capacity": %d}}`, broker.MaxCapacity),
		"class capacity at the bound": fmt.Sprintf(`{"broker": {"dead_letter": {"capacity": %d}}}`, broker.MaxCapacity),
		"classes at the bound":        classList(broker.MaxClasses-1, 1),
		"signatures at the bound":     classList(1, broker.MaxSignatures),
		"signature at the bound":      fmt.Sprintf(`{"broker": {"dead_letter": {"classes": [{"name": "a", "signatures": [%q]}]}}}`, strings.Repeat("a", broker.MaxSignatureLen)),
	}
	for name, body := range accepted {
		if _, err := Load(testContext(t), writeConfig(t, body)); err != nil {
			t.Fatalf("%s: Load = %v, want nil", name, err)
		}
	}
	refused := map[string]string{
		"queue capacity above the bound": fmt.Sprintf(`{"broker": {"queue_capacity": %d}}`, broker.MaxCapacity+1),
		"class capacity above the bound": fmt.Sprintf(`{"broker": {"dead_letter": {"capacity": %d}}}`, broker.MaxCapacity+1),
		"classes above the bound":        classList(broker.MaxClasses, 1),
		"signatures above the bound":     classList(1, broker.MaxSignatures+1),
	}
	for name, body := range refused {
		if _, err := Load(testContext(t), writeConfig(t, body)); err == nil {
			t.Fatalf("%s: Load = nil, want a refusal", name)
		}
	}
}

// schemaNumber reads the number at path in the embedded schema; each path element is an
// object key.
func schemaNumber(t *testing.T, path ...string) int {
	t.Helper()
	var node any
	if err := json.Unmarshal(SchemaJSON(), &node); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	for _, key := range path {
		object, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("schema path %s: %s is not in an object", strings.Join(path, "."), key)
		}
		node = object[key]
	}
	number, ok := node.(float64)
	if !ok {
		t.Fatalf("schema path %s = %#v, want a number", strings.Join(path, "."), node)
	}
	return int(number)
}

// TestBrokerSchemaBoundsAreTheBrokerConstants: each bound the schema states for the broker
// block is the broker package's constant, so neither can move alone.
func TestBrokerSchemaBoundsAreTheBrokerConstants(t *testing.T) {
	at := func(base []string, keys ...string) []string { return append(slices.Clone(base), keys...) }
	block := []string{"properties", "broker", "properties"}
	dl := at(block, "dead_letter", "properties")
	class := at(dl, "classes", "items", "properties")
	cases := []struct {
		path []string
		want int
	}{
		{at(block, "queue_capacity", "maximum"), broker.MaxCapacity},
		{at(dl, "capacity", "maximum"), broker.MaxCapacity},
		{at(dl, "classes", "maxItems"), broker.MaxClasses - 1},
		{at(class, "signatures", "maxItems"), broker.MaxSignatures},
		{at(class, "signatures", "items", "maxLength"), broker.MaxSignatureLen},
		{at(class, "max_retries", "maximum"), broker.MaxRetries},
	}
	for _, c := range cases {
		if got := schemaNumber(t, c.path...); got != c.want {
			t.Fatalf("schema %s = %d, want %d", strings.Join(c.path, "."), got, c.want)
		}
	}
}
