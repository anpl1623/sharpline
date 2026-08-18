package normalizer

import (
	"reflect"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// sampleRecord returns a NormalizedMarket with EVERY field set to a distinct,
// non-zero value.
//
// Non-zero everywhere is what makes the structural test below meaningful: a
// field left at its zero value could be mutated into a non-zero one and change
// the hash for the wrong reason, and — worse — a field the hash forgot could sit
// at zero on both sides of a comparison and never be noticed.
func sampleRecord() NormalizedMarket {
	line, err := domain.NewLine(-3.5)
	if err != nil {
		panic(err)
	}
	away, err := domain.NewLine(3.5)
	if err != nil {
		panic(err)
	}
	base := time.Date(2026, 8, 17, 18, 30, 0, 0, time.UTC)
	return NormalizedMarket{
		SchemaVersion: SchemaVersion,
		Provider:      "the-odds-api",
		Fingerprint:   "0000000000000000000000000000000000000000000000000000000000000000",
		Sport:         SportRef{ID: "p.s.americanfootball", Slug: "americanfootball", Name: "American Football"},
		League: LeagueRef{
			ID: "p.l.americanfootball_nfl", SportID: "p.s.americanfootball",
			Slug: "americanfootball_nfl", Name: "NFL",
		},
		Event: EventRef{
			ID: "p.e.abc", LeagueID: "p.l.americanfootball_nfl", Kind: "match",
			Name:           "Lions at Chiefs",
			Home:           CompetitorRef{ID: "home-1", Name: "Kansas City Chiefs"},
			Away:           CompetitorRef{ID: "away-1", Name: "Detroit Lions"},
			ScheduledStart: base.Add(2 * time.Hour),
			Status:         "scheduled",
		},
		Market: MarketRef{
			ID: "p.e.abc.m.spreads", EventID: "p.e.abc", Type: "spread",
			ProviderKey: "spreads", Line: line, Subject: "",
			Status: "open", UpdatedAt: base,
		},
		Books:      []BookRef{{ID: "p.b.draftkings", Slug: "draftkings", Name: "DraftKings", Kind: "external"}},
		Selections: []SelectionRef{{ID: "p.e.abc.m.spreads.x.home", MarketID: "p.e.abc.m.spreads", Role: "home", Name: "Kansas City Chiefs"}},
		Prices: []PriceRef{{
			SelectionID: "p.e.abc.m.spreads.x.home", BookID: "p.b.draftkings",
			Decimal: 1.909, Line: away, ObservedAt: base,
		}},
		ObservedAt: base,
		IngestedAt: base.Add(30 * time.Second),
	}
}

// leaf is one mutable field of the published record, addressed the way
// excludedPaths addresses it: "TypeName.FieldName".
type leaf struct {
	path string
	val  reflect.Value
}

// collectLeaves walks the record by reflection and returns every mutable leaf.
//
// The types it descends into are the wire Refs; time.Time and domain.Line are
// leaves because they are whole values the hash writes atomically, and because
// domain.Line's fields are unexported by design.
func collectLeaves(t *testing.T, v reflect.Value, typeName string, out *[]leaf) {
	t.Helper()
	rt := v.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			t.Fatalf("%s.%s is unexported; the wire record must be fully addressable "+
				"or this test cannot prove what the hash covers", typeName, f.Name)
		}
		fv := v.Field(i)
		path := typeName + "." + f.Name

		switch fv.Kind() {
		case reflect.String, reflect.Int, reflect.Float64:
			*out = append(*out, leaf{path: path, val: fv})
		case reflect.Struct:
			switch fv.Interface().(type) {
			case time.Time, domain.Line:
				*out = append(*out, leaf{path: path, val: fv})
			default:
				collectLeaves(t, fv, f.Type.Name(), out)
			}
		case reflect.Slice:
			if fv.Len() == 0 {
				t.Fatalf("%s is empty in sampleRecord; a collection with no element "+
					"hides every field of its element type from this test", path)
			}
			collectLeaves(t, fv.Index(0), f.Type.Elem().Name(), out)
		default:
			t.Fatalf("%s has unhandled kind %s; extend this walk before adding a field of that shape",
				path, fv.Kind())
		}
	}
}

// mutate replaces a leaf's value with a different one of the same type.
func mutate(t *testing.T, l leaf) {
	t.Helper()
	switch v := l.val.Interface().(type) {
	case string:
		l.val.SetString(v + "-changed")
	case int:
		l.val.SetInt(int64(v) + 1)
	case float64:
		l.val.SetFloat(v + 1)
	case time.Time:
		l.val.Set(reflect.ValueOf(v.Add(time.Second)))
	case domain.Line:
		cur, ok := v.Value()
		if !ok {
			cur = 0
		}
		next, err := domain.NewLine(cur + 1)
		if err != nil {
			t.Fatalf("%s: %v", l.path, err)
		}
		l.val.Set(reflect.ValueOf(next))
	default:
		t.Fatalf("%s: no mutation defined for %T", l.path, v)
	}
}

// TestFingerprintCoversTheWholePayloadExceptTheDeclaredExclusions is the
// structural guard doc.go and fingerprint.go both promise.
//
// It walks NormalizedMarket by reflection, mutates each field in turn, and
// requires the fingerprint to move — UNLESS the field is named in
// excludedPaths, in which case it requires the fingerprint to stay put.
//
// Both halves matter and getting either wrong is a serious defect:
//
//   - Too inclusive: the provider's observation instant advances on every
//     refresh whether or not a price moved, so hashing it means suppression
//     never fires once and the bus carries thousands of no-ops a minute.
//   - Too exclusive: leave the line, a quote or the status out and a real move
//     is swallowed. The compacted topic keeps serving the old record for ever,
//     because the only thing that would replace it is the next change to the
//     same key.
//
// Adding a field to the wire record without deciding which side it is on is a
// BUILD FAILURE here, not a staleness bug six weeks later.
func TestFingerprintCoversTheWholePayloadExceptTheDeclaredExclusions(t *testing.T) {
	rec := sampleRecord()
	base := rec.Hash()
	if base.IsZero() {
		t.Fatal("the fingerprint of a fully populated record is empty")
	}

	var leaves []leaf
	collectLeaves(t, reflect.ValueOf(&rec).Elem(), "NormalizedMarket", &leaves)
	if len(leaves) == 0 {
		t.Fatal("the reflection walk found no fields")
	}

	seen := make(map[string]bool, len(leaves))
	for _, l := range leaves {
		seen[l.path] = true
		original := reflect.New(l.val.Type()).Elem()
		original.Set(l.val)

		mutate(t, l)
		got := rec.Hash()
		l.val.Set(original)

		if why, excluded := excludedPaths[l.path]; excluded {
			if got != base {
				t.Errorf("%s is declared EXCLUDED from the fingerprint (%q) but changing it moved the hash",
					l.path, why)
			}
			continue
		}
		if got == base {
			t.Errorf("changing %s did not move the fingerprint. Either hash it, or add it to "+
				"excludedPaths with a written reason — a field that is neither is a real move "+
				"this pipeline will suppress and never republish", l.path)
		}
	}

	for path := range excludedPaths {
		if !seen[path] {
			t.Errorf("excludedPaths names %q, which the walk never reached: the exclusion is stale "+
				"and is silently exempting nothing", path)
		}
	}

	if got := rec.Hash(); got != base {
		t.Fatalf("the record was not restored between mutations: hash %s, want %s", got, base)
	}
}

// TestFingerprintIgnoresTheFingerprintFieldItself is what lets warm start VERIFY
// a stored fingerprint by recomputing it, rather than trusting the value the
// producer happened to write.
func TestFingerprintIgnoresTheFingerprintFieldItself(t *testing.T) {
	rec := sampleRecord()
	rec.Fingerprint = ""
	empty := rec.Hash()

	rec.Fingerprint = empty.String()
	if got := rec.Hash(); got != empty {
		t.Fatalf("hash changed once its own fingerprint was written back: %s, want %s", got, empty)
	}
	rec.Fingerprint = "not-a-hash"
	if got := rec.Hash(); got != empty {
		t.Fatalf("hash depends on the fingerprint field: %s, want %s", got, empty)
	}
}

// TestFingerprintIsOrderIndependentAcrossCollections pins the sort in Hash.
//
// Provider array order is not a contract and Go map iteration order is
// deliberately randomised, so a fingerprint that moved when two books swapped
// position would report a line move that never happened — on every poll, for
// every market, which defeats suppression entirely.
func TestFingerprintIsOrderIndependentAcrossCollections(t *testing.T) {
	rec := sampleRecord()
	rec.Books = append(rec.Books, BookRef{ID: "p.b.fanduel", Slug: "fanduel", Name: "FanDuel", Kind: "external"})
	rec.Selections = append(rec.Selections, SelectionRef{
		ID: "p.e.abc.m.spreads.x.away", MarketID: "p.e.abc.m.spreads", Role: "away", Name: "Detroit Lions",
	})
	rec.Prices = append(rec.Prices, PriceRef{
		SelectionID: "p.e.abc.m.spreads.x.away", BookID: "p.b.fanduel",
		Decimal: 1.98, Line: rec.Market.Line, ObservedAt: rec.ObservedAt,
	})
	want := rec.Hash()

	rec.Books[0], rec.Books[1] = rec.Books[1], rec.Books[0]
	rec.Selections[0], rec.Selections[1] = rec.Selections[1], rec.Selections[0]
	rec.Prices[0], rec.Prices[1] = rec.Prices[1], rec.Prices[0]

	if got := rec.Hash(); got != want {
		t.Fatalf("reordering the collections changed the fingerprint: %s, want %s", got, want)
	}
}

// TestFingerprintDistinguishesAbsentFromZeroLine is the distinction domain.Line
// exists to preserve: a moneyline has no line and a pick'em has 0.0, and "a bare
// float64 cannot tell those two apart, and the failure mode is silent".
func TestFingerprintDistinguishesAbsentFromZeroLine(t *testing.T) {
	zero, err := domain.NewLine(0)
	if err != nil {
		t.Fatal(err)
	}

	absent := sampleRecord()
	absent.Market.Line = domain.NoLine()
	pickem := sampleRecord()
	pickem.Market.Line = zero

	if absent.Hash() == pickem.Hash() {
		t.Fatal("an absent market line and a pick'em hash the same")
	}

	absentPrice := sampleRecord()
	absentPrice.Prices[0].Line = domain.NoLine()
	zeroPrice := sampleRecord()
	zeroPrice.Prices[0].Line = zero
	if absentPrice.Hash() == zeroPrice.Hash() {
		t.Fatal("an absent quote line and a pick'em quote hash the same")
	}
}

// TestFingerprintFoldsNegativeZero pins the -0/+0 fold in canon.f.
//
// They compare equal in Go, so a market oscillating between them is not
// changing — but they format as "-0" and "0" and would hash apart, which would
// republish the market on every single poll.
func TestFingerprintFoldsNegativeZero(t *testing.T) {
	pos, err := domain.NewLine(0)
	if err != nil {
		t.Fatal(err)
	}
	neg, err := domain.NewLine(negZero())
	if err != nil {
		t.Fatal(err)
	}

	a := sampleRecord()
	a.Market.Line = pos
	a.Prices[0].Decimal = 1.5
	b := sampleRecord()
	b.Market.Line = neg
	b.Prices[0].Decimal = 1.5

	if a.Hash() != b.Hash() {
		t.Fatal("a line of -0 and a line of +0 produced different fingerprints")
	}
}

// negZero returns -0.0 without the compiler folding the literal.
func negZero() float64 {
	v := 0.0
	return -v
}

// TestFingerprintIsLengthDelimited pins the encoding against the ambiguity a
// plain concatenation would have: ("ab","c") and ("a","bc") must not collide,
// because a collision there freezes a market silently.
func TestFingerprintIsLengthDelimited(t *testing.T) {
	a := sampleRecord()
	a.Sport.Slug = "ab"
	a.Sport.Name = "c"

	b := sampleRecord()
	b.Sport.Slug = "a"
	b.Sport.Name = "bc"

	if a.Hash() == b.Hash() {
		t.Fatal("two distinct field tuples produced one fingerprint; the encoding is not length-delimited")
	}
}

// TestFingerprintIsStableAcrossCalls guards against a hasher that accumulated
// state between invocations — the failure that would make every second poll
// look like a move.
func TestFingerprintIsStableAcrossCalls(t *testing.T) {
	rec := sampleRecord()
	first := rec.Hash()
	for i := 0; i < 8; i++ {
		if got := rec.Hash(); got != first {
			t.Fatalf("call %d returned %s, want %s", i, got, first)
		}
	}
	if got := sampleRecord().Hash(); got != first {
		t.Fatalf("an independently built identical record hashed to %s, want %s", got, first)
	}
}
