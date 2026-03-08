# Metrics

Metrics wrapper for Go libraries with support for Prometheus and OpenTelemetry backends.

## Install

```bash
go get github.com/tryfix/metrics/v2
```

## Backends

### Prometheus (direct)

```go
reporter := metrics.PrometheusReporter(metrics.ReporterConf{
    System:    "namespace",
    Subsystem: "subsystem",
    ConstLabels: map[string]string{"env": "prod"},
})
```

### OpenTelemetry

Uses the global `MeterProvider`:

```go
reporter := metrics.OTELReporter(metrics.ReporterConf{
    System:    "namespace",
    Subsystem: "subsystem",
})
```

Or inject a custom `MeterProvider`:

```go
reporter := metrics.OTELReporter(conf, metrics.WithMeterProvider(meterProvider))
```

## Usage

```go
// Counter
counter := reporter.Counter(metrics.MetricConf{
    Path:   "requests",
    Labels: []string{"method"},
    Help:   "Total requests",
})
counter.Count(1, map[string]string{"method": "GET"})

// Gauge
gauge := reporter.Gauge(metrics.MetricConf{
    Path:   "connections",
    Labels: []string{"pool"},
})
gauge.Set(42, map[string]string{"pool": "main"})
gauge.Count(1, map[string]string{"pool": "main"}) // increments by 1

// Observer (Histogram)
observer := reporter.Observer(metrics.MetricConf{
    Path:   "request_duration_seconds",
    Labels: []string{"endpoint"},
})
observer.Observe(0.25, map[string]string{"endpoint": "/api"})

// Sub-reporters inherit parent labels
subReporter := reporter.Reporter(metrics.ReporterConf{
    ConstLabels: map[string]string{"component": "auth"},
})
```

### Exemplars (trace context)

Pass a span context to link metrics with traces:

```go
ctx, span := tracer.Start(ctx, "operation")
defer span.End()

counter.Count(1, labels, metrics.WithContext(ctx))
observer.Observe(0.5, labels, metrics.WithContext(ctx))
```

## Exponential Histograms + Exemplars with Prometheus

The OTEL backend supports [exponential (native) histograms](https://prometheus.io/docs/specs/native_histograms/)
which auto-scale buckets without manual configuration. When combined with
exemplars, you get both high-resolution latency data and trace-to-metric
correlation.

### The challenge

Not all ingestion paths support both features simultaneously:

| Ingestion Path | Native Histograms | Exemplars | Both |
|---|---|---|---|
| **Protobuf Scrape (`PrometheusProto`)** | Yes | Yes | **Yes** |
| OpenMetrics Text Scrape | No (classic only) | Yes | No |
| OTLP Push | Yes | Not stored yet | No |
| OTEL Collector -> Remote Write | Yes | Dropped for histograms | No |

**The protobuf scrape path is the only proven end-to-end path** that delivers
both native histograms and exemplars into Prometheus.

### Setup

**Application:**

```go
import (
    promexporter "go.opentelemetry.io/otel/exporters/prometheus"
    sdkmetric "go.opentelemetry.io/otel/sdk/metric"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
    "go.opentelemetry.io/otel"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
    "github.com/tryfix/metrics/v2"
)

// Trace provider (needed for exemplar generation)
traceProvider := sdktrace.NewTracerProvider(
    sdktrace.WithSampler(sdktrace.AlwaysSample()),
)
otel.SetTracerProvider(traceProvider)

// OTEL Prometheus exporter with exponential histograms
exporter, _ := promexporter.New()
meterProvider := sdkmetric.NewMeterProvider(
    sdkmetric.WithReader(exporter),
    sdkmetric.WithView(sdkmetric.NewView(
        sdkmetric.Instrument{Kind: sdkmetric.InstrumentKindHistogram},
        sdkmetric.Stream{
            Aggregation: sdkmetric.AggregationBase2ExponentialHistogram{
                MaxSize:  160,
                MaxScale: 20,
            },
        },
    )),
)
otel.SetMeterProvider(meterProvider)

// Metrics reporter
reporter := metrics.OTELReporter(metrics.ReporterConf{
    System:    "myapp",
    Subsystem: "api",
})

// Expose /metrics endpoint (supports protobuf content negotiation)
http.Handle("/metrics", promhttp.HandlerFor(
    prometheus.DefaultGatherer,
    promhttp.HandlerOpts{EnableOpenMetrics: true},
))
```

**Prometheus configuration:**

```yaml
global:
  scrape_protocols:
    - PrometheusProto          # required for native histograms + exemplars
    - OpenMetricsText1.0.0

scrape_configs:
  - job_name: myapp
    static_configs:
      - targets: ['myapp:9090']
```

**Prometheus flags:**

```bash
prometheus \
  --enable-feature=native-histograms \
  --enable-feature=exemplar-storage
```

### What this gives you

- **Auto-scaling histogram buckets** - no manual bucket boundaries needed, accurate
  quantile computation across any value range
- **Exemplars on every observation** - each histogram data point carries a `trace_id`
  and `span_id` linking to the distributed trace
- **Full PromQL support** - `histogram_quantile()`, `histogram_count()`,
  `histogram_sum()` all work on native histograms

## Integration Tests

The integration tests verify all three ingestion paths using Testcontainers (requires Docker):

```bash
go test -tags=integration -v -timeout 120s
```

| Test | What it verifies |
|------|-----------------|
| `TestOTELDirectPush` | OTLP push: counters, exponential histogram quantiles, SDK-level exemplars |
| `TestExemplarsScrape` | OpenMetrics scrape: counter + classic histogram exemplars in Prometheus |
| `TestNativeHistogramExemplars` | Protobuf scrape: exponential histograms + exemplars together in Prometheus |
