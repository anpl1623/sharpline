// Package pgstore is the Postgres implementation of the persistence seams
// internal/auth declares.
//
// It is a separate package from internal/auth on purpose. Authentication rules
// — timing equality, rotation, reuse handling, replay prevention — are testable
// with a map, and keeping them in a package that does not import a database
// driver is what makes that true rather than aspirational. Everything here is
// SQL and error mapping; there is no policy in this package, and a review that
// finds one should move it.
//
// # Why the SQL is hand-written and not sqlc
//
// The rest of this repository generates its access layer with sqlc from
// internal/platform/postgres/queries. This package does not, and the reason is
// [Store.Rotate].
//
// Rotation is not a query. It is a decision tree over a locked row — is the
// family revoked, does it predate the password change, has the token expired,
// has it already been redeemed — where the branch taken determines whether the
// next statement is an UPDATE, an INSERT, a revocation, or a rollback followed
// by a revocation in a fresh transaction. sqlc generates one function per
// statement and has no way to express the control flow between them, so the
// generated code would be six functions that a hand-written orchestrator calls
// anyway, plus a second place for the query text to drift from the schema.
//
// The rest of the statements here are ordinary and could be generated; they are
// kept alongside the one that cannot be, so that all of authentication's SQL is
// in one file rather than split by which tool happened to be able to express it.
//
// # Every constraint in migrations/00005 is load-bearing here
//
// This package does not re-implement the invariants that migration encodes. It
// leans on them:
//
//   - users.email UNIQUE detects a duplicate registration. A SELECT-then-INSERT
//     would race and the loser would get a 500 instead of ErrEmailTaken.
//   - refresh_tokens_one_successor makes a token rotatable at most once. It is
//     what turns "two concurrent redemptions of one token" from a race into a
//     unique violation, and honouring that violation is how reuse detection
//     stops depending on the application winning a read.
//   - refresh_tokens_append_only refuses any edit to an issued token beyond the
//     once-only used_at transition, so a bug in this file cannot resurrect a
//     revoked lineage or clear a redemption.
//   - user_totp's ON CONFLICT guard refuses to overwrite a CONFIRMED enrolment,
//     which is the difference between "replace a mis-scan" and "an attacker
//     with a session replaces your second factor".
package pgstore
