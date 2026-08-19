package httpapi

import (
	"net/http"
	"slices"
	"time"

	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// Line-movement history — the reason the Timescale hypertable exists
// (CLAUDE.md §6 Core: "line movement charts from history").
//
// # Why the window is required and not defaulted
//
// `prices` is partitioned on `observed_at` in 12-hour chunks and migration 00004
// installs NO retention policy, so the chunk count grows for the life of the
// deployment. A read with no lower bound defeats chunk exclusion and makes the
// planner consult an index on every chunk that has ever existed. prices.sql
// makes this the reason both of its read queries take their bounds as REQUIRED
// parameters and provides no unbounded variant to reach for by accident; this
// endpoint keeps that property at the edge, where a client could otherwise
// reintroduce it by omitting a parameter.
//
// `to` defaults to now because "up to the present" is unambiguous and cannot be
// unbounded. `from` has no default, because every candidate default is a lie
// about what the caller wanted.
//
// # Why the response is a page and not a window that got truncated
//
// A window that would produce more than `max_points` is REJECTED with 422 and a
// suggested resolution, not silently truncated. A truncated line-movement chart
// does not look truncated: it looks like a market that stopped moving, or one
// that closed at a price it never closed at. Of the three available behaviours —
// serve everything, truncate, refuse — refusing is the only one that cannot
// mislead, and naming the resolution that would fit makes it actionable in one
// retry.

func (a *API) handleHistory(w http.ResponseWriter, r *http.Request) {
	selectionID, ok := pathSelectionID(r)
	if !ok {
		failNotFound(w, r)
		return
	}

	q := r.URL.Query()
	bad := &badParams{}

	format := parseOddsFormat(q, bad)
	resolution, bucket := parseResolution(q, bad)
	maxPoints := parseMaxPoints(q, bad)

	now := a.now().UTC()
	from := parseTime(q, "from", time.Time{}, bad)
	if first(q, "from") == "" {
		bad.add("from", "is required")
	}
	to := parseTime(q, "to", now, bad)

	bookSlugRaw := first(q, "book")
	if bookSlugRaw == "" {
		bad.add("book", "is required")
	}

	books, err := a.catalogue.Books(r.Context())
	if err != nil {
		failWith(w, r, a.log, "history: books", err)
		return
	}
	idx := slices.IndexFunc(books, func(b Book) bool { return b.Slug.String() == bookSlugRaw })
	if bookSlugRaw != "" && idx < 0 {
		bad.add("book", "names a book that does not exist")
	}

	if bad.any() {
		failInvalid(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidParameter, msgInvalidParam, bad.items)
		return
	}

	// The selection is resolved before the window is validated so an unknown
	// selection is a clean 404 rather than a 422 about a window on a series that
	// does not exist.
	if _, err := a.catalogue.Selection(r.Context(), selectionID); err != nil {
		a.notFoundOr(w, r, "history: selection", err)
		return
	}

	if semantic := validateWindow(from, to, bucket, maxPoints); semantic != nil {
		failInvalid(w, r, http.StatusUnprocessableEntity, gen.ErrorCodeUnprocessable, msgUnprocessable, semantic)
		return
	}

	points, err := a.prices.History(r.Context(), HistoryQuery{
		SelectionID: selectionID,
		BookID:      books[idx].ID,
		From:        from,
		To:          to,
		Bucket:      bucket,
		MaxPoints:   maxPoints,
	})
	if err != nil {
		failWith(w, r, a.log, "history", err)
		return
	}

	out := gen.HistorySeries{
		SelectionId: selectionID.String(),
		BookId:      books[idx].ID.String(),
		BookSlug:    books[idx].Slug.String(),
		Resolution:  resolution,
		From:        from,
		To:          to,
		Points:      make([]gen.HistoryPoint, 0, len(points)),
	}
	for _, p := range points {
		out.Points = append(out.Points, gen.HistoryPoint{
			At:           p.At.UTC(),
			Open:         float64(p.Open),
			High:         float64(p.High),
			Low:          float64(p.Low),
			Close:        float64(p.Close),
			CloseDisplay: renderOdds(p.Close, format),
			Line:         p.Line,
			Samples:      p.Samples,
		})
	}
	respond(w, http.StatusOK, out)
}

// validateWindow checks the semantic constraints on a history window, returning
// nil when the request is satisfiable.
//
// These are 422 and not 400: each one is a syntactically valid parameter set
// that cannot be answered. The distinction tells a client whether to fix its
// serialiser or its logic, which are bugs in different places.
func validateWindow(from, to time.Time, bucket time.Duration, maxPoints int32) []gen.InvalidParam {
	var bad badParams

	if !to.After(from) {
		bad.add("to", "must be after 'from'")
		return bad.items
	}

	window := to.Sub(from)

	// A bucketed request has a knowable point count before the query runs, so an
	// over-wide window is refused here rather than after doing the work.
	if bucket > 0 {
		points := int64(window/bucket) + 1
		if points > int64(maxPoints) {
			if suggested, ok := suggestResolution(window, maxPoints); ok {
				bad.add("resolution", "too fine for this window; try "+string(suggested))
			} else {
				bad.add("from", "window is too wide to return at any resolution; narrow it")
			}
			return bad.items
		}
		return nil
	}

	// A RAW request's point count is the number of stored quotes, which cannot
	// be known without running the query. So the guard is on the WINDOW instead:
	// past a few hours a raw series is certainly larger than any chart wants,
	// and refusing with a suggested bucket costs one retry where serving it
	// costs a multi-hundred-thousand-point response.
	const maxRawWindow = 6 * time.Hour
	if window > maxRawWindow {
		if suggested, ok := suggestResolution(window, maxPoints); ok {
			bad.add("resolution", "raw is only available for windows up to 6h; try "+string(suggested))
		} else {
			bad.add("from", "window is too wide for a raw series; narrow it or pass a resolution")
		}
	}
	return bad.items
}
