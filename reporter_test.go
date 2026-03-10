package metrics

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// ─── Existing name-prefixing tests ──────────────────────────────────────────

func TestOTELMetricNamePrefixing(t *testing.T) {
	tests := []struct {
		name     string
		system   string
		sub      string
		path     string
		wantName string
	}{
		{"full_prefix", "ns", "sub", "connections", "ns_sub_connections"},
		{"system_only", "ns", "", "connections", "ns_connections"},
		{"subsystem_only", "", "sub", "connections", "sub_connections"},
		{"path_only", "", "", "connections", "connections"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer provider.Shutdown(context.Background())

			reporter := OTELReporter(ReporterConf{
				System:    tt.system,
				Subsystem: tt.sub,
			}, WithMeterProvider(provider))

			gauge := reporter.Gauge(MetricConf{Path: tt.path})
			gauge.Set(1, map[string]string{})

			rm := collectOTEL(t, reader)
			got := findMetricName(rm)
			if got != tt.wantName {
				t.Errorf("metric name = %q, want %q", got, tt.wantName)
			}
		})
	}
}

func TestOTELMeterName(t *testing.T) {
	tests := []struct {
		name          string
		system        string
		sub           string
		wantMeterName string
	}{
		{"full_prefix", "ns", "sub", "ns_sub"},
		{"system_only", "ns", "", "ns"},
		{"subsystem_only", "", "sub", "sub"},
		{"empty", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer provider.Shutdown(context.Background())

			reporter := OTELReporter(ReporterConf{
				System:    tt.system,
				Subsystem: tt.sub,
			}, WithMeterProvider(provider))

			gauge := reporter.Gauge(MetricConf{Path: "test_metric"})
			gauge.Set(1, map[string]string{})

			rm := collectOTEL(t, reader)
			got := findMeterName(rm)
			if got != tt.wantMeterName {
				t.Errorf("meter name = %q, want %q", got, tt.wantMeterName)
			}
		})
	}
}

func TestOTELSubReporterPrefixing(t *testing.T) {
	tests := []struct {
		name     string
		childSub string
		path     string
		wantName string
	}{
		{"with_child_subsystem", "child", "connections", "ns_sub_child_connections"},
		{"no_child_subsystem", "", "connections", "ns_sub_connections"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer provider.Shutdown(context.Background())

			parent := OTELReporter(ReporterConf{
				System:    "ns",
				Subsystem: "sub",
			}, WithMeterProvider(provider))

			child := parent.Reporter(ReporterConf{Subsystem: tt.childSub})

			gauge := child.Gauge(MetricConf{Path: tt.path})
			gauge.Set(1, map[string]string{})

			rm := collectOTEL(t, reader)
			got := findMetricName(rm)
			if got != tt.wantName {
				t.Errorf("metric name = %q, want %q", got, tt.wantName)
			}
		})
	}
}

func TestPrometheusMetricNamePrefixing(t *testing.T) {
	tests := []struct {
		name     string
		system   string
		sub      string
		path     string
		wantName string
	}{
		{"full_prefix", "ns", "sub", "connections", "ns_sub_connections"},
		{"system_only", "ns", "", "connections", "ns_connections"},
		{"subsystem_only", "", "sub", "connections", "sub_connections"},
		{"path_only", "", "", "connections", "connections"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reporter := PrometheusReporter(ReporterConf{
				System:    tt.system,
				Subsystem: tt.sub,
			})

			gauge := reporter.Gauge(MetricConf{Path: tt.path})
			gauge.Set(1, map[string]string{})

			assertPromMetricExists(t, tt.wantName)

			reporter.UnRegister(tt.path)
		})
	}
}

func TestPrometheusSubReporterPrefixing(t *testing.T) {
	tests := []struct {
		name     string
		childSub string
		path     string
		wantName string
	}{
		{"with_child_subsystem", "child", "connections", "ns_sub_child_connections"},
		{"no_child_subsystem", "", "connections", "ns_sub_connections"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := PrometheusReporter(ReporterConf{
				System:    "ns",
				Subsystem: "sub",
			})

			child := parent.Reporter(ReporterConf{Subsystem: tt.childSub})

			gauge := child.Gauge(MetricConf{Path: tt.path})
			gauge.Set(1, map[string]string{})

			assertPromMetricExists(t, tt.wantName)

			child.UnRegister(tt.path)
		})
	}
}

// ─── 1. Pure function tests ─────────────────────────────────────────────────

func TestMergeLabels(t *testing.T) {
	t.Run("both_non_nil", func(t *testing.T) {
		got := mergeLabels(map[string]string{"a": "1"}, map[string]string{"b": "2"})
		if len(got) != 2 || got["a"] != "1" || got["b"] != "2" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("from_nil", func(t *testing.T) {
		got := mergeLabels(nil, map[string]string{"b": "2"})
		if len(got) != 1 || got["b"] != "2" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("to_nil", func(t *testing.T) {
		got := mergeLabels(map[string]string{"a": "1"}, nil)
		if len(got) != 1 || got["a"] != "1" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("both_nil", func(t *testing.T) {
		got := mergeLabels(nil, nil)
		if got == nil || len(got) != 0 {
			t.Errorf("expected empty non-nil map, got %v", got)
		}
	})

	t.Run("empty_maps", func(t *testing.T) {
		got := mergeLabels(map[string]string{}, map[string]string{})
		if len(got) != 0 {
			t.Errorf("expected empty map, got %v", got)
		}
	})

	t.Run("duplicate_key_panics", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic on duplicate key")
			}
			msg := fmt.Sprintf("%v", r)
			if msg != "label x already registered" {
				t.Errorf("unexpected panic message: %s", msg)
			}
		}()
		mergeLabels(map[string]string{"x": "1"}, map[string]string{"x": "2"})
	})

	t.Run("does_not_mutate_inputs", func(t *testing.T) {
		from := map[string]string{"a": "1"}
		to := map[string]string{"b": "2"}
		mergeLabels(from, to)
		if len(from) != 1 || len(to) != 1 {
			t.Errorf("inputs were mutated: from=%v to=%v", from, to)
		}
	})
}

func TestLabelsKey(t *testing.T) {
	t.Run("deterministic_order", func(t *testing.T) {
		got := labelsKey(map[string]string{"b": "2", "a": "1"})
		if got != "a=1;b=2;" {
			t.Errorf("got %q, want %q", got, "a=1;b=2;")
		}
	})

	t.Run("empty_map", func(t *testing.T) {
		got := labelsKey(map[string]string{})
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("single_entry", func(t *testing.T) {
		got := labelsKey(map[string]string{"method": "GET"})
		if got != "method=GET;" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("nil_map", func(t *testing.T) {
		got := labelsKey(nil)
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestLabelsToAttributes(t *testing.T) {
	t.Run("converts_labels", func(t *testing.T) {
		attrs := labelsToAttributes(map[string]string{"method": "GET", "status": "200"})
		if len(attrs) != 2 {
			t.Fatalf("expected 2 attributes, got %d", len(attrs))
		}
		found := map[string]string{}
		for _, a := range attrs {
			found[string(a.Key)] = a.Value.AsString()
		}
		if found["method"] != "GET" || found["status"] != "200" {
			t.Errorf("got %v", found)
		}
	})

	t.Run("empty_map", func(t *testing.T) {
		attrs := labelsToAttributes(map[string]string{})
		if len(attrs) != 0 {
			t.Errorf("expected 0 attributes, got %d", len(attrs))
		}
	})

	t.Run("nil_map", func(t *testing.T) {
		attrs := labelsToAttributes(nil)
		if len(attrs) != 0 {
			t.Errorf("expected 0 attributes, got %d", len(attrs))
		}
	})
}

func TestApplyRecordOptions(t *testing.T) {
	t.Run("default_context", func(t *testing.T) {
		o := applyRecordOptions(nil)
		if o.ctx == nil {
			t.Fatal("expected non-nil default context")
		}
	})

	t.Run("custom_context", func(t *testing.T) {
		type ctxKey struct{}
		ctx := context.WithValue(context.Background(), ctxKey{}, "val")
		o := applyRecordOptions([]RecordOption{WithContext(ctx)})
		if o.ctx.Value(ctxKey{}) != "val" {
			t.Error("custom context not applied")
		}
	})

	t.Run("last_option_wins", func(t *testing.T) {
		type ctxKey struct{}
		ctx1 := context.WithValue(context.Background(), ctxKey{}, "first")
		ctx2 := context.WithValue(context.Background(), ctxKey{}, "second")
		o := applyRecordOptions([]RecordOption{WithContext(ctx1), WithContext(ctx2)})
		if o.ctx.Value(ctxKey{}) != "second" {
			t.Error("expected last option to win")
		}
	})
}

// ─── 2. Noop backend tests ─────────────────────────────────────────────────

func TestNoopReporter(t *testing.T) {
	r := NoopReporter()
	conf := MetricConf{Path: "test", Labels: []string{"k"}}

	t.Run("info_returns_empty", func(t *testing.T) {
		if r.Info() != "" {
			t.Error("expected empty string")
		}
	})

	t.Run("counter_does_not_panic", func(t *testing.T) {
		r.Counter(conf).Count(1, map[string]string{"k": "v"})
	})

	t.Run("gauge_count_does_not_panic", func(t *testing.T) {
		r.Gauge(conf).Count(1, map[string]string{"k": "v"})
	})

	t.Run("gauge_set_does_not_panic", func(t *testing.T) {
		r.Gauge(conf).Set(1, map[string]string{"k": "v"})
	})

	t.Run("observer_does_not_panic", func(t *testing.T) {
		r.Observer(conf).Observe(1, map[string]string{"k": "v"})
	})

	t.Run("gauge_func_does_not_panic", func(t *testing.T) {
		gf := r.GaugeFunc(conf, func() float64 { return 1.0 })
		if gf == nil {
			t.Fatal("expected non-nil GaugeFunc")
		}
	})

	t.Run("unregister_does_not_panic", func(t *testing.T) {
		r.UnRegister("anything")
	})

	t.Run("counter_unregister", func(t *testing.T) {
		r.Counter(conf).UnRegister()
	})

	t.Run("gauge_unregister", func(t *testing.T) {
		r.Gauge(conf).UnRegister()
	})

	t.Run("observer_unregister", func(t *testing.T) {
		r.Observer(conf).UnRegister()
	})

	t.Run("gauge_func_unregister", func(t *testing.T) {
		r.GaugeFunc(conf, func() float64 { return 0 }).UnRegister()
	})

	t.Run("sub_reporter_returns_noop", func(t *testing.T) {
		sub := r.Reporter(ReporterConf{Subsystem: "child"})
		sub.Counter(conf).Count(1, map[string]string{"k": "v"})
	})

	t.Run("nil_labels", func(t *testing.T) {
		r.Counter(conf).Count(1, nil)
	})

	t.Run("with_context_option", func(t *testing.T) {
		r.Counter(conf).Count(1, map[string]string{"k": "v"}, WithContext(context.Background()))
	})
}

// ─── 3. OTEL recording tests ───────────────────────────────────────────────

func TestOTELCounterCount(t *testing.T) {
	t.Run("single_count", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		c := r.Counter(MetricConf{Path: "c1"})
		c.Count(5, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELSumValue(t, rm, "t_c1"); v != 5.0 {
			t.Errorf("got %v, want 5", v)
		}
	})

	t.Run("accumulates", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		c := r.Counter(MetricConf{Path: "c2"})
		c.Count(3, map[string]string{})
		c.Count(2, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELSumValue(t, rm, "t_c2"); v != 5.0 {
			t.Errorf("got %v, want 5", v)
		}
	})

	t.Run("with_labels", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		c := r.Counter(MetricConf{Path: "c3", Labels: []string{"method"}})
		c.Count(1, map[string]string{"method": "GET"})
		c.Count(2, map[string]string{"method": "POST"})
		rm := collectOTEL(t, reader)
		if v := findOTELSumValueWithAttrs(t, rm, "t_c3", map[string]string{"method": "GET"}); v != 1.0 {
			t.Errorf("GET got %v, want 1", v)
		}
		if v := findOTELSumValueWithAttrs(t, rm, "t_c3", map[string]string{"method": "POST"}); v != 2.0 {
			t.Errorf("POST got %v, want 2", v)
		}
	})

	t.Run("with_const_labels", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t", ConstLabels: map[string]string{"env": "prod"}})
		c := r.Counter(MetricConf{Path: "c4", Labels: []string{"method"}})
		c.Count(1, map[string]string{"method": "GET"})
		rm := collectOTEL(t, reader)
		if v := findOTELSumValueWithAttrs(t, rm, "t_c4", map[string]string{"env": "prod", "method": "GET"}); v != 1.0 {
			t.Errorf("got %v, want 1", v)
		}
	})
}

func TestOTELGaugeCount(t *testing.T) {
	t.Run("single_increment", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		g := r.Gauge(MetricConf{Path: "gc1"})
		g.Count(3, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELGaugeValue(t, rm, "t_gc1"); v != 3.0 {
			t.Errorf("got %v, want 3", v)
		}
	})

	t.Run("accumulates_positive", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		g := r.Gauge(MetricConf{Path: "gc2"})
		g.Count(3, map[string]string{})
		g.Count(2, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELGaugeValue(t, rm, "t_gc2"); v != 5.0 {
			t.Errorf("got %v, want 5", v)
		}
	})

	t.Run("accumulates_negative", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		g := r.Gauge(MetricConf{Path: "gc3"})
		g.Count(10, map[string]string{})
		g.Count(-3, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELGaugeValue(t, rm, "t_gc3"); v != 7.0 {
			t.Errorf("got %v, want 7", v)
		}
	})

	t.Run("separate_label_sets", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		g := r.Gauge(MetricConf{Path: "gc4", Labels: []string{"k"}})
		g.Count(5, map[string]string{"k": "a"})
		g.Count(10, map[string]string{"k": "b"})
		rm := collectOTEL(t, reader)
		if v := findOTELGaugeValueWithAttrs(t, rm, "t_gc4", map[string]string{"k": "a"}); v != 5.0 {
			t.Errorf("k=a got %v, want 5", v)
		}
		if v := findOTELGaugeValueWithAttrs(t, rm, "t_gc4", map[string]string{"k": "b"}); v != 10.0 {
			t.Errorf("k=b got %v, want 10", v)
		}
	})
}

func TestOTELGaugeSet(t *testing.T) {
	t.Run("set_value", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		g := r.Gauge(MetricConf{Path: "gs1"})
		g.Set(42, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELGaugeValue(t, rm, "t_gs1"); v != 42.0 {
			t.Errorf("got %v, want 42", v)
		}
	})

	t.Run("set_overwrites", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		g := r.Gauge(MetricConf{Path: "gs2"})
		g.Set(10, map[string]string{})
		g.Set(20, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELGaugeValue(t, rm, "t_gs2"); v != 20.0 {
			t.Errorf("got %v, want 20", v)
		}
	})

	t.Run("set_after_count", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		g := r.Gauge(MetricConf{Path: "gs3"})
		g.Count(5, map[string]string{})
		g.Set(100, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELGaugeValue(t, rm, "t_gs3"); v != 100.0 {
			t.Errorf("got %v, want 100", v)
		}
	})

	t.Run("count_after_set", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		g := r.Gauge(MetricConf{Path: "gs4"})
		g.Set(100, map[string]string{})
		g.Count(5, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELGaugeValue(t, rm, "t_gs4"); v != 105.0 {
			t.Errorf("got %v, want 105", v)
		}
	})
}

func TestOTELObserver(t *testing.T) {
	t.Run("single_observe", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		o := r.Observer(MetricConf{Path: "obs1"})
		o.Observe(0.5, map[string]string{})
		rm := collectOTEL(t, reader)
		count, sum := findOTELHistogramCount(t, rm, "t_obs1")
		if count != 1 {
			t.Errorf("count = %d, want 1", count)
		}
		if sum != 0.5 {
			t.Errorf("sum = %v, want 0.5", sum)
		}
	})

	t.Run("multiple_observations", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		o := r.Observer(MetricConf{Path: "obs2"})
		o.Observe(0.1, map[string]string{})
		o.Observe(0.2, map[string]string{})
		o.Observe(0.3, map[string]string{})
		rm := collectOTEL(t, reader)
		count, sum := findOTELHistogramCount(t, rm, "t_obs2")
		if count != 3 {
			t.Errorf("count = %d, want 3", count)
		}
		if math.Abs(sum-0.6) > 0.001 {
			t.Errorf("sum = %v, want ~0.6", sum)
		}
	})

	t.Run("with_labels", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		o := r.Observer(MetricConf{Path: "obs3", Labels: []string{"endpoint"}})
		o.Observe(1.0, map[string]string{"endpoint": "/api"})
		rm := collectOTEL(t, reader)
		count, _ := findOTELHistogramCountWithAttrs(t, rm, "t_obs3", map[string]string{"endpoint": "/api"})
		if count != 1 {
			t.Errorf("count = %d, want 1", count)
		}
	})

	t.Run("with_const_labels", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t", ConstLabels: map[string]string{"env": "staging"}})
		o := r.Observer(MetricConf{Path: "obs4", Labels: []string{"op"}})
		o.Observe(1.0, map[string]string{"op": "read"})
		rm := collectOTEL(t, reader)
		count, _ := findOTELHistogramCountWithAttrs(t, rm, "t_obs4", map[string]string{"env": "staging", "op": "read"})
		if count != 1 {
			t.Errorf("count = %d, want 1", count)
		}
	})
}

func TestOTELGaugeFunc(t *testing.T) {
	t.Run("callback_invoked", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		r.GaugeFunc(MetricConf{Path: "gf1"}, func() float64 { return 42.0 })
		rm := collectOTEL(t, reader)
		if v := findOTELGaugeValue(t, rm, "t_gf1"); v != 42.0 {
			t.Errorf("got %v, want 42", v)
		}
	})

	t.Run("callback_returns_dynamic_value", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		var val atomic.Int64
		r.GaugeFunc(MetricConf{Path: "gf2"}, func() float64 { return float64(val.Load()) })

		rm := collectOTEL(t, reader)
		if v := findOTELGaugeValue(t, rm, "t_gf2"); v != 0.0 {
			t.Errorf("initial got %v, want 0", v)
		}

		val.Store(10)
		rm = collectOTEL(t, reader)
		if v := findOTELGaugeValue(t, rm, "t_gf2"); v != 10.0 {
			t.Errorf("after update got %v, want 10", v)
		}
	})

	t.Run("with_const_labels", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t", ConstLabels: map[string]string{"env": "test"}})
		r.GaugeFunc(MetricConf{Path: "gf3"}, func() float64 { return 1.0 })
		rm := collectOTEL(t, reader)
		if v := findOTELGaugeValueWithAttrs(t, rm, "t_gf3", map[string]string{"env": "test"}); v != 1.0 {
			t.Errorf("got %v, want 1", v)
		}
	})
}

// ─── 4. OTEL structural tests ──────────────────────────────────────────────

func TestOTELMetricCaching(t *testing.T) {
	t.Run("counter_cached", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		c1 := r.Counter(MetricConf{Path: "cache_c"})
		c2 := r.Counter(MetricConf{Path: "cache_c"})
		c1.Count(1, map[string]string{})
		c2.Count(2, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELSumValue(t, rm, "t_cache_c"); v != 3.0 {
			t.Errorf("got %v, want 3 (proves same instance)", v)
		}
	})

	t.Run("gauge_cached", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		g1 := r.Gauge(MetricConf{Path: "cache_g"})
		g2 := r.Gauge(MetricConf{Path: "cache_g"})
		g1.Set(5, map[string]string{})
		g2.Set(10, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELGaugeValue(t, rm, "t_cache_g"); v != 10.0 {
			t.Errorf("got %v, want 10", v)
		}
	})

	t.Run("observer_cached", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t"})
		o1 := r.Observer(MetricConf{Path: "cache_o"})
		o2 := r.Observer(MetricConf{Path: "cache_o"})
		o1.Observe(1, map[string]string{})
		o2.Observe(2, map[string]string{})
		rm := collectOTEL(t, reader)
		count, _ := findOTELHistogramCount(t, rm, "t_cache_o")
		if count != 2 {
			t.Errorf("count = %d, want 2", count)
		}
	})
}

func TestOTELUnRegister(t *testing.T) {
	t.Run("removes_from_cache", func(t *testing.T) {
		r, _, _ := newOTELReporter(t, ReporterConf{System: "t"})
		c1 := r.Counter(MetricConf{Path: "unreg"})
		r.UnRegister("unreg")
		c2 := r.Counter(MetricConf{Path: "unreg"})
		// After unregister + re-create, c2 should be a different wrapper
		// (though OTEL SDK may reuse underlying instrument)
		c1.Count(1, map[string]string{})
		c2.Count(1, map[string]string{})
		// Should not panic — main point is cache removal works
	})

	t.Run("noop_for_unknown_path", func(t *testing.T) {
		r, _, _ := newOTELReporter(t, ReporterConf{System: "t"})
		r.UnRegister("nonexistent") // should not panic
	})
}

func TestOTELInfo(t *testing.T) {
	r, _, _ := newOTELReporter(t, ReporterConf{})
	if r.Info() != "otel" {
		t.Errorf("got %q, want %q", r.Info(), "otel")
	}
}

func TestOTELConstLabels(t *testing.T) {
	t.Run("appear_on_counter", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t", ConstLabels: map[string]string{"env": "prod"}})
		r.Counter(MetricConf{Path: "cl_c"}).Count(1, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELSumValueWithAttrs(t, rm, "t_cl_c", map[string]string{"env": "prod"}); v != 1.0 {
			t.Errorf("got %v, want 1", v)
		}
	})

	t.Run("appear_on_gauge", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t", ConstLabels: map[string]string{"env": "prod"}})
		r.Gauge(MetricConf{Path: "cl_g"}).Set(1, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELGaugeValueWithAttrs(t, rm, "t_cl_g", map[string]string{"env": "prod"}); v != 1.0 {
			t.Errorf("got %v, want 1", v)
		}
	})

	t.Run("inherited_by_sub_reporter", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t", ConstLabels: map[string]string{"env": "prod"}})
		child := r.Reporter(ReporterConf{ConstLabels: map[string]string{"region": "us"}})
		child.Counter(MetricConf{Path: "cl_inh"}).Count(1, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELSumValueWithAttrs(t, rm, "t_cl_inh", map[string]string{"env": "prod", "region": "us"}); v != 1.0 {
			t.Errorf("got %v, want 1", v)
		}
	})

	t.Run("duplicate_const_labels_panic", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on duplicate const label")
			}
		}()
		r, _, _ := newOTELReporter(t, ReporterConf{System: "t", ConstLabels: map[string]string{"env": "prod"}})
		r.Reporter(ReporterConf{ConstLabels: map[string]string{"env": "staging"}})
	})
}

func TestOTELSubReporterConstLabelInheritance(t *testing.T) {
	t.Run("three_level_hierarchy", func(t *testing.T) {
		r, reader, _ := newOTELReporter(t, ReporterConf{System: "t", ConstLabels: map[string]string{"a": "1"}})
		child := r.Reporter(ReporterConf{ConstLabels: map[string]string{"b": "2"}})
		grandchild := child.Reporter(ReporterConf{ConstLabels: map[string]string{"c": "3"}})
		grandchild.Counter(MetricConf{Path: "deep"}).Count(1, map[string]string{})
		rm := collectOTEL(t, reader)
		if v := findOTELSumValueWithAttrs(t, rm, "t_deep", map[string]string{"a": "1", "b": "2", "c": "3"}); v != 1.0 {
			t.Errorf("got %v, want 1", v)
		}
	})

	t.Run("sub_reporter_meter_name", func(t *testing.T) {
		reader := sdkmetric.NewManualReader()
		provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		defer provider.Shutdown(context.Background())

		parent := OTELReporter(ReporterConf{System: "sys", Subsystem: "sub"}, WithMeterProvider(provider))
		child := parent.Reporter(ReporterConf{Subsystem: "child"})
		child.Gauge(MetricConf{Path: "m"}).Set(1, map[string]string{})

		rm := collectOTEL(t, reader)
		got := findMeterName(rm)
		if got != "sys_sub_child" {
			t.Errorf("meter name = %q, want %q", got, "sys_sub_child")
		}
	})
}

// ─── 5. Prometheus recording tests ─────────────────────────────────────────

func TestPrometheusCounterCount(t *testing.T) {
	t.Run("single_count", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pcc"})
		c := r.Counter(MetricConf{Path: "single"})
		c.Count(5, map[string]string{})
		if v := findPromCounterValue(t, "pcc_single"); v != 5.0 {
			t.Errorf("got %v, want 5", v)
		}
		r.UnRegister("single")
	})

	t.Run("accumulates", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pcc"})
		c := r.Counter(MetricConf{Path: "accum"})
		c.Count(3, map[string]string{})
		c.Count(2, map[string]string{})
		if v := findPromCounterValue(t, "pcc_accum"); v != 5.0 {
			t.Errorf("got %v, want 5", v)
		}
		r.UnRegister("accum")
	})

	t.Run("with_labels", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pcc"})
		c := r.Counter(MetricConf{Path: "lbl", Labels: []string{"method"}})
		c.Count(1, map[string]string{"method": "GET"})
		c.Count(2, map[string]string{"method": "POST"})
		if v := findPromCounterValueWithLabels(t, "pcc_lbl", map[string]string{"method": "GET"}); v != 1.0 {
			t.Errorf("GET got %v, want 1", v)
		}
		if v := findPromCounterValueWithLabels(t, "pcc_lbl", map[string]string{"method": "POST"}); v != 2.0 {
			t.Errorf("POST got %v, want 2", v)
		}
		r.UnRegister("lbl")
	})

	t.Run("with_const_labels", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pcc", ConstLabels: map[string]string{"env": "prod"}})
		c := r.Counter(MetricConf{Path: "cl"})
		c.Count(1, map[string]string{})
		if v := findPromCounterValueWithLabels(t, "pcc_cl", map[string]string{"env": "prod"}); v != 1.0 {
			t.Errorf("got %v, want 1", v)
		}
		r.UnRegister("cl")
	})
}

func TestPrometheusGaugeCount(t *testing.T) {
	t.Run("single_increment", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pgc"})
		g := r.Gauge(MetricConf{Path: "single"})
		g.Count(3, map[string]string{})
		if v := findPromGaugeValue(t, "pgc_single"); v != 3.0 {
			t.Errorf("got %v, want 3", v)
		}
		r.UnRegister("single")
	})

	t.Run("accumulates", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pgc"})
		g := r.Gauge(MetricConf{Path: "accum"})
		g.Count(3, map[string]string{})
		g.Count(2, map[string]string{})
		if v := findPromGaugeValue(t, "pgc_accum"); v != 5.0 {
			t.Errorf("got %v, want 5", v)
		}
		r.UnRegister("accum")
	})

	t.Run("negative_delta", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pgc"})
		g := r.Gauge(MetricConf{Path: "neg"})
		g.Count(10, map[string]string{})
		g.Count(-3, map[string]string{})
		if v := findPromGaugeValue(t, "pgc_neg"); v != 7.0 {
			t.Errorf("got %v, want 7", v)
		}
		r.UnRegister("neg")
	})
}

func TestPrometheusGaugeSet(t *testing.T) {
	t.Run("set_value", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pgs"})
		g := r.Gauge(MetricConf{Path: "val"})
		g.Set(42, map[string]string{})
		if v := findPromGaugeValue(t, "pgs_val"); v != 42.0 {
			t.Errorf("got %v, want 42", v)
		}
		r.UnRegister("val")
	})

	t.Run("set_overwrites", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pgs"})
		g := r.Gauge(MetricConf{Path: "ow"})
		g.Set(10, map[string]string{})
		g.Set(20, map[string]string{})
		if v := findPromGaugeValue(t, "pgs_ow"); v != 20.0 {
			t.Errorf("got %v, want 20", v)
		}
		r.UnRegister("ow")
	})

	t.Run("set_after_count", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pgs"})
		g := r.Gauge(MetricConf{Path: "sac"})
		g.Count(5, map[string]string{})
		g.Set(100, map[string]string{})
		if v := findPromGaugeValue(t, "pgs_sac"); v != 100.0 {
			t.Errorf("got %v, want 100", v)
		}
		r.UnRegister("sac")
	})

	t.Run("count_after_set", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pgs"})
		g := r.Gauge(MetricConf{Path: "cas"})
		g.Set(100, map[string]string{})
		g.Count(5, map[string]string{})
		if v := findPromGaugeValue(t, "pgs_cas"); v != 105.0 {
			t.Errorf("got %v, want 105", v)
		}
		r.UnRegister("cas")
	})
}

func TestPrometheusObserver(t *testing.T) {
	t.Run("single_observe", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "po"})
		o := r.Observer(MetricConf{Path: "single"})
		o.Observe(0.5, map[string]string{})
		count, sum := findPromSummaryCount(t, "po_single")
		if count != 1 {
			t.Errorf("count = %d, want 1", count)
		}
		if sum != 0.5 {
			t.Errorf("sum = %v, want 0.5", sum)
		}
		r.UnRegister("single")
	})

	t.Run("multiple_observations", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "po"})
		o := r.Observer(MetricConf{Path: "multi"})
		o.Observe(0.1, map[string]string{})
		o.Observe(0.2, map[string]string{})
		o.Observe(0.3, map[string]string{})
		count, sum := findPromSummaryCount(t, "po_multi")
		if count != 3 {
			t.Errorf("count = %d, want 3", count)
		}
		if math.Abs(sum-0.6) > 0.001 {
			t.Errorf("sum = %v, want ~0.6", sum)
		}
		r.UnRegister("multi")
	})

	t.Run("with_labels", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "po"})
		o := r.Observer(MetricConf{Path: "lbl", Labels: []string{"endpoint"}})
		o.Observe(1.0, map[string]string{"endpoint": "/api"})
		count, _ := findPromSummaryCountWithLabels(t, "po_lbl", map[string]string{"endpoint": "/api"})
		if count != 1 {
			t.Errorf("count = %d, want 1", count)
		}
		r.UnRegister("lbl")
	})
}

func TestPrometheusGaugeFunc(t *testing.T) {
	t.Run("callback_invoked", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pgf"})
		r.GaugeFunc(MetricConf{Path: "cb"}, func() float64 { return 99.0 })
		if v := findPromGaugeValue(t, "pgf_cb"); v != 99.0 {
			t.Errorf("got %v, want 99", v)
		}
		r.UnRegister("cb")
	})

	t.Run("dynamic_value", func(t *testing.T) {
		var val atomic.Int64
		val.Store(10)
		r := PrometheusReporter(ReporterConf{System: "pgf"})
		r.GaugeFunc(MetricConf{Path: "dyn"}, func() float64 { return float64(val.Load()) })
		if v := findPromGaugeValue(t, "pgf_dyn"); v != 10.0 {
			t.Errorf("got %v, want 10", v)
		}
		r.UnRegister("dyn")
	})

	t.Run("with_const_labels", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pgf", ConstLabels: map[string]string{"env": "test"}})
		r.GaugeFunc(MetricConf{Path: "cl"}, func() float64 { return 1.0 })
		if v := findPromGaugeValueWithLabels(t, "pgf_cl", map[string]string{"env": "test"}); v != 1.0 {
			t.Errorf("got %v, want 1", v)
		}
		r.UnRegister("cl")
	})
}

// ─── 6. Prometheus structural tests ─────────────────────────────────────────

func TestPrometheusMetricCaching(t *testing.T) {
	t.Run("counter_cached", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pcache"})
		c1 := r.Counter(MetricConf{Path: "c"})
		c2 := r.Counter(MetricConf{Path: "c"})
		c1.Count(1, map[string]string{})
		c2.Count(2, map[string]string{})
		if v := findPromCounterValue(t, "pcache_c"); v != 3.0 {
			t.Errorf("got %v, want 3", v)
		}
		r.UnRegister("c")
	})

	t.Run("gauge_cached", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pcache"})
		g1 := r.Gauge(MetricConf{Path: "g"})
		g2 := r.Gauge(MetricConf{Path: "g"})
		g1.Set(5, map[string]string{})
		g2.Set(10, map[string]string{})
		if v := findPromGaugeValue(t, "pcache_g"); v != 10.0 {
			t.Errorf("got %v, want 10", v)
		}
		r.UnRegister("g")
	})

	t.Run("observer_cached", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pcache"})
		o1 := r.Observer(MetricConf{Path: "o"})
		o2 := r.Observer(MetricConf{Path: "o"})
		o1.Observe(1, map[string]string{})
		o2.Observe(2, map[string]string{})
		count, _ := findPromSummaryCount(t, "pcache_o")
		if count != 2 {
			t.Errorf("count = %d, want 2", count)
		}
		r.UnRegister("o")
	})
}

func TestPrometheusAlreadyRegistered(t *testing.T) {
	t.Run("counter_returns_existing", func(t *testing.T) {
		r1 := PrometheusReporter(ReporterConf{System: "par"})
		r2 := PrometheusReporter(ReporterConf{System: "par"})
		c1 := r1.Counter(MetricConf{Path: "c"})
		c2 := r2.Counter(MetricConf{Path: "c"})
		c1.Count(1, map[string]string{})
		c2.Count(2, map[string]string{})
		if v := findPromCounterValue(t, "par_c"); v != 3.0 {
			t.Errorf("got %v, want 3", v)
		}
		r1.UnRegister("c")
	})

	t.Run("gauge_returns_existing", func(t *testing.T) {
		r1 := PrometheusReporter(ReporterConf{System: "par"})
		r2 := PrometheusReporter(ReporterConf{System: "par"})
		r1.Gauge(MetricConf{Path: "g"}).Set(5, map[string]string{})
		r2.Gauge(MetricConf{Path: "g"}).Set(10, map[string]string{})
		if v := findPromGaugeValue(t, "par_g"); v != 10.0 {
			t.Errorf("got %v, want 10", v)
		}
		r1.UnRegister("g")
	})

	t.Run("observer_returns_existing", func(t *testing.T) {
		r1 := PrometheusReporter(ReporterConf{System: "par"})
		r2 := PrometheusReporter(ReporterConf{System: "par"})
		r1.Observer(MetricConf{Path: "o"}).Observe(1, map[string]string{})
		r2.Observer(MetricConf{Path: "o"}).Observe(2, map[string]string{})
		count, _ := findPromSummaryCount(t, "par_o")
		if count != 2 {
			t.Errorf("count = %d, want 2", count)
		}
		r1.UnRegister("o")
	})
}

func TestPrometheusUnRegister(t *testing.T) {
	t.Run("removes_metric", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pur"})
		r.Gauge(MetricConf{Path: "rm"}).Set(1, map[string]string{})
		assertPromMetricExists(t, "pur_rm")
		r.UnRegister("rm")
		assertPromMetricNotExists(t, "pur_rm")
	})

	t.Run("re_register_after_unregister", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pur"})
		r.Counter(MetricConf{Path: "rereg"}).Count(5, map[string]string{})
		r.UnRegister("rereg")
		r.Counter(MetricConf{Path: "rereg"}).Count(1, map[string]string{})
		if v := findPromCounterValue(t, "pur_rereg"); v != 1.0 {
			t.Errorf("got %v, want 1 (fresh registration)", v)
		}
		r.UnRegister("rereg")
	})

	t.Run("noop_for_unknown_path", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pur"})
		r.UnRegister("nonexistent") // should not panic
	})

	t.Run("collector_unregister", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pur"})
		g := r.Gauge(MetricConf{Path: "coll_unreg"})
		g.Set(1, map[string]string{})
		assertPromMetricExists(t, "pur_coll_unreg")
		g.UnRegister()
		assertPromMetricNotExists(t, "pur_coll_unreg")
	})
}

func TestPrometheusInfo(t *testing.T) {
	r := PrometheusReporter(ReporterConf{})
	if r.Info() != "" {
		t.Errorf("got %q, want empty string", r.Info())
	}
}

func TestPrometheusConstLabels(t *testing.T) {
	t.Run("appear_on_counter", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pcl", ConstLabels: map[string]string{"env": "prod"}})
		r.Counter(MetricConf{Path: "c"}).Count(1, map[string]string{})
		if v := findPromCounterValueWithLabels(t, "pcl_c", map[string]string{"env": "prod"}); v != 1.0 {
			t.Errorf("got %v, want 1", v)
		}
		r.UnRegister("c")
	})

	t.Run("appear_on_gauge", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pcl", ConstLabels: map[string]string{"env": "prod"}})
		r.Gauge(MetricConf{Path: "g"}).Set(1, map[string]string{})
		if v := findPromGaugeValueWithLabels(t, "pcl_g", map[string]string{"env": "prod"}); v != 1.0 {
			t.Errorf("got %v, want 1", v)
		}
		r.UnRegister("g")
	})

	t.Run("inherited_by_sub_reporter", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pcl", ConstLabels: map[string]string{"env": "prod"}})
		child := r.Reporter(ReporterConf{ConstLabels: map[string]string{"region": "us"}})
		child.Counter(MetricConf{Path: "inh"}).Count(1, map[string]string{})
		if v := findPromCounterValueWithLabels(t, "pcl_inh", map[string]string{"env": "prod", "region": "us"}); v != 1.0 {
			t.Errorf("got %v, want 1", v)
		}
		child.UnRegister("inh")
	})

	t.Run("duplicate_const_labels_panic", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on duplicate const label")
			}
		}()
		r := PrometheusReporter(ReporterConf{System: "pcl", ConstLabels: map[string]string{"env": "prod"}})
		r.Reporter(ReporterConf{ConstLabels: map[string]string{"env": "staging"}})
	})
}

func TestPrometheusSubReporterConstLabelInheritance(t *testing.T) {
	t.Run("three_level_hierarchy", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "pscl", ConstLabels: map[string]string{"a": "1"}})
		child := r.Reporter(ReporterConf{ConstLabels: map[string]string{"b": "2"}})
		grandchild := child.Reporter(ReporterConf{ConstLabels: map[string]string{"c": "3"}})
		grandchild.Counter(MetricConf{Path: "deep"}).Count(1, map[string]string{})
		if v := findPromCounterValueWithLabels(t, "pscl_deep", map[string]string{"a": "1", "b": "2", "c": "3"}); v != 1.0 {
			t.Errorf("got %v, want 1", v)
		}
		grandchild.UnRegister("deep")
	})
}

// ─── 7. Prometheus-specific methods (not on Reporter interface) ─────────────

type summaryReporter interface {
	Summary(MetricConf) Observer
}

type histogramReporter interface {
	Histogram(MetricConf) Observer
}

func TestPrometheusSummary(t *testing.T) {
	t.Run("records_observations", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "psum"})
		sr := r.(summaryReporter)
		o := sr.Summary(MetricConf{Path: "s"})
		o.Observe(0.1, map[string]string{})
		o.Observe(0.2, map[string]string{})
		o.Observe(0.3, map[string]string{})
		count, sum := findPromSummaryCount(t, "psum_s")
		if count != 3 {
			t.Errorf("count = %d, want 3", count)
		}
		if math.Abs(sum-0.6) > 0.001 {
			t.Errorf("sum = %v, want ~0.6", sum)
		}
		r.UnRegister("s")
	})

	t.Run("caches_by_path", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "psum"})
		sr := r.(summaryReporter)
		o1 := sr.Summary(MetricConf{Path: "cache"})
		o2 := sr.Summary(MetricConf{Path: "cache"})
		o1.Observe(1, map[string]string{})
		o2.Observe(2, map[string]string{})
		count, _ := findPromSummaryCount(t, "psum_cache")
		if count != 2 {
			t.Errorf("count = %d, want 2", count)
		}
		r.UnRegister("cache")
	})
}

func TestPrometheusHistogram(t *testing.T) {
	t.Run("records_observations", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "phist"})
		hr := r.(histogramReporter)
		o := hr.Histogram(MetricConf{Path: "h"})
		o.Observe(0.1, map[string]string{})
		o.Observe(0.5, map[string]string{})
		o.Observe(1.0, map[string]string{})
		count, sum := findPromHistogramCount(t, "phist_h")
		if count != 3 {
			t.Errorf("count = %d, want 3", count)
		}
		if math.Abs(sum-1.6) > 0.001 {
			t.Errorf("sum = %v, want ~1.6", sum)
		}
		r.UnRegister("h")
	})

	t.Run("caches_by_path", func(t *testing.T) {
		r := PrometheusReporter(ReporterConf{System: "phist"})
		hr := r.(histogramReporter)
		o1 := hr.Histogram(MetricConf{Path: "cache"})
		o2 := hr.Histogram(MetricConf{Path: "cache"})
		o1.Observe(1, map[string]string{})
		o2.Observe(2, map[string]string{})
		count, _ := findPromHistogramCount(t, "phist_cache")
		if count != 2 {
			t.Errorf("count = %d, want 2", count)
		}
		r.UnRegister("cache")
	})
}

// ─── Helper functions ───────────────────────────────────────────────────────

func newOTELReporter(t *testing.T, conf ReporterConf) (Reporter, *sdkmetric.ManualReader, *sdkmetric.MeterProvider) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { provider.Shutdown(context.Background()) })
	return OTELReporter(conf, WithMeterProvider(provider)), reader, provider
}

func collectOTEL(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("failed to collect: %v", err)
	}
	return rm
}

func findMeterName(rm metricdata.ResourceMetrics) string {
	for _, sm := range rm.ScopeMetrics {
		return sm.Scope.Name
	}
	return ""
}

func findMetricName(rm metricdata.ResourceMetrics) string {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			return m.Name
		}
	}
	return ""
}

// OTEL finders — no label filtering (single data point expected)

func findOTELSumValue(t *testing.T, rm metricdata.ResourceMetrics, name string) float64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				if s, ok := m.Data.(metricdata.Sum[float64]); ok {
					if len(s.DataPoints) > 0 {
						return s.DataPoints[0].Value
					}
				}
			}
		}
	}
	t.Fatalf("OTEL sum metric %q not found", name)
	return 0
}

func findOTELGaugeValue(t *testing.T, rm metricdata.ResourceMetrics, name string) float64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				if g, ok := m.Data.(metricdata.Gauge[float64]); ok {
					if len(g.DataPoints) > 0 {
						return g.DataPoints[0].Value
					}
				}
			}
		}
	}
	t.Fatalf("OTEL gauge metric %q not found", name)
	return 0
}

func findOTELHistogramCount(t *testing.T, rm metricdata.ResourceMetrics, name string) (uint64, float64) {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
					if len(h.DataPoints) > 0 {
						return h.DataPoints[0].Count, h.DataPoints[0].Sum
					}
				}
			}
		}
	}
	t.Fatalf("OTEL histogram metric %q not found", name)
	return 0, 0
}

// OTEL finders — with attribute filtering

func attrsMatch(dp attribute.Set, want map[string]string) bool {
	for k, v := range want {
		val, ok := dp.Value(attribute.Key(k))
		if !ok || val.AsString() != v {
			return false
		}
	}
	return true
}

func findOTELSumValueWithAttrs(t *testing.T, rm metricdata.ResourceMetrics, name string, wantAttrs map[string]string) float64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				if s, ok := m.Data.(metricdata.Sum[float64]); ok {
					for _, dp := range s.DataPoints {
						if attrsMatch(dp.Attributes, wantAttrs) {
							return dp.Value
						}
					}
				}
			}
		}
	}
	t.Fatalf("OTEL sum metric %q with attrs %v not found", name, wantAttrs)
	return 0
}

func findOTELGaugeValueWithAttrs(t *testing.T, rm metricdata.ResourceMetrics, name string, wantAttrs map[string]string) float64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				if g, ok := m.Data.(metricdata.Gauge[float64]); ok {
					for _, dp := range g.DataPoints {
						if attrsMatch(dp.Attributes, wantAttrs) {
							return dp.Value
						}
					}
				}
			}
		}
	}
	t.Fatalf("OTEL gauge metric %q with attrs %v not found", name, wantAttrs)
	return 0
}

func findOTELHistogramCountWithAttrs(t *testing.T, rm metricdata.ResourceMetrics, name string, wantAttrs map[string]string) (uint64, float64) {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
					for _, dp := range h.DataPoints {
						if attrsMatch(dp.Attributes, wantAttrs) {
							return dp.Count, dp.Sum
						}
					}
				}
			}
		}
	}
	t.Fatalf("OTEL histogram metric %q with attrs %v not found", name, wantAttrs)
	return 0, 0
}

// Prometheus finders

func gatherPromFamilies(t *testing.T) []*dto.MetricFamily {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather: %v", err)
	}
	return families
}

func findPromFamily(t *testing.T, name string) *dto.MetricFamily {
	t.Helper()
	for _, f := range gatherPromFamilies(t) {
		if f.GetName() == name {
			return f
		}
	}
	t.Fatalf("prometheus metric family %q not found", name)
	return nil
}

func promLabelsMatch(metric *dto.Metric, want map[string]string) bool {
	labels := map[string]string{}
	for _, lp := range metric.GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func assertPromMetricExists(t *testing.T, wantName string) {
	t.Helper()
	for _, f := range gatherPromFamilies(t) {
		if f.GetName() == wantName {
			return
		}
	}
	t.Errorf("metric %q not found", wantName)
}

func assertPromMetricNotExists(t *testing.T, wantName string) {
	t.Helper()
	for _, f := range gatherPromFamilies(t) {
		if f.GetName() == wantName {
			t.Errorf("metric %q should not exist but was found", wantName)
			return
		}
	}
}

func findPromCounterValue(t *testing.T, name string) float64 {
	t.Helper()
	f := findPromFamily(t, name)
	if len(f.GetMetric()) == 0 {
		t.Fatalf("no metrics in family %q", name)
	}
	return f.GetMetric()[0].GetCounter().GetValue()
}

func findPromCounterValueWithLabels(t *testing.T, name string, want map[string]string) float64 {
	t.Helper()
	f := findPromFamily(t, name)
	for _, m := range f.GetMetric() {
		if promLabelsMatch(m, want) {
			return m.GetCounter().GetValue()
		}
	}
	t.Fatalf("counter %q with labels %v not found", name, want)
	return 0
}

func findPromGaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	f := findPromFamily(t, name)
	if len(f.GetMetric()) == 0 {
		t.Fatalf("no metrics in family %q", name)
	}
	return f.GetMetric()[0].GetGauge().GetValue()
}

func findPromGaugeValueWithLabels(t *testing.T, name string, want map[string]string) float64 {
	t.Helper()
	f := findPromFamily(t, name)
	for _, m := range f.GetMetric() {
		if promLabelsMatch(m, want) {
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("gauge %q with labels %v not found", name, want)
	return 0
}

func findPromSummaryCount(t *testing.T, name string) (uint64, float64) {
	t.Helper()
	f := findPromFamily(t, name)
	if len(f.GetMetric()) == 0 {
		t.Fatalf("no metrics in family %q", name)
	}
	s := f.GetMetric()[0].GetSummary()
	return s.GetSampleCount(), s.GetSampleSum()
}

func findPromSummaryCountWithLabels(t *testing.T, name string, want map[string]string) (uint64, float64) {
	t.Helper()
	f := findPromFamily(t, name)
	for _, m := range f.GetMetric() {
		if promLabelsMatch(m, want) {
			s := m.GetSummary()
			return s.GetSampleCount(), s.GetSampleSum()
		}
	}
	t.Fatalf("summary %q with labels %v not found", name, want)
	return 0, 0
}

func findPromHistogramCount(t *testing.T, name string) (uint64, float64) {
	t.Helper()
	f := findPromFamily(t, name)
	if len(f.GetMetric()) == 0 {
		t.Fatalf("no metrics in family %q", name)
	}
	h := f.GetMetric()[0].GetHistogram()
	return h.GetSampleCount(), h.GetSampleSum()
}
