package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// POST /account/self-exclusion is the only irreversible control a customer can
// reach, so these assert the two things that make it that control rather than a
// button: that it CANNOT be triggered by accident, and that once it has fired
// the account's own read surface says so.
//
// What is deliberately NOT asserted here is that the exclusion binds. That is
// not this package's property to prove — the enforcement is a read of
// users.status inside internal/betting's placement transaction against a locked
// row, plus migration 00008's BEFORE INSERT trigger — and asserting it against a
// fake here would be asserting that the fake is consistent with itself. The real
// property is covered by internal/betting's placement tests and the integration
// tier.

func TestSelfExclusionRequiresTheLiteralConfirmation(t *testing.T) {
	t.Parallel()

	user := mustUserID(t, "usr_test")

	// Every one of these is a request a mis-wired button or a half-finished
	// client would send. None of them may change anything: the confirmation is
	// the whole reason the body exists.
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"empty object", `{}`, http.StatusUnprocessableEntity},
		{"empty confirmation", `{"confirm":""}`, http.StatusUnprocessableEntity},
		{"a different word", `{"confirm":"yes"}`, http.StatusUnprocessableEntity},
		{"the right word mis-cased", `{"confirm":"SELF_EXCLUDE"}`, http.StatusUnprocessableEntity},
		{"the endpoint's own name", `{"confirm":"self-exclusion"}`, http.StatusUnprocessableEntity},
		{"not an object", `"self_exclude"`, http.StatusBadRequest},
		{"an unknown field instead", `{"confirmed":"self_exclude"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDeps()
			d.accounts.profiles[user] = Profile{
				ID: user, Email: "a@b.test", Status: auth.UserStatusActive, CreatedAt: testNow,
			}
			api := d.api(t)

			rec := serveAuthed(t, api.handleSelfExclude, user, http.MethodPost,
				"/v1/account/self-exclusion", strings.NewReader(tc.body))

			requireStatus(t, rec, tc.want)
			if len(d.accounts.excluded) != 0 {
				t.Fatal("an unconfirmed request reached the store; this endpoint is not reversible")
			}
			if got := d.accounts.profiles[user].Status; got != auth.UserStatusActive {
				t.Errorf("status = %s after a refused request, want active", got)
			}
		})
	}
}

func TestSelfExclusionExcludesAndIsReportedByGetAccount(t *testing.T) {
	t.Parallel()

	user := mustUserID(t, "usr_test")
	d := newDeps()
	d.accounts.profiles[user] = Profile{
		ID: user, Email: "a@b.test", Status: auth.UserStatusActive, CreatedAt: testNow,
	}
	api := d.api(t)

	rec := serveAuthed(t, api.handleSelfExclude, user, http.MethodPost,
		"/v1/account/self-exclusion", strings.NewReader(`{"confirm":"self_exclude"}`))
	requireStatus(t, rec, http.StatusOK)

	body := decodeJSONBody[gen.Account](t, rec)
	if body.Status != gen.AccountStatusSelfExcluded {
		t.Errorf("response status = %s, want %s", body.Status, gen.AccountStatusSelfExcluded)
	}

	// The spec promises `GET /account` reports the new status so a client can
	// render the state, and that the token keeps working — self-exclusion is not
	// a logout. Both are one assertion: the same identity reads its own profile
	// back and sees the change.
	rec = serveAuthed(t, api.handleGetAccount, user, http.MethodGet, "/v1/account", nil)
	requireStatus(t, rec, http.StatusOK)
	if got := decodeJSONBody[gen.Account](t, rec).Status; got != gen.AccountStatusSelfExcluded {
		t.Errorf("GET /account status = %s after self-exclusion, want %s",
			got, gen.AccountStatusSelfExcluded)
	}
}

// TestSelfExclusionForwardsTheCallersOwnIdentity is about the one field a bug
// here would corrupt silently: the user id. Excluding the wrong account is
// unfixable through this API by design, so the handler must take the id from the
// verified identity and from nowhere else — there is no user field on the
// request schema, and there must never be one.
func TestSelfExclusionForwardsTheCallersOwnIdentity(t *testing.T) {
	t.Parallel()

	user := mustUserID(t, "usr_caller")
	d := newDeps()
	d.accounts.profiles[user] = Profile{
		ID: user, Email: "a@b.test", Status: auth.UserStatusActive, CreatedAt: testNow,
	}
	api := d.api(t)

	rec := serveAuthed(t, api.handleSelfExclude, user, http.MethodPost,
		"/v1/account/self-exclusion", strings.NewReader(`{"confirm":"self_exclude"}`))
	requireStatus(t, rec, http.StatusOK)

	if len(d.accounts.excluded) != 1 {
		t.Fatalf("store saw %d requests, want exactly 1", len(d.accounts.excluded))
	}
	req := d.accounts.excluded[0]
	if req.UserID != user {
		t.Errorf("store was asked to exclude %s, want the caller %s", req.UserID, user)
	}
	// The audit entry the store writes in the same transaction is stamped with
	// this provenance. An empty request id would make the trail unable to tie
	// the exclusion to the request that caused it.
	if req.Audit.At.IsZero() {
		t.Error("the audit context carried no instant")
	}
}

// TestSelfExclusionIsIdempotent covers the case the spec calls out explicitly: a
// customer who is already excluded gets 200 and their profile, not an error.
// They have the outcome they asked for, and a failure message on the one
// endpoint where somebody is least able to deal with one would be the worst
// available answer.
func TestSelfExclusionIsIdempotent(t *testing.T) {
	t.Parallel()

	user := mustUserID(t, "usr_test")
	d := newDeps()
	d.accounts.profiles[user] = Profile{
		ID: user, Email: "a@b.test", Status: auth.UserStatusSelfExcluded, CreatedAt: testNow,
	}
	api := d.api(t)

	rec := serveAuthed(t, api.handleSelfExclude, user, http.MethodPost,
		"/v1/account/self-exclusion", strings.NewReader(`{"confirm":"self_exclude"}`))
	requireStatus(t, rec, http.StatusOK)

	if got := decodeJSONBody[gen.Account](t, rec).Status; got != gen.AccountStatusSelfExcluded {
		t.Errorf("status = %s, want %s", got, gen.AccountStatusSelfExcluded)
	}
}

// TestSelfExclusionReportsAnUnknownUserAsNotFound: a token that verifies but
// names nobody is a server-side inconsistency, not the caller's fault, and it is
// answered exactly as GET /account answers it rather than as a 500.
func TestSelfExclusionReportsAnUnknownUserAsNotFound(t *testing.T) {
	t.Parallel()

	d := newDeps() // no profile for anybody
	api := d.api(t)

	rec := serveAuthed(t, api.handleSelfExclude, mustUserID(t, "usr_ghost"), http.MethodPost,
		"/v1/account/self-exclusion", strings.NewReader(`{"confirm":"self_exclude"}`))
	requireStatus(t, rec, http.StatusNotFound)
}

// TestSelfExclusionSurfacesAStoreFailure. There is no customer-fixable failure
// on this path, so anything that is not ErrNotFound is a 500 — and it must be
// one, because a customer told "done" whose exclusion did not commit is the
// worst outcome this endpoint has.
func TestSelfExclusionSurfacesAStoreFailure(t *testing.T) {
	t.Parallel()

	user := mustUserID(t, "usr_test")
	d := newDeps()
	d.accounts.profiles[user] = Profile{
		ID: user, Email: "a@b.test", Status: auth.UserStatusActive, CreatedAt: testNow,
	}
	d.accounts.err = errors.New("connection reset")
	api := d.api(t)

	rec := serveAuthed(t, api.handleSelfExclude, user, http.MethodPost,
		"/v1/account/self-exclusion", strings.NewReader(`{"confirm":"self_exclude"}`))
	requireStatus(t, rec, http.StatusInternalServerError)
}

// TestSelfExclusionNeverWidensAStatus guards the store contract this package
// relies on from the outside. The handler passes no destination — SelfExclusion
// has no status field, deliberately — so there is no request that can reinstate
// an account, and a suspended one that self-excludes must not come back as
// active.
func TestSelfExclusionNeverWidensAStatus(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		start auth.UserStatus
		want  auth.UserStatus
	}{
		{"active narrows", auth.UserStatusActive, auth.UserStatusSelfExcluded},
		{"suspended narrows", auth.UserStatusSuspended, auth.UserStatusSelfExcluded},
		{"already excluded stays", auth.UserStatusSelfExcluded, auth.UserStatusSelfExcluded},
		// Reported honestly as `closed` rather than as the status it did not
		// reach. A closed account cannot present a token at all
		// (auth.UserStatus.CanAuthenticate is false for it), so this is
		// hypothetical — but a store that answered `self_excluded` here would be
		// claiming a transition that did not happen.
		{"closed stays closed", auth.UserStatusClosed, auth.UserStatusClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := mustUserID(t, "usr_test")
			d := newDeps()
			d.accounts.profiles[user] = Profile{
				ID: user, Email: "a@b.test", Status: tc.start, CreatedAt: testNow,
			}
			api := d.api(t)

			rec := serveAuthed(t, api.handleSelfExclude, user, http.MethodPost,
				"/v1/account/self-exclusion", strings.NewReader(`{"confirm":"self_exclude"}`))
			requireStatus(t, rec, http.StatusOK)

			if got := d.accounts.profiles[user].Status; got != tc.want {
				t.Errorf("status went %s → %s, want %s", tc.start, got, tc.want)
			}
		})
	}
}
