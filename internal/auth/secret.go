package auth

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
)

// Redacted is the single placeholder every leak channel produces. It is a
// constant so a log-scraping test can assert on it, and so a reviewer grepping
// for it finds every site that could have leaked and did not.
const Redacted = "[REDACTED]"

// Secret carries a value that must never reach a log, a span attribute, an
// error message, or a JSON response by accident.
//
// # Why a type instead of a convention
//
// This phase's brief requires that redaction be "structural, not remembered".
// A rule saying "do not log the token" is remembered; it fails the first time
// somebody adds slog.Any("session", sess) to debug something at 2am and ships
// it. A type fails closed instead:
//
//	slog.Any("token", tok)      // "[REDACTED]" — LogValue
//	fmt.Sprintf("%v", tok)      // "[REDACTED]" — String
//	fmt.Sprintf("%#v", tok)     // "[REDACTED]" — GoString
//	fmt.Errorf("bad %s", tok)   // "[REDACTED]" — String
//	json.Marshal(response)      // ERROR       — MarshalJSON refuses
//
// and the whole struct inherits the behaviour: a [Session] containing a Secret
// prints its other fields and a placeholder for this one, because fmt and slog
// recurse into fields and find the methods.
//
// The zero Secret is a valid empty secret and redacts identically. That matters
// because a struct returned on an error path has zero-valued secrets, and those
// must be as unprintable as populated ones.
//
// # Marshalling refuses rather than redacting
//
// MarshalJSON returning "[REDACTED]" would be safe and silent. Returning an
// error is safe and LOUD, and loud is correct here: the only legitimate way a
// secret leaves this process is as the deliberate body of a token response or
// the value of a Set-Cookie header, and both of those are one call site each.
// Anything else marshalling a Secret is a mistake, and a mistake that fails a
// test is worth more than one that silently produces a placeholder where a
// working token was expected.
//
// # The escape hatch
//
// [Secret.Expose] is the only way to the value. It is deliberately ugly to
// read, trivial to grep for, and the review question at every occurrence is the
// same: does this string cross a process boundary to the user who owns it, and
// nowhere else?
type Secret struct {
	// Unexported and not a []byte: a []byte field would be printable via
	// %s/%q on the slice itself if a caller reached it, and it cannot be
	// reached, so string is simpler. Go strings are immutable, so a Secret
	// cannot be mutated through a copy.
	//
	// Zeroing is NOT attempted. Go's garbage collector copies strings freely;
	// a "wipe" would zero one of an unknown number of copies and would buy a
	// false sense of security. The threat model in doc.go excludes an attacker
	// who can read process memory, precisely because defending that here is
	// not achievable in this language.
	v string
}

// NewSecret wraps a value.
func NewSecret(v string) Secret { return Secret{v: v} }

// Expose returns the underlying value.
//
// Call it at the ONE place the value legitimately leaves the process — writing
// the refresh cookie, rendering the enrolment URI once at enrolment time — and
// nowhere else. Every other use is a leak waiting to be found in a log.
func (s Secret) Expose() string { return s.v }

// IsZero reports whether the secret holds no value.
func (s Secret) IsZero() bool { return s.v == "" }

// Len reports the byte length of the underlying value. Safe to log: a length is
// not a credential, and "the token we issued was 0 bytes" is exactly the sort
// of thing an operator needs to be able to see.
func (s Secret) Len() int { return len(s.v) }

// Equal compares two secrets in constant time with respect to their contents.
//
// subtle.ConstantTimeCompare returns 0 immediately on a length mismatch, so
// this leaks length equality and nothing else — which is already public for
// every secret this package mints, since they all have a fixed encoded width.
func (s Secret) Equal(other Secret) bool {
	return subtle.ConstantTimeCompare([]byte(s.v), []byte(other.v)) == 1
}

// String implements fmt.Stringer. It never returns the value.
func (s Secret) String() string { return Redacted }

// GoString implements fmt.GoStringer, so %#v — which ignores String — also
// redacts. Without this, `fmt.Sprintf("%#v", cfg)` would print the field.
func (s Secret) GoString() string { return Redacted }

// LogValue implements slog.LogValuer, so slog.Any and any struct containing a
// Secret redact rather than reflecting into the field.
func (s Secret) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON refuses. See the type comment: failing loudly is the point.
func (s Secret) MarshalJSON() ([]byte, error) { return nil, ErrSecretNotShown }

// MarshalText refuses, which also covers encoding/json's TextMarshaler path for
// map keys and every other encoder that honours it.
func (s Secret) MarshalText() ([]byte, error) { return nil, ErrSecretNotShown }

// UnmarshalJSON accepts a JSON string, so a Secret can be a field on a request
// body that internal/httpapi decodes. Decoding INTO a secret is safe; the
// asymmetry with MarshalJSON is the whole design.
//
// It delegates to encoding/json rather than stripping the surrounding quotes by
// hand, and that is not fussiness: a passphrase containing a quote, a backslash
// or a non-BMP character arrives escaped, and a hand-rolled unquote would
// silently register the user with a DIFFERENT password than they typed — a
// defect that only shows up as "I cannot log in" days later.
//
// A JSON string is the only accepted shape; a number, object or null is an
// error rather than a coerced value.
func (s *Secret) UnmarshalJSON(b []byte) error {
	// `null` is refused explicitly. encoding/json's documented behaviour for
	// null into a non-pointer is to leave the value UNCHANGED and report no
	// error, so without this a `{"password": null}` body would silently decode
	// to the empty secret and be checked against the stored hash as one. An
	// absent field and a null field are both "no password was sent", and both
	// should be a 400 rather than a failed login.
	if string(b) == "null" {
		return ErrInvalid
	}
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		// The error is NOT wrapped: encoding/json's syntax errors quote the
		// offending input, and the offending input here is a password.
		return ErrInvalid
	}
	s.v = v
	return nil
}

// UnmarshalText accepts raw bytes.
func (s *Secret) UnmarshalText(b []byte) error {
	s.v = string(b)
	return nil
}
