package theoddsapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// The shared test harness.
//
// # No mocked provider, and no invented payload
//
// Everything served here comes out of testdata/docsamples, which holds The Odds
// API's OWN PUBLISHED example responses with provenance recorded in
// testdata/docsamples/SOURCE.md. Nothing in this package fabricates a payload.
// That distinction is the whole point of the tier: a golden file is a
// REGRESSION TEST AGAINST A REAL PUBLISHED ARTIFACT, so a change to this
// package's parsing that stops agreeing with the provider's documented format
// fails the build. A hand-written "realistic-looking" response would instead
// bake this package's own assumptions into the test meant to detect them, which
// SOURCE.md explains at length.
//
// The transport underneath is an httptest server rather than a stubbed
// http.RoundTripper, so the real net/http client, the real redirect policy, the
// real header parsing and the real body limits are all exercised.

// testAPIKey is a fake credential of realistic length. It is deliberately
// distinctive so the redaction tests can assert its ABSENCE from strings that
// contain a lot of other text.
const testAPIKey = "sk-live-THISKEYMUSTNEVERAPPEAR-0123456789"

const (
	goldenSports    = "get_sports.json"
	goldenOdds      = "get_odds_americanfootball_nfl_h2h_spreads_american.json.elided"
	goldenEventOdds = "get_event_odds_americanfootball_nfl_player_pass_tds.json"
	goldenOpenAPI   = "openapi_v4.json"
)

// docsElision is the literal suffix the provider's documentation page uses in
// place of the rest of the array.
//
// SOURCE.md records it: the published sample "shows one complete event and then
// elides the rest of the array. It ends with the literal five bytes `,\n...`
// where the closing `]` would be". The repair happens HERE, in code a reviewer
// can read, rather than as an invisible hand-edit to the stored file — and the
// assertion below means a future re-fetch that returns a COMPLETE array fails
// loudly instead of silently accepting a different file.
const docsElision = ",\n..."

func goldenPath(name string) string { return filepath.Join("testdata", "docsamples", name) }

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(goldenPath(name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

// stripDocsElision repairs the documentation's own truncation.
func stripDocsElision(t *testing.T, raw []byte) []byte {
	t.Helper()
	if !bytes.HasSuffix(raw, []byte(docsElision)) {
		t.Fatalf("%s no longer ends with the documentation's elision %q — re-read SOURCE.md before "+
			"changing this test; a complete array means the sample was re-fetched and the repair is wrong",
			goldenOdds, docsElision)
	}
	body := bytes.TrimSuffix(raw, []byte(docsElision))
	repaired := make([]byte, 0, len(body)+2)
	repaired = append(repaired, body...)
	repaired = append(repaired, "\n]"...)
	if !json.Valid(repaired) {
		t.Fatalf("%s does not parse as JSON after repairing the documentation's elision", goldenOdds)
	}
	return repaired
}

// -----------------------------------------------------------------------------
// Provider stub
// -----------------------------------------------------------------------------

// recordedRequest is what the stub saw.
type recordedRequest struct {
	Path  string
	Query url.Values
}

// providerStub is an httptest server replaying documented payloads.
type providerStub struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest

	// routes maps a concrete path to its handler.
	routes map[string]http.HandlerFunc

	// fallback answers anything unrouted.
	fallback http.HandlerFunc

	// quota headers echoed on every response, including error responses —
	// the provider documents them on every endpoint and a 401 for an exhausted
	// quota is exactly the response whose headers matter most.
	remaining int64
	used      int64
	last      int64
	omitQuota bool
}

func newProviderStub(t *testing.T) *providerStub {
	t.Helper()
	s := &providerStub{
		t:         t,
		routes:    make(map[string]http.HandlerFunc),
		remaining: 99_000,
		used:      1_000,
		last:      2,
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *providerStub) URL() string { return s.srv.URL }

func (s *providerStub) route(path string, h http.HandlerFunc) *providerStub {
	s.routes[path] = h
	return s
}

// json200 serves a payload verbatim.
func json200(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

func (s *providerStub) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, recordedRequest{Path: r.URL.Path, Query: r.URL.Query()})
	s.mu.Unlock()

	// Every documented endpoint authenticates with the apiKey QUERY PARAMETER.
	// Asserting it here is what proves the client actually sends it, which the
	// redaction tests then rely on being true.
	if got := r.URL.Query().Get(apiKeyParam); got == "" {
		s.t.Errorf("request to %s carried no %s parameter", r.URL.Path, apiKeyParam)
	}

	if !s.omitQuota {
		w.Header().Set(HeaderRequestsRemaining, strconv.FormatInt(s.remaining, 10))
		w.Header().Set(HeaderRequestsUsed, strconv.FormatInt(s.used, 10))
		w.Header().Set(HeaderRequestsLast, strconv.FormatInt(s.last, 10))
	}

	if h, ok := s.routes[r.URL.Path]; ok {
		h(w, r)
		return
	}
	if s.fallback != nil {
		s.fallback(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *providerStub) seen() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

// -----------------------------------------------------------------------------
// Adapter construction
// -----------------------------------------------------------------------------

// testLogs captures every slog record the adapter emits, so a test can assert
// what did and did not reach a log line.
type testLogs struct {
	mu      sync.Mutex
	records []string
}

func (l *testLogs) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" ")
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(a.Value.String())
		return true
	})
	l.mu.Lock()
	l.records = append(l.records, b.String())
	l.mu.Unlock()
	return nil
}

func (l *testLogs) Enabled(context.Context, slog.Level) bool { return true }
func (l *testLogs) WithAttrs(_ []slog.Attr) slog.Handler     { return l }
func (l *testLogs) WithGroup(_ string) slog.Handler          { return l }
func (l *testLogs) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.records, "\n")
}

// testHarness bundles everything a test needs to make assertions.
type testHarness struct {
	Adapter  *Adapter
	Stub     *providerStub
	Metrics  *Metrics
	Reg      *prometheus.Registry
	Logs     *testLogs
	Spans    *tracetest.SpanRecorder
	Provider *sdktrace.TracerProvider
	Now      time.Time
}

// spanText renders every recorded span's attributes, events and status into one
// string, so a redaction test can assert an ABSENCE across the whole trace
// rather than across the one attribute it thought to check.
func (h *testHarness) spanText(t *testing.T) string {
	t.Helper()
	if err := h.Provider.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	var b strings.Builder
	for _, s := range h.Spans.Ended() {
		b.WriteString(s.Name())
		b.WriteString("\n")
		b.WriteString(s.Status().Description)
		b.WriteString("\n")
		for _, a := range s.Attributes() {
			b.WriteString(string(a.Key))
			b.WriteString("=")
			b.WriteString(a.Value.String())
			b.WriteString("\n")
		}
		for _, ev := range s.Events() {
			b.WriteString(ev.Name)
			b.WriteString("\n")
			for _, a := range ev.Attributes {
				b.WriteString(string(a.Key))
				b.WriteString("=")
				b.WriteString(a.Value.String())
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// baseConfig is the American-format configuration the /odds golden file was
// recorded under: `regions=us&markets=h2h,spreads&oddsFormat=american`.
func baseConfig(baseURL string) Config {
	return Config{
		APIKey:     testAPIKey,
		BaseURL:    baseURL,
		Regions:    []string{"us"},
		Markets:    []string{"h2h", "spreads"},
		OddsFormat: OddsFormatAmerican,
		Limiter: LimiterConfig{
			// ADR 0003's recommended tier.
			MonthlyCredits: 100_000,
		},
	}
}

func newHarness(t *testing.T, stub *providerStub, mutate func(*Config)) *testHarness {
	t.Helper()

	cfg := baseConfig(stub.URL())
	if mutate != nil {
		mutate(&cfg)
	}

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	logs := &testLogs{}

	// A real SDK tracer provider with an in-memory recorder. A no-op tracer
	// would make every "the key is not in a span attribute" assertion pass
	// vacuously, which is the worst possible way for a credential test to be
	// green.
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	// A fixed clock. The golden payloads carry 2021 and 2023 timestamps, and an
	// event's live/scheduled status is decided against the FETCH instant, so a
	// wall clock would make the expected status drift with the calendar.
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	a, err := NewAdapter(cfg,
		WithMetrics(m),
		WithLogger(slog.New(logs)),
		WithTracerProvider(tp),
		WithClock(func() time.Time { return now }),
		// Instant backoff: the retry PATH is under test, the wall-clock delay
		// is not.
		WithSleep(func(context.Context, time.Duration) error { return nil }),
		WithJitter(func(d time.Duration) time.Duration { return d }),
	)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	return &testHarness{
		Adapter: a, Stub: stub, Metrics: m, Reg: reg,
		Logs: logs, Spans: rec, Provider: tp, Now: now,
	}
}

// dropped reads the mapping-loss counter for one reason.
func (h *testHarness) dropped(reason string) float64 {
	return testutil.ToFloat64(h.Metrics.mappingDropped.WithLabelValues(ProviderSlug, reason))
}

// -----------------------------------------------------------------------------
// Identifier helpers, so a test states the EXPECTED identifier by deriving it
// the same way the pipeline does rather than by pasting a literal.
// -----------------------------------------------------------------------------

func testProvider(t *testing.T) kafka.Provider {
	t.Helper()
	p, err := kafka.NewProvider(ProviderSlug)
	if err != nil {
		t.Fatalf("kafka.NewProvider(%q): %v", ProviderSlug, err)
	}
	return p
}

func mustLeagueID(t *testing.T, key string) domain.LeagueID {
	t.Helper()
	id, err := normalizer.LeagueIDFor(testProvider(t), key)
	if err != nil {
		t.Fatalf("LeagueIDFor(%q): %v", key, err)
	}
	return id
}

func mustEventID(t *testing.T, key string) domain.EventID {
	t.Helper()
	id, err := normalizer.EventIDFor(testProvider(t), key)
	if err != nil {
		t.Fatalf("EventIDFor(%q): %v", key, err)
	}
	return id
}

// findMarket returns the snapshot for the market of the given type and subject.
func findMarket(t *testing.T, e provider.EventSnapshot, typ domain.MarketType, subject string) provider.MarketSnapshot {
	t.Helper()
	for _, m := range e.Markets {
		if m.Market.Type() == typ && m.Market.Subject() == subject {
			return m
		}
	}
	t.Fatalf("event %s has no %s market with subject %q (markets: %s)",
		e.Event.ID(), typ, subject, marketSummary(e))
	return provider.MarketSnapshot{}
}

func marketSummary(e provider.EventSnapshot) string {
	parts := make([]string, 0, len(e.Markets))
	for _, m := range e.Markets {
		parts = append(parts, m.Market.Type().String()+"/"+m.Market.Subject())
	}
	return strings.Join(parts, ", ")
}

// priceFor returns the price a given book quoted for a given selection role.
func priceFor(t *testing.T, m provider.MarketSnapshot, role domain.SelectionRole, bookKey string) (domain.Price, bool) {
	t.Helper()
	bookID, err := normalizer.BookIDFor(testProvider(t), bookKey)
	if err != nil {
		t.Fatalf("BookIDFor(%q): %v", bookKey, err)
	}
	var want domain.SelectionID
	for _, s := range m.Selections {
		if s.Role() == role {
			want = s.ID()
			break
		}
	}
	if want == "" {
		return domain.Price{}, false
	}
	for _, p := range m.Prices {
		if p.SelectionID() == want && p.BookID() == bookID {
			return p, true
		}
	}
	return domain.Price{}, false
}

func nearly(got, want float64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
