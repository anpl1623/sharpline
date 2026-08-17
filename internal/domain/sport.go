package domain

import "fmt"

// Sport is the root of the catalogue hierarchy: basketball, americanfootball,
// soccer.
//
// It is modelled as an entity rather than as a closed enum, and that is a
// deliberate trade. An enum would give compile-time exhaustiveness, but the set
// of sports is provider data — a new sport appearing in a feed would then
// require a code change, a release, and a migration to represent something the
// system only ever displays and groups by. Nothing in the pricing, settlement,
// or streaming path branches on which sport a market belongs to, so the
// compile-time check would protect no logic.
//
// Slug is the stable key. It, not the surrogate ID, is what appears in URLs and
// in operator configuration, because a human has to be able to read it.
type Sport struct {
	id   SportID
	slug Slug
	name string
}

// SportParams is the input to NewSport.
//
// Constructors take a params struct rather than a positional argument list
// because Sport, League, Event, Market, Selection, and Price all have at least
// three fields of compatible types, and a positional call site is exactly where
// two of them get swapped without the compiler noticing.
type SportParams struct {
	ID   SportID
	Slug Slug
	Name string
}

// NewSport validates its input and returns an immutable Sport.
//
// The ID and Slug are revalidated even though their types can only be produced
// by a validating constructor: a defined type over string can also be produced
// by conversion, so re-checking here is what makes "if you hold a Sport it is
// well-formed" true rather than merely intended.
func NewSport(p SportParams) (Sport, error) {
	if err := validID(string(p.ID)); err != nil {
		return Sport{}, idErr("sport id", string(p.ID), err)
	}
	if _, err := NewSlug(string(p.Slug)); err != nil {
		return Sport{}, fmt.Errorf("sport %s: %w", p.ID, err)
	}
	name, err := validateName("sport name", p.Name)
	if err != nil {
		return Sport{}, fmt.Errorf("sport %s: %w", p.ID, err)
	}
	return Sport{id: p.ID, slug: p.Slug, name: name}, nil
}

// ID returns the sport's identifier.
func (s Sport) ID() SportID { return s.id }

// Slug returns the sport's stable human-readable key.
func (s Sport) Slug() Slug { return s.slug }

// Name returns the sport's display name.
func (s Sport) Name() string { return s.name }

// IsZero reports whether s is the zero Sport, which no constructor produces.
func (s Sport) IsZero() bool { return s == Sport{} }

// String implements fmt.Stringer for logs and error messages.
func (s Sport) String() string {
	if s.IsZero() {
		return "sport(<zero>)"
	}
	return fmt.Sprintf("sport(%s %q)", s.slug, s.name)
}
