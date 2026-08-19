package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
