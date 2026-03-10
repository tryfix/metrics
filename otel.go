package metrics

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type otelReporter struct {
	meter            metric.Meter
	meterProvider    metric.MeterProvider
	namespace        string
	subSystem        string
	constLabels      map[string]string
	availableMetrics map[string]Collector
	*sync.Mutex
}

// OTELOption configures the OTEL reporter.
type OTELOption func(*otelOptions)

type otelOptions struct {
	meterProvider metric.MeterProvider
}

func applyOTELOptions(opts []OTELOption) otelOptions {
	o := otelOptions{meterProvider: otel.GetMeterProvider()}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// WithMeterProvider sets a custom MeterProvider for the reporter.
// By default, the global provider from otel.GetMeterProvider() is used.
func WithMeterProvider(provider metric.MeterProvider) OTELOption {
	return func(o *otelOptions) {
		o.meterProvider = provider
	}
}

// OTELReporter creates a new OpenTelemetry-based reporter.
// Use WithMeterProvider to inject a custom MeterProvider.
func OTELReporter(conf ReporterConf, opts ...OTELOption) Reporter {
	o := applyOTELOptions(opts)

	constLabels := map[string]string{}
	if conf.ConstLabels != nil {
		for k, v := range conf.ConstLabels {
			constLabels[k] = v
		}
	}

	meterName := conf.System
	if conf.System != "" && conf.Subsystem != "" {
		meterName = fmt.Sprintf("%s_%s", conf.System, conf.Subsystem)
	} else if conf.Subsystem != "" {
		meterName = conf.Subsystem
	}

	r := &otelReporter{
		meterProvider:    o.meterProvider,
		meter:            o.meterProvider.Meter(meterName),
		namespace:        conf.System,
		subSystem:        conf.Subsystem,
		constLabels:      constLabels,
		availableMetrics: make(map[string]Collector),
		Mutex:            new(sync.Mutex),
	}

	return r
}

func (r *otelReporter) Reporter(conf ReporterConf) Reporter {
	rConf := ReporterConf{
		System:      r.namespace,
		Subsystem:   r.subSystem,
		ConstLabels: mergeLabels(r.constLabels, conf.ConstLabels),
	}
	if conf.Subsystem != "" {
		rConf.Subsystem = fmt.Sprintf("%s_%s", r.subSystem, conf.Subsystem)
	}
	return OTELReporter(rConf, WithMeterProvider(r.meterProvider))
}

func (r *otelReporter) Counter(conf MetricConf) Counter {
	r.Lock()
	defer r.Unlock()

	if c, ok := r.availableMetrics[conf.Path]; ok {
		return c.(Counter)
	}

	name := r.metricName(conf.Path)
	counter, err := r.meter.Float64Counter(name,
		metric.WithDescription(conf.Help),
	)
	if err != nil {
		panic(err)
	}

	c := &otelCounter{
		counter:     counter,
		constLabels: mergeLabels(r.constLabels, conf.ConstLabels),
	}
	r.availableMetrics[conf.Path] = c
	return c
}

func (r *otelReporter) Gauge(conf MetricConf) Gauge {
	r.Lock()
	defer r.Unlock()

	if g, ok := r.availableMetrics[conf.Path]; ok {
		return g.(Gauge)
	}

	name := r.metricName(conf.Path)

	gauge, err := r.meter.Float64Gauge(name,
		metric.WithDescription(conf.Help),
	)
	if err != nil {
		panic(err)
	}

	g := &otelGauge{
		gauge:       gauge,
		constLabels: mergeLabels(r.constLabels, conf.ConstLabels),
		values:      make(map[string]float64),
		Mutex:       new(sync.Mutex),
	}
	r.availableMetrics[conf.Path] = g
	return g
}

func (r *otelReporter) GaugeFunc(conf MetricConf, f func() float64) GaugeFunc {
	r.Lock()
	defer r.Unlock()

	name := r.metricName(conf.Path)
	constAttrs := labelsToAttributes(mergeLabels(r.constLabels, conf.ConstLabels))

	_, err := r.meter.Float64ObservableGauge(name,
		metric.WithDescription(conf.Help),
		metric.WithFloat64Callback(func(ctx context.Context, obs metric.Float64Observer) error {
			obs.Observe(f(), metric.WithAttributes(constAttrs...))
			return nil
		}),
	)
	if err != nil {
		panic(err)
	}

	g := &otelGaugeFunc{}
	r.availableMetrics[conf.Path] = g
	return g
}

func (r *otelReporter) Observer(conf MetricConf) Observer {
	r.Lock()
	defer r.Unlock()

	if o, ok := r.availableMetrics[conf.Path]; ok {
		return o.(Observer)
	}

	name := r.metricName(conf.Path)
	histogram, err := r.meter.Float64Histogram(name,
		metric.WithDescription(conf.Help),
	)
	if err != nil {
		panic(err)
	}

	o := &otelHistogram{
		histogram:   histogram,
		constLabels: mergeLabels(r.constLabels, conf.ConstLabels),
	}
	r.availableMetrics[conf.Path] = o
	return o
}

func (r *otelReporter) Info() string { return "otel" }

func (r *otelReporter) UnRegister(metrics string) {
	r.Lock()
	defer r.Unlock()
	// OTEL doesn't support unregistering individual metrics
	delete(r.availableMetrics, metrics)
}

func (r *otelReporter) metricName(path string) string {
	if r.namespace != "" && r.subSystem != "" {
		return fmt.Sprintf("%s_%s_%s", r.namespace, r.subSystem, path)
	}
	if r.namespace != "" {
		return fmt.Sprintf("%s_%s", r.namespace, path)
	}
	if r.subSystem != "" {
		return fmt.Sprintf("%s_%s", r.subSystem, path)
	}
	return path
}

// --- Metric Implementations ---

type otelCounter struct {
	counter     metric.Float64Counter
	constLabels map[string]string
}

func (c *otelCounter) Count(value float64, lbs map[string]string, opts ...RecordOption) {
	o := applyRecordOptions(opts)
	attrs := labelsToAttributes(mergeLabels(c.constLabels, lbs))
	c.counter.Add(o.ctx, value, metric.WithAttributes(attrs...))
}

func (c *otelCounter) UnRegister() {}

type otelGauge struct {
	gauge       metric.Float64Gauge
	constLabels map[string]string
	values      map[string]float64
	*sync.Mutex
}

func (g *otelGauge) Count(value float64, lbs map[string]string, opts ...RecordOption) {
	g.Lock()
	defer g.Unlock()

	o := applyRecordOptions(opts)
	key := labelsKey(lbs)
	g.values[key] += value

	attrs := labelsToAttributes(mergeLabels(g.constLabels, lbs))
	g.gauge.Record(o.ctx, g.values[key], metric.WithAttributes(attrs...))
}

func (g *otelGauge) Set(value float64, lbs map[string]string, opts ...RecordOption) {
	g.Lock()
	defer g.Unlock()

	o := applyRecordOptions(opts)
	key := labelsKey(lbs)
	g.values[key] = value

	attrs := labelsToAttributes(mergeLabels(g.constLabels, lbs))
	g.gauge.Record(o.ctx, value, metric.WithAttributes(attrs...))
}

func (g *otelGauge) UnRegister() {}

type otelGaugeFunc struct{}

func (g *otelGaugeFunc) UnRegister() {}

type otelHistogram struct {
	histogram   metric.Float64Histogram
	constLabels map[string]string
}

func (h *otelHistogram) Observe(value float64, lbs map[string]string, opts ...RecordOption) {
	o := applyRecordOptions(opts)
	attrs := labelsToAttributes(mergeLabels(h.constLabels, lbs))
	h.histogram.Record(o.ctx, value, metric.WithAttributes(attrs...))
}

func (h *otelHistogram) UnRegister() {}

func labelsToAttributes(labels map[string]string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(labels))
	for k, v := range labels {
		attrs = append(attrs, attribute.String(k, v))
	}
	return attrs
}

func labelsKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	result := ""
	for _, k := range keys {
		result += k + "=" + labels[k] + ";"
	}
	return result
}
