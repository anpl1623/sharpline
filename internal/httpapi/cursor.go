package httpapi

import (
	"encoding/base64"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// Keyset pagination.
//
// # Why not OFFSET
//
// `ingest` writes the `events` table continuously. OFFSET re-evaluates the whole
// ordered set on every page, so a row inserted ahead of the offset between page
// N and page N+1 pushes one row across the page boundary and the reader NEVER
// SEES IT; a row that leaves the set duplicates one. On a board that changes
// every few seconds that is the normal case rather than a rare race, and both
// failures are silent — the client gets a plausible page that is missing a
// fixture.
//
// A keyset cursor names the LAST ROW of the previous page instead of counting
// rows, so an insertion or deletion anywhere else in the set cannot move it.
//
// # What a cursor has to carry, and why each part is there
//
//	v      A version byte. The encoding will change; a client holding an old
//	       cursor must get a clean 400 rather than a mis-parse.
//	k      The ordering key: (scheduled_start, id). ScheduledStart alone is not
//	       a total order — two fixtures at the same instant are returned in an
//	       arbitrary order — so a cursor on it alone cannot say which row it
//	       points after. `id` is a primary key, so the pair is total.
//	s      A SCOPE FINGERPRINT: a hash of the filters and the window the page
//	       was minted under. Without it, a client that changes `starting_before`
//	       or `league` while paging silently receives a page from a different
//	       set, ordered consistently, with no error anywhere. With it, the
//	       mismatch is a 400 naming the parameter.
//
// # Why it is opaque
//
// A cursor is not a secret and is not signed: there is nothing in it a client
// could learn that it does not already know, and nothing it could forge that
// would grant access to a row it could not reach by asking. It is base64url so
// that it survives a query string unescaped, and it is documented as opaque so
// the encoding can change without breaking a client that respected that.
//
// It is NOT signed with an HMAC, deliberately. Signing would defend against a
// tampered cursor, and the only thing a tampered cursor can do is produce a
// page starting somewhere else in a set the caller may already read — the same
// thing it could do by asking for a different `starting_before`. Paying a key
// and a rotation story for that is security theatre; strict decoding is the
// actual control, and a malformed cursor is a 400 rather than a panic or a
// wrong page.

const (
	// cursorVersion prefixes every cursor. Bump it when the payload changes.
	cursorVersion = "1"

	// maxCursorLen matches the spec's `Cursor` parameter maxLength. A longer
	// value is rejected before it is decoded, so a client cannot make the
	// decoder do unbounded work.
	maxCursorLen = 512
)

// ErrBadCursor reports a cursor that cannot be decoded, or one presented
// against a different query than the one it was minted under.
var ErrBadCursor = errors.New("httpapi: invalid cursor")

// cursor is the decoded form of a page continuation token.
type cursor struct {
	key EventKey

	// scope is the fingerprint of the filters this cursor was minted under.
	scope uint64
}

// cursorScope fingerprints everything that changes WHICH SET is being paged.
//
// It deliberately does NOT include the page size or the odds format: both change
// how a page is rendered, neither changes which rows are in the set or their
// order, and rejecting a cursor because the client resized the page would be
// user-hostile for no benefit.
//
// FNV-1a rather than SHA-256 because this is a consistency check against
// accident, not a defence against forgery — see the note above on why the cursor
// is unsigned. A collision produces the same failure mode as an unfingerprinted
// cursor, which is the behaviour every offset-paginated API has all the time.
func cursorScope(parts ...string) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		// The length prefix stops ("ab","c") and ("a","bc") hashing alike, which
		// is the whole reason a fingerprint over concatenated fields needs one.
		_, _ = fmt.Fprintf(h, "%d:%s\x00", len(p), p)
	}
	return h.Sum64()
}

// encodeCursor renders a cursor for the wire.
//
// The instant is encoded as UnixNano rather than as RFC 3339, because the
// database orders by the full-precision timestamptz and a cursor that rounded to
// microseconds could re-emit or skip a row quoted inside the rounded interval.
func encodeCursor(c cursor) string {
	raw := strings.Join([]string{
		cursorVersion,
		strconv.FormatInt(c.key.ScheduledStart.UTC().UnixNano(), 10),
		strconv.FormatUint(c.scope, 36),
		c.key.ID.String(),
	}, "\x1f")
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses a cursor and checks it against the scope of the query it
// was presented with.
//
// Every failure is the same error: a client cannot learn from the response
// whether its cursor was malformed, from an older version, or simply for a
// different query. There is nothing actionable in the distinction and reporting
// it would only describe the internals.
func decodeCursor(encoded string, scope uint64) (cursor, error) {
	if len(encoded) > maxCursorLen {
		return cursor{}, fmt.Errorf("%w: too long", ErrBadCursor)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: not base64url", ErrBadCursor)
	}

	parts := strings.Split(string(raw), "\x1f")
	if len(parts) != 4 {
		return cursor{}, fmt.Errorf("%w: wrong field count", ErrBadCursor)
	}
	if parts[0] != cursorVersion {
		return cursor{}, fmt.Errorf("%w: unsupported version", ErrBadCursor)
	}

	nanos, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: unparseable key instant", ErrBadCursor)
	}
	got, err := strconv.ParseUint(parts[2], 36, 64)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: unparseable scope", ErrBadCursor)
	}
	if got != scope {
		// The client changed a filter mid-page. Reporting this rather than
		// serving the page is the entire point of the fingerprint.
		return cursor{}, fmt.Errorf("%w: cursor belongs to a different query", ErrBadCursor)
	}

	// The identifier goes through the domain constructor rather than being cast,
	// so a cursor cannot smuggle a value the rest of the system considers
	// impossible into a query parameter.
	id, err := domain.NewEventID(parts[3])
	if err != nil {
		return cursor{}, fmt.Errorf("%w: unparseable key identifier", ErrBadCursor)
	}

	return cursor{
		key:   EventKey{ScheduledStart: time.Unix(0, nanos).UTC(), ID: id},
		scope: got,
	}, nil
}
