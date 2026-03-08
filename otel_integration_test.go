//go:build integration

package metrics

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/model"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const promConfig = `
global:
  scrape_interval: 15s
  evaluation_interval: 15s
`

func startPrometheus(t *testing.T, ctx context.Context) string {
	t.Helper()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "prom/prometheus:v3.0.0",
			ExposedPorts: []string{"9090/tcp"},
			Cmd: []string{
				"--config.file=/etc/prometheus/prometheus.yml",
				"--web.enable-otlp-receiver",
				"--enable-feature=native-histograms",
			},
			Files: []testcontainers.ContainerFile{
				{
					Reader:            strings.NewReader(promConfig),
					ContainerFilePath: "/etc/prometheus/prometheus.yml",
					FileMode:          0o644,
				},
			},
			WaitingFor: wait.ForHTTP("/-/ready").WithPort("9090/tcp").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("failed to start prometheus container: %v", err)
	}

	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("failed to terminate prometheus container: %v", err)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("failed to get container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9090/tcp")
	if err != nil {
		t.Fatalf("failed to get mapped port: %v", err)
	}

	return fmt.Sprintf("%s:%s", host, port.Port())
}

func setupOTELSDK(t *testing.T, ctx context.Context, promEndpoint string) *sdkmetric.MeterProvider {
	t.Helper()

	// Trace provider with AlwaysSample — needed for exemplar generation
	traceProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(traceProvider)
	t.Cleanup(func() {
		traceProvider.Shutdown(context.Background())
	})

	// OTLP HTTP exporter pointing at Prometheus's OTLP receiver
	// Prometheus requires cumulative temporality
	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpoint(promEndpoint),
		otlpmetrichttp.WithURLPath("/api/v1/otlp/v1/metrics"),
		otlpmetrichttp.WithInsecure(),
		otlpmetrichttp.WithTemporalitySelector(func(sdkmetric.InstrumentKind) metricdata.Temporality {
			return metricdata.CumulativeTemporality
		}),
	)
	if err != nil {
		t.Fatalf("failed to create OTLP exporter: %v", err)
	}

	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter,
			sdkmetric.WithInterval(1*time.Second),
		)),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Kind: sdkmetric.InstrumentKindHistogram},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationBase2ExponentialHistogram{
					MaxSize:  160,
					MaxScale: 20,
				},
			},
		)),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOnFilter),
	)

	t.Cleanup(func() {
		meterProvider.Shutdown(context.Background())
	})

	return meterProvider
}

func queryWithRetry(t *testing.T, promAPI promv1.API, query string, maxRetries int, retryDelay time.Duration) model.Value {
	t.Helper()
	var result model.Value
	var err error
	for i := 0; i < maxRetries; i++ {
		result, _, err = promAPI.Query(context.Background(), query, time.Now())
		if err == nil {
			if vec, ok := result.(model.Vector); ok && len(vec) > 0 {
				return result
			}
		}
		time.Sleep(retryDelay)
	}
	t.Fatalf("query %q returned no results after %d retries: err=%v, result=%v",
		query, maxRetries, err, result)
	return nil
}

func TestOTELDirectPush(t *testing.T) {
	ctx := context.Background()

	// Phase 1: Start Prometheus with OTLP receiver and native histograms
	promEndpoint := startPrometheus(t, ctx)
	t.Logf("Prometheus running at %s", promEndpoint)

	// Phase 2: Set up OTEL SDK with OTLP exporter and exponential histograms
	meterProvider := setupOTELSDK(t, ctx, promEndpoint)

	// Phase 3: Record metrics via the library
	reporter := OTELReporter(ReporterConf{
		System:    "test",
		Subsystem: "integration",
	}, WithMeterProvider(meterProvider))

	counter := reporter.Counter(MetricConf{
		Path:   "requests",
		Labels: []string{"method"},
		Help:   "Total requests",
	})

	observer := reporter.Observer(MetricConf{
		Path:   "request_duration_seconds",
		Labels: []string{"endpoint"},
		Help:   "Request duration in seconds",
	})

	tracer := otel.Tracer("test-tracer")

	// Record counter values with trace context
	ctx1, span1 := tracer.Start(ctx, "counter-op")
	counter.Count(1, map[string]string{"method": "GET"}, WithContext(ctx1))
	counter.Count(1, map[string]string{"method": "GET"}, WithContext(ctx1))
	counter.Count(1, map[string]string{"method": "POST"}, WithContext(ctx1))
	span1.End()

	// Record histogram values with trace context
	histValues := []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5}
	for _, v := range histValues {
		ctx2, span2 := tracer.Start(ctx, "observe-op")
		observer.Observe(v, map[string]string{"endpoint": "/api"}, WithContext(ctx2))
		span2.End()
	}

	// Phase 4: Flush metrics to Prometheus
	if err := meterProvider.ForceFlush(ctx); err != nil {
		t.Fatalf("failed to flush metrics: %v", err)
	}

	// Create Prometheus API client
	client, err := api.NewClient(api.Config{
		Address: fmt.Sprintf("http://%s", promEndpoint),
	})
	if err != nil {
		t.Fatalf("failed to create prometheus client: %v", err)
	}
	promAPI := promv1.NewAPI(client)

	// Wait for Prometheus to ingest
	time.Sleep(3 * time.Second)

	// Phase 5: Verify
	t.Run("counter_values", func(t *testing.T) {
		result := queryWithRetry(t, promAPI, `test_integration_requests_total`, 10, 2*time.Second)
		vec := result.(model.Vector)

		if len(vec) < 2 {
			t.Fatalf("expected at least 2 series, got %d", len(vec))
		}

		counts := map[string]float64{}
		for _, sample := range vec {
			method := string(sample.Metric["method"])
			counts[method] = float64(sample.Value)
		}

		if counts["GET"] != 2 {
			t.Errorf("expected GET count=2, got %v", counts["GET"])
		}
		if counts["POST"] != 1 {
			t.Errorf("expected POST count=1, got %v", counts["POST"])
		}
	})

	t.Run("histogram_buckets", func(t *testing.T) {
		// Verify observation count
		countResult := queryWithRetry(t, promAPI, `histogram_count(test_integration_request_duration_seconds)`, 10, 2*time.Second)
		countVec := countResult.(model.Vector)
		count := float64(countVec[0].Value)
		if count != 7 {
			t.Errorf("expected histogram count=7, got %v", count)
		}

		// Verify sum
		sumResult := queryWithRetry(t, promAPI, `histogram_sum(test_integration_request_duration_seconds)`, 10, 2*time.Second)
		sumVec := sumResult.(model.Vector)
		sum := float64(sumVec[0].Value)
		expectedSum := 0.01 + 0.05 + 0.1 + 0.25 + 0.5 + 1.0 + 2.5 // 4.41
		if diff := sum - expectedSum; diff > 0.01 || diff < -0.01 {
			t.Errorf("expected histogram sum≈%.2f, got %v", expectedSum, sum)
		}

		// Verify quantile — proves real buckets exist (not just +Inf)
		p50Result := queryWithRetry(t, promAPI, `histogram_quantile(0.5, test_integration_request_duration_seconds)`, 10, 2*time.Second)
		p50Vec := p50Result.(model.Vector)
		p50 := float64(p50Vec[0].Value)
		// Median of [0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5] is 0.25
		// Exponential buckets give approximate values
		if p50 < 0.1 || p50 > 0.5 {
			t.Errorf("expected p50 between 0.1 and 0.5, got %v", p50)
		}
		t.Logf("p50 quantile: %v", p50)
	})

	t.Run("exemplars", func(t *testing.T) {
		// Verify the OTEL SDK produces exemplars with trace context.
		// Prometheus's OTLP receiver does not yet store exemplars from OTLP push,
		// so we verify at the SDK level using a ManualReader.
		manualReader := sdkmetric.NewManualReader()
		exemplarProvider := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(manualReader),
			sdkmetric.WithExemplarFilter(exemplar.AlwaysOnFilter),
		)
		defer exemplarProvider.Shutdown(ctx)

		exemplarReporter := OTELReporter(ReporterConf{
			System:    "exemplar",
			Subsystem: "test",
		}, WithMeterProvider(exemplarProvider))

		exemplarCounter := exemplarReporter.Counter(MetricConf{
			Path:   "exemplar_counter",
			Labels: []string{"k"},
		})

		exemplarObserver := exemplarReporter.Observer(MetricConf{
			Path:   "exemplar_histogram",
			Labels: []string{"k"},
		})

		// Record counter with trace context
		spanCtx, span := tracer.Start(ctx, "exemplar-counter-span")
		exemplarCounter.Count(1, map[string]string{"k": "v"}, WithContext(spanCtx))
		span.End()

		// Record histogram with trace context
		spanCtx2, span2 := tracer.Start(ctx, "exemplar-histogram-span")
		exemplarObserver.Observe(0.5, map[string]string{"k": "v"}, WithContext(spanCtx2))
		span2.End()

		var rm metricdata.ResourceMetrics
		if err := manualReader.Collect(ctx, &rm); err != nil {
			t.Fatalf("failed to collect: %v", err)
		}

		foundCounter := false
		foundHistogram := false
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				switch data := m.Data.(type) {
				case metricdata.Sum[float64]:
					for _, dp := range data.DataPoints {
						for _, ex := range dp.Exemplars {
							if len(ex.TraceID) > 0 && len(ex.SpanID) > 0 {
								foundCounter = true
								t.Logf("counter exemplar: traceID=%x spanID=%x", ex.TraceID, ex.SpanID)
							}
						}
					}
				case metricdata.Histogram[float64]:
					for _, dp := range data.DataPoints {
						for _, ex := range dp.Exemplars {
							if len(ex.TraceID) > 0 && len(ex.SpanID) > 0 {
								foundHistogram = true
								t.Logf("histogram exemplar: traceID=%x spanID=%x", ex.TraceID, ex.SpanID)
							}
						}
					}
				case metricdata.ExponentialHistogram[float64]:
					for _, dp := range data.DataPoints {
						for _, ex := range dp.Exemplars {
							if len(ex.TraceID) > 0 && len(ex.SpanID) > 0 {
								foundHistogram = true
								t.Logf("exp histogram exemplar: traceID=%x spanID=%x", ex.TraceID, ex.SpanID)
							}
						}
					}
				}
			}
		}

		if !foundCounter {
			t.Error("expected counter exemplar with valid trace_id and span_id")
		}
		if !foundHistogram {
			t.Error("expected histogram exemplar with valid trace_id and span_id")
		}
	})
}

func TestExemplarsScrape(t *testing.T) {
	ctx := context.Background()

	// Phase 1: Set up trace provider
	traceProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(traceProvider)
	defer traceProvider.Shutdown(ctx)

	// Phase 2: Set up OTEL Prometheus exporter bridge
	metricExporter, err := promexporter.New()
	if err != nil {
		t.Fatalf("failed to create prometheus exporter: %v", err)
	}

	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(metricExporter),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOnFilter),
	)
	defer meterProvider.Shutdown(ctx)

	// Phase 3: Start HTTP server exposing /metrics with OpenMetrics format
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	metricsPort := listener.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(
		prometheus.DefaultGatherer,
		promhttp.HandlerOpts{EnableOpenMetrics: true},
	))
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Close()

	t.Logf("Metrics server on port %d", metricsPort)

	// Phase 4: Record metrics with trace context
	reporter := OTELReporter(ReporterConf{
		System:    "scrape",
		Subsystem: "test",
	}, WithMeterProvider(meterProvider))

	counter := reporter.Counter(MetricConf{
		Path:   "requests",
		Labels: []string{"method"},
		Help:   "Total requests",
	})

	observer := reporter.Observer(MetricConf{
		Path:   "duration_seconds",
		Labels: []string{"op"},
		Help:   "Duration in seconds",
	})

	tracer := otel.Tracer("scrape-test-tracer")

	ctx1, span1 := tracer.Start(ctx, "counter-op")
	counter.Count(1, map[string]string{"method": "GET"}, WithContext(ctx1))
	span1.End()

	ctx2, span2 := tracer.Start(ctx, "observe-op")
	observer.Observe(0.5, map[string]string{"op": "read"}, WithContext(ctx2))
	span2.End()

	// Phase 5: Start Prometheus configured to scrape our metrics server
	// Use testcontainers.HostInternal so this works on both Docker Desktop and Linux CI
	scrapeConfig := fmt.Sprintf(`
global:
  scrape_interval: 2s
  evaluation_interval: 2s
  scrape_protocols:
    - OpenMetricsText1.0.0
    - OpenMetricsText0.0.1
scrape_configs:
  - job_name: test
    scrape_interval: 2s
    metrics_path: /metrics
    static_configs:
      - targets: ['%s:%d']
`, testcontainers.HostInternal, metricsPort)

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "prom/prometheus:v3.0.0",
			ExposedPorts: []string{"9090/tcp"},
			Cmd: []string{
				"--config.file=/etc/prometheus/prometheus.yml",
				"--enable-feature=exemplar-storage",
			},
			Files: []testcontainers.ContainerFile{
				{
					Reader:            strings.NewReader(scrapeConfig),
					ContainerFilePath: "/etc/prometheus/prometheus.yml",
					FileMode:          0o644,
				},
			},
			HostAccessPorts: []int{metricsPort},
			WaitingFor:      wait.ForHTTP("/-/ready").WithPort("9090/tcp").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("failed to start prometheus container: %v", err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("failed to terminate: %v", err)
		}
	}()

	host, _ := container.Host(ctx)
	port, _ := container.MappedPort(ctx, "9090/tcp")
	promEndpoint := fmt.Sprintf("%s:%s", host, port.Port())
	t.Logf("Prometheus running at %s", promEndpoint)

	// Phase 6: Create Prometheus API client and wait for scrape
	client, err := api.NewClient(api.Config{
		Address: fmt.Sprintf("http://%s", promEndpoint),
	})
	if err != nil {
		t.Fatalf("failed to create prometheus client: %v", err)
	}
	promAPI := promv1.NewAPI(client)

	// Wait for at least one scrape cycle
	queryWithRetry(t, promAPI, `scrape_test_requests_total`, 15, 2*time.Second)

	// Phase 7: Query exemplars
	t.Run("counter_exemplars", func(t *testing.T) {
		results, err := promAPI.QueryExemplars(ctx,
			`scrape_test_requests_total`,
			time.Now().Add(-5*time.Minute),
			time.Now(),
		)
		if err != nil {
			t.Fatalf("failed to query exemplars: %v", err)
		}

		found := false
		for _, result := range results {
			for _, ex := range result.Exemplars {
				t.Logf("counter exemplar: labels=%v value=%v", ex.Labels, ex.Value)
				if _, ok := ex.Labels["trace_id"]; ok {
					found = true
				}
			}
		}

		if !found {
			t.Error("expected counter exemplar with trace_id from Prometheus scrape")
		}
	})

	t.Run("histogram_exemplars", func(t *testing.T) {
		// Prometheus stores histogram exemplars against _bucket series
		results, err := promAPI.QueryExemplars(ctx,
			`scrape_test_duration_seconds_bucket`,
			time.Now().Add(-5*time.Minute),
			time.Now(),
		)
		if err != nil {
			t.Fatalf("failed to query exemplars: %v", err)
		}

		found := false
		for _, result := range results {
			for _, ex := range result.Exemplars {
				t.Logf("histogram exemplar: labels=%v value=%v", ex.Labels, ex.Value)
				if _, ok := ex.Labels["trace_id"]; ok {
					found = true
				}
			}
		}

		if !found {
			t.Error("expected histogram exemplar with trace_id from Prometheus scrape")
		}
	})
}

// TestNativeHistogramExemplars verifies that exponential (native) histograms
// AND exemplars work together end-to-end through Prometheus protobuf scraping.
// This is the only scrape path that supports both features simultaneously:
//   - PrometheusProto scrape protocol carries native histogram data
//   - Native histograms support exemplars in the protobuf format
//   - Prometheus TSDB stores exemplars for native histograms
func TestNativeHistogramExemplars(t *testing.T) {
	ctx := context.Background()

	// Phase 1: Set up trace provider
	traceProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(traceProvider)
	defer traceProvider.Shutdown(ctx)

	// Phase 2: Set up OTEL Prometheus exporter with exponential histograms
	metricExporter, err := promexporter.New()
	if err != nil {
		t.Fatalf("failed to create prometheus exporter: %v", err)
	}

	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(metricExporter),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Kind: sdkmetric.InstrumentKindHistogram},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationBase2ExponentialHistogram{
					MaxSize:  160,
					MaxScale: 20,
				},
			},
		)),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOnFilter),
	)
	defer meterProvider.Shutdown(ctx)

	// Phase 3: Start HTTP server exposing /metrics with protobuf negotiation
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	metricsPort := listener.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(
		prometheus.DefaultGatherer,
		promhttp.HandlerOpts{EnableOpenMetrics: true},
	))
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Close()

	t.Logf("Metrics server on port %d", metricsPort)

	// Phase 4: Record metrics with trace context
	reporter := OTELReporter(ReporterConf{
		System:    "native",
		Subsystem: "hist",
	}, WithMeterProvider(meterProvider))

	counter := reporter.Counter(MetricConf{
		Path:   "requests",
		Labels: []string{"method"},
		Help:   "Total requests",
	})

	observer := reporter.Observer(MetricConf{
		Path:   "duration_seconds",
		Labels: []string{"op"},
		Help:   "Duration in seconds",
	})

	tracer := otel.Tracer("native-hist-tracer")

	ctx1, span1 := tracer.Start(ctx, "counter-op")
	counter.Count(1, map[string]string{"method": "GET"}, WithContext(ctx1))
	span1.End()

	// Record multiple histogram values with trace context
	histValues := []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5}
	for _, v := range histValues {
		spanCtx, span := tracer.Start(ctx, "observe-op")
		observer.Observe(v, map[string]string{"op": "read"}, WithContext(spanCtx))
		span.End()
	}

	// Phase 5: Start Prometheus with PrometheusProto scrape protocol + native histograms
	// Use testcontainers.HostInternal so this works on both Docker Desktop and Linux CI
	scrapeConfig := fmt.Sprintf(`
global:
  scrape_interval: 2s
  evaluation_interval: 2s
  scrape_protocols:
    - PrometheusProto
    - OpenMetricsText1.0.0
scrape_configs:
  - job_name: test
    scrape_interval: 2s
    metrics_path: /metrics
    static_configs:
      - targets: ['%s:%d']
`, testcontainers.HostInternal, metricsPort)

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "prom/prometheus:v3.0.0",
			ExposedPorts: []string{"9090/tcp"},
			Cmd: []string{
				"--config.file=/etc/prometheus/prometheus.yml",
				"--enable-feature=native-histograms,exemplar-storage",
			},
			Files: []testcontainers.ContainerFile{
				{
					Reader:            strings.NewReader(scrapeConfig),
					ContainerFilePath: "/etc/prometheus/prometheus.yml",
					FileMode:          0o644,
				},
			},
			HostAccessPorts: []int{metricsPort},
			WaitingFor:      wait.ForHTTP("/-/ready").WithPort("9090/tcp").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("failed to start prometheus container: %v", err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("failed to terminate: %v", err)
		}
	}()

	host, _ := container.Host(ctx)
	port, _ := container.MappedPort(ctx, "9090/tcp")
	promEndpoint := fmt.Sprintf("%s:%s", host, port.Port())
	t.Logf("Prometheus running at %s", promEndpoint)

	// Phase 6: Create Prometheus API client and wait for scrape
	client, err := api.NewClient(api.Config{
		Address: fmt.Sprintf("http://%s", promEndpoint),
	})
	if err != nil {
		t.Fatalf("failed to create prometheus client: %v", err)
	}
	promAPI := promv1.NewAPI(client)

	// Wait for data to appear — use counter as readiness signal
	queryWithRetry(t, promAPI, `native_hist_requests_total`, 15, 2*time.Second)

	// Phase 7: Verify native histogram buckets
	t.Run("native_histogram_quantile", func(t *testing.T) {
		// histogram_count works for both classic and native histograms
		countResult := queryWithRetry(t, promAPI, `histogram_count(native_hist_duration_seconds)`, 10, 2*time.Second)
		countVec := countResult.(model.Vector)
		count := float64(countVec[0].Value)
		if count != 7 {
			t.Errorf("expected histogram count=7, got %v", count)
		}

		// histogram_quantile on native histograms proves real bucket resolution
		p50Result := queryWithRetry(t, promAPI, `histogram_quantile(0.5, native_hist_duration_seconds)`, 10, 2*time.Second)
		p50Vec := p50Result.(model.Vector)
		p50 := float64(p50Vec[0].Value)
		// Median of [0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5] is 0.25
		if p50 < 0.1 || p50 > 0.5 {
			t.Errorf("expected p50 between 0.1 and 0.5, got %v (proves native histogram has real bucket resolution)", p50)
		}
		t.Logf("native histogram p50 quantile: %v", p50)
	})

	// Phase 8: Verify exemplars from Prometheus
	t.Run("counter_exemplars", func(t *testing.T) {
		results, err := promAPI.QueryExemplars(ctx,
			`native_hist_requests_total`,
			time.Now().Add(-5*time.Minute),
			time.Now(),
		)
		if err != nil {
			t.Fatalf("failed to query exemplars: %v", err)
		}

		found := false
		for _, result := range results {
			for _, ex := range result.Exemplars {
				t.Logf("counter exemplar: labels=%v value=%v", ex.Labels, ex.Value)
				if _, ok := ex.Labels["trace_id"]; ok {
					found = true
				}
			}
		}

		if !found {
			t.Error("expected counter exemplar with trace_id")
		}
	})

	t.Run("native_histogram_exemplars", func(t *testing.T) {
		// For native histograms, exemplars are stored against the base metric name
		// (no _bucket suffix since native histograms don't have explicit bucket series)
		selectors := []string{
			`native_hist_duration_seconds`,
			`{__name__=~"native_hist_duration.*"}`,
		}

		found := false
		for _, selector := range selectors {
			results, err := promAPI.QueryExemplars(ctx,
				selector,
				time.Now().Add(-5*time.Minute),
				time.Now(),
			)
			if err != nil {
				t.Logf("exemplar query %q error: %v", selector, err)
				continue
			}
			t.Logf("exemplar query %q returned %d result(s)", selector, len(results))
			for _, result := range results {
				for _, ex := range result.Exemplars {
					t.Logf("native histogram exemplar: labels=%v value=%v", ex.Labels, ex.Value)
					if _, ok := ex.Labels["trace_id"]; ok {
						found = true
					}
				}
			}
			if found {
				break
			}
		}

		if !found {
			t.Error("expected native histogram exemplar with trace_id — " +
				"this proves exponential histograms + exemplars work together via PrometheusProto scrape")
		}
	})
}
