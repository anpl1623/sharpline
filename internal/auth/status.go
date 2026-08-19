package auth

import "fmt"

// The four value sets migrations/00005 delegated to this package.
//
// That migration's header names them explicitly and states the obligation:
//
//	"Phase 5 (internal/auth) MUST define matching Go constants with String() /
//	 ParseX() pairs producing exactly these lowercase spellings, the same way
//	 internal/domain does for WagerStatus and EntryKind. Until then this file is
//	 the single source of truth for them."
//
// So the spellings below are copied from the CHECK constraints character for
// character, and status_test.go asserts that every constant round-trips through
// String/Parse. The database is TEXT + CHECK rather than a native ENUM (00005
// explains why), which means the only thing stopping a typo from becoming a
// 23514 at runtime is this file agreeing with that one.

// UserStatus is the lifecycle state of an account: users.status, constrained by
// users_status_defined to active | suspended | self_excluded | closed.
type UserStatus uint8

const (
	// UserStatusUnknown is the invalid zero value. There is no default: a
	// status that was never set must not silently read as "active", because
	// "active" is the one value that permits everything.
	UserStatusUnknown UserStatus = iota

	// UserStatusActive is the normal state. Login permitted, wagering
	// permitted.
	UserStatusActive

	// UserStatusSuspended is an OPERATOR action. Login is refused — but only
	// after the password has been verified, so the refusal cannot be used to
	// discover that an account exists.
	UserStatusSuspended

	// UserStatusSelfExcluded is the responsible-gaming state (CLAUDE.md §6:
	// "responsible-gaming-style self-imposed limits (a nod to how the real
	// domain works)").
	//
	// LOGIN IS PERMITTED AND THAT IS DELIBERATE. A self-excluded customer must
	// still be able to sign in to read their wager history, see their limits,
	// and manage the exclusion itself. Locking them out would make the tool
	// that exists to protect them a punishment for using it, and — practically
	// — a customer who cannot see their own record cannot dispute it.
	//
	// What is blocked is WAGERING, and see the note on [UserStatus.CanWager]
	// for where that block has to live to be real.
	UserStatusSelfExcluded

	// UserStatusClosed is a terminated account. Login refused, after
	// verification, exactly as for suspended.
	UserStatusClosed
)

// String implements fmt.Stringer and produces the exact spelling stored in
// users.status.
func (s UserStatus) String() string {
	switch s {
	case UserStatusActive:
		return "active"
	case UserStatusSuspended:
		return "suspended"
	case UserStatusSelfExcluded:
		return "self_excluded"
	case UserStatusClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// Valid reports whether s is one of the four defined statuses.
func (s UserStatus) Valid() bool {
	switch s {
	case UserStatusActive, UserStatusSuspended, UserStatusSelfExcluded, UserStatusClosed:
		return true
	default:
		return false
	}
}

// CanAuthenticate reports whether a correct password may open a session.
//
// True for active and self-excluded; false for suspended and closed. The check
// is applied AFTER password verification in [Service.Login] — see
// [ErrAccountSuspended].
func (s UserStatus) CanAuthenticate() bool {
	return s == UserStatusActive || s == UserStatusSelfExcluded
}

// CanWager reports whether this status permits placing a wager. True only for
// active.
//
// # Where the self-exclusion check has to live, and why not anywhere else
//
// This phase's brief says self_excluded "must genuinely block wagering, not
// merely hide a button". Three placements were considered:
//
//  1. Hide the bet slip in the frontend. Not a control at all — the REST
//     endpoint is still there and curl still exists.
//
//  2. Carry the status as a JWT claim and check it in HTTP middleware. This is
//     the tempting one and it is WRONG, subtly. A JWT is a snapshot: it is
//     minted at login and refresh and is not re-read from the database in
//     between. A customer who self-excludes at 14:00 holds an access token
//     minted at 13:55 that still says "active", and every request that token
//     authorises until it expires would pass the middleware. The window is
//     exactly the access-token lifetime, and the ONE moment a
//     responsible-gaming control matters most is the minutes right after
//     somebody decides to use it. That is why status is deliberately NOT a
//     claim — see jwt.go, which lists what the claims carry and what they do
//     not.
//
//  3. Read users.status inside the transaction that writes the wager. This is
//     the placement, and it is the only one with no window: the row is read and
//     the wager is inserted under one snapshot, so an exclusion that commits
//     first is seen, and one that commits after was not in force when the
//     wager was accepted. Either outcome is defensible to the customer, which
//     is the actual requirement.
//
// So: internal/betting MUST call [Service.AuthorizeWagering] — or, inside a
// transaction it already holds, a [StatusReader] built over that transaction —
// before it inserts a wager. It is stated here rather than only in prose
// because this is the function that says no.
func (s UserStatus) CanWager() bool { return s == UserStatusActive }

// ParseUserStatus is the inverse of String for the defined statuses.
func ParseUserStatus(s string) (UserStatus, error) {
	switch s {
	case "active":
		return UserStatusActive, nil
	case "suspended":
		return UserStatusSuspended, nil
	case "self_excluded":
		return UserStatusSelfExcluded, nil
	case "closed":
		return UserStatusClosed, nil
	default:
		return UserStatusUnknown, fmt.Errorf("%w: user status %q", ErrInvalid, sample(s))
	}
}

// MarshalText implements encoding.TextMarshaler.
func (s UserStatus) MarshalText() ([]byte, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("%w: user status %d", ErrInvalid, uint8(s))
	}
	return []byte(s.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *UserStatus) UnmarshalText(b []byte) error {
	parsed, err := ParseUserStatus(string(b))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// RevokedReason is why a refresh-token family was ended:
// refresh_token_families.revoked_reason, constrained by
// refresh_token_families_revoked_reason_defined to
// logout | reuse_detected | credential_change | operator.
//
// The set is CLOSED and it is closed in a way that constrains the design. There
// is no 'expired' value, so an absolute session lifetime cannot be recorded as
// a revocation — which is why [Options.SessionLifetime] is enforced by CAPPING
// each successor's expires_at at started_at + lifetime rather than by revoking
// the family when it ages out. See refresh.go. Inventing a reason the CHECK
// would reject, or filing an expiry under 'operator', were both rejected: the
// second is a lie in an audit trail, and "how many sessions did we kill for
// token reuse this week" is only answerable if the reasons mean what they say.
type RevokedReason uint8

const (
	// RevokedReasonUnknown is the invalid zero value. A revocation always
	// carries a reason — refresh_token_families_revocation_complete makes the
	// biconditional a database invariant — so there is nothing for a zero value
	// to mean.
	RevokedReasonUnknown RevokedReason = iota

	// RevokedReasonLogout is the user ending their own session.
	RevokedReasonLogout

	// RevokedReasonReuseDetected is an already-redeemed token being presented
	// again. This is the value that matters: its rate is the only signal that
	// says a token was stolen.
	RevokedReasonReuseDetected

	// RevokedReasonCredentialChange is every family that predates a password
	// change. users.password_changed_at makes "log out everywhere" a
	// comparison rather than a scan (migrations/00005), and this is the reason
	// stamped on the families that comparison condemns.
	RevokedReasonCredentialChange

	// RevokedReasonOperator is an administrative revocation.
	RevokedReasonOperator
)

// String implements fmt.Stringer and produces the exact stored spelling.
func (r RevokedReason) String() string {
	switch r {
	case RevokedReasonLogout:
		return "logout"
	case RevokedReasonReuseDetected:
		return "reuse_detected"
	case RevokedReasonCredentialChange:
		return "credential_change"
	case RevokedReasonOperator:
		return "operator"
	default:
		return "unknown"
	}
}

// Valid reports whether r is one of the four defined reasons.
func (r RevokedReason) Valid() bool {
	switch r {
	case RevokedReasonLogout, RevokedReasonReuseDetected,
		RevokedReasonCredentialChange, RevokedReasonOperator:
		return true
	default:
		return false
	}
}

// ParseRevokedReason is the inverse of String for the defined reasons.
func ParseRevokedReason(s string) (RevokedReason, error) {
	switch s {
	case "logout":
		return RevokedReasonLogout, nil
	case "reuse_detected":
		return RevokedReasonReuseDetected, nil
	case "credential_change":
		return RevokedReasonCredentialChange, nil
	case "operator":
		return RevokedReasonOperator, nil
	default:
		return RevokedReasonUnknown, fmt.Errorf("%w: revoked reason %q", ErrInvalid, sample(s))
	}
}

// MarshalText implements encoding.TextMarshaler.
func (r RevokedReason) MarshalText() ([]byte, error) {
	if !r.Valid() {
		return nil, fmt.Errorf("%w: revoked reason %d", ErrInvalid, uint8(r))
	}
	return []byte(r.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (r *RevokedReason) UnmarshalText(b []byte) error {
	parsed, err := ParseRevokedReason(string(b))
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}

// LimitKind is what a self-imposed responsible-gaming limit constrains:
// user_limits.kind, constrained by user_limits_kind_defined to
// grant | stake | loss | session.
//
// Three of the four spellings are chosen to equal internal/domain's EntryKind
// spellings exactly. migrations/00005 says why: "so that enforcing a limit is a
// sum over ledger_entries filtered by the same string, with no translation
// table in between." Keep them equal.
//
// There is no 'deposit' kind, and its absence is load-bearing rather than
// incidental — CLAUDE.md §0 rules out payment processing and custody of funds,
// so a deposit limit would be a control that can never fire and an invitation
// to build the flow §0 forbids. 'grant' is the play-money analogue.
type LimitKind uint8

const (
	// LimitKindUnknown is the invalid zero value.
	LimitKindUnknown LimitKind = iota

	// LimitKindGrant caps play-money grants in the period. Evaluated as the sum
	// of ledger_entries.amount_minor with kind='grant' against the user's cash
	// account.
	LimitKindGrant

	// LimitKindStake caps total staked in the period: the sum of
	// wagers.stake_minor over wagers placed in it.
	LimitKindStake

	// LimitKindLoss caps net loss in the period: the net of ledger entries
	// against the user's cash account.
	LimitKindLoss

	// LimitKindSession caps the wall-clock duration of one authenticated
	// session. It is the only kind denominated in seconds rather than minor
	// units, which is why user_limits carries the three biconditional CHECKs
	// that make a money-denominated session limit unstorable.
	LimitKindSession
)

// String implements fmt.Stringer and produces the exact stored spelling.
func (k LimitKind) String() string {
	switch k {
	case LimitKindGrant:
		return "grant"
	case LimitKindStake:
		return "stake"
	case LimitKindLoss:
		return "loss"
	case LimitKindSession:
		return "session"
	default:
		return "unknown"
	}
}

// Valid reports whether k is one of the four defined kinds.
func (k LimitKind) Valid() bool {
	switch k {
	case LimitKindGrant, LimitKindStake, LimitKindLoss, LimitKindSession:
		return true
	default:
		return false
	}
}

// IsMoney reports whether this kind is denominated in minor units. Exactly the
// complement of "is a session limit", which is what user_limits_session_period,
// user_limits_session_is_duration and user_limits_money_is_amount encode as
// three biconditionals.
func (k LimitKind) IsMoney() bool {
	switch k {
	case LimitKindGrant, LimitKindStake, LimitKindLoss:
		return true
	default:
		return false
	}
}

// ParseLimitKind is the inverse of String for the defined kinds.
func ParseLimitKind(s string) (LimitKind, error) {
	switch s {
	case "grant":
		return LimitKindGrant, nil
	case "stake":
		return LimitKindStake, nil
	case "loss":
		return LimitKindLoss, nil
	case "session":
		return LimitKindSession, nil
	default:
		return LimitKindUnknown, fmt.Errorf("%w: limit kind %q", ErrInvalid, sample(s))
	}
}

// MarshalText implements encoding.TextMarshaler.
func (k LimitKind) MarshalText() ([]byte, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("%w: limit kind %d", ErrInvalid, uint8(k))
	}
	return []byte(k.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (k *LimitKind) UnmarshalText(b []byte) error {
	parsed, err := ParseLimitKind(string(b))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// LimitPeriod is the window a limit is measured over: user_limits.period,
// constrained by user_limits_period_defined to day | week | month | session.
//
// The column is named 'period' rather than 'window' because WINDOW is a
// reserved word in PostgreSQL; the Go name follows the column so the two read
// the same at every call site.
type LimitPeriod uint8

const (
	// LimitPeriodUnknown is the invalid zero value.
	LimitPeriodUnknown LimitPeriod = iota

	// LimitPeriodDay is a rolling or calendar day — which of the two is policy
	// and lives in internal/betting, not in the vocabulary.
	LimitPeriodDay
	// LimitPeriodWeek is a week.
	LimitPeriodWeek
	// LimitPeriodMonth is a month.
	LimitPeriodMonth
	// LimitPeriodSession is one authenticated session, and pairs with
	// LimitKindSession and nothing else — user_limits_session_period makes that
	// a biconditional in the database.
	LimitPeriodSession
)

// String implements fmt.Stringer and produces the exact stored spelling.
func (p LimitPeriod) String() string {
	switch p {
	case LimitPeriodDay:
		return "day"
	case LimitPeriodWeek:
		return "week"
	case LimitPeriodMonth:
		return "month"
	case LimitPeriodSession:
		return "session"
	default:
		return "unknown"
	}
}

// Valid reports whether p is one of the four defined periods.
func (p LimitPeriod) Valid() bool {
	switch p {
	case LimitPeriodDay, LimitPeriodWeek, LimitPeriodMonth, LimitPeriodSession:
		return true
	default:
		return false
	}
}

// ParseLimitPeriod is the inverse of String for the defined periods.
func ParseLimitPeriod(s string) (LimitPeriod, error) {
	switch s {
	case "day":
		return LimitPeriodDay, nil
	case "week":
		return LimitPeriodWeek, nil
	case "month":
		return LimitPeriodMonth, nil
	case "session":
		return LimitPeriodSession, nil
	default:
		return LimitPeriodUnknown, fmt.Errorf("%w: limit period %q", ErrInvalid, sample(s))
	}
}

// MarshalText implements encoding.TextMarshaler.
func (p LimitPeriod) MarshalText() ([]byte, error) {
	if !p.Valid() {
		return nil, fmt.Errorf("%w: limit period %d", ErrInvalid, uint8(p))
	}
	return []byte(p.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (p *LimitPeriod) UnmarshalText(b []byte) error {
	parsed, err := ParseLimitPeriod(string(b))
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

// LimitPairValid reports whether a (kind, period) pair is storable, reproducing
// user_limits_session_period in Go: 'session' pairs with 'session' and nothing
// else, in both directions.
//
// It exists so a caller can reject the combination with a 400 instead of
// discovering it as a 23514 from the database, which is a 500 by the time it
// reaches a handler.
func LimitPairValid(k LimitKind, p LimitPeriod) bool {
	if !k.Valid() || !p.Valid() {
		return false
	}
	return (k == LimitKindSession) == (p == LimitPeriodSession)
}
