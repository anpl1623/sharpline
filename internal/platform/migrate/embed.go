package migrate

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/anpl1623/sharpline/migrations"
	"github.com/pressly/goose/v3"
)

// ErrNoMigrations means the binary carries an empty migration set. It is always
// a build defect rather than an operational state: if the embed pattern matched
// nothing, `up` would report "0 applied" and exit 0, and api would then start
// against an empty schema. Failing here is the whole point.
var ErrNoMigrations = errors.New("migrate: no migrations are embedded in this binary")

// ErrMalformedMigration means an embedded file is not a usable goose migration
// — a name goose cannot derive a version from, or a body missing one of the
// required direction markers.
var ErrMalformedMigration = errors.New("migrate: malformed migration")

// goose direction markers. A migration missing the Up marker applies nothing;
// one missing the Down marker is not reversible, which CLAUDE.md §12 requires
// ("every one is reversible in review before it is applied").
const (
	markerUp             = "-- +goose Up"
	markerDown           = "-- +goose Down"
	markerStatementBegin = "-- +goose StatementBegin"
	markerStatementEnd   = "-- +goose StatementEnd"
)

// EmbeddedFS returns the migration set compiled into this binary.
//
// It is returned as an fs.FS rather than the concrete embed.FS so that callers
// — and tests — can substitute a different filesystem without the package
// growing a knob for it.
func EmbeddedFS() fs.FS { return migrations.FS }

// Source identifies one embedded migration.
type Source struct {
	// Version is the numeric prefix goose orders migrations by.
	Version int64
	// Name is the basename as embedded, e.g. "00001_extensions_and_enums.sql".
	Name string
}

// Sources lists every migration in fsys, ordered by version.
//
// It is the inventory every command reports and the left-hand side of the drift
// check in Runner.Run: a version applied in the database with no source here
// means the database was migrated by a newer binary than this one.
func Sources(fsys fs.FS) ([]Source, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("migrate: glob embedded migrations: %w", err)
	}
	if len(names) == 0 {
		return nil, ErrNoMigrations
	}

	sources := make([]Source, 0, len(names))
	seen := make(map[int64]string, len(names))
	for _, name := range names {
		base := path.Base(name)
		version, err := goose.NumericComponent(base)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: no numeric version prefix: %w", ErrMalformedMigration, base, err)
		}
		if version <= 0 {
			// goose reserves version 0 for the row it writes when it creates
			// its bookkeeping table, so a file claiming 0 would collide with it.
			return nil, fmt.Errorf("%w: %s: version %d is not positive", ErrMalformedMigration, base, version)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("%w: version %d is claimed by both %s and %s",
				ErrMalformedMigration, version, other, base)
		}
		seen[version] = base
		sources = append(sources, Source{Version: version, Name: base})
	}

	slices.SortFunc(sources, func(a, b Source) int {
		switch {
		case a.Version < b.Version:
			return -1
		case a.Version > b.Version:
			return 1
		default:
			return 0
		}
	})
	return sources, nil
}

// Versions projects sources onto their version numbers, in order.
func Versions(sources []Source) []int64 {
	versions := make([]int64, len(sources))
	for i, s := range sources {
		versions[i] = s.Version
	}
	return versions
}

// Names projects sources onto their file names, in order.
func Names(sources []Source) []string {
	names := make([]string, len(sources))
	for i, s := range sources {
		names[i] = s.Name
	}
	return names
}

// CheckStructure verifies every embedded migration is structurally a goose
// migration: it declares both directions and its StatementBegin/StatementEnd
// markers are balanced.
//
// This is deliberately a STRUCTURAL check, not a SQL parse. Nothing short of a
// server can tell you that a CREATE TRIGGER body is valid, and goose's SQL
// parser is in an internal/ package that cannot be imported. Proving the SQL
// itself applies and rolls back is the job of `make migrate-dry-run`, which
// runs the whole set up, down to 0, and up again against a throwaway
// TimescaleDB container. What this catches is the authoring mistake that the
// dry run reports as a confusing runtime error instead of a naming problem: a
// missing `-- +goose Down` section, or a StatementBegin left unterminated.
func CheckStructure(fsys fs.FS, sources []Source) error {
	var problems []error
	for _, s := range sources {
		body, err := fs.ReadFile(fsys, s.Name)
		if err != nil {
			problems = append(problems, fmt.Errorf("read %s: %w", s.Name, err))
			continue
		}
		if err := checkBody(s.Name, string(body)); err != nil {
			problems = append(problems, err)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %w", ErrMalformedMigration, errors.Join(problems...))
	}
	return nil
}

// checkBody applies the structural rules to one migration body. Split out from
// CheckStructure so it is testable against strings without a filesystem.
func checkBody(name, body string) error {
	var problems []error

	upAt := markerIndex(body, markerUp)
	downAt := markerIndex(body, markerDown)
	switch {
	case upAt < 0:
		problems = append(problems, fmt.Errorf("%s: missing %q", name, markerUp))
	case downAt < 0:
		problems = append(problems, fmt.Errorf("%s: missing %q — a migration that cannot be rolled back is not reviewable (CLAUDE.md §12)", name, markerDown))
	case downAt < upAt:
		problems = append(problems, fmt.Errorf("%s: %q appears before %q", name, markerDown, markerUp))
	}

	begins := markerCount(body, markerStatementBegin)
	ends := markerCount(body, markerStatementEnd)
	if begins != ends {
		problems = append(problems, fmt.Errorf("%s: %d %q markers but %d %q markers",
			name, begins, markerStatementBegin, ends, markerStatementEnd))
	}

	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	return nil
}

// markerIndex returns the byte offset of the first line that is exactly marker
// (ignoring trailing whitespace), or -1. Whole-line matching matters: the
// string "-- +goose Up" inside a comment or a function body is not a directive,
// and goose itself only honours it at the start of a line.
func markerIndex(body, marker string) int {
	offset := 0
	for _, line := range strings.SplitAfter(body, "\n") {
		if strings.TrimRight(line, " \t\r\n") == marker {
			return offset
		}
		offset += len(line)
	}
	return -1
}

// markerCount counts whole lines equal to marker.
func markerCount(body, marker string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimRight(line, " \t\r") == marker {
			n++
		}
	}
	return n
}
