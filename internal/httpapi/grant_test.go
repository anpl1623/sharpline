package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/betting"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// grantReq dispatches a top-up through the real route table.
//
// There is deliberately no test here that an UNAUTHENTICATED top-up is refused.
// That property is asserted on the route table by
// TestPrivateRoutesRequireAuthentication, which already covers every route under
// /v1/account, and it is asserted better there: it checks where the decision is
// actually made rather than checking one handler's behaviour and leaving the
// next one written without the reflex.
func grantReq(t *testing.T, d *deps, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{}
	if key != "" {
		headers[idempotencyHeader] = key
	}
	return callAuthed(t, d, http.MethodPost, "/v1/account/grant", body, headers)
}

// TestGrantRequiresAnIdempotencyKey.
//
// The ledger transaction's id is derived from the key, so a keyless top-up has
// an at-least-once path into the one operation that CREATES money. It must be
// refused before it reaches the service, and with a 400 rather than a 422
// because the fault is in the request's framing.
func TestGrantRequiresAnIdempotencyKey(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"", "   "} {
		t.Run(fmt.Sprintf("key=%q", key), func(t *testing.T) {
			t.Parallel()
			d := newDeps()

			rec := grantReq(t, d, `{"amount_minor":5000}`, key)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if d.betting.grantCalls != 0 {
				t.Errorf("the grant service was called %d times; a keyless top-up must not reach it",
					d.betting.grantCalls)
			}
		})
	}
}

// TestGrantStatusDistinguishesFirstFromReplay is the idempotency contract at the
// HTTP layer, and it matters more here than on placement: a client that retried
// a top-up after a timeout learns from the status line whether it was already
// credited, rather than topping up again to be sure.
func TestGrantStatusDistinguishesFirstFromReplay(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		replayed bool
		want     int
	}{
		{"first submit", false, http.StatusCreated},
		{"replay", true, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDeps()
			d.betting.grant = betting.Grant{
				TransactionID: domain.TransactionID("txn_1"),
				Amount:        domain.Money(5_000),
				Balance:       domain.Money(12_500),
				OccurredAt:    time.Now().UTC(),
				Replayed:      tc.replayed,
			}

			rec := grantReq(t, d, `{"amount_minor":5000}`, "key-1")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}

			body := decodeJSONBody[gen.GrantResponse](t, rec)
			if body.Replayed != tc.replayed {
				t.Errorf("replayed = %v, want %v", body.Replayed, tc.replayed)
			}
			// Money crosses the wire as an integer number of minor units, never
			// as a float and never as a major-unit string (CLAUDE.md §12).
			if body.AmountMinor != 5_000 {
				t.Errorf("amount_minor = %d, want 5000", body.AmountMinor)
			}
			if body.BalanceMinor != 12_500 {
				t.Errorf("balance_minor = %d, want 12500", body.BalanceMinor)
			}
			if body.TransactionId != "txn_1" {
				t.Errorf("transaction_id = %q, want txn_1", body.TransactionId)
			}
		})
	}
}

// TestGrantPassesTheAmountThrough asserts the wire field reaches the field it is
// supposed to reach, in minor units, unscaled.
//
// A handler that divided or multiplied here would be a rounding bug in the one
// place CLAUDE.md §12 forbids one outright.
func TestGrantPassesTheAmountThrough(t *testing.T) {
	t.Parallel()

	d := newDeps()
	rec := grantReq(t, d, `{"amount_minor":123456}`, "key-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if got := d.betting.lastGrant.Amount; got != domain.Money(123_456) {
		t.Errorf("the service received %d minor units, want 123456", got.MinorUnits())
	}
	if d.betting.lastGrant.IdempotencyKey != "key-1" {
		t.Errorf("the service received key %q, want key-1", d.betting.lastGrant.IdempotencyKey)
	}
}

// TestGrantSentinelMapping is the table this layer exists to get right: which
// refusal earns which status and which code.
//
// A self-exclusion reported as a 500 would look like an outage; a limit breach
// reported as a 403 would look like the account was blocked. The distinctions
// are the response, so they are asserted one by one.
func TestGrantSentinelMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		wantCode int
		wantErr  gen.ErrorCode
	}{
		{
			name:     "self excluded",
			err:      fmt.Errorf("betting: %w", betting.ErrSelfExcluded),
			wantCode: http.StatusForbidden,
			wantErr:  gen.ErrorCodeSelfExcluded,
		},
		{
			name:     "suspended or closed",
			err:      fmt.Errorf("betting: %w", betting.ErrAccountNotWagerable),
			wantCode: http.StatusForbidden,
			wantErr:  gen.ErrorCodeAccountNotActive,
		},
		{
			name:     "a limit would be breached",
			err:      fmt.Errorf("betting: %w", betting.ErrLimitExceeded),
			wantCode: http.StatusUnprocessableEntity,
			wantErr:  gen.ErrorCodeLimitExceeded,
		},
		{
			name:     "an invalid amount",
			err:      fmt.Errorf("betting: %w", betting.ErrInvalidGrantAmount),
			wantCode: http.StatusUnprocessableEntity,
			wantErr:  gen.ErrorCodeInvalidGrantAmount,
		},
		{
			// Anything unrecognised is an outage, not a customer error, and
			// must not leak the error's text.
			name:     "an unrecognised failure",
			err:      errors.New("the database fell over"),
			wantCode: http.StatusInternalServerError,
			wantErr:  gen.ErrorCodeInternal,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDeps()
			d.betting.grantErr = tc.err

			rec := grantReq(t, d, `{"amount_minor":5000}`, "key-1")
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			body := decodeJSONBody[gen.Error](t, rec)
			if body.Code != tc.wantErr {
				t.Errorf("code = %q, want %q", body.Code, tc.wantErr)
			}
			// respond.go's rule: nothing derived from an error value reaches
			// the wire. The internal case is the one that would leak.
			if body.Message == tc.err.Error() {
				t.Errorf("the error's own text reached the response body: %q", body.Message)
			}
		})
	}
}
