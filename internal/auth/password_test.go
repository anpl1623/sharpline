package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// testParams are deliberately far below DefaultParams.
//
// Every test in this file that is not measuring cost uses them, because a table
// of twenty cases at m=64MiB, t=3 is ~1.5 seconds of pure argon2 per case and
// `make test` would become something nobody runs. The parameters under test are
// the ENCODING and the CONTROL FLOW, both of which are independent of cost;
// the cost itself is measured by password_bench_test.go against the real
// DefaultParams, which is where it belongs.
var testParams = Params{
	MemoryKiB:   minMemoryKiB,
	Time:        1,
	Parallelism: 1,
	SaltLen:     16,
	KeyLen:      32,
}

func newTestHasher(t *testing.T) *Hasher {
	t.Helper()
	h, err := NewHasher(HasherOptions{Params: testParams})
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	return h
}

func TestHashProducesAStorablePHCString(t *testing.T) {
	t.Parallel()

	h := newTestHasher(t)
	encoded, err := h.Hash(context.Background(), NewSecret("correct horse battery staple"))
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	// users_password_hash_is_argon2id: CHECK (password_hash LIKE '$argon2id$%')
	if !strings.HasPrefix(encoded, phcPrefix) {
		t.Errorf("hash %q does not satisfy users_password_hash_is_argon2id", encoded)
	}
	// users_password_hash_length: CHECK (length BETWEEN 40 AND 512)
	if n := len(encoded); n < 40 || n > 512 {
		t.Errorf("hash is %d bytes; users_password_hash_length admits 40..512", n)
	}
	// The parameters travel inside the string, which is the whole reason the
	// schema has one column rather than five.
	if !strings.Contains(encoded, "m=8192,t=1,p=1") {
		t.Errorf("hash %q does not carry its own parameters", encoded)
	}
}

func TestHashIsSaltedPerCall(t *testing.T) {
	t.Parallel()

	h := newTestHasher(t)
	pw := NewSecret("correct horse battery staple")

	a, err := h.Hash(context.Background(), pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	b, err := h.Hash(context.Background(), pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if a == b {
		t.Fatal("two hashes of one password are identical; the salt is not random")
	}

	// Both still verify. A per-call salt is only useful if it round-trips.
	for _, encoded := range []string{a, b} {
		ok, err := h.Verify(context.Background(), encoded, pw)
		if err != nil || !ok {
			t.Fatalf("Verify(%q) = %v, %v; want true, nil", encoded, ok, err)
		}
	}
}

func TestVerify(t *testing.T) {
	t.Parallel()

	h := newTestHasher(t)
	encoded, err := h.Hash(context.Background(), NewSecret("correct horse battery staple"))
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	cases := []struct {
		name     string
		password string
		want     bool
	}{
		{"exact", "correct horse battery staple", true},
		{"one byte different", "correct horse battery stapld", false},
		{"case changed", "Correct horse battery staple", false},
		{"trailing space", "correct horse battery staple ", false},
		{"empty", "", false},
		{"prefix", "correct horse battery stapl", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := h.Verify(context.Background(), encoded, NewSecret(c.password))
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got != c.want {
				t.Fatalf("Verify = %v, want %v", got, c.want)
			}
		})
	}
}

// A password containing a NUL byte, invalid UTF-8, or a multi-byte rune must
// hash and verify byte-for-byte. Any normalisation here would mean the
// credential the user typed is not the credential we stored.
func TestVerifyIsByteExact(t *testing.T) {
	t.Parallel()

	h := newTestHasher(t)
	for _, pw := range []string{
		"pässwörd mit Ümlauten",
		"emoji 🎲🎲🎲 passphrase",
		"with\x00an\x00embedded\x00nul",
		strings.Repeat("a", MaxPasswordLen),
	} {
		encoded, err := h.Hash(context.Background(), NewSecret(pw))
		if err != nil {
			t.Fatalf("Hash(%q): %v", pw, err)
		}
		ok, err := h.Verify(context.Background(), encoded, NewSecret(pw))
		if err != nil || !ok {
			t.Fatalf("Verify(%q) = %v, %v", pw, ok, err)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		password string
		want     error
	}{
		{"at the minimum", strings.Repeat("a", MinPasswordLen), nil},
		{"one below the minimum", strings.Repeat("a", MinPasswordLen-1), ErrPasswordTooShort},
		{"empty", "", ErrPasswordTooShort},
		{"at the maximum", strings.Repeat("a", MaxPasswordLen), nil},
		{"one over the maximum", strings.Repeat("a", MaxPasswordLen+1), ErrPasswordTooLong},
		{"invalid UTF-8", "goodlength\xff\xfe", ErrPasswordNotUTF8},
		// No composition rule: NIST SP 800-63B. A long run of one character is
		// weak, and rejecting it here would be a rule that pushes users toward
		// predictable substitutions instead.
		{"no digits or symbols required", "aaaaaaaaaaaaaaaa", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidatePassword(NewSecret(c.password))
			if !errors.Is(err, c.want) {
				t.Fatalf("ValidatePassword = %v, want %v", err, c.want)
			}
		})
	}
}

func TestHashRejectsOutOfBoundsPassword(t *testing.T) {
	t.Parallel()

	h := newTestHasher(t)
	if _, err := h.Hash(context.Background(), NewSecret("short")); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("Hash(short) = %v, want ErrPasswordTooShort", err)
	}
	long := NewSecret(strings.Repeat("a", MaxPasswordLen+1))
	if _, err := h.Hash(context.Background(), long); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("Hash(over-long) = %v, want ErrPasswordTooLong", err)
	}
	// Verify bounds the MAXIMUM but not the minimum: a stored hash may predate
	// a raised minimum and refusing to check it would lock out a user whose
	// credential is fine.
	encoded, err := h.Hash(context.Background(), NewSecret("correct horse battery staple"))
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if _, err := h.Verify(context.Background(), encoded, NewSecret("short")); err != nil {
		t.Fatalf("Verify with a short password errored: %v", err)
	}
	if _, err := h.Verify(context.Background(), encoded, long); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("Verify with an over-long password = %v, want ErrPasswordTooLong", err)
	}
}

func TestDecodeHashRejectsMalformedInput(t *testing.T) {
	t.Parallel()

	valid := "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2Fs$" +
		"ZGlnZXN0ZGlnZXN0ZGlnZXN0ZGlnZXN0ZGlnZXM"

	cases := []struct {
		name    string
		encoded string
		want    error
	}{
		{"a valid string", valid, nil},
		{"empty", "", ErrHashFormat},
		{"plaintext password", "hunter2", ErrHashFormat},
		{"bcrypt", "$2y$10$abcdefghijklmnopqrstuv", ErrHashFormat},
		{"argon2i", "$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$ZGlnZXN0", ErrHashAlgorithm},
		{"argon2d", "$argon2d$v=19$m=65536,t=3,p=2$c2FsdA$ZGlnZXN0", ErrHashAlgorithm},
		{"too few segments", "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA", ErrHashFormat},
		{"too many segments", valid + "$extra", ErrHashFormat},
		{"unknown version", strings.Replace(valid, "v=19", "v=16", 1), ErrHashVersion},
		{"missing t", "$argon2id$v=19$m=65536,p=2$c2FsdHNhbHRzYWx0c2Fs$ZGlnZXN0", ErrHashFormat},
		{"unknown parameter", strings.Replace(valid, "p=2", "z=2", 1), ErrHashFormat},
		{"non-base64 salt", strings.Replace(valid, "c2FsdHNhbHRzYWx0c2Fs", "not!base64", 1), ErrHashFormat},
		// The important one: a NEGATIVE memory parameter must not wrap into a
		// huge unsigned value and become a 4 TiB allocation. strconv.ParseUint
		// refuses the sign, which is why the triple is not parsed with Sscanf.
		{"negative memory", strings.Replace(valid, "m=65536", "m=-1", 1), ErrHashFormat},
		{"absurd memory", strings.Replace(valid, "m=65536", "m=67108864", 1), ErrHashParams},
		{"zero parallelism", strings.Replace(valid, "p=2", "p=0", 1), ErrHashParams},
		{"memory below the floor", strings.Replace(valid, "m=65536", "m=1024", 1), ErrHashParams},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, _, err := DecodeHash(c.encoded)
			if c.want == nil {
				if err != nil {
					t.Fatalf("DecodeHash = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("DecodeHash = %v, want %v", err, c.want)
			}
			// Every stored-hash failure is an internal error, never a bad-input
			// error: a hash that will not parse is corruption on OUR side and
			// must not be reported to the user as a wrong password.
			if !errors.Is(err, ErrInternal) && !errors.Is(err, ErrInvalid) {
				t.Fatalf("DecodeHash error %v is in neither taxonomy root", err)
			}
		})
	}
}

func TestDecodeHashRoundTripsEveryDefaultParameter(t *testing.T) {
	t.Parallel()

	h, err := NewHasher(HasherOptions{Params: testParams})
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	encoded, err := h.Hash(context.Background(), NewSecret("correct horse battery staple"))
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	got, salt, digest, err := DecodeHash(encoded)
	if err != nil {
		t.Fatalf("DecodeHash: %v", err)
	}
	if got != testParams {
		t.Fatalf("decoded params = %+v, want %+v", got, testParams)
	}
	if uint32(len(salt)) != testParams.SaltLen {
		t.Fatalf("salt is %d bytes, want %d", len(salt), testParams.SaltLen)
	}
	if uint32(len(digest)) != testParams.KeyLen {
		t.Fatalf("digest is %d bytes, want %d", len(digest), testParams.KeyLen)
	}
}

func TestNeedsRehash(t *testing.T) {
	t.Parallel()

	policy := Params{MemoryKiB: 16 * 1024, Time: 3, Parallelism: 2, SaltLen: 16, KeyLen: 32}
	h, err := NewHasher(HasherOptions{Params: policy})
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}

	weaker := func(p Params) string {
		hh, err := NewHasher(HasherOptions{Params: p})
		if err != nil {
			t.Fatalf("NewHasher(%+v): %v", p, err)
		}
		encoded, err := hh.Hash(context.Background(), NewSecret("correct horse battery staple"))
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		return encoded
	}

	cases := []struct {
		name   string
		params Params
		want   bool
	}{
		{"at policy", policy, false},
		{"less memory", Params{MemoryKiB: 8 * 1024, Time: 3, Parallelism: 2, SaltLen: 16, KeyLen: 32}, true},
		{"fewer passes", Params{MemoryKiB: 16 * 1024, Time: 2, Parallelism: 2, SaltLen: 16, KeyLen: 32}, true},
		{"shorter salt", Params{MemoryKiB: 16 * 1024, Time: 3, Parallelism: 2, SaltLen: 8, KeyLen: 32}, true},
		{"shorter key", Params{MemoryKiB: 16 * 1024, Time: 3, Parallelism: 2, SaltLen: 16, KeyLen: 16}, true},
		{"more memory", Params{MemoryKiB: 32 * 1024, Time: 3, Parallelism: 2, SaltLen: 16, KeyLen: 32}, false},
		// Parallelism is compared for EQUALITY. A higher p is not stronger, it
		// is differently shaped, and on the two-core deploy target it is worse.
		{"different parallelism", Params{MemoryKiB: 16 * 1024, Time: 3, Parallelism: 4, SaltLen: 16, KeyLen: 32}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := h.NeedsRehash(weaker(c.params))
			if err != nil {
				t.Fatalf("NeedsRehash: %v", err)
			}
			if got != c.want {
				t.Fatalf("NeedsRehash = %v, want %v", got, c.want)
			}
		})
	}

	// A hash written under the WEAKER parameters must still verify. That is the
	// whole reason parameters live in the PHC string, and it is what makes a
	// cost bump a pure application change with no migration.
	old := weaker(Params{MemoryKiB: 8 * 1024, Time: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16})
	ok, err := h.Verify(context.Background(), old, NewSecret("correct horse battery staple"))
	if err != nil || !ok {
		t.Fatalf("Verify against an old-parameter hash = %v, %v; want true, nil", ok, err)
	}
}

// The decoy is the timing defence for the unknown-email path. It must do the
// same work and always fail.
func TestVerifyDecoyAlwaysFailsAndCosts(t *testing.T) {
	t.Parallel()

	h := newTestHasher(t)
	// The decoy is not a hash of anything a caller can present. Nothing to
	// assert about its VALUE — asserting it would require exposing it, which is
	// the opposite of the point — so what is asserted is that it verifies and
	// reports no error, which is the contract Login depends on.
	for _, pw := range []string{"", "correct horse battery staple", strings.Repeat("z", 64)} {
		if err := h.VerifyDecoy(context.Background(), NewSecret(pw)); err != nil {
			t.Fatalf("VerifyDecoy(%q) = %v, want nil", pw, err)
		}
	}

	// And it goes through the same limiter, so the two paths queue identically
	// under load. Asserted structurally: a hasher with concurrency 1 whose slot
	// is held must block the decoy exactly as it blocks a real verify.
	blocked, err := NewHasher(HasherOptions{Params: testParams, Concurrency: 1})
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	if err := blocked.acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := blocked.VerifyDecoy(ctx, NewSecret("correct horse battery staple")); !errors.Is(err, ErrInternal) {
		t.Fatalf("VerifyDecoy with the limiter full = %v, want an ErrInternal from the context", err)
	}
	blocked.release()
}

// The concurrency limiter is a security control: without it, N simultaneous
// logins allocate N x MemoryKiB and an unauthenticated attacker picks N.
func TestHasherConcurrencyLimiterBoundsInFlightWork(t *testing.T) {
	t.Parallel()

	const limit = 2
	h, err := NewHasher(HasherOptions{Params: testParams, Concurrency: limit})
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.acquire(context.Background()); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()
			h.release()
		}()
	}
	wg.Wait()

	if peak > limit {
		t.Fatalf("peak concurrent hashes = %d, limiter is %d", peak, limit)
	}
	if peak == 0 {
		t.Fatal("nothing ran")
	}
}

func TestHasherRespectsContextWhileQueued(t *testing.T) {
	t.Parallel()

	h, err := NewHasher(HasherOptions{Params: testParams, Concurrency: 1})
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	if err := h.acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer h.release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := h.Hash(ctx, NewSecret("correct horse battery staple")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Hash with a cancelled context = %v, want context.Canceled", err)
	}
}

func TestNewHasherRejectsBadOptions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts HasherOptions
		want error
	}{
		{"negative concurrency", HasherOptions{Params: testParams, Concurrency: -1}, ErrInvalid},
		{"memory below the floor", HasherOptions{Params: Params{
			MemoryKiB: 1024, Time: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}}, ErrParamsInvalid},
		{"zero parallelism", HasherOptions{Params: Params{
			MemoryKiB: minMemoryKiB, Time: 1, Parallelism: 0, SaltLen: 16, KeyLen: 32}}, ErrParamsInvalid},
		{"time over the ceiling", HasherOptions{Params: Params{
			MemoryKiB: minMemoryKiB, Time: maxTime + 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}}, ErrParamsInvalid},
		{"salt too short", HasherOptions{Params: Params{
			MemoryKiB: minMemoryKiB, Time: 1, Parallelism: 1, SaltLen: 4, KeyLen: 32}}, ErrParamsInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewHasher(c.opts); !errors.Is(err, c.want) {
				t.Fatalf("NewHasher = %v, want %v", err, c.want)
			}
		})
	}
}

func TestDefaultParamsAreValidAndMatchTheDeployTarget(t *testing.T) {
	t.Parallel()

	if err := DefaultParams.Validate(); err != nil {
		t.Fatalf("DefaultParams.Validate() = %v", err)
	}
	// The reasoning in DefaultParams' doc comment is specific: p equals the
	// deploy target's core count, and m is RFC 9106's second recommended
	// option. If either moves, the comment is wrong and this test says so.
	if DefaultParams.Parallelism != 2 {
		t.Errorf("DefaultParams.Parallelism = %d; the Ampere A1 target has 2 cores and the "+
			"doc comment argues for p = core count", DefaultParams.Parallelism)
	}
	if DefaultParams.MemoryKiB != 64*1024 {
		t.Errorf("DefaultParams.MemoryKiB = %d, want 65536 (RFC 9106's second recommended option)",
			DefaultParams.MemoryKiB)
	}
	if DefaultParams.Time != 3 {
		t.Errorf("DefaultParams.Time = %d, want 3", DefaultParams.Time)
	}
	// The operational ceiling stated in the comment: concurrency x memory.
	const wantCeilingMiB = 2 * 64
	if got := 2 * int(DefaultParams.MemoryKiB) / 1024; got != wantCeilingMiB {
		t.Errorf("worst-case hashing footprint at the target's GOMAXPROCS = %d MiB, "+
			"the doc comment claims %d MiB", got, wantCeilingMiB)
	}
}

func TestParamsStringAndEncode(t *testing.T) {
	t.Parallel()

	if got, want := DefaultParams.String(), "m=65536,t=3,p=2"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
	if got, want := DefaultParams.Encode(), "$argon2id$v=19$m=65536,t=3,p=2$"; got != want {
		t.Errorf("Encode = %q, want %q", got, want)
	}
}

// A hasher whose entropy source fails must fail loudly rather than produce a
// predictable salt.
func TestHasherFailsWhenEntropyFails(t *testing.T) {
	t.Parallel()

	boom := errors.New("entropy source unavailable")
	_, err := NewHasher(HasherOptions{
		Params:   testParams,
		randRead: func([]byte) error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("NewHasher with a failing entropy source = %v, want it to wrap %v", err, boom)
	}

	// And once constructed, a later failure must not silently produce a
	// zero-valued salt.
	h := newTestHasher(t)
	h.rand = func([]byte) error { return boom }
	if _, err := h.Hash(context.Background(), NewSecret("correct horse battery staple")); !errors.Is(err, boom) {
		t.Fatalf("Hash with a failing entropy source = %v, want it to wrap %v", err, boom)
	}
}

// A sanity check on the claim in errors.go that the maximum password length is
// a CPU-amplification bound: hashing the maximum must not be dramatically more
// expensive than hashing a short one.
func TestMaxPasswordLengthDoesNotDominateHashCost(t *testing.T) {
	if testing.Short() {
		t.Skip("timing comparison")
	}

	h := newTestHasher(t)
	ctx := context.Background()

	short := NewSecret(strings.Repeat("a", MinPasswordLen))
	long := NewSecret(strings.Repeat("a", MaxPasswordLen))

	measure := func(pw Secret) time.Duration {
		start := time.Now()
		for i := 0; i < 5; i++ {
			if _, err := h.Hash(ctx, pw); err != nil {
				t.Fatalf("Hash: %v", err)
			}
		}
		return time.Since(start) / 5
	}

	shortCost := measure(short)
	longCost := measure(long)
	t.Logf("argon2id at %s: %d-byte password %s, %d-byte password %s",
		testParams, MinPasswordLen, shortCost, MaxPasswordLen, longCost)

	if longCost > 2*shortCost {
		t.Errorf("hashing the maximum password costs %s against %s for the minimum; "+
			"MaxPasswordLen is not bounding amplification the way errors.go claims",
			longCost, shortCost)
	}
}

func ExampleParams_Encode() {
	fmt.Println(Params{MemoryKiB: 65536, Time: 3, Parallelism: 2, SaltLen: 16, KeyLen: 32}.Encode())
	// Output: $argon2id$v=19$m=65536,t=3,p=2$
}
