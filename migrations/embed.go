// Package migrations carries the goose SQL migrations and the one //go:embed
// directive that compiles them into the migrate binary.
//
// # Why this file lives here and not under internal/
//
// A //go:embed pattern is resolved relative to the directory of the file that
// carries it, and the pattern may not contain "..". No file under internal/ or
// cmd/ can therefore reach migrations/ at the repository root. The embedding
// file has to live in a directory that can see the .sql files, which leaves two
// candidates: the repository root, or this directory. This directory is
// strictly better:
//
//   - The directive sits next to the files it embeds. `make migrate-create`
//     scaffolds NNNNN_name.sql straight into this directory and it is embedded
//     with no second edit, so the SQL on disk and the SQL inside the binary
//     cannot drift.
//   - A root-level package would put a stray Go file at the top of the
//     repository purely to work around an embed rule, and would still have to
//     name this directory in its pattern.
//
// Embedding rather than mounting is required, not preferred: the migrate image
// is gcr.io/distroless/static:nonroot with a single executable in it, and a
// Kubernetes Job has no repository checkout to bind-mount. Shipping the SQL
// beside the binary would also let the image and the schema it claims to apply
// drift apart silently, which is the one failure mode a migration runner must
// never have.
//
// # Interaction with the goose CLI
//
// Adding a .go file to this directory does not change what `goose -dir
// migrations` sees. Both of goose's collectors derive a migration's version
// from the numeric prefix of the basename and skip any file whose basename has
// none — the Provider collector calls collectFilesystemSources with strict=false
// (provider.go), and the legacy CLI collector `continue`s on the same parse
// failure (migrate.go). "embed.go" has no numeric prefix, so
// `make migrate-status`, `make migrate-down` and `make migrate-dry-run` are
// unaffected.
package migrations

import "embed"

// FS holds every goose migration in this directory, rooted at the root of the
// FS: migrations/00001_extensions_and_enums.sql is the FS entry
// "00001_extensions_and_enums.sql". goose's Provider expects exactly that
// shape, so no fs.Sub is needed.
//
// The pattern is "*.sql" and not "all:*" on purpose — this file, and any helper
// added later, must not end up inside the filesystem goose walks. embed.FS is
// immutable, so exporting it as a package-level variable does not create the
// global mutable state CLAUDE.md §12 forbids.
//
//go:embed *.sql
var FS embed.FS
