package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// MinSigningKeyLen is the floor for the HMAC key: 32 bytes, the output width of
// SHA-256.
//
// It matches internal/platform/config's minJWTKeyLen exactly, and the
// duplication is on purpose rather than an import: config validates at startup
// so a bad deployment dies before it serves traffic, and this package validates
// at construction so a caller that builds an issuer some other way — a test, a
// future admin tool — cannot hold it weaker. Two independent checks of the same
// invariant is the correct amount for a signing key.
const MinSigningKeyLen = 32

// Access-token defaults.
const (
	// DefaultIssuer is the `iss` claim. Verified, not decorative: a token minted
	// by some other system that happens to share our signing key is rejected.
	DefaultIssuer = "sharpline"

	// DefaultAudience is the `aud` claim. The API is the only intended
	// audience today. When `stream` starts verifying tokens it should get its
	// own audience value rather than reusing this one, so that a token scoped
	// to the WebSocket gateway cannot place a wager.
	DefaultAudience = "sharpline-api"

	// DefaultAccessTTL is how long an access token is valid.
	//
	// Ten minutes is a deliberate midpoint. Shorter is more secure — a stolen
	// access token is useless sooner — and the cost is refresh traffic: every
	// active client performs one refresh per TTL, and a refresh is a write
	// transaction (an UPDATE and an INSERT, per rotation). At CLAUDE.md §10's
	// stated target of 10k concurrent subscribers, a 10-minute TTL is ~17
	// refreshes per second, which is nothing for Postgres; a 1-minute TTL would
	// be ~167/s of write traffic bought for very little, since the refresh
	// token itself is the thing worth stealing at that point.
	//
	// The upper bound on the choice is set by revocation: an access token is
	// not checked against the database, so a revoked session keeps working
	// until its current access token expires. Ten minutes is how long "log out
	// everywhere" takes to be complete. Anything measured in hours would make
	// that promise false.
	DefaultAccessTTL = 10 * time.Minute
)

// jwtAlg is the ONE algorithm this package signs and verifies with. It is a
// constant rather than a field: an algorithm that can be configured is an
// algorithm that can be configured to "none".
const jwtAlg = "HS256"

// jwtTyp is the required `typ` header.
const jwtTyp = "JWT"

// b64url is JWT's encoding: URL-safe alphabet, no padding (RFC 7515 §2).
var b64url = base64.RawURLEncoding

// Claims is the payload of an access token.
//
// # What is here, and what is deliberately not
//
// This phase's brief: "Include only what the API needs in claims. A JWT is
// readable by anyone holding it." A JWT is signed, not encrypted; the payload
// is base64, which is not a secret. So:
//
//   - No email address. It is personal data and the API can look it up.
//   - No display name.
//   - NO ACCOUNT STATUS. This is the one that looks like an omission and is
//     not. See [UserStatus.CanWager]: a claim is a snapshot taken at mint time,
//     so a customer who self-excludes at 14:00 would carry an "active" claim
//     until their token expired, and the minutes right after somebody decides
//     to self-exclude are exactly when the control has to work. Status is read
//     from the database inside the transaction that needs it, every time.
//   - No entitlement or role. There is nothing in the schema to populate one
//     from; when the admin console (CLAUDE.md §6, Platform) needs one it should
//     arrive as a column, a migration, and an explicit decision — not as a
//     claim the API trusts because it signed it.
//
// What is here is identity (`sub`), session lineage (`sid`), and the standard
// registered claims that make the token verifiable.
type Claims struct {
	// Issuer is `iss`. Verified against the verifier's configured issuer.
	Issuer string
	// Subject is `sub`: the user's domain.UserID.
	Subject domain.UserID
	// Audience is `aud`. Verified for membership.
	Audience string
	// ID is `jti`: a unique identifier for this access token. Safe to log, and
	// the value an audit-log row should carry to tie an action to a token.
	ID string
	// SessionID is `sid`: the refresh_token_families.id this token was minted
	// under. It makes "which login did this action come from" answerable
	// without a database round trip, and it is what internal/httpapi should
	// stamp on an audit-log row alongside jti.
	SessionID string
	// IssuedAt is `iat`.
	IssuedAt time.Time
	// NotBefore is `nbf`.
	NotBefore time.Time
	// ExpiresAt is `exp`.
	ExpiresAt time.Time
}

// wireClaims is the JSON shape. Times are Unix seconds, per RFC 7519's
// NumericDate.
//
// Times are int64 rather than json.Number or float64: a float64 NumericDate is
// legal in the RFC and is a bad idea, because a fractional expiry is a rounding
// question nobody wants to answer inside a security check. We emit integers and
// accept integers.
type wireClaims struct {
	Issuer    string          `json:"iss"`
	Subject   string          `json:"sub"`
	Audience  json.RawMessage `json:"aud"`
	ID        string          `json:"jti"`
	SessionID string          `json:"sid,omitempty"`
	IssuedAt  int64           `json:"iat"`
	NotBefore int64           `json:"nbf"`
	ExpiresAt int64           `json:"exp"`
}

// wireHeader is the JOSE header. `kid` is absent: there is one symmetric key,
// named by SHARPLINE_JWT_SIGNING_KEY, and a key id would imply a key set this
// system does not have. When rotation arrives it arrives as a keyring the same
// way TOTP's did (see keyring.go) and adds `kid` then.
type wireHeader struct {
	Alg  string          `json:"alg"`
	Typ  string          `json:"typ"`
	Crit json.RawMessage `json:"crit,omitempty"`
}

// TokenIssuer mints and verifies HS256 access tokens.
//
// # Why this is ~150 lines of standard library and not a JWT dependency
//
// The single most exploited JWT vulnerability class is algorithm confusion: a
// library that reads `alg` out of the attacker-supplied header and dispatches
// on it, so `{"alg":"none"}` verifies with an empty signature, or `{"alg":
// "HS256"}` against an RSA verifier makes the PUBLIC key the HMAC secret. Every
// mature library has a way to pin the algorithm, and every one of them makes
// pinning opt-in — the vulnerable call is the short one.
//
// Here there is no dispatch to get wrong. [TokenIssuer.Verify] computes
// HMAC-SHA256 over the received `header.payload` with the configured key and
// compares it to the received signature, and that is the entire verification
// decision. The header's `alg` is checked for equality with "HS256" AFTERWARDS
// as defence in depth, and that check is redundant by construction: a token
// with `{"alg":"none"}` carries no signature and therefore already failed. An
// asymmetric algorithm cannot be reached because no asymmetric code exists in
// this file.
//
// The secondary benefit is that go.mod does not grow. The cost is that this
// file supports exactly one algorithm and no key set, which is precisely the
// feature set the system has.
type TokenIssuer struct {
	key      []byte
	issuer   string
	audience string
	ttl      time.Duration
	leeway   time.Duration
	now      func() time.Time
	newID    func() (string, error)
}

// TokenIssuerOptions configures [NewTokenIssuer].
type TokenIssuerOptions struct {
	// SigningKey is the HMAC secret, at least [MinSigningKeyLen] bytes. It
	// comes from SHARPLINE_JWT_SIGNING_KEY, which internal/platform/config
	// already refuses to load below that length.
	//
	// It is a []byte and it is NOT wrapped in a [Secret]. A Secret's job is to
	// survive being logged by accident; this value is held for the process's
	// lifetime inside an unexported field of a struct that has no String
	// method, is never part of a response, and would have to be reached
	// deliberately to be leaked. Wrapping it would mean an Expose() call on
	// every sign, which is the exact call this package wants to keep rare and
	// greppable.
	SigningKey []byte

	// Issuer is the `iss` claim. Empty means [DefaultIssuer].
	Issuer string
	// Audience is the `aud` claim. Empty means [DefaultAudience].
	Audience string
	// TTL is the access-token lifetime. Zero means [DefaultAccessTTL].
	TTL time.Duration

	// Leeway is the tolerance applied to `exp` and `nbf`.
	//
	// It defaults to ZERO, which is unusual advice and is right here: CLAUDE.md
	// §9 puts every service on hardware the author controls, in one cluster,
	// with one clock. There is no cross-provider skew to absorb, and a leeway
	// is a window during which an expired token still works. Set it only if a
	// deployment genuinely spans machines whose clocks are not disciplined —
	// and then fix the clocks.
	Leeway time.Duration

	// Now is the clock seam. Nil means time.Now.
	Now func() time.Time

	// newID mints the `jti`. Unexported: only this package's tests substitute
	// it.
	newID func() (string, error)
}

// NewTokenIssuer validates the options and builds an issuer.
func NewTokenIssuer(opts TokenIssuerOptions) (*TokenIssuer, error) {
	if len(opts.SigningKey) < MinSigningKeyLen {
		// The key length is reported; the key is not. A length is not a secret
		// and "your key is 8 bytes" is the only useful thing to say here.
		return nil, fmt.Errorf("%w (got %d)", ErrSigningKeyLen, len(opts.SigningKey))
	}

	iss := opts.Issuer
	if iss == "" {
		iss = DefaultIssuer
	}
	aud := opts.Audience
	if aud == "" {
		aud = DefaultAudience
	}
	ttl := opts.TTL
	if ttl == 0 {
		ttl = DefaultAccessTTL
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("%w: access token TTL %s is not positive", ErrInvalid, ttl)
	}
	if opts.Leeway < 0 {
		return nil, fmt.Errorf("%w: leeway %s is negative", ErrInvalid, opts.Leeway)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	newID := opts.newID
	if newID == nil {
		newID = func() (string, error) { return NewOpaqueID("") }
	}

	// The key is copied. A caller holding the slice it passed in could
	// otherwise mutate the signing key underneath a running issuer, which is
	// not a threat so much as an unpleasant surprise waiting to be debugged.
	key := make([]byte, len(opts.SigningKey))
	copy(key, opts.SigningKey)

	return &TokenIssuer{
		key:      key,
		issuer:   iss,
		audience: aud,
		ttl:      ttl,
		leeway:   opts.Leeway,
		now:      now,
		newID:    newID,
	}, nil
}

// TTL returns the configured access-token lifetime, so a caller can tell the
// client when to refresh without hardcoding the number twice.
func (ti *TokenIssuer) TTL() time.Duration { return ti.ttl }

// Issue mints a signed access token for a user and session.
//
// The token comes back in a [Secret]. It is a bearer credential for its whole
// lifetime, and the value of wrapping it is that a handler cannot log the
// response struct on the way out.
func (ti *TokenIssuer) Issue(userID domain.UserID, sessionID string) (Secret, Claims, error) {
	if userID.IsZero() {
		return Secret{}, Claims{}, fmt.Errorf("%w: access token subject is empty", ErrInvalid)
	}

	jti, err := ti.newID()
	if err != nil {
		return Secret{}, Claims{}, fmt.Errorf("%w: minting jti: %w", ErrInternal, err)
	}

	issued := ti.now().UTC().Truncate(time.Second)
	claims := Claims{
		Issuer:    ti.issuer,
		Subject:   userID,
		Audience:  ti.audience,
		ID:        jti,
		SessionID: sessionID,
		IssuedAt:  issued,
		NotBefore: issued,
		ExpiresAt: issued.Add(ti.ttl),
	}

	audJSON, err := json.Marshal(claims.Audience)
	if err != nil {
		return Secret{}, Claims{}, fmt.Errorf("%w: encoding audience: %w", ErrInternal, err)
	}
	payload, err := json.Marshal(wireClaims{
		Issuer:    claims.Issuer,
		Subject:   claims.Subject.String(),
		Audience:  audJSON,
		ID:        claims.ID,
		SessionID: claims.SessionID,
		IssuedAt:  claims.IssuedAt.Unix(),
		NotBefore: claims.NotBefore.Unix(),
		ExpiresAt: claims.ExpiresAt.Unix(),
	})
	if err != nil {
		return Secret{}, Claims{}, fmt.Errorf("%w: encoding claims: %w", ErrInternal, err)
	}

	header, err := json.Marshal(wireHeader{Alg: jwtAlg, Typ: jwtTyp})
	if err != nil {
		return Secret{}, Claims{}, fmt.Errorf("%w: encoding header: %w", ErrInternal, err)
	}

	signingInput := b64url.EncodeToString(header) + "." + b64url.EncodeToString(payload)
	sig := ti.sign(signingInput)

	return NewSecret(signingInput + "." + b64url.EncodeToString(sig)), claims, nil
}

// Verify checks a presented access token and returns its claims.
//
// Order of operations, and why signature comes first: nothing in the payload is
// read as data until the signature has been checked. A verifier that parsed
// claims first would be running a JSON decoder over attacker-controlled bytes
// and, worse, would invite a future edit that made a decision from an
// unverified field.
//
// Every failure returns [ErrAccessTokenInvalid] wrapped with a reason. The
// reason is for logs and metrics; the caller must render all of them as one
// 401, because "expired" versus "bad signature" versus "wrong audience" is
// information an attacker can use to steer.
func (ti *TokenIssuer) Verify(token Secret) (Claims, error) {
	raw := token.Expose()

	// Exactly three segments. strings.Split then a length check would also
	// accept "a.b.c.d" as four and reject it, but Cut twice makes the failure
	// mode explicit and avoids allocating a slice for a malformed token.
	headerB64, rest, ok := strings.Cut(raw, ".")
	if !ok {
		return Claims{}, fmt.Errorf("%w: not three dot-separated segments", ErrAccessTokenInvalid)
	}
	payloadB64, sigB64, ok := strings.Cut(rest, ".")
	if !ok {
		return Claims{}, fmt.Errorf("%w: not three dot-separated segments", ErrAccessTokenInvalid)
	}
	if strings.Contains(sigB64, ".") {
		return Claims{}, fmt.Errorf("%w: more than three segments", ErrAccessTokenInvalid)
	}

	gotSig, err := b64url.DecodeString(sigB64)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: signature is not base64url", ErrAccessTokenInvalid)
	}

	// THE verification decision. The algorithm is `jwtAlg` because this line
	// says so, not because the token asked for it. An `{"alg":"none"}` token
	// arrives with an empty signature and fails here.
	wantSig := ti.sign(headerB64 + "." + payloadB64)
	if subtle.ConstantTimeCompare(gotSig, wantSig) != 1 {
		return Claims{}, fmt.Errorf("%w: signature does not verify", ErrAccessTokenInvalid)
	}

	// From here the bytes are ours: they carry a MAC we produced. The header
	// checks below are defence in depth — redundant by construction, kept
	// because a future edit that broke the redundancy should fail a test.
	headerJSON, err := b64url.DecodeString(headerB64)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: header is not base64url", ErrAccessTokenInvalid)
	}
	var hdr wireHeader
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return Claims{}, fmt.Errorf("%w: header is not JSON", ErrAccessTokenInvalid)
	}
	if hdr.Alg != jwtAlg {
		return Claims{}, fmt.Errorf("%w: header alg is not %s", ErrAccessTokenInvalid, jwtAlg)
	}
	if hdr.Typ != jwtTyp {
		return Claims{}, fmt.Errorf("%w: header typ is not %s", ErrAccessTokenInvalid, jwtTyp)
	}
	// RFC 7515 §4.1.11: an unrecognised `crit` extension MUST be rejected. This
	// implementation understands no extensions, so any `crit` at all is fatal.
	if len(hdr.Crit) != 0 {
		return Claims{}, fmt.Errorf("%w: header declares unsupported crit extensions", ErrAccessTokenInvalid)
	}

	payloadJSON, err := b64url.DecodeString(payloadB64)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: payload is not base64url", ErrAccessTokenInvalid)
	}
	var wc wireClaims
	dec := json.NewDecoder(strings.NewReader(string(payloadJSON)))
	// Refuse unknown claims. A token carrying a field this build does not
	// understand is a token minted by a different version of this code against
	// the same key, and treating it as valid is how a "role" claim added in one
	// service becomes invisible in another.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wc); err != nil {
		return Claims{}, fmt.Errorf("%w: payload is not the expected claim set", ErrAccessTokenInvalid)
	}

	if wc.Issuer != ti.issuer {
		return Claims{}, fmt.Errorf("%w: issuer is not %q", ErrAccessTokenInvalid, ti.issuer)
	}
	aud, err := decodeAudience(wc.Audience)
	if err != nil {
		return Claims{}, err
	}
	if !containsAudience(aud, ti.audience) {
		return Claims{}, fmt.Errorf("%w: audience does not include %q", ErrAccessTokenInvalid, ti.audience)
	}
	subject, err := domain.NewUserID(wc.Subject)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: subject is not a user id", ErrAccessTokenInvalid)
	}

	now := ti.now().UTC()
	exp := time.Unix(wc.ExpiresAt, 0).UTC()
	nbf := time.Unix(wc.NotBefore, 0).UTC()
	if !now.Before(exp.Add(ti.leeway)) {
		return Claims{}, fmt.Errorf("%w: expired at %s", ErrAccessTokenInvalid, exp.Format(time.RFC3339))
	}
	if now.Before(nbf.Add(-ti.leeway)) {
		return Claims{}, fmt.Errorf("%w: not valid before %s", ErrAccessTokenInvalid, nbf.Format(time.RFC3339))
	}

	return Claims{
		Issuer:    wc.Issuer,
		Subject:   subject,
		Audience:  ti.audience,
		ID:        wc.ID,
		SessionID: wc.SessionID,
		IssuedAt:  time.Unix(wc.IssuedAt, 0).UTC(),
		NotBefore: nbf,
		ExpiresAt: exp,
	}, nil
}

// sign computes HMAC-SHA256 over the signing input.
func (ti *TokenIssuer) sign(signingInput string) []byte {
	mac := hmac.New(sha256.New, ti.key)
	// hash.Hash's Write never returns an error; the interface says so.
	mac.Write([]byte(signingInput))
	return mac.Sum(nil)
}

// decodeAudience accepts RFC 7519's two legal shapes for `aud`: a string, or an
// array of strings. Anything else is rejected rather than coerced.
func decodeAudience(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: audience is absent", ErrAccessTokenInvalid)
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	return nil, fmt.Errorf("%w: audience is neither a string nor an array of strings", ErrAccessTokenInvalid)
}

// containsAudience reports membership. The comparison is a plain == because an
// audience value is not a secret and its equality is not timing-sensitive.
func containsAudience(aud []string, want string) bool {
	for _, a := range aud {
		if a == want {
			return true
		}
	}
	return false
}
