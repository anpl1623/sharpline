// The catalogue upserts: sport → league → event → market → selection, plus the
// books axis.
//
// # Why the catalogue is written here at all
//
// `prices` carries foreign keys to `selections` and `books`, and `selections`
// hangs off `markets` → `events` → `leagues` → `sports`, every edge declared
// ON DELETE RESTRICT. A price cannot be stored before its spine exists, and
// nothing else in the system creates that spine: migrations/00002 says so in as
// many words — sports is "Provider data, not a closed reference set: populated
// by ingest, empty until then, and deliberately unseeded", and books is
// "Deliberately unseeded: ingest creates the synthetic row". The catalogue is a
// projection of the stream, and these statements are the projection.
//
// # Two guards, and they answer different questions
//
//	DISTINCTNESS   Every statement's ON CONFLICT DO UPDATE carries
//	               `WHERE (stored columns) IS DISTINCT FROM (excluded columns)`.
//	               Without it the steady state — the same market re-asserted on
//	               every poll — would be an UPDATE per record per row, each one
//	               firing the schema-wide set_updated_at trigger and writing a
//	               new row version for no change. Row-value IS DISTINCT FROM is
//	               used rather than a chain of `<>` because it is NULL-correct:
//	               `line <> excluded.line` is NULL, not true, when either side
//	               is NULL, and a market that gains or loses its line would
//	               silently fail to update.
//
//	MONOTONICITY   `events` and `markets` additionally carry
//	               `WHERE excluded.observed_at >= <table>.observed_at`. Kafka
//	               orders records within a partition, not across them, and a
//	               redelivery after a rebalance can land after a newer record
//	               has already been applied. Without this guard a replayed
//	               record would roll a live event back to `scheduled`. The
//	               comparison is `>=` rather than `>` — the column comment on
//	               events.observed_at specifies exactly this — so two
//	               observations sharing an instant are not silently discarded;
//	               the distinctness guard is what stops the equal-and-identical
//	               case from writing.
//
//	               `sports`, `leagues`, `books` and `selections` have no
//	               observation column and need none: nothing about them moves.
//	               A league's name changing is a correction, and last-writer-wins
//	               is the right resolution for a correction.
//
// # RETURNING (xmax = 0), and why the row count is not enough
//
// Each statement returns one boolean per row it touched: true if the row was
// INSERTED, false if it was UPDATED. A row the WHERE clause declined returns
// nothing at all. So the three outcomes — inserted, updated, unchanged — are
// read off the result exactly rather than inferred, which is what makes
// sharpline_writer_catalogue_upserts_total able to answer "did a new event
// appear on the slate?" instead of only "did something get written?".
//
// xmax is zero on a freshly inserted tuple and carries the updating
// transaction's id otherwise. The idiom is standard; the integration tier
// asserts it against a real server rather than trusting it.
//
// # Six round trips, not one
//
// These are issued sequentially rather than pipelined through pgx.Batch. The
// trade was made deliberately: the statements have genuinely different failure
// modes (a slug collision on `books` is a catalogue conflict; a declined WHERE
// on `events` is the normal steady state), and attributing an error to the right
// table by position in a batch result is exactly the kind of bookkeeping that
// goes wrong quietly. The cost is six round trips on a container-local socket
// inside a transaction that already pays one fsync at COMMIT —
// deploy/postgres/postgresql.conf keeps synchronous_commit ON — which dominates
// them. pgx.Batch is the available optimisation if measurement ever disagrees,
// and it needs no change to the statements themselves.
package writer

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/anpl1623/sharpline/internal/domain"
)

// Table names, used as the `table` label on sharpline_writer_catalogue_upserts_total.
const (
	tableSports     = "sports"
	tableLeagues    = "leagues"
	tableBooks      = "books"
	tableEvents     = "events"
	tableMarkets    = "markets"
	tableSelections = "selections"
	tablePrices     = "prices"
)

const upsertSport = `
INSERT INTO sports (id, slug, name)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE
   SET slug = excluded.slug,
       name = excluded.name
 WHERE (sports.slug, sports.name) IS DISTINCT FROM (excluded.slug, excluded.name)
RETURNING (xmax = 0)`

const upsertLeague = `
INSERT INTO leagues (id, sport_id, slug, name)
VALUES ($1, $2, $3, $4)
ON CONFLICT (id) DO UPDATE
   SET sport_id = excluded.sport_id,
       slug     = excluded.slug,
       name     = excluded.name
 WHERE (leagues.sport_id, leagues.slug, leagues.name)
       IS DISTINCT FROM
       (excluded.sport_id, excluded.slug, excluded.name)
RETURNING (xmax = 0)`

const upsertEvent = `
INSERT INTO events (
    id, league_id, kind, name,
    home_competitor_id, home_competitor_name,
    away_competitor_id, away_competitor_name,
    scheduled_start, status,
    clock_period, clock_elapsed_ns, clock_running,
    score_home, score_away,
    observed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (id) DO UPDATE
   SET league_id            = excluded.league_id,
       kind                 = excluded.kind,
       name                 = excluded.name,
       home_competitor_id   = excluded.home_competitor_id,
       home_competitor_name = excluded.home_competitor_name,
       away_competitor_id   = excluded.away_competitor_id,
       away_competitor_name = excluded.away_competitor_name,
       scheduled_start      = excluded.scheduled_start,
       status               = excluded.status,
       clock_period         = excluded.clock_period,
       clock_elapsed_ns     = excluded.clock_elapsed_ns,
       clock_running        = excluded.clock_running,
       score_home           = excluded.score_home,
       score_away           = excluded.score_away,
       observed_at          = excluded.observed_at
 WHERE excluded.observed_at >= events.observed_at
   AND (events.league_id, events.kind, events.name,
        events.home_competitor_id, events.home_competitor_name,
        events.away_competitor_id, events.away_competitor_name,
        events.scheduled_start, events.status,
        events.clock_period, events.clock_elapsed_ns, events.clock_running,
        events.score_home, events.score_away, events.observed_at)
       IS DISTINCT FROM
       (excluded.league_id, excluded.kind, excluded.name,
        excluded.home_competitor_id, excluded.home_competitor_name,
        excluded.away_competitor_id, excluded.away_competitor_name,
        excluded.scheduled_start, excluded.status,
        excluded.clock_period, excluded.clock_elapsed_ns, excluded.clock_running,
        excluded.score_home, excluded.score_away, excluded.observed_at)
RETURNING (xmax = 0)`

const upsertMarket = `
INSERT INTO markets (id, event_id, type, line, subject, status, observed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO UPDATE
   SET event_id    = excluded.event_id,
       type        = excluded.type,
       line        = excluded.line,
       subject     = excluded.subject,
       status      = excluded.status,
       observed_at = excluded.observed_at
 WHERE excluded.observed_at >= markets.observed_at
   AND (markets.event_id, markets.type, markets.line,
        markets.subject, markets.status, markets.observed_at)
       IS DISTINCT FROM
       (excluded.event_id, excluded.type, excluded.line,
        excluded.subject, excluded.status, excluded.observed_at)
RETURNING (xmax = 0)`

// Books and selections arrive as sets, so they go in as one statement over
// unnest'ed arrays rather than as a statement per row.
//
// ON CONFLICT DO UPDATE and not DO NOTHING: a book's display name or its
// reference flag legitimately changes, and a selection's name does (a provider
// correcting a player's spelling). The distinctness guard makes the unchanged
// case free.
const upsertBooks = `
INSERT INTO books (id, slug, name, kind, is_reference)
SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::boolean[])
ON CONFLICT (id) DO UPDATE
   SET slug         = excluded.slug,
       name         = excluded.name,
       kind         = excluded.kind,
       is_reference = excluded.is_reference
 WHERE (books.slug, books.name, books.kind, books.is_reference)
       IS DISTINCT FROM
       (excluded.slug, excluded.name, excluded.kind, excluded.is_reference)
RETURNING (xmax = 0)`

const upsertSelections = `
INSERT INTO selections (id, market_id, market_type, role, name)
SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[])
ON CONFLICT (id) DO UPDATE
   SET market_id   = excluded.market_id,
       market_type = excluded.market_type,
       role        = excluded.role,
       name        = excluded.name
 WHERE (selections.market_id, selections.market_type, selections.role, selections.name)
       IS DISTINCT FROM
       (excluded.market_id, excluded.market_type, excluded.role, excluded.name)
RETURNING (xmax = 0)`

// upsertCatalogue writes the whole spine in FK order.
//
// The order is load-bearing and is the hierarchy CLAUDE.md §4 defines:
// sports before leagues before events before markets before selections, because
// each references the one before it. Books are independent of that chain — a
// book is not part of the event hierarchy — but they must precede the prices
// that reference them, so they are written here rather than left to prices.go.
//
// A statement that fails aborts the transaction; postgres.InTx rolls it back and
// returns the error with its SQLSTATE intact. Nothing is retried here.
func (w *Writer) upsertCatalogue(ctx context.Context, tx pgx.Tx, s snapshot) error {
	if err := w.upsertOne(ctx, tx, tableSports, upsertSport,
		string(s.sport.ID()), string(s.sport.Slug()), s.sport.Name(),
	); err != nil {
		return err
	}

	if err := w.upsertOne(ctx, tx, tableLeagues, upsertLeague,
		string(s.league.ID()), string(s.league.SportID()),
		string(s.league.Slug()), s.league.Name(),
	); err != nil {
		return err
	}

	if err := w.upsertOne(ctx, tx, tableEvents, upsertEvent, eventArgs(s.event)...); err != nil {
		return err
	}

	if err := w.upsertOne(ctx, tx, tableMarkets, upsertMarket, marketArgs(s.market)...); err != nil {
		return err
	}

	// Books before selections is not required by any constraint; it is here so
	// that the one statement in this function that can fail on a UNIQUE index
	// unrelated to the event hierarchy (books_slug_key, or
	// books_reference_unique_idx when a payload flags a second reference book)
	// does so before the composite-FK statement, which makes a failing
	// transaction easier to read in the log.
	if err := w.upsertMany(ctx, tx, tableBooks, upsertBooks, bookArgs(s.books)...); err != nil {
		return err
	}

	return w.upsertMany(ctx, tx, tableSelections, upsertSelections,
		selectionArgs(s.selections, s.market.Type())...)
}

// upsertOne runs a single-row upsert and records which of the three outcomes it
// produced.
//
// pgx.ErrNoRows is NOT an error here: it is how the WHERE clause reports that it
// declined the update, which is both the steady state and the out-of-order
// guard doing its job.
func (w *Writer) upsertOne(ctx context.Context, tx pgx.Tx, table, sql string, args ...any) error {
	var inserted bool
	switch err := tx.QueryRow(ctx, sql, args...).Scan(&inserted); {
	case err == nil:
		w.metrics.observeCatalogue(table, outcomeFor(inserted), 1)
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		w.metrics.observeCatalogue(table, upsertUnchanged, 1)
		return nil
	default:
		return fmt.Errorf("writer: upsert %s: %w", table, err)
	}
}

// upsertMany runs a set-valued upsert and folds the returned booleans.
//
// `offered` is the array length, which is where the third outcome comes from:
// rows the WHERE clause declined return nothing, so unchanged is offered minus
// what came back.
func (w *Writer) upsertMany(ctx context.Context, tx pgx.Tx, table, sql string, args ...any) error {
	offered := arrayLen(args)

	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("writer: upsert %s: %w", table, err)
	}
	defer rows.Close()

	var inserted, updated int
	for rows.Next() {
		var isInsert bool
		if err := rows.Scan(&isInsert); err != nil {
			return fmt.Errorf("writer: upsert %s: scan result: %w", table, err)
		}
		if isInsert {
			inserted++
			continue
		}
		updated++
	}
	// Checked after the loop rather than only at Query: pgx surfaces a
	// statement error that occurred mid-stream here, and ignoring it would turn
	// a partially applied statement into a silent success.
	if err := rows.Err(); err != nil {
		return fmt.Errorf("writer: upsert %s: %w", table, err)
	}

	w.metrics.observeCatalogue(table, upsertInserted, inserted)
	w.metrics.observeCatalogue(table, upsertUpdated, updated)
	w.metrics.observeCatalogue(table, upsertUnchanged, offered-inserted-updated)
	return nil
}

func outcomeFor(inserted bool) string {
	if inserted {
		return upsertInserted
	}
	return upsertUpdated
}

// arrayLen reports how many rows a set-valued statement was offered, by reading
// the length of its first array argument. Every such statement here passes
// parallel arrays of equal length.
func arrayLen(args []any) int {
	if len(args) == 0 {
		return 0
	}
	if ss, ok := args[0].([]string); ok {
		return len(ss)
	}
	return 0
}

// -----------------------------------------------------------------------------
// Argument construction
// -----------------------------------------------------------------------------
//
// Domain values are converted to plain Go types here rather than passed
// through. domain's identifier types are defined over string and pgx would
// encode them, but the arrays would not: an explicit []string is unambiguous to
// the array codec and to a reader.
//
// Optionality is a pointer, never a sentinel. The schema's all-or-nothing CHECKs
// (events_clock_all_or_nothing, events_score_all_or_nothing) mean a partially
// nil clock is rejected by the database, so the three clock columns are built
// together from one domain.GameClock and cannot disagree.

func eventArgs(e domain.Event) []any {
	home, away := e.Home(), e.Away()

	var clockPeriod *int32
	var clockElapsed *int64
	var clockRunning *bool
	if c, ok := e.Clock(); ok {
		p := int32(c.Period())
		ns := c.Elapsed().Nanoseconds()
		r := c.Running()
		clockPeriod, clockElapsed, clockRunning = &p, &ns, &r
	}

	var scoreHome, scoreAway *int32
	if s, ok := e.Score(); ok {
		h, a := int32(s.Home()), int32(s.Away())
		scoreHome, scoreAway = &h, &a
	}

	return []any{
		string(e.ID()),
		string(e.LeagueID()),
		e.Kind().String(),
		e.Name(),
		optionalID(string(home.ID())),
		optionalText(home.Name()),
		optionalID(string(away.ID())),
		optionalText(away.Name()),
		e.ScheduledStart(),
		e.Status().String(),
		clockPeriod,
		clockElapsed,
		clockRunning,
		scoreHome,
		scoreAway,
		// domain.Event.UpdatedAt() is the PROVIDER observation instant, which is
		// what events.observed_at stores. The row's own updated_at is written by
		// the set_updated_at trigger from the database clock and is not an event
		// time; the phase-2 handoff is explicit that the two must not be
		// conflated.
		e.UpdatedAt(),
	}
}

func marketArgs(m domain.Market) []any {
	var line *float64
	if v, ok := m.Line().Value(); ok {
		line = &v
	}
	return []any{
		string(m.ID()),
		string(m.EventID()),
		m.Type().String(),
		line,
		optionalText(m.Subject()),
		m.Status().String(),
		m.UpdatedAt(),
	}
}

func bookArgs(books []domain.Book) []any {
	ids := make([]string, len(books))
	slugs := make([]string, len(books))
	names := make([]string, len(books))
	kinds := make([]string, len(books))
	refs := make([]bool, len(books))
	for i, b := range books {
		ids[i] = string(b.ID())
		slugs[i] = string(b.Slug())
		names[i] = b.Name()
		kinds[i] = b.Kind().String()
		refs[i] = b.IsReference()
	}
	return []any{ids, slugs, names, kinds, refs}
}

// selectionArgs denormalises the market's type onto every selection.
//
// selections.market_type is pinned to markets(id, type) by a composite foreign
// key, so this value cannot drift from the parent — it exists only so that
// domain.MarketType.AllowsRole() can be a declarative CHECK rather than a
// trigger. Passing the market's own type is therefore the only correct value,
// and the FK is what proves it.
func selectionArgs(sels []domain.Selection, typ domain.MarketType) []any {
	ids := make([]string, len(sels))
	marketIDs := make([]string, len(sels))
	types := make([]string, len(sels))
	roles := make([]string, len(sels))
	names := make([]string, len(sels))
	for i, s := range sels {
		ids[i] = string(s.ID())
		marketIDs[i] = string(s.MarketID())
		types[i] = typ.String()
		roles[i] = s.Role().String()
		names[i] = s.Name()
	}
	return []any{ids, marketIDs, types, roles, names}
}

// optionalText maps the domain's "absent" (an empty string) onto SQL NULL.
//
// The two are not interchangeable in this schema and the difference is enforced:
// markets_subject_matches_type makes `subject IS NOT NULL` a biconditional with
// `type = 'player_prop'`, and every name column's CHECK requires at least one
// character. An empty string would fail both.
func optionalText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// optionalID is optionalText for an identifier. It is a separate name so that
// the call sites read as what they are: a competitor id that the provider may
// legitimately not supply (migrations/00002: "providers frequently supply a name
// and no identifier, and refusing the event over a missing surrogate key would
// drop real markets").
func optionalID(s string) *string { return optionalText(s) }
