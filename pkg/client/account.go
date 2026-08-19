package client

import (
	"context"
	"net/http"
)

// Account returns the authenticated user's profile.
//
// There is no name, address, date of birth, document, country or jurisdiction
// field, and there never will be: Sharpline is a play-money simulation, not a
// licensed sportsbook, and no such column exists in the schema.
func (c *Client) Account(ctx context.Context) (*Account, error) {
	var out Account
	err := c.do(ctx, call{
		op:     "GET /account",
		method: http.MethodGet,
		path:   "/account",
		auth:   true,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Balance returns the derived play-money balances.
//
// # Money is integer minor units, everywhere, including here
//
// Every amount is an int64 count of minor units — [MoneyMinor] — and never a
// float and never a string. Divide by
// [BalanceResponse.MinorUnitsPerMajor] only to DISPLAY a value; doing
// arithmetic in major units reintroduces exactly the rounding error the integer
// representation exists to prevent. [FormatMinor] does the display conversion
// without floating point.
//
// The balance is a FOLD OVER THE LEDGER, not a stored field, so
// [AccountBalance.EntryCount] of zero means "this account has never moved" —
// which is a different fact from "moved and nets to zero" and is the only thing
// that distinguishes them.
func (c *Client) Balance(ctx context.Context) (*BalanceResponse, error) {
	var out BalanceResponse
	err := c.do(ctx, call{
		op:     "GET /account/balance",
		method: http.MethodGet,
		path:   "/account/balance",
		auth:   true,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Limits returns the self-imposed responsible-gaming limits currently in
// force.
//
// Superseded rows are history and are not returned. There is no deposit limit
// and there will not be one — this system has no deposits; the play-money
// analogue is [LimitKindGrant].
func (c *Client) Limits(ctx context.Context) (*LimitPage, error) {
	var out LimitPage
	err := c.do(ctx, call{
		op:     "GET /account/limits",
		method: http.MethodGet,
		path:   "/account/limits",
		auth:   true,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetLimit sets or changes a self-imposed limit.
//
// # Tightening binds immediately; loosening serves a cooling-off period
//
// That asymmetry is the entire control: a limit a user can lift the instant
// they want to is not a limit. Always read [Limit.EffectiveFrom] on the
// response — for a loosening it is in the future, and [Limit.InForce] is false
// until it passes. Treating the call as "the limit is now X" is wrong half the
// time.
//
// Exactly one of AmountMinor and DurationSeconds is set, decided by Kind:
// [LimitKindSession] takes a duration, every other kind takes an amount.
// Omitting both REMOVES the limit, which is a loosening and serves the
// cooling-off period like any other.
//
// A 409 ([ErrConflict]) means a concurrent request superseded the row this one
// was based on. Re-read [Client.Limits] and decide again; this SDK does not
// retry it, because the right resubmission depends on what the other change
// was.
func (c *Client) SetLimit(ctx context.Context, req SetLimitRequest) (*Limit, error) {
	var out Limit
	err := c.do(ctx, call{
		op:     "POST /account/limits",
		method: http.MethodPost,
		path:   "/account/limits",
		body:   req,
		auth:   true,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// BeginTOTPEnrolment mints a second factor and returns its provisioning URI.
//
// # The URI is credential material and is returned exactly once
//
// [TOTPEnrolment.ProvisioningUri] embeds the shared secret. Anyone holding it
// can mint valid codes forever. Show it to the user, let them scan it, and do
// not log it, persist it, put it in a URL or attach it to a span. The server
// stores only AEAD ciphertext and cannot show it again; a lost enrolment is
// restarted, not recovered.
//
// The factor is NOT active until [Client.ConfirmTOTPEnrolment] proves a code
// from it. That two-step shape is what stops a mis-scanned QR code from locking
// the account out.
func (c *Client) BeginTOTPEnrolment(ctx context.Context) (*TOTPEnrolment, error) {
	var out TOTPEnrolment
	err := c.do(ctx, call{
		op:     "POST /account/totp",
		method: http.MethodPost,
		path:   "/account/totp",
		auth:   true,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ConfirmTOTPEnrolment proves a code and activates the second factor. From
// then on [Client.Login] requires [Credentials.TOTPCode].
func (c *Client) ConfirmTOTPEnrolment(ctx context.Context, code string) (*Account, error) {
	var out Account
	err := c.do(ctx, call{
		op:     "POST /account/totp/confirm",
		method: http.MethodPost,
		path:   "/account/totp/confirm",
		body:   TOTPCodeRequest{Code: code},
		auth:   true,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveTOTP removes the second factor.
//
// It requires a currently-valid code from the factor being removed, so a stolen
// access token alone cannot strip 2FA off an account.
func (c *Client) RemoveTOTP(ctx context.Context, code string) error {
	return c.do(ctx, call{
		op:     "DELETE /account/totp",
		method: http.MethodDelete,
		path:   "/account/totp",
		body:   TOTPCodeRequest{Code: code},
		auth:   true,
	})
}
