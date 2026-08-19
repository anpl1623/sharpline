package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// maxRefreshSkew is how long before an access token's stated expiry the SDK
// refreshes proactively, at most.
//
// The server's access-token lifetime is minutes, so a minute of skew is a
// modest fraction of it and it absorbs both clock drift and the flight time of
// a request that would otherwise arrive just after expiry. Refreshing early
// costs one extra rotation; refreshing late costs a 401 the caller has to
// survive.
//
// It is a CEILING and not a fixed value — see [effectiveSkew].
const maxRefreshSkew = time.Minute

// effectiveSkew clamps the proactive-refresh window to half the token's
// lifetime.
//
// Without the clamp, a deployment whose access-token lifetime is shorter than
// maxRefreshSkew makes EVERY token stale the instant it is issued: the client
// refreshes, gets a token that is already inside the skew, and refreshes again
// on the next call. That is a refresh storm — one rotation per request against
// an endpoint whose whole purpose is to be called rarely — and it was found by
// a test, not by reasoning, which is the reason it is written down here.
//
// Half is the natural bound: it guarantees a token is considered fresh for at
// least as long as it is considered stale, so concurrent callers have a real
// window in which to share one credential however short the lifetime is.
func effectiveSkew(lifetime time.Duration) time.Duration {
	if lifetime <= 0 {
		return 0
	}
	if half := lifetime / 2; half < maxRefreshSkew {
		return half
	}
	return maxRefreshSkew
}

// TokenSource supplies the bearer token for an authenticated call.
//
// # Why AccessToken returns a generation
//
// This is not the usual oauth2.TokenSource shape, and the difference exists for
// one reason: REFRESH TOKENS ROTATE AND REUSE REVOKES THE FAMILY.
//
// When several goroutines share a session and the access token expires, they
// all get a 401 at once. If each responds by redeeming the refresh token it
// last saw, the second redemption presents an ALREADY-REDEEMED token — which
// the server cannot distinguish from theft, so it revokes the whole login
// lineage and logs the user out everywhere. A correct client must collapse
// those N refreshes into one.
//
// The generation is what makes that collapse possible without a callback or a
// singleflight dependency: it names the credential the caller actually used, so
// [TokenSource.Refresh] can see that someone else has already moved past it and
// return immediately instead of redeeming a second time.
type TokenSource interface {
	// AccessToken returns a bearer token and the generation it belongs to.
	AccessToken(ctx context.Context) (token string, generation uint64, err error)
	// Refresh obtains a new access token, unless the source has already moved
	// beyond generation — in which case it is a no-op and returns nil.
	Refresh(ctx context.Context, generation uint64) error
}

// Tokens is a session's credential material, for persisting across process
// restarts.
//
// # Handling rules, which are not optional
//
// RefreshToken is a BEARER CREDENTIAL: anyone holding it can mint access
// tokens until the family expires. Store it the way a password manager stores a
// password, never in a log, never in a URL, never in a span attribute. The
// server shows it exactly once and cannot show it again.
//
// String and LogValue both redact, so `fmt.Printf("%v", tokens)` and
// `slog.Info("...", "tokens", tokens)` are safe by construction rather than by
// the caller remembering. Reading the fields is an explicit act.
type Tokens struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
}

// String implements fmt.Stringer and redacts both tokens.
func (t Tokens) String() string {
	return fmt.Sprintf("client.Tokens{access:[redacted] expires:%s refresh:[redacted] expires:%s}",
		t.AccessExpiresAt.Format(time.RFC3339), t.RefreshExpiresAt.Format(time.RFC3339))
}

// LogValue implements slog.LogValuer and redacts both tokens.
func (t Tokens) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("access_token_set", t.AccessToken != ""),
		slog.Time("access_expires_at", t.AccessExpiresAt),
		slog.Bool("refresh_token_set", t.RefreshToken != ""),
		slog.Time("refresh_expires_at", t.RefreshExpiresAt),
	)
}

// Credentials are what a user types.
//
// String and LogValue redact the password and the code, for the same structural
// reason [Tokens] does.
type Credentials struct {
	Email    string
	Password string
	// TOTPCode is required only when the account has a confirmed second
	// factor. Attempt the login without it; a [ErrTOTPRequired] response is how
	// the server asks for one.
	TOTPCode string
}

// String implements fmt.Stringer and redacts the password and the code.
func (c Credentials) String() string {
	return fmt.Sprintf("client.Credentials{email:%q password:[redacted] totp:[redacted]}", c.Email)
}

// LogValue implements slog.LogValuer and redacts the password and the code.
func (c Credentials) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("email", c.Email),
		slog.Bool("password_set", c.Password != ""),
		slog.Bool("totp_code_set", c.TOTPCode != ""),
	)
}

// Session holds a rotating credential and refreshes it exactly once per
// expiry, however many goroutines notice at the same moment.
//
// Safe for concurrent use. The mutex is held across the refresh round trip on
// purpose: that IS the single-flight. Concurrent callers block for one network
// call rather than each redeeming the refresh token and revoking the family
// between them.
type Session struct {
	mu      sync.Mutex
	tokens  Tokens
	account Account
	gen     uint64

	// refreshAt is when the access token stops being used, which is earlier
	// than its expiry by [effectiveSkew]. Zero means "no expiry is known", in
	// which case the token is used until the server rejects it.
	refreshAt time.Time

	onRotate func(Tokens)

	// client performs the refresh. It is the UNAUTHENTICATED client the
	// session was created from, so refreshing cannot recurse through the
	// bearer-token path.
	client *Client

	// now is injectable so expiry logic is testable without sleeping.
	now func() time.Time
}

// Resume builds a session from a refresh token persisted by a previous run.
//
// There is no access token yet, so the first authenticated call rotates. That
// is the intended shape: an access token's lifetime is minutes, so one stored
// across a restart is almost always dead anyway, and persisting it would mean
// writing a second credential to disk for no benefit.
func (c *Client) Resume(refreshToken string) *Session {
	return &Session{
		tokens: Tokens{RefreshToken: refreshToken},
		client: c,
		now:    time.Now,
	}
}

// AccessToken implements [TokenSource]. It refreshes proactively when the
// current token is within [effectiveSkew] of its stated expiry.
func (s *Session) AccessToken(ctx context.Context) (string, uint64, error) {
	s.mu.Lock()
	// Freshness is recomputed UNDER THE LOCK, which is what makes the
	// proactive path single-flight too: the second goroutine to arrive finds
	// the token the first one just fetched and does not refresh again.
	fresh := s.tokens.AccessToken != "" && (s.refreshAt.IsZero() || s.clock().Before(s.refreshAt))
	if fresh {
		token, generation := s.tokens.AccessToken, s.gen
		s.mu.Unlock()
		return token, generation, nil
	}

	rotated, err := s.refreshLocked(ctx)
	token, generation := s.tokens.AccessToken, s.gen
	cb, snapshot := s.onRotate, s.tokens
	s.mu.Unlock()

	if err != nil {
		return "", 0, err
	}
	if rotated && cb != nil {
		cb(snapshot)
	}
	return token, generation, nil
}

// Refresh implements [TokenSource].
//
// generation names the credential the caller was using when it got a 401. If
// the session has already moved past it, another goroutine did the work and
// this returns nil without touching the network — which is the whole reason the
// generation exists.
func (s *Session) Refresh(ctx context.Context, generation uint64) error {
	s.mu.Lock()
	if s.gen != generation {
		s.mu.Unlock()
		return nil
	}
	rotated, err := s.refreshLocked(ctx)
	cb, snapshot := s.onRotate, s.tokens
	s.mu.Unlock()

	if err != nil {
		return err
	}
	if rotated && cb != nil {
		cb(snapshot)
	}
	return nil
}

// refreshLocked redeems the refresh token. The caller holds s.mu.
func (s *Session) refreshLocked(ctx context.Context) (bool, error) {
	if s.tokens.RefreshToken == "" {
		return false, ErrNoSession
	}
	// A refresh token past the family's absolute end cannot work. Refusing here
	// rather than presenting it is not just a saved round trip: the family
	// lifetime is measured from when the login STARTED, so a token past it is
	// dead for a reason no retry can fix, and the caller needs to be told to
	// authenticate rather than to wait.
	if !s.tokens.RefreshExpiresAt.IsZero() && !s.clock().Before(s.tokens.RefreshExpiresAt) {
		s.tokens = Tokens{}
		s.refreshAt = time.Time{}
		return false, ErrSessionExpired
	}

	resp, err := s.client.refreshSession(ctx, s.tokens.RefreshToken)
	if err != nil {
		// A rejected refresh token is terminal. Wiping the session is the
		// important part: keeping a token the server has revoked would make
		// every subsequent call present a credential that, if the revocation
		// was a reuse detection, is now evidence of an attack.
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == 401 {
			s.tokens = Tokens{}
			s.refreshAt = time.Time{}
			return false, fmt.Errorf("%w: %w", ErrSessionExpired, err)
		}
		return false, err
	}

	s.applyLocked(resp)
	return true, nil
}

// applyLocked installs a session response. The caller holds s.mu.
func (s *Session) applyLocked(resp *SessionResponse) {
	now := s.clock()
	lifetime := time.Duration(resp.ExpiresIn) * time.Second

	s.tokens = Tokens{
		AccessToken:      resp.AccessToken,
		AccessExpiresAt:  now.Add(lifetime),
		RefreshToken:     resp.RefreshToken,
		RefreshExpiresAt: resp.RefreshExpiresAt,
	}
	// Expiry is measured from the LOCAL clock at the moment the response
	// arrived, using the server's relative `expires_in`, rather than from an
	// absolute instant the server chose. A client whose clock is minutes off
	// would otherwise treat a live token as long dead, or a dead one as live —
	// and the relative form is immune to that by construction.
	if lifetime > 0 {
		s.refreshAt = now.Add(lifetime - effectiveSkew(lifetime))
	} else {
		s.refreshAt = time.Time{}
	}

	s.account = resp.Account
	s.gen++
}

func (s *Session) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// Tokens returns a snapshot for persistence.
//
// Call it from [Session.OnRotate] rather than polling: the refresh token
// changes on every rotation, and a stored copy that is one rotation behind is
// not merely stale — presenting it looks like reuse and revokes the family.
func (s *Session) Tokens() Tokens {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens
}

// Account returns the profile the server last reported with a session
// response. It is a snapshot, not a live view; call [Client.Account] for the
// current state.
func (s *Session) Account() Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.account
}

// OnRotate registers a callback invoked after every successful rotation, with
// the new credential.
//
// This is the hook for persisting the refresh token. It is called WITHOUT the
// session lock held, so the callback may call back into the session without
// deadlocking; it is called on whichever goroutine performed the rotation, so
// it should be quick and must not panic.
func (s *Session) OnRotate(fn func(Tokens)) {
	s.mu.Lock()
	s.onRotate = fn
	s.mu.Unlock()
}

// Logout revokes the whole login family and clears the session.
//
// The local state is cleared whatever the server says. A logout that failed
// because the family was already revoked, or because the network was down, must
// still leave this process holding no credential — the alternative is a
// "logged out" client that carries a working token.
func (s *Session) Logout(ctx context.Context) error {
	s.mu.Lock()
	refresh := s.tokens.RefreshToken
	s.tokens = Tokens{}
	s.refreshAt = time.Time{}
	s.account = Account{}
	s.gen++
	client := s.client
	s.mu.Unlock()

	if refresh == "" {
		return nil
	}
	return client.logout(ctx, refresh)
}

// staticSource is a fixed bearer token: no refresh, no rotation.
type staticSource string

func (s staticSource) AccessToken(context.Context) (string, uint64, error) {
	return string(s), 0, nil
}

// Refresh cannot do anything for a static token, and saying so is better than
// silently returning a token the server has already rejected.
func (staticSource) Refresh(context.Context, uint64) error { return ErrSessionExpired }

// StaticToken returns a [TokenSource] that always presents token.
//
// It is for a caller who obtained an access token elsewhere — a test, a
// short-lived script, an operator with a token pasted from somewhere. It cannot
// refresh, so when the token expires every call returns [ErrUnauthenticated];
// anything long-running wants a [Session] instead.
func StaticToken(token string) TokenSource { return staticSource(token) }
