// Package client is the Go SDK for the Sharpline REST API (CLAUDE.md §8).
//
// Sharpline is a PLAY-MONEY SIMULATION of a sportsbook, not a licensed
// sportsbook. No real money moves through this API. There is no deposit, no
// withdrawal, no payment instrument, no identity verification and no
// geolocation gating, and there is no method on this client that implies any of
// them.
//
// # Getting started
//
//	c, err := client.New(client.Options{BaseURL: "https://sharpline.example"})
//	if err != nil {
//	    return err
//	}
//
//	// The catalogue, the board, line history and search are public.
//	board, err := c.Board(ctx, client.GetBoardParams{})
//
//	// Account calls need a session.
//	sess, err := c.Login(ctx, client.Credentials{Email: email, Password: password})
//	auth := c.WithSession(sess)
//	balance, err := auth.Balance(ctx)
//
// A complete, runnable program is in the package example.
//
// # How this package is put together
//
// The wire types — [Board], [Event], [Price], [SessionResponse] and everything
// else in types.gen.go — are GENERATED from internal/httpapi/openapi.yaml,
// which is the API's contract and is authored by hand. The client itself, the
// typed errors, the retry policy and the session are hand-written, because the
// ergonomics are the deliverable and a generated client has no concept of a
// rotating credential.
//
// The generated types are duplicated rather than shared with the server: Go
// forbids importing an `internal/` package from outside its module, so an SDK
// whose types lived in internal/httpapi/gen would be an SDK nobody outside this
// repository could compile against. pkg/client/oapi-codegen.yaml carries the
// full argument.
//
// # Errors
//
// Every non-2xx answer becomes an [*APIError] carrying the status, the server's
// stable machine-readable code, and the request id. Match it with errors.Is
// against the package sentinels rather than comparing status codes:
//
//	_, err := c.Login(ctx, creds)
//	switch {
//	case errors.Is(err, client.ErrTOTPRequired):
//	    // prompt for a code and call Login again with Credentials.TOTPCode set
//	case errors.Is(err, client.ErrInvalidCredentials):
//	    // wrong email or wrong password — the API does not say which, and
//	    // neither does this SDK: telling them apart is account enumeration
//	case errors.Is(err, client.ErrRateLimited):
//	    // APIError.RetryAfter carries the server's advice
//	}
//
// # Sessions, rotation, and the one mistake this package exists to prevent
//
// Refresh tokens ROTATE: redeeming one invalidates it and issues a successor.
// Presenting an already-redeemed token is indistinguishable from theft, so the
// server revokes the ENTIRE login family and the user is logged out
// everywhere.
//
// Two consequences shape this package:
//
//   - A [Session] refreshes ONCE per expiry no matter how many goroutines
//     notice at the same moment. That is what [TokenSource]'s generation
//     parameter is for, and it is why the session holds its mutex across the
//     refresh round trip.
//
//   - No mutating request is ever retried, however transient the failure
//     looked. A retry of a refresh whose response was lost on the wire would
//     present the redeemed token a second time and revoke the family. Only GET
//     and HEAD are retried.
//
// Persist the refresh token from [Session.OnRotate], not by polling: a stored
// copy one rotation behind is not merely stale, presenting it looks like reuse.
//
// # Money
//
// Every amount is an int64 count of MINOR UNITS ([MoneyMinor]) and is a JSON
// number on the wire — never a float, never a string. The bound is 2^53-1,
// which is both the largest integer a float64 holds exactly and JavaScript's
// Number.MAX_SAFE_INTEGER, so the representation is lossless for every value
// that can exist. Use [FormatMinor] to display one; never do arithmetic in
// major units.
//
// Odds are float64 decimal odds, and decimal is canonical: American and
// fractional are lossy display formats, so [Price.DecimalOdds] is always
// present and the rendered `display` string is additional.
//
// # Concurrency and state
//
// A [Client] is immutable after construction and safe for concurrent use. A
// [Session] is safe for concurrent use and is the only thing in the package
// that holds mutable state. There are no package-level variables holding
// configuration, no environment variables are read, and there is NO LOGGER —
// an SDK that logs requests eventually logs an Authorization header. Wrap
// [Doer] if you want request logging, so the redaction decision is yours and
// visible.
package client
