// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package telemetry

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golusoris/golusoris/observability/metricdef"
	golusorisotel "github.com/golusoris/golusoris/otel"
	"github.com/prometheus/client_golang/prometheus"
	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/tekmora/epitrop/internal/graph"
	"github.com/tekmora/epitrop/internal/telemetry/telemetrytest"
)

func TestTracesInMemory(t *testing.T) {
	rec := telemetrytest.Install(t)
	tel, err := New(rec.TracerProvider(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, span := tel.StartSpan(context.Background(), "test.span", attribute.String("epitrop.unit", "unit-42"))
	span.End()

	spans := rec.Spans()
	if len(spans) != 1 {
		t.Fatalf("spans count = %d, want 1", len(spans))
	}
	if spans[0].Name != "test.span" {
		t.Errorf("span name = %s, want test.span", spans[0].Name)
	}
	if val, ok := rec.SpanAttr(spans[0], "epitrop.unit"); !ok || val != "unit-42" {
		t.Errorf("epitrop.unit attribute %q (ok=%v), want unit-42", val, ok)
	}
}

func TestMetricsInMemory(t *testing.T) {
	rec := telemetrytest.Install(t)
	tel, err := New(nil, rec.MeterProvider())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	tel.EffectRun(ctx, "executed")
	tel.SlotRun(ctx, "done")
	tel.SlotRun(ctx, "preempted")
	tel.JobRun(ctx, "started")
	tel.CheckpointWrite(ctx, "written")
	tel.AdmissionDecision(ctx, "admitted")
	tel.GraphTasks(ctx, map[graph.StatusName]int{
		graph.StatusReady: 4,
	})

	if v := rec.Value(t, MetricEffectRunsTotal, "outcome", "executed"); v != 1 {
		t.Errorf("effect runs executed = %d, want 1", v)
	}
	if v := rec.Value(t, MetricSlotRunsTotal, "outcome", "done"); v != 1 {
		t.Errorf("slot runs done = %d, want 1", v)
	}
	// A preempted run is counted under its own outcome, not folded into the overflow value.
	if v := rec.Value(t, MetricSlotRunsTotal, "outcome", "preempted"); v != 1 {
		t.Errorf("slot runs preempted = %d, want 1", v)
	}
	if v := rec.Value(t, MetricJobRunsTotal, "outcome", "started"); v != 1 {
		t.Errorf("job runs started = %d, want 1", v)
	}
	if v := rec.Value(t, MetricCheckpointWritesTotal, "outcome", "written"); v != 1 {
		t.Errorf("checkpoint writes written = %d, want 1", v)
	}
	if v := rec.Value(t, MetricAdmissionDecisionsTotal, "outcome", "admitted"); v != 1 {
		t.Errorf("admission decisions admitted = %d, want 1", v)
	}
	if v := rec.Value(t, MetricGraphTasks, "status", "ready"); v != 4 {
		t.Errorf("graph tasks ready = %d, want 4", v)
	}
}

func testCatalog(t *testing.T) *metricdef.Catalog {
	t.Helper()
	c, err := Catalog()
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	return c
}

func TestCatalogBuilds(t *testing.T) {
	c, err := buildCatalog()
	if err != nil {
		t.Fatalf("buildCatalog: %v", err)
	}
	if got := len(c.Defs()); got != 7 {
		t.Fatalf("defs count = %d, want 7", got)
	}
}

func TestCatalogVsExposedNamesBothWays(t *testing.T) {
	reg, providers := setupPrometheusTestRegistry(t)
	tel, err := New(providers.Tracer, providers.Meter)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	tel.EffectRun(ctx, "executed")
	tel.SlotRun(ctx, "done")
	tel.JobRun(ctx, "started")
	tel.CheckpointWrite(ctx, "written")
	tel.AdmissionDecision(ctx, "admitted")
	tel.GraphTasks(ctx, map[graph.StatusName]int{graph.StatusReady: 1})
	used := int64(1)
	tel.BudgetUsed(ctx, BudgetLayerPlatform, &used)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	exposed := make(map[string]bool)
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), "epitrop_") {
			exposed[mf.GetName()] = true
		}
	}

	catalogDefs := make(map[string]bool)
	for _, def := range testCatalog(t).Defs() {
		catalogDefs[def.Name] = true
	}

	for defName := range catalogDefs {
		if !exposed[defName] {
			t.Errorf("catalog def %q was not exposed by prometheus registry", defName)
		}
	}
	for expName := range exposed {
		if !catalogDefs[expName] {
			t.Errorf("exposed metric %q is absent from Catalog", expName)
		}
	}
}

func setupPrometheusTestRegistry(t *testing.T) (*prometheus.Registry, *golusorisotel.Providers) {
	t.Helper()
	for _, env := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
		"OTEL_SDK_DISABLED",
	} {
		t.Setenv(env, "")
	}

	oldTP := otelapi.GetTracerProvider()
	oldMP := otelapi.GetMeterProvider()
	oldProp := otelapi.GetTextMapPropagator()

	reg := prometheus.NewRegistry()
	opts := golusorisotel.Options{
		Enabled: true,
		Service: golusorisotel.ServiceOptions{Name: "test"},
		Export:  golusorisotel.ExportOptions{Prometheus: true},
	}
	providers, err := golusorisotel.NewWithRegisterer(t.Context(), opts, reg)
	if err != nil {
		t.Fatalf("NewWithRegisterer: %v", err)
	}
	t.Cleanup(func() {
		if cur := otelapi.GetTracerProvider(); cur != oldTP {
			otelapi.SetTracerProvider(oldTP)
		}
		if cur := otelapi.GetMeterProvider(); cur != oldMP {
			otelapi.SetMeterProvider(oldMP)
		}
		// golusoris always installs its composite propagator, so oldProp differs and is set
		// back unconditionally: a composite propagator is a slice and cannot be compared.
		otelapi.SetTextMapPropagator(oldProp)
		if err := providers.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return reg, providers
}

func TestDocsMetricListMatchesCatalog(t *testing.T) {
	docsPath := filepath.Join("..", "..", "docs", "telemetry.md")
	docsMetrics := parseDocsMetricNames(t, docsPath)

	catalogDefs := make(map[string]bool)
	for _, def := range testCatalog(t).Defs() {
		catalogDefs[def.Name] = true
	}

	if len(docsMetrics) != len(catalogDefs) {
		t.Fatalf("docs metrics count = %d, want %d (docs: %v, catalog: %v)",
			len(docsMetrics), len(catalogDefs), docsMetrics, catalogDefs)
	}
	for c := range catalogDefs {
		if !docsMetrics[c] {
			t.Errorf("catalog def %q missing from docs/telemetry.md table", c)
		}
	}
	for d := range docsMetrics {
		if !catalogDefs[d] {
			t.Errorf("docs/telemetry.md lists metric %q absent from catalog", d)
		}
	}
}

func parseDocsMetricNames(t *testing.T, docsPath string) map[string]bool {
	t.Helper()
	f, err := os.Open(docsPath)
	if err != nil {
		t.Fatalf("Open docs/telemetry.md: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("Close docs/telemetry.md: %v", err)
		}
	}()

	docsMetrics := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	inTable := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if processDocsMetricLine(line, &inTable, docsMetrics) {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan docs: %v", err)
	}
	return docsMetrics
}

func processDocsMetricLine(line string, inTable *bool, docsMetrics map[string]bool) bool {
	if strings.HasPrefix(line, "| Name |") {
		*inTable = true
		return false
	}
	if !*inTable {
		return false
	}
	if !strings.HasPrefix(line, "|") {
		return len(docsMetrics) > 0
	}
	if name := parseMetricRow(line); name != "" {
		docsMetrics[name] = true
	}
	return false
}

func parseMetricRow(line string) string {
	if strings.Contains(line, ":---") || strings.Contains(line, "---") {
		return ""
	}
	parts := strings.Split(line, "|")
	if len(parts) < 2 {
		return ""
	}
	name := strings.TrimSpace(parts[1])
	name = strings.Trim(name, "`")
	if name == "Name" {
		return ""
	}
	return name
}

func TestUnknownOutcomeAndNilReceiverBoundary(t *testing.T) {
	rec := telemetrytest.Install(t)
	tel, err := New(nil, rec.MeterProvider())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	tel.EffectRun(ctx, "unrecognized_effect")
	tel.SlotRun(ctx, "unrecognized_slot")
	tel.JobRun(ctx, "unrecognized_job")
	tel.AdmissionDecision(ctx, "unrecognized_admission")
	tel.GraphTasks(ctx, map[graph.StatusName]int{
		"unrecognized_status": 7,
		"fix-round-3":         2,
	})

	if v := rec.Value(t, MetricEffectRunsTotal, "outcome", "other"); v != 1 {
		t.Errorf("unknown effect outcome = %d, want 1 under other", v)
	}
	if v := rec.Value(t, MetricSlotRunsTotal, "outcome", "other"); v != 1 {
		t.Errorf("unknown slot outcome = %d, want 1 under other", v)
	}
	if v := rec.Value(t, MetricJobRunsTotal, "outcome", "other"); v != 1 {
		t.Errorf("unknown job outcome = %d, want 1 under other", v)
	}
	if v := rec.Value(t, MetricAdmissionDecisionsTotal, "outcome", "other"); v != 1 {
		t.Errorf("unknown admission outcome = %d, want 1 under other", v)
	}
	if v := rec.Value(t, MetricGraphTasks, "status", "other"); v != 7 {
		t.Errorf("unknown graph status = %d, want 7 under other", v)
	}
	if v := rec.Value(t, MetricGraphTasks, "status", "fix-round"); v != 2 {
		t.Errorf("folded fix-round = %d, want 2 under fix-round", v)
	}

	var nilT *Telemetry
	nilT.EffectRun(ctx, "executed")
	nilT.SlotRun(ctx, "done")
	nilT.JobRun(ctx, "started")
	nilT.AdmissionDecision(ctx, "admitted")
	nilT.GraphTasks(ctx, map[graph.StatusName]int{graph.StatusReady: 1})
	nilT.BudgetUsed(ctx, BudgetLayerPlatform, new(int64))
	spanCtx, span := nilT.StartSpan(ctx, "noop", attribute.String("k", "v"))
	if spanCtx == nil || span == nil {
		t.Fatal("nil Telemetry StartSpan returned nil context or span")
	}
	span.RecordError(errors.New("fail"))
	span.SetStatus(codes.Error, "fail")
	span.End()
}

func TestGraphTasksGaugeReset(t *testing.T) {
	rec := telemetrytest.Install(t)
	tel, err := New(nil, rec.MeterProvider())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	tel.GraphTasks(ctx, map[graph.StatusName]int{graph.StatusReady: 3})
	if got := rec.Value(t, MetricGraphTasks, "status", "ready"); got != 3 {
		t.Fatalf("ready = %d, want 3", got)
	}
	if got := rec.Value(t, MetricGraphTasks, "status", "landed"); got != 0 {
		t.Fatalf("landed = %d, want 0", got)
	}

	tel.GraphTasks(ctx, map[graph.StatusName]int{graph.StatusLanded: 2})
	if got := rec.Value(t, MetricGraphTasks, "status", "ready"); got != 0 {
		t.Fatalf("ready = %d, want 0 after reset", got)
	}
	if got := rec.Value(t, MetricGraphTasks, "status", "landed"); got != 2 {
		t.Fatalf("landed = %d, want 2", got)
	}

	tel.GraphTasks(ctx, map[graph.StatusName]int{})
	if got := rec.Value(t, MetricGraphTasks, "status", "landed"); got != 0 {
		t.Fatalf("landed = %d, want 0 after empty call", got)
	}
}

func TestGraphStatusesMatchTelemetryCatalog(t *testing.T) {
	def, ok := testCatalog(t).Lookup(MetricGraphTasks)
	if !ok {
		t.Fatalf("metric %s not found in catalog", MetricGraphTasks)
	}
	catalogAllowed := make(map[string]bool)
	for _, a := range def.Limits["status"].Allow {
		catalogAllowed[a] = true
	}
	allStatuses := []graph.StatusName{
		graph.StatusProposed,
		graph.StatusReady,
		graph.StatusClaimed,
		graph.StatusImplementing,
		graph.StatusReview,
		graph.StatusFixRound,
		graph.StatusBlocked,
		graph.StatusQueued,
		graph.StatusLanding,
		graph.StatusLanded,
		graph.StatusDropped,
	}
	expected := make(map[string]bool, len(allStatuses))
	for _, s := range allStatuses {
		name := string(s)
		if s == graph.StatusFixRound {
			name = "fix-round"
		}
		expected[name] = true
	}
	for exp := range expected {
		if !catalogAllowed[exp] {
			t.Errorf("status %q missing from catalog Allow", exp)
		}
	}
	for cat := range catalogAllowed {
		if !expected[cat] {
			t.Errorf("catalog Allow has unexpected status %q", cat)
		}
	}
}

func TestDefault(t *testing.T) {
	d := Default()
	if d == nil {
		t.Fatal("Default() returned nil with default providers")
	}
}

// An unmeasured use writes nothing; a measured zero is a value.
func TestBudgetUsedGauge(t *testing.T) {
	rec := telemetrytest.Install(t)
	tel, err := New(nil, rec.MeterProvider())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	tel.BudgetUsed(ctx, BudgetLayerPlatform, nil)
	if rec.Recorded(t, MetricBudgetUsedFrontierTokens, "layer", BudgetLayerPlatform) {
		t.Fatal("unmeasured use recorded, want no value")
	}
	zero := int64(0)
	tel.BudgetUsed(ctx, BudgetLayerPlatform, &zero)
	if !rec.Recorded(t, MetricBudgetUsedFrontierTokens, "layer", BudgetLayerPlatform) {
		t.Fatal("measured zero not recorded, want a value of 0")
	}
	used := int64(750000)
	tel.BudgetUsed(ctx, BudgetLayerPlatform, &used)
	if got := rec.Value(t, MetricBudgetUsedFrontierTokens, "layer", BudgetLayerPlatform); got != 750000 {
		t.Fatalf("platform use = %d, want 750000", got)
	}
	tel.BudgetUsed(ctx, "unrecognized_layer", &used)
	if got := rec.Value(t, MetricBudgetUsedFrontierTokens, "layer", "other"); got != 750000 {
		t.Fatalf("unknown budget layer = %d, want 750000 under other", got)
	}
}
