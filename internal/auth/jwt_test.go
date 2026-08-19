package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

var testSigningKey = []byte("0123456789abcdef0123456789abcdef") // 32 bytes

func newTestIssuer(t *testing.T, now func() time.Time) *TokenIssuer {
	t.Helper()
	ti, err := NewTokenIssuer(TokenIssuerOptions{SigningKey: testSigningKey, Now: now})
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	return ti
}

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

func TestIssueAndVerifyRoundTrip(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	ti := newTestIssuer(t, fixedClock(at))

	token, claims, err := ti.Issue("usr_abc", "fam_xyz")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	got, err := ti.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Subject != domain.UserID("usr_abc") {
		t.Errorf("sub = %q, want usr_abc", got.Subject)
	}
	if got.SessionID != "fam_xyz" {
		t.Errorf("sid = %q, want fam_xyz", got.SessionID)
	}
	if got.Issuer != DefaultIssuer {
		t.Errorf("iss = %q, want %q", got.Issuer, DefaultIssuer)
	}
	if got.Audience != DefaultAudience {
		t.Errorf("aud = %q, want %q", got.Audience, DefaultAudience)
	}
	if !got.ExpiresAt.Equal(at.Add(DefaultAccessTTL)) {
		t.Errorf("exp = %s, want %s", got.ExpiresAt, at.Add(DefaultAccessTTL))
	}
	if got.ID == "" {
		t.Error("jti is empty; an audit-log row has nothing to key on")
	}
	if got.ID != claims.ID {
		t.Errorf("Verify jti %q != Issue jti %q", got.ID, claims.ID)
	}
}

// THE test this file exists for.
//
// Algorithm confusion is the most exploited JWT vulnerability class: a verifier
// that reads `alg` out of the attacker-supplied header and dispatches on it
// will accept {"alg":"none"} with an empty signature. This verifier does not
// dispatch on alg at all — it computes HMAC-SHA256 with the configured key and
// compares — so the attack is structurally impossible rather than defended
// against.
func TestVerifyRejectsAlgNone(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	ti := newTestIssuer(t, fixedClock(at))

	// Build a token exactly as an attacker would: real claims, alg=none, empty
	// signature.
	header := b64url.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := b64url.EncodeToString(mustClaimsJSON(t, at))

	for _, forged := range []string{
		header + "." + payload + ".", // empty signature
		header + "." + payload + "." + b64url.EncodeToString([]byte("")),
		header + "." + payload, // signature segment absent
	} {
		if _, err := ti.Verify(NewSecret(forged)); !errors.Is(err, ErrAccessTokenInvalid) {
			t.Fatalf("Verify(alg=none) = %v, want ErrAccessTokenInvalid; token %q", err, forged)
		}
	}
}

// The header may claim anything. The verifier still computes HS256 with its own
// key, so a token signed with the right key but labelled with a different
// algorithm is rejected by the redundant header check — and one labelled
// differently AND signed differently fails at the signature.
func TestVerifyIgnoresTheHeadersAlgorithmClaim(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	ti := newTestIssuer(t, fixedClock(at))
	payload := b64url.EncodeToString(mustClaimsJSON(t, at))

	for _, alg := range []string{"none", "None", "NONE", "HS384", "HS512", "RS256", "ES256", "PS256", ""} {
		header := b64url.EncodeToString([]byte(`{"alg":"` + alg + `","typ":"JWT"}`))
		// Signed CORRECTLY with our HMAC key. Only the label is wrong. A
		// verifier that trusted the label would either reject with a key-type
		// error or, for RS256 against an HMAC verifier, do something far worse.
		mac := hmac.New(sha256.New, testSigningKey)
		mac.Write([]byte(header + "." + payload))
		forged := header + "." + payload + "." + b64url.EncodeToString(mac.Sum(nil))

		if _, err := ti.Verify(NewSecret(forged)); !errors.Is(err, ErrAccessTokenInvalid) {
			t.Errorf("Verify with alg=%q accepted the token: %v", alg, err)
		}
	}
}

func TestVerifyRejectsATamperedPayload(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	ti := newTestIssuer(t, fixedClock(at))

	token, _, err := ti.Issue("usr_victim", "fam_xyz")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	parts := strings.Split(token.Expose(), ".")

	// Swap the subject and keep the original signature.
	var wc wireClaims
	raw, err := b64url.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding the payload: %v", err)
	}
	if err := json.Unmarshal(raw, &wc); err != nil {
		t.Fatalf("unmarshalling the payload: %v", err)
	}
	wc.Subject = "usr_attacker"
	tampered, err := json.Marshal(wc)
	if err != nil {
		t.Fatalf("re-marshalling: %v", err)
	}

	forged := parts[0] + "." + b64url.EncodeToString(tampered) + "." + parts[2]
	if _, err := ti.Verify(NewSecret(forged)); !errors.Is(err, ErrAccessTokenInvalid) {
		t.Fatalf("Verify of a tampered payload = %v, want ErrAccessTokenInvalid", err)
	}
}

func TestVerifyRejectsAForeignKey(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	mine := newTestIssuer(t, fixedClock(at))

	theirs, err := NewTokenIssuer(TokenIssuerOptions{
		SigningKey: []byte("ffffffffffffffffffffffffffffffff"),
		Now:        fixedClock(at),
	})
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}

	token, _, err := theirs.Issue("usr_abc", "fam_xyz")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := mine.Verify(token); !errors.Is(err, ErrAccessTokenInvalid) {
		t.Fatalf("Verify of a foreign-key token = %v, want ErrAccessTokenInvalid", err)
	}
}

func TestVerifyChecksIssuerAudienceAndTime(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	t.Run("wrong issuer", func(t *testing.T) {
		other, err := NewTokenIssuer(TokenIssuerOptions{
			SigningKey: testSigningKey, Issuer: "somebody-else", Now: fixedClock(at)})
		if err != nil {
			t.Fatalf("NewTokenIssuer: %v", err)
		}
		token, _, err := other.Issue("usr_abc", "fam_xyz")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		// Same key, different issuer. Without the iss check this would verify,
		// which is exactly the "another system shares our key" case.
		if _, err := newTestIssuer(t, fixedClock(at)).Verify(token); !errors.Is(err, ErrAccessTokenInvalid) {
			t.Fatalf("Verify across issuers = %v, want ErrAccessTokenInvalid", err)
		}
	})

	t.Run("wrong audience", func(t *testing.T) {
		// The case this guards: a token scoped to the WebSocket gateway must
		// not be able to place a wager through the API.
		other, err := NewTokenIssuer(TokenIssuerOptions{
			SigningKey: testSigningKey, Audience: "sharpline-stream", Now: fixedClock(at)})
		if err != nil {
			t.Fatalf("NewTokenIssuer: %v", err)
		}
		token, _, err := other.Issue("usr_abc", "fam_xyz")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if _, err := newTestIssuer(t, fixedClock(at)).Verify(token); !errors.Is(err, ErrAccessTokenInvalid) {
			t.Fatalf("Verify across audiences = %v, want ErrAccessTokenInvalid", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		issuer := newTestIssuer(t, fixedClock(at))
		token, _, err := issuer.Issue("usr_abc", "fam_xyz")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}

		later := newTestIssuer(t, fixedClock(at.Add(DefaultAccessTTL+time.Second)))
		if _, err := later.Verify(token); !errors.Is(err, ErrAccessTokenInvalid) {
			t.Fatalf("Verify after expiry = %v, want ErrAccessTokenInvalid", err)
		}

		// One second before expiry it is still good. The boundary matters: an
		// off-by-one here is either a token that dies early or one that
		// outlives its stated lifetime.
		justBefore := newTestIssuer(t, fixedClock(at.Add(DefaultAccessTTL-time.Second)))
		if _, err := justBefore.Verify(token); err != nil {
			t.Fatalf("Verify one second before expiry = %v, want nil", err)
		}
	})

	t.Run("not yet valid", func(t *testing.T) {
		issuer := newTestIssuer(t, fixedClock(at))
		token, _, err := issuer.Issue("usr_abc", "fam_xyz")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		earlier := newTestIssuer(t, fixedClock(at.Add(-time.Minute)))
		if _, err := earlier.Verify(token); !errors.Is(err, ErrAccessTokenInvalid) {
			t.Fatalf("Verify before nbf = %v, want ErrAccessTokenInvalid", err)
		}
	})
}

func TestVerifyRejectsMalformedTokens(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	ti := newTestIssuer(t, fixedClock(at))
	payload := b64url.EncodeToString(mustClaimsJSON(t, at))
	header := b64url.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

	signed := func(h, p string) string {
		mac := hmac.New(sha256.New, testSigningKey)
		mac.Write([]byte(h + "." + p))
		return h + "." + p + "." + b64url.EncodeToString(mac.Sum(nil))
	}

	cases := []struct{ name, token string }{
		{"empty", ""},
		{"one segment", "abc"},
		{"two segments", header + "." + payload},
		{"four segments", signed(header, payload) + ".extra"},
		{"signature not base64url", header + "." + payload + ".!!!!"},
		{"padded base64", header + "." + payload + ".AAAA="},
		{"header not base64url", "!!!." + payload + ".AAAA"},
		{"typ is not JWT", signed(b64url.EncodeToString([]byte(`{"alg":"HS256","typ":"JWE"}`)), payload)},
		// RFC 7515 §4.1.11: an unrecognised `crit` extension MUST be rejected.
		// This implementation understands no extensions, so any crit is fatal.
		{"crit extension", signed(b64url.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT","crit":["exp"]}`)), payload)},
		// A claim this build does not know is a token from a different version
		// of this code against the same key.
		{"unknown claim", signed(header, b64url.EncodeToString(mustJSON(t, map[string]any{
			"iss": DefaultIssuer, "sub": "usr_abc", "aud": DefaultAudience,
			"jti": "j", "iat": at.Unix(), "nbf": at.Unix(), "exp": at.Add(time.Hour).Unix(),
			"role": "admin",
		})))},
		{"subject is not a user id", signed(header, b64url.EncodeToString(mustJSON(t, map[string]any{
			"iss": DefaultIssuer, "sub": "usr:abc", "aud": DefaultAudience,
			"jti": "j", "iat": at.Unix(), "nbf": at.Unix(), "exp": at.Add(time.Hour).Unix(),
		})))},
		{"audience absent", signed(header, b64url.EncodeToString(mustJSON(t, map[string]any{
			"iss": DefaultIssuer, "sub": "usr_abc",
			"jti": "j", "iat": at.Unix(), "nbf": at.Unix(), "exp": at.Add(time.Hour).Unix(),
		})))},
		{"audience is an object", signed(header, b64url.EncodeToString(mustJSON(t, map[string]any{
			"iss": DefaultIssuer, "sub": "usr_abc", "aud": map[string]string{"a": "b"},
			"jti": "j", "iat": at.Unix(), "nbf": at.Unix(), "exp": at.Add(time.Hour).Unix(),
		})))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ti.Verify(NewSecret(c.token)); !errors.Is(err, ErrAccessTokenInvalid) {
				t.Fatalf("Verify = %v, want ErrAccessTokenInvalid", err)
			}
		})
	}
}

// RFC 7519 permits `aud` as an array. A token from a future version of this
// code that lists several audiences must still verify here as long as ours is
// among them.
func TestVerifyAcceptsAnAudienceArray(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	ti := newTestIssuer(t, fixedClock(at))
	header := b64url.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := b64url.EncodeToString(mustJSON(t, map[string]any{
		"iss": DefaultIssuer, "sub": "usr_abc",
		"aud": []string{"sharpline-stream", DefaultAudience},
		"jti": "j", "sid": "fam_xyz",
		"iat": at.Unix(), "nbf": at.Unix(), "exp": at.Add(time.Hour).Unix(),
	}))
	mac := hmac.New(sha256.New, testSigningKey)
	mac.Write([]byte(header + "." + payload))
	token := header + "." + payload + "." + b64url.EncodeToString(mac.Sum(nil))

	if _, err := ti.Verify(NewSecret(token)); err != nil {
		t.Fatalf("Verify with an audience array = %v, want nil", err)
	}
}

// The claim set is a security surface: anything in it is readable by whoever
// holds the token, and anything the API TRUSTS from it is a stale snapshot.
// This test pins what is in there.
func TestClaimsCarryNoPersonalDataAndNoAccountStatus(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	ti := newTestIssuer(t, fixedClock(at))

	token, _, err := ti.Issue("usr_abc", "fam_xyz")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	payload := strings.Split(token.Expose(), ".")[1]
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decoding the payload: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshalling the payload: %v", err)
	}

	want := map[string]bool{"iss": true, "sub": true, "aud": true, "jti": true, "sid": true,
		"iat": true, "nbf": true, "exp": true}
	for k := range got {
		if !want[k] {
			t.Errorf("unexpected claim %q; a JWT is readable by anyone holding it", k)
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing claim %q", k)
		}
	}
	// Named explicitly, because each is a specific thing somebody will be
	// tempted to add. Status above all: see UserStatus.CanWager for why it
	// cannot be a claim.
	for _, forbidden := range []string{"email", "status", "role", "roles", "scope", "name", "balance"} {
		if _, ok := got[forbidden]; ok {
			t.Errorf("claim %q must not be in an access token", forbidden)
		}
	}
}

func TestNewTokenIssuerRejectsAShortKey(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 1, 16, MinSigningKeyLen - 1} {
		_, err := NewTokenIssuer(TokenIssuerOptions{SigningKey: make([]byte, n)})
		if !errors.Is(err, ErrSigningKeyLen) {
			t.Errorf("NewTokenIssuer with a %d-byte key = %v, want ErrSigningKeyLen", n, err)
		}
		// The key must never appear in the error. It is zero bytes here, so the
		// assertion is on the shape rather than the content: the message says a
		// length and nothing else.
		if err != nil && strings.Contains(err.Error(), "\x00") {
			t.Errorf("the error message contains key bytes: %q", err.Error())
		}
	}
	if _, err := NewTokenIssuer(TokenIssuerOptions{SigningKey: make([]byte, MinSigningKeyLen)}); err != nil {
		t.Errorf("NewTokenIssuer with a %d-byte key = %v, want nil", MinSigningKeyLen, err)
	}
}

// The issuer copies its key, so a caller that reuses or zeroes the slice it
// passed cannot change the signing key underneath a running service.
func TestTokenIssuerCopiesItsKey(t *testing.T) {
	t.Parallel()

	key := make([]byte, MinSigningKeyLen)
	copy(key, testSigningKey)

	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	ti, err := NewTokenIssuer(TokenIssuerOptions{SigningKey: key, Now: fixedClock(at)})
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	token, _, err := ti.Issue("usr_abc", "fam_xyz")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	for i := range key {
		key[i] = 0
	}
	if _, err := ti.Verify(token); err != nil {
		t.Fatalf("Verify after the caller zeroed its key slice = %v, want nil", err)
	}
}

func TestIssueRejectsAnEmptySubject(t *testing.T) {
	t.Parallel()

	ti := newTestIssuer(t, time.Now)
	if _, _, err := ti.Issue("", "fam_xyz"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Issue with no subject = %v, want ErrInvalid", err)
	}
}

func mustClaimsJSON(t *testing.T, at time.Time) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"iss": DefaultIssuer,
		"sub": "usr_abc",
		"aud": DefaultAudience,
		"jti": "jti_test",
		"sid": "fam_xyz",
		"iat": at.Unix(),
		"nbf": at.Unix(),
		"exp": at.Add(time.Hour).Unix(),
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling test JSON: %v", err)
	}
	return b
}
