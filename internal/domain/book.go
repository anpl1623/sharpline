package domain

import "fmt"

// SyntheticBookSlug is the slug of the in-house book that the synthetic
// provider quotes under.
//
// CLAUDE.md §4 requires "a synthetic in-house book for development", and the
// ledger fixes the synthetic provider as the no-API-key path: it is a live
// stochastic market maker, not fixture data, so the prices attributed to this
// book are computed rather than canned. Naming its slug as a constant means the
// board, the analytics, and the tests all agree on which book that is instead
// of each hard-coding a string.
const SyntheticBookSlug Slug = "sharpline"

// BookKind distinguishes a book whose lines are ingested from a real provider
// from the in-house synthetic one.
//
// It matters beyond bookkeeping. An arbitrage or +EV signal computed against
// synthetic quotes is a statement about a random number generator, so any
// analytics surface that presents a signal as actionable has to be able to tell
// the two apart — and that is a property of the book, which is why it lives
// here and not in a config file.
type BookKind uint8

const (
	// BookKindUnknown is the invalid zero value.
	BookKindUnknown BookKind = iota

	// BookKindExternal is a real sportsbook whose lines are ingested from a
	// provider.
	BookKindExternal

	// BookKindSynthetic is the in-house book quoted by the synthetic provider.
	BookKindSynthetic
)

// String implements fmt.Stringer. The lowercase forms are the serialized
// values used by the database, the bus, and the API.
func (k BookKind) String() string {
	switch k {
	case BookKindExternal:
		return "external"
	case BookKindSynthetic:
		return "synthetic"
	default:
		return "unknown"
	}
}

// Valid reports whether k is a defined kind.
func (k BookKind) Valid() bool {
	return k == BookKindExternal || k == BookKindSynthetic
}

// ParseBookKind is the inverse of String for the defined kinds.
func ParseBookKind(s string) (BookKind, error) {
	switch s {
	case "external":
		return BookKindExternal, nil
	case "synthetic":
		return BookKindSynthetic, nil
	default:
		return BookKindUnknown, fmt.Errorf("book kind %q: %w", sample(s), ErrUnknownBookKind)
	}
}

// MarshalText implements encoding.TextMarshaler.
func (k BookKind) MarshalText() ([]byte, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("book kind %d: %w", uint8(k), ErrUnknownBookKind)
	}
	return []byte(k.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (k *BookKind) UnmarshalText(b []byte) error {
	parsed, err := ParseBookKind(string(b))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// Book is a sportsbook whose lines Sharpline ingests.
type Book struct {
	id        BookID
	slug      Slug
	name      string
	kind      BookKind
	reference bool
}

// BookParams is the input to NewBook.
type BookParams struct {
	ID   BookID
	Slug Slug
	Name string
	Kind BookKind

	// Reference marks this book as the sharp reference that CLAUDE.md §6's
	// positive-EV finder prices against.
	//
	// It is orthogonal to Kind, not derived from it: a book can be external and
	// sharp (Pinnacle), external and soft (a recreational book), or synthetic
	// and — in the offline no-API-key path — the only quote source there is,
	// which is why marking a synthetic book as the reference is permitted
	// rather than rejected. Refusing it would leave the offline demo with no
	// reference at all and the +EV surface permanently empty.
	//
	// That exactly one book should carry this flag is a property of the
	// catalogue, not of any single Book, so it is enforced where books are
	// stored rather than here.
	Reference bool
}

// NewBook validates its input and returns an immutable Book.
func NewBook(p BookParams) (Book, error) {
	if err := validID(string(p.ID)); err != nil {
		return Book{}, idErr("book id", string(p.ID), err)
	}
	if _, err := NewSlug(string(p.Slug)); err != nil {
		return Book{}, fmt.Errorf("book %s: %w", p.ID, err)
	}
	if !p.Kind.Valid() {
		return Book{}, fmt.Errorf("book %s: %w", p.ID, ErrUnknownBookKind)
	}
	name, err := validateName("book name", p.Name)
	if err != nil {
		return Book{}, fmt.Errorf("book %s: %w", p.ID, err)
	}
	return Book{id: p.ID, slug: p.Slug, name: name, kind: p.Kind, reference: p.Reference}, nil
}

// NewSyntheticBook returns the in-house book the synthetic provider quotes
// under, with the canonical slug.
//
// The identifier is supplied by the caller because it comes from persistence;
// everything else about this book is fixed by the system rather than by a
// provider, so it is defined once here instead of being respelled by the
// ingester, the seed path, and each test.
func NewSyntheticBook(id BookID, reference bool) (Book, error) {
	return NewBook(BookParams{
		ID:        id,
		Slug:      SyntheticBookSlug,
		Name:      "Sharpline Synthetic",
		Kind:      BookKindSynthetic,
		Reference: reference,
	})
}

// ID returns the book's identifier.
func (b Book) ID() BookID { return b.id }

// Slug returns the book's stable human-readable key.
func (b Book) Slug() Slug { return b.slug }

// Name returns the book's display name.
func (b Book) Name() string { return b.name }

// Kind returns whether the book is external or the in-house synthetic one.
func (b Book) Kind() BookKind { return b.kind }

// IsSynthetic reports whether the book's quotes are generated rather than
// ingested. Any surface presenting a signal as actionable should say so when
// this is true.
func (b Book) IsSynthetic() bool { return b.kind == BookKindSynthetic }

// IsReference reports whether the book is the sharp reference the +EV finder
// prices against.
func (b Book) IsReference() bool { return b.reference }

// Quoted reports whether the given price came from this book.
func (b Book) Quoted(p Price) bool { return !b.id.IsZero() && p.BookID() == b.id }

// IsZero reports whether b is the zero Book, which no constructor produces.
func (b Book) IsZero() bool { return b == Book{} }

// String implements fmt.Stringer.
func (b Book) String() string {
	if b.IsZero() {
		return "book(<zero>)"
	}
	if b.reference {
		return fmt.Sprintf("book(%s %s reference)", b.slug, b.kind)
	}
	return fmt.Sprintf("book(%s %s)", b.slug, b.kind)
}
