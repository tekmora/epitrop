package broker

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const realFailure = "real-failure"

// fleetClasses is the first class set #9 names: known flake, time limit, rebase conflict,
// generated-value drift, and real failure as the fallback.
func fleetClasses() []ClassSpec {
	return []ClassSpec{
		{Name: "known-flake", Signatures: []string{`TestSelfTest.*SIGSEGV`, `markdownlint: deadline on a loaded runner`}, MaxRetries: 1},
		{Name: "time-limit", Signatures: []string{`context deadline exceeded`, `exceeded the maximum execution time`}, MaxRetries: 1},
		{Name: "rebase-conflict", Signatures: []string{`^CONFLICT \(`}},
		{Name: "generated-drift", Signatures: []string{`go mod tidy -diff`, `generated file .* differs`}},
		{Name: realFailure},
	}
}

func newDeadLetters(t *testing.T, capacity int) *DeadLetters {
	t.Helper()
	d, err := NewDeadLetters(fleetClasses(), realFailure, capacity)
	if err != nil {
		t.Fatalf("NewDeadLetters: %v", err)
	}
	return d
}

func TestEachClassTakesItsPlantedFailure(t *testing.T) {
	cases := []struct{ text, class string }{
		{"ok  pkg\nTestSelfTest: signal SIGSEGV: segmentation violation", "known-flake"},
		{"run: context deadline exceeded", "time-limit"},
		{"Auto-merging go.sum\nCONFLICT (content): Merge conflict in go.sum", "rebase-conflict"},
		{"make: tidy-check\ngo mod tidy -diff\n+github.com/x/y v1", "generated-drift"},
		{"--- FAIL: TestLedger (0.01s)\nwant 2, got 3", realFailure},
	}
	for _, c := range cases {
		d := newDeadLetters(t, 4)
		v, err := d.Route(testContext(t), Failure{Unit: "u1", Text: c.text, Attempt: 1})
		if err != nil {
			t.Fatalf("Route(%q): %v", c.text, err)
		}
		if v.Class != c.class {
			t.Fatalf("Route(%q) class = %s, want %s", c.text, v.Class, c.class)
		}
	}
}

func TestTheFirstMatchingClassWins(t *testing.T) {
	d := newDeadLetters(t, 4)
	text := "CONFLICT (content): x\ncontext deadline exceeded"
	v, err := d.Route(testContext(t), Failure{Unit: "u1", Text: text, Attempt: 1})
	if err != nil || v.Class != "time-limit" {
		t.Fatalf("Route = %+v, %v; want time-limit, the earlier class", v, err)
	}
}

func TestARetryableClassRetriesOnceThenParks(t *testing.T) {
	d := newDeadLetters(t, 4)
	f := Failure{Unit: "u1", Text: "context deadline exceeded", Attempt: 1}
	v, err := d.Route(testContext(t), f)
	if err != nil || !v.Retry {
		t.Fatalf("first failure = %+v, %v; want a retry", v, err)
	}
	if got := d.Len("time-limit"); got != 0 {
		t.Fatalf("a retried failure was parked: %d", got)
	}
	f.Attempt = 2
	v, err = d.Route(testContext(t), f)
	if err != nil || v.Retry {
		t.Fatalf("second failure = %+v, %v; want parked", v, err)
	}
	parked, err := d.Next(testContext(t), "time-limit")
	if err != nil || parked != f {
		t.Fatalf("parked = %+v, %v; want %+v", parked, err, f)
	}
}

func TestAClassWithoutRetriesParksAtOnce(t *testing.T) {
	d := newDeadLetters(t, 4)
	v, err := d.Route(testContext(t), Failure{Unit: "u1", Text: "want 2, got 3", Attempt: 1})
	if err != nil || v.Retry || v.Class != realFailure {
		t.Fatalf("Route = %+v, %v; want %s parked", v, err, realFailure)
	}
	if got := d.Len(realFailure); got != 1 {
		t.Fatalf("%s holds %d, want 1", realFailure, got)
	}
}

func TestAFullClassQueueRefusesRetryably(t *testing.T) {
	d := newDeadLetters(t, 1)
	f := Failure{Unit: "u1", Text: "want 2, got 3", Attempt: 1}
	if _, err := d.Route(testContext(t), f); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if _, err := d.Route(testContext(t), f); !errors.Is(err, ErrFull) || !Retryable(err) {
		t.Fatalf("Route on a full class = %v, want ErrFull", err)
	}
	if _, err := d.Route(testContext(t), Failure{Unit: "u2", Text: "CONFLICT (add/add): y", Attempt: 1}); err != nil {
		t.Fatalf("another class must still take failures: %v", err)
	}
}

func TestClassSetRefusals(t *testing.T) {
	long := strings.Repeat("a", MaxSignatureLen+1)
	signatures := make([]string, MaxSignatures+1)
	for i := range signatures {
		signatures[i] = "x"
	}
	many := make([]ClassSpec, MaxClasses)
	for i := range many {
		many[i] = ClassSpec{Name: "c" + strings.Repeat("x", i), Signatures: []string{"x"}}
	}
	cases := []struct {
		name     string
		classes  []ClassSpec
		fallback string
	}{
		{"no classes", nil, realFailure},
		{"fallback missing", fleetClasses()[:4], realFailure},
		{"fallback with signatures", []ClassSpec{{Name: realFailure, Signatures: []string{"x"}}}, realFailure},
		{"duplicate name", append(fleetClasses(), ClassSpec{Name: "time-limit", Signatures: []string{"x"}}), realFailure},
		{"bad name", []ClassSpec{{Name: "Time Limit", Signatures: []string{"x"}}, {Name: realFailure}}, realFailure},
		{"class without signatures", []ClassSpec{{Name: "empty"}, {Name: realFailure}}, realFailure},
		{"bad signature", []ClassSpec{{Name: "a", Signatures: []string{"("}}, {Name: realFailure}}, realFailure},
		{"too many signatures", []ClassSpec{{Name: "a", Signatures: signatures}, {Name: realFailure}}, realFailure},
		{"long signature", []ClassSpec{{Name: "a", Signatures: []string{long}}, {Name: realFailure}}, realFailure},
		{"two retries", []ClassSpec{{Name: "a", Signatures: []string{"x"}, MaxRetries: 2}, {Name: realFailure}}, realFailure},
		{"negative retries", []ClassSpec{{Name: "a", Signatures: []string{"x"}, MaxRetries: -1}, {Name: realFailure}}, realFailure},
		{"too many classes", append(many, ClassSpec{Name: realFailure}), realFailure},
	}
	for _, c := range cases {
		if _, err := NewDeadLetters(c.classes, c.fallback, 1); !errors.Is(err, ErrClassSet) {
			t.Fatalf("%s: NewDeadLetters = %v, want ErrClassSet", c.name, err)
		}
	}
	if _, err := NewDeadLetters(append(many[:MaxClasses-1], ClassSpec{Name: realFailure}), realFailure, 1); err != nil {
		t.Fatalf("%d classes: %v", MaxClasses, err)
	}
	if _, err := NewDeadLetters([]ClassSpec{{Name: "a", Signatures: signatures[:MaxSignatures]}, {Name: realFailure}}, realFailure, 1); err != nil {
		t.Fatalf("%d signatures: %v", MaxSignatures, err)
	}
	if _, err := NewDeadLetters(fleetClasses(), realFailure, 0); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity 0: %v, want ErrCapacity", err)
	}
}

func TestTheFallbackMayRetryOnce(t *testing.T) {
	classes := fleetClasses()
	classes[4].MaxRetries = 1
	d, err := NewDeadLetters(classes, realFailure, 1)
	if err != nil {
		t.Fatalf("NewDeadLetters: %v", err)
	}
	v, err := d.Route(testContext(t), Failure{Unit: "u1", Text: "want 2, got 3", Attempt: 1})
	if err != nil || !v.Retry {
		t.Fatalf("Route = %+v, %v; a fallback may set one retry", v, err)
	}
}

func TestRouteWithAnEndedContextParksNothing(t *testing.T) {
	d := newDeadLetters(t, 1)
	ctx, cancel := context.WithCancel(testContext(t))
	cancel()
	_, err := d.Route(ctx, Failure{Unit: "u1", Text: "want 2, got 3", Attempt: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Route with an ended context = %v, want context.Canceled", err)
	}
	if got := d.Len(realFailure); got != 0 {
		t.Fatalf("%s holds %d, want 0", realFailure, got)
	}
}

func TestUnknownClassReadsAreRefused(t *testing.T) {
	d := newDeadLetters(t, 1)
	if got := d.Len("nope"); got != 0 {
		t.Fatalf("Len of an unknown class = %d", got)
	}
	if _, err := d.Next(testContext(t), "nope"); !errors.Is(err, ErrUnknownClass) {
		t.Fatalf("Next of an unknown class = %v, want ErrUnknownClass", err)
	}
}
