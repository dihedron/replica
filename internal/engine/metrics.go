package engine

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics are the engine's Prometheus metrics (§7); no label carries a
// tenant (T-19).
type metrics struct {
	bytes         prometheus.Counter
	attempts      *prometheus.CounterVec
	adoptions     prometheus.Counter
	expiredLeases prometheus.Gauge
	webhooks      *prometheus.CounterVec
}

func newMetrics() *metrics {
	return &metrics{
		bytes: prometheus.NewCounter(prometheus.CounterOpts{Name: "veho_transfer_bytes_total", Help: "Bytes copied and recorded by this node's transfers."}),
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "veho_transfer_attempts_total",
			Help: "Transfer attempts that failed, by error class."}, []string{"class"}),
		adoptions: prometheus.NewCounter(prometheus.CounterOpts{Name: "veho_adoptions_total", Help: "Workflows of dead executors resumed by this node."}),
		webhooks: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "veho_webhook_deliveries_total",
			Help: "Webhook deliveries by result: success, failure (will be retried) or abandoned."}, []string{"result"}),
		expiredLeases: prometheus.NewGauge(prometheus.GaugeOpts{Name: "veho_expired_leases", Help: "Transfers whose lease expired, at the last housekeeping pass."}),
	}
}

// Register adds the engine's metrics to reg: its own, the heartbeat's age,
// and the jobs and transfers by state, counted at each scrape.
func (e *Engine) Register(reg prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{e.metrics.bytes, e.metrics.attempts, e.metrics.adoptions, e.metrics.expiredLeases, e.metrics.webhooks,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "veho_webhook_outbox_pending", Help: "Webhook events waiting for delivery."}, e.pendingWebhooks),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "veho_heartbeat_age_seconds", Help: "Time since this node's last successful heartbeat."},
			func() float64 {
				e.mu.Lock()
				defer e.mu.Unlock()
				return time.Since(e.lastOK).Seconds()
			}),
		states{e},
	} {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}

var stateDesc = prometheus.NewDesc("veho_objects", "Jobs and transfers in the state store, by kind and state.", []string{"kind", "state"}, nil)

// states counts the jobs and transfers by state at scrape time.
type states struct{ e *Engine }

func (s states) Describe(ch chan<- *prometheus.Desc) { ch <- stateDesc }

func (s states) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := s.e.worker.Query(ctx, "select kind, state, n from veho.state_counts()")
	if err != nil {
		slog.Warn("cannot count the jobs and transfers", "error", err)
		return
	}
	defer rows.Close()
	// every state, so that a state with nothing in it reads 0
	counts := map[[2]string]int64{}
	for _, st := range []string{"ACCEPTED", "RUNNING", "SUCCEEDED", "FAILED", "CANCELLED"} {
		counts[[2]string{"job", st}] = 0
	}
	for _, st := range []string{"PENDING", "COPYING", "VERIFYING", "RETRY_WAIT", "SUCCEEDED", "FAILED", "CANCELLED"} {
		counts[[2]string{"transfer", st}] = 0
	}
	for rows.Next() {
		var kind, state string
		var n int64
		if err := rows.Scan(&kind, &state, &n); err != nil {
			slog.Warn("cannot count the jobs and transfers", "error", err)
			return
		}
		counts[[2]string{kind, state}] = n
	}
	for k, n := range counts {
		ch <- prometheus.MustNewConstMetric(stateDesc, prometheus.GaugeValue, float64(n), k[0], k[1])
	}
}

// pendingWebhooks counts the events waiting, at scrape time.
func (e *Engine) pendingWebhooks() float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var n int64
	if err := e.worker.QueryRow(ctx, "select veho.webhook_outbox_pending()").Scan(&n); err != nil {
		slog.Warn("cannot count the pending webhook events", "error", err)
		return 0
	}
	return float64(n)
}
