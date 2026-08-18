package theoddsapi

import (
	"net/url"
	"strings"
)

// redactedPlaceholder is what replaces a secret. It is deliberately not empty:
// a log line reading `apiKey=` is indistinguishable from a request that forgot
// the key, whereas this one says "there was a value and it was removed".
const redactedPlaceholder = "[REDACTED]"

// apiKeyParam is the query parameter The Odds API takes the key in.
// https://the-odds-api.com/liveapi/guides/v4/ — every endpoint, "apiKey".
const apiKeyParam = "apiKey"

// minRedactableLen guards against a degenerate secret. Replacing every
// occurrence of a one- or two-character "secret" would corrupt unrelated text
// into nonsense while protecting nothing. The provider's own OpenAPI document
// describes the key as 40 characters, so anything this short is not a key.
const minRedactableLen = 8

// redactor removes secrets from strings and URLs.
//
// It is a VALUE, constructed with the key and passed down by the client. There
// is no package-level redactor and no global secret list — CLAUDE.md §12
// forbids global mutable state, and a shared mutable secret list is exactly the
// thing that ends up empty in one goroutine and populated in another.
//
// Redaction happens on two independent axes, and both are needed:
//
//   - BY PARAMETER NAME, in URLs. This catches a key this redactor was never
//     told about — a URL assembled elsewhere, a redirect target, a key rotated
//     at runtime. It works even when secrets is empty.
//   - BY VALUE, in arbitrary strings. This catches the key wherever it ended up
//     that is not a URL query: an error message, a provider response that
//     echoes the request, a header dump.
type redactor struct {
	// secrets holds the literal forms to remove: the key, plus its
	// query-escaped form when that differs.
	secrets []string
}

// newRedactor builds a redactor for the given secrets. Empty and implausibly
// short values are dropped rather than accepted, so an unconfigured redactor
// degrades to parameter-name redaction instead of mangling every string it
// sees.
func newRedactor(secrets ...string) redactor {
	var out []string
	seen := make(map[string]struct{}, len(secrets)*2)
	add := func(s string) {
		if len(s) < minRedactableLen {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range secrets {
		add(s)
		// The key may already have been percent-encoded by the time it reaches
		// a string we are redacting. For a 40-character alphanumeric key these
		// are identical, so this is defensive rather than routine — but the
		// cost of being wrong here is a leaked credential.
		add(url.QueryEscape(s))
	}
	return redactor{secrets: out}
}

// String removes every known secret from s.
//
// It is the last line of defence and it is applied to every error message this
// package returns, on the assumption that some future code path will format a
// URL without going through URL below.
func (r redactor) String(s string) string {
	if s == "" || len(r.secrets) == 0 {
		return s
	}
	for _, secret := range r.secrets {
		if strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, redactedPlaceholder)
		}
	}
	return s
}

// URL renders u with the apiKey parameter's value replaced, and with any known
// secret removed from the rest of the string.
//
// The parameter-name pass runs first and unconditionally, so a URL carrying a
// key this redactor has never been told about is still safe to log. u is not
// mutated — the caller's URL keeps its real key, because it still has to be
// requested.
func (r redactor) URL(u *url.URL) string {
	if u == nil {
		return ""
	}
	clone := *u
	if q := clone.Query(); q.Has(apiKeyParam) {
		q.Set(apiKeyParam, redactedPlaceholder)
		clone.RawQuery = q.Encode()
	}
	// User info would be a second place a credential could hide. This API does
	// not use it, but Redacted() costs nothing and closes the hole.
	out := r.String(clone.Redacted())
	// Encode() percent-escapes the placeholder's brackets. Putting them back is
	// cosmetic but not pointless: `apiKey=%5BREDACTED%5D` in a log line reads as
	// a value someone might try to use, and the marker exists to say plainly
	// that a value was removed.
	return strings.ReplaceAll(out, url.QueryEscape(redactedPlaceholder), redactedPlaceholder)
}

// RawURL is URL for a URL that is still a string — most importantly the one
// inside a *url.Error, which net/http fills in from the request it was about to
// make.
//
// It matters that this parses rather than only doing a value replacement. A URL
// reaching an error is not always one this package built: a redirect target is
// chosen by whoever answered the request, and a hostile or merely eccentric one
// can carry the key percent-encoded differently from the literal this redactor
// was told about. The by-value pass would miss that; the by-parameter-name pass
// cannot, because it works on the decoded query regardless of how it was
// spelled on the wire.
//
// An unparseable input falls back to the value pass and is never returned raw.
func (r redactor) RawURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return r.String(raw)
	}
	return r.URL(u)
}
