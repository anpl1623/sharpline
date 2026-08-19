package client

import (
	"context"
	"net/http"
	"time"
)

// Register creates an account and opens a session.
//
// The password must be at least 12 characters; the server imposes no
// composition rules, because length beats character classes and composition
// rules push people toward `Password1!` and toward reuse.
//
// An email already in use is reported as [ErrAlreadyExists]. That is a
// deliberate difference from login, which never reveals whether an account
// exists: registration cannot avoid saying so and remain usable, whereas login
// can and must.
func (c *Client) Register(ctx context.Context, creds Credentials) (*Session, error) {
	var resp SessionResponse
	err := c.do(ctx, call{
		op:     "POST /auth/register",
		method: http.MethodPost,
		path:   "/auth/register",
		body:   RegisterRequest{Email: creds.Email, Password: creds.Password},
		out:    &resp,
	})
	if err != nil {
		return nil, err
	}
	return c.newSession(&resp), nil
}

// Login authenticates and opens a session.
//
// # Handling the second factor
//
// Attempt the login without [Credentials.TOTPCode] first. If the account has a
// confirmed second factor the server answers [ErrTOTPRequired]; prompt, then
// call Login again with the code set. Asking every user for a code up front
// would tell an attacker which accounts have 2FA enabled, which is exactly the
// enumeration the server's error design avoids.
//
// A wrong email and a wrong password are BOTH [ErrInvalidCredentials], and the
// server makes them indistinguishable in body, status and timing. This SDK
// preserves that: there is nothing in the returned error that separates them,
// and a caller must not try to infer it.
func (c *Client) Login(ctx context.Context, creds Credentials) (*Session, error) {
	body := LoginRequest{Email: creds.Email, Password: creds.Password}
	if creds.TOTPCode != "" {
		code := creds.TOTPCode
		body.TotpCode = &code
	}

	var resp SessionResponse
	err := c.do(ctx, call{
		op:     "POST /auth/login",
		method: http.MethodPost,
		path:   "/auth/login",
		body:   body,
		out:    &resp,
	})
	if err != nil {
		return nil, err
	}
	return c.newSession(&resp), nil
}

// newSession wraps a session response.
func (c *Client) newSession(resp *SessionResponse) *Session {
	s := &Session{client: c, now: time.Now}
	s.applyLocked(resp)
	return s
}

// refreshSession redeems a refresh token for a new pair.
//
// It is unexported and takes the token as an argument rather than reading it
// from a session, so there is exactly one caller — [Session.refreshLocked],
// which holds the session lock. That is what makes the single-flight guarantee
// hold: a rotation cannot be started from anywhere else, so it cannot race with
// itself.
//
// It is also never retried. [call.safe] explains why at length; the short
// version is that a second presentation of a rotated token is indistinguishable
// from theft and revokes the entire login family.
func (c *Client) refreshSession(ctx context.Context, refreshToken string) (*SessionResponse, error) {
	var resp SessionResponse
	err := c.do(ctx, call{
		op:     "POST /auth/refresh",
		method: http.MethodPost,
		path:   "/auth/refresh",
		body:   RefreshRequest{RefreshToken: refreshToken},
		out:    &resp,
	})
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// logout revokes a login family. Call [Session.Logout] rather than this.
//
// The endpoint takes the refresh token rather than a bearer token because
// revoking a family must work when the access token has already expired —
// which is most of the time a user clicks "log out".
func (c *Client) logout(ctx context.Context, refreshToken string) error {
	return c.do(ctx, call{
		op:     "POST /auth/logout",
		method: http.MethodPost,
		path:   "/auth/logout",
		body:   RefreshRequest{RefreshToken: refreshToken},
	})
}
