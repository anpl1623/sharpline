package domain

import "fmt"

// League is a competition within a Sport: the NBA within basketball, the EPL
// within soccer.
//
// A league is the unit users browse and subscribe to — CLAUDE.md §5 defines a
// `league:{slug}` WebSocket channel — which is why Slug is a first-class,
// charset-constrained field rather than a derived display string.
//
// League holds its parent's SportID rather than an embedded Sport. Domain
// entities in this package reference their parent by identifier only: embedding
// would mean every League carrying a copy of its Sport that could drift out of
// date, and it would make the aggregate that must be loaded to produce one
// League unbounded.
type League struct {
	id      LeagueID
	sportID SportID
	slug    Slug
	name    string
}

// LeagueParams is the input to NewLeague.
type LeagueParams struct {
	ID      LeagueID
	SportID SportID
	Slug    Slug
	Name    string
}

// NewLeague validates its input and returns an immutable League.
func NewLeague(p LeagueParams) (League, error) {
	if err := validID(string(p.ID)); err != nil {
		return League{}, idErr("league id", string(p.ID), err)
	}
	if err := validID(string(p.SportID)); err != nil {
		return League{}, idErr("sport id", string(p.SportID), err)
	}
	if _, err := NewSlug(string(p.Slug)); err != nil {
		return League{}, fmt.Errorf("league %s: %w", p.ID, err)
	}
	name, err := validateName("league name", p.Name)
	if err != nil {
		return League{}, fmt.Errorf("league %s: %w", p.ID, err)
	}
	return League{id: p.ID, sportID: p.SportID, slug: p.Slug, name: name}, nil
}

// ID returns the league's identifier.
func (l League) ID() LeagueID { return l.id }

// SportID returns the identifier of the sport this league belongs to.
func (l League) SportID() SportID { return l.sportID }

// Slug returns the league's stable human-readable key.
func (l League) Slug() Slug { return l.slug }

// Name returns the league's display name.
func (l League) Name() string { return l.name }

// BelongsTo reports whether the league sits under the given sport.
func (l League) BelongsTo(s Sport) bool { return !l.sportID.IsZero() && l.sportID == s.ID() }

// IsZero reports whether l is the zero League, which no constructor produces.
func (l League) IsZero() bool { return l == League{} }

// String implements fmt.Stringer for logs and error messages.
func (l League) String() string {
	if l.IsZero() {
		return "league(<zero>)"
	}
	return fmt.Sprintf("league(%s %q)", l.slug, l.name)
}
