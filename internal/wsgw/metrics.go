// Prometheus instrumentation for the WebSocket gateway.
//
// # These names are a CONTRACT that was written before this package existed
//
// deploy/observability/grafana/dashboards/sharpline-overview.json and
// deploy/observability/rules/sharpline-alerts.yml both select these series by
// name and by exact label value. Read them before changing anything here:
//
//	sharpline_ws_connections_active              dashboard panels 6 and 21; the
//	                                             CUSTOM METRIC the `stream` HPA
//	                                             scales on (CLAUDE.md §9), and
//	                                             the `and on ()` guard in
//	                                             OddsFanoutStopped
//	sharpline_ws_connections_total{result}       dashboard, rate by result
//	sharpline_ws_clients_dropped_total{reason}   dashboard, rate by reason;
//	                                             alert WebSocketClientsDropping,
//	                                             whose description names
//	                                             reason="slow_consumer"
//	sharpline_ws_resyncs_total{reason}           dashboard, rate by reason;
//	                                             numerator of WebSocketResyncStorm
//	sharpline_ws_messages_sent_total{kind}       dashboard, rate by kind;
//	                                             denominator of
//	                                             WebSocketResyncStorm at
//	                                             kind="delta"
//
// Every label value comes from a typed constant in protocol.go. That is the
// mechanical half of keeping the contract: a value spelled once in the hub and
// once here eventually differs by a character, and the alert selecting the other
// spelling matches NOTHING — Prometheus answers a query for a series that does
// not exist with no data rather than with an error, so the SLI reads as absent
// rather than as broken. sharpline-alerts.yml makes exactly that point about
// bucket boundaries and it applies equally to label values.
//
// # Three series here are SHARED and are adopted, not redeclared
//
// sharpline_odds_staleness_seconds{stage,league,provider},
// sharpline_pipeline_latency_seconds{stage,league} and
// sharpline_odds_clock_skew_total{provider,stage} are declared by
// internal/ingest/provider and internal/ingest/normalizer, and this package
// emits stage="fanout" onto them.
//
// That stage is THE HEADLINE SLO. CLAUDE.md §9 says "Odds staleness is the
// headline SLO — define it, alert on it, put it on the dashboard", and
// internal/ingest/provider's StageFanout says it in capitals: "THIS ONE IS THE
// HEADLINE SLO; the recording rules read only this stage." The recording rules
// sharpline:odds_staleness_seconds:p50 and :p99 have returned "No data" since
// phase 0 waiting for this file.
//
// The adoption uses prometheus.AlreadyRegisteredError, structurally identical to
// internal/ingest/normalizer's sharedHistogramVec/sharedCounterVec, and for the
// reason that file gives: AlreadyRegisteredError is returned only for an
// IDENTICAL descriptor, so a disagreement about help text, label names or bucket
// boundaries produces a plain error and fails startup loudly. The help strings
// and buckets below are therefore COPIED CHARACTER FOR CHARACTER from the
// declaring packages; they are not a paraphrase, and metrics_test.go registers
// all three packages' sets on one registry in both orders to keep it that way.
//
// # Labels deliberately not set
//
//   - `service`. deploy/observability/prometheus.yml attaches it as a TARGET
//     label; a metric label of the same name is renamed `exported_service` and
//     the two drift. internal/platform/kafka, internal/ingest/writer and
//     internal/ingest/normalizer all make the same choice.
//   - a connection id, a session id, a user id or a channel. Unbounded
//     cardinality, and the last three are user data.
//   - error text, or any client-supplied string. Bounded classifications only —
//     [RejectReason] and [DropReason] exist precisely so an untrusted string
//     never becomes a label value.
package wsgw

import (
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/pricing"
)

// Metric namespace and subsystem: together, the `sharpline_ws_` prefix.
// deploy/observability/prometheus.yml states the rule that every application
// series is prefixed `sharpline_`.
const (
	metricNamespace = "sharpline"
	metricSubsystem = "ws"
)

// Bus record outcomes — the fold's vocabulary. A closed set; every value appears
// in a code path in state.go.
const (
	// recordStored: the record was decoded, validated and folded into the slate.
	recordStored = "stored"
	// recordRemoved: a tombstone deleted the key. On a compacted topic that is
	// permanent, so the market leaves the board.
	recordRemoved = "removed"
	// recordStale: observed strictly before the state already held, so applying
	// it would regress the slate and republish an older price as news.
	recordStale = "stale"
	// recordRejected: undecodable, or a document that fails Validate, or a key
	// that disagrees with its payload. NOT stored — a market a client cannot be
	// told the truth about must not be in the slate.
	recordRejected = "rejected"
	// recordUnsupported: an envelope message type this build does not read.
	recordUnsupported = "unsupported"
)

// PresenceOp names the Redis operation a presence error came from. A closed set,
// because it is the label on sharpline_ws_presence_errors_total and D6 requires
// the degradation to be visible per use rather than as one undifferentiated
// counter.
type PresenceOp string

// The presence operations.
const (
	// PresenceOpRestore — reading a durable subscription set on connect. Its
	// failure costs resume-on-reconnect and nothing else.
	PresenceOpRestore PresenceOp = "restore"
	// PresenceOpSubscribe — adding channels to the durable set.
	PresenceOpSubscribe PresenceOp = "subscribe"
	// PresenceOpUnsubscribe — removing channels from it.
	PresenceOpUnsubscribe PresenceOp = "unsubscribe"
	// PresenceOpHeartbeat — refreshing the TTLs. Sustained failure is what
	// silently expires a session that is still live.
	PresenceOpHeartbeat PresenceOp = "heartbeat"
	// PresenceOpForget — tearing the session down on close.
	PresenceOpForget PresenceOp = "forget"
)

// String implements fmt.Stringer.
func (o PresenceOp) String() string { return string(o) }

// Valid reports whether o is an operation this build reports.
func (o PresenceOp) Valid() bool {
	switch o {
	case PresenceOpRestore, PresenceOpSubscribe, PresenceOpUnsubscribe,
		PresenceOpHeartbeat, PresenceOpForget:
		return true
	default:
		return false
	}
}

// PresenceOps returns the presence operations in a stable order.
func PresenceOps() []PresenceOp {
	return []PresenceOp{
		PresenceOpRestore, PresenceOpSubscribe, PresenceOpUnsubscribe,
		PresenceOpHeartbeat, PresenceOpForget,
	}
}

// writeDelayBuckets covers the gap between "the hub handed the frame to a send
// queue" and "the bytes went to the socket".
//
// The interesting region is tens of microseconds — the queue is in memory and
// the write is one syscall — so the resolution is at the bottom. The tail
// reaches 2.5s because that is where the interesting FAILURE lives: a client
// whose TCP window has closed makes this histogram, and nothing else, say so.
var writeDelayBuckets = []float64{
	0.00001, 0.000025, 0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025,
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5,
}

// sendQueueBuckets covers a per-connection send queue at enqueue time, on a
// doubling scale up to DefaultSendQueueCapacity. The top bucket is the one that
// matters: samples arriving there are clients about to be dropped as
// slow_consumer, which makes this histogram the leading indicator for
// WebSocketClientsDropping rather than a lagging one.
var sendQueueBuckets = []float64{1, 2, 4, 8, 16, 32, 64, 128, 256}

// Metrics is the gateway's collector set.
//
// Registration happens once, in [NewMetrics], and the value is injected — the
// pattern internal/platform/kafka, internal/ingest/provider,
// internal/ingest/normalizer and internal/pricing all follow, for the same
// reason: one process may legitimately build more than one hub, and duplicate
// registration should fail its startup rather than its code review.
//
// A nil Registerer builds the collectors WITHOUT registering them. That is right
// for a unit test and for any process with no /metrics endpoint: the observe
// calls stay live and cost a few nanoseconds, so no call site needs a nil check.
// A nil *Metrics is also safe — every method below returns early — which is what
// lets Options.Metrics be optional.
type Metrics struct {
	connectionsActive prometheus.Gauge
	connections       *prometheus.CounterVec
	dropped           *prometheus.CounterVec
	resyncs           *prometheus.CounterVec
	messagesSent      *prometheus.CounterVec
	writeDelay        prometheus.Histogram
	sendQueue         prometheus.Histogram
	subscriptions     prometheus.Gauge
	marketsTracked    prometheus.Gauge
	busRecords        *prometheus.CounterVec
	channelRejects    *prometheus.CounterVec
	presenceErrors    *prometheus.CounterVec

	// Shared with internal/ingest/provider (staleness, skew),
	// internal/ingest/normalizer (all three) and internal/pricing. See the file
	// comment: these are adopted, never redeclared.
	staleness *prometheus.HistogramVec
	pipeline  *prometheus.HistogramVec
	clockSkew *prometheus.CounterVec
}

// NewMetrics builds the collectors and registers them on reg.
//
// It returns an error rather than panicking: CLAUDE.md §12 forbids a panic
// outside main, and a registration conflict is a wiring mistake the caller
// reports with the rest of its startup context.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		connectionsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "connections_active",
			Help: "Live WebSocket connections on this replica. It is the custom metric the " +
				"stream HPA scales on (CLAUDE.md §9) and the guard in OddsFanoutStopped, which " +
				"reads a zero fanout rate as a failure only while clients are connected.",
		}),

		connections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "connections_total",
			Help: "Upgrade attempts by outcome (accepted|rejected|upgrade_failed). rejected is " +
				"this gateway's own decision — no sharpline.v1 offer, a token in the query " +
				"string, a credential that did not verify — and upgrade_failed is the handshake " +
				"itself failing, which is a network or client problem rather than a policy one.",
		}, []string{"result"}),

		dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "clients_dropped_total",
			Help: "Connections taken down, by bounded reason (slow_consumer|write_error|" +
				"read_error|idle_timeout|protocol_error|shutdown). slow_consumer means the " +
				"per-client bounded send queue overflowed and its pending buffer was discarded " +
				"(CLAUDE.md §5) — expected under load, a defect at idle.",
		}, []string{"reason"}),

		resyncs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "resyncs_total",
			Help: "Clients told to resynchronise, by bounded reason (slow_consumer|" +
				"client_requested|presence_lost). Measured against the delta rate by " +
				"WebSocketResyncStorm: sustained resyncs mean the delta stream is losing " +
				"messages rather than merely being busy.",
		}, []string{"reason"}),

		messagesSent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "messages_sent_total",
			Help: "Frames enqueued to clients, by kind (hello|ack|snapshot|delta|resync|error|" +
				"pong). Counted at ENQUEUE, which is where the sequence number is assigned, so " +
				"it agrees with the numbers a client actually sees rather than with the subset " +
				"that survived a slow consumer.",
		}, []string{"kind"}),

		writeDelay: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "write_delay_seconds",
			Help: "Time from a frame being enqueued to it being written to the client socket. " +
				"It exists because the staleness SLO is observed at the queue hand-off rather " +
				"than at the syscall, so this is the difference between what was measured and " +
				"what the client received — measured rather than assumed.",
			Buckets: writeDelayBuckets,
		}),

		sendQueue: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "send_queue_depth",
			Help: "Depth of a per-client send queue at the moment a frame was enqueued. Samples " +
				"near the capacity are clients about to be dropped as slow_consumer, which makes " +
				"this the leading indicator for WebSocketClientsDropping.",
			Buckets: sendQueueBuckets,
		}),

		subscriptions: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "subscriptions_active",
			Help: "Channel subscriptions held across every connection on this replica. Divided " +
				"by connections_active it is the average channels per client, which is what " +
				"decides whether a delta costs one send or a thousand.",
		}),

		marketsTracked: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "markets_tracked",
			Help: "Markets currently held in this replica's fold of the compacted price.computed " +
				"topic. Every replica reads every partition (no consumer group), so a persistent " +
				"disagreement between replicas means one of them is not caught up.",
		}),

		busRecords: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "bus_records_total",
			Help: "price.computed records folded into the slate, by outcome (stored|removed|" +
				"stale|rejected|unsupported). Anything but stored and removed is a market some " +
				"client is not being told about, so rejected must be zero in a healthy system.",
		}, []string{"result"}),

		channelRejects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "channel_rejects_total",
			Help: "Channels refused on a subscribe, by bounded reason (malformed|unknown_kind|" +
				"invalid_id|too_long|limit_reached|duplicate). The reason is NEVER derived from " +
				"the client's string; a steady rate of one reason is a client bug worth naming.",
		}, []string{"reason"}),

		presenceErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "presence_errors_total",
			Help: "Redis subscription-state operations that failed, by operation. Redis is never " +
				"the source of truth here (CLAUDE.md §3): these failures cost " +
				"resume-on-reconnect, not correctness, and the socket is unaffected — which is " +
				"why they are counted rather than escalated.",
		}, []string{"op"}),

		// ---- shared contract series -----------------------------------------
		// Help text and buckets are copied character for character from the
		// declaring packages. A paraphrase here would produce a descriptor that
		// is NOT identical, AlreadyRegisteredError would not be returned, and
		// the process would fail to start — which is the designed behaviour, but
		// the intent is to share the series, not to test the guard.
		staleness: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "odds_staleness_seconds",
			Help:      "Age of a price, measured from the provider's own observation instant. stage=received is the provider-attributable share.",
			Buckets:   provider.StalenessBuckets(),
		}, []string{"stage", "league", "provider"}),

		pipeline: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "pipeline_latency_seconds",
			Help:      "Age of a price measured from ingested_at: the share of staleness this system controls, as opposed to the provider's. SLO 2 reads stage=fanout.",
			Buckets:   normalizer.PipelineLatencyBuckets(),
		}, []string{"stage", "league"}),

		clockSkew: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "odds_clock_skew_total",
			Help:      "Prices whose observation instant was in the future, so the staleness observation was clamped to zero.",
		}, []string{"provider", "stage"}),
	}

	if reg == nil {
		return m, nil
	}
	for _, c := range []prometheus.Collector{
		m.connectionsActive, m.connections, m.dropped, m.resyncs, m.messagesSent,
		m.writeDelay, m.sendQueue, m.subscriptions, m.marketsTracked,
		m.busRecords, m.channelRejects, m.presenceErrors,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("wsgw metrics: %w", err)
		}
	}

	var err error
	if m.staleness, err = sharedHistogramVec(reg, m.staleness); err != nil {
		return nil, err
	}
	if m.pipeline, err = sharedHistogramVec(reg, m.pipeline); err != nil {
		return nil, err
	}
	if m.clockSkew, err = sharedCounterVec(reg, m.clockSkew); err != nil {
		return nil, err
	}
	return m, nil
}

// sharedHistogramVec registers a contract histogram that another package in this
// process may already own, and adopts the existing collector if so.
//
// AlreadyRegisteredError is returned only for an IDENTICAL descriptor. A
// disagreement about help text, label names or bucket boundaries is a different
// error and fails startup, which is the point: several packages emitting
// different stages of one SLO series must agree about the series, and the
// registry is the only place that check can be mechanical.
//
// The order of construction must not matter, and metrics_test.go asserts it in
// both directions. internal/ingest learned that the hard way — one package
// registered directly and treated AlreadyRegisteredError as a failure, so
// reversing two lines in cmd/ingest killed the process at startup with nothing
// in the type system saying so.
func sharedHistogramVec(reg prometheus.Registerer, c *prometheus.HistogramVec) (*prometheus.HistogramVec, error) {
	existing, err := shared(reg, c)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return c, nil
	}
	v, ok := existing.(*prometheus.HistogramVec)
	if !ok {
		return nil, fmt.Errorf("wsgw metrics: a collector of type %T is already registered "+
			"where a *prometheus.HistogramVec was expected", existing)
	}
	return v, nil
}

// sharedCounterVec is sharedHistogramVec for a counter.
func sharedCounterVec(reg prometheus.Registerer, c *prometheus.CounterVec) (*prometheus.CounterVec, error) {
	existing, err := shared(reg, c)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return c, nil
	}
	v, ok := existing.(*prometheus.CounterVec)
	if !ok {
		return nil, fmt.Errorf("wsgw metrics: a collector of type %T is already registered "+
			"where a *prometheus.CounterVec was expected", existing)
	}
	return v, nil
}

// shared returns the already-registered collector, or nil when c was registered.
func shared(reg prometheus.Registerer, c prometheus.Collector) (prometheus.Collector, error) {
	err := reg.Register(c)
	if err == nil {
		return nil, nil
	}
	var already prometheus.AlreadyRegisteredError
	if errors.As(err, &already) {
		return already.ExistingCollector, nil
	}
	return nil, fmt.Errorf("wsgw metrics: %w", err)
}

// -----------------------------------------------------------------------------
// Observations
// -----------------------------------------------------------------------------
//
// Every method is nil-safe on the receiver, so Options.Metrics can be nil and no
// call site needs a check. Every label value is a typed constant from
// protocol.go, so no call site can spell one.

// observeConnection counts one upgrade attempt.
func (m *Metrics) observeConnection(result ConnectionResult) {
	if m == nil {
		return
	}
	m.connections.WithLabelValues(string(result)).Inc()
}

// observeConnectionOpened and observeConnectionClosed move the active gauge.
//
// They are a matched pair and the caller must pair them from a defer: the gauge
// is the HPA's input (CLAUDE.md §9), so a leaked increment does not merely
// mis-report, it scales the deployment up and keeps it there.
func (m *Metrics) observeConnectionOpened() {
	if m == nil {
		return
	}
	m.connectionsActive.Inc()
}

// observeConnectionClosed is the other half of observeConnectionOpened.
func (m *Metrics) observeConnectionClosed() {
	if m == nil {
		return
	}
	m.connectionsActive.Dec()
}

// observeDrop counts one connection taken down.
func (m *Metrics) observeDrop(reason DropReason) {
	if m == nil {
		return
	}
	m.dropped.WithLabelValues(string(reason)).Inc()
}

// observeResync counts one resync instruction.
func (m *Metrics) observeResync(reason ResyncReason) {
	if m == nil {
		return
	}
	m.resyncs.WithLabelValues(string(reason)).Inc()
}

// observeSent counts one frame enqueued to one client.
func (m *Metrics) observeSent(kind MessageKind) {
	if m == nil {
		return
	}
	m.messagesSent.WithLabelValues(string(kind)).Inc()
}

// observeSentN counts n frames of one kind enqueued — the fanout path, which
// enqueues the same body to many connections and would otherwise pay a map
// lookup per subscriber.
func (m *Metrics) observeSentN(kind MessageKind, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.messagesSent.WithLabelValues(string(kind)).Add(float64(n))
}

// observeWriteDelay records the enqueue-to-socket delay for one frame.
func (m *Metrics) observeWriteDelay(d time.Duration) {
	if m == nil {
		return
	}
	m.writeDelay.Observe(d.Seconds())
}

// observeQueueDepth records a send queue's depth at enqueue.
func (m *Metrics) observeQueueDepth(depth int) {
	if m == nil {
		return
	}
	m.sendQueue.Observe(float64(depth))
}

// observeSubscriptions publishes the replica-wide subscription count.
func (m *Metrics) observeSubscriptions(n int) {
	if m == nil {
		return
	}
	m.subscriptions.Set(float64(n))
}

// observeMarketsTracked publishes the slate size.
func (m *Metrics) observeMarketsTracked(n int) {
	if m == nil {
		return
	}
	m.marketsTracked.Set(float64(n))
}

// observeBusRecord counts one fold outcome.
func (m *Metrics) observeBusRecord(result string) {
	if m == nil {
		return
	}
	m.busRecords.WithLabelValues(result).Inc()
}

// observeChannelReject counts one refused channel.
func (m *Metrics) observeChannelReject(reason RejectReason) {
	if m == nil {
		return
	}
	m.channelRejects.WithLabelValues(string(reason)).Inc()
}

// observePresenceError counts one failed Redis subscription-state operation.
func (m *Metrics) observePresenceError(op PresenceOp) {
	if m == nil {
		return
	}
	m.presenceErrors.WithLabelValues(string(op)).Inc()
}

// observeFanout records the two staleness quantities for a market that has just
// been handed to at least one client's send queue.
//
// # It is observed ONCE PER FANOUT EVENT, not once per subscriber
//
// A delta on a popular market reaches thousands of connections. Observing per
// subscriber would put thousands of IDENTICAL samples into the histogram and
// weight it by MARKET POPULARITY, which is not the quantity the SLO is about:
// the dashboard's question is "how old is a price when it reaches a client", and
// the answer must not change because more people opened the same page. It is the
// same argument internal/ingest/normalizer's observePublished already makes for
// observing pipeline latency once per record rather than once per book, and the
// same reason it refuses to observe staleness once per record — both are about
// choosing the unit the metric is a statement about.
//
// The caller must therefore invoke this ONCE per publish, and ONLY when at least
// one client actually received the frame. A market nobody is subscribed to is
// not a fanout event; counting it would fill the histogram with samples that
// never reached a socket and would make the SLO a statement about the bus rather
// than about the clients.
//
// # Staleness is per PRICE; pipeline latency is per RECORD
//
// The dashboard defines freshness as "the instant the price is written to the
// client socket − observed_at carried on THAT PRICE", so every quote on the
// record contributes its own sample. Observing once with the newest instant
// would report the freshest book's age for every book on the market, which is
// the number that flatters the pipeline most.
//
// ingested_at, by contrast, is a property of the PAYLOAD rather than of a quote,
// so N identical samples would only weight the histogram by book count.
//
// # `at` is the enqueue instant, and write_delay_seconds is why that is honest
//
// The observation is taken where the hub hands the frame to the send queues, not
// at the syscall, because the syscall happens on another goroutine at a time the
// publisher cannot see. sharpline_ws_write_delay_seconds measures the difference,
// so the gap between "what we measured" and "what the client received" is itself
// a series rather than an assumption.
//
// # Negative staleness is clamped AND counted, never swallowed
//
// A provider may stamp an observation instant slightly in the future.
// domain.Price.Age returns the negative duration deliberately so "a monitor can
// detect the skew instead of silently reporting healthy staleness". A histogram
// destroys that signal — a negative sample lands in the lowest bucket and reads
// as EXCELLENT — so the contract is: clamp at 0 and increment
// sharpline_odds_clock_skew_total. ProviderClockSkewDetected alerts on the
// counter, so the clamp is never silent.
func (m *Metrics) observeFanout(c pricing.ComputedMarket, at time.Time) {
	if m == nil {
		return
	}
	league := c.League.ID
	skew := 0
	for _, b := range c.Books {
		for _, q := range b.Quotes {
			if q.ObservedAt.IsZero() {
				continue
			}
			age := at.Sub(q.ObservedAt).Seconds()
			if age < 0 {
				skew++
				age = 0
			}
			m.staleness.WithLabelValues(provider.StageFanout, league, c.Provider).Observe(age)
		}
	}
	if skew > 0 {
		m.clockSkew.WithLabelValues(c.Provider, provider.StageFanout).Add(float64(skew))
	}
	if !c.IngestedAt.IsZero() {
		lat := at.Sub(c.IngestedAt).Seconds()
		if lat < 0 {
			lat = 0
		}
		m.pipeline.WithLabelValues(provider.StageFanout, league).Observe(lat)
	}
}
