package auth

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// The roots of this package's error taxonomy. internal/domain uses the same
// shape (ErrInvalid / ErrConflict) and for the same reason: an HTTP layer needs
// coarse buckets to choose a status code, and a package with zero knowledge of
// HTTP can still offer them.
//
// There is a third root here that the domain does not need, and it is the
// important one. [ErrUnauthenticated] is the bucket every credential failure
// lands in, and it is deliberately WIDE: an unknown email, a wrong password, an
// expired token, a revoked family, a replayed TOTP code and a token that never
// existed all wrap it. A caller that maps the root to 401 with a fixed body
// cannot accidentally leak which of those it was.
var (
	// ErrInvalid is the root of every validation failure: a value that could
	// not be correct under any circumstances. Maps to 400.
	ErrInvalid = errors.New("auth: invalid value")

	// ErrConflict is the root of every state failure: a well-formed request
	// that is not legal against the state it is applied to. Maps to 409.
	ErrConflict = errors.New("auth: conflicting state")

	// ErrUnauthenticated is the root of every credential failure. Maps to 401,
	// with the SAME body for every leaf, always.
	ErrUnauthenticated = errors.New("auth: not authenticated")

	// ErrForbidden is the root of "we know who you are and you may not do
	// this". Maps to 403. Distinct from ErrUnauthenticated because retrying
	// with better credentials cannot help.
	ErrForbidden = errors.New("auth: not permitted")

	// ErrInternal is the root of failures that are the system's fault rather
	// than the caller's: a store that would not answer, a key that would not
	// decrypt. Maps to 500. It is a root rather than a bare wrap so that a
	// handler cannot mistake an infrastructure failure for a bad password and
	// return 401 — which would make an outage look like a wave of credential
	// stuffing.
	ErrInternal = errors.New("auth: internal failure")
)

// Credential and session failures. Every one of these wraps
// [ErrUnauthenticated], and the caller is expected to render all of them
// identically.
var (
	// ErrCredentials is returned for BOTH an unknown email and a wrong
	// password, and the two paths do the same amount of argon2id work. One
	// sentinel is what makes "indistinguishable in the response body" the
	// default rather than a thing the handler has to remember.
	ErrCredentials = fmt.Errorf("%w: email or password is incorrect", ErrUnauthenticated)

	// ErrSecondFactorRequired reports that the password was correct and a
	// confirmed TOTP enrolment exists, so the login is incomplete rather than
	// wrong. It necessarily distinguishes an enrolled account from a
	// non-enrolled one, but only to a caller who has ALREADY proved the
	// password, so it leaks nothing to an attacker who has not.
	ErrSecondFactorRequired = fmt.Errorf("%w: a second factor is required", ErrUnauthenticated)

	// ErrSecondFactorInvalid covers a wrong code, a code outside the skew
	// window, and a code already consumed inside its window.
	ErrSecondFactorInvalid = fmt.Errorf("%w: second factor is incorrect", ErrUnauthenticated)

	// ErrTokenUnknown means the presented refresh token hashes to nothing on
	// record. Either it was never issued, or it was swept after expiry.
	ErrTokenUnknown = fmt.Errorf("%w: refresh token is not recognised", ErrUnauthenticated)

	// ErrTokenExpired means the token was real and its expiry has passed. It is
	// separate from ErrTokenUnknown for METRICS, not for the response: a spike
	// in expiries is a clock or a lifetime problem, a spike in unknowns is a
	// scan.
	ErrTokenExpired = fmt.Errorf("%w: refresh token has expired", ErrUnauthenticated)

	// ErrTokenReuse means an already-redeemed token was presented again, or two
	// redemptions of one token raced. The lineage has been revoked by the time
	// this error is returned — the revocation is committed before the error is
	// constructed, so a caller that ignores the error still gets the protection.
	ErrTokenReuse = fmt.Errorf("%w: refresh token was already redeemed; the session lineage has been revoked", ErrUnauthenticated)

	// ErrSessionRevoked means the family was revoked before this presentation:
	// a logout, an operator action, a credential change, or an earlier reuse.
	ErrSessionRevoked = fmt.Errorf("%w: session has been revoked", ErrUnauthenticated)

	// ErrAccessTokenInvalid covers every way a presented access token fails
	// verification: malformed, wrong signature, wrong algorithm, wrong issuer,
	// wrong audience, expired, or not yet valid.
	ErrAccessTokenInvalid = fmt.Errorf("%w: access token is not valid", ErrUnauthenticated)
)

// Account-state failures.
var (
	// ErrAccountSuspended is returned AFTER the password has been verified, so
	// that an attacker who does not know the password cannot use it to discover
	// that an account exists and is suspended.
	ErrAccountSuspended = fmt.Errorf("%w: account is suspended", ErrForbidden)

	// ErrAccountClosed is likewise returned only after verification.
	ErrAccountClosed = fmt.Errorf("%w: account is closed", ErrForbidden)

	// ErrSelfExcluded is the responsible-gaming refusal (CLAUDE.md §6). It is
	// NOT returned by Login: a self-excluded customer must be able to sign in,
	// read their history and manage their exclusion. It is returned by
	// [Service.AuthorizeWagering], which internal/betting must call in the same
	// transaction that writes the wager. See status.go for why that placement
	// is the only one that works.
	ErrSelfExcluded = fmt.Errorf("%w: account is self-excluded from wagering", ErrForbidden)
)

// Registration and enrolment failures.
var (
	// ErrEmailTaken means the normalised address is already registered.
	//
	// This is an ENUMERATION ORACLE and there is no way to remove it from a
	// system that must refuse duplicate addresses. The mitigation is at the
	// transport: internal/httpapi should rate-limit registration per IP hard,
	// and may choose to answer "check your email" uniformly instead of
	// surfacing this. That decision is the handler's; hiding it here would
	// leave the service unable to tell its caller what happened.
	ErrEmailTaken = fmt.Errorf("%w: email address is already registered", ErrConflict)

	// ErrTOTPAlreadyEnrolled means a CONFIRMED enrolment exists. An
	// unconfirmed one is replaceable, per migrations/00005: "An unconfirmed row
	// must NOT be treated as an enrolled second factor -- otherwise a mis-scan
	// locks the account out."
	ErrTOTPAlreadyEnrolled = fmt.Errorf("%w: a second factor is already enrolled", ErrConflict)

	// ErrTOTPNotEnrolled means there is no enrolment to confirm, use or remove.
	ErrTOTPNotEnrolled = fmt.Errorf("%w: no second factor is enrolled", ErrConflict)
)

// Validation failures on input this package accepts directly.
var (
	ErrEmailEmpty    = fmt.Errorf("%w: email address is empty", ErrInvalid)
	ErrEmailTooLong  = fmt.Errorf("%w: email address is longer than %d bytes", ErrInvalid, MaxEmailLen)
	ErrEmailTooShort = fmt.Errorf("%w: email address is shorter than %d bytes", ErrInvalid, MinEmailLen)
	ErrEmailShape    = fmt.Errorf("%w: email address must be one '@' with non-empty, whitespace-free sides", ErrInvalid)

	// ErrPasswordTooShort and ErrPasswordTooLong bound what will be hashed.
	//
	// The MAXIMUM is the security-relevant one and it is not a usability
	// choice: argon2id's cost is linear in the input length once the input
	// exceeds a block, so an unbounded password field is a CPU-amplification
	// primitive — one request, megabytes of hashing. 1024 bytes is far past any
	// real passphrase and far below the point where length matters to cost.
	ErrPasswordTooShort = fmt.Errorf("%w: password is shorter than %d bytes", ErrInvalid, MinPasswordLen)
	ErrPasswordTooLong  = fmt.Errorf("%w: password is longer than %d bytes", ErrInvalid, MaxPasswordLen)
	ErrPasswordNotUTF8  = fmt.Errorf("%w: password is not valid UTF-8", ErrInvalid)

	// ErrPasswordUnchanged refuses a "change" that is a no-op. Accepting it
	// would revoke every session and move password_changed_at for nothing,
	// which is a self-inflicted denial of service dressed as a security event.
	ErrPasswordUnchanged = fmt.Errorf("%w: new password is the same as the current one", ErrConflict)
)

// Hash and token encoding failures. These are ErrInternal rather than
// ErrInvalid: a stored hash that will not parse is corruption on our side, not
// bad input from the caller.
var (
	ErrHashFormat     = fmt.Errorf("%w: stored password hash is not a PHC-format argon2id string", ErrInternal)
	ErrHashAlgorithm  = fmt.Errorf("%w: stored password hash is not argon2id", ErrInternal)
	ErrHashVersion    = fmt.Errorf("%w: stored password hash has an unsupported argon2 version", ErrInternal)
	ErrHashParams     = fmt.Errorf("%w: stored password hash carries unusable argon2id parameters", ErrInternal)
	ErrParamsInvalid  = fmt.Errorf("%w: argon2id parameters are outside the permitted range", ErrInvalid)
	ErrKeyUnknown     = fmt.Errorf("%w: no key in the keyring matches the key id on this row", ErrInternal)
	ErrKeyLength      = fmt.Errorf("%w: encryption key is not %d bytes", ErrInvalid, KeyLen)
	ErrKeyringEmpty   = fmt.Errorf("%w: keyring holds no keys", ErrInvalid)
	ErrKeyringFormat  = fmt.Errorf("%w: keyring specification is not id:base64,id:base64", ErrInvalid)
	ErrSigningKeyLen  = fmt.Errorf("%w: JWT signing key is shorter than %d bytes", ErrInvalid, MinSigningKeyLen)
	ErrDecrypt        = fmt.Errorf("%w: stored ciphertext failed authenticated decryption", ErrInternal)
	ErrSecretNotShown = fmt.Errorf("%w: a Secret cannot be serialised; call Expose at the single call site that needs it", ErrInvalid)
)

// errSampleLen caps how much of a rejected value appears in an error message.
// Same value and same reasoning as internal/domain: input here is untrusted and
// an unbounded echo into a log line is how a log becomes an attack surface.
const errSampleLen = 32

// sample truncates an untrusted value for inclusion in an error message.
//
// It is used ONLY on values that are not credentials — an email address on a
// shape failure, a key id that is not in the keyring. A password, a token or a
// TOTP secret is never passed to it, and the review rule is simpler than that:
// those values live in a [Secret], which has no accessor that fmt will call.
func sample(s string) string {
	if len(s) <= errSampleLen {
		return s
	}
	// Trim to a rune boundary so the message stays valid UTF-8.
	cut := errSampleLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
