// Shared client plumbing: the options every bus client takes, the franz-go
// client they are turned into, the slog adapter for franz-go's own logging, and
// the startup readiness gate.
//
// Everything here is deliberately boring and shared, so that a producer and a
// consumer in the same process cannot disagree about dial timeouts, metadata
// age, retry budgets or how a connectivity failure is classified.
package kafka

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Client geometry defaults. Each is explained at its use site in
// ClientOptions.
const (
	// DefaultDialTimeout bounds one TCP dial and handshake. franz-go's own
	// default is 20s, which is long enough that a dead broker looks like a hung
	// service; 10s is still generous for a container network.
	DefaultDialTimeout = 10 * time.Second

	// DefaultRequestRetries bounds how many times franz-go retries one request
	// against retryable errors. Kept at franz-go's default: the value that
	// actually bounds a produce is RecordDeliveryTimeout, set per producer
	// profile.
	DefaultRequestRetries = 20

	// DefaultRetryTimeout bounds the total time spent retrying one request.
	DefaultRetryTimeout = 30 * time.Second

	// DefaultMetadataMaxAge is how long cluster metadata is trusted. Shorter
	// than franz-go's 5 minutes on purpose: topics are created by Terraform
	// (CLAUDE.md §9), so a `terraform apply` during development is followed by a
	// service that has to notice a topic appearing. franz-go also forces a
	// refresh on UNKNOWN_TOPIC_OR_PARTITION, so this is the ceiling on the wait,
	// not the wait.
	DefaultMetadataMaxAge = time.Minute

	// DefaultMetadataMinAge floors the refresh rate, so a topic that stays
	// unknown cannot turn into a metadata request loop.
	DefaultMetadataMinAge = 5 * time.Second

	// DefaultProbeTimeout bounds the readiness round trip. Deliberately below
	// httpx.DefaultReadinessTimeout (3s) so /readyz names `kafka` as the failing
	// check instead of the whole probe timing out with no detail.
	DefaultProbeTimeout = 2 * time.Second
)

// Startup retry defaults. Worst case with these values is
// 0.25+0.5+1+2+4+5+5 = 17.75s of backoff plus up to 8 probe timeouts.
//
// Sized for Kubernetes rather than for compose. Compose gates every service on
// `kafka` being service_healthy so the race barely exists there; a StatefulSet
// has no such gate, and a KRaft broker that has not yet elected a controller
// answers metadata requests with an empty cluster.
const (
	// DefaultConnectAttempts counts the first try, so N attempts means N-1
	// retries.
	DefaultConnectAttempts = 8
	// DefaultConnectBackoff is the first sleep; it doubles per attempt.
	DefaultConnectBackoff = 250 * time.Millisecond
	// DefaultConnectBackoffMax caps the doubling.
	DefaultConnectBackoffMax = 5 * time.Second
)

// checkerName is the identifier every bus client reports in the /readyz
// payload. All of them report the same name because they all mean the same
// thing: this process can reach the cluster.
const checkerName = "kafka"

// ClientOptions is the configuration every bus client shares. It is embedded in
// ProducerOptions, ConsumerOptions and SnapshotOptions.
//
// Nothing here is read from the process environment: configuration is loaded
// once by internal/platform/config and injected (CLAUDE.md §12).
type ClientOptions struct {
	// Brokers is SHARPLINE_KAFKA_BROKERS, already split and validated by
	// internal/platform/config (Config.KafkaBrokers). Required.
	Brokers []string

	// Service is the binary name. It becomes the Kafka client id — which is
	// what appears in the broker's request logs and in kafka-ui's client list —
	// and the `producer` field of every envelope this client writes. Required.
	Service string

	// Logger receives lifecycle events and franz-go's own internal logging.
	// Required: a bus client that cannot report its own rebalances is a bus
	// client whose failures are invisible.
	Logger *slog.Logger

	// Metrics is the shared collector set. nil builds an unregistered set, which
	// is correct for a unit test; a service serving /metrics must call NewMetrics
	// once and pass the same value to every client in the process.
	Metrics *Metrics

	// TracerProvider supplies the tracer. nil uses the OTel global provider,
	// which is a no-op until a cmd/ entrypoint installs a real one — so an
	// un-instrumented binary costs nothing and a traced one needs no change here.
	TracerProvider trace.TracerProvider

	// Propagator serialises trace context into record headers. nil uses W3C
	// trace context DIRECTLY rather than the OTel global, because the global
	// defaults to a no-op and a no-op propagator produces traces that stop at
	// every hop while still emitting spans — see otel.go.
	Propagator propagation.TextMapPropagator

	// ClientID overrides the Kafka client id. Defaults to Service, or to
	// "sharpline-<Service>" is NOT used: prometheus.yml already labels by
	// service and a second prefix only makes the broker log wider.
	ClientID string

	// FranzLogLevel bounds how much of franz-go's internal logging is forwarded
	// to Logger. Zero means kgo.LogLevelInfo, which logs metadata refreshes,
	// group joins and request failures. kgo.LogLevelDebug is extremely verbose —
	// a line per request — and is for diagnosing a rebalance, not for running.
	FranzLogLevel kgo.LogLevel

	// Timeouts and budgets. Zero means the corresponding Default* constant.
	DialTimeout     time.Duration
	RequestRetries  int
	RetryTimeout    time.Duration
	MetadataMaxAge  time.Duration
	MetadataMinAge  time.Duration
	ProbeTimeout    time.Duration
	ConnectAttempts int

	ConnectBackoff    time.Duration
	ConnectBackoffMax time.Duration

	// SkipStartupProbe skips the connectivity gate in the constructor.
	//
	// The gate is on by default because a client that returns successfully and
	// then cannot reach the cluster pushes the failure into the first produce,
	// where it is attributed to whatever business operation happened to be
	// first. Skipping it is for a binary that must start while the bus is down
	// and degrade rather than crash-loop — which no current service does, so
	// this is off.
	SkipStartupProbe bool
}

// validate checks the shared options.
func (o ClientOptions) validate() error {
	switch {
	case len(o.Brokers) == 0:
		return fmt.Errorf("%w: Brokers is empty", ErrInvalidOptions)
	case o.Service == "":
		return fmt.Errorf("%w: Service is empty", ErrInvalidOptions)
	case o.Logger == nil:
		return fmt.Errorf("%w: Logger is nil", ErrInvalidOptions)
	}
	for i, b := range o.Brokers {
		if strings.TrimSpace(b) == "" {
			return fmt.Errorf("%w: Brokers[%d] is empty", ErrInvalidOptions, i)
		}
		if _, _, err := net.SplitHostPort(b); err != nil {
			return fmt.Errorf("%w: Brokers[%d]=%q is not host:port: %w", ErrInvalidOptions, i, b, err)
		}
	}
	return nil
}

// clientID resolves the Kafka client id.
func (o ClientOptions) clientID() string {
	if o.ClientID != "" {
		return o.ClientID
	}
	return o.Service
}

// metrics resolves the collector set, building an unregistered one when the
// caller supplied none. Returning a non-nil value unconditionally is what lets
// every observe call site skip a nil check.
func (o ClientOptions) resolveMetrics() (*Metrics, error) {
	if o.Metrics != nil {
		return o.Metrics, nil
	}
	return NewMetrics(nil)
}

// tracer resolves the tracer.
func (o ClientOptions) tracer() trace.Tracer {
	tp := o.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	return tp.Tracer(tracerName)
}

// propagator resolves the trace-context propagator.
func (o ClientOptions) propagator() propagation.TextMapPropagator {
	if o.Propagator != nil {
		return o.Propagator
	}
	return defaultPropagator()
}

// baseOpts builds the franz-go options every client shares.
func (o ClientOptions) baseOpts() []kgo.Opt {
	level := o.FranzLogLevel
	if level == kgo.LogLevelNone {
		level = kgo.LogLevelInfo
	}
	return []kgo.Opt{
		kgo.SeedBrokers(o.Brokers...),
		kgo.ClientID(o.clientID()),
		kgo.WithLogger(&slogAdapter{log: o.Logger, level: level}),
		kgo.DialTimeout(positiveOr(o.DialTimeout, DefaultDialTimeout)),
		kgo.RequestRetries(positiveIntOr(o.RequestRetries, DefaultRequestRetries)),
		kgo.RetryTimeout(positiveOr(o.RetryTimeout, DefaultRetryTimeout)),
		kgo.MetadataMaxAge(positiveOr(o.MetadataMaxAge, DefaultMetadataMaxAge)),
		kgo.MetadataMinAge(positiveOr(o.MetadataMinAge, DefaultMetadataMinAge)),
	}
}

// -----------------------------------------------------------------------------
// franz-go logging
// -----------------------------------------------------------------------------

// slogAdapter forwards franz-go's internal logging into log/slog.
//
// franz-go logs a great deal that matters here and nowhere else — group joins,
// generation numbers, partition assignments, coordinator moves, produce retries
// — and CLAUDE.md §9 mandates structured JSON logging with trace correlation.
// Discarding it would mean debugging a rebalance from metrics alone.
type slogAdapter struct {
	log   *slog.Logger
	level kgo.LogLevel
}

// Level implements kgo.Logger.
func (a *slogAdapter) Level() kgo.LogLevel { return a.level }

// Log implements kgo.Logger. franz-go passes alternating key/value pairs, which
// is exactly slog's variadic form, so they pass through unchanged.
func (a *slogAdapter) Log(level kgo.LogLevel, msg string, keyvals ...any) {
	// franz-go's message text is a sentence fragment ("immediate metadata
	// update triggered"); prefixing keeps it attributable in a merged log
	// stream without inventing a structured field for it.
	a.log.Log(context.Background(), slogLevel(level), "kafka: "+msg, keyvals...)
}

// slogLevel maps franz-go's levels onto slog's.
//
// kgo.LogLevelInfo maps to slog.LevelDebug rather than to Info, deliberately.
// franz-go's info level includes every metadata refresh and every group
// heartbeat outcome, which at the default 15s scrape cadence and a one-minute
// metadata age is a steady drip of lines that would drown this system's own
// logging. The information is kept — it moves to debug, where
// SHARPLINE_LOG_LEVEL=debug turns it on for exactly the session that needs it.
func slogLevel(level kgo.LogLevel) slog.Level {
	switch level {
	case kgo.LogLevelError:
		return slog.LevelError
	case kgo.LogLevelWarn:
		return slog.LevelWarn
	case kgo.LogLevelInfo, kgo.LogLevelDebug:
		return slog.LevelDebug
	default:
		return slog.LevelDebug
	}
}

// -----------------------------------------------------------------------------
// Startup readiness
// -----------------------------------------------------------------------------

// probe performs one connectivity round trip and records it.
//
// kgo.Client.Ping sends an ApiVersions request to a broker, which requires a
// live connection and no topic, so it proves reachability without depending on
// Terraform having applied anything.
func probe(ctx context.Context, cl *kgo.Client, m *Metrics, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	err := cl.Ping(ctx)
	m.observeProbe(time.Since(start), err)
	return err
}

// awaitReady proves connectivity before a constructor returns, retrying
// transient failures with exponential backoff and equal jitter.
//
// # What is retried, and what is not
//
// Only the probe. It is idempotent by construction — an ApiVersions request
// mutates nothing — so retrying it cannot apply anything twice.
//
// There is deliberately no exported retry helper in this package, for the same
// reason internal/platform/postgres refuses to provide one: a caller's function
// may have produced to Kafka, written to Postgres or mutated in-memory state
// before it failed, and only the caller knows whether re-running all of that is
// safe. franz-go already retries at the level where retrying IS safe — one
// request, bounded by RetryTimeout, with idempotent production making a
// duplicate produce impossible.
func awaitReady(ctx context.Context, cl *kgo.Client, opts ClientOptions, m *Metrics, log *slog.Logger) error {
	attempts := positiveIntOr(opts.ConnectAttempts, DefaultConnectAttempts)
	backoff := positiveOr(opts.ConnectBackoff, DefaultConnectBackoff)
	backoffMax := positiveOr(opts.ConnectBackoffMax, DefaultConnectBackoffMax)
	timeout := positiveOr(opts.ProbeTimeout, DefaultProbeTimeout)

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("kafka: connect aborted after %d attempt(s): %w",
				attempt-1, errors.Join(err, lastErr))
		}

		err := probe(ctx, cl, m, timeout)
		if err == nil {
			m.observeConnectAttempt(connectOK)
			return nil
		}
		lastErr = err

		if !IsTransientClusterError(err) {
			m.observeConnectAttempt(connectFatal)
			log.Error("kafka unreachable and the failure is not transient; not retrying",
				slog.Int("attempt", attempt),
				slog.String("code", errorCode(err)),
				slog.String("error", err.Error()),
			)
			return fmt.Errorf("kafka: connect: %w", err)
		}

		m.observeConnectAttempt(connectRetryable)
		if attempt == attempts {
			break
		}

		delay := jitter(backoffFor(backoff, backoffMax, attempt))
		log.Warn("kafka unreachable, retrying",
			slog.Int("attempt", attempt),
			slog.Int("of", attempts),
			slog.String("code", errorCode(err)),
			slog.String("retry_in", delay.String()),
			slog.String("error", err.Error()),
		)
		if err := sleep(ctx, delay); err != nil {
			return fmt.Errorf("kafka: connect aborted while backing off: %w",
				errors.Join(err, lastErr))
		}
	}
	return fmt.Errorf("%w after %d attempt(s): %w", ErrUnavailable, attempts, lastErr)
}

// IsTransientClusterError reports whether err is a failure to reach or stay
// connected to the cluster that is expected to succeed on a later attempt.
//
// It answers ONE question — "should the connectivity probe be tried again?" —
// and it deliberately does not report on business failures. A produce that was
// rejected because the topic does not exist is not transient: Terraform owns
// topic creation and the broker has auto-creation disabled, so waiting cannot
// fix it and retrying only delays the error that names the real cause.
func IsTransientClusterError(err error) bool {
	if err == nil {
		return false
	}

	// A Kafka-protocol error decides on its own retriable flag, which the
	// protocol itself defines per error code. errors.As walks the chain.
	var kerrErr *kerr.Error
	if errors.As(err, &kerrErr) {
		return kerrErr.Retriable
	}

	// The caller gave up. Respect the cancellation.
	if errors.Is(err, context.Canceled) {
		return false
	}
	// A deadline that expired is a dial or request that did not finish in time.
	// Per-attempt deadlines are how ProbeTimeout is spelled, and awaitReady
	// checks the parent context separately before each attempt, so this cannot
	// loop past a caller-imposed deadline.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// The client is gone. Nothing to retry. net.ErrClosed belongs in this family
	// rather than with the socket failures below: "use of closed network
	// connection" is reported when THIS process closed the descriptor, so it
	// describes our own shutdown and not a broker that went away. The match is on
	// the sentinel's identity, so an unrelated error that merely carries the same
	// message text is classified by the rules below like any other socket failure.
	if errors.Is(err, kgo.ErrClientClosed) || errors.Is(err, ErrClosed) || errors.Is(err, net.ErrClosed) {
		return false
	}

	// Socket-level failures, before any request completed.
	for _, e := range []error{
		syscall.ECONNREFUSED, // nothing listening yet — the compose/k8s startup race
		syscall.ECONNRESET,
		syscall.ECONNABORTED,
		syscall.EPIPE,
		syscall.EHOSTUNREACH,
		syscall.ENETUNREACH,
		syscall.ENETDOWN,
		syscall.ETIMEDOUT,
	} {
		if errors.Is(err, e) {
			return true
		}
	}

	// Truncated handshake: the broker accepted the TCP connection and went away
	// before answering. Seen every time Kafka is mid-startup, because the KRaft
	// listener binds before the controller has a quorum.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// Service DNS that has not propagated yet. In Kubernetes a Service name
	// resolves only once the pod has an endpoint, so this is a normal startup
	// state — a permanently wrong hostname simply exhausts the retry budget and
	// then fails loudly.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	// A net.Error that timed out is transient, and a timeout is the only thing
	// this branch may conclude.
	//
	// It used to `return netErr.Timeout()`, and that return was a bug with two
	// heads. *net.OpError itself implements net.Error, so this branch matched every
	// OpError and the *net.OpError branch below could never run — dead code. Worse,
	// the classification it produced was backwards where it mattered most: a socket
	// failure that is NOT a timeout — a connection reset by a broker that is
	// restarting, a refused dial whose errno is not in the list above, an aborted
	// TLS handshake — has Timeout() == false, so it was declared FATAL and the
	// connect loop gave up on exactly the condition the loop exists to ride out. A
	// non-timeout is not evidence of permanence; it is the absence of evidence, so
	// this branch now decides only the case it can decide and lets everything else
	// fall through.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	// Any other failure of a network operation against a broker. Reaching here
	// means the operation failed for a reason none of the branches above named,
	// and on a Kafka endpoint the overwhelming cause of that is a broker that is
	// starting, rolling or restarting — so it is worth another attempt inside the
	// bounded budget. Non-OpError net.Errors stop here and stay fatal:
	// *net.AddrError and net.UnknownNetworkError describe a malformed address,
	// which no amount of waiting repairs.
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

// -----------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------

// closedFlag makes a client's methods safe to call from a goroutine that races
// Close.
type closedFlag struct{ v atomic.Bool }

func (f *closedFlag) set() bool   { return f.v.CompareAndSwap(false, true) }
func (f *closedFlag) isSet() bool { return f.v.Load() }
func (f *closedFlag) err() error {
	if f.isSet() {
		return ErrClosed
	}
	return nil
}

// positiveOr resolves a non-positive duration to its default.
func positiveOr(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}

// positiveIntOr resolves a non-positive count to its default.
func positiveIntOr(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

// backoffFor returns base * 2^(attempt-1), capped, without overflowing.
func backoffFor(base, max time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt; i++ {
		if d >= max/2 {
			return max
		}
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}

// jitter applies equal jitter: half the delay is fixed, half is random. Full
// jitter would sometimes return near-zero and hammer a broker that is still
// starting; no jitter would make every replica retry in the same millisecond.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// sleep waits for d or until ctx is done, whichever comes first.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
