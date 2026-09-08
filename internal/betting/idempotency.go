// Deterministic identifier derivation, which is the whole idempotency
// mechanism, plus the Redis fast path that sits in front of it.
//
// # The claim
//
// A placement carrying the same (userID, idempotencyKey) twice books ONE wager,
// and the second submit returns the first one. That guarantee comes from a
// UNIQUE INDEX — wagers_pkey — and from nothing else. Not from a lock, not from
// a lease, not from a cache, and not from a client that behaves.
//
// The mechanism is one line long: the wager id is a pure function of
// (userID, idempotencyKey, combinationIndex). A replayed submit therefore
// derives the SAME primary key, its INSERT collides, and the store reports
// [ErrAlreadyPlaced] (SQLSTATE 23505). placement.go treats that as the answer
// rather than as a failure: it reads the existing wager back and returns it.
//
// # Why this is better than the obvious alternatives
//
// A separate `idempotency_keys` table with a unique constraint would work and
// is what most systems do. It costs a second row, a second index, a retention
// policy, and — the real cost — a WINDOW: the key row and the wager row are two
// inserts, so a crash between them leaves a key claimed with no wager behind
// it, and every subsequent replay has to decide whether that means "in flight"
// or "lost". Deriving the id removes the second row entirely, and with it the
// window, because the thing being made unique IS the thing being written.
//
// A UUID minted per request would make replays undetectable. A hash of the
// whole slip body would make a resubmit of a DIFFERENT slip under the same key
// silently place a second bet, which is exactly what an idempotency key is
// supposed to prevent; the key is the client's declaration of intent and the
// body is not part of it.
//
// # Why the hash input is length-prefixed
//
// Without it, ("ab", "c") and ("a", "bc") concatenate to the same bytes and
// derive the same wager id, so two different customers' placements could
// collide. The user id is charset-restricted to [A-Za-z0-9._-] but the
// idempotency key is arbitrary client input, so no separator character is safe.
// Length-prefixing every field makes the encoding injective by construction,
// which is the property the whole scheme rests on.
//
// A DOMAIN TAG is mixed in for the same reason a password hash has a version
// byte: if the derivation ever changes, ids minted under the old scheme must
// not be reachable by the new one, and a tag makes that a one-character edit
// rather than a migration.
package betting

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash"
	"strings"

	"github.com/anpl1623/sharpline/internal/domain"
)

// derivationTag versions the hash input.
//
// Changing it changes every derived id, which would make every in-flight
// idempotency key miss and every replay book a second bet — so it changes only
// alongside a deliberate migration, never as a side effect of an edit here.
const derivationTag = "sharpline/betting/v1"

// Identifier prefixes.
//
// They exist so an id is readable in a log line, a span attribute and a psql
// dump without a lookup, and so two ids of different kinds are visibly
// different even when their hashes are not compared. Every one is inside
// domain's [A-Za-z0-9._-] charset.
const (
	wagerIDPrefix       = "wgr_"
	roundRobinIDPrefix  = "rrb_"
	legIDPrefix         = "leg_"
	transactionIDPrefix = "txn_"
)

// derivedDigestLen is how many base64url characters of the SHA-256 are kept.
//
// 43 characters is the full 256 bits (ceil(256/6)); nothing is truncated. The
// alternative — truncating to, say, 16 characters for readability — would
// reduce the collision space to 96 bits, and a collision here is not a cosmetic
// problem: it is one customer's wager id resolving to another's, which the
// user-id-in-the-hash makes cross-customer-safe but which would still merge two
// of one customer's tickets. Full width costs 27 characters in a column that
// allows 128.
const derivedDigestLen = 43

// noCombination is the combination ordinal used by every ticket that is not a
// round-robin expansion.
//
// It is 0 rather than a sentinel like -1, and that is deliberate: a straight
// under key K and the FIRST ticket of a round robin under key K derive the same
// id and therefore collide. That is the correct behaviour, not a bug — one key
// means one placement, so a client that submits two different slips under one
// key is telling the server they are the same request, and being handed the
// first one back is precisely what it asked for.
const noCombination = 0

// DeriveWagerID returns the wager id for one ticket of one placement.
//
// It is a PURE FUNCTION: the same inputs always produce the same id, different
// inputs produce different ids, and the result always satisfies
// domain.NewWagerID's charset (^[A-Za-z0-9._-]{1,128}$) — base64url emits only
// [A-Za-z0-9-_], and the prefix is alphanumeric with an underscore.
//
// `combination` is the ticket's ordinal within a round-robin expansion, and
// [noCombination] for everything else. It is part of the hash because a round
// robin's N tickets share one idempotency key and must not share one id.
func DeriveWagerID(user domain.UserID, key string, combination int) (domain.WagerID, error) {
	if err := checkDerivationInput(user, key); err != nil {
		return "", err
	}
	raw := wagerIDPrefix + digest("wager", string(user), key, int64(combination))
	id, err := domain.NewWagerID(raw)
	if err != nil {
		return "", fmt.Errorf("betting: derive wager id: %w", err)
	}
	return id, nil
}

// DeriveRoundRobinID returns the parent round robin's id for one placement.
//
// It uses a different field tag from [DeriveWagerID], so a round robin's parent
// and its first ticket can never collide even though they share a key and an
// ordinal.
func DeriveRoundRobinID(user domain.UserID, key string) (domain.RoundRobinID, error) {
	if err := checkDerivationInput(user, key); err != nil {
		return "", err
	}
	raw := roundRobinIDPrefix + digest("round_robin", string(user), key, noCombination)
	id, err := domain.NewRoundRobinID(raw)
	if err != nil {
		return "", fmt.Errorf("betting: derive round robin id: %w", err)
	}
	return id, nil
}

// DeriveLegID returns the id of one selection on one ticket.
//
// IT IS KEYED ON (WAGER, SELECTION) AND THAT IS A REQUIREMENT, NOT A CHOICE.
// migrations/00006 states it directly: RoundRobin.Combinations() returns subsets
// of the SAME []Leg values, so leg AB.a and leg AC.a arrive carrying one LegID,
// and "the betting service must MINT A DISTINCT LegID PER (TICKET, SELECTION)
// when it turns those combinations into wagers, or the second INSERT violates
// this primary key."
//
// That migration also records why the alternative was rejected: a composite
// PRIMARY KEY (wager_id, id) would let one LegID repeat across a round robin's
// tickets, but each ticket's legs grade independently and at different times, so
// two rows sharing an id would carry independent statuses — "an identifier that
// does not identify is worse than a longer key."
//
// Deriving it rather than minting a UUID keeps the whole placement reproducible:
// a replay that somehow reached the insert would produce byte-identical legs,
// so the collision is on wagers_pkey where it is understood, and never a
// half-written ticket.
func DeriveLegID(w domain.WagerID, sel domain.SelectionID) (domain.LegID, error) {
	if w.IsZero() {
		return "", fmt.Errorf("betting: derive leg id: %w", domain.ErrEmptyID)
	}
	if sel.IsZero() {
		return "", fmt.Errorf("betting: derive leg id: %w", domain.ErrEmptyID)
	}
	raw := legIDPrefix + digest("leg", string(w), string(sel), noCombination)
	id, err := domain.NewLegID(raw)
	if err != nil {
		return "", fmt.Errorf("betting: derive leg id: %w", err)
	}
	return id, nil
}

// DeriveTransactionID returns the ledger transaction id for a money movement
// about one wager.
//
// Derived rather than random so the whole placement is reproducible from its
// inputs, which is what makes a partially-applied placement diagnosable: given
// a wager id and a kind, the transaction that should exist has a computable id,
// so "did the ledger movement land" is a primary-key lookup rather than a scan.
//
// The kind is in the hash because one wager accumulates several movements over
// its life — a stake at placement, a payout or loss at settlement — and they
// must not collide. This package only ever mints the stake one; settlement
// mints its own with the same function.
func DeriveTransactionID(w domain.WagerID, kind domain.EntryKind) (domain.TransactionID, error) {
	if w.IsZero() {
		return "", fmt.Errorf("betting: derive transaction id: %w", domain.ErrEmptyID)
	}
	if !kind.Valid() {
		return "", fmt.Errorf("betting: derive transaction id: %w", domain.ErrUnknownEntryKind)
	}
	raw := transactionIDPrefix + digest("transaction", string(w), kind.String(), noCombination)
	id, err := domain.NewTransactionID(raw)
	if err != nil {
		return "", fmt.Errorf("betting: derive transaction id: %w", err)
	}
	return id, nil
}

// DeriveGrantTransactionID returns the ledger transaction id for a play-money
// grant, keyed on the SUBMIT rather than on a wager.
//
// A grant is the one money movement in this package that is not about a ticket,
// so [DeriveTransactionID]'s (wagerID, kind) key has nothing to bind to. It is
// keyed on (userID, idempotencyKey) instead — the same pair a wager id is
// derived from — which gives a grant exactly the same replay guarantee for
// exactly the same reason: a resubmitted top-up derives the id it already
// wrote, collides with ledger_transactions' primary key, and credits nobody
// twice.
//
// THIS IS THE PROPERTY THAT MATTERS MOST IN THE PACKAGE. A doubled wager is a
// visible, refundable mistake; a doubled grant silently mints money into a
// double-entry ledger that will still balance afterwards, because both halves
// are written. The derived key is what makes "credit me 500" not mean "credit
// me 500 per network retry".
//
// The field tag is "grant" rather than "transaction", so a grant id and a wager
// movement's id can never coincide even if a wager id ever equalled a user id.
func DeriveGrantTransactionID(user domain.UserID, key string) (domain.TransactionID, error) {
	if err := checkDerivationInput(user, key); err != nil {
		return "", err
	}
	raw := transactionIDPrefix + digest("grant", string(user), key, noCombination)
	id, err := domain.NewTransactionID(raw)
	if err != nil {
		return "", fmt.Errorf("betting: derive grant transaction id: %w", err)
	}
	return id, nil
}

// digest is the injective encoding described in the file header, hashed and
// rendered as base64url without padding.
//
// The encoding is: the domain tag, the field tag, both strings and the integer,
// each preceded by its length as a big-endian uint64. Every boundary is
// therefore recoverable from the bytes alone, so no two distinct input tuples
// produce the same input string — which is the property that makes a collision
// a SHA-256 collision rather than an encoding accident.
//
// sha256.Hash.Write never returns an error (its documented contract), so the
// return values are deliberately not checked; checking them would add an
// unreachable error path to four call sites that have nothing to do with it.
func digest(field, a, b string, n int64) string {
	h := sha256.New()
	writeLengthPrefixed(h, derivationTag)
	writeLengthPrefixed(h, field)
	writeLengthPrefixed(h, a)
	writeLengthPrefixed(h, b)

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(n))
	_, _ = h.Write(buf[:])

	sum := h.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(sum)[:derivedDigestLen]
}

// writeLengthPrefixed writes len(s) as a big-endian uint64 followed by s.
//
// The parameter is [hash.Hash] rather than a structural
// interface{ Write([]byte) (int, error) } because the named type is what
// carries the contract the unchecked writes rely on: hash.Hash documents that
// Write "never returns an error". A structural interface admits any writer,
// including ones that fail, so under it the discarded returns would be a real
// defect rather than a documented no-op — and errcheck reads it exactly that
// way. Naming the type states the guarantee instead of asserting it in a
// comment.
func writeLengthPrefixed(h hash.Hash, s string) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(len(s)))
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte(s))
}

// checkDerivationInput validates the two values every derived id is keyed on.
//
// The key is checked here rather than only at the API edge because derivation
// is exported: a caller that reached [DeriveWagerID] with an empty key would
// get an id that every other caller with an empty key also gets, which is a
// cross-request collision dressed up as an identifier.
func checkDerivationInput(user domain.UserID, key string) error {
	if user.IsZero() {
		return fmt.Errorf("betting: derive id: %w", domain.ErrEmptyID)
	}
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("betting: %w", ErrIdempotencyKeyRequired)
	}
	if len(key) > MaxIdempotencyKeyLen {
		return fmt.Errorf("betting: idempotency key is %d bytes, the maximum is %d: %w",
			len(key), MaxIdempotencyKeyLen, ErrIdempotencyKeyInvalid)
	}
	return nil
}

// normaliseIdempotencyKey trims surrounding whitespace and validates the result.
//
// Trimming matters because the key arrives as an HTTP header value, and a
// trailing space that some proxy did or did not strip would derive a different
// wager id on the retry than on the original — which is the one failure mode
// this entire file exists to prevent, arriving through the door nobody watches.
func normaliseIdempotencyKey(key string) (string, error) {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return "", fmt.Errorf("betting: %w", ErrIdempotencyKeyRequired)
	}
	if len(trimmed) > MaxIdempotencyKeyLen {
		return "", fmt.Errorf("betting: idempotency key is %d bytes, the maximum is %d: %w",
			len(trimmed), MaxIdempotencyKeyLen, ErrIdempotencyKeyInvalid)
	}
	return trimmed, nil
}
