package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Password length bounds.
//
// The MINIMUM follows NIST SP 800-63B: at least 8 characters, and no
// composition rules (no forced symbol, no forced digit, no forced mixed case),
// because composition rules push users toward predictable substitutions and
// measurably reduce entropy. 12 is chosen over the 8 floor precisely BECAUSE
// there are no composition rules — length is the only knob left, so it is set
// where it does some work.
//
// The MAXIMUM is the security-relevant bound and it is not a usability choice.
// argon2id's first step hashes the password with BLAKE2b, whose cost is linear
// in input length, so an unbounded password field is a CPU-amplification
// primitive: one HTTP request, megabytes of hashing, on a two-core box.
// 1024 bytes is far past any real passphrase — NIST asks that at least 64 be
// accepted — and far below the point where length contributes to cost.
const (
	MinPasswordLen = 12
	MaxPasswordLen = 1024
)

// Params are the argon2id cost parameters.
//
// They travel INSIDE the PHC string (see [Params.Encode]) rather than in
// columns, which is what migrations/00005 built the users.password_hash column
// to hold and why a cost bump is a pure application change with no migration:
// an old hash still verifies under its own recorded parameters, and
// [Service.Login] rehashes it on the next successful sign-in.
type Params struct {
	// MemoryKiB is argon2's m: the size of the memory block, in kibibytes.
	// This is the dominant security parameter — it is what makes a GPU or an
	// ASIC a bad deal for the attacker — and the dominant operational one,
	// because it is allocated per concurrent hash.
	MemoryKiB uint32
	// Time is argon2's t: passes over the memory block.
	Time uint32
	// Parallelism is argon2's p: independent lanes.
	Parallelism uint8
	// SaltLen is the length of the random salt in bytes.
	SaltLen uint32
	// KeyLen is the length of the derived key in bytes.
	KeyLen uint32
}

// DefaultParams is the current policy, chosen against the REAL deploy target
// rather than against a benchmark on the author's laptop.
//
// # The target
//
// Oracle Cloud Always-Free, Ampere A1: 2 OCPU, 12 GB. An Ampere OCPU is one
// physical Neoverse N1 core with no SMT, so the box has exactly two hardware
// threads, and it is also running Postgres, Redis, Kafka, six Go services and
// Next.js. Every parameter below is bounded by that, not by what an M-series
// Mac can do.
//
// # p = 2
//
// argon2's lanes only buy speed if there are cores to run them on. p=4 — the
// value RFC 9106 and OWASP both print — on a two-core box costs the
// synchronisation and delivers half the parallelism, so the hash gets SLOWER
// per unit of memory-hardness. p is set to the core count. Note that p is part
// of the hash: changing it later invalidates nothing, because the old value is
// recorded in each stored PHC string and [NeedsRehash] schedules the upgrade.
//
// # m = 64 MiB
//
// This is RFC 9106's "second recommended option" (m=64MiB, t=3, p=4) and
// OWASP's, with p adjusted as above. It is the parameter the whole scheme rests
// on, and it is also the one with an operational ceiling: memory is allocated
// per CONCURRENT hash, so the process's worst case is
// MemoryKiB × [Hasher] concurrency. With the default concurrency (GOMAXPROCS,
// = 2 on the target) that is 128 MiB — about 1% of the box, which is the point
// at which a login flood cannot evict Postgres's shared buffers.
//
// Going up to RFC 9106's first option (m=2 GiB) is not available here: two
// concurrent logins would ask for a third of the machine's RAM.
//
// # t = 3
//
// The published pairing with m=64MiB. m and t multiply into the attacker's
// cost, and raising t is the cheap way to add cost when m is capped by RAM — so
// t is the knob to turn first if the measurement below ever says there is
// headroom.
//
// # The measurement
//
// BenchmarkHashCostOnDeployTarget in password_bench_test.go measures one hash
// under these parameters, and the recorded figures are in that file's comment.
// It is a benchmark rather than a comment precisely so the number is
// re-derivable on the real hardware instead of trusted from a doc block: a
// parameter set that takes two seconds on two cores is a denial of service on
// our own login endpoint, and the only way to know is to run it there.
var DefaultParams = Params{
	MemoryKiB:   64 * 1024, // 64 MiB
	Time:        3,
	Parallelism: 2,
	SaltLen:     16, // RFC 9106 §4: 128 bits.
	KeyLen:      32, // 256 bits, matching the SHA-256 width used elsewhere here.
}

// Parameter bounds. These are sanity limits on what may be CONSTRUCTED or
// PARSED, not policy: policy is DefaultParams. The upper bound on MemoryKiB is
// the one that matters — a stored hash claiming m=64 GiB would otherwise make
// verifying it an out-of-memory kill, so a corrupt or hostile row is rejected
// at parse time rather than allocated.
const (
	minMemoryKiB uint32 = 8 * 1024    // 8 MiB — below this argon2id is not worth running.
	maxMemoryKiB uint32 = 1024 * 1024 // 1 GiB — above this, one hash owns the box.
	minTime      uint32 = 1
	maxTime      uint32 = 16
	minSaltLen   uint32 = 8
	maxSaltLen   uint32 = 64
	minKeyLen    uint32 = 16
	maxKeyLen    uint32 = 64
)

// Validate reports whether the parameters are inside the permitted ranges.
func (p Params) Validate() error {
	switch {
	case p.MemoryKiB < minMemoryKiB || p.MemoryKiB > maxMemoryKiB:
		return fmt.Errorf("%w: m=%d KiB outside [%d, %d]", ErrParamsInvalid, p.MemoryKiB, minMemoryKiB, maxMemoryKiB)
	case p.Time < minTime || p.Time > maxTime:
		return fmt.Errorf("%w: t=%d outside [%d, %d]", ErrParamsInvalid, p.Time, minTime, maxTime)
	case p.Parallelism == 0:
		return fmt.Errorf("%w: p=0", ErrParamsInvalid)
	case p.SaltLen < minSaltLen || p.SaltLen > maxSaltLen:
		return fmt.Errorf("%w: salt length %d outside [%d, %d]", ErrParamsInvalid, p.SaltLen, minSaltLen, maxSaltLen)
	case p.KeyLen < minKeyLen || p.KeyLen > maxKeyLen:
		return fmt.Errorf("%w: key length %d outside [%d, %d]", ErrParamsInvalid, p.KeyLen, minKeyLen, maxKeyLen)
	}
	// argon2 requires m >= 8*p. With the minimum m above this cannot fail for
	// any p that fits in a uint8, but the check is here so a future widening of
	// minMemoryKiB downward cannot silently produce a panic inside argon2.
	if uint32(p.Parallelism) > p.MemoryKiB/8 {
		return fmt.Errorf("%w: m=%d KiB is less than 8*p for p=%d", ErrParamsInvalid, p.MemoryKiB, p.Parallelism)
	}
	return nil
}

// AtLeastAsStrongAs reports whether p is no weaker than policy on every axis
// that matters. It is the predicate [NeedsRehash] is the negation of.
//
// Parallelism is compared for EQUALITY rather than for ">=", and that is
// deliberate: a higher p is not stronger, it is different — it trades
// sequential depth for width and on a two-core box it is strictly worse. So a
// hash written under p=4 when policy says p=2 is due for a rehash in the same
// way one written under m=16MiB is.
func (p Params) AtLeastAsStrongAs(policy Params) bool {
	return p.MemoryKiB >= policy.MemoryKiB &&
		p.Time >= policy.Time &&
		p.Parallelism == policy.Parallelism &&
		p.SaltLen >= policy.SaltLen &&
		p.KeyLen >= policy.KeyLen
}

// phcPrefix is the algorithm identifier. users_password_hash_is_argon2id
// CHECKs that every stored hash starts with it, which is the schema refusing to
// hold a bcrypt hash, an argon2i hash, or a plaintext password.
const phcPrefix = "$argon2id$"

// b64 is PHC's encoding: standard alphabet, no padding.
var b64 = base64.RawStdEncoding

// Encode renders the PHC prefix for these parameters:
//
//	$argon2id$v=19$m=65536,t=3,p=2$
//
// Exported for tests and for an operator who needs to recognise a hash's cost
// without decoding it.
func (p Params) Encode() string {
	return fmt.Sprintf("%sv=%d$m=%d,t=%d,p=%d$", phcPrefix, argon2.Version, p.MemoryKiB, p.Time, p.Parallelism)
}

// String implements fmt.Stringer with the compact cost triple.
func (p Params) String() string {
	return fmt.Sprintf("m=%d,t=%d,p=%d", p.MemoryKiB, p.Time, p.Parallelism)
}

// Hasher derives and verifies argon2id password hashes.
//
// # The concurrency limiter is a security control, not a tuning knob
//
// argon2id allocates MemoryKiB per hash in progress. Without a bound, N
// simultaneous login requests allocate N × 64 MiB, so ~190 concurrent requests
// exhaust a 12 GB box — and an unauthenticated attacker chooses N. That turns
// the memory-hardness that protects stolen hashes into a remote OOM primitive
// against the live service.
//
// The limiter caps the process's hashing footprint at Concurrency × MemoryKiB,
// which is a number an operator can put in a container memory limit. Requests
// beyond it queue on the semaphore and are released in order, or fail with the
// caller's context error if the caller gave up first — so backpressure surfaces
// as a timeout on a bounded queue rather than as an OOM kill.
//
// Rate limiting per IP and per user (CLAUDE.md §6) is the OTHER half and lives
// in internal/httpapi; this bound is what keeps the box alive when that half is
// misconfigured.
type Hasher struct {
	params Params

	// sem is the concurrency limiter. Buffered to Concurrency; a send acquires
	// and a receive releases.
	sem chan struct{}

	// decoy is a PHC hash of a random value, minted at construction under
	// params. Verifying against it is how the unknown-email path spends the
	// same argon2id time as the wrong-password path. See [Hasher.VerifyDecoy].
	decoy string

	// rand is the entropy source for salts, seamed for tests. It is NOT a
	// knob: nothing but a test may pass anything other than crypto/rand.
	rand func([]byte) error
}

// HasherOptions configures [NewHasher]. The zero value is valid and yields the
// documented defaults.
type HasherOptions struct {
	// Params is the cost policy. Zero value means [DefaultParams].
	Params Params

	// Concurrency bounds simultaneous hashes. Zero means GOMAXPROCS, which on
	// the deploy target is 2 and gives a 128 MiB ceiling. Negative is an error.
	//
	// Raising it above the core count does not raise throughput — argon2id is
	// CPU- and memory-bound, not I/O-bound — it only raises the memory
	// ceiling. Raise it only alongside the container memory limit.
	Concurrency int

	// randRead is the salt source. Unexported so that only this package's tests
	// can substitute it; production always uses crypto/rand.Read.
	randRead func([]byte) error
}

// NewHasher builds a Hasher and mints its decoy hash.
//
// Minting the decoy costs one argon2id hash at startup — 60-odd milliseconds on
// the deploy target — which is paid once so that every unknown-email login
// afterwards costs the same as a real one.
func NewHasher(opts HasherOptions) (*Hasher, error) {
	params := opts.Params
	if params == (Params{}) {
		params = DefaultParams
	}
	if err := params.Validate(); err != nil {
		return nil, err
	}

	concurrency := opts.Concurrency
	switch {
	case concurrency < 0:
		return nil, fmt.Errorf("%w: hasher concurrency %d is negative", ErrInvalid, concurrency)
	case concurrency == 0:
		concurrency = runtime.GOMAXPROCS(0)
	}

	randRead := opts.randRead
	if randRead == nil {
		randRead = func(b []byte) error {
			_, err := rand.Read(b)
			return err
		}
	}

	h := &Hasher{
		params: params,
		sem:    make(chan struct{}, concurrency),
		rand:   randRead,
	}

	// The decoy's plaintext is 32 random bytes and is discarded immediately: it
	// exists only so the hash is well-formed and its verification does real
	// work. Nothing can present it, because nothing ever knows it.
	seed := make([]byte, 32)
	if err := h.rand(seed); err != nil {
		return nil, fmt.Errorf("%w: seeding the timing decoy: %w", ErrInternal, err)
	}
	decoy, err := h.hash(string(seed))
	if err != nil {
		return nil, fmt.Errorf("minting the timing decoy: %w", err)
	}
	h.decoy = decoy

	return h, nil
}

// Params returns the hasher's current cost policy.
func (h *Hasher) Params() Params { return h.params }

// Hash derives a PHC-format argon2id hash of password.
//
// The password arrives as a [Secret] so that no call site can log it on the way
// in, and the returned string is a plain string because it is a hash — it is
// meant to be stored, and users_password_hash_is_argon2id will reject anything
// that is not one.
//
// ctx bounds the wait for the concurrency limiter. It does NOT interrupt the
// argon2 computation itself: argon2.IDKey is a tight non-preemptible loop with
// no cancellation seam, and pretending otherwise would return a context error
// while a core stayed busy for the remaining tens of milliseconds. The bound
// that matters is on the queue, and that is honoured.
func (h *Hasher) Hash(ctx context.Context, password Secret) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()

	return h.hash(password.Expose())
}

// hash is Hash without validation or the limiter, for the decoy path (which
// runs before the limiter exists) and for internal reuse.
func (h *Hasher) hash(password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	if err := h.rand(salt); err != nil {
		return "", fmt.Errorf("%w: reading salt entropy: %w", ErrInternal, err)
	}

	key := argon2.IDKey(
		[]byte(password),
		salt,
		h.params.Time,
		h.params.MemoryKiB,
		h.params.Parallelism,
		h.params.KeyLen,
	)

	var sb strings.Builder
	sb.WriteString(h.params.Encode())
	sb.WriteString(b64.EncodeToString(salt))
	sb.WriteByte('$')
	sb.WriteString(b64.EncodeToString(key))
	return sb.String(), nil
}

// Verify checks password against a stored PHC hash in constant time with
// respect to the digest.
//
// It returns (false, nil) for a wrong password and (false, err) only when the
// STORED value could not be used — a hash that does not parse is corruption on
// our side, so it wraps [ErrInternal] and must not be reported to the user as a
// bad password.
//
// The comparison is subtle.ConstantTimeCompare over the derived key. The
// parameters and salt are read from the stored string, so a hash written under
// an older policy verifies correctly; [NeedsRehash] is how it gets upgraded.
func (h *Hasher) Verify(ctx context.Context, encoded string, password Secret) (bool, error) {
	params, salt, want, err := DecodeHash(encoded)
	if err != nil {
		return false, err
	}
	// Length is bounded before any work is done, so an oversized body cannot
	// buy CPU time. A too-SHORT password is not rejected here: a stored hash
	// may predate a raised minimum, and refusing to check it would lock out a
	// user whose password is fine.
	if password.Len() > MaxPasswordLen {
		return false, ErrPasswordTooLong
	}

	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()

	got := argon2.IDKey(
		[]byte(password.Expose()),
		salt,
		params.Time,
		params.MemoryKiB,
		params.Parallelism,
		params.KeyLen,
	)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// VerifyDecoy performs the same argon2id work as [Hasher.Verify] against a hash
// nobody knows the password to, and always reports failure.
//
// It exists for exactly one call site: [Service.Login] when no user matches the
// address. Without it, "unknown email" returns in microseconds and "wrong
// password" returns in tens of milliseconds, and that difference is a reliable
// remote oracle for which addresses are registered — measurable over the
// network with a few hundred samples, because argon2id's cost is three orders
// of magnitude above the noise floor of everything else in the request.
//
// The decoy shares the hasher's semaphore, so the two paths also queue
// identically under load. That matters: a limiter applied to one path and not
// the other would reintroduce the distinguisher at exactly the moment an
// attacker is generating enough traffic to measure it.
func (h *Hasher) VerifyDecoy(ctx context.Context, password Secret) error {
	// The result is discarded on purpose. It is false with overwhelming
	// probability and the caller must not branch on it either way.
	if _, err := h.Verify(ctx, h.decoy, password); err != nil {
		return err
	}
	return nil
}

// NeedsRehash reports whether a stored hash was written under parameters weaker
// than, or differently shaped from, the hasher's current policy.
//
// The upgrade path this enables is the reason parameters live in the PHC string
// (migrations/00005: "a parameter bump a pure application change ... No
// migration"): [Service.Login] has the plaintext in hand exactly once, at the
// moment it verifies successfully, and that is the only moment a stronger hash
// can be derived without asking the user to type their password again.
func (h *Hasher) NeedsRehash(encoded string) (bool, error) {
	params, _, _, err := DecodeHash(encoded)
	if err != nil {
		return false, err
	}
	return !params.AtLeastAsStrongAs(h.params), nil
}

// acquire takes a slot on the concurrency limiter, or returns the caller's
// context error.
func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: waiting for a password-hashing slot: %w", ErrInternal, ctx.Err())
	}
}

func (h *Hasher) release() { <-h.sem }

// DecodeHash parses a PHC-format argon2id string into its parameters, salt and
// digest.
//
// It is strict on every field. A stored hash is trusted input in the sense that
// we wrote it, and untrusted in the sense that "we wrote it" is exactly what an
// attacker with an INSERT would like us to assume — so the memory parameter is
// range-checked before anything allocates, the algorithm must be argon2id, and
// the version must be the one this build of argon2 implements.
func DecodeHash(encoded string) (params Params, salt, digest []byte, err error) {
	// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<digest>
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, digest]
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return Params{}, nil, nil, ErrHashFormat
	}
	if parts[1] != "argon2id" {
		return Params{}, nil, nil, ErrHashAlgorithm
	}

	var version int
	if _, scanErr := fmt.Sscanf(parts[2], "v=%d", &version); scanErr != nil {
		return Params{}, nil, nil, ErrHashFormat
	}
	if version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("%w: v=%d, this build implements v=%d",
			ErrHashVersion, version, argon2.Version)
	}

	memory, time, parallelism, err := decodeCostTriple(parts[3])
	if err != nil {
		return Params{}, nil, nil, err
	}

	salt, decErr := b64.DecodeString(parts[4])
	if decErr != nil {
		return Params{}, nil, nil, ErrHashFormat
	}
	digest, decErr = b64.DecodeString(parts[5])
	if decErr != nil {
		return Params{}, nil, nil, ErrHashFormat
	}

	params = Params{
		MemoryKiB:   memory,
		Time:        time,
		Parallelism: parallelism,
		SaltLen:     uint32(len(salt)),
		KeyLen:      uint32(len(digest)),
	}
	if err := params.Validate(); err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: %w", ErrHashParams, err)
	}
	return params, salt, digest, nil
}

// decodeCostTriple parses "m=65536,t=3,p=2".
//
// fmt.Sscanf is not used for this field. It would accept "m=65536,t=3,p=2junk"
// and, worse, it happily parses a negative into an unsigned via %d on an int
// and then the conversion wraps — so a stored "m=-1" would become 4294967295
// KiB and the range check would be the only thing between that and a 4 TiB
// allocation. strconv.ParseUint refuses the sign outright.
func decodeCostTriple(s string) (memory, time uint32, parallelism uint8, err error) {
	fields := strings.Split(s, ",")
	if len(fields) != 3 {
		return 0, 0, 0, ErrHashFormat
	}
	var seenM, seenT, seenP bool
	for _, f := range fields {
		name, value, ok := strings.Cut(f, "=")
		if !ok {
			return 0, 0, 0, ErrHashFormat
		}
		switch name {
		case "m":
			v, perr := strconv.ParseUint(value, 10, 32)
			if perr != nil {
				return 0, 0, 0, ErrHashFormat
			}
			memory, seenM = uint32(v), true
		case "t":
			v, perr := strconv.ParseUint(value, 10, 32)
			if perr != nil {
				return 0, 0, 0, ErrHashFormat
			}
			time, seenT = uint32(v), true
		case "p":
			v, perr := strconv.ParseUint(value, 10, 8)
			if perr != nil {
				return 0, 0, 0, ErrHashFormat
			}
			parallelism, seenP = uint8(v), true
		default:
			return 0, 0, 0, ErrHashFormat
		}
	}
	if !seenM || !seenT || !seenP {
		return 0, 0, 0, ErrHashFormat
	}
	return memory, time, parallelism, nil
}

// ValidatePassword checks a candidate password against the length and encoding
// bounds. It is exported so internal/httpapi can reject a bad body before it
// costs a hash, and it is applied again inside [Hasher.Hash] so that skipping
// it is not possible.
//
// It applies NO composition rule — no required digit, no required symbol, no
// required case mix — per NIST SP 800-63B, which found those rules push users
// toward predictable substitutions and reduce real entropy. Length and a
// breached-password check are the controls that work; the second one needs a
// corpus this project does not ship, and is noted rather than faked.
func ValidatePassword(password Secret) error {
	switch n := password.Len(); {
	case n < MinPasswordLen:
		return ErrPasswordTooShort
	case n > MaxPasswordLen:
		return ErrPasswordTooLong
	}
	// UTF-8 validity is required because the password is compared byte-for-byte
	// after hashing: a client that sends invalid UTF-8 once and a corrected
	// form later would silently have two different passwords. Rejecting at
	// registration is the only point where that is fixable.
	if !utf8.ValidString(password.Expose()) {
		return ErrPasswordNotUTF8
	}
	return nil
}
