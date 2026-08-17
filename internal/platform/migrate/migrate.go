// Package migrate applies Sharpline's embedded goose migrations.
//
// CLAUDE.md §9 is specific about the shape of this: migrations run "as a
// container — a compose service locally, a Kubernetes Job in the cluster. Never
// a binary someone runs by hand." Three consequences drive every decision here.
//
// 1. The SQL is EMBEDDED, not read from disk. The runtime image is
// gcr.io/distroless/static:nonroot with exactly one executable in it, and a
// Kubernetes Job has no repository checkout to mount. See package
// github.com/anpl1623/sharpline/migrations for why the //go:embed directive
// lives beside the .sql files rather than in this package.
//
// 2. Concurrency is assumed, not hoped against. A Job can be retried while the
// previous pod is still terminating, and a Helm pre-upgrade hook can overlap a
// pre-install hook on a fast redeploy. Two migrators applying DDL to one
// database concurrently corrupts the schema, so every mutating command takes a
// Postgres session-level advisory lock first (goose's own SessionLocker,
// wrapped so the wait is visible in the logs) and the loser waits rather than
// races.
//
// 3. Failure must be loud. `api` starts only after `migrate` exits 0
// (compose: service_completed_successfully), so a migrate container that exits 0
// having silently failed puts api in front of the wrong schema — the worst
// available outcome. Every error path returns an error that the caller turns
// into a non-zero exit, and the runner additionally refuses to proceed when the
// database is AHEAD of the migration set this binary carries, which is what a
// rollback to an older image looks like.
//
// The package holds no global mutable state and no package-level logger: a
// Runner is constructed with its dependencies and closed by its caller
// (CLAUDE.md §12).
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// Sentinel errors. These are platform-level, not domain-level, and follow the
// precedent set by internal/platform/config: callers match them with errors.Is.
var (
	// ErrUsage means argv did not name a command this binary implements.
	ErrUsage = errors.New("migrate: usage")
	// ErrInvalidOptions means Options was not usable — no DSN, no filesystem.
	ErrInvalidOptions = errors.New("migrate: invalid options")
	// ErrSchemaAhead means the database has applied a migration version this
	// binary does not carry. Almost always a deploy of an older image over a
	// newer schema. Refusing is correct: the alternative is api starting against
	// a schema whose shape it does not know.
	ErrSchemaAhead = errors.New("migrate: database schema is ahead of the migrations embedded in this binary")
	// ErrNothingToRollBack means `down` was asked to roll back a migration when
	// none is applied.
	ErrNothingToRollBack = errors.New("migrate: no applied migration to roll back")
)

// Command is one subcommand of the migrate binary.
type Command string

// The commands. `up` is the zero-argument default because the compose service
// and the Kubernetes Job both invoke the image's ENTRYPOINT with no arguments.
const (
	// CommandUp applies every pending migration. Mutating; takes the lock.
	CommandUp Command = "up"
	// CommandUpTo applies pending migrations through a target version.
	CommandUpTo Command = "up-to"
	// CommandDown rolls back the most recently applied migration. Mutating.
	CommandDown Command = "down"
	// CommandDownTo rolls back down to (and not including) a target version.
	CommandDownTo Command = "down-to"
	// CommandStatus lists every embedded migration and whether it is applied.
	CommandStatus Command = "status"
	// CommandVersion reports the schema version the database is on.
	CommandVersion Command = "version"
	// CommandValidate is the read-only pre-flight: it proves the embedded set is
	// well formed, that the database agrees with it, and reports what an `up`
	// would apply — without applying anything.
	CommandValidate Command = "validate"
)

// commandDryRun is accepted as an alias for validate, because "dry run" is what
// the Makefile target and the CI job are called and an operator will reach for
// the word they already know.
const commandDryRun = "dry-run"

// Mutates reports whether the command changes the schema.
//
// It is NOT the same question as "does this command take the advisory lock".
// goose locks around Status and GetDBVersion as well, so that a read sees a
// consistent picture rather than a half-applied one — verified against
// provider.go, where status and getDBVersion call initialize(ctx, true) while
// HasPending and GetVersions pass false and document that they never block. A
// read-only command can therefore wait behind a running migration, which is
// correct behaviour and not a bug to work around.
func (c Command) Mutates() bool {
	switch c {
	case CommandUp, CommandUpTo, CommandDown, CommandDownTo:
		return true
	default:
		return false
	}
}

// needsVersion reports whether the command requires a target version argument.
func (c Command) needsVersion() bool {
	return c == CommandUpTo || c == CommandDownTo
}

// Invocation is a parsed command line.
type Invocation struct {
	Command Command
	// TargetVersion is meaningful only for up-to and down-to.
	TargetVersion int64
}

// Usage is the one-screen help text, written to stderr on a usage error. It is
// the only documentation an operator staring at a crash-looping Job gets.
func Usage() string {
	return strings.Join([]string{
		"sharpline migrate — apply the embedded goose migrations",
		"",
		"usage: sharpline [command]",
		"",
		"commands:",
		"  up                 apply every pending migration (default when no command is given)",
		"  up-to VERSION      apply pending migrations through VERSION",
		"  down               roll back the most recently applied migration",
		"  down-to VERSION    roll back to VERSION (0 rolls everything back)",
		"  status             list every embedded migration and whether it is applied",
		"  version            report the schema version the database is on",
		"  validate           read-only pre-flight; reports what up would apply and applies nothing",
		"  dry-run            alias for validate",
		"",
		"configuration comes from the environment (internal/platform/config):",
		"  SHARPLINE_POSTGRES_DSN   required",
		"  SHARPLINE_ENV            dev|test|staging|prod",
		"  SHARPLINE_LOG_LEVEL      debug|info|warn|error",
		"",
		"exit codes: 0 success · 1 failure · 2 usage",
	}, "\n")
}

// ParseArgs parses argv without the program name (os.Args[1:]).
//
// No flag package: this binary's entire surface is one verb and an optional
// version, and a Job's argv is written once in a compose file or a Helm
// template. Flags would also be a second configuration path, which CLAUDE.md
// §12 rules out — every tunable belongs in the typed config struct.
func ParseArgs(args []string) (Invocation, error) {
	if len(args) == 0 {
		return Invocation{Command: CommandUp}, nil
	}

	raw := strings.TrimSpace(args[0])
	if raw == commandDryRun {
		raw = string(CommandValidate)
	}

	cmd := Command(raw)
	switch cmd {
	case CommandUp, CommandUpTo, CommandDown, CommandDownTo, CommandStatus, CommandVersion, CommandValidate:
	default:
		return Invocation{}, fmt.Errorf("%w: unknown command %q", ErrUsage, args[0])
	}

	rest := args[1:]
	if !cmd.needsVersion() {
		if len(rest) > 0 {
			return Invocation{}, fmt.Errorf("%w: %s takes no arguments, got %q", ErrUsage, cmd, strings.Join(rest, " "))
		}
		return Invocation{Command: cmd}, nil
	}

	if len(rest) != 1 {
		return Invocation{}, fmt.Errorf("%w: %s requires exactly one VERSION argument", ErrUsage, cmd)
	}
	version, err := strconv.ParseInt(strings.TrimSpace(rest[0]), 10, 64)
	if err != nil {
		return Invocation{}, fmt.Errorf("%w: %s: VERSION %q is not a base-10 integer", ErrUsage, cmd, rest[0])
	}
	if version < 0 {
		return Invocation{}, fmt.Errorf("%w: %s: VERSION %d is negative", ErrUsage, cmd, version)
	}
	return Invocation{Command: cmd, TargetVersion: version}, nil
}

// Timeouts. CLAUDE.md §12: "every external call has a timeout." These are
// constants rather than environment variables on purpose — adding
// SHARPLINE_MIGRATE_* would be the second configuration path §12 forbids, and
// internal/platform/config is the only one. Revisit by adding typed fields
// there, not by reading os.Getenv here.
const (
	// ConnectTimeout bounds the initial ping. Generous enough to survive a
	// Postgres container that has just passed its healthcheck and is still
	// running Timescale's background workers.
	ConnectTimeout = 20 * time.Second

	// ReadTimeout bounds the read-only commands (status, version, validate).
	ReadTimeout = 30 * time.Second

	// MutateTimeout bounds a whole up/down operation INCLUDING the advisory lock
	// wait. It must exceed lockWaitCeiling by a comfortable margin or a
	// migration that legitimately queued behind another migrator would be killed
	// by the outer deadline instead of running.
	MutateTimeout = 30 * time.Minute

	// holderProbeTimeout bounds the best-effort "who holds the lock" query. It
	// is diagnostics only; a timeout here never fails a migration.
	holderProbeTimeout = 3 * time.Second
)

// Advisory lock retry policy, in the (period, threshold) form goose's
// SessionLocker takes. period is in whole seconds and the total wait is
// period × threshold.
//
// goose's own default is 5s × 60. The period is shortened to 1s here for one
// concrete reason: a Kubernetes Job that lost the race should start within a
// second of the winner finishing, and a 1s probe interval makes the wait legible
// in the logs (lock_wait_ms is meaningful at 1s granularity, mostly noise at 5s).
// The ceiling is kept at 5 minutes.
const (
	lockRetryPeriodSeconds   uint64 = 1
	lockRetryThreshold       uint64 = 300
	unlockRetryPeriodSeconds uint64 = 1
	unlockRetryThreshold     uint64 = 30

	lockWaitCeiling = time.Duration(lockRetryPeriodSeconds*lockRetryThreshold) * time.Second
)

// maxOpenConns caps the pool. The Provider holds exactly one connection for the
// duration of an operation — the advisory lock is session-scoped, so the lock
// and the DDL must share a session — and the drift check borrows a second. Two
// is the true requirement; goose additionally rejects a pool of 1 when Go
// migrations are registered, and this project has only SQL ones, but there is no
// reason to sit on that edge.
const maxOpenConns = 2

// applicationName lands in pg_stat_activity and in Postgres' logs, so an
// operator watching a slow migration can tell which session is the migrator.
const applicationName = "sharpline-migrate"

// Options configures a Runner. Dependencies are injected; nothing is read from
// the process environment in this package (CLAUDE.md §12).
type Options struct {
	// DSN is the Postgres connection string, from config.Config.PostgresDSN.
	// Both forms config accepts work: the postgres:// URL and the libpq
	// keyword/value form.
	DSN string

	// Logger receives every structured line. Required.
	Logger *slog.Logger

	// FS is the migration set. Nil means EmbeddedFS(), which is what every
	// caller outside tests wants.
	FS fs.FS

	// Verbose forwards goose's own per-migration progress into Logger. The
	// migrate container's log is the only observability a Job has, so the
	// binary passes true.
	Verbose bool

	// LockID overrides the advisory lock key. Zero means lock.DefaultLockID,
	// which is also what the goose CLI in the tools image uses — keeping the
	// default means `make migrate` and `make migrate-down` mutually exclude each
	// other rather than silently running side by side.
	LockID int64
}

// Runner applies migrations. Construct with New, close with Close.
type Runner struct {
	db       *sql.DB
	provider *goose.Provider
	log      *slog.Logger
	fsys     fs.FS
	sources  []Source
	lockID   int64
	observer *lockObserver
}

// New validates opts, opens the pool and builds the goose provider over the
// embedded migration set.
//
// It does no I/O: database/sql opens lazily and goose collects sources from the
// filesystem. The first network call is the ping inside Run, which is where the
// context and its timeout arrive.
func New(opts Options) (*Runner, error) {
	if strings.TrimSpace(opts.DSN) == "" {
		return nil, fmt.Errorf("%w: DSN is empty", ErrInvalidOptions)
	}
	if opts.Logger == nil {
		return nil, fmt.Errorf("%w: Logger is nil", ErrInvalidOptions)
	}

	fsys := opts.FS
	if fsys == nil {
		fsys = EmbeddedFS()
	}

	sources, err := Sources(fsys)
	if err != nil {
		return nil, err
	}

	lockID := opts.LockID
	if lockID == 0 {
		lockID = lock.DefaultLockID
	}

	db, err := openDB(opts.DSN)
	if err != nil {
		return nil, err
	}

	sessionLocker, err := lock.NewPostgresSessionLocker(
		lock.WithLockID(lockID),
		lock.WithLockTimeout(lockRetryPeriodSeconds, lockRetryThreshold),
		lock.WithUnlockTimeout(unlockRetryPeriodSeconds, unlockRetryThreshold),
	)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("migrate: build advisory session locker: %w", err), db.Close())
	}

	observer := &lockObserver{
		inner:  sessionLocker,
		log:    opts.Logger,
		lockID: lockID,
		holder: pgAdvisoryLockHolder,
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys,
		// Session-level advisory locking. This is the requirement that makes a
		// retried Job safe; see the package comment.
		goose.WithSessionLocker(observer),
		// goose's own progress lines arrive as JSON on our logger, tagged
		// logger=goose, instead of on a private stdout logger.
		goose.WithSlog(opts.Logger),
		goose.WithVerbose(opts.Verbose),
	)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("migrate: build goose provider: %w", err), db.Close())
	}

	return &Runner{
		db:       db,
		provider: provider,
		log:      opts.Logger,
		fsys:     fsys,
		sources:  sources,
		lockID:   lockID,
		observer: observer,
	}, nil
}

// Close releases the pool. Safe to call on a Runner whose Run failed.
func (r *Runner) Close() error {
	var errs []error
	if r.provider != nil {
		if err := r.provider.Close(); err != nil {
			errs = append(errs, fmt.Errorf("migrate: close goose provider: %w", err))
		}
	} else if r.db != nil {
		// provider.Close closes the pool it was given, so only close the pool
		// directly when there is no provider to do it.
		if err := r.db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("migrate: close database pool: %w", err))
		}
	}
	return errors.Join(errs...)
}

// openDB builds a database/sql pool over pgx.
//
// pgx is mandatory, not preferred: it is pure Go, so CGO_ENABLED=0 keeps
// working and the binary keeps running on distroless/static. Anything binding
// libpq would break the image and therefore the prime directive.
//
// stdlib.OpenDB rather than sql.Open("pgx", dsn): OpenDB takes the parsed
// config directly and needs no blank import to register a driver name in
// database/sql's global registry.
func openDB(dsn string) (*sql.DB, error) {
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		// The DSN can carry a password. pgx's error text quotes the parse
		// problem, not the value, but the DSN itself is never echoed here.
		return nil, fmt.Errorf("%w: parse Postgres DSN: %w", ErrInvalidOptions, err)
	}

	// Simple protocol, deliberately. A migration runner sends each DDL
	// statement exactly once, so prepared-statement caching buys nothing, and
	// the extended protocol refuses more than one statement per Exec — which a
	// hand-written `-- +goose StatementBegin` block is entitled to contain.
	// Simple protocol removes that whole class of surprise from migrations
	// nobody wrote with a wire protocol in mind.
	cc.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	if cc.RuntimeParams == nil {
		cc.RuntimeParams = make(map[string]string, 1)
	}
	cc.RuntimeParams["application_name"] = applicationName

	db := stdlib.OpenDB(*cc)
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(1)
	// No ConnMaxLifetime: this process runs to completion in seconds to minutes
	// and recycling the session holding the advisory lock would drop the lock.
	return db, nil
}

// Applied records one migration that ran.
type Applied struct {
	Version   int64
	Name      string
	Direction string
	Duration  time.Duration
	// Empty is goose's report that the migration was versioned but contained no
	// statements. Worth surfacing: an accidentally empty file otherwise looks
	// exactly like a successful migration.
	Empty bool
}

// Status is one row of the status report.
type Status struct {
	Version   int64
	Name      string
	State     string
	AppliedAt time.Time
}

// Summary is the machine-readable result of a Run, logged as one structured line
// so the answer to "what did this Job do" is a single grep.
type Summary struct {
	Command       Command
	Embedded      int
	VersionBefore int64
	VersionAfter  int64
	PendingBefore int
	Applied       []Applied
	Statuses      []Status
	LockWait      time.Duration
	Duration      time.Duration
}

// LogValue implements slog.LogValuer so a Summary logs as a group rather than
// as a Go-syntax blob.
func (s Summary) LogValue() slog.Value {
	names := make([]string, 0, len(s.Applied))
	versions := make([]int64, 0, len(s.Applied))
	for _, a := range s.Applied {
		names = append(names, a.Name)
		versions = append(versions, a.Version)
	}
	return slog.GroupValue(
		slog.String("command", string(s.Command)),
		slog.Int("migrations_embedded", s.Embedded),
		slog.Int64("schema_version_before", s.VersionBefore),
		slog.Int64("schema_version_after", s.VersionAfter),
		slog.Int("migrations_pending_before", s.PendingBefore),
		slog.Int("migrations_applied", len(s.Applied)),
		slog.Any("applied_versions", versions),
		slog.Any("applied_names", names),
		slog.Int64("lock_wait_ms", s.LockWait.Milliseconds()),
		slog.Int64("duration_ms", s.Duration.Milliseconds()),
	)
}

// Run executes inv.
//
// ctx is the process-lifetime context — cancel it (SIGTERM) and an in-flight
// migration's transaction rolls back and Run returns an error, which the caller
// turns into a non-zero exit. Each command derives its own deadline from ctx.
func (r *Runner) Run(ctx context.Context, inv Invocation) (*Summary, error) {
	started := time.Now()

	r.log.Info("migration set",
		slog.Int("migrations_embedded", len(r.sources)),
		slog.Any("migrations", Names(r.sources)),
		slog.String("goose_table", goose.DefaultTablename),
		slog.Int64("advisory_lock_id", r.lockID),
		// Whether this invocation can change the schema at all. The first
		// question asked of a migrate container's log is usually "did this one
		// touch anything", and this answers it on line two.
		slog.Bool("mutating", inv.Command.Mutates()),
	)

	if err := r.ping(ctx); err != nil {
		return nil, err
	}

	summary := &Summary{Command: inv.Command, Embedded: len(r.sources)}

	var err error
	switch inv.Command {
	case CommandUp:
		err = r.runUp(ctx, summary, inv.TargetVersion, false)
	case CommandUpTo:
		err = r.runUp(ctx, summary, inv.TargetVersion, true)
	case CommandDown:
		err = r.runDown(ctx, summary, 0, false)
	case CommandDownTo:
		err = r.runDown(ctx, summary, inv.TargetVersion, true)
	case CommandStatus:
		err = r.runStatus(ctx, summary)
	case CommandVersion:
		err = r.runVersion(ctx, summary)
	case CommandValidate:
		err = r.runValidate(ctx, summary)
	default:
		// Unreachable via ParseArgs; a direct caller can still get here.
		err = fmt.Errorf("%w: unknown command %q", ErrUsage, inv.Command)
	}

	// The summary is returned even on failure, so a partially applied `up` can
	// report which migrations committed before it broke.
	summary.LockWait = r.observer.waited()
	summary.Duration = time.Since(started)
	return summary, err
}

// ping proves the database is reachable before anything else runs, and reports
// what it connected to. A migrator that cannot reach Postgres must say so in
// one line rather than surfacing as a goose error twelve frames deep.
func (r *Runner) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()

	const q = `SELECT current_database(), current_user, current_setting('server_version')`
	var database, user, serverVersion string
	if err := r.db.QueryRowContext(ctx, q).Scan(&database, &user, &serverVersion); err != nil {
		return fmt.Errorf("migrate: connect to Postgres: %w", err)
	}
	r.log.Info("database reachable",
		slog.String("database", database),
		slog.String("user", user),
		slog.String("server_version", serverVersion),
	)
	return nil
}

// runUp applies pending migrations, up to target when hasTarget.
func (r *Runner) runUp(ctx context.Context, summary *Summary, target int64, hasTarget bool) error {
	ctx, cancel := context.WithTimeout(ctx, MutateTimeout)
	defer cancel()

	if _, err := r.collectBefore(ctx, summary); err != nil {
		return err
	}

	if summary.PendingBefore == 0 && !hasTarget {
		r.log.Info("nothing to apply",
			slog.Int64("schema_version", summary.VersionBefore),
			slog.Int("migrations_embedded", summary.Embedded),
		)
		summary.VersionAfter = summary.VersionBefore
		return nil
	}

	r.log.Info("applying migrations",
		slog.Int("migrations_pending", summary.PendingBefore),
		slog.Int64("schema_version_before", summary.VersionBefore),
		slog.Bool("targeted", hasTarget),
		slog.Int64("target_version", target),
		slog.String("lock_wait_ceiling", lockWaitCeiling.String()),
	)

	var (
		results []*goose.MigrationResult
		err     error
	)
	if hasTarget {
		results, err = r.provider.UpTo(ctx, target)
	} else {
		results, err = r.provider.Up(ctx)
	}

	// A PartialError carries the migrations that DID apply before the failure.
	// Those rows are committed and must be reported, or the operator is told
	// "up failed" with no idea what state the schema is in.
	var partial *goose.PartialError
	if errors.As(err, &partial) {
		summary.Applied = appliedFrom(partial.Applied)
		r.logApplied(summary.Applied)
		after, verr := r.schemaVersion(ctx)
		if verr == nil {
			summary.VersionAfter = after
		}
		failedVersion := int64(0)
		failedPath := ""
		if partial.Failed != nil && partial.Failed.Source != nil {
			failedVersion = partial.Failed.Source.Version
			failedPath = partial.Failed.Source.Path
		}
		return fmt.Errorf("migrate: up failed at version %d (%s) after applying %d migration(s); schema is now at %d: %w",
			failedVersion, failedPath, len(summary.Applied), summary.VersionAfter, err)
	}
	if err != nil {
		return fmt.Errorf("migrate: up: %w", err)
	}

	summary.Applied = appliedFrom(results)
	r.logApplied(summary.Applied)

	after, err := r.schemaVersion(ctx)
	if err != nil {
		return err
	}
	summary.VersionAfter = after
	return nil
}

// runDown rolls back one migration, or down to target when hasTarget.
func (r *Runner) runDown(ctx context.Context, summary *Summary, target int64, hasTarget bool) error {
	ctx, cancel := context.WithTimeout(ctx, MutateTimeout)
	defer cancel()

	if _, err := r.collectBefore(ctx, summary); err != nil {
		return err
	}

	r.log.Info("rolling back",
		slog.Int64("schema_version_before", summary.VersionBefore),
		slog.Bool("targeted", hasTarget),
		slog.Int64("target_version", target),
	)

	var (
		results []*goose.MigrationResult
		err     error
	)
	if hasTarget {
		results, err = r.provider.DownTo(ctx, target)
	} else {
		var one *goose.MigrationResult
		one, err = r.provider.Down(ctx)
		if one != nil {
			results = []*goose.MigrationResult{one}
		}
	}

	var partial *goose.PartialError
	if errors.As(err, &partial) {
		summary.Applied = appliedFrom(partial.Applied)
		r.logApplied(summary.Applied)
		after, verr := r.schemaVersion(ctx)
		if verr == nil {
			summary.VersionAfter = after
		}
		return fmt.Errorf("migrate: down failed after rolling back %d migration(s); schema is now at %d: %w",
			len(summary.Applied), summary.VersionAfter, err)
	}
	switch {
	case errors.Is(err, goose.ErrNoNextVersion), errors.Is(err, goose.ErrNoCurrentVersion):
		// Reported as a failure, not a no-op success. `down` is an explicit
		// human request to change the schema; "there was nothing to change" is
		// information the operator needs on stderr with a non-zero exit, not a
		// line they might scroll past.
		return fmt.Errorf("%w: schema is at version %d: %w", ErrNothingToRollBack, summary.VersionBefore, err)
	case err != nil:
		return fmt.Errorf("migrate: down: %w", err)
	}

	summary.Applied = appliedFrom(results)
	r.logApplied(summary.Applied)

	after, err := r.schemaVersion(ctx)
	if err != nil {
		return err
	}
	summary.VersionAfter = after
	return nil
}

// runStatus reports every embedded migration and whether the database has it.
func (r *Runner) runStatus(ctx context.Context, summary *Summary) error {
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()

	statuses, err := r.collectBefore(ctx, summary)
	if err != nil {
		return err
	}
	summary.VersionAfter = summary.VersionBefore

	summary.Statuses = make([]Status, 0, len(statuses))
	for _, st := range statuses {
		row := Status{State: string(st.State), AppliedAt: st.AppliedAt}
		if st.Source != nil {
			row.Version = st.Source.Version
			row.Name = st.Source.Path
		}
		summary.Statuses = append(summary.Statuses, row)

		attrs := []any{
			slog.Int64("version", row.Version),
			slog.String("migration", row.Name),
			slog.String("state", row.State),
		}
		if !row.AppliedAt.IsZero() {
			attrs = append(attrs, slog.Time("applied_at", row.AppliedAt.UTC()))
		}
		r.log.Info("migration", attrs...)
	}
	return nil
}

// runVersion reports the schema version.
func (r *Runner) runVersion(ctx context.Context, summary *Summary) error {
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()

	version, err := r.schemaVersion(ctx)
	if err != nil {
		return err
	}
	summary.VersionBefore, summary.VersionAfter = version, version

	highest := int64(0)
	if len(r.sources) > 0 {
		highest = r.sources[len(r.sources)-1].Version
	}
	r.log.Info("schema version",
		slog.Int64("schema_version", version),
		slog.Int64("highest_embedded_version", highest),
		slog.Bool("at_head", version == highest),
	)
	return nil
}

// runValidate is the read-only pre-flight. It applies no migration, and it is
// the mode CI and a pre-deploy check want: it fails when the embedded set is
// malformed or when the database has drifted ahead of this binary, and it
// succeeds — reporting what an `up` would do — when migrations are merely
// pending, because pending migrations are the normal state before a deploy.
//
// One honest caveat: no MIGRATION is applied, but goose creates its own
// goose_db_version bookkeeping table on the first operation that reads it, so
// running validate against a virgin database does leave that one table behind.
// Verified: after validate on an empty database, `\dt` lists exactly
// goose_db_version holding the single version_id 0 sentinel row, and no
// application table exists. That is goose's ensureVersionTable, not a write this
// package performs, and `up` would create the same table a moment later.
func (r *Runner) runValidate(ctx context.Context, summary *Summary) error {
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()

	if err := CheckStructure(r.fsys, r.sources); err != nil {
		return err
	}
	r.log.Info("embedded migration set is well formed",
		slog.Int("migrations_embedded", len(r.sources)),
		slog.Any("versions", Versions(r.sources)),
	)

	statuses, err := r.collectBefore(ctx, summary)
	if err != nil {
		return err
	}
	summary.VersionAfter = summary.VersionBefore

	pending := make([]string, 0, len(statuses))
	for _, st := range statuses {
		if st.State == goose.StatePending && st.Source != nil {
			pending = append(pending, st.Source.Path)
		}
	}

	r.log.Info("validation passed; nothing was applied",
		slog.Int64("schema_version", summary.VersionBefore),
		slog.Int("migrations_pending", len(pending)),
		slog.Any("would_apply", pending),
	)
	return nil
}

// collectBefore fills in the pre-operation state every command reports, runs the
// drift check, and returns goose's per-migration status.
//
// The statuses are RETURNED rather than re-fetched by the caller on purpose:
// goose takes the advisory lock around Status, so calling it twice per command
// acquires and releases the lock twice for one question. Verified by the log of
// an earlier revision, which showed two acquire/release pairs on a plain
// `status`.
func (r *Runner) collectBefore(ctx context.Context, summary *Summary) ([]*goose.MigrationStatus, error) {
	state, err := r.readAppliedState(ctx)
	if err != nil {
		return nil, err
	}
	summary.VersionBefore = state.max

	if err := checkDrift(Versions(r.sources), state.versions, len(r.sources), highestVersion(r.sources)); err != nil {
		return nil, err
	}

	statuses, err := r.provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: read migration status: %w", err)
	}
	for _, st := range statuses {
		if st.State == goose.StatePending {
			summary.PendingBefore++
		}
	}
	return statuses, nil
}

// appliedState is what the database itself says has been applied.
type appliedState struct {
	// tableExists is false on a virgin database, before goose has created its
	// bookkeeping table.
	tableExists bool
	// versions are the applied migration versions in ascending order, excluding
	// goose's version 0 sentinel row.
	versions []int64
	// max is the highest applied version, 0 when nothing is applied. This is the
	// number every command reports as the schema version.
	max int64
}

// readAppliedState reads goose's bookkeeping table directly, in one query, with
// no advisory lock.
//
// It deliberately does NOT use provider.GetDBVersion, and the reason is
// measured: goose takes the advisory lock around GetDBVersion, so reading the
// version before and after an operation added two lock acquire/release cycles to
// every `up` — verified in the logs of an earlier revision, which showed four
// pairs for one `up` where two are needed. Two consequences of owning the query:
// `version` never blocks behind a running migration, and the drift check below
// is free because it needs the same rows.
//
// The query reproduces GetDBVersion's documented semantics exactly — "the
// highest version recorded in the database, regardless of the order in which
// migrations were applied" — and filters version_id > 0 to drop the sentinel row
// goose writes when it creates the table, the same way `make migrate-dry-run`
// does. It reads goose.DefaultTablename, which is correct only because this
// package never passes goose.WithTableName; the two must stay in step.
func (r *Runner) readAppliedState(ctx context.Context) (appliedState, error) {
	var state appliedState

	// goose.DefaultTablename is a library constant, never operator input, so
	// interpolating it into the query below introduces no injection surface. It
	// is still passed as a parameter to to_regclass, where a parameter works.
	if err := r.db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, goose.DefaultTablename).
		Scan(&state.tableExists); err != nil {
		return state, fmt.Errorf("migrate: look up %s: %w", goose.DefaultTablename, err)
	}
	if !state.tableExists {
		// A fresh database is the normal first case, not an error: goose creates
		// the table on the first operation that needs it.
		return state, nil
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT version_id FROM `+goose.DefaultTablename+` WHERE is_applied AND version_id > 0 ORDER BY version_id`)
	if err != nil {
		return state, fmt.Errorf("migrate: list applied versions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return state, fmt.Errorf("migrate: scan applied version: %w", err)
		}
		state.versions = append(state.versions, v)
		if v > state.max {
			state.max = v
		}
	}
	if err := rows.Err(); err != nil {
		return state, fmt.Errorf("migrate: read applied versions: %w", err)
	}
	return state, nil
}

// schemaVersion is readAppliedState reduced to the version number, for the
// post-operation read.
func (r *Runner) schemaVersion(ctx context.Context) (int64, error) {
	state, err := r.readAppliedState(ctx)
	if err != nil {
		return 0, err
	}
	return state.max, nil
}

// checkDrift refuses to run when the database has applied a version this binary
// does not carry.
//
// goose already rejects an out-of-order source (a pending migration numbered
// below the current version). It does NOT notice the opposite: an applied row
// with no corresponding source, which is what deploying an older image over a
// newer schema looks like. Letting that pass means `up` reports "0 applied",
// exits 0, and api starts against a schema newer than the code — a silent
// success that is worse than any crash.
//
// Pure, so the rule is testable without a database.
func checkDrift(embedded, applied []int64, embeddedCount int, highest int64) error {
	unknown := UnknownApplied(embedded, applied)
	if len(unknown) == 0 {
		return nil
	}
	return fmt.Errorf("%w: version(s) %v are applied in the database but are not embedded in this binary (%d embedded, highest %d) — this looks like an older image deployed over a newer schema",
		ErrSchemaAhead, unknown, embeddedCount, highest)
}

// UnknownApplied returns the applied versions that have no embedded source,
// preserving the order of applied. Exported and pure so the drift rule is
// testable without a database.
func UnknownApplied(embedded, applied []int64) []int64 {
	known := make(map[int64]struct{}, len(embedded))
	for _, v := range embedded {
		known[v] = struct{}{}
	}
	var unknown []int64
	for _, v := range applied {
		if _, ok := known[v]; !ok {
			unknown = append(unknown, v)
		}
	}
	return unknown
}

func highestVersion(sources []Source) int64 {
	if len(sources) == 0 {
		return 0
	}
	return sources[len(sources)-1].Version
}

// appliedFrom converts goose results into the package's own record type, so
// nothing outside this package depends on goose's shapes.
func appliedFrom(results []*goose.MigrationResult) []Applied {
	out := make([]Applied, 0, len(results))
	for _, res := range results {
		if res == nil {
			continue
		}
		row := Applied{Direction: res.Direction, Duration: res.Duration, Empty: res.Empty}
		if res.Source != nil {
			row.Version = res.Source.Version
			row.Name = res.Source.Path
		}
		out = append(out, row)
	}
	return out
}

// logApplied emits one line per migration that ran — the "which migrations
// applied" half of the reporting requirement, with the count in the summary.
//
// These lines are what makes goose's verbose mode unnecessary at info level: a
// verbose run logs every individual SQL statement, which for this migration set
// is 208 lines and 220KB, and buries exactly the seven facts an operator wants.
func (r *Runner) logApplied(applied []Applied) {
	for _, a := range applied {
		attrs := []any{
			slog.Int64("version", a.Version),
			slog.String("migration", a.Name),
			slog.String("direction", a.Direction),
			slog.Int64("duration_ms", a.Duration.Milliseconds()),
		}
		// The message names the direction rather than saying "applied" for a
		// rollback, so a log search for a rollback does not have to know that
		// goose calls both directions an application.
		msg := "migration applied"
		if a.Direction == directionDown {
			msg = "migration rolled back"
		}
		if a.Empty {
			// An empty migration is versioned but ran no statements. Almost
			// always a truncated file, so it is a warning rather than an info.
			r.log.Warn(msg+" but contained no statements", attrs...)
			continue
		}
		r.log.Info(msg, attrs...)
	}
}

// directionDown is the value goose puts in MigrationResult.Direction for a
// rollback.
const directionDown = "down"

// lockObserver wraps goose's SessionLocker so the advisory lock is visible in
// the logs.
//
// Without this, a Job that queued behind another migrator looks identical to
// one that was simply slow: goose's locker retries silently. The wrapper adds
// the two facts an operator needs — that the lock was held by someone else, and
// for how long this process waited — and it is the evidence that the lock is
// actually engaged rather than merely configured.
//
// It delegates the locking itself to goose's implementation; the primitive is
// pg_try_advisory_lock in goose's own tested code, not a reimplementation.
type lockObserver struct {
	inner  lock.SessionLocker
	log    *slog.Logger
	lockID int64

	// holder is the best-effort "who has it" probe, injectable for tests.
	holder func(ctx context.Context, conn *sql.Conn, lockID int64) (int32, bool)

	// lockWaitNanos is the measured acquisition wait, read back into the
	// Summary. Atomic because goose owns the goroutine that calls SessionLock
	// and the reader is the caller's.
	lockWaitNanos atomic.Int64
}

var _ lock.SessionLocker = (*lockObserver)(nil)

func (o *lockObserver) waited() time.Duration {
	return time.Duration(o.lockWaitNanos.Load())
}

// SessionLock acquires the advisory lock, waiting for a concurrent migrator to
// finish if one holds it.
func (o *lockObserver) SessionLock(ctx context.Context, conn *sql.Conn) error {
	started := time.Now()

	if o.holder != nil {
		if pid, held := o.holder(ctx, conn, o.lockID); held {
			o.log.Warn("advisory lock is held by another migrator; waiting for it to finish",
				slog.Int64("advisory_lock_id", o.lockID),
				slog.Int("lock_holder_backend_pid", int(pid)),
				slog.String("lock_wait_ceiling", lockWaitCeiling.String()),
			)
		} else {
			o.log.Debug("acquiring advisory lock",
				slog.Int64("advisory_lock_id", o.lockID),
			)
		}
	}

	if err := o.inner.SessionLock(ctx, conn); err != nil {
		waited := time.Since(started)
		o.lockWaitNanos.Store(int64(waited))
		return fmt.Errorf("migrate: acquire advisory lock %d after waiting %s (ceiling %s): %w",
			o.lockID, waited.Round(time.Millisecond), lockWaitCeiling, err)
	}

	waited := time.Since(started)
	o.lockWaitNanos.Store(int64(waited))
	o.log.Info("advisory lock acquired",
		slog.Int64("advisory_lock_id", o.lockID),
		slog.Int64("lock_wait_ms", waited.Milliseconds()),
		slog.Int("backend_pid", int(backendPID(ctx, conn))),
	)
	return nil
}

// SessionUnlock releases the advisory lock.
func (o *lockObserver) SessionUnlock(ctx context.Context, conn *sql.Conn) error {
	if err := o.inner.SessionUnlock(ctx, conn); err != nil {
		// Not fatal to the migration — the work is committed and Postgres
		// releases session advisory locks when the connection closes — but it
		// must be visible, because a stuck lock makes the NEXT migrator wait.
		o.log.Error("failed to release advisory lock; it will be released when the session ends",
			slog.Int64("advisory_lock_id", o.lockID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("migrate: release advisory lock %d: %w", o.lockID, err)
	}
	o.log.Info("advisory lock released", slog.Int64("advisory_lock_id", o.lockID))
	return nil
}

// pgAdvisoryLockHolder reports the backend pid currently holding lockID, if any.
//
// A single-argument pg_advisory_lock(bigint) is recorded in pg_locks with the
// high 32 bits of the key in classid and the low 32 in objid, so the key is
// reassembled rather than compared field by field.
//
// Strictly diagnostics: every failure path returns "not held" rather than an
// error, because failing a migration over a logging query would be absurd.
func pgAdvisoryLockHolder(ctx context.Context, conn *sql.Conn, lockID int64) (int32, bool) {
	if conn == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, holderProbeTimeout)
	defer cancel()

	const q = `SELECT pid FROM pg_locks
	            WHERE locktype = 'advisory'
	              AND granted
	              AND ((classid::bigint << 32) | objid::bigint) = $1
	            LIMIT 1`
	var pid int32
	if err := conn.QueryRowContext(ctx, q, lockID).Scan(&pid); err != nil {
		return 0, false
	}
	return pid, true
}

// backendPID reports this session's Postgres pid, so a waiting migrator's
// "lock_holder_backend_pid" can be matched against the winner's "backend_pid"
// in two containers' logs. Best effort: 0 when unavailable.
func backendPID(ctx context.Context, conn *sql.Conn) int32 {
	if conn == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, holderProbeTimeout)
	defer cancel()

	var pid int32
	if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		return 0
	}
	return pid
}
