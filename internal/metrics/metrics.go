// Package metrics exposes Prometheus metrics. Counters are updated from the
// asynchronous pipeline, never on the request path (except budget rejections).
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// M bundles all metrics.
type M struct {
	reg *prometheus.Registry

	Requests         *prometheus.CounterVec
	Tokens           *prometheus.CounterVec
	Cost             *prometheus.CounterVec
	Latency          *prometheus.HistogramVec
	TTFT             *prometheus.HistogramVec
	UsageUnknown     *prometheus.CounterVec
	BudgetRejections *prometheus.CounterVec
	EventsDropped    prometheus.Counter
	DBWriteErrors    prometheus.Counter
	DBWritten        prometheus.Counter
	QueueLength      prometheus.GaugeFunc
	AlertsSent       *prometheus.CounterVec
}

// New registers all metrics. queueLen reports the pipeline backlog.
func New(queueLen func() float64) *M {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := func(c prometheus.Collector) { reg.MustRegister(c) }
	latBuckets := []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 20, 40, 80, 160, 320}
	m := &M{
		reg: reg,
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "llmtee_requests_total",
			Help: "Proxied requests by upstream, model and HTTP status."}, []string{"upstream", "model", "status"}),
		Tokens: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "llmtee_tokens_total",
			Help: "Tokens by upstream, model and type (input, output, cache_read, cache_write)."}, []string{"upstream", "model", "type"}),
		Cost: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "llmtee_cost_usd_total",
			Help: "Cost in USD by upstream and model."}, []string{"upstream", "model"}),
		Latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "llmtee_request_duration_seconds",
			Help: "Total request duration including the upstream.", Buckets: latBuckets}, []string{"upstream"}),
		TTFT: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "llmtee_time_to_first_token_seconds",
			Help: "Time to first streamed byte.", Buckets: latBuckets}, []string{"upstream"}),
		UsageUnknown: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "llmtee_usage_unknown_total",
			Help: "Successful requests whose response contained no usage."}, []string{"upstream"}),
		BudgetRejections: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "llmtee_budget_rejections_total",
			Help: "Requests rejected with 429 because a key budget was exceeded."}, []string{"key_id", "window"}),
		EventsDropped: prometheus.NewCounter(prometheus.CounterOpts{Name: "llmtee_events_dropped_total",
			Help: "Log events dropped because the write queue was full."}),
		DBWriteErrors: prometheus.NewCounter(prometheus.CounterOpts{Name: "llmtee_db_write_errors_total",
			Help: "Failed database batch writes (events in the batch are lost)."}),
		DBWritten: prometheus.NewCounter(prometheus.CounterOpts{Name: "llmtee_db_events_written_total",
			Help: "Log events written to the database."}),
		AlertsSent: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "llmtee_alerts_sent_total",
			Help: "Budget alerts by channel and result."}, []string{"channel", "result"}),
	}
	m.QueueLength = prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "llmtee_queue_length",
		Help: "Log events waiting to be written."}, queueLen)
	for _, c := range []prometheus.Collector{m.Requests, m.Tokens, m.Cost, m.Latency, m.TTFT, m.UsageUnknown,
		m.BudgetRejections, m.EventsDropped, m.DBWriteErrors, m.DBWritten, m.QueueLength, m.AlertsSent} {
		f(c)
	}
	return m
}

// Handler serves /metrics.
func (m *M) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}
