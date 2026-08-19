package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238's default and the only algorithm every authenticator app implements; see TOTPConfig.
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// TOTP defaults, from RFC 6238 and from what authenticator apps actually
// implement.
const (
	// TOTPPeriod is the time step. RFC 6238 §5.2 recommends 30 seconds and
	// every mainstream authenticator hardcodes it.
	TOTPPeriod = 30 * time.Second

	// TOTPDigits is the code length. Six, for the same reason.
	TOTPDigits = 6

	// TOTPSkewSteps is how many steps either side of the current one are
	// accepted.
	//
	// One step means the accepted window is (-30s, +60s) relative to the
	// server: the previous code, the current code, and the next one. That
	// tolerates a phone whose clock is up to 30 seconds out, which is the
	// common real failure, without opening a 5-minute window the way a skew of
	// 5 would.
	//
	// The window is a real widening of the guess space — 3 codes instead of 1,
	// so a blind guess succeeds with probability 3/10^6 instead of 1/10^6 —
	// which is why rate limiting on the verification endpoint is not optional
	// and is stated as a requirement in [ValidateTOTPCode]'s comment.
	TOTPSkewSteps = 1

	// TOTPSecretBytes is the shared secret width. RFC 4226 §4 requires at least
	// 128 bits and recommends 160, which is also SHA-1's output width and what
	// every authenticator app expects to receive.
	TOTPSecretBytes = 20
)

// b32 is the encoding authenticator apps read: RFC 4648 base32, uppercase,
// without padding. Padding is stripped because the `secret` parameter of an
// otpauth:// URI conventionally carries none and some apps choke on the '='.
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// TOTPConfig is the parameter set for one deployment's TOTP.
//
// # Why SHA-1, in 2026
//
// RFC 6238 defines HMAC-SHA1, HMAC-SHA256 and HMAC-SHA512 variants and every
// serious analysis says SHA-256 is the better primitive. It is not used here,
// and the reason is interoperability rather than preference: the otpauth:// URI
// has an `algorithm` parameter, and a large fraction of authenticator apps —
// including ones many users have — ignore it and compute SHA-1 regardless. An
// enrolment that silently disagrees with the user's app produces "my codes
// never work", which is indistinguishable from a broken second factor and
// drives people to turn 2FA off.
//
// The security cost is close to zero. SHA-1's break is collision resistance;
// HMAC-SHA1 depends on preimage and PRF properties, which are unbroken, and
// there is no published attack on HMAC-SHA1 that is relevant at a 160-bit
// secret and a 30-second window. This is the same call RFC 6238's own reference
// implementation and every mainstream product makes.
//
// The field exists so that a future deployment can move, and so that this
// decision is visible in code rather than assumed.
type TOTPConfig struct {
	// Period is the time step. Zero means [TOTPPeriod].
	Period time.Duration
	// Digits is the code length. Zero means [TOTPDigits].
	Digits int
	// SkewSteps is the accepted window either side of now. Negative is an
	// error; zero means [TOTPSkewSteps]. To accept ONLY the current step, set
	// it explicitly with [TOTPConfig.WithExactSkew].
	SkewSteps int

	// exactSkew records that SkewSteps=0 was meant literally rather than as
	// "unset". Without it the zero value could not express a zero window.
	exactSkew bool
}

// WithExactSkew returns a copy of c that accepts only the current step.
func (c TOTPConfig) WithExactSkew() TOTPConfig {
	c.SkewSteps = 0
	c.exactSkew = true
	return c
}

// normalise fills in defaults and validates.
func (c TOTPConfig) normalise() (TOTPConfig, error) {
	if c.Period == 0 {
		c.Period = TOTPPeriod
	}
	if c.Period <= 0 {
		return c, fmt.Errorf("%w: TOTP period %s is not positive", ErrInvalid, c.Period)
	}
	if c.Digits == 0 {
		c.Digits = TOTPDigits
	}
	if c.Digits < 6 || c.Digits > 8 {
		// RFC 4226 §5.3 permits 6 to 8. Below 6 the guess space is small enough
		// that rate limiting alone is doing all the work.
		return c, fmt.Errorf("%w: TOTP digits %d outside [6, 8]", ErrInvalid, c.Digits)
	}
	if c.SkewSteps < 0 {
		return c, fmt.Errorf("%w: TOTP skew %d is negative", ErrInvalid, c.SkewSteps)
	}
	if c.SkewSteps == 0 && !c.exactSkew {
		c.SkewSteps = TOTPSkewSteps
	}
	if c.SkewSteps > 10 {
		return c, fmt.Errorf("%w: TOTP skew %d steps is a %s window", ErrInvalid,
			c.SkewSteps, time.Duration(2*c.SkewSteps+1)*c.Period)
	}
	return c, nil
}

// NewTOTPSecret mints a shared secret: [TOTPSecretBytes] from crypto/rand.
//
// The secret is returned as raw bytes rather than a [Secret] because its
// immediate destinations are an AEAD seal and a base32 encoding, both of which
// want bytes. It is the caller's job — [Service] — to get it into ciphertext
// and out of memory promptly, and never to log it. Everything that CARRIES the
// secret past this package's boundary (the provisioning URI) is a [Secret].
func NewTOTPSecret() ([]byte, error) {
	buf := make([]byte, TOTPSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("%w: reading TOTP secret entropy: %w", ErrInternal, err)
	}
	return buf, nil
}

// TOTPCodeAt computes the code for a given instant, per RFC 6238.
//
// Exported because an enrolment flow has to prove to itself that it and the
// user's app agree, and because a test that cannot compute an expected code
// cannot test anything.
func TOTPCodeAt(secret []byte, at time.Time, cfg TOTPConfig) (string, error) {
	cfg, err := cfg.normalise()
	if err != nil {
		return "", err
	}
	if len(secret) == 0 {
		return "", fmt.Errorf("%w: TOTP secret is empty", ErrInvalid)
	}
	return totpCodeForStep(secret, totpStep(at, cfg.Period), cfg.Digits), nil
}

// totpStep is RFC 6238's T: the number of periods since the Unix epoch.
//
// Unix() rather than UnixNano() and an integer division: negative instants (a
// clock set before 1970) floor toward zero in Go, which would make step 0 twice
// as long. That cannot happen on a machine with a sane clock, and the code
// below does not depend on it — it is noted so that nobody "fixes" the division
// into something that changes the mapping for positive times.
func totpStep(at time.Time, period time.Duration) int64 {
	return at.Unix() / int64(period/time.Second)
}

// totpCodeForStep is HOTP (RFC 4226 §5.3) over the step counter.
func totpCodeForStep(secret []byte, step int64, digits int) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))

	mac := hmac.New(sha1.New, secret)
	mac.Write(counter[:])
	sum := mac.Sum(nil)

	// Dynamic truncation, RFC 4226 §5.3.
	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])

	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	// Zero-padded to the full width: "000123" and "123" are different strings
	// and only the first is a valid code.
	return fmt.Sprintf("%0*d", digits, value%mod)
}

// ValidateTOTPCode checks a presented code against the secret over the accepted
// window and returns the step it matched.
//
// # The returned step is the whole point
//
// A code is valid for its entire 30-second step, so without further state the
// same code works twice. This phase's brief calls that out: "a code valid for
// 30 seconds that can be used twice within its window is a weaker control than
// it looks" — an attacker who observes one code over the user's shoulder, or
// captures one from a phished form, has up to 30 seconds to use it against a
// session the user did not intend.
//
// So this function does not answer "is this code valid"; it answers "which step
// did this code come from", and the caller must then burn that step through a
// [ReplayGuard]. [Service] does exactly that, in that order, and never accepts
// a code it did not first consume.
//
// # Constant time
//
// Every step in the window is compared with subtle.ConstantTimeCompare and the
// loop does NOT break on a match, so the work is identical for a valid code, an
// invalid code, and a code from the far edge of the window. Selecting the
// matched step uses subtle.ConstantTimeSelect for the same reason.
//
// # Rate limiting is required, not optional
//
// The guess space is 10^digits and the window admits 2*skew+1 of them, so with
// the defaults a blind guess succeeds with probability 3 in 10^6. That is
// negligible for one attempt and not negligible for a million, which is
// seconds of traffic. internal/httpapi must rate-limit second-factor
// verification per user (CLAUDE.md §6: "rate limiting per user and per IP") or
// this control is worth about 19 bits.
func ValidateTOTPCode(secret []byte, code string, at time.Time, cfg TOTPConfig) (step int64, ok bool, err error) {
	cfg, err = cfg.normalise()
	if err != nil {
		return 0, false, err
	}
	if len(secret) == 0 {
		return 0, false, fmt.Errorf("%w: TOTP secret is empty", ErrInvalid)
	}

	presented := normaliseCode(code)
	if len(presented) != cfg.Digits {
		// A length mismatch cannot match any code, and comparing anyway would
		// still be a constant-time no-op. Returning early is safe: the length
		// of the presented code is chosen by the presenter and is already known
		// to them.
		return 0, false, nil
	}

	current := totpStep(at, cfg.Period)
	var matched int
	var matchedStep int64

	for delta := -cfg.SkewSteps; delta <= cfg.SkewSteps; delta++ {
		candidateStep := current + int64(delta)
		candidate := totpCodeForStep(secret, candidateStep, cfg.Digits)
		eq := subtle.ConstantTimeCompare([]byte(candidate), []byte(presented))
		matchedStep = int64(subtle.ConstantTimeSelect(eq, int(candidateStep), int(matchedStep)))
		matched |= eq
	}

	return matchedStep, matched == 1, nil
}

// normaliseCode strips the separators and whitespace a user may paste in.
// Authenticator apps display "123 456"; a form that rejects the space the user
// copied is a support ticket.
func normaliseCode(code string) string {
	var sb strings.Builder
	sb.Grow(len(code))
	for _, r := range code {
		if r >= '0' && r <= '9' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// ProvisioningURI renders the otpauth:// URI an authenticator app scans.
//
// It is a [Secret] and that is not decoration: the URI CONTAINS the shared
// secret in base32. A URI logged once is a permanent 2FA bypass for that user,
// and a URI in a browser history or a screenshot is the same. It is produced at
// exactly one moment — the enrolment response, before confirmation — and it
// must never be re-derivable afterwards, which is why [Service] does not offer
// a "show me my QR code again" path: the answer is to re-enrol.
//
// The `algorithm`, `digits` and `period` parameters are emitted even though
// they are the defaults, because an app that DOES read them should not have to
// guess, and one that ignores them computes the defaults anyway.
func ProvisioningURI(issuer, account string, secret []byte, cfg TOTPConfig) (Secret, error) {
	cfg, err := cfg.normalise()
	if err != nil {
		return Secret{}, err
	}
	if issuer == "" {
		return Secret{}, fmt.Errorf("%w: provisioning URI needs an issuer", ErrInvalid)
	}
	if account == "" {
		return Secret{}, fmt.Errorf("%w: provisioning URI needs an account name", ErrInvalid)
	}
	if len(secret) == 0 {
		return Secret{}, fmt.Errorf("%w: provisioning URI needs a secret", ErrInvalid)
	}
	// A ':' in either component would break the label's own "issuer:account"
	// encoding, which is a spec-level ambiguity rather than a cosmetic one.
	if strings.ContainsRune(issuer, ':') || strings.ContainsRune(account, ':') {
		return Secret{}, fmt.Errorf("%w: provisioning URI label parts must not contain ':'", ErrInvalid)
	}

	q := url.Values{}
	q.Set("secret", b32.EncodeToString(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(cfg.Digits))
	q.Set("period", strconv.Itoa(int(cfg.Period/time.Second)))

	u := url.URL{
		Scheme:   "otpauth",
		Host:     "totp",
		Path:     "/" + issuer + ":" + account,
		RawQuery: q.Encode(),
	}
	return NewSecret(u.String()), nil
}

// ReplayGuard burns a (user, step) pair so a TOTP code cannot be used twice
// inside its own validity window.
//
// # Why this is an interface and not a table
//
// The obvious implementation is a `last_used_step` column on user_totp.
// migrations/00005 does not have one, and that is deliberate rather than an
// oversight: the table is credential material that phase 10 may put behind its
// own database role, and a column written on every successful login turns a
// read-mostly credential table into a write-hot one.
//
// CLAUDE.md §3 assigns exactly this to Redis — "current-line snapshot cache,
// WebSocket presence, distributed rate limiting, IDEMPOTENCY KEYS. Never the
// source of truth" — and a burnt TOTP step is an idempotency key in the precise
// sense: it makes a repeated presentation of the same code a no-op. It is also
// correctly NOT the source of truth, because losing the whole store degrades
// the control to "a code is valid for its 30 seconds" rather than breaking
// authentication.
//
// The Redis implementation is a `SET key 1 NX PX <ttl>` and is one method. It
// is not in this package because adding a Redis client is a go.mod change this
// phase's auth work does not own; the handoff notes name it. [MemoryReplayGuard]
// is what ships today and is correct for a single replica.
//
// Implementations must be atomic: Consume reports true only for the caller that
// won the race, and false for every other, or the guard does not guard.
type ReplayGuard interface {
	// Consume marks (user, step) as used and reports whether this caller was
	// the first to do so. expiry is when the entry may be discarded — one step
	// past the far edge of the accepted window, so an entry outlives every
	// presentation that could match it.
	Consume(ctx context.Context, user domain.UserID, step int64, expiry time.Time) (bool, error)
}

// MemoryReplayGuard is an in-process [ReplayGuard].
//
// It is CORRECT for a single replica and INSUFFICIENT for more than one: two
// api pods each hold their own map, so a code burnt on pod A is still fresh on
// pod B. CLAUDE.md §9 requires `stream` to be horizontally scalable and says
// subscription state therefore lives in Redis rather than in a pod; the same
// argument applies to this map the moment `api` has two replicas.
//
// It is shipped anyway because a guard that exists is better than a TODO, the
// compose stack runs one api container, and the interface makes the swap a
// constructor argument.
type MemoryReplayGuard struct {
	mu   sync.Mutex
	used map[replayKey]time.Time

	// now is the clock seam, used only to expire entries.
	now func() time.Time
}

type replayKey struct {
	user domain.UserID
	step int64
}

// NewMemoryReplayGuard builds an in-process guard.
//
// now is the clock, and it is a PARAMETER rather than a hardcoded time.Now
// because the guard and its caller must share one. The guard receives an
// ABSOLUTE expiry from [ReplayGuard.Consume] and compares it against its own
// clock to decide whether an entry is still live; if [Service] computes that
// expiry from one clock and the guard evaluates it against another, the two
// disagree and the disagreement is silent. Specifically, a guard on the wall
// clock holding an expiry computed from a clock set in the past sweeps the
// entry immediately and every code becomes replayable — which is the control
// failing open, invisibly.
//
// Pass nil for time.Now.
func NewMemoryReplayGuard(now func() time.Time) *MemoryReplayGuard {
	if now == nil {
		now = time.Now
	}
	return &MemoryReplayGuard{
		used: make(map[replayKey]time.Time),
		now:  now,
	}
}

// Consume implements [ReplayGuard].
//
// Expired entries are swept opportunistically on each call rather than by a
// background goroutine. The map's size is bounded by (active users) × (window
// steps) over one window — a few entries per user per 90 seconds — so the sweep
// is cheap and a goroutine with its own lifecycle would be more machinery than
// the problem deserves.
func (g *MemoryReplayGuard) Consume(ctx context.Context, user domain.UserID, step int64, expiry time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("%w: consuming a TOTP step: %w", ErrInternal, err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	for k, exp := range g.used {
		if !exp.After(now) {
			delete(g.used, k)
		}
	}

	key := replayKey{user: user, step: step}
	if exp, seen := g.used[key]; seen && exp.After(now) {
		return false, nil
	}
	g.used[key] = expiry
	return true, nil
}

// Recovery codes.
//
// CLAUDE.md §6 asks for TOTP; this phase's brief adds "Recovery codes:
// single-use, hashed at rest." They are the answer to the failure mode that
// makes users refuse 2FA — a lost or wiped phone — and without them the only
// recovery is an operator manually disabling the second factor, which is a
// social-engineering target far weaker than the control it restores.
const (
	// RecoveryCodeCount is how many are minted per enrolment. Ten is the
	// conventional number and is enough that a user who has used a few is not
	// forced through a re-mint at the worst moment.
	RecoveryCodeCount = 10

	// RecoveryCodeBytes is the entropy per code: 15 bytes = 120 bits, which
	// encodes to exactly 24 base32 characters with no padding.
	//
	// 120 bits is chosen so the SAME argument that justifies SHA-256 for
	// refresh tokens holds here: the value is high-entropy random bytes we
	// generated, so there is no dictionary and no reason to pay argon2id. At 80
	// bits that argument would be uncomfortable against an offline attacker
	// with a stolen database; at 120 it is not.
	RecoveryCodeBytes = 15

	// recoveryGroupLen is the display grouping. 24 characters in groups of four
	// is six groups, which is what a human transcribing from paper can hold.
	recoveryGroupLen = 4
)

// NewRecoveryCodes mints a fresh set.
//
// Each code is returned once, in a [Secret], and its SHA-256 digest is what the
// caller stores. There is deliberately no way to recover a code from its digest
// and no "show them to me again" path — a user who did not save them re-mints,
// which invalidates the old set.
func NewRecoveryCodes(n int) (codes []Secret, digests [][]byte, err error) {
	if n <= 0 || n > 64 {
		return nil, nil, fmt.Errorf("%w: recovery code count %d outside [1, 64]", ErrInvalid, n)
	}

	codes = make([]Secret, 0, n)
	digests = make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		buf := make([]byte, RecoveryCodeBytes)
		if _, err := rand.Read(buf); err != nil {
			return nil, nil, fmt.Errorf("%w: reading recovery code entropy: %w", ErrInternal, err)
		}
		raw := b32.EncodeToString(buf)
		codes = append(codes, NewSecret(groupString(raw, recoveryGroupLen, "-")))
		digests = append(digests, HashRecoveryCode(NewSecret(raw)))
	}
	return codes, digests, nil
}

// HashRecoveryCode computes the value stored for a recovery code.
//
// It normalises first, so the digest of what the user types equals the digest
// of what we minted regardless of case, spacing or the dashes we added for
// legibility. Storing an un-normalised digest would make "paste it with the
// dashes" and "type it without" two different credentials.
func HashRecoveryCode(code Secret) []byte {
	sum := sha256.Sum256([]byte(normaliseRecoveryCode(code.Expose())))
	return sum[:]
}

// MatchRecoveryCode reports which stored digest a presented code matches, or -1.
//
// Every digest is compared and the loop does not break, so the time taken does
// not reveal how many codes the user has left or which one matched.
func MatchRecoveryCode(digests [][]byte, presented Secret) int {
	want := HashRecoveryCode(presented)
	found := -1
	for i, d := range digests {
		eq := subtle.ConstantTimeCompare(d, want)
		// Keep the FIRST match. ConstantTimeSelect with a guard on found == -1
		// would branch; instead select unconditionally and rely on digests
		// being distinct, which they are with overwhelming probability at 120
		// bits.
		found = subtle.ConstantTimeSelect(eq, i, found)
	}
	return found
}

// normaliseRecoveryCode upper-cases, drops separators, and repairs the three
// unambiguous transcription errors.
//
// RFC 4648's base32 alphabet is A-Z and 2-7, so '0', '1' and '8' cannot appear
// in a real code and their intended characters are unambiguous: a handwritten
// 'O' read as '0', 'I' as '1', 'B' as '8'. Repairing them removes the most
// common reason a correctly-copied code is rejected. '9' is left alone —
// nothing in the alphabet is reliably confusable with it.
func normaliseRecoveryCode(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range strings.ToUpper(s) {
		switch r {
		case '-', ' ', '\t':
			continue
		case '0':
			sb.WriteRune('O')
		case '1':
			sb.WriteRune('I')
		case '8':
			sb.WriteRune('B')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// groupString inserts sep every n runes.
func groupString(s string, n int, sep string) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	var sb strings.Builder
	for i, r := range s {
		if i > 0 && i%n == 0 {
			sb.WriteString(sep)
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
