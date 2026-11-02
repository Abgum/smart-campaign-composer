// Package metrics holds the app's Prometheus metrics: counters a process
// keeps as it works, and gauges read from the database at each scrape.
// Every method is safe on a nil *Metrics, so code records unconditionally.
package metrics

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"smart-campaign-composer/backend/internal/clock"
	"smart-campaign-composer/backend/internal/tracking"
)

const (
	namespace     = "scc"
	scrapeTimeout = 3 * time.Second
)

// Metrics are the counters and histograms a process keeps.
type Metrics struct {
	providerAttempts *prometheus.CounterVec
	providerLatency  *prometheus.HistogramVec
	rateLimitWait    *prometheus.HistogramVec
	givenBack        *prometheus.CounterVec
	leasesReclaimed  prometheus.Counter
	llmRequests      *prometheus.CounterVec
	draftRejections  *prometheus.CounterVec
	llmRepairs       prometheus.Counter
	turns            *prometheus.CounterVec
	turnDedup        prometheus.Counter
	targetAmbiguous  prometheus.Counter
}

// New creates the metrics and registers them on reg.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		providerAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "provider_attempts_total",
			Help: "Provider calls by channel and outcome: sent, not_sent_retryable, not_sent_permanent, ambiguous.",
		}, []string{"channel", "outcome"}),
		providerLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "provider_latency_seconds", Help: "Duration of provider calls.",
			Buckets: prometheus.DefBuckets,
		}, []string{"channel"}),
		rateLimitWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "ratelimit_wait_seconds",
			Help:    "How long a ready delivery waited for its channel's grant before its call.",
			Buckets: []float64{0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"channel"}),
		givenBack: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "deliveries_given_back_total",
			Help: "Deliveries given back to the queue without a call, by reason: paused, busy, lease_short, stopped, error.",
		}, []string{"channel", "reason"}),
		leasesReclaimed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "leases_reclaimed_total",
			Help: "In-flight deliveries reclaimed after their lease ran out; each counts as an ambiguous attempt.",
		}),
		llmRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "llm_requests_total",
			Help: "Model calls by kind (router, main), provider and outcome (ok or an error code).",
		}, []string{"kind", "provider", "outcome"}),
		draftRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "draft_rejections_total",
			Help: "Drafts (save_campaign_draft calls) rejected by Go's checks, by reason: invalid_json, validation, " +
				"target, stale_version, ask_user_first, unknown_tool. Every rejection counts, the turn-ending one included.",
		}, []string{"reason"}),
		llmRepairs: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "llm_repairs_total",
			Help: "Rounds of rejected drafts sent back to the model to repair. The third rejected round of a turn " +
				"ends it (repair_exhausted) and is no repair, so this is not the number of rejections.",
		}),
		turns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "turns_total",
			Help: "Chat turns by outcome: ok, partial, failed, persist_failed.",
		}, []string{"outcome"}),
		turnDedup: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "turn_dedup_total",
			Help: "Repeated turn requests (same client_turn_id) answered without running the turn again.",
		}),
		targetAmbiguous: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "target_ambiguous_total",
			Help: "Turns that asked which campaign was meant instead of guessing.",
		}),
	}
	reg.MustRegister(m.providerAttempts, m.providerLatency, m.rateLimitWait, m.givenBack, m.leasesReclaimed,
		m.llmRequests, m.draftRejections, m.llmRepairs, m.turns, m.turnDedup, m.targetAmbiguous)
	return m
}

// ProviderAttempt counts a provider call and its duration.
func (m *Metrics) ProviderAttempt(channel, outcome string, latency time.Duration) {
	if m == nil {
		return
	}
	m.providerAttempts.WithLabelValues(channel, outcome).Inc()
	m.providerLatency.WithLabelValues(channel).Observe(latency.Seconds())
}

// RateLimitWait records how long a call waited for its grant.
func (m *Metrics) RateLimitWait(channel string, d time.Duration) {
	if m == nil {
		return
	}
	m.rateLimitWait.WithLabelValues(channel).Observe(d.Seconds())
}

// GivenBack counts n deliveries given back without a call.
func (m *Metrics) GivenBack(channel, reason string, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.givenBack.WithLabelValues(channel, reason).Add(float64(n))
}

// LeasesReclaimed counts expired leases taken back by the reaper.
func (m *Metrics) LeasesReclaimed(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.leasesReclaimed.Add(float64(n))
}

// LLMRequest counts a model call.
func (m *Metrics) LLMRequest(kind, provider, outcome string) {
	if m == nil {
		return
	}
	m.llmRequests.WithLabelValues(kind, provider, outcome).Inc()
}

// DraftRejected counts a rejected draft.
func (m *Metrics) DraftRejected(reason string) {
	if m == nil {
		return
	}
	m.draftRejections.WithLabelValues(reason).Inc()
}

// LLMRepairs counts rounds sent back to the model to repair.
func (m *Metrics) LLMRepairs(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.llmRepairs.Add(float64(n))
}

// Turn counts a finished turn.
func (m *Metrics) Turn(outcome string) {
	if m == nil {
		return
	}
	m.turns.WithLabelValues(outcome).Inc()
}

// TurnDedup counts a repeated turn request.
func (m *Metrics) TurnDedup() {
	if m == nil {
		return
	}
	m.turnDedup.Inc()
}

// TargetAmbiguous counts a turn that asked which campaign was meant.
func (m *Metrics) TargetAmbiguous() {
	if m == nil {
		return
	}
	m.targetAmbiguous.Inc()
}

// DBCollector reports gauges read from the database at each scrape:
// deliveries by channel and status, people left out at approval, the queue
// due now and channel pauses. Any process with a database pool can serve
// them; they are the same everywhere.
type DBCollector struct {
	pool                              *pgxpool.Pool
	clock                             clock.Clock
	deliveries, excluded, due, paused *prometheus.Desc
}

// NewDBCollector returns a collector reading through pool.
func NewDBCollector(pool *pgxpool.Pool, c clock.Clock) *DBCollector {
	return &DBCollector{
		pool: pool, clock: c,
		deliveries: prometheus.NewDesc(namespace+"_deliveries", "Deliveries by channel and status.",
			[]string{"channel", "status"}, nil),
		excluded: prometheus.NewDesc(namespace+"_deliveries_excluded",
			"People left out at approval, by channel and reason: consent, duplicate_content.", []string{"channel", "reason"}, nil),
		due: prometheus.NewDesc(namespace+"_queue_due", "Deliveries due now, by channel.", []string{"channel"}, nil),
		paused: prometheus.NewDesc(namespace+"_channel_paused", "1 while a provider 429 pauses the channel.",
			[]string{"channel"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *DBCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.deliveries
	ch <- c.excluded
	ch <- c.due
	ch <- c.paused
}

// Collect implements prometheus.Collector.
func (c *DBCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()
	t, err := tracking.ReadTotals(ctx, c.pool, c.clock.Now())
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.deliveries, err)
		return
	}
	for channel, counts := range t.Counts {
		for status, n := range counts.ByStatus() {
			ch <- prometheus.MustNewConstMetric(c.deliveries, prometheus.GaugeValue, float64(n), channel, status)
		}
		ch <- prometheus.MustNewConstMetric(c.excluded, prometheus.GaugeValue, float64(counts.ExcludedConsent), channel, "consent")
		ch <- prometheus.MustNewConstMetric(c.excluded, prometheus.GaugeValue, float64(counts.ExcludedDuplicate), channel, "duplicate_content")
	}
	for channel, s := range t.Channels {
		paused := 0.0
		if s.PausedUntil != nil {
			paused = 1
		}
		ch <- prometheus.MustNewConstMetric(c.due, prometheus.GaugeValue, float64(s.Due), channel)
		ch <- prometheus.MustNewConstMetric(c.paused, prometheus.GaugeValue, paused, channel)
	}
}
