package budget

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/ledger"
	"github.com/tekmora/epitrop/internal/telemetry"
	"github.com/tekmora/epitrop/internal/telemetry/telemetrytest"
)

// readReport reads a ledger report whose summary says frontier tokens are
// summary, in the engine's own form.
func readReport(t *testing.T, summary string) ledger.Report {
	t.Helper()
	body := `{"units":[],"milestone_summary":{"metric_epoch":"2026-10-10",` + summary +
		`,"total_spend":"not measured"},"sources_measured":{"forge":true}}`
	rep, err := ledger.Read(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ledger.Read() = %v", err)
	}
	return rep
}

func measured(t *testing.T, used string) ledger.Report {
	t.Helper()
	return readReport(t, `"frontier_tokens":"`+used+`","frontier_tokens_num":`+used)
}

func newTelemetry(t *testing.T) (*telemetry.Telemetry, *telemetrytest.Recorder) {
	t.Helper()
	rec := telemetrytest.Install(t)
	tel, err := telemetry.New(nil, rec.MeterProvider())
	if err != nil {
		t.Fatalf("telemetry.New() = %v", err)
	}
	return tel, rec
}

func task(tokens int) config.TaskBudget { return config.TaskBudget{Tokens: tokens} }

func platform(tokens int) config.PlatformBudget { return config.PlatformBudget{FrontierTokens: tokens} }

func wantRefused(t *testing.T, err error, reason string) {
	t.Helper()
	var refused *RefusedError
	if !errors.Is(err, ErrRefused) || !errors.As(err, &refused) || refused.Layer != "platform" || !strings.Contains(err.Error(), reason) {
		t.Fatalf("Check() = %v, want a platform refusal with %q", err, reason)
	}
}

func TestCheckAdmitsATaskThatFits(t *testing.T) {
	tel, rec := newTelemetry(t)
	if err := Check(context.Background(), tel, task(200000), platform(1000000), measured(t, "500000")); err != nil {
		t.Fatalf("Check() = %v, want admitted", err)
	}
	if v := rec.Value(t, telemetry.MetricBudgetUsedFrontierTokens, "layer", "platform"); v != 500000 {
		t.Errorf("budget used = %d, want 500000", v)
	}
}

// The planted over-budget task of #6: the message carries the measured basis.
func TestCheckRefusesAnOverBudgetTask(t *testing.T) {
	err := Check(context.Background(), nil, task(200000), platform(1000000), measured(t, "900000"))
	wantRefused(t, err, "declares 200000 frontier tokens; the platform budget is 1000000 and the ledger measured 900000 used, which leaves 100000")
	err = Check(context.Background(), nil, task(1), platform(1000000), measured(t, "1500000"))
	wantRefused(t, err, "measured 1500000 used, which leaves 0")
}

// A ledger that has not measured frontier tokens refuses the task and records
// no use, never a zero.
func TestCheckRefusesOnAnUnmeasuredLedger(t *testing.T) {
	for _, reason := range []string{"not measured", "not measured (see #897)"} {
		tel, rec := newTelemetry(t)
		err := Check(context.Background(), tel, task(0), platform(1000000), readReport(t, `"frontier_tokens":"`+reason+`"`))
		wantRefused(t, err, `frontier tokens as "`+reason+`"`)
		if rec.Recorded(t, telemetry.MetricBudgetUsedFrontierTokens, "layer", "platform") {
			t.Errorf("%s: budget use recorded, want no value", reason)
		}
	}
}

// With the platform layer off, nothing is checked or recorded, even on an
// unmeasured ledger.
func TestCheckIsOffWithoutAPlatformBudget(t *testing.T) {
	tel, rec := newTelemetry(t)
	if err := Check(context.Background(), tel, task(200000), platform(0), readReport(t, `"frontier_tokens":"not measured"`)); err != nil {
		t.Fatalf("Check(layer off) = %v, want admitted", err)
	}
	if rec.Recorded(t, telemetry.MetricBudgetUsedFrontierTokens, "layer", "platform") {
		t.Error("budget use recorded with the layer off, want no value")
	}
}

func TestCheckBoundaries(t *testing.T) {
	ctx := context.Background()
	if err := Check(ctx, nil, task(100000), platform(1000000), measured(t, "900000")); err != nil {
		t.Errorf("Check(exact fit) = %v, want admitted", err)
	}
	wantRefused(t, Check(ctx, nil, task(100001), platform(1000000), measured(t, "900000")), "leaves 100000")
	if err := Check(ctx, nil, task(0), platform(1000000), measured(t, "1000000")); err != nil {
		t.Errorf("Check(zero tokens, nothing left) = %v, want admitted", err)
	}
	tel, rec := newTelemetry(t)
	if err := Check(ctx, tel, task(5), platform(10), measured(t, "0")); err != nil {
		t.Errorf("Check(measured zero use) = %v, want admitted", err)
	}
	if !rec.Recorded(t, telemetry.MetricBudgetUsedFrontierTokens, "layer", "platform") {
		t.Error("measured zero use not recorded, want a recorded 0")
	}
	tel, rec = newTelemetry(t)
	wantRefused(t, Check(ctx, tel, task(1), platform(10), measured(t, "1e300")), "leaves 0")
	if rec.Recorded(t, telemetry.MetricBudgetUsedFrontierTokens, "layer", "platform") {
		t.Error("use above the gauge range recorded, want no value")
	}
}
