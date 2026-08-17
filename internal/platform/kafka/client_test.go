package kafka

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/propagation"
)

// discardLogger is the logger these tests inject. ClientOptions.validate requires
// a non-nil one — "a bus client that cannot report its own rebalances is a bus
// client whose failures are invisible" — so every valid options value needs one.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// validOptions is the smallest ClientOptions that passes validation. Tests mutate
// a copy of it, so each case states exactly one thing that is wrong.
func validOptions() ClientOptions {
	return ClientOptions{
		Brokers: []string{"kafka:9092"},
		Service: "ingest",
		Logger:  discardLogger(),
	}
}

// TestClientOptionsValidate covers the startup-time configuration gate.
//
// CLAUDE.md §12: "Config via environment variables with a typed struct and
// startup validation — fail fast and loudly on a bad config." Everything here
// fails BEFORE any network I/O, which is what makes a misconfiguration
// distinguishable from a broker that is down.
func TestClientOptionsValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*ClientOptions)
		valid  bool
		why    string
	}{
		{name: "the minimal valid options", mutate: func(*ClientOptions) {}, valid: true},
		{
			name: "several brokers",
			mutate: func(o *ClientOptions) {
				o.Brokers = []string{"kafka-0.kafka:9092", "kafka-1.kafka:9092", "kafka-2.kafka:9092"}
			},
			valid: true,
		},
		{
			name:   "an IPv6 broker in brackets",
			mutate: func(o *ClientOptions) { o.Brokers = []string{"[::1]:9092"} },
			valid:  true,
		},

		{
			name:   "no brokers",
			mutate: func(o *ClientOptions) { o.Brokers = nil },
			why:    "SHARPLINE_KAFKA_BROKERS was unset or empty",
		},
		{
			name:   "an empty broker list",
			mutate: func(o *ClientOptions) { o.Brokers = []string{} },
			why:    "same, after splitting",
		},
		{
			name:   "a whitespace-only broker",
			mutate: func(o *ClientOptions) { o.Brokers = []string{"kafka:9092", "   "} },
			why:    "a trailing comma in the env var produces exactly this",
		},
		{
			name:   "a broker with no port",
			mutate: func(o *ClientOptions) { o.Brokers = []string{"kafka"} },
			why:    "franz-go would default the port silently; failing here names the real cause",
		},
		{
			name:   "a broker with a scheme",
			mutate: func(o *ClientOptions) { o.Brokers = []string{"kafka://kafka:9092"} },
			why:    "host:port, not a URL",
		},
		{
			name:   "a bare IPv6 address without brackets",
			mutate: func(o *ClientOptions) { o.Brokers = []string{"::1:9092"} },
			why:    "ambiguous; SplitHostPort rejects it",
		},
		{
			name:   "no service name",
			mutate: func(o *ClientOptions) { o.Service = "" },
			why:    "the service name is the client id AND the envelope's producer field",
		},
		{
			name:   "no logger",
			mutate: func(o *ClientOptions) { o.Logger = nil },
			why:    "rebalances would be invisible",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := validOptions()
			tc.mutate(&opts)

			err := opts.validate()
			if tc.valid {
				if err != nil {
					t.Fatalf("validate() = %v, want ok", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate() = ok, want an error (%s)", tc.why)
			}
			if !errors.Is(err, ErrInvalidOptions) {
				t.Errorf("validate() = %v, want it to wrap ErrInvalidOptions", err)
			}
		})
	}
}

// TestClientOptionsBrokerErrorNamesTheIndex checks that a bad broker in a list is
// attributable.
//
// With three brokers in an env var, "not host:port" without an index is a
// message that requires the reader to re-derive which one.
func TestClientOptionsBrokerErrorNamesTheIndex(t *testing.T) {
	t.Parallel()

	opts := validOptions()
	opts.Brokers = []string{"kafka-0:9092", "kafka-1:9092", "kafka-2"}

	err := opts.validate()
	if err == nil {
		t.Fatal("validate() = ok, want an error")
	}
	if got := err.Error(); !strings.Contains(got, "Brokers[2]") || !strings.Contains(got, "kafka-2") {
		t.Errorf("error %q names neither the index nor the value", got)
	}
}

// TestClientOptionsResolution covers the defaulting the constructors rely on.
func TestClientOptionsResolution(t *testing.T) {
	t.Parallel()

	t.Run("the client id defaults to the service name", func(t *testing.T) {
		t.Parallel()
		opts := validOptions()
		if got, want := opts.clientID(), "ingest"; got != want {
			t.Errorf("clientID() = %q, want %q", got, want)
		}
	})

	t.Run("an explicit client id wins", func(t *testing.T) {
		t.Parallel()
		opts := validOptions()
		opts.ClientID = "ingest-canary"
		if got, want := opts.clientID(), "ingest-canary"; got != want {
			t.Errorf("clientID() = %q, want %q", got, want)
		}
	})

	t.Run("nil metrics build an unregistered set rather than a nil pointer", func(t *testing.T) {
		t.Parallel()
		// Returning a non-nil value unconditionally is what lets every observe
		// call site skip a nil check. If this ever returned nil, the first
		// produce in a test would panic instead of being measured.
		opts := validOptions()
		m, err := opts.resolveMetrics()
		if err != nil {
			t.Fatalf("resolveMetrics() = %v", err)
		}
		if m == nil {
			t.Fatal("resolveMetrics() = nil")
		}
		// The collectors must be live, not merely allocated.
		m.observeProbe(time.Millisecond, nil)
		m.observeConnectAttempt(connectOK)
	})

	t.Run("injected metrics are returned unchanged", func(t *testing.T) {
		t.Parallel()
		injected, err := NewMetrics(nil)
		if err != nil {
			t.Fatalf("NewMetrics: %v", err)
		}
		opts := validOptions()
		opts.Metrics = injected
		got, err := opts.resolveMetrics()
		if err != nil {
			t.Fatalf("resolveMetrics() = %v", err)
		}
		if got != injected {
			t.Error("resolveMetrics() built a second Metrics instead of returning the injected one; " +
				"two Metrics on one registry means two halves of a process reporting under one series")
		}
	})

	t.Run("the propagator defaults to W3C trace context, not the OTel global", func(t *testing.T) {
		t.Parallel()
		// This is the divergence otel.go argues for at length: the OTel global
		// propagator is a no-op until an entrypoint installs one, and a no-op
		// propagator produces spans that merely fail to join up — a failure
		// indistinguishable from tracing being switched off.
		opts := validOptions()
		if _, ok := opts.propagator().(propagation.TraceContext); !ok {
			t.Errorf("propagator() = %T, want propagation.TraceContext", opts.propagator())
		}
	})

	t.Run("an injected propagator wins", func(t *testing.T) {
		t.Parallel()
		opts := validOptions()
		opts.Propagator = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
		if _, ok := opts.propagator().(propagation.TraceContext); ok {
			t.Error("propagator() returned the default despite an injected one")
		}
	})

	t.Run("a tracer is always available", func(t *testing.T) {
		t.Parallel()
		opts := validOptions()
		if opts.tracer() == nil {
			t.Fatal("tracer() = nil; an un-instrumented binary must get a no-op tracer, not a panic")
		}
	})

	t.Run("baseOpts is buildable and non-empty", func(t *testing.T) {
		t.Parallel()
		// kgo.Opt is opaque, so this asserts only that every shared option is
		// constructed without panicking and that the set is not accidentally
		// empty — which would silently drop the dial timeout and the metadata
		// ages onto franz-go's much longer defaults.
		opts := validOptions()
		if got := len(opts.baseOpts()); got < 7 {
			t.Errorf("baseOpts() returned %d options, want at least 7", got)
		}
		opts.FranzLogLevel = kgo.LogLevelDebug
		if got := len(opts.baseOpts()); got < 7 {
			t.Errorf("baseOpts() with an explicit log level returned %d options", got)
		}
	})
}

// TestSlogLevelMapping pins franz-go's info level onto slog's DEBUG.
//
// It is deliberate and it is easy to "fix" by mistake. franz-go's info level
// includes every metadata refresh and every group heartbeat outcome; at a
// one-minute metadata age that is a steady drip that would drown this system's
// own logging. The information is not discarded — it moves to debug, where
// SHARPLINE_LOG_LEVEL=debug turns it on for exactly the session that needs it.
func TestSlogLevelMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   kgo.LogLevel
		want slog.Level
	}{
		{"error stays error", kgo.LogLevelError, slog.LevelError},
		{"warn stays warn", kgo.LogLevelWarn, slog.LevelWarn},
		{"info is demoted to debug", kgo.LogLevelInfo, slog.LevelDebug},
		{"debug stays debug", kgo.LogLevelDebug, slog.LevelDebug},
		{"none falls through to debug", kgo.LogLevelNone, slog.LevelDebug},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := slogLevel(tc.in); got != tc.want {
				t.Errorf("slogLevel(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestSlogAdapterForwardsKeyvals checks the franz-go logger adapter, including
// that the message is prefixed so it stays attributable in a merged log stream.
func TestSlogAdapterForwardsKeyvals(t *testing.T) {
	t.Parallel()

	var buf lineBuffer
	adapter := &slogAdapter{
		log:   slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		level: kgo.LogLevelInfo,
	}

	if got := adapter.Level(); got != kgo.LogLevelInfo {
		t.Errorf("Level() = %v, want %v", got, kgo.LogLevelInfo)
	}

	adapter.Log(kgo.LogLevelWarn, "immediate metadata update triggered", "why", "unknown topic")

	line := buf.String()
	if !strings.Contains(line, "kafka: immediate metadata update triggered") {
		t.Errorf("log line %q is missing the prefixed message", line)
	}
	if !strings.Contains(line, "why=") || !strings.Contains(line, "unknown topic") {
		t.Errorf("log line %q dropped franz-go's key/value pairs", line)
	}
	if !strings.Contains(line, "WARN") {
		t.Errorf("log line %q lost the level", line)
	}
}

// lineBuffer is a tiny io.Writer that keeps everything written to it.
type lineBuffer struct{ b []byte }

func (l *lineBuffer) Write(p []byte) (int, error) { l.b = append(l.b, p...); return len(p), nil }
func (l *lineBuffer) String() string              { return string(l.b) }

// -----------------------------------------------------------------------------
// Transient-error classification
// -----------------------------------------------------------------------------

// timeoutNetError is a net.Error whose Timeout() answer the test controls.
type timeoutNetError struct{ timeout bool }

func (e timeoutNetError) Error() string   { return fmt.Sprintf("net error (timeout=%v)", e.timeout) }
func (e timeoutNetError) Timeout() bool   { return e.timeout }
func (e timeoutNetError) Temporary() bool { return e.timeout }

// TestIsTransientClusterError is the classification that decides whether a
// startup that cannot reach Kafka retries or crashes.
//
// Getting it wrong in one direction is a crash-loop on a compose stack whose
// broker is two seconds from ready; in the other it is a service that spends its
// whole retry budget on a permanently wrong hostname and then reports the wrong
// cause. It answers ONE question — "should the connectivity probe be tried
// again?" — and deliberately says nothing about business failures.
func TestIsTransientClusterError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
		why  string
	}{
		{name: "nil", err: nil, want: false},

		{
			name: "a retriable Kafka protocol error",
			err:  &kerr.Error{Code: 5, Message: "LEADER_NOT_AVAILABLE", Retriable: true},
			want: true,
			why:  "the protocol itself defines retriability per error code; this package defers to it",
		},
		{
			name: "a non-retriable Kafka protocol error",
			err:  &kerr.Error{Code: 29, Message: "TOPIC_AUTHORIZATION_FAILED", Retriable: false},
			want: false,
		},
		{
			name: "a wrapped Kafka protocol error",
			err:  fmt.Errorf("ping: %w", &kerr.Error{Code: 6, Message: "NOT_LEADER_OR_FOLLOWER", Retriable: true}),
			want: true,
			why:  "errors.As walks the chain, so wrapping must not change the classification",
		},

		{
			name: "the caller cancelled",
			err:  context.Canceled,
			want: false,
			why:  "respect the cancellation rather than retrying past it",
		},
		{
			name: "a deadline expired",
			err:  context.DeadlineExceeded,
			want: true,
			why:  "per-attempt deadlines are how ProbeTimeout is spelled; awaitReady checks the parent context separately",
		},
		{name: "a wrapped cancellation", err: fmt.Errorf("probe: %w", context.Canceled), want: false},

		{name: "the franz-go client is closed", err: kgo.ErrClientClosed, want: false},
		{name: "this package's closed sentinel", err: ErrClosed, want: false},
		{name: "a wrapped closed sentinel", err: fmt.Errorf("publish: %w", ErrClosed), want: false},

		{
			name: "connection refused",
			err:  fmt.Errorf("dial: %w", syscall.ECONNREFUSED),
			want: true,
			why:  "nothing listening yet — the compose and StatefulSet startup race",
		},
		{name: "connection reset", err: fmt.Errorf("read: %w", syscall.ECONNRESET), want: true},
		{name: "connection aborted", err: fmt.Errorf("read: %w", syscall.ECONNABORTED), want: true},
		{name: "broken pipe", err: fmt.Errorf("write: %w", syscall.EPIPE), want: true},
		{name: "host unreachable", err: fmt.Errorf("dial: %w", syscall.EHOSTUNREACH), want: true},
		{name: "network unreachable", err: fmt.Errorf("dial: %w", syscall.ENETUNREACH), want: true},
		{name: "network down", err: fmt.Errorf("dial: %w", syscall.ENETDOWN), want: true},
		{name: "syscall timeout", err: fmt.Errorf("dial: %w", syscall.ETIMEDOUT), want: true},
		{
			name: "a syscall inside an os.SyscallError inside a net.OpError, which is what net actually returns",
			err: &net.OpError{
				Op: "dial", Net: "tcp",
				Err: os.NewSyscallError("connect", syscall.ECONNREFUSED),
			},
			want: true,
			why:  "this is the exact shape of `dial tcp 172.18.0.4:9092: connect: connection refused`",
		},

		{
			name: "a truncated handshake",
			err:  io.EOF,
			want: true,
			why:  "the KRaft listener binds before the controller has a quorum, so this is normal mid-startup",
		},
		{name: "an unexpected EOF", err: fmt.Errorf("read: %w", io.ErrUnexpectedEOF), want: true},

		{
			name: "DNS that has not propagated",
			err:  &net.DNSError{Err: "no such host", Name: "kafka", IsNotFound: true},
			want: true,
			why:  "a Service name resolves only once the pod has an endpoint; a permanently wrong host exhausts the budget and then fails loudly",
		},
		{name: "a wrapped DNS error", err: fmt.Errorf("dial: %w", &net.DNSError{Err: "server misbehaving"}), want: true},

		{name: "a net.Error that timed out", err: timeoutNetError{timeout: true}, want: true},
		{name: "a net.Error that did not time out", err: timeoutNetError{timeout: false}, want: false},

		{
			name: "an unrelated error",
			err:  errors.New("something else entirely"),
			want: false,
			why:  "an unclassified failure must not be retried; retrying only delays the error that names the real cause",
		},
		{
			name: "a business failure is not transient",
			err:  ErrInvalidTopic,
			want: false,
			why:  "Terraform owns topic creation and auto-creation is disabled, so waiting cannot fix it",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsTransientClusterError(tc.err); got != tc.want {
				t.Errorf("IsTransientClusterError(%v) = %v, want %v (%s)", tc.err, got, tc.want, tc.why)
			}
		})
	}
}

// TestIsTransientClusterErrorDefersToFranzGosOwnTable checks the delegation
// against franz-go's real error table rather than against struct literals, so
// that a franz-go upgrade that changed a retriability flag would be visible here.
func TestIsTransientClusterErrorDefersToFranzGosOwnTable(t *testing.T) {
	t.Parallel()

	// 3 = UNKNOWN_TOPIC_OR_PARTITION, 5 = LEADER_NOT_AVAILABLE,
	// 17 = INVALID_TOPIC_EXCEPTION, 29 = TOPIC_AUTHORIZATION_FAILED.
	for _, code := range []int16{3, 5, 17, 29} {
		err := kerr.ErrorForCode(code)

		var kerrErr *kerr.Error
		if !errors.As(err, &kerrErr) {
			t.Fatalf("kerr.ErrorForCode(%d) = %T, want *kerr.Error", code, err)
		}
		if got, want := IsTransientClusterError(err), kerrErr.Retriable; got != want {
			t.Errorf("IsTransientClusterError(%s) = %v, want %v (franz-go's own Retriable flag)",
				kerrErr.Message, got, want)
		}
	}
}

// TestIsTransientClusterErrorClassifiesSocketErrorsBothWays covers the branch
// ordering inside the classifier, which used to hide a defect.
//
// The classifier ended with:
//
//	var netErr net.Error
//	if errors.As(err, &netErr) { return netErr.Timeout() }
//	var opErr *net.OpError
//	return errors.As(err, &opErr)
//
// *net.OpError itself implements net.Error, so the first branch always matched
// first and the trailing *net.OpError branch was unreachable. The consequence was
// behavioural rather than cosmetic: a socket failure that is NOT a timeout and not
// one of the enumerated syscalls returned Timeout() == false and was therefore
// classified FATAL, so the connect loop refused to retry precisely the condition a
// restarting broker produces.
//
// An earlier revision of this test pinned that behaviour and said the expectation
// would flip if the final branch were ever made reachable. It has been, so it has.
// Both directions are asserted here, because a fix that made everything transient
// would be the opposite error — a service burning its whole budget on a
// permanently wrong address.
func TestIsTransientClusterErrorClassifiesSocketErrorsBothWays(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
		why  string
	}{
		{
			name: "a timed-out socket operation",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ETIMEDOUT)},
			want: true,
			why:  "a timeout against a broker is the canonical retry-me failure",
		},
		{
			name: "a socket operation that failed without timing out",
			err:  &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")},
			want: true,
			why:  "Timeout() is false here; a restarting broker is the overwhelming cause and the loop exists to ride it out",
		},
		{
			name: "a wrapped non-timeout socket operation",
			err: fmt.Errorf("probe: %w",
				&net.OpError{Op: "write", Net: "tcp", Err: errors.New("tls: handshake failure")}),
			want: true,
			why:  "errors.As walks the chain, so wrapping must not change the classification",
		},
		{
			name: "our own closed connection",
			err:  &net.OpError{Op: "read", Net: "tcp", Err: net.ErrClosed},
			want: false,
			why:  "net.ErrClosed means this process closed the descriptor; that is shutdown, not an unreachable broker",
		},
		{
			name: "a malformed address",
			err:  &net.AddrError{Err: "missing port in address", Addr: "kafka"},
			want: false,
			why:  "a net.Error that is not an *net.OpError; no amount of waiting repairs a bad address",
		},
		{
			name: "an unknown network",
			err:  net.UnknownNetworkError("quic"),
			want: false,
			why:  "same shape as the address error: permanent configuration, not connectivity",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsTransientClusterError(tc.err); got != tc.want {
				t.Errorf("IsTransientClusterError(%v) = %v, want %v (%s)", tc.err, got, tc.want, tc.why)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Backoff, jitter and the small helpers
// -----------------------------------------------------------------------------

// TestPositiveOr covers the "zero means the default" convention every timeout
// field in ClientOptions relies on.
func TestPositiveOr(t *testing.T) {
	t.Parallel()

	t.Run("durations", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name     string
			in       time.Duration
			fallback time.Duration
			want     time.Duration
		}{
			{"zero takes the default", 0, DefaultDialTimeout, DefaultDialTimeout},
			{"negative takes the default", -time.Second, DefaultDialTimeout, DefaultDialTimeout},
			{"a positive value is kept", 2 * time.Second, DefaultDialTimeout, 2 * time.Second},
			{"one nanosecond is positive", time.Nanosecond, DefaultDialTimeout, time.Nanosecond},
		}
		for _, tc := range tests {
			if got := positiveOr(tc.in, tc.fallback); got != tc.want {
				t.Errorf("%s: positiveOr(%v, %v) = %v, want %v", tc.name, tc.in, tc.fallback, got, tc.want)
			}
		}
	})

	t.Run("counts", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name     string
			in       int
			fallback int
			want     int
		}{
			{"zero takes the default", 0, DefaultConnectAttempts, DefaultConnectAttempts},
			{"negative takes the default", -3, DefaultConnectAttempts, DefaultConnectAttempts},
			{"a positive value is kept", 1, DefaultConnectAttempts, 1},
		}
		for _, tc := range tests {
			if got := positiveIntOr(tc.in, tc.fallback); got != tc.want {
				t.Errorf("%s: positiveIntOr(%d, %d) = %d, want %d", tc.name, tc.in, tc.fallback, got, tc.want)
			}
		}
	})
}

// TestBackoffForDoublesAndCaps checks the exponential schedule, including the
// overflow guard.
//
// The schedule is quoted in client.go's own comment as
// 0.25+0.5+1+2+4+5+5 = 17.75s over the default eight attempts, and that number is
// what makes the default budget defensible against a KRaft broker that has not
// yet elected a controller. It is asserted rather than trusted.
func TestBackoffForDoublesAndCaps(t *testing.T) {
	t.Parallel()

	base, max := DefaultConnectBackoff, DefaultConnectBackoffMax

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 250 * time.Millisecond},
		{2, 500 * time.Millisecond},
		{3, time.Second},
		{4, 2 * time.Second},
		{5, 4 * time.Second},
		{6, max},
		{7, max},
		{8, max},
	}

	var total time.Duration
	for _, tc := range tests {
		got := backoffFor(base, max, tc.attempt)
		if got != tc.want {
			t.Errorf("backoffFor(%v, %v, attempt=%d) = %v, want %v", base, max, tc.attempt, got, tc.want)
		}
		// awaitReady does not sleep after the LAST attempt, so the documented
		// total covers attempts 1..DefaultConnectAttempts-1.
		if tc.attempt < DefaultConnectAttempts {
			total += got
		}
	}
	if want := 17750 * time.Millisecond; total != want {
		t.Errorf("total backoff across %d attempts = %v, want %v (the figure client.go documents)",
			DefaultConnectAttempts, total, want)
	}

	t.Run("never exceeds the cap and never decreases", func(t *testing.T) {
		t.Parallel()
		var prev time.Duration
		for attempt := 1; attempt <= 64; attempt++ {
			got := backoffFor(base, max, attempt)
			if got > max {
				t.Fatalf("backoffFor(attempt=%d) = %v, exceeds the cap %v", attempt, got, max)
			}
			if got < prev {
				t.Fatalf("backoffFor(attempt=%d) = %v, less than the previous %v", attempt, got, prev)
			}
			prev = got
		}
	})

	t.Run("a large attempt count cannot overflow", func(t *testing.T) {
		t.Parallel()
		// Doubling a time.Duration 1000 times would wrap to a negative value
		// and produce a negative sleep. The early return on d >= max/2 is what
		// prevents it; this asserts that guard rather than the arithmetic.
		got := backoffFor(time.Hour, 2*time.Hour, 1000)
		if got != 2*time.Hour {
			t.Errorf("backoffFor(1h, 2h, 1000) = %v, want 2h", got)
		}
		if got < 0 {
			t.Errorf("backoffFor overflowed to %v", got)
		}
	})

	t.Run("a base already above the cap is capped", func(t *testing.T) {
		t.Parallel()
		if got := backoffFor(10*time.Second, time.Second, 1); got != time.Second {
			t.Errorf("backoffFor(10s, 1s, 1) = %v, want 1s", got)
		}
	})
}

// TestJitterStaysWithinEqualJitterBounds checks the [d/2, d] envelope.
//
// Equal jitter rather than full jitter is a deliberate choice with a stated
// reason: full jitter would sometimes return near-zero and hammer a broker that
// is still starting, while no jitter would make every replica retry in the same
// millisecond. Both bounds therefore matter, and both are asserted.
func TestJitterStaysWithinEqualJitterBounds(t *testing.T) {
	t.Parallel()

	t.Run("non-positive input", func(t *testing.T) {
		t.Parallel()
		if got := jitter(0); got != 0 {
			t.Errorf("jitter(0) = %v, want 0", got)
		}
		if got := jitter(-time.Second); got != 0 {
			t.Errorf("jitter(-1s) = %v, want 0", got)
		}
	})

	for _, d := range []time.Duration{time.Nanosecond, time.Millisecond, 250 * time.Millisecond, 5 * time.Second} {
		t.Run(d.String(), func(t *testing.T) {
			t.Parallel()

			lo, hi := d/2, d
			seen := make(map[time.Duration]bool)
			for i := 0; i < 4096; i++ {
				got := jitter(d)
				if got < lo || got > hi {
					t.Fatalf("jitter(%v) = %v, outside [%v, %v]", d, got, lo, hi)
				}
				seen[got] = true
			}

			// A degenerate jitter (always the same value) would defeat the
			// thundering-herd protection entirely, and it is exactly what a
			// rand bound off by one would produce. One nanosecond has only two
			// possible outcomes, so it is exempt from the spread check.
			if d > time.Millisecond && len(seen) < 100 {
				t.Errorf("jitter(%v) produced only %d distinct values over 4096 draws; "+
					"that is not jitter", d, len(seen))
			}
		})
	}
}

// TestSleepRespectsContext covers the cancellable sleep the retry loop uses.
func TestSleepRespectsContext(t *testing.T) {
	t.Parallel()

	t.Run("a completed sleep returns nil", func(t *testing.T) {
		t.Parallel()
		start := time.Now()
		if err := sleep(t.Context(), 20*time.Millisecond); err != nil {
			t.Fatalf("sleep() = %v, want nil", err)
		}
		if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
			t.Errorf("sleep(20ms) returned after %v; it did not actually wait", elapsed)
		}
	})

	t.Run("a cancelled context returns promptly", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		err := sleep(ctx, time.Minute)
		elapsed := time.Since(start)

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sleep() = %v, want context.Canceled", err)
		}
		if elapsed > 5*time.Second {
			t.Errorf("sleep() took %v to notice the cancellation", elapsed)
		}
	})

	t.Run("an already-cancelled context does not sleep at all", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		start := time.Now()
		if err := sleep(ctx, time.Minute); !errors.Is(err, context.Canceled) {
			t.Fatalf("sleep() = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("sleep() took %v on an already-cancelled context", elapsed)
		}
	})

	t.Run("a non-positive duration reports the context state", func(t *testing.T) {
		t.Parallel()
		if err := sleep(t.Context(), 0); err != nil {
			t.Errorf("sleep(ctx, 0) = %v, want nil for a live context", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := sleep(ctx, 0); !errors.Is(err, context.Canceled) {
			t.Errorf("sleep(cancelled, 0) = %v, want context.Canceled", err)
		}
	})
}

// TestClosedFlag covers the close-once guard.
//
// set() returning true exactly once is what lets Close be idempotent without a
// mutex, and err() is what every method uses to fail fast rather than racing a
// half-closed franz-go client.
func TestClosedFlag(t *testing.T) {
	t.Parallel()

	var f closedFlag
	if f.isSet() {
		t.Fatal("isSet() = true before any close")
	}
	if err := f.err(); err != nil {
		t.Fatalf("err() = %v before any close, want nil", err)
	}

	if !f.set() {
		t.Fatal("the first set() = false, want true")
	}
	if f.set() {
		t.Error("a second set() = true; Close would run its teardown twice")
	}
	if !f.isSet() {
		t.Error("isSet() = false after set()")
	}
	if err := f.err(); !errors.Is(err, ErrClosed) {
		t.Errorf("err() = %v, want ErrClosed", err)
	}
}

// TestClosedFlagIsRaceFree exercises the flag from many goroutines so that
// `make test-race` has something to catch if the atomic is ever replaced with a
// plain bool.
func TestClosedFlagIsRaceFree(t *testing.T) {
	t.Parallel()

	var f closedFlag
	const goroutines = 64

	wins := make(chan bool, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			<-start
			_ = f.isSet()
			wins <- f.set()
			_ = f.err()
		}()
	}
	close(start)

	won := 0
	for i := 0; i < goroutines; i++ {
		if <-wins {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d goroutines observed a successful set(), want exactly 1", won)
	}
}

// TestDefaultsAreOrderedSensibly checks the relationships between the tuning
// constants that would otherwise only be discovered by a production incident.
func TestDefaultsAreOrderedSensibly(t *testing.T) {
	t.Parallel()

	// The probe timeout must sit BELOW httpx's readiness timeout (3s) so that
	// /readyz names `kafka` as the failing check instead of the whole probe
	// timing out with no detail. The 3s figure is httpx's, restated here because
	// importing it would make this package depend on the HTTP layer.
	const httpxReadinessTimeout = 3 * time.Second
	if DefaultProbeTimeout >= httpxReadinessTimeout {
		t.Errorf("DefaultProbeTimeout = %v, must be below httpx's readiness timeout %v",
			DefaultProbeTimeout, httpxReadinessTimeout)
	}
	if DefaultMetadataMinAge >= DefaultMetadataMaxAge {
		t.Errorf("DefaultMetadataMinAge (%v) must floor DefaultMetadataMaxAge (%v)",
			DefaultMetadataMinAge, DefaultMetadataMaxAge)
	}
	if DefaultConnectBackoff >= DefaultConnectBackoffMax {
		t.Errorf("DefaultConnectBackoff (%v) must be below DefaultConnectBackoffMax (%v)",
			DefaultConnectBackoff, DefaultConnectBackoffMax)
	}
	if DefaultConnectAttempts < 2 {
		t.Errorf("DefaultConnectAttempts = %d; fewer than 2 means no retry at all", DefaultConnectAttempts)
	}
	if DefaultRetryTimeout <= DefaultDialTimeout {
		t.Errorf("DefaultRetryTimeout (%v) must exceed DefaultDialTimeout (%v), or a single dial consumes the whole retry budget",
			DefaultRetryTimeout, DefaultDialTimeout)
	}
	if checkerName != "kafka" {
		t.Errorf("checkerName = %q; the /readyz payload key is a contract with the dashboard", checkerName)
	}
}
