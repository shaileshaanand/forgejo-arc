package controller

import (
	arc "forgejo-arc/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	activeGauge    = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "forgejo_arc_active_runners", Help: "Starting and running workers in a pool."}, []string{"namespace", "pool"})
	waitingGauge   = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "forgejo_arc_waiting_jobs", Help: "Matching Forgejo jobs waiting for a runner."}, []string{"namespace", "pool"})
	exhaustedGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "forgejo_arc_exhausted_jobs", Help: "Waiting jobs whose infrastructure retry budget is exhausted."}, []string{"namespace", "pool"})
	readyGauge     = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "forgejo_arc_pool_ready", Help: "Whether the last reconciliation succeeded."}, []string{"namespace", "pool"})
	startedCounter = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "forgejo_arc_workers_started_total", Help: "Workers started; not a count of successful workflows."}, []string{"namespace", "pool"})
)

func init() {
	metrics.Registry.MustRegister(activeGauge, waitingGauge, exhaustedGauge, readyGauge, startedCounter)
}

func updateMetrics(pool *arc.RunnerPool, ready bool) {
	labels := []string{pool.Namespace, pool.Name}
	activeGauge.WithLabelValues(labels...).Set(float64(pool.Status.ActiveRunners))
	waitingGauge.WithLabelValues(labels...).Set(float64(pool.Status.WaitingJobs))
	exhaustedGauge.WithLabelValues(labels...).Set(float64(pool.Status.ExhaustedJobs))
	value := 0.0
	if ready {
		value = 1
	}
	readyGauge.WithLabelValues(labels...).Set(value)
}

func deleteMetrics(namespace, name string) {
	activeGauge.DeleteLabelValues(namespace, name)
	waitingGauge.DeleteLabelValues(namespace, name)
	exhaustedGauge.DeleteLabelValues(namespace, name)
	readyGauge.DeleteLabelValues(namespace, name)
	startedCounter.DeleteLabelValues(namespace, name)
}
