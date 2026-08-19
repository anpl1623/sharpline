// Package auth is the domain of authentication: credentials, sessions, and the
// second factor. It is phase 5's security core (CLAUDE.md §11).
//
// CLAUDE.md §6 states the requirement in one sentence, and every clause of it
// is implemented here:
//
//	"email/password auth with argon2id; JWT access tokens plus rotating refresh
//	 tokens with reuse detection; optional TOTP 2FA; play-money balance;
//	 responsible-gaming-style self-imposed limits"
//
// The two clauses that are NOT implemented here are deliberate. The play-money
// balance is a fold over ledger_entries (CLAUDE.md §4: "Balances are derived,
// never stored as a mutable field") and belongs to internal/betting. The limits
// themselves are evaluated against wagers and ledger entries, so this package
// owns only their vocabulary — [LimitKind] and [LimitPeriod] — which
// migrations/00005 explicitly delegated to phase 5.
//
// # No HTTP, no transport
//
// Nothing in this package imports net/http, reads a header, sets a cookie, or
// knows what a status code is. It hands back values — an access token, a
// refresh token, an error from the taxonomy in errors.go — and internal/httpapi
// decides how those become a response. That boundary is what lets the whole of
// authentication be tested without a server.
//
// # What is deliberately absent
//
// CLAUDE.md §0: Sharpline is "not a licensed sportsbook. No real money moves.
// No KYC, no geolocation gating, no payment processing, no custody of funds."
// migrations/00005 carries the same list at length and asks that it be kept.
// So this package has no identity document, no date of birth, no legal name, no
// address, no payment instrument, and no geolocation of any kind — not even an
// IP address, which appears only in the audit log and is not modelled here.
//
// # Redaction is structural
//
// The brief for this phase says a password, a token, a TOTP secret or a JWT
// must never reach a log, a span attribute, or an error message, and that the
// redaction "must be structural, not remembered". Remembering does not scale:
// one `slog.String("token", t)` written at 2am defeats a policy.
//
// So every secret in this package is carried in a [Secret], whose String,
// GoString and LogValue all return a fixed placeholder and whose MarshalJSON
// and MarshalText return an error. A struct containing a Secret cannot be
// printed, logged or serialised into a leak; reaching the value requires
// calling [Secret.Expose], which is greppable and reviewable. Errors returned
// from this package never interpolate a credential — the untrusted values they
// do carry (an email address on a validation failure) are truncated the way
// internal/domain truncates provider strings.
//
// # Timing
//
// A login against an unknown email and a login with a wrong password must be
// indistinguishable in body, status AND timing (this phase's brief). Body and
// status are the caller's job and this package helps by returning one sentinel,
// [ErrCredentials], for both. Timing is this package's job and is handled in
// [Service.Login]: when no user matches, the password is verified against a
// decoy hash minted at construction under the current policy parameters, so the
// argon2id work — which dominates the request by three orders of magnitude —
// happens either way. See password.go.
//
// # Threat model, stated so the choices below can be argued with
//
// Assumed capable of:
//
//   - Reading the entire database. Hence: passwords are argon2id, refresh
//     tokens are stored only as SHA-256 digests, and the TOTP shared secret is
//     AEAD ciphertext under a key that lives outside the database entirely.
//   - Stealing one refresh token from a client. Hence: rotation, and reuse
//     detection that revokes the whole lineage rather than one token.
//   - Crafting a JWT, including one whose header claims alg=none. Hence: the
//     verifier never reads `alg` to decide anything. See jwt.go.
//   - Replaying a TOTP code observed over the user's shoulder within its
//     30-second step. Hence: [ReplayGuard].
//
// Assumed NOT capable of: reading process memory, or reading the signing key
// and the TOTP key material. Those are the trust anchors; if they leak, this
// package's guarantees end, which is why they are separate values with separate
// rotation stories rather than one key doing two jobs.
package auth
