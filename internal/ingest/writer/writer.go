// The Writer: a kafka.Handler that turns one `odds.normalized` record into one
// committed transaction.
//
// Read doc.go first. It has the argument for why the flush is synchronous, why
// redelivery is harmless, and why a tombstone does not delete anything.
package writer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// Defaults. Each is overridable through Options; zero means the default.
const (
	// DefaultMaxRowsPerStatement bounds one INSERT's array length.
	//
	// It is a statement-size bound, not a batch bound: every chunk of one
	// record is written inside the SAME transaction, so it changes how the rows
	// travel and never how many of them commit together. 2000 is comfortably
	// above the 60-120 rows migrations/00003 sizes a market at, so the ordinary
	// record is one statement, and comfortably below the wire's
	// 65535-parameter ceiling.
	DefaultMaxRowsPerStatement = 2000

	// DefaultFlushTimeout bounds one write transaction.
	//
	// CLAUDE.md §12: every external call has a timeout. This one is also a
	// rebalance-safety parameter — internal/platform/kafka's Consumer blocks the
	// group's rebalance for the whole poll and fences a member that exceeds
	// RebalanceTimeout (60s), so a handler that could block indefinitely on a
	// stalled database would convert a slow query into LOST partitions and
	// duplicated work.
	DefaultFlushTimeout = 10 * time.Second
)

// ErrNotRunnable is returned by Run when it is given no consumer.
var ErrNotRunnable = errors.New("writer: no consumer")

// -----------------------------------------------------------------------------
// Consumer-declared dependencies (CLAUDE.md §12)
// -----------------------------------------------------------------------------

// DB is the part of *postgres.DB this package uses.
//
// One method, because one is all that is needed: every write here is a
// transaction, and postgres.InTx already owns the rollback, the panic guard and
// the commit-error propagation that the deferred ledger constraint made
// mandatory. Declaring the interface here rather than taking the concrete type
// is CLAUDE.md §12's rule, and it is what lets the unit tier assert the
// transaction BODY without a database while the integration tier asserts the
// same body against a real one.
type DB interface {
	InTx(ctx context.Context, fn postgres.TxFunc) error
}

// Consumer is the part of *kafka.Consumer that Run drives.
//
// The Consumer handed in MUST be built with DisableLagExport left false: the
// dashboard's bus-lag panels are fed by its background refresher and this
// package deliberately emits no competing series. See metrics.go.
type Consumer interface {
	Run(ctx context.Context, h kafka.Handler) error
}

// Compile-time proof that the shipped types satisfy the declarations above. Both
// are here rather than at the call site because a mismatch should break THIS
// package's build, where the interface is declared.
var (
	_ DB       = (*postgres.DB)(nil)
	_ Consumer = (*kafka.Consumer)(nil)
)

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

// Options configures a Writer.
type Options struct {
	// DB is the connection pool. Required.
	DB DB

	// Logger is required; there is no silent fallback, because a writer that
	// logs nowhere makes a skipped record invisible.
	Logger *slog.Logger

	// Registry receives the collectors. Nil builds them unregistered, which is
	// right for a unit test and for a process with no /metrics endpoint.
	//
	// Ignored when Metrics is set.
	Registry prometheus.Registerer

	// Metrics is an already-registered collector set, for a process running
	// more than one Writer. Takes precedence over Registry.
	Metrics *Metrics

	// MaxRowsPerStatement bounds one INSERT. Zero means
	// DefaultMaxRowsPerStatement.
	MaxRowsPerStatement int

	// FlushTimeout bounds one write transaction. Zero means
	// DefaultFlushTimeout.
	FlushTimeout time.Duration

	// Now supplies the clock used for the two lag histograms. Zero means
	// time.Now.
	//
	// Injected rather than read from the package so a test can assert an exact
	// lag rather than a bound (CLAUDE.md §12: no global mutable state). It is
	// NEVER used to stamp a stored value: observed_at is the provider's instant,
	// ingested_at is ingest's, and created_at is the database's own clock.
	Now func() time.Time
}

func (o Options) validate() error {
	switch {
	case o.DB == nil:
		return errors.New("writer: Options.DB is nil")
	case o.Logger == nil:
		return errors.New("writer: Options.Logger is nil")
	case o.MaxRowsPerStatement < 0:
		return fmt.Errorf("writer: MaxRowsPerStatement is %d, which is negative", o.MaxRowsPerStatement)
	case o.FlushTimeout < 0:
		return fmt.Errorf("writer: FlushTimeout is %s, which is negative", o.FlushTimeout)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Writer
// -----------------------------------------------------------------------------

// Writer consumes `odds.normalized` and writes line history.
//
// It is safe for concurrent use only in the sense that it holds no mutable
// state: the Consumer delivers records sequentially on one goroutine, and every
// per-record value lives on the stack.
type Writer struct {
	db      DB
	log     *slog.Logger
	metrics *Metrics
	now     func() time.Time

	maxRowsPerStatement int
	flushTimeout        time.Duration
}

// Compile-time proof that a Writer is what the Consumer expects.
var _ kafka.Handler = (*Writer)(nil)

// New builds a Writer.
func New(opts Options) (*Writer, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	m := opts.Metrics
	if m == nil {
		var err error
		if m, err = NewMetrics(opts.Registry); err != nil {
			return nil, fmt.Errorf("writer: register metrics: %w", err)
		}
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Writer{
		db:                  opts.DB,
		log:                 opts.Logger.With(slog.String("component", "ingest.writer")),
		metrics:             m,
		now:                 now,
		maxRowsPerStatement: positiveIntOr(opts.MaxRowsPerStatement, DefaultMaxRowsPerStatement),
		flushTimeout:        positiveOr(opts.FlushTimeout, DefaultFlushTimeout),
	}, nil
}

// Metrics returns the collector set, so a process running several Writers can
// share one registration.
func (w *Writer) Metrics() *Metrics { return w.metrics }

// Run consumes until ctx is cancelled or the consumer stops.
//
// It is a thin wrapper: the Consumer owns the poll loop, the commit boundary and
// the group lifecycle, and this package deliberately reimplements none of them.
// There is no final flush on the way out, and there is nothing to flush —
// HandleMessage commits before it returns, so a Writer never holds unwritten
// rows. That is the whole point of the design and is why shutdown here is
// uneventful.
func (w *Writer) Run(ctx context.Context, c Consumer) error {
	if c == nil {
		return ErrNotRunnable
	}
	w.log.Info("timescale writer running",
		slog.String("topic", kafka.TopicOddsNormalized),
		slog.String("message_type", MessageType),
		slog.Int("max_rows_per_statement", w.maxRowsPerStatement),
		slog.String("flush_timeout", w.flushTimeout.String()),
	)
	err := c.Run(ctx, w)
	w.log.Info("timescale writer stopped")
	return err
}

// HandleMessage writes one record's catalogue and quotes, and returns only once
// they are committed.
//
// It implements kafka.Handler. The Consumer commits the last successfully
// handled record per partition, so a nil return here is a claim of durability;
// every path that returns nil has either committed or has deliberately written
// nothing (a tombstone).
//
// # Errors and what the Consumer does with them
//
// A returned error leaves the offset uncommitted, so the record is redelivered.
// Under ErrorPolicyStop the consumer also stops, which for a TRANSIENT failure
// (the database restarting, a connection reset) is the right outcome: the
// process exits, the container restarts, and the record is redelivered from the
// last commit. For a PERMANENT failure — a payload that cannot be turned into
// domain values — redelivery cannot help, and the record retries for ever.
//
// The two are distinguished in the log by the `permanent` field and in the
// metrics by outcome=invalid versus outcome=failed, so an operator can tell a
// poison record from an outage without reading the error text. The CHOICE
// between halting and skipping belongs to whoever builds the Consumer; see the
// note on ErrorPolicy in internal/platform/kafka/consumer.go, which blesses
// ErrorPolicySkip for the odds path specifically because the next provider poll
// republishes the same market.
func (w *Writer) HandleMessage(ctx context.Context, d *kafka.Delivery) error {
	// A deletion on the compacted topic. It removes the market from the
	// current-line snapshot and says nothing about history, which is what this
	// table is — and prices_no_delete would refuse to remove a row anyway. No
	// catalogue mutation is invented either: the tombstone carries no market
	// status, so writing one would be fabricating a transition the provider
	// never reported.
	if d.Tombstone {
		w.metrics.observeMessage(msgTombstone)
		w.log.Info("market tombstoned; line history retained by design",
			slog.String("key", d.Key),
			slog.String("reason", d.TombstoneReason),
			slog.Int64("offset", d.Offset),
		)
		return nil
	}

	if d.Envelope.Type != MessageType {
		w.metrics.observeMessage(msgInvalid)
		return fmt.Errorf("%w: %q at %s/%d offset %d, expected %q",
			ErrWrongMessageType, d.Envelope.Type, d.Topic, d.Partition, d.Offset, MessageType)
	}

	// Typed rather than parsed from d.Key: MarketID checks the topic's declared
	// key kind, so asking odds.normalized for an EventID fails instead of
	// returning a plausible identifier of the wrong sort.
	key, err := d.MarketID()
	if err != nil {
		w.metrics.observeMessage(msgInvalid)
		return fmt.Errorf("writer: %w", err)
	}

	var rec Record
	if err := d.Unmarshal(&rec); err != nil {
		w.metrics.observeMessage(msgInvalid)
		return fmt.Errorf("writer: %w", err)
	}

	snap, err := resolve(rec, key)
	if err != nil {
		w.metrics.observeMessage(msgInvalid)
		w.log.Error("record rejected; redelivery cannot change the outcome",
			slog.Any("record", d),
			slog.Bool("permanent", true),
			slog.String("error", err.Error()),
		)
		return err
	}

	if err := w.write(ctx, snap); err != nil {
		w.metrics.observeMessage(msgFailed)
		w.log.Error("write transaction failed; the offset is uncommitted and the record will be redelivered",
			slog.Any("record", d),
			slog.Bool("permanent", false),
			slog.String("sqlstate", postgres.SQLState(err)),
			slog.String("error", err.Error()),
		)
		return err
	}

	w.metrics.observeMessage(msgWritten)
	return nil
}

// write is the transaction: catalogue upserts, then the prices they make
// storable.
//
// The whole record is one transaction on purpose. A price row whose selection
// was written by a transaction that later rolled back would be a foreign-key
// violation; a selection written without its prices would leave a market on the
// board with no quotes. Neither state is reachable, because the boundary is the
// record.
//
// The context is given its own deadline rather than inheriting the consumer's,
// which has none. See DefaultFlushTimeout for why that is a rebalance-safety
// concern and not merely hygiene.
func (w *Writer) write(ctx context.Context, s snapshot) error {
	ctx, cancel := context.WithTimeout(ctx, w.flushTimeout)
	defer cancel()

	offered := len(s.prices)
	inserted := 0

	start := w.now()
	err := w.db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := w.upsertCatalogue(ctx, tx, s); err != nil {
			return err
		}
		n, err := w.insertPrices(ctx, tx, s.prices, s.ingestedAt)
		if err != nil {
			return err
		}
		inserted = n
		return nil
	})
	committed := w.now()
	w.metrics.observeFlush(committed.Sub(start), err)

	if err != nil {
		return err
	}

	w.metrics.observePriceRows(offered, inserted)
	// Measured from the COMMIT rather than from the start of the transaction:
	// what these two histograms describe is the age of a quote at the instant it
	// became durable, which is the same instant the created_at column records.
	w.metrics.observeLags(committed, oldestObservation(s.prices), s.ingestedAt)

	w.log.Debug("market written",
		slog.String("market", string(s.market.ID())),
		slog.String("event", string(s.event.ID())),
		slog.Int("quotes_offered", offered),
		slog.Int("rows_inserted", inserted),
		slog.String("duration", committed.Sub(start).String()),
	)
	return nil
}

// oldestObservation returns the earliest observation instant in the batch.
//
// The OLDEST rather than the newest, because a staleness number is a claim about
// the worst thing in the batch. A market whose favourite re-priced a moment ago
// and whose longshot has not moved in ten minutes is ten minutes stale in the
// only sense a reader cares about; reporting the fresh quote would flatter the
// pipeline exactly when it should not.
func oldestObservation(prices []domain.Price) time.Time {
	var oldest time.Time
	for _, p := range prices {
		at := p.ObservedAt()
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	return oldest
}

func positiveOr(v, fallback time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return fallback
}

func positiveIntOr(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}
