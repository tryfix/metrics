package main

import (
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tryfix/metrics/v2"
	"log"
	"math/rand"
	"net/http"
	"time"
)

func main() {
	reporter := metrics.PrometheusReporter(metrics.ReporterConf{
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

			subScoreLatency.Observe(float64(time.Since(last).Milliseconds()), map[string]string{`type`: `type1`})
			last = time.Now()

			scoreCounter.Count(1, map[string]string{
				`type`: `major`,
			})

			subScoreCounter.Count(1, map[string]string{
				`attr_1`: `val 1`,
				`attr_2`: `val 2`,
			})

			thirdScoreCounter.Count(1, map[string]string{
				`attr_1`: `val 1`,
			})
		}
	}()

	r := http.NewServeMux()

	r.Handle(`/metrics`, promhttp.Handler())

	if err := http.ListenAndServe(`:9999`, r); err != nil {
		log.Fatal(err)
	}
}
