// The price insert: the only write path into the `prices` hypertable.
//
// # One statement, many rows
//
// The rows of one record arrive together — a market is 6 selections across 10 to
// 20 books, so 60 to 120 quotes — and they go in as ONE
// `INSERT ... SELECT ... FROM unnest(...)` over six parallel arrays. That is one
// round trip and one plan for the whole market, against 120 for a row-at-a-time
// loop, and it is why "writing one row per message" is not what this does.
//
// unnest over arrays rather than a generated `VALUES ($1,$2,...),($7,$8,...)`
// list: the statement text is CONSTANT regardless of row count, so the server
// plans it once and the plan cache is not blown by a different statement for
// every batch size. It also stays well clear of the 65535-parameter wire limit,
// which a 120-row × 6-column VALUES list would approach at a few hundred rows.
//
// # COPY is not usable here, and the reason is the whole design
//
// pgx.CopyFrom is faster still and cannot express ON CONFLICT. The conflict
// clause is not an optimisation, it is the idempotency guard that makes
// at-least-once redelivery a no-op on an append-only table (see doc.go), so a
// path that drops it would trade correctness for throughput on the one table
// where history is the product. The comment on the sqlc InsertPrice query
// reaches the same conclusion for the same reason.
//
// # ON CONFLICT DO NOTHING, never DO UPDATE
//
// migrations/00003 installs prices_no_update, prices_no_delete and
// prices_no_truncate as triggers, so DO UPDATE is not merely wrong, it is
// refused by the database with SQLSTATE 23001. CLAUDE.md §4: "Immutable; a new
// price is a new row." A quote that needs correcting is corrected by INSERTing
// the corrected observation.
//
// The conflict target is named rather than left bare so the statement asserts
// WHICH uniqueness it tolerates: if a second unique index is ever added to this
// table, a collision on it surfaces as an error instead of being silently
// swallowed. Index inference matches on key columns and opclasses and ignores
// ASC/DESC, so the ascending spelling here matches the descending
// prices_natural_key_idx.
package writer

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/anpl1623/sharpline/internal/domain"
)

const insertPrices = `
INSERT INTO prices (selection_id, book_id, decimal_odds, line, observed_at, ingested_at)
SELECT * FROM unnest(
    $1::text[], $2::text[], $3::float8[], $4::float8[], $5::timestamptz[], $6::timestamptz[])
ON CONFLICT (selection_id, book_id, observed_at) DO NOTHING`

// insertPrices writes every quote in the snapshot and returns how many rows were
// actually stored.
//
// The difference between what is offered and what is returned is the number of
// duplicates the natural-key index absorbed. CommandTag.RowsAffected() counts
// rows the statement INSERTED; rows skipped by ON CONFLICT DO NOTHING are not
// among them, which is what makes the subtraction exact rather than an estimate.
//
// ingestedAt is the payload's own value, written unchanged onto every row. It is
// not re-stamped, because (ingested_at − observed_at) is the
// provider-attributable half of the staleness SLO and a replay that re-stamped
// it would report perfect provider latency for hours-old data.
func (w *Writer) insertPrices(ctx context.Context, tx pgx.Tx, prices []domain.Price, ingestedAt time.Time) (int, error) {
	total := 0
	for chunk := range chunks(prices, w.maxRowsPerStatement) {
		n, err := w.insertPriceChunk(ctx, tx, chunk, ingestedAt)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// insertPriceChunk issues one statement.
func (w *Writer) insertPriceChunk(ctx context.Context, tx pgx.Tx, prices []domain.Price, ingestedAt time.Time) (int, error) {
	n := len(prices)
	selectionIDs := make([]string, n)
	bookIDs := make([]string, n)
	decimals := make([]float64, n)
	lines := make([]*float64, n)
	observedAt := make([]time.Time, n)
	ingested := make([]time.Time, n)

	for i, p := range prices {
		selectionIDs[i] = string(p.SelectionID())
		bookIDs[i] = string(p.BookID())
		decimals[i] = p.Decimal()
		if v, ok := p.Line().Value(); ok {
			// A fresh variable per row: taking the address of the loop's own
			// copy would make every element of the slice alias the same float.
			line := v
			lines[i] = &line
		}
		// Already UTC — domain.NewPrice normalises — but the column is
		// TIMESTAMPTZ and it is the hypertable's time dimension, so the
		// normalisation is spelled out at the point of storage too.
		observedAt[i] = p.ObservedAt().UTC()
		ingested[i] = ingestedAt.UTC()
	}

	tag, err := tx.Exec(ctx, insertPrices,
		selectionIDs, bookIDs, decimals, lines, observedAt, ingested)
	if err != nil {
		return 0, fmt.Errorf("writer: insert %d price rows: %w", n, err)
	}
	return int(tag.RowsAffected()), nil
}

// chunks yields successive slices of at most size elements.
//
// It exists so that a pathologically large record — a futures market with
// hundreds of runners across twenty books — is written as several statements
// inside ONE transaction rather than as one statement with an array Postgres has
// to materialise whole. The transaction boundary is unchanged, so the durability
// claim HandleMessage makes is unaffected: either every chunk commits or none
// does.
func chunks[T any](s []T, size int) func(func([]T) bool) {
	return func(yield func([]T) bool) {
		if size <= 0 {
			// Not reachable through New, which defaults and validates the
			// option, but a zero here would loop for ever and that is a worse
			// failure than one oversized statement.
			size = len(s)
		}
		for i := 0; i < len(s); i += size {
			end := min(i+size, len(s))
			if !yield(s[i:end]) {
				return
			}
		}
	}
}
