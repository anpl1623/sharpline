package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// Request parsing.
//
// Every parameter is parsed ONCE, into a typed value, with the bounds the spec
// declares. A handler that reached for r.URL.Query().Get directly would be
// re-deriving those bounds at each call site, and the third call site is where
// they stop agreeing with openapi.yaml.
//
// The collector pattern below — accumulate into `bad`, answer once — exists so a
// client that sends three wrong parameters learns about three, not about the
// first. A form that has to be submitted three times to discover three problems
// is a form nobody fills in twice.

// Bounds, all matching openapi.yaml. They are duplicated from the spec rather
// than generated because oapi-codegen emits validation for none of them in
// models-only mode; params_test.go asserts each constant against the spec text
// so the duplication cannot drift silently.
const (
	defaultPageLimit = 50
	maxPageLimit     = 200

	minSearchPrefix = 2
	maxSearchPrefix = 64

	defaultMaxHistoryPoints = 1000
	maxHistoryPoints        = 5000

	maxBookFilter = 32

	// defaultBoardHorizon is how far ahead `/board` looks when the caller does
	// not say. A day is "today's board", which is what a landing page wants;
	// futures markets months out are reachable by asking for them and are
	// deliberately not on the default page.
	defaultBoardHorizon = 24 * time.Hour

	// quoteFreshness is the staleness horizon for "current line". A quote older
	// than this is not a current price, it is history, and the board must not
	// render it as live. It is also the lower bound that gives the `prices`
	// hypertable chunk exclusion — without one, a board read consults an index
	// on every chunk that has ever existed.
	quoteFreshness = 6 * time.Hour
)

// badParams accumulates parameter failures so one response reports all of them.
type badParams struct {
	items []gen.InvalidParam
}

func (b *badParams) add(name, reason string) {
	if len(b.items) >= maxBookFilter {
		return
	}
	b.items = append(b.items, gen.InvalidParam{Name: name, Reason: reason})
}

func (b *badParams) any() bool { return len(b.items) > 0 }

// parseLimit reads `limit`, clamped to the spec's bounds.
//
// An out-of-range value is REJECTED rather than clamped. Silently serving 200
// rows to a client that asked for 5000 makes the client believe it has the whole
// set, and the bug shows up as missing data much later and somewhere else.
func parseLimit(q map[string][]string, bad *badParams) int32 {
	raw := first(q, "limit")
	if raw == "" {
		return defaultPageLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		bad.add("limit", "must be an integer")
		return defaultPageLimit
	}
	if n < 1 || n > maxPageLimit {
		bad.add("limit", fmt.Sprintf("must be between 1 and %d", maxPageLimit))
		return defaultPageLimit
	}
	return int32(n)
}

// parseOddsFormat reads `odds_format`.
//
// Decimal is the default and is ALWAYS the value that travels; this parameter
// only decides whether a rendered display string travels beside it. See
// [renderOdds].
func parseOddsFormat(q map[string][]string, bad *badParams) odds.Format {
	raw := first(q, "odds_format")
	if raw == "" {
		return odds.FormatDecimal
	}
	f, err := odds.ParseFormat(raw)
	if err != nil {
		bad.add("odds_format", "must be one of decimal, american, fractional")
		return odds.FormatDecimal
	}
	return f
}

// parseTime reads an RFC 3339 instant.
func parseTime(q map[string][]string, name string, fallback time.Time, bad *badParams) time.Time {
	raw := first(q, name)
	if raw == "" {
		return fallback
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		bad.add(name, "must be an RFC 3339 timestamp")
		return fallback
	}
	return t.UTC()
}

// parseBookFilter reads the repeatable `book` parameter and resolves each slug
// against the catalogue.
//
// AN UNKNOWN SLUG IS A 400, NOT AN EMPTY RESULT. A typo that quietly returns no
// prices is the worst possible outcome for a price-comparison endpoint: the
// client renders an empty grid and concludes no book is quoting the market.
//
// The returned set is nil when no filter was given, which means "every book" —
// distinct from an empty non-nil set, which cannot occur because an empty
// filter is a parameter error.
func parseBookFilter(q map[string][]string, books []Book, bad *badParams) map[domain.BookID]struct{} {
	raw := q["book"]
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > maxBookFilter {
		bad.add("book", fmt.Sprintf("at most %d books may be named", maxBookFilter))
		return nil
	}

	bySlug := make(map[domain.Slug]domain.BookID, len(books))
	for _, b := range books {
		bySlug[b.Slug] = b.ID
	}

	out := make(map[domain.BookID]struct{}, len(raw))
	for _, slug := range raw {
		id, ok := bySlug[domain.Slug(slug)]
		if !ok {
			// The slug is echoed only as a REASON about the parameter, never as
			// the parameter's value, so nothing client-controlled is reflected
			// verbatim into a field a client is likely to render as markup.
			bad.add("book", "names a book that does not exist")
			continue
		}
		out[id] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseSearchPrefix reads and escapes `q`.
//
// ESCAPING HAPPENS HERE AND NOWHERE ELSE. `%` and `_` are LIKE metacharacters;
// left unescaped, a query of `%` turns a prefix search into a leading-wildcard
// full scan of the events table, which is a denial of service any client can
// trigger with one character. `\` is escaped first, or escaping the other two
// would be undone by a user-supplied backslash.
func parseSearchPrefix(q map[string][]string, bad *badParams) string {
	raw := strings.TrimSpace(first(q, "q"))
	switch {
	case raw == "":
		bad.add("q", "is required")
		return ""
	case len(raw) < minSearchPrefix:
		bad.add("q", fmt.Sprintf("must be at least %d characters", minSearchPrefix))
		return ""
	case len(raw) > maxSearchPrefix:
		bad.add("q", fmt.Sprintf("must be at most %d characters", maxSearchPrefix))
		return ""
	}
	return raw
}

// escapeLike neutralises the three characters PostgreSQL LIKE treats specially.
//
// `\` must be replaced first: doing it last would double the backslashes this
// function itself introduced and turn `50%` into a literal backslash followed by
// a wildcard.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// historyResolutions maps the spec's enum onto bucket widths. `raw` is the zero
// duration, which the store reads as "every stored quote".
var historyResolutions = map[gen.HistoryResolution]time.Duration{
	gen.Raw:  0,
	gen.N10s: 10 * time.Second,
	gen.N1m:  time.Minute,
	gen.N5m:  5 * time.Minute,
	gen.N15m: 15 * time.Minute,
	gen.N1h:  time.Hour,
	gen.N6h:  6 * time.Hour,
	gen.N1d:  24 * time.Hour,
}

// resolutionOrder is the widths in ascending order, used to suggest a
// resolution that would fit a window the caller asked too finely for.
var resolutionOrder = []gen.HistoryResolution{
	gen.Raw, gen.N10s, gen.N1m, gen.N5m, gen.N15m, gen.N1h, gen.N6h, gen.N1d,
}

// parseResolution reads `resolution`.
func parseResolution(q map[string][]string, bad *badParams) (gen.HistoryResolution, time.Duration) {
	raw := first(q, "resolution")
	if raw == "" {
		return gen.Raw, 0
	}
	res := gen.HistoryResolution(raw)
	width, ok := historyResolutions[res]
	if !ok {
		bad.add("resolution", "must be one of raw, 10s, 1m, 5m, 15m, 1h, 6h, 1d")
		return gen.Raw, 0
	}
	return res, width
}

// parseMaxPoints reads `max_points`.
func parseMaxPoints(q map[string][]string, bad *badParams) int32 {
	raw := first(q, "max_points")
	if raw == "" {
		return defaultMaxHistoryPoints
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxHistoryPoints {
		bad.add("max_points", fmt.Sprintf("must be an integer between 1 and %d", maxHistoryPoints))
		return defaultMaxHistoryPoints
	}
	return int32(n)
}

// suggestResolution returns the narrowest bucket width that fits `window` inside
// `maxPoints`, and whether one exists.
//
// This is what makes the 422 on an over-wide window actionable: the client is
// told which resolution to ask for instead of being told only that it asked
// wrongly.
func suggestResolution(window time.Duration, maxPoints int32) (gen.HistoryResolution, bool) {
	if maxPoints < 1 {
		return gen.Raw, false
	}
	// `raw` is skipped: its point count is the number of stored quotes, which
	// this function cannot know without querying, so it can never be *proved* to
	// fit and must never be suggested.
	for _, res := range resolutionOrder[1:] {
		width := historyResolutions[res]
		if int64(window/width)+1 <= int64(maxPoints) {
			return res, true
		}
	}
	return gen.Raw, false
}

// first returns the first value of a repeated query parameter, or "".
func first(q map[string][]string, name string) string {
	v := q[name]
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// pathID reads a path wildcard and validates it through the domain constructor.
//
// Going through the constructor rather than casting is what stops a path segment
// carrying a value the rest of the system considers impossible — a 200-byte id,
// or one containing the `:` that WebSocket channel names split on.
func pathEventID(r *http.Request) (domain.EventID, bool) {
	id, err := domain.NewEventID(r.PathValue("eventId"))
	return id, err == nil
}

func pathMarketID(r *http.Request) (domain.MarketID, bool) {
	id, err := domain.NewMarketID(r.PathValue("marketId"))
	return id, err == nil
}

func pathSelectionID(r *http.Request) (domain.SelectionID, bool) {
	id, err := domain.NewSelectionID(r.PathValue("selectionId"))
	return id, err == nil
}

func pathSlug(r *http.Request, name string) (domain.Slug, bool) {
	s, err := domain.NewSlug(r.PathValue(name))
	return s, err == nil
}

// errBodyTooLarge distinguishes a body the limiter truncated from a body that
// was simply malformed, so the first is a 413-shaped answer rather than a
// confusing "unexpected EOF".
var errBodyTooLarge = errors.New("httpapi: request body too large")

// decodeJSON reads a JSON request body into v.
//
// DisallowUnknownFields is on. Every request schema in the spec declares
// `additionalProperties: false`, and a server that silently ignored an extra
// field would let a client believe it had set something it had not — the failure
// mode where a caller "sets" a limit through a misspelled key and is told it
// worked.
//
// The body is already capped by the server's MaxRequestBody middleware; the
// second Decode call is what rejects a second JSON document in the same body,
// which is otherwise a way to smuggle a payload past a proxy that inspects only
// the first.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errBodyTooLarge
		}
		return fmt.Errorf("decode body: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("body carries more than one JSON document")
	}
	return nil
}

// renderOdds returns the display string for a price in the requested format, or
// nil when the format is decimal.
//
// # Why the canonical value always travels and this is only ever ADDITIONAL
//
// Phase 1 established that Decimal is canonical and that American and Fractional
// are LOSSY display formats — RenderAmerican rounds, RenderFractional searches
// for a best rational approximation, and neither round-trips to the original
// decimal. If the server substituted the rendered form for the canonical one,
// the client could not switch format without refetching, and any arithmetic it
// did (a payout preview, an implied probability) would be done on a rounded
// number.
//
// So `decimal_odds` is unconditional and this is an extra field. The client
// converts for its own format toggle — it must, because a live board cannot
// refetch to change a display setting — and non-browser callers get the
// rendering without reimplementing a continued-fraction search.
//
// A render failure returns nil rather than an error: the canonical value is
// already in the response and is what matters, and failing a whole board page
// because one longshot price has no tidy fraction would be absurd.
func renderOdds(d odds.Decimal, format odds.Format) *string {
	if format == odds.FormatDecimal {
		return nil
	}
	s, err := odds.Render(d, format)
	if err != nil {
		return nil
	}
	return &s
}

// -----------------------------------------------------------------------------
// Analytics parameters
//
// The same collector discipline as everything above: accumulate into `bad`,
// answer once, and REJECT an out-of-range value rather than clamping it.
// parseLimit's argument applies to every one of these — silently serving a
// narrower window than the caller asked for makes the caller believe it has seen
// the whole set, and the bug surfaces much later and somewhere else.
// -----------------------------------------------------------------------------

// parseFloatParam reads a bounded floating-point parameter.
//
// The bounds are INCLUSIVE at both ends and are the spec's own; a value outside
// them is a 400 naming the parameter. NaN and the infinities are rejected by the
// range test itself — every comparison against NaN is false, so `n < lo` and
// `n > hi` both being false cannot happen for it, and the `!(lo <= n && n <= hi)`
// spelling below is what makes that true rather than an accident.
func parseFloatParam(q map[string][]string, name string, fallback, lo, hi float64, bad *badParams) float64 {
	raw := first(q, name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		bad.add(name, "must be a number")
		return fallback
	}
	if !(n >= lo && n <= hi) {
		bad.add(name, fmt.Sprintf("must be between %g and %g", lo, hi))
		return fallback
	}
	return n
}

// parseSecondsParam reads a strictly positive duration expressed in seconds.
//
// Seconds rather than a Go duration string because this is an HTTP query
// parameter read by browsers and curl, and `30` is unambiguous where `30s` needs
// a parser the spec would have to document. The upper bound is what stops a
// caller passing a value large enough to overflow the multiplication into
// nanoseconds.
func parseSecondsParam(q map[string][]string, name string, fallback time.Duration, bad *badParams) time.Duration {
	raw := first(q, name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		bad.add(name, "must be a number of seconds")
		return fallback
	}
	if !(n > 0 && n <= maxSignalSeconds) {
		bad.add(name, fmt.Sprintf("must be greater than 0 and at most %d", maxSignalSeconds))
		return fallback
	}
	return time.Duration(n * float64(time.Second))
}

// parseInt32Param reads a bounded 32-bit integer parameter.
func parseInt32Param(q map[string][]string, name string, fallback, lo, hi int32, bad *badParams) int32 {
	raw := first(q, name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || int32(n) < lo || int32(n) > hi {
		bad.add(name, fmt.Sprintf("must be an integer between %d and %d", lo, hi))
		return fallback
	}
	return int32(n)
}

// parseInt64Param reads a bounded 64-bit integer parameter.
func parseInt64Param(q map[string][]string, name string, fallback, lo, hi int64, bad *badParams) int64 {
	raw := first(q, name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < lo || n > hi {
		bad.add(name, fmt.Sprintf("must be an integer between %d and %d", lo, hi))
		return fallback
	}
	return n
}

// parseMarketTypes reads the repeatable `market_type` filter.
//
// An unknown member is a 400 rather than a silently narrower filter, for
// parseBookFilter's reason. The returned slice is SORTED and de-duplicated: it
// feeds a cursor fingerprint, so `?market_type=total&market_type=spread` and the
// reverse order must hash alike or a client reordering its own parameters
// between pages would be told its cursor belongs to a different query.
//
// nil means "every type", which is what an absent filter means. An empty
// non-nil slice cannot occur, because an empty filter is a parameter error.
func parseMarketTypes(q map[string][]string, bad *badParams) []domain.MarketType {
	raw := q["market_type"]
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > maxMarketTypeFilter {
		bad.add("market_type", fmt.Sprintf("at most %d types may be named", maxMarketTypeFilter))
		return nil
	}
	out := make([]domain.MarketType, 0, len(raw))
	for _, s := range raw {
		t, err := domain.ParseMarketType(s)
		if err != nil {
			bad.add("market_type", "names a market type that does not exist")
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// parseLowerBound reads the required-in-substance lower time bound every signal
// feed takes.
//
// # Why it has a default AND a ceiling
//
// `ev_signals` and `steam_signals` are hypertables with no retention policy, so
// an unbounded read consults an index on every chunk that has ever existed —
// history.go states the rule for `prices`. Unlike a line-movement chart, though,
// these feeds have an obviously correct default ("recently"), so refusing a
// request that omitted the parameter would be ceremony rather than protection.
//
// The CEILING is what actually keeps the promise. It is the retention of the
// matching Kafka topic in every case, so the window this API will serve and the
// window the bus can replay are the same window — which means a reader cannot
// ask REST for a range that phase 12's stream job could not reproduce.
//
// A bound in the future is accepted rather than rejected: it is a strange
// request, but it is a well-defined one that returns nothing, and the caller
// most likely has a clock that is ahead. A bound past the ceiling is refused,
// because that one has a plausible-looking wrong answer.
func parseLowerBound(
	q map[string][]string,
	name string,
	now time.Time,
	window, maxLookback time.Duration,
	bad *badParams,
) time.Time {
	fallback := now.Add(-window)
	raw := first(q, name)
	if raw == "" {
		return fallback
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		bad.add(name, "must be an RFC 3339 timestamp")
		return fallback
	}
	t = t.UTC()
	if t.Before(now.Add(-maxLookback)) {
		bad.add(name, fmt.Sprintf("may not be earlier than %s before now", maxLookback))
		return fallback
	}
	return t
}

// parseWindow reads a half-open [from, to) pair of instants.
//
// `to` defaults to now, which is unambiguous and cannot be unbounded. `from`
// defaults to `now - window`. A `from` at or after `to` is a 422 rather than a
// 400 and rather than an empty result: it is syntactically valid and
// semantically impossible, which is exactly the distinction respond.go keeps,
// and an empty page would be indistinguishable from a customer with no history.
func parseWindow(
	q map[string][]string,
	fromName, toName string,
	now time.Time,
	window time.Duration,
	bad *badParams,
) (from, to time.Time) {
	to = parseTime(q, toName, now, bad)
	from = parseTime(q, fromName, to.Add(-window), bad)
	return from, to
}

// parseLeaderboardBasis reads the `basis` parameter.
//
// There is no member for raw profit and the parser cannot be made to produce
// one: CLAUDE.md §6 ranks the public board "on ROI and CLV, not raw profit",
// because a profit board ranks stake size and variance. Making it
// unrepresentable is stronger than documenting that it is not offered.
func parseLeaderboardBasis(q map[string][]string, bad *badParams) LeaderboardBasis {
	raw := first(q, "basis")
	if raw == "" {
		return LeaderboardByROI
	}
	basis, err := ParseLeaderboardBasis(raw)
	if err != nil {
		bad.add("basis", "must be one of roi, clv")
		return LeaderboardByROI
	}
	return basis
}
