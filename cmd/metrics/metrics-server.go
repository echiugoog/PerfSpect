package metrics

import (
	"log/slog"
	"math"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func startPrometheusServer(listenAddr string) {
	slog.Info("startPrometheusServer", slog.String("listenAddr", listenAddr))
	prometheus.MustRegister(prometheusMetricsGaugeVec)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	slog.Info("Starting Prometheus metrics server", slog.String("address", listenAddr))
	go func() {
		err := http.ListenAndServe(listenAddr, mux)
		if err != nil && err != http.ErrServerClosed {
			slog.Error("Prometheus HTTP server ListenAndServe error", slog.String("error", err.Error()))
		}
	}()
}

func updatePrometheusMetrics(metricFrames []MetricFrame) {
	for _, frame := range metricFrames {
		for _, metric := range frame.Metrics {
			if !math.IsNaN(metric.Value) {
				metricKey := promMetricPrefix + shortenMetricName(metric.Name)
				if m, ok := promMetrics[metricKey]; ok {
					m.WithLabelValues(
						frame.Socket,
						frame.CPU,
						frame.Cgroup,
						frame.PID,
						frame.Cmd,
					).Set(metric.Value)
				} else {
					slog.Warn("Unable to find metric", slog.String("metric", metricKey))
				}
			}
		}
	}
}
