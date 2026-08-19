package auth

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Email length bounds, matching users_email_length in migrations/00005 exactly.
// 254 is the RFC 5321 maximum path length; 3 is the shortest thing that can
// have a non-empty local part, an '@' and a non-empty domain.
const (
	MinEmailLen = 3
	MaxEmailLen = 254
)

// Email is a normalised address: the value stored in users.email.
//
// It is a defined type rather than a bare string for the same reason
// internal/domain gives its identifiers their own types — so that a function
// taking an Email cannot be handed a raw, un-normalised user input by accident.
// The only way to obtain one is [NewEmail], which normalises.
type Email string

// String returns the address.
func (e Email) String() string { return string(e) }

// IsZero reports whether the address is unset.
func (e Email) IsZero() bool { return e == "" }

// NewEmail normalises and validates an address.
//
// # Normalisation, and why it is the caller's problem exactly once
//
// migrations/00005 stores the address ALREADY lowercased and makes that a
// database invariant with `CHECK (email = lower(email))`, so that a plain
// UNIQUE index is a correct case-insensitive uniqueness constraint. Its comment
// is explicit that the alternatives are worse: citext hides a locale-dependent
// comparison behind an ordinary `=`, and a `UNIQUE (lower(email))` functional
// index lets both 'A@x.com' and 'a@x.com' exist in the stored column while
// forcing every lookup to remember the lower() wrapper.
//
// The consequence is that normalisation must happen before the value reaches
// SQL, on BOTH the write and the read path — a login that looked up the raw
// input would miss the row for a user who capitalised their address, and would
// then be indistinguishable from a wrong password. This function is that single
// point, and every store method in this package takes an Email rather than a
// string so the compiler enforces it.
//
// Normalisation is: trim surrounding whitespace, then lowercase. It is
// deliberately NOT more than that. Stripping dots from a Gmail local part or
// cutting a '+tag' is a provider-specific policy that silently merges two
// addresses their owner considers distinct, and getting it wrong means one user
// can claim another's account.
//
// strings.ToLower is Unicode-aware, so an address with a non-ASCII local part
// lowercases the way the user expects. Postgres's lower() on a UTF-8 database
// agrees with it for every case pair that appears in a deliverable address, and
// the CHECK is what would catch a disagreement — loudly, at INSERT, rather than
// as a duplicate row.
func NewEmail(raw string) (Email, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrEmailEmpty
	}
	if !utf8.ValidString(trimmed) {
		return "", ErrEmailShape
	}

	lowered := strings.ToLower(trimmed)

	switch n := len(lowered); {
	case n < MinEmailLen:
		return "", ErrEmailTooShort
	case n > MaxEmailLen:
		return "", ErrEmailTooLong
	}

	// The shape check reproduces users_email_shape:
	// '^[^[:space:]@]+@[^[:space:]@]+$' — exactly one '@', non-empty and
	// whitespace-free on both sides.
	//
	// It is deliberately permissive, and migrations/00005 says why: "Tighter
	// regexes reject deliverable addresses (quoted local parts, single-label
	// domains) and the only real proof an address exists is sending mail to
	// it." Anything stricter here would also drift from the CHECK, and a
	// validator that is stricter than the database is a source of 400s the
	// schema would have accepted.
	local, domain, ok := strings.Cut(lowered, "@")
	if !ok || local == "" || domain == "" {
		return "", ErrEmailShape
	}
	if strings.ContainsRune(domain, '@') {
		return "", ErrEmailShape
	}
	if containsSpaceOrControl(local) || containsSpaceOrControl(domain) {
		return "", ErrEmailShape
	}

	return Email(lowered), nil
}

// containsSpaceOrControl reports whether s holds a whitespace or control
// character. POSIX [:space:] is space, tab, newline, vertical tab, form feed
// and carriage return; the other C0 and C1 controls are excluded too, because a
// newline inside a value that reaches a log line is a log-injection primitive
// and there is no address that legitimately contains one.
func containsSpaceOrControl(s string) bool {
	for _, r := range s {
		if r <= 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return true
		}
	}
	return false
}

// emailErr wraps a shape failure with a truncated sample of the offending
// address, for an operator reading a 400.
//
// An email address is personal data but it is not a credential, so a truncated
// echo is acceptable where a password's would not be. It is truncated anyway:
// the input is untrusted and unbounded echoes into logs are how logs become
// attack surfaces.
func emailErr(raw string, err error) error {
	return fmt.Errorf("email %q: %w", sample(raw), err)
}
