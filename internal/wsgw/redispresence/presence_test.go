// Unit tests for the subscription store: option validation, bounds, key shape,
// the error classification and the metric contract. NO REDIS.
//
// # Why these can run without a server at all
//
// Every assertion here is about a decision this package makes BEFORE it issues a
// command: whether the options describe a usable store, whether an argument is
// admissible, what the key would be, and which of the two error classes a
// failure belongs to. A Redis would not make any of them more convincing, and
// requiring one would mean the cheapest half of the suite could not run.
//
// The tests that need a server — atomicity of the cap under concurrency, TTL
// expiry, and the one property this whole package exists for, "a second replica
// reads what the first wrote" — are in presence_integration_test.go against a
// real Redis 7.
//
// # The zero redis.Client
//
// Several tests build a store over `&redis.Client{}`. That is a legal composite
// literal (no unexported field is named), it makes Client.Key work — the key
// prefix is simply empty — and its Redis() handle is nil, which is exactly the
// state a store reaches when the composition root closes the pool during
// shutdown. So it is not a mock standing in for a server: it is a real object in
// a real state, and it is what proves the store degrades instead of panicking.
package redispresence

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/anpl1623/sharpline/internal/platform/redis"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// disconnectedStore builds a store whose client has no connection. Every method
// on it must return an error and must never panic.
func disconnectedStore(t *testing.T, reg prometheus.Registerer) *Store {
	t.Helper()
	s, err := New(Options{
		Client:   &redis.Client{},
		Logger:   discardLogger(),
		Replica:  "stream-test-0",
		Registry: reg,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

func TestNewRejectsUnusableOptions(t *testing.T) {
	t.Parallel()

	base := func() Options {
		return Options{
			Client:  &redis.Client{},
			Logger:  discardLogger(),
			Replica: "stream-0",
		}
	}

	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{"no client", func(o *Options) { o.Client = nil }},
		{"no logger", func(o *Options) { o.Logger = nil }},
		{"no replica", func(o *Options) { o.Replica = "" }},
		{"blank replica", func(o *Options) { o.Replica = "   " }},
		// A replica that redis.Client.Key would have to sanitise is refused
		// rather than rewritten, so the key an operator greps and the identity a
		// log line prints stay the same string.
		{"replica with a colon", func(o *Options) { o.Replica = "stream:0" }},
		{"replica with a slash", func(o *Options) { o.Replica = "stream/0" }},
		{"over-long replica", func(o *Options) { o.Replica = strings.Repeat("a", maxKeySegmentLen+1) }},
		{"negative ttl", func(o *Options) { o.TTL = -time.Second }},
		{"ttl below the floor", func(o *Options) { o.TTL = MinTTL - time.Millisecond }},
		{"ttl above the ceiling", func(o *Options) { o.TTL = MaxTTL + time.Second }},
		{"negative max channels", func(o *Options) { o.MaxChannels = -1 }},
		{"max channels past the hard cap", func(o *Options) { o.MaxChannels = MaxChannelsHardCap + 1 }},
		{"negative timeout", func(o *Options) { o.Timeout = -time.Second }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := base()
			tc.mutate(&opts)
			if _, err := New(opts); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("New = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

func TestNewResolvesTheGeometryDefaults(t *testing.T) {
	t.Parallel()

	s, err := New(Options{Client: &redis.Client{}, Logger: discardLogger(), Replica: "stream-0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.TTL() != DefaultTTL {
		t.Errorf("TTL = %s, want %s", s.TTL(), DefaultTTL)
	}
	if s.timeout != DefaultTimeout {
		t.Errorf("timeout = %s, want %s", s.timeout, DefaultTimeout)
	}
	if s.maxChannels != DefaultMaxChannels {
		t.Errorf("maxChannels = %d, want %d", s.maxChannels, DefaultMaxChannels)
	}
	if s.Replica() != "stream-0" {
		t.Errorf("Replica = %q, want %q", s.Replica(), "stream-0")
	}
}

// The floor exists so state cannot expire between heartbeats; it must still
// admit a TTL a test can wait out, or the expiry behaviour would be untestable
// and therefore untested.
func TestNewAcceptsATTLAtTheFloor(t *testing.T) {
	t.Parallel()

	s, err := New(Options{
		Client: &redis.Client{}, Logger: discardLogger(), Replica: "stream-0", TTL: MinTTL,
	})
	if err != nil {
		t.Fatalf("New with TTL=%s: %v", MinTTL, err)
	}
	if s.TTL() != MinTTL {
		t.Fatalf("TTL = %s, want %s", s.TTL(), MinTTL)
	}
}

// -----------------------------------------------------------------------------
// Keys
// -----------------------------------------------------------------------------

// The key shape is a contract with every operator who will ever run redis-cli
// against this deployment, and with the session-key hashing rule. Both halves
// are asserted here.
func TestKeyShapes(t *testing.T) {
	t.Parallel()

	s := disconnectedStore(t, nil)
	const session = "sid-abc123"

	digest := hashSession(session)

	if got, want := s.subKey(session), "ws:sub:"+digest; got != want {
		t.Errorf("subKey = %q, want %q", got, want)
	}
	if got, want := s.sessionKey(session), "ws:sess:"+digest; got != want {
		t.Errorf("sessionKey = %q, want %q", got, want)
	}
	if got, want := s.presenceKey, "ws:presence:stream-test-0"; got != want {
		t.Errorf("presenceKey = %q, want %q", got, want)
	}
	// A channel carries the ':' that separates key segments, so redis.Client.Key
	// rewrites it to '_' and appends a digest of the ORIGINAL. The readable stem
	// stays at the front — `KEYS …:ws:chan:market_evt-1.*` still finds it — and
	// the digest is what keeps the mapping injective.
	for channel, stem := range map[string]string{
		"market:evt-1": "ws:chan:market_evt-1.",
		"league:nba":   "ws:chan:league_nba.",
	} {
		got := s.channelKey(channel)
		if !strings.HasPrefix(got, stem) {
			t.Errorf("channelKey(%q) = %q, want the prefix %q", channel, got, stem)
		}
		if len(got) != len(stem)+16 {
			t.Errorf("channelKey(%q) = %q, want the stem plus a 16-character digest", channel, got)
		}
	}

	// THE reason the digest is there: this package does not check the channel
	// grammar, so a well-formed channel and a malformed one that flattens to the
	// same string must not share a counter.
	if s.channelKey("market:evt-1") == s.channelKey("market_evt-1") {
		t.Fatal("two distinct channels collapsed onto one counter key")
	}
}

// THE rule from doc.go: no secret is ever a key. A caller that mistakenly passes
// a bearer token instead of a `sid` must not put it in the keyspace in
// cleartext, where a keyspace dump or a MONITOR trace would carry it.
func TestTheSessionKeyNeverAppearsInAKey(t *testing.T) {
	t.Parallel()

	s := disconnectedStore(t, nil)
	const secretish = "eyJhbGciOiJIUzI1NiJ9.super-secret-token-body"

	for _, key := range []string{s.subKey(secretish), s.sessionKey(secretish)} {
		if strings.Contains(key, "secret") || strings.Contains(key, secretish) {
			t.Fatalf("key %q carries the caller's session key verbatim", key)
		}
	}
}

func TestHashSessionIsStableAndFixedLength(t *testing.T) {
	t.Parallel()

	a := hashSession("sid-1")
	if a != hashSession("sid-1") {
		t.Fatal("hashSession is not deterministic; a reconnect would restore nothing")
	}
	if a == hashSession("sid-2") {
		t.Fatal("two session keys hashed to one segment")
	}
	if len(a) != sessionDigestLen {
		t.Fatalf("digest is %d characters, want %d", len(a), sessionDigestLen)
	}
	// A caller-supplied key of any length produces the same key segment length,
	// so the keyspace's shape does not depend on the caller at all.
	if got := len(hashSession(strings.Repeat("x", MaxSessionKeyLen))); got != sessionDigestLen {
		t.Fatalf("digest of a long session key is %d characters, want %d", got, sessionDigestLen)
	}
}

// -----------------------------------------------------------------------------
// Bounds
// -----------------------------------------------------------------------------

func TestValidateChannelsBoundsAndRefuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
	}{
		{"none", nil},
		{"empty slice", []string{}},
		{"empty name", []string{"market:a", ""}},
		{"over-long name", []string{strings.Repeat("m", MaxChannelLen+1)}},
		{"embedded space", []string{"market:a b"}},
		{"embedded newline", []string{"market:a\nb"}},
		{"embedded nul", []string{"market:a\x00b"}},
		{"non-ascii", []string{"market:café"}},
		{"too many in one call", make([]string, MaxChannelsPerCall+1)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := validateChannels(tc.in); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("validateChannels(%q) = %v, want ErrInvalidArgument", tc.name, err)
			}
		})
	}
}

// An over-long channel name is untrusted input. The error says how long it was
// and does NOT quote it, for the reason internal/domain/ids.go gives about
// echoing unbounded values into log lines.
func TestAnOverLongChannelIsNotEchoedIntoTheError(t *testing.T) {
	t.Parallel()

	bad := strings.Repeat("Z", MaxChannelLen+50)
	_, err := validateChannels([]string{bad})
	if err == nil {
		t.Fatal("an over-long channel was accepted")
	}
	if strings.Contains(err.Error(), bad[:40]) {
		t.Fatalf("the error quotes the rejected value: %v", err)
	}
}

// Deduplication is not tidiness: the subscribe script counts a channel as new by
// SISMEMBER, so a duplicate in one call would increment the fleet counter twice
// for a single set member.
func TestValidateChannelsDeduplicatesAndPreservesOrder(t *testing.T) {
	t.Parallel()

	got, err := validateChannels([]string{"market:b", "market:a", "market:b", "league:nba", "market:a"})
	if err != nil {
		t.Fatalf("validateChannels: %v", err)
	}
	want := []string{"market:b", "market:a", "league:nba"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestValidateSessionKeyBounds(t *testing.T) {
	t.Parallel()

	if err := validateSessionKey(""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("empty session key = %v, want ErrInvalidArgument", err)
	}
	if err := validateSessionKey(strings.Repeat("s", MaxSessionKeyLen+1)); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("over-long session key = %v, want ErrInvalidArgument", err)
	}
	// The charset is deliberately unconstrained: the value is hashed before it
	// becomes a key segment, so no byte in it can forge a segment boundary, and
	// this package does not get to dictate the shape of a `sid` it did not mint.
	if err := validateSessionKey("sid with spaces:and:colons"); err != nil {
		t.Errorf("a session key with unusual bytes was refused: %v", err)
	}
}

func TestValidateConnectionIDBounds(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"", strings.Repeat("c", MaxConnectionIDLen+1), "conn 1", "conn\n1", "conn\x7f"} {
		if err := validateConnectionID(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("validateConnectionID(%q) = %v, want ErrInvalidArgument", bad, err)
		}
	}
	if err := validateConnectionID("conn-01HX9Z.abc"); err != nil {
		t.Errorf("a well-formed connection id was refused: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Ordering, degradation and the closed store
// -----------------------------------------------------------------------------

// Argument validation runs BEFORE anything reaches Redis. The store here has no
// connection at all, so if any of these had attempted I/O the result would be
// ErrUnavailable (or a panic) rather than ErrInvalidArgument.
func TestInvalidArgumentsAreRefusedBeforeAnyIO(t *testing.T) {
	t.Parallel()

	s := disconnectedStore(t, nil)
	ctx := context.Background()

	calls := map[string]error{
		"subscribe/no session":   s.Subscribe(ctx, "", []string{"market:a"}, "conn-1", false),
		"subscribe/no channels":  s.Subscribe(ctx, "sid-1", nil, "conn-1", false),
		"subscribe/no conn id":   s.Subscribe(ctx, "sid-1", []string{"market:a"}, "", false),
		"unsubscribe/no session": s.Unsubscribe(ctx, "", []string{"market:a"}),
		"touch/no conn id":       s.Touch(ctx, "sid-1", ""),
		"forget/no session":      s.Forget(ctx, "", "conn-1"),
		"connected/no conn id":   s.Connected(ctx, "sid-1", "", false),
		"disconnected/no conn":   s.Disconnected(ctx, "sid-1", ""),
	}
	for name, err := range calls {
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s = %v, want ErrInvalidArgument", name, err)
		}
	}

	if _, err := s.Channels(ctx, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("channels/no session = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.Session(ctx, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("session/no session = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.Subscribers(ctx, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("subscribers/no channel = %v, want ErrInvalidArgument", err)
	}
}

// A store whose client has no connection DEGRADES: every method reports
// ErrUnavailable and none of them panics. That is the state reached when the
// composition root closes the pool while connections are still draining, and it
// is the property that lets the hub ignore every error this package returns.
func TestAStoreWithoutAConnectionDegradesAndNeverPanics(t *testing.T) {
	t.Parallel()

	s := disconnectedStore(t, nil)
	ctx := context.Background()

	errs := map[string]error{
		"subscribe":    s.Subscribe(ctx, "sid-1", []string{"market:a"}, "conn-1", false),
		"unsubscribe":  s.Unsubscribe(ctx, "sid-1", []string{"market:a"}),
		"touch":        s.Touch(ctx, "sid-1", "conn-1"),
		"connected":    s.Connected(ctx, "sid-1", "conn-1", true),
		"disconnected": s.Disconnected(ctx, "sid-1", "conn-1"),
		"forget":       s.Forget(ctx, "sid-1", "conn-1"),
	}
	for name, err := range errs {
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s = %v, want ErrUnavailable", name, err)
		}
	}

	channels, err := s.Channels(ctx, "sid-1")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("channels = %v, want ErrUnavailable", err)
	}
	if len(channels) != 0 {
		t.Errorf("channels returned %d entries alongside an error; a failed read must not invent a set", len(channels))
	}
	if _, err := s.Session(ctx, "sid-1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("session = %v, want ErrUnavailable", err)
	}
	if _, err := s.Subscribers(ctx, "market:a"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("subscribers = %v, want ErrUnavailable", err)
	}
	if _, err := s.Present(ctx); !errors.Is(err, ErrUnavailable) {
		t.Errorf("present = %v, want ErrUnavailable", err)
	}
}

func TestAClosedStoreRefusesEverything(t *testing.T) {
	t.Parallel()

	s := disconnectedStore(t, nil)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is idempotent: shutdown paths call it from more than one place.
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	ctx := context.Background()
	errs := []error{
		s.Subscribe(ctx, "sid-1", []string{"market:a"}, "conn-1", false),
		s.Unsubscribe(ctx, "sid-1", []string{"market:a"}),
		s.Touch(ctx, "sid-1", "conn-1"),
		s.Connected(ctx, "sid-1", "conn-1", false),
		s.Disconnected(ctx, "sid-1", "conn-1"),
		s.Forget(ctx, "sid-1", "conn-1"),
	}
	for i, err := range errs {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("call %d = %v, want ErrClosed", i, err)
		}
	}
	if _, err := s.Channels(ctx, "sid-1"); !errors.Is(err, ErrClosed) {
		t.Errorf("channels = %v, want ErrClosed", err)
	}
	if _, err := s.Present(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("present = %v, want ErrClosed", err)
	}
}

// -----------------------------------------------------------------------------
// Error classification
// -----------------------------------------------------------------------------

// A caller whose own context is done is NOT an outage. Without this distinction
// every deploy and every client disconnect would spike the one series that is
// supposed to mean "Redis is degrading".
func TestACancelledCallerIsNotClassifiedAsAnOutage(t *testing.T) {
	t.Parallel()

	s := disconnectedStore(t, nil)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.fail(cancelled, OpTouch, "sid-1", context.Canceled)
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("a cancelled caller was classified as an outage: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the underlying cause was lost: %v", err)
	}

	// The store's OWN timeout expiring, with a live parent, IS an outage: Redis
	// did not answer inside the budget.
	err = s.fail(context.Background(), OpTouch, "sid-1", context.DeadlineExceeded)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a lapsed store timeout was not classified as an outage: %v", err)
	}
}

func TestDecodeTripleRefusesAMalformedReply(t *testing.T) {
	t.Parallel()

	for _, bad := range []any{nil, "ok", []any{int64(0)}, []any{int64(0), "x", int64(0)}} {
		if _, _, err := decodeTriple(bad); err == nil {
			t.Errorf("decodeTriple(%v) reported no error; a guessed cap decision is worse than a reported one", bad)
		}
	}
	status, stored, err := decodeTriple([]any{int64(1), int64(7), int64(0)})
	if err != nil || status != 1 || stored != 7 {
		t.Fatalf("decodeTriple = %d, %d, %v; want 1, 7, nil", status, stored, err)
	}
}

// -----------------------------------------------------------------------------
// Metrics
// -----------------------------------------------------------------------------

// The duration series lives in internal/wsgw's `sharpline_ws_` namespace. If
// that package ever declares an identical descriptor in the same process, the
// registration must ADOPT rather than fail — and a descriptor that disagrees
// must still fail loudly. Both halves are asserted here.
func TestTheDurationSeriesIsAdoptedOnASecondRegistration(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewPedanticRegistry()

	first, err := newMetrics(reg)
	if err != nil {
		t.Fatalf("first newMetrics: %v", err)
	}
	second, err := newMetrics(reg)
	if err != nil {
		t.Fatalf("second newMetrics: %v", err)
	}
	if first.duration != second.duration {
		t.Fatal("the second registration built its own collector; two stores in one process " +
			"would report two halves of one series")
	}

	// A DISAGREEING descriptor still fails. This is the half that keeps the
	// adoption from being a duplicate-registration workaround.
	conflicting := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Subsystem: metricSubsystem,
		Name:      "presence_duration_seconds",
		Help:      "a different help string",
		Buckets:   PresenceBuckets(),
	}, []string{"op"})
	if err := reg.Register(conflicting); err == nil {
		t.Fatal("a conflicting descriptor registered cleanly")
	}
}

// sharpline_ws_presence_errors_total belongs to internal/wsgw. This package must
// not declare it: two declarers fail the process at startup, and two
// incrementers would report every failure twice under two different `op` values.
func TestThePresenceErrorCounterIsLeftToTheHub(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewPedanticRegistry()
	if _, err := newMetrics(reg); err != nil {
		t.Fatalf("newMetrics: %v", err)
	}
	for _, mf := range gather(t, reg) {
		if mf.GetName() == "sharpline_ws_presence_errors_total" {
			t.Fatal("redispresence declared sharpline_ws_presence_errors_total; internal/wsgw " +
				"already owns it, and the process would fail to start")
		}
	}
}

func TestANilRegistryStillBuildsLiveCollectors(t *testing.T) {
	t.Parallel()

	m, err := newMetrics(nil)
	if err != nil {
		t.Fatalf("newMetrics(nil): %v", err)
	}
	// The observe call must stay live so no call site needs a nil check.
	m.duration.WithLabelValues(OpTouch).Observe(0.01)
}

// The duration histogram measures ROUND TRIPS. A call refused before it reached
// Redis — a client mistake, or a store whose pool has already been closed — must
// not land in it, or the p99 of a healthy Redis would be diluted by argument
// checking that took nanoseconds.
func TestCallsThatNeverReachRedisAreNotMeasured(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewPedanticRegistry()
	s := disconnectedStore(t, reg)
	ctx := context.Background()

	if err := s.Subscribe(ctx, "sid-1", []string{"bad channel"}, "conn-1", false); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Subscribe = %v, want ErrInvalidArgument", err)
	}
	if err := s.Subscribe(ctx, "sid-1", []string{"market:a"}, "conn-1", false); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Subscribe = %v, want ErrUnavailable", err)
	}
	if got := histogramCount(t, reg, "sharpline_ws_presence_duration_seconds", OpSubscribe); got != 0 {
		t.Errorf("presence_duration_seconds{op=subscribe} recorded %d samples for calls that never "+
			"reached redis, want 0", got)
	}
}

// histogramCount reads one labelled histogram's sample count.
func histogramCount(t *testing.T, g prometheus.Gatherer, name, op string) uint64 {
	t.Helper()
	for _, mf := range gather(t, g) {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelValue(m, "op") == op {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

func gather(t *testing.T, g prometheus.Gatherer) []*dto.MetricFamily {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	return families
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------

// An outage must cost a bounded number of log lines without hiding its scale:
// one line per interval, carrying the count it swallowed.
func TestWarnLimiterEmitsOncePerIntervalAndReportsWhatItSwallowed(t *testing.T) {
	t.Parallel()

	var w warnLimiter
	base := time.Now()

	if ok, suppressed := w.allow(base, time.Minute); !ok || suppressed != 0 {
		t.Fatalf("first allow = %v, %d; want true, 0", ok, suppressed)
	}
	for i := 1; i <= 5; i++ {
		if ok, _ := w.allow(base.Add(time.Duration(i)*time.Second), time.Minute); ok {
			t.Fatalf("allow %d inside the interval reported true", i)
		}
	}
	ok, suppressed := w.allow(base.Add(2*time.Minute), time.Minute)
	if !ok {
		t.Fatal("allow after the interval reported false")
	}
	if suppressed != 5 {
		t.Fatalf("suppressed = %d, want 5", suppressed)
	}
	// The count resets, so the next line reports only what it itself swallowed.
	if _, suppressed := w.allow(base.Add(4*time.Minute), time.Minute); suppressed != 0 {
		t.Fatalf("suppressed = %d after a reset, want 0", suppressed)
	}
}

func TestChunks(t *testing.T) {
	t.Parallel()

	if got := chunks(nil, 3); len(got) != 0 {
		t.Errorf("chunks(nil) = %v, want no chunks", got)
	}
	got := chunks([]string{"a", "b", "c", "d", "e"}, 2)
	if len(got) != 3 || len(got[0]) != 2 || len(got[2]) != 1 {
		t.Fatalf("chunks = %v, want [[a b] [c d] [e]]", got)
	}
}

func TestIsSafeKeySegment(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"stream-0", "stream_0", "stream.0", "S0"} {
		if !isSafeKeySegment(ok) {
			t.Errorf("isSafeKeySegment(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "stream:0", "stream 0", "stream/0", strings.Repeat("x", maxKeySegmentLen+1)} {
		if isSafeKeySegment(bad) {
			t.Errorf("isSafeKeySegment(%q) = true, want false", bad)
		}
	}
}

func TestBoolArg(t *testing.T) {
	t.Parallel()

	if boolArg(true) != "1" || boolArg(false) != "0" {
		t.Fatal("the authenticated flag is not stored as 0/1")
	}
}
