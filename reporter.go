package metrics

import "context"

// MetricConf configures an individual metric (counter, gauge, or observer).
type MetricConf struct {
	// Path is the metric name suffix. The full name is built as {System}_{Subsystem}_{Path}.
	Path string
	// Labels are the dynamic label keys that must be provided when recording values.
	Labels []string
	// Help is a human-readable description of the metric.
	Help string
	// ConstLabels are static key-value pairs attached to every observation of this metric.
	ConstLabels map[string]string
}

// ReporterConf configures a Reporter instance.
type ReporterConf struct {
	// System is the top-level namespace prefix for all metrics created by this reporter.
	System string
	// Subsystem is appended after System in the metric name hierarchy.
	// Sub-reporters created via Reporter.Reporter() concatenate their Subsystem with the parent's.
	Subsystem string
	// ConstLabels are static key-value pairs attached to every metric created by this reporter.
	// Child reporters inherit and merge parent const labels; duplicate keys cause a panic.
	ConstLabels map[string]string
}

// RecordOption applies optional configuration to metric recording calls.
type RecordOption func(*recordOptions)

// recordOptions holds the resolved configuration from RecordOption functions.
type recordOptions struct {
	// ctx carries trace context used by the OTEL backend to generate exemplars.
	ctx context.Context
}

func applyRecordOptions(opts []RecordOption) recordOptions {
	o := recordOptions{ctx: context.Background()}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// WithContext sets the context for a metric recording call.
// When using the OTEL backend, this propagates trace context as exemplars.
func WithContext(ctx context.Context) RecordOption {
	return func(o *recordOptions) {
		o.ctx = ctx
	}
}

// Reporter is the central factory for creating metrics. It is backend-agnostic
// interface and swap implementations (Prometheus, OTEL, Noop)
// without changing application code.
type Reporter interface {
	// Reporter creates a child reporter that inherits the parent's const labels
	// and appends to the subsystem prefix.
	Reporter(ReporterConf) Reporter
	// Counter creates or retrieves a monotonically increasing counter metric.
	Counter(MetricConf) Counter
	// Observer creates or retrieves a histogram/summary metric for recording distributions.
	Observer(MetricConf) Observer
	// Gauge creates or retrieves a metric that can go up and down.
	Gauge(MetricConf) Gauge
	// GaugeFunc creates a gauge whose value is computed by the provided function on each collection.
	GaugeFunc(MetricConf, func() float64) GaugeFunc
	// Info returns a description of the reporter backend.
	Info() string
	// UnRegister removes a previously registered metric by its Path.
	UnRegister(metrics string)
}

// Collector is the base interface for all metric types, providing lifecycle management.
type Collector interface {
	// UnRegister removes this metric from the underlying registry.
	UnRegister()
}

// Counter is a monotonically increasing metric (e.g. total requests, errors).
type Counter interface {
	Collector
	// Count adds the given value to the counter. Labels must match the keys defined in MetricConf.Labels.
	// Pass WithContext(ctx) to attach trace context as exemplars (OTEL backend).
	Count(value float64, lbs map[string]string, opts ...RecordOption)
}

// Gauge is a metric that can increase and decrease (e.g. active connections, queue depth).
type Gauge interface {
	Collector
	// Count adds the given value to the gauge (use negative values to decrement).
	Count(value float64, lbs map[string]string, opts ...RecordOption)
	// Set replaces the gauge value with the given absolute value.
	Set(value float64, lbs map[string]string, opts ...RecordOption)
}

// GaugeFunc is a gauge whose value is computed by a callback function on each collection.
type GaugeFunc interface {
	Collector
}

// Observer records value distributions (e.g. request latency, response sizes).
// Backed by a histogram (OTEL, Prometheus Histogram) or summary (Prometheus Summary).
type Observer interface {
	Collector
	// Observe records a single value into the distribution.
	// Pass WithContext(ctx) to attach trace context as exemplars (OTEL backend).
	Observe(value float64, lbs map[string]string, opts ...RecordOption)
}
