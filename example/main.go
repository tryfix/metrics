package main

import (
	"context"
	"log"
	"math/rand"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tryfix/metrics/v2"
	"go.opentelemetry.io/otel"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func main() {
	// Set up trace provider with stdout exporter
	traceExporter, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
	if err != nil {
		log.Fatal(err)
	}

	traceProvider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExporter))
	otel.SetTracerProvider(traceProvider)

	// Set up the OTEL Prometheus exporter and MeterProvider
	metricExporter, err := promexporter.New()
	if err != nil {
		log.Fatal(err)
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(metricExporter),
		// Use exponential histograms — auto-scaling buckets, no manual bucket config needed
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

	tracer := otel.Tracer("example")

	reporter := metrics.OTELReporter(metrics.ReporterConf{
		System:    `namespace`,
		Subsystem: `subsystem`,
		ConstLabels: map[string]string{
			`score`:      `100`,
			`score_type`: `other`,
		},
	})

	scoreCounter := reporter.Counter(metrics.MetricConf{
		Path:   `score_count`,
		Labels: []string{`type`},
		Help:   `Example score_count help`,
	})

	subScoreReporter := reporter.Reporter(metrics.ReporterConf{
		ConstLabels: map[string]string{
			`sub_score`: `2000`,
		},
	})

	subScoreCounter := subScoreReporter.Counter(metrics.MetricConf{
		Path:   `sub_score_count`,
		Labels: []string{`attr_1`, `attr_2`},
		Help:   `Example sub_score_count help`,
	})

	subScoreLatency := subScoreReporter.Observer(metrics.MetricConf{
		Path:   `sub_score_latency_milliseconds`,
		Labels: []string{`type`},
		Help:   `Example sub_score_latency in Milliseconds`,
	})

	thirdScoreReporter := subScoreReporter.Reporter(metrics.ReporterConf{
		ConstLabels: map[string]string{
			`third_sub_score`: `99999`,
		},
	})

	thirdScoreCounter := thirdScoreReporter.Counter(metrics.MetricConf{
		Path:   `third_sub_score_count`,
		Labels: []string{`attr_1`},
	})

	go func() {
		last := time.Now()
		for {
			time.Sleep(time.Duration(rand.Intn(500)) * time.Millisecond)

			// Create a trace span and pass its context to metrics
			ctx, span := tracer.Start(context.Background(), "process_scores")

			subScoreLatency.Observe(float64(time.Since(last).Milliseconds()), map[string]string{`type`: `type1`}, metrics.WithContext(ctx))
			last = time.Now()

			scoreCounter.Count(1, map[string]string{
				`type`: `major`,
			}, metrics.WithContext(ctx))

			subScoreCounter.Count(1, map[string]string{
				`attr_1`: `val 1`,
				`attr_2`: `val 2`,
			}, metrics.WithContext(ctx))

			thirdScoreCounter.Count(1, map[string]string{
				`attr_1`: `val 1`,
			}, metrics.WithContext(ctx))

			span.End()
		}
	}()

	r := http.NewServeMux()

	r.Handle(`/metrics`, promhttp.HandlerFor(
		prometheus.DefaultGatherer,
		promhttp.HandlerOpts{EnableOpenMetrics: true},
	))

	host := `:9999`
	log.Printf(`metrics endpoint can be accessed from %s/metrics`, host)
	if err := http.ListenAndServe(host, r); err != nil {
		log.Fatal(err)
	}
}
