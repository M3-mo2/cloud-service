package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds Prometheus collectors and a private registry.
type Metrics struct {
	registry      *prometheus.Registry
	requestsTotal *prometheus.CounterVec
	requestDur    *prometheus.HistogramVec
	inflight      prometheus.Gauge
	runsTotal     *prometheus.CounterVec
}

// NewMetrics builds a fresh metrics set with its own registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sandboxd_http_requests_total",
			Help: "Total number of HTTP requests.",
		}, []string{"method", "route", "status"}),
		requestDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sandboxd_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sandboxd_http_inflight_requests",
			Help: "Number of in-flight HTTP requests.",
		}),
		runsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sandboxd_runs_total",
			Help: "Total number of runs by terminal status.",
		}, []string{"status"}),
	}
	reg.MustRegister(m.requestsTotal, m.requestDur, m.inflight, m.runsTotal)
	reg.MustRegister(prometheus.NewGoCollector())
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	return m
}

// Registry returns the metrics registry.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Middleware records request counts, latency, and in-flight requests.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m.inflight.Inc()
		rw := &respWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		m.inflight.Dec()
		m.requestDur.WithLabelValues(r.Method, routeLabel(r)).Observe(time.Since(start).Seconds())
		m.requestsTotal.WithLabelValues(r.Method, routeLabel(r), strconv.Itoa(rw.status)).Inc()
	})
}

func routeLabel(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		if p := rc.RoutePattern(); p != "" {
			return p
		}
	}
	return "unmatched"
}
