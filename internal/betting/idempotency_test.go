package betting

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/anpl1623/sharpline/internal/domain"
)

// domainIDCharset is migrations/00006's `id ~ '^[A-Za-z0-9._-]{1,128}$'`, which
// every derived identifier has to satisfy or the INSERT fails a CHECK
// constraint. Reproduced here rather than imported because domain.validID is
// unexported, and because asserting against the REGEX THE DATABASE USES is the
// point — a Go-side check that agreed with a different pattern would prove
// nothing about what Postgres will accept.
var domainIDCharset = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func TestDeriveWagerIDIsDeterministic(t *testing.T) {
	t.Parallel()

	first, err := DeriveWagerID(testUser, testKey, 0)
	if err != nil {
		t.Fatalf("DeriveWagerID: %v", err)
	}
	second, err := DeriveWagerID(testUser, testKey, 0)
	if err != nil {
		t.Fatalf("DeriveWagerID: %v", err)
	}
	if first != second {
		t.Fatalf("DeriveWagerID is not deterministic: %s != %s", first, second)
	}
	if !domainIDCharset.MatchString(first.String()) {
		t.Fatalf("derived id %q does not satisfy the schema's charset", first)
	}
}

func TestDeriveWagerIDSeparatesInputs(t *testing.T) {
	t.Parallel()

	base, err := DeriveWagerID("user-1", "key-1", 0)
	if err != nil {
		t.Fatalf("DeriveWagerID: %v", err)
	}

	tests := []struct {
		name        string
		user        domain.UserID
		key         string
		combination int
	}{
		{name: "a different user", user: "user-2", key: "key-1"},
		{name: "a different key", user: "user-1", key: "key-2"},
		{name: "a different combination", user: "user-1", key: "key-1", combination: 1},
		{
			// The collision length-prefixing exists to prevent: without it,
			// ("user-1", "key-1") and ("user-1k", "ey-1") concatenate to the
			// same bytes and two customers' placements derive one wager id.
			name: "a boundary shifted between the two fields",
			user: "user-1k", key: "ey-1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := DeriveWagerID(tc.user, tc.key, tc.combination)
			if err != nil {
				t.Fatalf("DeriveWagerID: %v", err)
			}
			if got == base {
				t.Fatalf("DeriveWagerID(%s, %q, %d) collided with the base id %s",
					tc.user, tc.key, tc.combination, base)
			}
		})
	}
}

// TestDerivedIDKindsDoNotCollide covers the one collision the ordinal alone
// cannot prevent: a round robin's parent and its first ticket share a user, a
// key and an ordinal, and are told apart only by the field tag in the hash.
func TestDerivedIDKindsDoNotCollide(t *testing.T) {
	t.Parallel()

	wager, err := DeriveWagerID(testUser, testKey, 0)
	if err != nil {
		t.Fatalf("DeriveWagerID: %v", err)
	}
	parent, err := DeriveRoundRobinID(testUser, testKey)
	if err != nil {
		t.Fatalf("DeriveRoundRobinID: %v", err)
	}
	leg, err := DeriveLegID(wager, "sel-1")
	if err != nil {
		t.Fatalf("DeriveLegID: %v", err)
	}
	txn, err := DeriveTransactionID(wager, domain.EntryKindStake)
	if err != nil {
		t.Fatalf("DeriveTransactionID: %v", err)
	}

	seen := map[string]string{}
	for name, id := range map[string]string{
		"wager":       wager.String(),
		"round robin": parent.String(),
		"leg":         leg.String(),
		"transaction": txn.String(),
	} {
		if !domainIDCharset.MatchString(id) {
			t.Errorf("%s id %q does not satisfy the schema's charset", name, id)
		}
		// The digests must differ, not merely the prefixes: two ids that were
		// the same hash under different prefixes would collide the moment
		// somebody normalised the prefixes away.
		digest := id[strings.Index(id, "_")+1:]
		if other, dup := seen[digest]; dup {
			t.Fatalf("%s and %s derive the same digest %q", name, other, digest)
		}
		seen[digest] = name
	}
}

// TestDeriveLegIDIsPerTicket is the requirement migrations/00006 states
// directly: RoundRobin.Combinations() returns subsets of the SAME []Leg values,
// so leg AB.a and leg AC.a arrive carrying one LegID, and the betting service
// must mint a distinct LegID per (ticket, selection) or the second INSERT
// violates legs_pkey.
func TestDeriveLegIDIsPerTicket(t *testing.T) {
	t.Parallel()

	ticketA, err := DeriveWagerID(testUser, testKey, 0)
	if err != nil {
		t.Fatalf("DeriveWagerID: %v", err)
	}
	ticketB, err := DeriveWagerID(testUser, testKey, 1)
	if err != nil {
		t.Fatalf("DeriveWagerID: %v", err)
	}

	onA, err := DeriveLegID(ticketA, "sel-1")
	if err != nil {
		t.Fatalf("DeriveLegID: %v", err)
	}
	onB, err := DeriveLegID(ticketB, "sel-1")
	if err != nil {
		t.Fatalf("DeriveLegID: %v", err)
	}
	if onA == onB {
		t.Fatal("the same selection on two tickets derived one leg id; legs_pkey would refuse the second insert")
	}

	// And the same selection on the same ticket is stable, so a replay produces
	// byte-identical legs.
	again, err := DeriveLegID(ticketA, "sel-1")
	if err != nil {
		t.Fatalf("DeriveLegID: %v", err)
	}
	if again != onA {
		t.Fatalf("DeriveLegID is not deterministic: %s != %s", again, onA)
	}
}

func TestDeriveTransactionIDSeparatesKinds(t *testing.T) {
	t.Parallel()

	wager, err := DeriveWagerID(testUser, testKey, 0)
	if err != nil {
		t.Fatalf("DeriveWagerID: %v", err)
	}

	// One wager accumulates several movements over its life — a stake at
	// placement, a payout or a loss at settlement — and they must not collide.
	seen := map[domain.TransactionID]domain.EntryKind{}
	for _, kind := range []domain.EntryKind{
		domain.EntryKindStake,
		domain.EntryKindPayout,
		domain.EntryKindLoss,
		domain.EntryKindRefund,
		domain.EntryKindCashOut,
		domain.EntryKindAdjustment,
	} {
		id, err := DeriveTransactionID(wager, kind)
		if err != nil {
			t.Fatalf("DeriveTransactionID(%s): %v", kind, err)
		}
		if other, dup := seen[id]; dup {
			t.Fatalf("%s and %s derive the same transaction id", kind, other)
		}
		seen[id] = kind
	}
}

func TestDerivationRefusesBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func() error
		want error
	}{
		{
			name: "no user",
			call: func() error { _, err := DeriveWagerID("", testKey, 0); return err },
			want: domain.ErrEmptyID,
		},
		{
			name: "no key",
			call: func() error { _, err := DeriveWagerID(testUser, "", 0); return err },
			want: ErrIdempotencyKeyRequired,
		},
		{
			name: "a whitespace-only key",
			call: func() error { _, err := DeriveWagerID(testUser, "   ", 0); return err },
			want: ErrIdempotencyKeyRequired,
		},
		{
			name: "an over-long key",
			call: func() error {
				_, err := DeriveWagerID(testUser, strings.Repeat("k", MaxIdempotencyKeyLen+1), 0)
				return err
			},
			want: ErrIdempotencyKeyInvalid,
		},
		{
			name: "no wager for a leg",
			call: func() error { _, err := DeriveLegID("", "sel-1"); return err },
			want: domain.ErrEmptyID,
		},
		{
			name: "no selection for a leg",
			call: func() error { _, err := DeriveLegID("wgr_x", ""); return err },
			want: domain.ErrEmptyID,
		},
		{
			name: "an unknown entry kind",
			call: func() error { _, err := DeriveTransactionID("wgr_x", domain.EntryKindUnknown); return err },
			want: domain.ErrUnknownEntryKind,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.call(); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want errors.Is(_, %v)", err, tc.want)
			}
		})
	}
}

func TestNormaliseIdempotencyKeyTrims(t *testing.T) {
	t.Parallel()

	// The key arrives as an HTTP header value, and a trailing space that some
	// proxy did or did not strip would derive a DIFFERENT wager id on the retry
	// than on the original — which is the one failure this whole file exists to
	// prevent, arriving through the door nobody watches.
	for _, raw := range []string{"idem-1", " idem-1", "idem-1 ", "\tidem-1\n"} {
		got, err := normaliseIdempotencyKey(raw)
		if err != nil {
			t.Fatalf("normaliseIdempotencyKey(%q) = %v", raw, err)
		}
		if got != "idem-1" {
			t.Fatalf("normaliseIdempotencyKey(%q) = %q, want %q", raw, got, "idem-1")
		}
	}
}

// TestPropertyDeriveWagerIDIsInjective is the property the whole idempotency
// scheme rests on: distinct inputs derive distinct ids, so a collision is a
// SHA-256 collision and never an encoding accident. rapid drives arbitrary
// user ids and keys, including ones whose field boundaries can be shifted.
func TestPropertyDeriveWagerIDIsInjective(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		// User ids are charset-restricted by domain.NewUserID; keys are
		// arbitrary client input, which is exactly why the encoding cannot rely
		// on a separator character.
		userGen := rapid.StringMatching(`[A-Za-z0-9._-]{1,32}`)
		keyGen := rapid.StringN(1, 40, 40)
		combGen := rapid.IntRange(0, 1013)

		userA := domain.UserID(userGen.Draw(rt, "userA"))
		keyA := keyGen.Draw(rt, "keyA")
		combA := combGen.Draw(rt, "combA")

		userB := domain.UserID(userGen.Draw(rt, "userB"))
		keyB := keyGen.Draw(rt, "keyB")
		combB := combGen.Draw(rt, "combB")

		idA, err := DeriveWagerID(userA, keyA, combA)
		if err != nil {
			rt.Skip("input rejected: ", err)
		}
		idB, err := DeriveWagerID(userB, keyB, combB)
		if err != nil {
			rt.Skip("input rejected: ", err)
		}

		// The charset holds for every input, not merely for the fixtures: an id
		// that fails it is refused by a CHECK constraint at INSERT time, which
		// is a 500 for a placement that was otherwise correct.
		if !domainIDCharset.MatchString(idA.String()) {
			rt.Fatalf("derived id %q does not satisfy the schema's charset", idA)
		}

		// The trimmed key is what is hashed, so two keys that differ only in
		// surrounding whitespace are ONE key by design.
		sameInputs := userA == userB &&
			strings.TrimSpace(keyA) == strings.TrimSpace(keyB) &&
			combA == combB

		if sameInputs && idA != idB {
			rt.Fatalf("equal inputs derived different ids: %s != %s", idA, idB)
		}
		if !sameInputs && idA == idB {
			rt.Fatalf("distinct inputs (%s,%q,%d) and (%s,%q,%d) derived one id %s",
				userA, keyA, combA, userB, keyB, combB, idA)
		}
	})
}

// TestPropertyDigestIsStableAcrossFieldBoundaries pins the length-prefixing
// directly, at the level of the encoding rather than of the identifier: no
// re-partition of one concatenation produces one digest.
func TestPropertyDigestIsStableAcrossFieldBoundaries(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		joined := rapid.StringN(2, 24, 24).Draw(rt, "joined")
		if len(joined) < 2 {
			rt.Skip("nothing to split")
		}
		i := rapid.IntRange(1, len(joined)-1).Draw(rt, "i")
		j := rapid.IntRange(1, len(joined)-1).Draw(rt, "j")
		if i == j {
			rt.Skip("same split point")
		}
		if digest("wager", joined[:i], joined[i:], 0) == digest("wager", joined[:j], joined[j:], 0) {
			rt.Fatalf("splitting %q at %d and at %d produced one digest", joined, i, j)
		}
	})
}
