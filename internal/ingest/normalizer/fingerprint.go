// Change detection.
//
// CLAUDE.md §5: "Hash each normalized market to suppress no-op updates — most
// polls return identical data and must not generate bus traffic."
//
// # The rule
//
//	THE FINGERPRINT COVERS THE ENTIRE PUBLISHED PAYLOAD EXCEPT THE FINGERPRINT
//	FIELD ITSELF AND THE OBSERVATION AND INGESTION INSTANTS.
//
// Everything that reaches a consumer is hashed, so no change to anything a
// consumer can see is ever suppressed. The exclusions are exactly three
// timestamps and the self-reference, they are enumerated in excludedPaths below,
// and fingerprint_test.go walks the struct by reflection and fails the build if a
// field is neither excluded nor demonstrably load-bearing.
//
// # Why the observation instant is excluded, in the provider's own terms
//
// The Odds API advances `last_update` on every refresh of a bookmaker's odds,
// whether or not any number moved. Hashing it would make every poll differ, and
// suppression — the entire mechanism — would never fire once. ADR 0003's own
// arithmetic assumes it does fire: "it always saves bus traffic, pricing CPU, and
// hypertable writes. Most polls return identical data, and suppressing those is
// the difference between a quiet system and one that writes thousands of no-op
// rows per minute."
//
// # Why the line, the status and every quote are included
//
// The mirror-image failure, and the worse of the two. A fingerprint that omitted
// the line would suppress a move from -3.5 to -4 as a no-op. The compacted topic
// would keep serving the -3.5 record, and because compaction keeps only the
// latest record PER KEY, nothing would correct it until the market next changed
// in some other way. The board would show a line the book has not offered for
// hours, and there would be no error anywhere.
//
// # Why the raw payload is not what gets hashed
//
// Raw bytes differ on fields that mean nothing here: a response-level timestamp,
// a bookmaker this build does not map, a market key outside the featured three,
// key ordering in a re-serialised JSON object. Hashing them would suppress
// nothing, which is the failure mode that looks like it is working — bus traffic
// stays high and the metric says 0% suppressed, and only someone who reads the
// metric notices.
//
// # Encoding
//
// SHA-256 over a length-delimited encoding. Every field is written as its byte
// length in decimal, a colon, then its bytes, so no two distinct field tuples can
// produce identical input — plain concatenation cannot distinguish ("ab","c")
// from ("a","bc"), and a collision there would freeze a market silently.
//
// Collections are sorted by identifier before hashing, because Go map iteration
// and provider array order are both unstable and neither is a change worth
// republishing for.
//
// Floats are written with strconv 'g' and precision -1, which is the shortest
// representation that round-trips exactly, so two float64s hash the same iff they
// are the same float64. Negative zero is folded onto positive zero first: they
// compare equal in Go but format differently, and a line that flipped between "0"
// and "-0" would look like a change for ever.
package normalizer

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"math"
	"slices"
	"strconv"

	"github.com/anpl1623/sharpline/internal/domain"
)

// Fingerprint is the hex SHA-256 of a normalized market's content.
//
// A string rather than a [32]byte because it goes onto the wire, into a Redis
// value and into a log line, and one representation with no conversion at the
// boundaries is worth the 32 extra bytes.
type Fingerprint string

// String returns the fingerprint as a bare string.
func (f Fingerprint) String() string { return string(f) }

// IsZero reports whether the fingerprint is unset.
func (f Fingerprint) IsZero() bool { return f == "" }

// excludedPaths names every field of NormalizedMarket that the fingerprint does
// NOT cover, in `Type.Field` form.
//
// It is not documentation. fingerprint_test.go reads it, and a field that is
// neither listed here nor able to change the fingerprint fails the build. Adding
// an entry is therefore a deliberate, reviewed statement that a change to that
// field must not reach consumers.
var excludedPaths = map[string]string{
	"NormalizedMarket.Fingerprint": "the hash cannot cover itself",
	"NormalizedMarket.ObservedAt":  "provider observation instant; advances on every refresh whether or not a price moved",
	"NormalizedMarket.IngestedAt":  "when ingest received the payload; advances on every poll by construction",
	"MarketRef.UpdatedAt":          "the observation instant restated on the market; same reason as ObservedAt",
	"PriceRef.ObservedAt":          "the per-quote observation instant; the staleness subtrahend, and it advances on every refresh",
}

// Hash computes the fingerprint of a record.
//
// It ignores whatever is already in m.Fingerprint, so hashing a record that has
// been round-tripped through the topic reproduces the value the producer wrote —
// which is what lets warm start VERIFY its recomputation instead of trusting the
// stored value.
func (m NormalizedMarket) Hash() Fingerprint {
	w := newCanon()

	w.i(int64(m.SchemaVersion))
	w.s(m.Provider)

	w.s(m.Sport.ID)
	w.s(m.Sport.Slug)
	w.s(m.Sport.Name)

	w.s(m.League.ID)
	w.s(m.League.SportID)
	w.s(m.League.Slug)
	w.s(m.League.Name)

	w.s(m.Event.ID)
	w.s(m.Event.LeagueID)
	w.s(m.Event.Kind)
	w.s(m.Event.Name)
	w.s(m.Event.Home.ID)
	w.s(m.Event.Home.Name)
	w.s(m.Event.Away.ID)
	w.s(m.Event.Away.Name)
	w.t(m.Event.ScheduledStart.UnixNano())
	w.s(m.Event.Status)

	w.s(m.Market.ID)
	w.s(m.Market.EventID)
	w.s(m.Market.Type)
	w.s(m.Market.ProviderKey)
	w.line(m.Market.Line)
	w.s(m.Market.Subject)
	w.s(m.Market.Status)

	books := slices.Clone(m.Books)
	slices.SortFunc(books, func(a, b BookRef) int { return cmpString(a.ID, b.ID) })
	w.i(int64(len(books)))
	for _, b := range books {
		w.s(b.ID)
		w.s(b.Slug)
		w.s(b.Name)
		w.s(b.Kind)
		// A book becoming (or ceasing to be) the sharp reference changes which
		// book every downstream fair value is derived from, so it is a change
		// consumers must see rather than a cosmetic relabelling.
		w.b(b.Reference)
	}

	sels := slices.Clone(m.Selections)
	slices.SortFunc(sels, func(a, b SelectionRef) int { return cmpString(a.ID, b.ID) })
	w.i(int64(len(sels)))
	for _, s := range sels {
		w.s(s.ID)
		w.s(s.MarketID)
		w.s(s.Role)
		w.s(s.Name)
	}

	prices := slices.Clone(m.Prices)
	slices.SortFunc(prices, func(a, b PriceRef) int {
		if c := cmpString(a.SelectionID, b.SelectionID); c != 0 {
			return c
		}
		return cmpString(a.BookID, b.BookID)
	})
	w.i(int64(len(prices)))
	for _, p := range prices {
		w.s(p.SelectionID)
		w.s(p.BookID)
		w.f(p.Decimal)
		w.line(p.Line)
	}

	return Fingerprint(hex.EncodeToString(w.h.Sum(nil)))
}

// canon is the length-delimited hash writer.
type canon struct {
	h   hash.Hash
	buf []byte
}

func newCanon() *canon { return &canon{h: sha256.New(), buf: make([]byte, 0, 64)} }

// s writes a string field.
func (c *canon) s(v string) {
	c.buf = strconv.AppendInt(c.buf[:0], int64(len(v)), 10)
	c.buf = append(c.buf, ':')
	c.buf = append(c.buf, v...)
	_, _ = c.h.Write(c.buf)
}

// i writes an integer field.
func (c *canon) i(v int64) { c.s(strconv.FormatInt(v, 10)) }

// b writes a boolean field. The two words are written in full rather than as
// 0/1 so a boolean can never collide with an adjacent integer field.
func (c *canon) b(v bool) { c.s(strconv.FormatBool(v)) }

// t writes a timestamp field.
//
// Only the event's scheduled start uses it. That is not an observation instant —
// it is the advertised kickoff, and a postponement moving it is a real change the
// board must show — which is exactly why it is hashed while the three instants in
// excludedPaths are not.
func (c *canon) t(nanos int64) { c.i(nanos) }

// f writes a float field.
func (c *canon) f(v float64) {
	if v == 0 {
		// Folds -0 onto +0. They are ==, so a market oscillating between them is
		// not changing, but they format as "-0" and "0" and would hash apart.
		v = 0
	}
	if math.IsNaN(v) {
		// Unreachable through the constructors — domain.NewPrice and
		// domain.NewLine both reject NaN — but a NaN reaching a hash would make a
		// value that never equals itself, so the market would republish on every
		// poll for ever. One branch is cheaper than that failure.
		c.s("nan")
		return
	}
	c.s(strconv.FormatFloat(v, 'g', -1, 64))
}

// line writes a domain.Line, distinguishing absent from present-and-zero exactly
// as the type does. A moneyline's absent line and a pick'em's 0.0 must not hash
// alike; domain.Line exists because "a bare float64 cannot tell those two apart,
// and the failure mode is silent".
func (c *canon) line(l domain.Line) {
	v, ok := l.Value()
	if !ok {
		c.s("-")
		return
	}
	c.s("+")
	c.f(v)
}

// cmpString orders strings for the deterministic sort. strings.Compare in a
// helper so every sort in this file uses one comparison.
func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
