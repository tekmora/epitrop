// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package telemetry

import (
	"context"
	"fmt"
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
	tracer                   trace.Tracer
	effectRuns               metric.Int64Counter
	slotRuns                 metric.Int64Counter
	jobRuns                  metric.Int64Counter
	checkpointWrites         metric.Int64Counter
	admissionDecisions       metric.Int64Counter
	graphTasks               metric.Int64Gauge
	budgetUsed               metric.Int64Gauge
	allowedEffectOutcome     map[string]struct{}
	allowedSlotOutcome       map[string]struct{}
	allowedJobOutcome        map[string]struct{}
	allowedCheckpointOutcome map[string]struct{}
	allowedAdmissionOutcome  map[string]struct{}
	allowedGraphStatus       map[string]struct{}
	allowedBudgetLayer       map[string]struct{}
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
		return nil, err
	}
	t := &Telemetry{
		tracer: tracer,
	}
	if err := t.initInstruments(meter, cat); err != nil {
		return nil, err
	}
	t.initAllowedSets(cat)
	return t, nil
}

func (t *Telemetry) initInstruments(meter metric.Meter, cat *metricdef.Catalog) error {
	for _, def := range cat.Defs() {
		switch def.Kind {
		case metricdef.KindCounter:
			c, err := meter.Int64Counter(
				strings.TrimSuffix(def.Name, "_total"),
				metric.WithDescription(def.Help),
			)
			if err != nil {
				return err
			}
			t.setCounter(def.Name, c)
		case metricdef.KindGauge:
			g, err := meter.Int64Gauge(
				def.Name,
				metric.WithDescription(def.Help),
			)
			if err != nil {
				return err
			}
			t.setGauge(def.Name, g)
		default:
			return fmt.Errorf("telemetry: unknown metric kind %v for %q", def.Kind, def.Name)
		}
	}
	return nil
}

func (t *Telemetry) setCounter(name string, c metric.Int64Counter) {
	switch name {
	case MetricEffectRunsTotal:
		t.effectRuns = c
	case MetricSlotRunsTotal:
		t.slotRuns = c
	case MetricJobRunsTotal:
		t.jobRuns = c
	case MetricCheckpointWritesTotal:
		t.checkpointWrites = c
	case MetricAdmissionDecisionsTotal:
		t.admissionDecisions = c
	}
}

func (t *Telemetry) setGauge(name string, g metric.Int64Gauge) {
	switch name {
	case MetricGraphTasks:
		t.graphTasks = g
	case MetricBudgetUsedFrontierTokens:
		t.budgetUsed = g
	}
}

func (t *Telemetry) initAllowedSets(cat *metricdef.Catalog) {
	for _, def := range cat.Defs() {
		t.applyAllowedLimits(def)
	}
}

func (t *Telemetry) applyAllowedLimits(def metricdef.Def) {
	switch def.Name {
	case MetricEffectRunsTotal:
		t.allowedEffectOutcome = toAllowSet(def.Limits["outcome"].Allow)
	case MetricSlotRunsTotal:
		t.allowedSlotOutcome = toAllowSet(def.Limits["outcome"].Allow)
	case MetricJobRunsTotal:
		t.allowedJobOutcome = toAllowSet(def.Limits["outcome"].Allow)
	case MetricCheckpointWritesTotal:
		t.allowedCheckpointOutcome = toAllowSet(def.Limits["outcome"].Allow)
	case MetricAdmissionDecisionsTotal:
		t.allowedAdmissionOutcome = toAllowSet(def.Limits["outcome"].Allow)
	case MetricGraphTasks:
		t.allowedGraphStatus = toAllowSet(def.Limits["status"].Allow)
	case MetricBudgetUsedFrontierTokens:
		t.allowedBudgetLayer = toAllowSet(def.Limits["layer"].Allow)
	}
}

func toAllowSet(items []string) map[string]struct{} {
	m := make(map[string]struct{}, len(items))
	for i := 0; i < len(items); i++ {
		m[items[i]] = struct{}{}
	}
	return m
}

func (t *Telemetry) add(ctx context.Context, counter metric.Int64Counter, allow map[string]struct{}, outcome string) {
	if counter == nil {
		return
	}
	if _, ok := allow[outcome]; !ok {
		outcome = metricdef.OtherValue
	}
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// EffectRun records one side effect execution outcome.
func (t *Telemetry) EffectRun(ctx context.Context, outcome string) {
	if t == nil {
		return
	}
	t.add(ctx, t.effectRuns, t.allowedEffectOutcome, outcome)
}

// SlotRun records one slot execution outcome.
func (t *Telemetry) SlotRun(ctx context.Context, outcome string) {
	if t == nil {
		return
	}
	t.add(ctx, t.slotRuns, t.allowedSlotOutcome, outcome)
}

// JobRun records one supervisor job start outcome.
func (t *Telemetry) JobRun(ctx context.Context, outcome string) {
	if t == nil {
		return
	}
	t.add(ctx, t.jobRuns, t.allowedJobOutcome, outcome)
}

// CheckpointWrite records one event log checkpoint write attempt outcome.
func (t *Telemetry) CheckpointWrite(ctx context.Context, outcome string) {
	if t == nil {
		return
	}
	t.add(ctx, t.checkpointWrites, t.allowedCheckpointOutcome, outcome)
}

// AdmissionDecision records one router alias admission decision outcome.
func (t *Telemetry) AdmissionDecision(ctx context.Context, outcome string) {
	if t == nil {
		return
	}
	t.add(ctx, t.admissionDecisions, t.allowedAdmissionOutcome, outcome)
}

// GraphTasks records queue depth for each task status in counts.
// Statuses of kind fix-round-N fold into "fix-round".
func (t *Telemetry) GraphTasks(ctx context.Context, counts map[graph.StatusName]int) {
	if t == nil || t.graphTasks == nil {
		return
	}
	recorded := make(map[string]int64, len(t.allowedGraphStatus)+1)
	for s := range t.allowedGraphStatus {
		recorded[s] = 0
	}
	var otherCount int64
	for status, count := range counts {
		s := string(status)
		if s == string(graph.StatusFixRound) || strings.HasPrefix(s, "fix-round") {
			s = "fix-round"
		}
		if _, ok := t.allowedGraphStatus[s]; ok {
			recorded[s] += int64(count)
		} else {
			otherCount += int64(count)
		}
	}
	if otherCount > 0 {
		recorded[metricdef.OtherValue] = otherCount
	}
	for s, c := range recorded {
		t.graphTasks.Record(ctx, c, metric.WithAttributes(attribute.String("status", s)))
	}
}

// BudgetUsed records the frontier tokens layer has used. A nil used means the ledger
// has not measured them; nothing is recorded then, so the metric never shows a zero
// that was not measured.
func (t *Telemetry) BudgetUsed(ctx context.Context, layer string, used *int64) {
	if t == nil || t.budgetUsed == nil || used == nil {
		return
	}
	if _, ok := t.allowedBudgetLayer[layer]; !ok {
		layer = metricdef.OtherValue
	}
	t.budgetUsed.Record(ctx, *used, metric.WithAttributes(attribute.String("layer", layer)))
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
