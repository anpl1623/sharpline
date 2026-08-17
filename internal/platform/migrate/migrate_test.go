package migrate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3/lock"
)

// unreachableDSN is syntactically valid and points at a port nothing listens on.
// Every test that uses it relies on database/sql opening lazily: New must do no
// I/O, so the DSN is never dialed. If a future change adds a connection to New,
// these tests start failing with a dial error, which is the correct alarm.
const unreachableDSN = "postgres://sharpline:throwaway@127.0.0.1:1/sharpline_test?sslmode=disable"

// -----------------------------------------------------------------------------
// argv parsing
// -----------------------------------------------------------------------------

func TestParseArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		want    Invocation
		wantErr bool
	}{
		{
			// The compose service and the k8s Job both run the ENTRYPOINT with
			// no arguments, so this case is the production path.
			name: "no arguments defaults to up",
			args: nil,
			want: Invocation{Command: CommandUp},
		},
		{name: "empty slice defaults to up", args: []string{}, want: Invocation{Command: CommandUp}},
		{name: "up", args: []string{"up"}, want: Invocation{Command: CommandUp}},
		{name: "down", args: []string{"down"}, want: Invocation{Command: CommandDown}},
		{name: "status", args: []string{"status"}, want: Invocation{Command: CommandStatus}},
		{name: "version", args: []string{"version"}, want: Invocation{Command: CommandVersion}},
		{name: "validate", args: []string{"validate"}, want: Invocation{Command: CommandValidate}},
		{
			name: "dry-run is an alias for validate",
			args: []string{"dry-run"},
			want: Invocation{Command: CommandValidate},
		},
		{
			name: "surrounding whitespace is tolerated",
			args: []string{"  status  "},
			want: Invocation{Command: CommandStatus},
		},
		{name: "up-to with version", args: []string{"up-to", "3"}, want: Invocation{Command: CommandUpTo, TargetVersion: 3}},
		{
			// down-to 0 is how the dry run proves reversibility.
			name: "down-to zero",
			args: []string{"down-to", "0"},
			want: Invocation{Command: CommandDownTo, TargetVersion: 0},
		},
		{name: "unknown command", args: []string{"upgrade"}, wantErr: true},
		{name: "healthcheck is not a migrate subcommand", args: []string{"healthcheck"}, wantErr: true},
		{name: "up rejects extra arguments", args: []string{"up", "3"}, wantErr: true},
		{name: "status rejects extra arguments", args: []string{"status", "--verbose"}, wantErr: true},
		{name: "up-to requires a version", args: []string{"up-to"}, wantErr: true},
		{name: "down-to requires a version", args: []string{"down-to"}, wantErr: true},
		{name: "up-to rejects two versions", args: []string{"up-to", "1", "2"}, wantErr: true},
		{name: "version must be an integer", args: []string{"up-to", "head"}, wantErr: true},
		{name: "version must not be negative", args: []string{"down-to", "-1"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseArgs(%q) = %+v, want an error", tt.args, got)
				}
				if !errors.Is(err, ErrUsage) {
					t.Fatalf("ParseArgs(%q) error = %v, want it to wrap ErrUsage", tt.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseArgs(%q) unexpected error: %v", tt.args, err)
			}
			if got != tt.want {
				t.Fatalf("ParseArgs(%q) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

func TestCommandMutates(t *testing.T) {
	t.Parallel()

	// Mutates answers "does this change the schema", which is not the same as
	// "does this take the advisory lock" — goose locks around Status and
	// GetDBVersion too. See the method's comment.
	mutating := map[Command]bool{
		CommandUp:       true,
		CommandUpTo:     true,
		CommandDown:     true,
		CommandDownTo:   true,
		CommandStatus:   false,
		CommandVersion:  false,
		CommandValidate: false,
	}
	for cmd, want := range mutating {
		if got := cmd.Mutates(); got != want {
			t.Errorf("Command(%q).Mutates() = %t, want %t", cmd, got, want)
		}
	}
}

func TestUsageDocumentsEveryCommand(t *testing.T) {
	t.Parallel()

	usage := Usage()
	for _, cmd := range []string{
		string(CommandUp), string(CommandUpTo), string(CommandDown), string(CommandDownTo),
		string(CommandStatus), string(CommandVersion), string(CommandValidate), commandDryRun,
	} {
		if !strings.Contains(usage, cmd) {
			t.Errorf("Usage() does not mention %q; an operator reading a crash-looping Job has only this text", cmd)
		}
	}
	// The exit-code contract is the other half of what an operator needs.
	for _, want := range []string{"exit codes", "SHARPLINE_POSTGRES_DSN"} {
		if !strings.Contains(usage, want) {
			t.Errorf("Usage() does not mention %q", want)
		}
	}
}

// -----------------------------------------------------------------------------
// the embedded migration set
// -----------------------------------------------------------------------------

// TestEmbeddedSetMatchesRepository is the drift guard on the embed itself.
//
// The failure it exists to catch: someone replaces the "*.sql" pattern with an
// explicit file list, adds an eighth migration, and ships a binary that silently
// applies only seven. Nothing else in the build would notice.
func TestEmbeddedSetMatchesRepository(t *testing.T) {
	t.Parallel()

	// The test's working directory is the package directory, so the repository
	// root is three levels up.
	dir := filepath.Join("..", "..", "..", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var onDisk []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			onDisk = append(onDisk, e.Name())
		}
	}
	if len(onDisk) == 0 {
		t.Fatalf("no .sql files in %s; this test cannot prove anything", dir)
	}

	sources, err := Sources(EmbeddedFS())
	if err != nil {
		t.Fatalf("Sources(EmbeddedFS()): %v", err)
	}
	embedded := Names(sources)

	if len(embedded) != len(onDisk) {
		t.Fatalf("embedded %d migrations %v but %s holds %d %v",
			len(embedded), embedded, dir, len(onDisk), onDisk)
	}
	got := make(map[string]bool, len(embedded))
	for _, name := range embedded {
		got[name] = true
	}
	for _, name := range onDisk {
		if !got[name] {
			t.Errorf("%s is on disk but is not embedded in the binary", name)
		}
	}
}

// TestEmbeddedFSHoldsOnlySQL proves the embed pattern is "*.sql" and not a
// wildcard: the Go file carrying the directive must not end up inside the
// filesystem goose walks.
func TestEmbeddedFSHoldsOnlySQL(t *testing.T) {
	t.Parallel()

	err := fs.WalkDir(EmbeddedFS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".sql") {
			t.Errorf("embedded filesystem contains a non-SQL entry: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded filesystem: %v", err)
	}
}

func TestEmbeddedNamesAreGooseParseable(t *testing.T) {
	t.Parallel()

	// Lenient on purpose: goose derives the version from any leading run of
	// digits, so both the sequential 00001_ style this repository uses and
	// goose's default timestamp style are valid. What is asserted is that every
	// name HAS a version and a description, since a name goose cannot parse is
	// skipped silently rather than rejected.
	nameRE := regexp.MustCompile(`^[0-9]+_[^.]+\.sql$`)

	sources, err := Sources(EmbeddedFS())
	if err != nil {
		t.Fatalf("Sources(EmbeddedFS()): %v", err)
	}
	var previous int64
	for _, s := range sources {
		if !nameRE.MatchString(s.Name) {
			t.Errorf("migration %q does not match %s", s.Name, nameRE)
		}
		if s.Version <= previous {
			t.Errorf("migration %q version %d does not exceed the previous version %d", s.Name, s.Version, previous)
		}
		previous = s.Version
	}
}

func TestEmbeddedMigrationsAreStructurallyValid(t *testing.T) {
	t.Parallel()

	sources, err := Sources(EmbeddedFS())
	if err != nil {
		t.Fatalf("Sources(EmbeddedFS()): %v", err)
	}
	if err := CheckStructure(EmbeddedFS(), sources); err != nil {
		t.Fatalf("CheckStructure over the real migration set: %v", err)
	}
}

func TestSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		files    fstest.MapFS
		wantErr  error
		wantName []string
	}{
		{
			name:    "empty filesystem is a build defect",
			files:   fstest.MapFS{},
			wantErr: ErrNoMigrations,
		},
		{
			name: "only non-sql files is also empty",
			files: fstest.MapFS{
				"embed.go": &fstest.MapFile{Data: []byte("package migrations")},
			},
			wantErr: ErrNoMigrations,
		},
		{
			name: "sorted by version, not by name",
			files: fstest.MapFS{
				"00010_ten.sql": &fstest.MapFile{},
				"00002_two.sql": &fstest.MapFile{},
				"00001_one.sql": &fstest.MapFile{},
			},
			wantName: []string{"00001_one.sql", "00002_two.sql", "00010_ten.sql"},
		},
		{
			name: "no numeric prefix",
			files: fstest.MapFS{
				"schema.sql": &fstest.MapFile{},
			},
			wantErr: ErrMalformedMigration,
		},
		{
			name: "duplicate version",
			files: fstest.MapFS{
				"00001_one.sql":     &fstest.MapFile{},
				"00001_one_bis.sql": &fstest.MapFile{},
			},
			wantErr: ErrMalformedMigration,
		},
		{
			// goose writes a version_id 0 row when it creates its bookkeeping
			// table, so a migration claiming 0 would collide with it.
			name: "version zero is reserved by goose",
			files: fstest.MapFS{
				"00000_zero.sql": &fstest.MapFile{},
			},
			wantErr: ErrMalformedMigration,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Sources(tt.files)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Sources() error = %v, want it to wrap %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Sources() unexpected error: %v", err)
			}
			names := Names(got)
			if len(names) != len(tt.wantName) {
				t.Fatalf("Sources() = %v, want %v", names, tt.wantName)
			}
			for i := range names {
				if names[i] != tt.wantName[i] {
					t.Fatalf("Sources() = %v, want %v", names, tt.wantName)
				}
			}
		})
	}
}

func TestVersions(t *testing.T) {
	t.Parallel()

	got := Versions([]Source{{Version: 1, Name: "a"}, {Version: 7, Name: "b"}})
	if len(got) != 2 || got[0] != 1 || got[1] != 7 {
		t.Fatalf("Versions() = %v, want [1 7]", got)
	}
}

func TestCheckBody(t *testing.T) {
	t.Parallel()

	const good = "-- +goose Up\ncreate table t ();\n-- +goose Down\ndrop table t;\n"

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "up and down", body: good},
		{
			name: "statement blocks balanced",
			body: "-- +goose Up\n-- +goose StatementBegin\nselect 1;\n-- +goose StatementEnd\n" +
				"-- +goose Down\n-- +goose StatementBegin\nselect 2;\n-- +goose StatementEnd\n",
		},
		{
			name: "trailing whitespace after a marker is still a marker",
			body: "-- +goose Up  \ncreate table t ();\n-- +goose Down\t\ndrop table t;\n",
		},
		{
			// The migration set is full of trigger and function bodies; a marker
			// appearing mid-line inside one of them is not a directive, and
			// goose does not treat it as one either.
			name: "marker text inside a statement is not a directive",
			body: "-- +goose Up\nselect '-- +goose Down' as not_a_marker;\n-- +goose Down\ndrop table t;\n",
		},
		{name: "missing up", body: "-- +goose Down\ndrop table t;\n", wantErr: true},
		{name: "missing down is not reversible", body: "-- +goose Up\ncreate table t ();\n", wantErr: true},
		{
			name:    "down before up",
			body:    "-- +goose Down\ndrop table t;\n-- +goose Up\ncreate table t ();\n",
			wantErr: true,
		},
		{
			name: "unterminated statement block",
			body: "-- +goose Up\n-- +goose StatementBegin\nselect 1;\n-- +goose Down\ndrop table t;\n",
			// One StatementBegin, zero StatementEnd.
			wantErr: true,
		},
		{name: "empty file", body: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := checkBody("test.sql", tt.body)
			if tt.wantErr && err == nil {
				t.Fatalf("checkBody() = nil, want an error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("checkBody() unexpected error: %v", err)
			}
		})
	}
}

func TestCheckStructureReportsEveryBadFileAtOnce(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"00001_no_down.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nselect 1;\n")},
		"00002_no_up.sql":   &fstest.MapFile{Data: []byte("-- +goose Down\nselect 1;\n")},
	}
	sources, err := Sources(files)
	if err != nil {
		t.Fatalf("Sources(): %v", err)
	}
	err = CheckStructure(files, sources)
	if err == nil {
		t.Fatal("CheckStructure() = nil, want an error")
	}
	if !errors.Is(err, ErrMalformedMigration) {
		t.Fatalf("CheckStructure() error = %v, want it to wrap ErrMalformedMigration", err)
	}
	// Both problems in one error, so an author fixes them in one pass — the same
	// property internal/platform/config gives configuration failures.
	for _, want := range []string{"00001_no_down.sql", "00002_no_up.sql"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CheckStructure() error does not mention %s: %v", want, err)
		}
	}
}

// -----------------------------------------------------------------------------
// the drift rule
// -----------------------------------------------------------------------------

func TestUnknownApplied(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		embedded []int64
		applied  []int64
		want     []int64
	}{
		{name: "fresh database", embedded: []int64{1, 2, 3}, applied: nil, want: nil},
		{name: "fully applied", embedded: []int64{1, 2, 3}, applied: []int64{1, 2, 3}, want: nil},
		{name: "partially applied is fine", embedded: []int64{1, 2, 3}, applied: []int64{1}, want: nil},
		{
			// The rollback-to-an-older-image case. This is the whole reason the
			// check exists.
			name:     "database ahead of the binary",
			embedded: []int64{1, 2, 3},
			applied:  []int64{1, 2, 3, 4},
			want:     []int64{4},
		},
		{
			name:     "several unknown versions",
			embedded: []int64{1},
			applied:  []int64{1, 8, 9},
			want:     []int64{8, 9},
		},
		{
			// A file deleted from the repository after being applied is also
			// drift, even though the version number is low.
			name:     "applied version whose file was removed",
			embedded: []int64{1, 3},
			applied:  []int64{1, 2, 3},
			want:     []int64{2},
		},
		{name: "no migrations embedded at all", embedded: nil, applied: []int64{1}, want: []int64{1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := UnknownApplied(tt.embedded, tt.applied)
			if len(got) != len(tt.want) {
				t.Fatalf("UnknownApplied(%v, %v) = %v, want %v", tt.embedded, tt.applied, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("UnknownApplied(%v, %v) = %v, want %v", tt.embedded, tt.applied, got, tt.want)
				}
			}
		})
	}
}

func TestCheckDrift(t *testing.T) {
	t.Parallel()

	if err := checkDrift([]int64{1, 2, 3}, []int64{1, 2}, 3, 3); err != nil {
		t.Fatalf("checkDrift() on a partially applied database: %v", err)
	}

	err := checkDrift([]int64{1, 2, 3}, []int64{1, 2, 3, 4}, 3, 3)
	if !errors.Is(err, ErrSchemaAhead) {
		t.Fatalf("checkDrift() error = %v, want it to wrap ErrSchemaAhead", err)
	}
	// The message has to name the offending version and the shape of the
	// mismatch, because the operator's next move is choosing which image to
	// deploy.
	for _, want := range []string{"[4]", "3 embedded", "highest 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("checkDrift() error does not mention %q: %v", want, err)
		}
	}
}

// -----------------------------------------------------------------------------
// construction
// -----------------------------------------------------------------------------

func TestNewValidation(t *testing.T) {
	t.Parallel()

	discard := slog.New(slog.NewJSONHandler(new(bytes.Buffer), nil))

	tests := []struct {
		name    string
		opts    Options
		wantErr error
	}{
		{
			name:    "empty DSN",
			opts:    Options{Logger: discard},
			wantErr: ErrInvalidOptions,
		},
		{
			name:    "whitespace DSN",
			opts:    Options{DSN: "   ", Logger: discard},
			wantErr: ErrInvalidOptions,
		},
		{
			name:    "nil logger",
			opts:    Options{DSN: unreachableDSN},
			wantErr: ErrInvalidOptions,
		},
		{
			name:    "unparseable DSN",
			opts:    Options{DSN: "postgres://%zz", Logger: discard},
			wantErr: ErrInvalidOptions,
		},
		{
			name:    "empty migration set",
			opts:    Options{DSN: unreachableDSN, Logger: discard, FS: fstest.MapFS{}},
			wantErr: ErrNoMigrations,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner, err := New(tt.opts)
			if runner != nil {
				t.Cleanup(func() { _ = runner.Close() })
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("New() error = %v, want it to wrap %v", err, tt.wantErr)
			}
		})
	}
}

// TestNewOverEmbeddedSet proves goose accepts the embedded filesystem and finds
// every migration in it, without touching a database.
func TestNewOverEmbeddedSet(t *testing.T) {
	t.Parallel()

	runner, err := New(Options{DSN: unreachableDSN, Logger: slog.New(slog.NewJSONHandler(new(bytes.Buffer), nil))})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(func() {
		if err := runner.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})

	onDisk, err := Sources(EmbeddedFS())
	if err != nil {
		t.Fatalf("Sources(): %v", err)
	}
	got := runner.provider.ListSources()
	if len(got) != len(onDisk) {
		t.Fatalf("goose collected %d sources, the embedded set has %d", len(got), len(onDisk))
	}
	for i, src := range got {
		if src.Version != onDisk[i].Version {
			t.Errorf("source %d: goose version %d, embedded version %d", i, src.Version, onDisk[i].Version)
		}
	}

	// The default lock id must match the goose CLI's, or `make migrate` and
	// `make migrate-down` would not exclude each other.
	if runner.lockID != lock.DefaultLockID {
		t.Errorf("default lock id = %d, want goose's DefaultLockID %d", runner.lockID, lock.DefaultLockID)
	}
}

func TestNewAcceptsLibpqKeywordDSN(t *testing.T) {
	t.Parallel()

	// internal/platform/config validates both DSN forms, so both must open.
	runner, err := New(Options{
		DSN:    "host=127.0.0.1 port=1 user=sharpline password=throwaway dbname=sharpline_test sslmode=disable",
		Logger: slog.New(slog.NewJSONHandler(new(bytes.Buffer), nil)),
	})
	if err != nil {
		t.Fatalf("New() with a libpq keyword/value DSN: %v", err)
	}
	if err := runner.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
}

// -----------------------------------------------------------------------------
// the advisory-lock observer
// -----------------------------------------------------------------------------

type fakeSessionLocker struct {
	lockDelay time.Duration
	lockErr   error
	unlockErr error
	locks     int
	unlocks   int
}

func (f *fakeSessionLocker) SessionLock(_ context.Context, _ *sql.Conn) error {
	f.locks++
	if f.lockDelay > 0 {
		time.Sleep(f.lockDelay)
	}
	return f.lockErr
}

func (f *fakeSessionLocker) SessionUnlock(_ context.Context, _ *sql.Conn) error {
	f.unlocks++
	return f.unlockErr
}

// newObserver wires a lockObserver over a fake locker, capturing its log.
func newObserver(t *testing.T, inner lock.SessionLocker, holderPID int32, held bool) (*lockObserver, *bytes.Buffer) {
	t.Helper()

	buf := new(bytes.Buffer)
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &lockObserver{
		inner:  inner,
		log:    log,
		lockID: lock.DefaultLockID,
		holder: func(context.Context, *sql.Conn, int64) (int32, bool) { return holderPID, held },
	}, buf
}

// logLines decodes the captured JSON log into one map per line.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func findLine(lines []map[string]any, msg string) map[string]any {
	for _, l := range lines {
		if l["msg"] == msg {
			return l
		}
	}
	return nil
}

func TestLockObserverUncontendedAcquisition(t *testing.T) {
	t.Parallel()

	inner := &fakeSessionLocker{}
	obs, buf := newObserver(t, inner, 0, false)

	if err := obs.SessionLock(context.Background(), nil); err != nil {
		t.Fatalf("SessionLock(): %v", err)
	}
	if inner.locks != 1 {
		t.Fatalf("inner locker called %d times, want 1", inner.locks)
	}

	lines := logLines(t, buf)
	if findLine(lines, "advisory lock acquired") == nil {
		t.Fatalf("no acquisition line logged; got %v", buf.String())
	}
	if findLine(lines, "advisory lock is held by another migrator; waiting for it to finish") != nil {
		t.Error("logged a contention warning for an uncontended lock")
	}

	if err := obs.SessionUnlock(context.Background(), nil); err != nil {
		t.Fatalf("SessionUnlock(): %v", err)
	}
	if inner.unlocks != 1 {
		t.Fatalf("inner unlocker called %d times, want 1", inner.unlocks)
	}
	if findLine(logLines(t, buf), "advisory lock released") == nil {
		t.Error("no release line logged")
	}
}

// TestLockObserverReportsContention is the unit-level half of the concurrency
// proof: when another migrator holds the lock, the wait is announced with the
// holder's backend pid and the measured wait is recorded for the summary.
func TestLockObserverReportsContention(t *testing.T) {
	t.Parallel()

	const holderPID = 4242
	inner := &fakeSessionLocker{lockDelay: 25 * time.Millisecond}
	obs, buf := newObserver(t, inner, holderPID, true)

	if err := obs.SessionLock(context.Background(), nil); err != nil {
		t.Fatalf("SessionLock(): %v", err)
	}

	warn := findLine(logLines(t, buf), "advisory lock is held by another migrator; waiting for it to finish")
	if warn == nil {
		t.Fatalf("no contention warning logged; got %v", buf.String())
	}
	if got, ok := warn["lock_holder_backend_pid"].(float64); !ok || int32(got) != holderPID {
		t.Errorf("lock_holder_backend_pid = %v, want %d", warn["lock_holder_backend_pid"], holderPID)
	}
	if warn["level"] != "WARN" {
		t.Errorf("contention line level = %v, want WARN", warn["level"])
	}

	if waited := obs.waited(); waited < 25*time.Millisecond {
		t.Errorf("waited() = %s, want at least the 25ms the inner locker blocked for", waited)
	}
}

func TestLockObserverPropagatesFailure(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("lock timeout")
	inner := &fakeSessionLocker{lockErr: sentinel}
	obs, _ := newObserver(t, inner, 0, false)

	err := obs.SessionLock(context.Background(), nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("SessionLock() error = %v, want it to wrap %v", err, sentinel)
	}
	// The message has to name the ceiling, because "could not get the lock" with
	// no duration is not actionable.
	if !strings.Contains(err.Error(), lockWaitCeiling.String()) {
		t.Errorf("SessionLock() error does not mention the wait ceiling %s: %v", lockWaitCeiling, err)
	}
}

func TestLockObserverPropagatesUnlockFailure(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("unlock refused")
	inner := &fakeSessionLocker{unlockErr: sentinel}
	obs, buf := newObserver(t, inner, 0, false)

	err := obs.SessionUnlock(context.Background(), nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("SessionUnlock() error = %v, want it to wrap %v", err, sentinel)
	}
	line := findLine(logLines(t, buf), "failed to release advisory lock; it will be released when the session ends")
	if line == nil {
		t.Fatalf("unlock failure was not logged; got %v", buf.String())
	}
	if line["level"] != "ERROR" {
		t.Errorf("unlock failure level = %v, want ERROR", line["level"])
	}
}

// TestLockWaitCeilingBelowMutateTimeout guards the relationship between the two
// budgets: if the outer deadline were the smaller one, a migrator that
// legitimately queued behind another would be killed by its own timeout instead
// of running when the lock came free.
func TestLockWaitCeilingBelowMutateTimeout(t *testing.T) {
	t.Parallel()

	if lockWaitCeiling >= MutateTimeout {
		t.Fatalf("lock wait ceiling %s must be comfortably below the mutate timeout %s", lockWaitCeiling, MutateTimeout)
	}
	if lockWaitCeiling != 5*time.Minute {
		t.Errorf("lock wait ceiling = %s, want 5m (period %ds x threshold %d)",
			lockWaitCeiling, lockRetryPeriodSeconds, lockRetryThreshold)
	}
}

// -----------------------------------------------------------------------------
// summary reporting
// -----------------------------------------------------------------------------

// TestSummaryLogValue pins the field names the summary line emits. They are the
// binary's only machine-readable output — migrate is deliberately not scraped by
// Prometheus (it is a run-to-completion Job with no port), so these keys are
// what a log query has to match.
func TestSummaryLogValue(t *testing.T) {
	t.Parallel()

	buf := new(bytes.Buffer)
	log := slog.New(slog.NewJSONHandler(buf, nil))
	summary := Summary{
		Command:       CommandUp,
		Embedded:      7,
		VersionBefore: 0,
		VersionAfter:  7,
		PendingBefore: 7,
		Applied: []Applied{
			{Version: 1, Name: "00001_extensions_and_enums.sql", Direction: "up", Duration: 12 * time.Millisecond},
			{Version: 2, Name: "00002_catalogue.sql", Direction: "up", Duration: 30 * time.Millisecond},
		},
		LockWait: 1500 * time.Millisecond,
		Duration: 2 * time.Second,
	}
	log.Info("completed", slog.Any("summary", summary))

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("want one log line, got %d", len(lines))
	}
	group, ok := lines[0]["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary did not log as a group: %v", lines[0]["summary"])
	}

	want := map[string]any{
		"command":                   "up",
		"migrations_embedded":       float64(7),
		"schema_version_before":     float64(0),
		"schema_version_after":      float64(7),
		"migrations_pending_before": float64(7),
		"migrations_applied":        float64(2),
		"lock_wait_ms":              float64(1500),
		"duration_ms":               float64(2000),
	}
	for key, wantValue := range want {
		got, present := group[key]
		if !present {
			t.Errorf("summary is missing key %q", key)
			continue
		}
		if got != wantValue {
			t.Errorf("summary[%q] = %v, want %v", key, got, wantValue)
		}
	}

	versions, ok := group["applied_versions"].([]any)
	if !ok || len(versions) != 2 {
		t.Fatalf("applied_versions = %v, want two entries", group["applied_versions"])
	}
	names, ok := group["applied_names"].([]any)
	if !ok || len(names) != 2 || names[0] != "00001_extensions_and_enums.sql" {
		t.Fatalf("applied_names = %v, want the two migration file names", group["applied_names"])
	}
}

func TestAppliedFromToleratesNilResults(t *testing.T) {
	t.Parallel()

	// goose returns a nil *MigrationResult from Down when there was nothing to
	// roll back; the conversion must not panic on it.
	if got := appliedFrom(nil); len(got) != 0 {
		t.Fatalf("appliedFrom(nil) = %v, want empty", got)
	}
}
