package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/betting"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// The account surface: profile, derived play-money balance, and self-imposed
// responsible-gaming limits.
//
// EVERY HANDLER HERE ACTS ON THE TOKEN'S OWN USER AND ONLY ON THAT USER. There
// is no path parameter, no query parameter and no body field in this file that
// names a user id — [API.caller] is the only source — so "act on behalf of
// somebody else" is not expressible rather than merely forbidden. That is why
// none of these handlers contains an authorization check: there is nothing for
// one to compare.

func (a *API) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}
	profile, err := a.accounts.Profile(r.Context(), user)
	if err != nil {
		// A token that verifies but names a user who does not exist is not a
		// 404 — the resource is "my account" and the caller was authenticated —
		// it means the account was deleted under a live session, which is a
		// server-side inconsistency and is logged as one.
		a.notFoundOr(w, r, "account profile", err)
		return
	}
	respond(w, http.StatusOK, wireProfile(profile))
}

// handleGrant credits the caller's balance with play money.
//
// # Why this endpoint exists, and why a customer may call it for themselves
//
// It is the ONLY path by which money enters the system. Every other movement in
// the ledger is zero-sum between accounts that already hold a balance — a stake
// moves cash to escrow, a settlement moves escrow to cash or to the house — so
// without an issuance path every balance is permanently zero and the whole
// betting surface is unreachable. migration 00006 names the gap directly:
// "Money enters through EntryKindGrant, written by the application." This is the
// application writing it.
//
// It is not a deposit and must never be described as one. CLAUDE.md §0 is
// explicit that no real money moves and that there is no payment processing and
// no custody of funds; a grant is the play-money analogue, and the OpenAPI
// document says the same at the `grant` limit kind.
//
// The control on issuance is therefore not an authorisation check but the
// customer's OWN self-imposed `grant` limit, evaluated inside the transaction
// against the same ledger sum the limit is defined over, plus a hard
// per-request ceiling. That is the design the rest of the tree already
// committed to: auth.LimitKindGrant, user_limits' CHECK and
// internal/betting/limits.go all carry a 'grant' limit end to end, and a limit
// bounding something no customer could do would bound nothing.
//
// 201 when this request wrote the movement, 200 when a previous one already
// had, on the same reasoning as [API.handlePlaceWager]: a client that retried
// after a timeout learns from the status line whether its first attempt landed.
// The body reports what the ORIGINAL request credited, not what this one asked
// for.
func (a *API) handleGrant(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}

	key, ok := a.idempotencyKey(w, r)
	if !ok {
		return
	}

	var body gen.GrantRequest
	if err := decodeJSON(r, &body); err != nil {
		a.badBody(w, r, err)
		return
	}

	grant, err := a.betting.Grant(r.Context(), betting.GrantRequest{
		UserID:         user,
		Amount:         domain.Money(body.AmountMinor),
		IdempotencyKey: key,
		Audit:          a.auditContext(r).forBetting(),
	})
	if err != nil {
		a.failBetting(w, r, "grant play money", err)
		return
	}

	// The audit entry — `ledger.grant`, against the ledger transaction — is
	// written by internal/betting INSIDE the grant transaction, from the
	// provenance handed to Grant above, so the record of who minted the money
	// commits with the money. a.record is deliberately not called here; see
	// [API.handlePlaceWager] for why an after-the-fact write on another
	// connection is the wrong shape, and betting.Tx.RecordAudit for the full
	// argument.
	//
	// It matters more on this path than on any other. A grant is the only
	// operation in the system that CREATES value, and a doubled or
	// unattributed one leaves a ledger that still balances afterwards — so the
	// audit trail is the only place the anomaly would ever surface. A replay
	// writes no row, because it writes no money.
	status := http.StatusCreated
	if grant.Replayed {
		status = http.StatusOK
	}
	respond(w, status, gen.GrantResponse{
		TransactionId: grant.TransactionID.String(),
		AmountMinor:   grant.Amount.MinorUnits(),
		BalanceMinor:  grant.Balance.MinorUnits(),
		OccurredAt:    grant.OccurredAt,
		Replayed:      grant.Replayed,
	})
}

// handleGetBalance serves the derived play-money balance.
//
// # The balance is a fold, not a field
//
// CLAUDE.md §4: "Balances are derived, never stored as a mutable field." There
// is no balance column anywhere in the schema and migration 00006 makes that
// structural rather than conventional; this handler reads the `account_balances`
// VIEW, which is `sum(amount_minor) GROUP BY account`. A stored balance can be
// stale, and a bet slip validated against a stale balance is an overdraft.
//
// If the fold ever becomes too slow the answer is a MATERIALISED VIEW refreshed
// from the entries, never a column on `users`: a materialised view can be
// provably rebuilt from the ledger and a column cannot, and the property worth
// protecting is that the ledger remains the only place money exists.
//
// # Both accounts are always reported
//
// `cash` is spendable; `escrow` holds the stakes of open wagers, which have left
// the spendable balance and have not yet been won or lost. Reporting only cash
// would make a user with three open parlays look like they had lost the money.
// An account with no ledger movement is returned as zero with `entry_count: 0`
// rather than omitted — "never funded" and "funded and spent to zero" are
// different facts and entry_count is the only thing that distinguishes them.
func (a *API) handleGetBalance(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}

	balances, err := a.ledger.Balances(r.Context(), user)
	if err != nil {
		failWith(w, r, a.log, "account balance", err)
		return
	}

	out := gen.BalanceResponse{
		Currency: gen.PLAY,
		// 100 minor units per major, for DISPLAY only. Every arithmetic value in
		// this response is minor units; a client that divides is formatting, and
		// a client that divides and then does arithmetic has a rounding bug.
		MinorUnitsPerMajor: gen.N100,
		Cash:               gen.AccountBalance{AccountKind: gen.UserCash},
		Escrow:             gen.AccountBalance{AccountKind: gen.UserEscrow},
		AsOf:               a.now().UTC(),
	}

	var total domain.Money
	for _, b := range balances {
		switch b.Kind {
		case domain.AccountKindUserCash:
			out.Cash.BalanceMinor = b.Amount.MinorUnits()
			out.Cash.EntryCount = b.Entries
		case domain.AccountKindUserEscrow:
			out.Escrow.BalanceMinor = b.Amount.MinorUnits()
			out.Escrow.EntryCount = b.Entries
		default:
			// The store filters to the two customer account kinds, so a house or
			// issuance row reaching here would mean the query changed underneath
			// this handler. Dropping it is right — it is emphatically not this
			// user's money — and it is logged so the divergence is findable.
			a.log.WarnContext(r.Context(), "unexpected account kind in user balance",
				"account_kind", b.Kind.String())
			continue
		}
		sum, err := total.Add(b.Amount)
		if err != nil {
			// Both operands are inside the database's own CHECK bounds
			// (±2^53−1), so their sum can only overflow if the ledger itself is
			// corrupt. Reporting a wrong total would be worse than reporting
			// nothing.
			failWith(w, r, a.log, "account balance: total", err)
			return
		}
		total = sum
	}
	out.TotalMinor = total.MinorUnits()

	respond(w, http.StatusOK, out)
}

// handleSelfExclude stops the caller wagering, permanently.
//
// # The strongest control in §6, and the only irreversible one
//
// CLAUDE.md §6 asks for "responsible-gaming-style self-imposed limits".
// [API.handleSetLimit] is the amount-and-period half; this is the other half —
// the customer asking the system to stop them entirely. `users.status` has
// carried `self_excluded` since migration 00005 and the placement path has
// enforced it in three independent layers since 00008, but until this handler
// existed nothing could SET it: the control was implemented everywhere except
// where a customer could reach it.
//
// # Irreversibility is structural, not a policy this handler enforces
//
// The statement behind [Accounts.SelfExclude] can reach `self_excluded` and
// `closed` and no other status, and refuses a source that is already one of
// those. So there is no request to this endpoint, and no bug in this package,
// that can lift a self-exclusion — or an operator's suspension. That asymmetry
// is migration 00008's own: an operator may lift a suspension they imposed, and
// nobody may quietly book a bet for a customer who asked the system to stop
// them. Reinstatement gets its own statement, its own actor and its own audit
// action when the admin console is built. It does not get this one.
//
// # What "immediately" means, exactly
//
// After this returns, a placement is refused at all three layers: the betting
// service reads the status inside the placement transaction against a locked
// row, this package maps its sentinel to 403, and migration 00008's BEFORE
// INSERT trigger on `wagers` refuses the row for any writer that reached the
// table another way. There is NO CACHED STATUS anywhere in between —
// middleware.Identity carries the user id, the session id and the token's
// bounds and deliberately no status, so nothing has to be invalidated.
//
// ONE REQUEST CAN STILL COMPLETE AFTER THIS ONE STARTS, and it is stated rather
// than glossed: a placement that already holds the lock on the customer's users
// row finishes, and this call waits behind it. The window is exactly one
// in-flight placement transaction, it exists because that lock is what
// serialises the two, and it cannot admit a slip submitted after this handler
// has responded.
//
// # The session is deliberately NOT ended
//
// auth.UserStatus.CanAuthenticate stays true for this status and
// `password_changed_at` is untouched, so refresh-token families survive and the
// access token keeps working. Locking a customer out of their own history and
// balance is not a protection; it is an obstacle to the person the control is
// for. `GET /account` reports the new status so a client can render the state.
//
// Audited as `user.self_exclude`, INSIDE the same transaction as the change.
func (a *API) handleSelfExclude(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}

	var body gen.SelfExclusionRequest
	if err := decodeJSON(r, &body); err != nil {
		a.badBody(w, r, err)
		return
	}
	if body.Confirm != gen.SelfExclude {
		// The confirmation is the whole reason the body exists. An action this
		// final must not be reachable by an empty object or by a button wired
		// to the wrong path, so the check is explicit here rather than left to
		// the generated type's Valid() — which would also accept a value added
		// to the enum later.
		failInvalid(w, r, http.StatusUnprocessableEntity, gen.ErrorCodeUnprocessable, msgUnprocessable,
			[]gen.InvalidParam{{
				Name:   "confirm",
				Reason: `must be the literal string "self_exclude"`,
			}})
		return
	}

	profile, err := a.accounts.SelfExclude(r.Context(), SelfExclusion{
		UserID: user,
		Audit:  a.auditContext(r),
	})
	if err != nil {
		// A token that verifies but names a user who does not exist is the same
		// server-side inconsistency [API.handleGetAccount] describes, and is
		// answered the same way. Everything else is a 500: there is no
		// customer-fixable failure on this path, and in particular "you are
		// already self-excluded" is NOT one — the store reports that as success
		// with the profile, because it is the outcome the customer asked for.
		a.notFoundOr(w, r, "self-exclusion", err)
		return
	}

	respond(w, http.StatusOK, wireProfile(profile))
}

func (a *API) handleListLimits(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}
	limits, err := a.limits.Current(r.Context(), user)
	if err != nil {
		failWith(w, r, a.log, "list limits", err)
		return
	}

	now := a.now().UTC()
	out := gen.LimitPage{
		Data: make([]gen.Limit, 0, len(limits)),
		Page: singlePage(len(limits)),
	}
	for _, l := range limits {
		out.Data = append(out.Data, wireLimit(l, now))
	}
	respond(w, http.StatusOK, out)
}

// handleSetLimit records a new self-imposed limit.
//
// # The asymmetry is the control
//
// A limit a user can lift the instant they want to is not a limit. Tightening
// binds immediately; loosening — raising a money cap, lengthening a session cap,
// or removing a limit entirely — serves a cooling-off period, and the response
// reports the `effective_from` instant so the user can see when.
//
// THE ASYMMETRY IS DECIDED IN THE STORE, NOT HERE, and deliberately: the
// decision needs the current row and the new one under the same lock, and this
// handler cannot hold one. Reimplementing the comparison here would put two
// answers to "is this a loosening" in the tree, and the one that matters is the
// one the transaction uses.
//
// # Append-only
//
// The previous row is closed with `superseded_at` and a new one written; nothing
// is edited. "What limit was in force when this wager was accepted" must stay
// answerable, and an operator must not be able to make a customer's limit
// history disappear — the database refuses both an UPDATE of the substantive
// columns and a DELETE outright.
//
// Audited as `user_limit.set`, INSIDE the same transaction as the change.
func (a *API) handleSetLimit(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}

	var body gen.SetLimitRequest
	if err := decodeJSON(r, &body); err != nil {
		a.badBody(w, r, err)
		return
	}

	req, bad := a.parseSetLimit(user, body)
	if len(bad) > 0 {
		failInvalid(w, r, http.StatusUnprocessableEntity, gen.ErrorCodeUnprocessable, msgUnprocessable, bad)
		return
	}
	req.Audit = a.auditContext(r)

	limit, err := a.limits.Set(r.Context(), req)
	switch {
	case err == nil:
	case errors.Is(err, ErrConflict):
		// A concurrent request superseded the row this one read. The partial
		// unique index would refuse a second open row anyway; reporting 409
		// lets the client re-read and retry rather than discovering it as a
		// constraint violation.
		fail(w, r, http.StatusConflict, gen.ErrorCodeConflict, msgConflict)
		return
	default:
		failWith(w, r, a.log, "set limit", err)
		return
	}

	respond(w, http.StatusCreated, wireLimit(limit, a.now().UTC()))
}

// parseSetLimit validates the body against the same biconditionals the database
// enforces.
//
// Validating here as well as in the schema is not duplication for its own sake:
// a check violation from Postgres is a 500-shaped error with no field
// attribution, and a user who set a session limit in dollars deserves to be told
// which field was wrong. The database constraint remains the authority — it is
// what makes the impossible combination unstorable rather than merely rejected.
func (a *API) parseSetLimit(user domain.UserID, body gen.SetLimitRequest) (SetLimit, []gen.InvalidParam) {
	var bad badParams

	kind, err := auth.ParseLimitKind(string(body.Kind))
	if err != nil {
		bad.add("kind", "must be one of grant, stake, loss, session")
	}
	period, err := auth.ParseLimitPeriod(string(body.Period))
	if err != nil {
		bad.add("period", "must be one of day, week, month, session")
	}
	if bad.any() {
		return SetLimit{}, bad.items
	}

	if !auth.LimitPairValid(kind, period) {
		// The database's user_limits_session_period biconditional says the same
		// thing: (kind = 'session') = (period = 'session').
		bad.add("period", "a session limit takes the session period, and no other kind may use it")
		return SetLimit{}, bad.items
	}

	req := SetLimit{UserID: user, Kind: kind, Period: period}

	switch {
	case kind.IsMoney():
		if body.DurationSeconds != nil {
			bad.add("duration_seconds", "only a session limit is denominated in time")
		}
		if body.AmountMinor != nil {
			amount, err := domain.FromMinorUnits(*body.AmountMinor)
			if err != nil || !amount.IsPositive() {
				bad.add("amount_minor", "must be a positive integer number of minor units")
			} else {
				req.Amount = &amount
			}
		}
		// Both nil is legal and means REMOVE the limit, which is a loosening and
		// serves the cooling-off period like any other.

	default: // session
		if body.AmountMinor != nil {
			bad.add("amount_minor", "a session limit is denominated in time, not money")
		}
		if body.DurationSeconds != nil {
			d := time.Duration(*body.DurationSeconds) * time.Second
			if d <= 0 || d > 24*time.Hour {
				bad.add("duration_seconds", "must be between 1 and 86400")
			} else {
				req.Duration = &d
			}
		}
	}

	return req, bad.items
}

// wireProfile maps the profile onto the wire.
//
// `totp_enrolled` reports the CONFIRMED enrolment only. An enrolment that has
// been started and not proved is not a second factor, and reporting it as one
// would tell a user they are protected when a mis-scanned QR code means they are
// not — and would lock them out at the next login.
func wireProfile(p Profile) gen.Account {
	return gen.Account{
		Id:           p.ID.String(),
		Email:        p.Email,
		Status:       gen.AccountStatus(p.Status.String()),
		TotpEnrolled: p.TOTPConfirmed,
		CreatedAt:    p.CreatedAt.UTC(),
	}
}

func wireLimit(l Limit, now time.Time) gen.Limit {
	out := gen.Limit{
		Id:            l.ID,
		Kind:          gen.LimitKind(l.Kind.String()),
		Period:        gen.LimitPeriod(l.Period.String()),
		RequestedAt:   l.RequestedAt.UTC(),
		EffectiveFrom: l.EffectiveFrom.UTC(),
		// A pending loosening is returned with in_force false rather than
		// hidden: the user asked for it, it is going to happen, and the whole
		// point of the cooling-off period is that they can see it coming and
		// change their mind.
		InForce: !l.EffectiveFrom.After(now),
	}
	if l.Amount != nil {
		minor := l.Amount.MinorUnits()
		out.AmountMinor = &minor
	}
	if l.Duration != nil {
		secs := int32(*l.Duration / time.Second)
		out.DurationSeconds = &secs
	}
	return out
}

// badBody answers a malformed request body.
//
// The decoder's error is NOT echoed. encoding/json includes the offending JSON
// fragment in some of its messages, and a request body is the one place a client
// can put arbitrary bytes — reflecting them into a response is how an error
// envelope becomes an injection surface for whatever renders it.
func (a *API) badBody(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errBodyTooLarge) {
		fail(w, r, http.StatusRequestEntityTooLarge, gen.ErrorCodeBadRequest, msgBadRequest)
		return
	}
	a.log.DebugContext(r.Context(), "malformed request body", "error", err.Error())
	fail(w, r, http.StatusBadRequest, gen.ErrorCodeBadRequest, msgBadRequest)
}
