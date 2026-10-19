// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

// Package telemetrytest provides in-memory test instrumentation helpers.
package telemetrytest

import (
	"context"
	"strings"
	"testing"
	"time"

	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// Recorder captures spans and metrics in memory.
type Recorder struct {
	exporter *tracetest.InMemoryExporter
	reader   *sdkmetric.ManualReader
	tp       *sdktrace.TracerProvider
	mp       *sdkmetric.MeterProvider
}

// Install sets global tracer and meter providers to in-memory implementations
// and restores the previous providers through t.Cleanup.
func Install(t testing.TB) *Recorder {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	oldTP := otelapi.GetTracerProvider()
	oldMP := otelapi.GetMeterProvider()

	otelapi.SetTracerProvider(tp)
	otelapi.SetMeterProvider(mp)

	t.Cleanup(func() {
		if cur := otelapi.GetTracerProvider(); cur != oldTP {
			otelapi.SetTracerProvider(oldTP)
		}
		if cur := otelapi.GetMeterProvider(); cur != oldMP {
			otelapi.SetMeterProvider(oldMP)
		}
	})

	return &Recorder{
		exporter: exporter,
		reader:   reader,
		tp:       tp,
		mp:       mp,
	}
}

// TracerProvider returns the underlying trace provider.
func (r *Recorder) TracerProvider() trace.TracerProvider {
	return r.tp
}

// MeterProvider returns the underlying meter provider.
func (r *Recorder) MeterProvider() metric.MeterProvider {
	return r.mp
}

// Spans returns captured spans.
func (r *Recorder) Spans() tracetest.SpanStubs {
	if r == nil || r.exporter == nil {
		return nil
	}
	return r.exporter.GetSpans()
}

// Value returns the aggregated metric value matching attrKey=attrVal, truncated to int64.
func (r *Recorder) Value(t testing.TB, name, attrKey, attrVal string) int64 {
	t.Helper()
	return int64(r.FloatValue(t, name, attrKey, attrVal))
}

// Recorded reports whether metric name has a data point with attrKey=attrVal, so a
// test can tell a value that was never written from a written zero.
func (r *Recorder) Recorded(t testing.TB, name, attrKey, attrVal string) bool {
	t.Helper()
	rm := r.ResourceMetrics(t)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name && hasGaugePoint(m.Data, attrKey, attrVal) {
				return true
			}
		}
	}
	return false
}

// ResourceMetrics returns captured resource metrics.
func (r *Recorder) ResourceMetrics(t testing.TB) metricdata.ResourceMetrics {
	t.Helper()
	if r == nil || r.reader == nil {
		return metricdata.ResourceMetrics{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("telemetrytest: collect metrics: %v", err)
	}
	return rm
}

// FloatValue returns the aggregated float64 metric value matching attrKey=attrVal.
func (r *Recorder) FloatValue(t testing.TB, name, attrKey, attrVal string) float64 {
	t.Helper()
	rm := r.ResourceMetrics(t)
	trimmed := strings.TrimSuffix(name, "_total")
	var sum float64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name && m.Name != trimmed {
				continue
			}
			sum += extractFloatDataPoint(m.Data, attrKey, attrVal)
		}
	}
	return sum
}

func hasGaugePoint(data metricdata.Aggregation, attrKey, attrVal string) bool {
	switch d := data.(type) {
	case metricdata.Gauge[int64]:
		for _, dp := range d.DataPoints {
			if hasAttr(dp.Attributes, attrKey, attrVal) {
				return true
			}
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			if hasAttr(dp.Attributes, attrKey, attrVal) {
				return true
			}
		}
	}
	return false
}

func extractFloatDataPoint(data metricdata.Aggregation, attrKey, attrVal string) float64 {
	switch d := data.(type) {
	case metricdata.Sum[int64]:
		return sumIntPoints(d.DataPoints, attrKey, attrVal)
	case metricdata.Sum[float64]:
		return sumFloatPoints(d.DataPoints, attrKey, attrVal)
	case metricdata.Gauge[int64]:
		return gaugeIntPoints(d.DataPoints, attrKey, attrVal)
	case metricdata.Gauge[float64]:
		return gaugeFloatPoints(d.DataPoints, attrKey, attrVal)
	default:
		return 0
	}
}

func sumFloatPoints(dps []metricdata.DataPoint[float64], attrKey, attrVal string) float64 {
	var s float64
	for _, dp := range dps {
		if hasAttr(dp.Attributes, attrKey, attrVal) {
			s += dp.Value
		}
	}
	return s
}

func sumIntPoints(dps []metricdata.DataPoint[int64], attrKey, attrVal string) float64 {
	var s int64
	for _, dp := range dps {
		if hasAttr(dp.Attributes, attrKey, attrVal) {
			s += dp.Value
		}
	}
	return float64(s)
}

func gaugeFloatPoints(dps []metricdata.DataPoint[float64], attrKey, attrVal string) float64 {
	for _, dp := range dps {
		if hasAttr(dp.Attributes, attrKey, attrVal) {
			return dp.Value
		}
	}
	return 0
}

func gaugeIntPoints(dps []metricdata.DataPoint[int64], attrKey, attrVal string) float64 {
	for _, dp := range dps {
		if hasAttr(dp.Attributes, attrKey, attrVal) {
			return float64(dp.Value)
		}
	}
	return 0
}

func hasAttr(set attribute.Set, key, want string) bool {
	val, ok := set.Value(attribute.Key(key))
	return ok && val.AsString() == want
}

// SpanAttr retrieves the string value of key from span attributes.
func (r *Recorder) SpanAttr(span tracetest.SpanStub, key string) (string, bool) {
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			return attr.Value.AsString(), true
		}
	}
	return "", false
}
