// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package telemetry

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/golusoris/golusoris/core/validate"
	"github.com/golusoris/golusoris/observability/metricdef"
	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"

	"github.com/tekmora/epitrop/internal/graph"
)

const instrumentationScope = "github.com/tekmora/epitrop"

// Telemetry holds OpenTelemetry tracer and metric instruments.
type Telemetry struct {
	tracer             trace.Tracer
	effectRuns         *metricdef.Counter
	slotRuns           *metricdef.Counter
	jobRuns            *metricdef.Counter
	checkpointWrites   *metricdef.Counter
	admissionDecisions *metricdef.Counter
	graphTasks         *metricdef.OTelGauge
	budgetUsed         *metricdef.OTelGauge
	allowedGraphStatus []string
}

// New constructs Telemetry with instruments created on the provided meter provider
// and a tracer from the tracer provider. Nil providers default to no-op.
func New(tp trace.TracerProvider, mp metric.MeterProvider) (*Telemetry, error) {
	if validate.IsNil(tp) {
		tp = nooptrace.NewTracerProvider()
	}
	if validate.IsNil(mp) {
		mp = noopmetric.NewMeterProvider()
	}
	tracer := tp.Tracer(instrumentationScope)
	meter := mp.Meter(instrumentationScope)

	cat, err := Catalog()
	if err != nil {
		return nil, fmt.Errorf("telemetry: catalog: %w", err)
	}
	handles, err := cat.RegisterOTel(meter)
	if err != nil {
		return nil, fmt.Errorf("telemetry: register OTel: %w", err)
	}

	t := &Telemetry{tracer: tracer}
	if err := t.bindHandles(cat, handles); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Telemetry) bindHandles(cat *metricdef.Catalog, handles *metricdef.Handles) error {
	var err error
	if t.effectRuns, err = handles.Counter(MetricEffectRunsTotal); err != nil {
		return fmt.Errorf("telemetry: counter %s: %w", MetricEffectRunsTotal, err)
	}
	if t.slotRuns, err = handles.Counter(MetricSlotRunsTotal); err != nil {
		return fmt.Errorf("telemetry: counter %s: %w", MetricSlotRunsTotal, err)
	}
	if t.jobRuns, err = handles.Counter(MetricJobRunsTotal); err != nil {
		return fmt.Errorf("telemetry: counter %s: %w", MetricJobRunsTotal, err)
	}
	if t.checkpointWrites, err = handles.Counter(MetricCheckpointWritesTotal); err != nil {
		return fmt.Errorf("telemetry: counter %s: %w", MetricCheckpointWritesTotal, err)
	}
	if t.admissionDecisions, err = handles.Counter(MetricAdmissionDecisionsTotal); err != nil {
		return fmt.Errorf("telemetry: counter %s: %w", MetricAdmissionDecisionsTotal, err)
	}
	if t.graphTasks, err = handles.OTelGauge(MetricGraphTasks); err != nil {
		return fmt.Errorf("telemetry: gauge %s: %w", MetricGraphTasks, err)
	}
	if t.budgetUsed, err = handles.OTelGauge(MetricBudgetUsedFrontierTokens); err != nil {
		return fmt.Errorf("telemetry: gauge %s: %w", MetricBudgetUsedFrontierTokens, err)
	}
	graphDef, ok := cat.Lookup(MetricGraphTasks)
	if !ok {
		return fmt.Errorf("telemetry: metric %s absent from catalog", MetricGraphTasks)
	}
	t.allowedGraphStatus = graphDef.Limits["status"].Allow
	return nil
}

func (t *Telemetry) inc(ctx context.Context, counter *metricdef.Counter, outcome string) {
	if counter == nil {
		return
	}
	// A failed recording must not fail the caller's operation; it goes to the OTel error handler.
	if err := counter.Inc(ctx, outcome); err != nil {
		otelapi.Handle(err)
	}
}

// EffectRun records one side effect execution outcome.
func (t *Telemetry) EffectRun(ctx context.Context, outcome string) {
	if t == nil {
		return
	}
	t.inc(ctx, t.effectRuns, outcome)
}

// SlotRun records one slot execution outcome.
func (t *Telemetry) SlotRun(ctx context.Context, outcome string) {
	if t == nil {
		return
	}
	t.inc(ctx, t.slotRuns, outcome)
}

// JobRun records one supervisor job start outcome.
func (t *Telemetry) JobRun(ctx context.Context, outcome string) {
	if t == nil {
		return
	}
	t.inc(ctx, t.jobRuns, outcome)
}

// CheckpointWrite records one event log checkpoint write attempt outcome.
func (t *Telemetry) CheckpointWrite(ctx context.Context, outcome string) {
	if t == nil {
		return
	}
	t.inc(ctx, t.checkpointWrites, outcome)
}

// AdmissionDecision records one router alias admission decision outcome.
func (t *Telemetry) AdmissionDecision(ctx context.Context, outcome string) {
	if t == nil {
		return
	}
	t.inc(ctx, t.admissionDecisions, outcome)
}

// GraphTasks records queue depth for each task status in counts.
// Statuses of kind fix-round-N fold into "fix-round".
func (t *Telemetry) GraphTasks(_ context.Context, counts map[graph.StatusName]int) {
	if t == nil || t.graphTasks == nil {
		return
	}
	recorded := make(map[string]float64, len(t.allowedGraphStatus)+1)
	for i := 0; i < len(t.allowedGraphStatus); i++ {
		recorded[t.allowedGraphStatus[i]] = 0
	}
	var otherCount float64
	for status, count := range counts {
		s := string(status)
		if s == string(graph.StatusFixRound) || strings.HasPrefix(s, "fix-round") {
			s = "fix-round"
		}
		if slices.Contains(t.allowedGraphStatus, s) {
			recorded[s] += float64(count)
		} else {
			otherCount += float64(count)
		}
	}
	if otherCount > 0 {
		recorded[metricdef.OtherValue] = otherCount
	}
	for s, c := range recorded {
		setGauge(t.graphTasks, c, s)
	}
}

// BudgetUsed records the frontier tokens layer has used. A nil used means the ledger
// has not measured them; nothing is recorded then, so the metric never shows a zero
// that was not measured.
func (t *Telemetry) BudgetUsed(_ context.Context, layer string, used *int64) {
	if t == nil || t.budgetUsed == nil || used == nil {
		return
	}
	setGauge(t.budgetUsed, float64(*used), layer)
}

// setGauge records v under label. A failed recording must not fail the caller's
// operation; it goes to the OTel error handler.
func setGauge(g *metricdef.OTelGauge, v float64, label string) {
	if err := g.Set(v, label); err != nil {
		otelapi.Handle(err)
	}
}

// StartSpan starts an OpenTelemetry span on the configured tracer.
func (t *Telemetry) StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if t == nil || t.tracer == nil {
		return nooptrace.NewTracerProvider().Tracer(instrumentationScope).Start(ctx, name, trace.WithAttributes(attrs...))
	}
	return t.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}

type providerKey struct {
	tp trace.TracerProvider
	mp metric.MeterProvider
}

var (
	defaultMu    sync.RWMutex
	defaultCache = make(map[providerKey]*Telemetry)
)

// Default returns a Telemetry handle built on otelapi global providers.
func Default() *Telemetry {
	tp := otelapi.GetTracerProvider()
	mp := otelapi.GetMeterProvider()
	key := providerKey{tp: tp, mp: mp}

	defaultMu.RLock()
	t, ok := defaultCache[key]
	defaultMu.RUnlock()
	if ok {
		return t
	}

	defaultMu.Lock()
	defer defaultMu.Unlock()
	if t, ok = defaultCache[key]; ok {
		return t
	}

	created, err := New(tp, mp)
	if err != nil {
		otelapi.Handle(err)
		return nil
	}
	defaultCache[key] = created
	return created
}
