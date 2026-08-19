// Identity on a WebSocket: how a credential is presented, why it is never in a
// URL, and the one-method seam that verifies it.
//
// # Market data is public, so anonymous is the default and not a fallback
//
// CLAUDE.md §6 puts the catalogue read surface — the odds board, event detail,
// line history, multi-book comparison — in the public tier, and
// internal/httpapi/middleware says so where it declines to require an identity
// chain-wide: "the catalogue read surface […] is public, while the account and
// wagering surface is not. A chain-wide requirement would make the landing page
// impossible." The same is true here. An anonymous connection is a first-class
// state, not a degraded one.
//
// What a verified identity buys is the account-shaped part of the stream —
// which is not in this phase — plus the durable session key D6 restores a
// subscription set from. So the token is OPTIONAL and, when present, STRICT.
//
// # There is exactly ONE token verifier in this repository
//
// This package declares [TokenVerifier], one method, and nothing else. It does
// NOT import internal/auth. cmd/stream adapts auth.TokenIssuer to it in one
// line, mirroring cmd/api's newAuthenticator, and that indirection is the
// point: internal/auth pins the signing algorithm from configuration and
// ignores the presented token's own `alg` header, so `alg: none` and an
// algorithm-confusion downgrade are unrepresentable rather than merely
// rejected. A second verifier anywhere in the tree would be a second place for
// that to be subtly wrong, and the second one is always the one nobody reviews.
//
// The interface is declared by the CONSUMER, per CLAUDE.md §12, for the reason
// internal/httpapi/middleware gives at its own Authenticator: internal/auth owns
// minting, argon2id, TOTP and the refresh family; this package owns one
// question — "who is this connection?" — and this is the whole of it.
//
// # Audience
//
// The token is verified against auth.DefaultAudience ("sharpline-api"), because
// that is what the API actually mints today. internal/auth already says what the
// better end state is: "When `stream` starts verifying tokens it should get its
// own audience value rather than reusing this one, so that a token scoped to the
// WebSocket gateway cannot place a wager." That is right, and it is deliberately
// NOT done here — it requires the API to mint a second token, which is a phase-5
// surface change. Written down rather than quietly deferred, because the reason
// it is safe today (this gateway can do nothing a wager could be placed with) is
// the same reason it stops being safe the moment that changes.
package wsgw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// Subprotocol names.
const (
	// BearerSubprotocolPrefix is the offer that carries an access token:
	// "sharpline.bearer.<jwt>".
	//
	// # Why the subprotocol, of all places
	//
	// A browser's `new WebSocket(url, protocols)` can set exactly one request
	// header: Sec-WebSocket-Protocol. There is no options bag, no header map,
	// and no way to attach an Authorization header — the API simply does not
	// exist. So the choice for a browser client is between the subprotocol list
	// and the URL, and the URL is disqualified: a query string is written to
	// every access log in the path, is sent in Referer headers, is kept in
	// browser history, and is the thing a developer pastes into a chat window
	// when they ask a colleague why their connection is failing. The
	// subprotocol offer is none of those things.
	//
	// It is not free of cost. The offer is still a request header, so it is
	// visible to a proxy that logs headers, and it is not encrypted beyond TLS.
	// That is true of `Authorization: Bearer` as well, and it is the level of
	// exposure a bearer token is designed for; the URL is a strictly worse
	// place, which is the comparison that matters.
	BearerSubprotocolPrefix = "sharpline.bearer."

	// MaxTokenLen bounds a presented credential.
	//
	// An HS256 access token with this build's claims is a few hundred bytes. 4
	// KiB is generous headroom for a future key id or an `amr` claim and is far
	// below anything worth using as a memory lever, and the bound exists at all
	// so nothing downstream — the verifier, a log line, a span — ever handles an
	// unbounded string that arrived from a stranger.
	MaxTokenLen = 4096

	// maxSubprotocolOffers bounds how many offers are examined.
	//
	// Sec-WebSocket-Protocol is a comma-separated list and a client may repeat
	// the header. Both are unbounded on the wire, so the parse is bounded here:
	// a legitimate client offers two entries, and refusing to walk more than 16
	// costs nothing real.
	maxSubprotocolOffers = 16
)

// bearerPrefixLen is len("Bearer ").
const bearerPrefixLen = 7

// refusedQueryParams are the query parameter names that cause [ErrTokenInQuery].
//
// A CLOSED list rather than a heuristic. A heuristic ("does any value look like
// a JWT?") would refuse a legitimate parameter that happened to contain two
// dots, and would miss a token under a name nobody thought of — so it would be
// both wrong and incomplete. These are the names a developer reaches for, which
// is precisely the population the refusal exists to teach.
var refusedQueryParams = []string{"token", "access_token", "accesstoken", "jwt", "bearer", "authorization"}

// Identity is who a connection belongs to.
//
// It carries what the gateway can act on and nothing more, for the reason
// internal/auth gives about the claims themselves: a JWT is signed, not
// encrypted, and its payload is readable by anyone holding it. There is no
// email, no display name and no account status here — status especially, because
// a claim is a snapshot taken at mint time and a customer who self-excludes at
// 14:00 must not be carried as "active" until their token expires.
//
// The ZERO VALUE is the anonymous case, and it is a legitimate, expected state.
type Identity struct {
	// UserID is users.id. Zero for an anonymous connection.
	UserID domain.UserID

	// SessionID is the refresh_token_families row this access token descends
	// from. It is what a durable subscription set is keyed by (D6) and what
	// "log out this device" would revoke. Empty is legal — a token minted
	// outside a family has none.
	SessionID string

	// ExpiresAt is the token's own expiry, already verified.
	//
	// It is carried so the connection loop can reason about remaining lifetime
	// without re-parsing anything. It is worth being explicit that this gateway
	// does NOT currently close a connection when its token expires: the
	// connection was authorised at upgrade, and the only thing on it is public
	// market data. The day an account-scoped channel exists, this field is what
	// the re-authorisation check reads, and that decision belongs with that
	// feature rather than being pre-guessed here.
	ExpiresAt time.Time
}

// Anonymous reports whether the connection carries no verified identity. It is
// the same predicate as "the zero value", named so the call sites read as the
// state they are testing for.
func (i Identity) Anonymous() bool { return i.UserID.IsZero() }

// LogValue implements slog.LogValuer.
//
// It exists so that logging a whole Identity is safe BY CONSTRUCTION rather than
// by every call site remembering which fields are secret. There is no token
// field on this type — deliberately, exactly as middleware.Identity has none —
// so there is nothing here that must not be logged, and keeping the rendering in
// one place means a field added later is considered once.
func (i Identity) LogValue() slog.Value {
	if i.Anonymous() {
		return slog.GroupValue(slog.Bool("anonymous", true))
	}
	return slog.GroupValue(
		slog.String("user_id", i.UserID.String()),
		slog.String("session_id", i.SessionID),
		slog.Time("expires_at", i.ExpiresAt),
	)
}

// TokenVerifier verifies an access token and produces the identity it names.
//
// It is declared here, by the consumer (CLAUDE.md §12), and it has one method
// because this package has one question.
//
// # Requirements on any implementation
//
// These are not suggestions. This package cannot enforce them from the outside,
// so they are the contract the implementation is reviewed against — the same
// list internal/httpapi/middleware states at its Authenticator, restated rather
// than cross-referenced because a requirement one import away is a requirement
// somebody will not read:
//
//   - Pin the signing algorithm from CONFIGURATION and never from the token's
//     own header. "alg": "none" must be rejected, and so must a token
//     presenting a symmetric algorithm to a verifier holding a public key.
//   - Verify issuer, audience, expiry and not-before.
//   - Compare every secret in constant time.
//   - Return an error wrapping [ErrInvalidCredential] for EVERY failure, with
//     no distinction the client can observe between expired, badly signed,
//     unknown-subject and revoked. Each distinction is an oracle.
//   - NEVER put the token, or any part of it, in the returned error.
type TokenVerifier interface {
	// VerifyAccessToken verifies token and returns the identity it names. The
	// token is the raw credential: it must not be logged, traced, or included
	// in the returned error.
	VerifyAccessToken(ctx context.Context, token string) (Identity, error)
}

// TokenVerifierFunc adapts a function to TokenVerifier, so internal/auth does
// not have to import this package to satisfy it — a cmd/ entrypoint adapts its
// verifier in one line, which is exactly what cmd/api already does for the REST
// surface.
type TokenVerifierFunc func(ctx context.Context, token string) (Identity, error)

// VerifyAccessToken implements TokenVerifier.
func (f TokenVerifierFunc) VerifyAccessToken(ctx context.Context, token string) (Identity, error) {
	return f(ctx, token)
}

// CredentialSource says where a presented credential came from. It is a closed
// set and is safe to log: it names a mechanism, never a value.
type CredentialSource string

// The credential sources.
const (
	// CredentialAbsent — no credential was presented. The normal case.
	CredentialAbsent CredentialSource = "absent"

	// CredentialSubprotocol — a "sharpline.bearer.<jwt>" offer. The browser
	// path.
	CredentialSubprotocol CredentialSource = "subprotocol"

	// CredentialHeader — an `Authorization: Bearer` header. The non-browser
	// path: pkg/client, tests, curl.
	CredentialHeader CredentialSource = "header"
)

// String implements fmt.Stringer.
func (s CredentialSource) String() string { return string(s) }

// Credential is a presented access token and where it came from.
//
// The token is UNEXPORTED and reachable only through [Credential.Token]. That is
// not decoration: it means a Credential cannot be spread into a log line, a span
// attribute or a fmt.Sprintf by accident, and the only way to reach the secret is
// to write the call that reaches it. [Credential.LogValue] renders the mechanism
// and whether one was present, which is everything an operator needs.
type Credential struct {
	token  string
	source CredentialSource
}

// Source reports where the credential came from, or CredentialAbsent.
func (c Credential) Source() CredentialSource { return c.source }

// Present reports whether a credential was supplied at all.
func (c Credential) Present() bool { return c.source != CredentialAbsent && c.source != "" }

// Token returns the raw credential.
//
// It must be passed to a [TokenVerifier] and nowhere else. Do not log it, do not
// put it in an error, do not attach it to a span.
func (c Credential) Token() string { return c.token }

// LogValue implements slog.LogValuer. It renders the mechanism and presence and
// never the value.
func (c Credential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("source", string(c.source)),
		slog.Bool("present", c.Present()),
	)
}

// ExtractCredential pulls the access token out of an upgrade request.
//
// Order of operations, and each step's reason:
//
//  1. THE QUERY STRING IS CHECKED FIRST, AND IS A REFUSAL. If any of
//     [refusedQueryParams] is present the request is rejected with
//     [ErrTokenInQuery], even if a perfectly good subprotocol credential is also
//     offered — because the token is already in the URL by then, and the URL is
//     already in the proxy's access log. Refusing the connection is the only
//     outcome that makes a developer stop doing it; ignoring it silently would
//     have them ship it, watch public market data arrive, and conclude it works.
//
//  2. THE SUBPROTOCOL OFFER, because it is the browser's only option (see
//     [BearerSubprotocolPrefix]) and is therefore the mechanism this gateway
//     wants clients to standardise on.
//
//  3. `Authorization: Bearer`, for clients that can set headers.
//
// A credential that is PRESENT BUT UNUSABLE — an empty token after the prefix,
// an Authorization header that is not a bearer token, a token past
// [MaxTokenLen], a token containing bytes a JWT cannot contain — is
// [ErrInvalidCredential], never "absent". A client that believes it is
// authenticated and is quietly not is the failure this branch exists to prevent,
// and it is the same judgement internal/httpapi/middleware makes for the same
// case on the REST surface.
//
// No error returned from here contains the token or any part of it.
func ExtractCredential(r *http.Request) (Credential, error) {
	if r == nil {
		return Credential{}, fmt.Errorf("%w: nil request", ErrInvalidCredential)
	}

	if r.URL != nil {
		q := r.URL.Query()
		for _, name := range refusedQueryParams {
			if q.Has(name) {
				return Credential{}, fmt.Errorf("%w: %q is not accepted; present the token as a "+
					"%q subprotocol offer or as an Authorization: Bearer header",
					ErrTokenInQuery, name, BearerSubprotocolPrefix+"<token>")
			}
		}
	}

	for _, offer := range Offers(r) {
		if !strings.HasPrefix(offer, BearerSubprotocolPrefix) {
			continue
		}
		token := offer[len(BearerSubprotocolPrefix):]
		if err := validToken(token); err != nil {
			return Credential{}, err
		}
		return Credential{token: token, source: CredentialSubprotocol}, nil
	}

	if h := r.Header.Get("Authorization"); h != "" {
		// Case-insensitive scheme match: RFC 7235 says the scheme token is
		// case-insensitive and real clients send "bearer". Everything after the
		// single space is the token — a second space or a comma-separated
		// credential list is not a bearer token this system issued, and is
		// refused rather than partially parsed. Same rule, same reason, as
		// internal/httpapi/middleware's extractToken.
		if len(h) <= bearerPrefixLen || !strings.EqualFold(h[:bearerPrefixLen], "bearer ") {
			return Credential{}, fmt.Errorf("%w: Authorization header is not a bearer credential",
				ErrInvalidCredential)
		}
		token := strings.TrimSpace(h[bearerPrefixLen:])
		if err := validToken(token); err != nil {
			return Credential{}, err
		}
		return Credential{token: token, source: CredentialHeader}, nil
	}

	return Credential{source: CredentialAbsent}, nil
}

// validToken applies the bounds a credential must satisfy before it is handed to
// a verifier. It reports what was wrong about the SHAPE and never what the value
// was.
func validToken(token string) error {
	if token == "" {
		return fmt.Errorf("%w: empty credential", ErrInvalidCredential)
	}
	if len(token) > MaxTokenLen {
		return fmt.Errorf("%w: credential is %d bytes, limit is %d",
			ErrInvalidCredential, len(token), MaxTokenLen)
	}
	// A JWT is three base64url segments separated by dots. Anything outside
	// that alphabet cannot be one, and a comma in particular would have split
	// the subprotocol header — so rejecting it here is what keeps the transport
	// and the credential from disagreeing about where the value ends.
	for i := 0; i < len(token); i++ {
		c := token[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '=':
		default:
			return fmt.Errorf("%w: credential contains a byte no access token can contain",
				ErrInvalidCredential)
		}
	}
	return nil
}

// Offers returns the subprotocols the client offered, bounded.
//
// Sec-WebSocket-Protocol may appear more than once and each occurrence is a
// comma-separated list; both are flattened here. Parsing stops after
// [maxSubprotocolOffers] entries, which is why a client cannot make this
// function walk an arbitrary list.
func Offers(r *http.Request) []string {
	if r == nil {
		return nil
	}
	values := r.Header.Values("Sec-WebSocket-Protocol")
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, maxSubprotocolOffers)
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			out = append(out, part)
			if len(out) == maxSubprotocolOffers {
				return out
			}
		}
	}
	return out
}

// SelectSubprotocol returns the subprotocol the server must echo in the
// handshake response, and whether the client offered it.
//
// It returns [Protocol] and ONLY [Protocol]. The bearer offer is a credential
// carrier, not a protocol, and echoing it back would write the client's own
// access token into the handshake RESPONSE — which is a header a proxy may log
// on a path where the request headers were not logged at all. That single line
// is the difference between "the token was in a request header" and "the token
// was in both directions of the handshake", so it is stated here rather than
// left as an obvious omission.
//
// A client that offers no subprotocol at all gets false. There is no default:
// an unversioned connection cannot have its frame shapes changed later without
// breaking somebody silently (see [Protocol]).
func SelectSubprotocol(r *http.Request) (string, bool) {
	for _, offer := range Offers(r) {
		if offer == Protocol {
			return Protocol, true
		}
	}
	return "", false
}

// Authenticate resolves a presented credential into an [Identity].
//
// The three outcomes, and only one of them proceeds authenticated:
//
//	no credential presented   -> anonymous, no error. Market data is public (D5).
//	a credential, no verifier -> ErrInvalidCredential. NOT a downgrade: see
//	                             Options.Verifier on why a deployment missing a
//	                             signing key must fail loudly.
//	a credential that fails   -> ErrInvalidCredential, including when the cause
//	                             is INTERNAL to the verifier. An error during
//	                             validation is never a pass-through; `if err !=
//	                             nil { continue }` is the one-character-looking
//	                             mistake that converts a verifier outage into an
//	                             authentication bypass, and there is no branch
//	                             here that returns an identity after a non-nil
//	                             error.
//
// A verifier that returns no error and an anonymous identity is a bug in the
// verifier, and it is treated as a rejection rather than as an anonymous
// pass-through — failing closed, exactly as internal/httpapi/middleware does.
//
// The verifier's own error is wrapped rather than replaced, because
// internal/auth's verification errors are already incurious — they say a token
// failed, never which check it failed or whose token it was — and they are the
// only diagnostic an operator gets. They reach the log and nothing else.
func Authenticate(ctx context.Context, v TokenVerifier, c Credential) (Identity, error) {
	if !c.Present() {
		return Identity{}, nil
	}
	if v == nil {
		return Identity{}, fmt.Errorf("%w: this gateway has no token verifier configured",
			ErrInvalidCredential)
	}

	id, err := v.VerifyAccessToken(ctx, c.Token())
	if err != nil {
		if errors.Is(err, ErrInvalidCredential) {
			return Identity{}, err
		}
		return Identity{}, fmt.Errorf("%w: %w", ErrInvalidCredential, err)
	}
	if id.Anonymous() {
		return Identity{}, fmt.Errorf("%w: verifier returned an empty identity with no error",
			ErrInvalidCredential)
	}
	return id, nil
}
