package main

import (
	"github.com/tryfix/metrics"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"log"
	"net/http"
	"time"
)

func main() {

	reporter := metrics.PrometheusReporter(metrics.ReporterConf{
		System: `namespace`,
		Subsystem: `subsystem`,
		ConstLabels: map[string]string{
			`score`:`100`,
			`score_type`:`other`,
	},
	})

	scoreCounter := reporter.Counter(metrics.MetricConf{
		Path: `score_count`,
		Labels:[]string{`type`},
	})



	subScoreReporter := reporter.Reporter(metrics.ReporterConf{
		ConstLabels: map[string]string{
			`sub_score`:`2000`,
		},
	})

	subScoreCounter := subScoreReporter.Counter(metrics.MetricConf{
		Path: `sub_score_count`,
		Labels:[]string{`attr_1`, `attr_2`},
	})

	thirddScoreReporter := subScoreReporter.Reporter(metrics.ReporterConf{
		ConstLabels: map[string]string{
			`third_sub_score`:`99999`,
		},
	})

	thirddScoreCounter := thirddScoreReporter.Counter(metrics.MetricConf{
		Path: `third_sub_score_count`,
		Labels:[]string{`attr_1`},
	})

	//r2.GaugeFunc(metrics.MetricConf{
	//	Path: `test_count_rr`,
	//	Labels:[]string{`attr_1`, `attr_2`},
	//}, func() float64 {
	//	return 11.11
	//})

	//r3 := subScoreReportor.Reporter(metrics.ReporterConf{
	//	Subsystem: `level_two`,
	//	ConstLabels: map[string]string{
	//		`const_val_1_1_1`:`111`,
	//		`const_val2_2_2`:`222`,
	//	},
	//})
	//
	//c3 := r3.Counter(metrics.MetricConf{
	//	Path: `test_count`,
	//	Labels:[]string{`attr_1`, `attr_2`},
	//})
	//
	//eee := r3.GaugeFunc(metrics.MetricConf{
	//	Path: `test_count_rr`,
	//	Labels:[]string{`attr_1`, `attr_2`},
	//}, func() float64 {
	//	return 33.11
	//})




	go func() {
		for {
			time.Sleep(1 * time.Second)
			scoreCounter.Count(1, map[string]string{
				`type`: `major`,
			})

			subScoreCounter.Count(1, map[string]string{
				`attr_1`: `val 1`,
				`attr_2`: `val 2`,
			})

			thirddScoreCounter.Count(1, map[string]string{
				`attr_1`: `val 1`,
			})
		}
	}()

	r := http.NewServeMux()

	r.Handle(`/metrics`, promhttp.Handler())

	if err := http.ListenAndServe(`:9999`, r); err != nil{
		log.Fatal(err)
	}
}
