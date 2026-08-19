package auth

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// rfc6238Secret is the shared secret from RFC 6238 Appendix B: the ASCII string
// "12345678901234567890", 20 bytes, which is also [TOTPSecretBytes].
var rfc6238Secret = []byte("12345678901234567890")

// TestTOTPMatchesRFC6238TestVectors is the correctness anchor for this file.
//
// Everything else here tests OUR behaviour — the window, the replay guard, the
// URI. This one tests that the arithmetic is TOTP and not something that
// resembles it, against the vectors in the specification itself. Without it,
// a subtly wrong dynamic truncation would pass every other test in this package
// and fail against every authenticator app in the world.
//
// The vectors are 8-digit, which is why [TOTPConfig.Digits] admits 6 to 8 and
// not only 6.
func TestTOTPMatchesRFC6238TestVectors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		unix int64
		code string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}

	cfg := TOTPConfig{Digits: 8}
	for _, c := range cases {
		at := time.Unix(c.unix, 0).UTC()
		got, err := TOTPCodeAt(rfc6238Secret, at, cfg)
		if err != nil {
			t.Fatalf("TOTPCodeAt(%d): %v", c.unix, err)
		}
		if got != c.code {
			t.Errorf("TOTPCodeAt(T=%d) = %s, RFC 6238 Appendix B says %s", c.unix, got, c.code)
		}
	}
}

func TestValidateTOTPCodeAcceptsTheSkewWindowAndNothingElse(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0).UTC()
	cfg := TOTPConfig{} // defaults: 30s period, 6 digits, skew 1

	code := func(offsetSteps int64) string {
		c, err := TOTPCodeAt(rfc6238Secret, now.Add(time.Duration(offsetSteps)*TOTPPeriod), cfg)
		if err != nil {
			t.Fatalf("TOTPCodeAt: %v", err)
		}
		return c
	}

	cases := []struct {
		name       string
		code       string
		wantOK     bool
		wantOffset int64
	}{
		{"current step", code(0), true, 0},
		{"one step back", code(-1), true, -1},
		{"one step forward", code(+1), true, +1},
		{"two steps back", code(-2), false, 0},
		{"two steps forward", code(+2), false, 0},
		{"ten steps forward", code(+10), false, 0},
	}

	current := totpStep(now, TOTPPeriod)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			step, ok, err := ValidateTOTPCode(rfc6238Secret, c.code, now, cfg)
			if err != nil {
				t.Fatalf("ValidateTOTPCode: %v", err)
			}
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && step != current+c.wantOffset {
				t.Fatalf("step = %d, want %d", step, current+c.wantOffset)
			}
		})
	}
}

func TestValidateTOTPCodeRejectsMalformedCodes(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0).UTC()
	cfg := TOTPConfig{}

	for _, code := range []string{"", "12345", "1234567", "abcdef", "  ", "000000000000"} {
		_, ok, err := ValidateTOTPCode(rfc6238Secret, code, now, cfg)
		if err != nil {
			t.Fatalf("ValidateTOTPCode(%q): %v", code, err)
		}
		if ok {
			t.Errorf("ValidateTOTPCode(%q) accepted a malformed code", code)
		}
	}
}

// Authenticator apps display "123 456". A form that rejects the space the user
// copied is a support ticket, not a security control.
func TestValidateTOTPCodeToleratesSeparators(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0).UTC()
	cfg := TOTPConfig{}
	code, err := TOTPCodeAt(rfc6238Secret, now, cfg)
	if err != nil {
		t.Fatalf("TOTPCodeAt: %v", err)
	}

	for _, presented := range []string{
		code,
		code[:3] + " " + code[3:],
		code[:3] + "-" + code[3:],
		" " + code + " ",
	} {
		_, ok, err := ValidateTOTPCode(rfc6238Secret, presented, now, cfg)
		if err != nil {
			t.Fatalf("ValidateTOTPCode(%q): %v", presented, err)
		}
		if !ok {
			t.Errorf("ValidateTOTPCode(%q) rejected a correct code", presented)
		}
	}
}

func TestTOTPConfigNormalisation(t *testing.T) {
	t.Parallel()

	got, err := (TOTPConfig{}).normalise()
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if got.Period != TOTPPeriod || got.Digits != TOTPDigits || got.SkewSteps != TOTPSkewSteps {
		t.Fatalf("zero TOTPConfig normalised to %+v, want the RFC 6238 defaults", got)
	}

	// A skew of zero must be EXPRESSIBLE, which is what WithExactSkew is for:
	// without it the zero value could not distinguish "unset" from "accept only
	// the current step".
	exact, err := (TOTPConfig{}).WithExactSkew().normalise()
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if exact.SkewSteps != 0 {
		t.Fatalf("WithExactSkew normalised to skew %d, want 0", exact.SkewSteps)
	}

	now := time.Unix(1_700_000_000, 0).UTC()
	previous, err := TOTPCodeAt(rfc6238Secret, now.Add(-TOTPPeriod), exact)
	if err != nil {
		t.Fatalf("TOTPCodeAt: %v", err)
	}
	if _, ok, _ := ValidateTOTPCode(rfc6238Secret, previous, now, exact); ok {
		t.Fatal("the previous step's code verified under an exact-skew config")
	}

	for _, bad := range []TOTPConfig{
		{Digits: 5},
		{Digits: 9},
		{Period: -time.Second},
		{SkewSteps: -1},
		{SkewSteps: 100},
	} {
		if _, err := bad.normalise(); !errors.Is(err, ErrInvalid) {
			t.Errorf("normalise(%+v) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestNewTOTPSecretIsFullWidthAndRandom(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool)
	for i := 0; i < 32; i++ {
		s, err := NewTOTPSecret()
		if err != nil {
			t.Fatalf("NewTOTPSecret: %v", err)
		}
		if len(s) != TOTPSecretBytes {
			t.Fatalf("secret is %d bytes, want %d (RFC 4226 §4 recommends 160 bits)", len(s), TOTPSecretBytes)
		}
		key := string(s)
		if seen[key] {
			t.Fatal("two secrets collided; the source is not random")
		}
		seen[key] = true
	}
}

func TestProvisioningURI(t *testing.T) {
	t.Parallel()

	uri, err := ProvisioningURI("Sharpline", "person@example.com", rfc6238Secret, TOTPConfig{})
	if err != nil {
		t.Fatalf("ProvisioningURI: %v", err)
	}

	u, err := url.Parse(uri.Expose())
	if err != nil {
		t.Fatalf("parsing the URI: %v", err)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" {
		t.Fatalf("URI = %s://%s, want otpauth://totp", u.Scheme, u.Host)
	}
	if got, want := u.Path, "/Sharpline:person@example.com"; got != want {
		t.Errorf("label = %q, want %q", got, want)
	}

	q := u.Query()
	if got := q.Get("secret"); got != b32.EncodeToString(rfc6238Secret) {
		t.Errorf("secret parameter = %q", got)
	}
	for k, want := range map[string]string{
		"issuer": "Sharpline", "algorithm": "SHA1", "digits": "6", "period": "30",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// The URI CONTAINS the shared secret, so it must be unprintable. A URI in a
	// log or a screenshot is a permanent 2FA bypass for that user.
	if strings.Contains(uri.String(), "otpauth") {
		t.Error("the provisioning URI printed itself")
	}
	if !strings.Contains(uri.String(), Redacted) {
		t.Error("the provisioning URI does not redact")
	}
}

func TestProvisioningURIRejectsBadLabels(t *testing.T) {
	t.Parallel()

	cases := []struct{ issuer, account string }{
		{"", "person@example.com"},
		{"Sharpline", ""},
		// A ':' would break the label's own "issuer:account" encoding, which is
		// a spec-level ambiguity rather than a cosmetic one.
		{"Sharp:line", "person@example.com"},
		{"Sharpline", "person:example.com"},
	}
	for _, c := range cases {
		if _, err := ProvisioningURI(c.issuer, c.account, rfc6238Secret, TOTPConfig{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("ProvisioningURI(%q, %q) = %v, want ErrInvalid", c.issuer, c.account, err)
		}
	}
	if _, err := ProvisioningURI("Sharpline", "a@b.c", nil, TOTPConfig{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("ProvisioningURI with no secret = %v, want ErrInvalid", err)
	}
}

// The replay guard is what makes a 30-second code single-use. Without it, a
// code observed over the user's shoulder works for the rest of its step.
func TestMemoryReplayGuardBurnsAStepExactlyOnce(t *testing.T) {
	t.Parallel()

	g := NewMemoryReplayGuard(nil)
	ctx := context.Background()
	user := domain.UserID("usr_abc")
	expiry := time.Now().Add(time.Minute)

	first, err := g.Consume(ctx, user, 42, expiry)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !first {
		t.Fatal("the first consumption of a step was refused")
	}

	second, err := g.Consume(ctx, user, 42, expiry)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if second {
		t.Fatal("a step was consumed twice; a TOTP code is replayable inside its window")
	}

	// A DIFFERENT user's identical step is untouched. Without the user in the
	// key, one user's login would burn everybody's current step.
	other, err := g.Consume(ctx, domain.UserID("usr_other"), 42, expiry)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !other {
		t.Fatal("one user's consumption burnt another user's step")
	}

	// And the next step is a separate entry, or a user could log in once per
	// account lifetime.
	next, err := g.Consume(ctx, user, 43, expiry)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !next {
		t.Fatal("burning one step burnt the next one too")
	}
}

func TestMemoryReplayGuardExpiresEntries(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0).UTC()
	clock := base
	g := NewMemoryReplayGuard(func() time.Time { return clock })

	ctx := context.Background()
	user := domain.UserID("usr_abc")
	expiry := base.Add(90 * time.Second)

	if ok, err := g.Consume(ctx, user, 7, expiry); err != nil || !ok {
		t.Fatalf("Consume = %v, %v", ok, err)
	}
	if ok, _ := g.Consume(ctx, user, 7, expiry); ok {
		t.Fatal("the guard did not hold the step")
	}

	// Past the entry's expiry the map must not grow without bound. The step is
	// reusable again, which is harmless: no code from it can still verify.
	clock = base.Add(2 * time.Minute)
	if ok, err := g.Consume(ctx, user, 7, base.Add(4*time.Minute)); err != nil || !ok {
		t.Fatalf("Consume after expiry = %v, %v; want true, nil", ok, err)
	}
	if n := len(g.used); n != 1 {
		t.Fatalf("guard holds %d entries after the sweep, want 1", n)
	}
}

func TestMemoryReplayGuardHonoursContext(t *testing.T) {
	t.Parallel()

	g := NewMemoryReplayGuard(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := g.Consume(ctx, "usr_abc", 1, time.Now().Add(time.Minute)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Consume with a cancelled context = %v, want context.Canceled", err)
	}
}

func TestRecoveryCodes(t *testing.T) {
	t.Parallel()

	codes, digests, err := NewRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		t.Fatalf("NewRecoveryCodes: %v", err)
	}
	if len(codes) != RecoveryCodeCount || len(digests) != RecoveryCodeCount {
		t.Fatalf("got %d codes and %d digests, want %d of each", len(codes), len(digests), RecoveryCodeCount)
	}

	seen := make(map[string]bool)
	for i, c := range codes {
		raw := c.Expose()
		if seen[raw] {
			t.Fatalf("code %d is a duplicate", i)
		}
		seen[raw] = true

		if len(digests[i]) != 32 {
			t.Fatalf("digest %d is %d bytes, want 32", i, len(digests[i]))
		}
		// The displayed form is grouped for transcription; the digest is over
		// the NORMALISED form, so "paste it with the dashes" and "type it
		// without" are one credential rather than two.
		if !strings.Contains(raw, "-") {
			t.Errorf("code %q is not grouped for transcription", raw)
		}
		if got := MatchRecoveryCode(digests, c); got != i {
			t.Errorf("MatchRecoveryCode(codes[%d]) = %d", i, got)
		}
	}

	// The three unambiguous transcription repairs. RFC 4648's base32 alphabet
	// is A-Z and 2-7, so 0, 1 and 8 cannot appear in a real code and their
	// intended characters are unambiguous.
	original := codes[0].Expose()
	for _, variant := range []string{
		strings.ToLower(original),
		strings.ReplaceAll(original, "-", ""),
		strings.ReplaceAll(original, "-", " "),
		strings.ReplaceAll(strings.ReplaceAll(original, "O", "0"), "I", "1"),
		strings.ReplaceAll(original, "B", "8"),
	} {
		if got := MatchRecoveryCode(digests, NewSecret(variant)); got != 0 {
			t.Errorf("MatchRecoveryCode(%q) = %d, want 0", variant, got)
		}
	}

	if got := MatchRecoveryCode(digests, NewSecret("AAAA-AAAA-AAAA-AAAA-AAAA-AAAA")); got != -1 {
		t.Errorf("MatchRecoveryCode of an unrelated code = %d, want -1", got)
	}
	if got := MatchRecoveryCode(nil, codes[0]); got != -1 {
		t.Errorf("MatchRecoveryCode against no digests = %d, want -1", got)
	}

	// A recovery code is a printed credential and must redact like any other.
	if !strings.Contains(codes[0].String(), Redacted) {
		t.Error("a recovery code printed itself")
	}
}

func TestNewRecoveryCodesRejectsBadCounts(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, -1, 65, 1000} {
		if _, _, err := NewRecoveryCodes(n); !errors.Is(err, ErrInvalid) {
			t.Errorf("NewRecoveryCodes(%d) = %v, want ErrInvalid", n, err)
		}
	}
}

func TestRecoveryCodeEntropyWidth(t *testing.T) {
	t.Parallel()

	// 15 bytes is 120 bits, which is what makes SHA-256 at rest defensible
	// rather than uncomfortable — the same argument that justifies SHA-256 for
	// refresh tokens. 15 bytes also encodes to exactly 24 base32 characters
	// with no padding, which is why 15 and not 16.
	if RecoveryCodeBytes != 15 {
		t.Fatalf("RecoveryCodeBytes = %d, want 15", RecoveryCodeBytes)
	}
	codes, _, err := NewRecoveryCodes(1)
	if err != nil {
		t.Fatalf("NewRecoveryCodes: %v", err)
	}
	bare := strings.ReplaceAll(codes[0].Expose(), "-", "")
	if len(bare) != 24 {
		t.Fatalf("code is %d characters, want 24", len(bare))
	}
}
