package auth

import (
	"errors"
	"strings"
	"testing"
)

// These tests are the Go half of a contract whose other half is
// migrations/00005. That file says so directly:
//
//	"Phase 5 (internal/auth) MUST define matching Go constants with String() /
//	 ParseX() pairs producing exactly these lowercase spellings"
//
// The database stores these value sets as TEXT + CHECK rather than as native
// ENUMs, which means nothing but agreement between the two files stops a typo
// from becoming a 23514 at runtime. The spellings below are copied from the
// CHECK constraints; if either side moves, this fails.

func TestUserStatusMatchesTheSchema(t *testing.T) {
	t.Parallel()

	// users_status_defined:
	//   CHECK (status IN ('active', 'suspended', 'self_excluded', 'closed'))
	want := map[UserStatus]string{
		UserStatusActive:       "active",
		UserStatusSuspended:    "suspended",
		UserStatusSelfExcluded: "self_excluded",
		UserStatusClosed:       "closed",
	}
	assertEnum(t, want, ParseUserStatus, UserStatusUnknown)
}

func TestRevokedReasonMatchesTheSchema(t *testing.T) {
	t.Parallel()

	// refresh_token_families_revoked_reason_defined:
	//   CHECK (revoked_reason IN ('logout', 'reuse_detected',
	//                             'credential_change', 'operator'))
	want := map[RevokedReason]string{
		RevokedReasonLogout:           "logout",
		RevokedReasonReuseDetected:    "reuse_detected",
		RevokedReasonCredentialChange: "credential_change",
		RevokedReasonOperator:         "operator",
	}
	assertEnum(t, want, ParseRevokedReason, RevokedReasonUnknown)
}

func TestLimitKindMatchesTheSchema(t *testing.T) {
	t.Parallel()

	// user_limits_kind_defined:
	//   CHECK (kind IN ('grant', 'stake', 'loss', 'session'))
	want := map[LimitKind]string{
		LimitKindGrant:   "grant",
		LimitKindStake:   "stake",
		LimitKindLoss:    "loss",
		LimitKindSession: "session",
	}
	assertEnum(t, want, ParseLimitKind, LimitKindUnknown)

	// migrations/00005: three of the four spellings are chosen to equal
	// internal/domain's EntryKind spellings exactly, "so that enforcing a limit
	// is a sum over ledger_entries filtered by the same string, with no
	// translation table in between".
	for _, k := range []LimitKind{LimitKindGrant, LimitKindStake, LimitKindLoss} {
		if !k.IsMoney() {
			t.Errorf("%s should be denominated in minor units", k)
		}
	}
	if LimitKindSession.IsMoney() {
		t.Error("a session limit is denominated in seconds, not minor units")
	}

	// There is deliberately no 'deposit' kind: CLAUDE.md §0 rules out payment
	// processing, so a deposit limit would be a control that can never fire.
	if _, err := ParseLimitKind("deposit"); err == nil {
		t.Error("ParseLimitKind accepted 'deposit'; CLAUDE.md §0 has no deposits")
	}
}

func TestLimitPeriodMatchesTheSchema(t *testing.T) {
	t.Parallel()

	// user_limits_period_defined:
	//   CHECK (period IN ('day', 'week', 'month', 'session'))
	want := map[LimitPeriod]string{
		LimitPeriodDay:     "day",
		LimitPeriodWeek:    "week",
		LimitPeriodMonth:   "month",
		LimitPeriodSession: "session",
	}
	assertEnum(t, want, ParseLimitPeriod, LimitPeriodUnknown)
}

// LimitPairValid reproduces user_limits_session_period in Go, so a bad
// combination is a 400 rather than a 23514 surfacing as a 500.
func TestLimitPairValid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		kind   LimitKind
		period LimitPeriod
		want   bool
	}{
		{LimitKindSession, LimitPeriodSession, true},
		{LimitKindStake, LimitPeriodDay, true},
		{LimitKindLoss, LimitPeriodWeek, true},
		{LimitKindGrant, LimitPeriodMonth, true},
		// Both directions of the biconditional.
		{LimitKindSession, LimitPeriodDay, false},
		{LimitKindStake, LimitPeriodSession, false},
		{LimitKindUnknown, LimitPeriodDay, false},
		{LimitKindStake, LimitPeriodUnknown, false},
	}
	for _, c := range cases {
		if got := LimitPairValid(c.kind, c.period); got != c.want {
			t.Errorf("LimitPairValid(%s, %s) = %v, want %v", c.kind, c.period, got, c.want)
		}
	}
}

// The two predicates that decide what an account may do. Their asymmetry is the
// responsible-gaming design: a self-excluded customer signs in and cannot bet.
func TestUserStatusPermissions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status          UserStatus
		canAuthenticate bool
		canWager        bool
	}{
		{UserStatusActive, true, true},
		// Login permitted so the customer can read their history and manage
		// the exclusion. Wagering blocked — see UserStatus.CanWager for why the
		// block has to be read from the database rather than from a JWT claim.
		{UserStatusSelfExcluded, true, false},
		{UserStatusSuspended, false, false},
		{UserStatusClosed, false, false},
		// The zero value must not read as permissive. "unknown" is the value a
		// failed parse produces, and a failed parse that permitted wagering
		// would be the worst possible default.
		{UserStatusUnknown, false, false},
	}
	for _, c := range cases {
		if got := c.status.CanAuthenticate(); got != c.canAuthenticate {
			t.Errorf("%s.CanAuthenticate() = %v, want %v", c.status, got, c.canAuthenticate)
		}
		if got := c.status.CanWager(); got != c.canWager {
			t.Errorf("%s.CanWager() = %v, want %v", c.status, got, c.canWager)
		}
	}
}

func TestStatusErrorMapsEveryNonPermittingStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status UserStatus
		want   error
	}{
		{UserStatusSelfExcluded, ErrSelfExcluded},
		{UserStatusSuspended, ErrAccountSuspended},
		{UserStatusClosed, ErrAccountClosed},
	}
	for _, c := range cases {
		if err := statusError(c.status); !errors.Is(err, c.want) {
			t.Errorf("statusError(%s) = %v, want %v", c.status, err, c.want)
		}
		// Every one of them is a 403, not a 401: retrying with better
		// credentials cannot help.
		if err := statusError(c.status); !errors.Is(err, ErrForbidden) {
			t.Errorf("statusError(%s) does not wrap ErrForbidden", c.status)
		}
	}
	// Even the cases that should be unreachable must refuse rather than fall
	// through to a permissive answer.
	for _, s := range []UserStatus{UserStatusActive, UserStatusUnknown, UserStatus(99)} {
		if err := statusError(s); err == nil {
			t.Errorf("statusError(%v) returned nil", s)
		}
	}
}

// enumSpec is the shape every enum in this file satisfies.
type enumSpec interface {
	~uint8
	String() string
	Valid() bool
	MarshalText() ([]byte, error)
}

func assertEnum[T enumSpec](t *testing.T, want map[T]string, parse func(string) (T, error), zero T) {
	t.Helper()

	for value, spelling := range want {
		if got := value.String(); got != spelling {
			t.Errorf("String() = %q, want %q (the CHECK constraint's spelling)", got, spelling)
		}
		if !value.Valid() {
			t.Errorf("%s is not Valid()", spelling)
		}

		parsed, err := parse(spelling)
		if err != nil {
			t.Errorf("Parse(%q) = %v", spelling, err)
			continue
		}
		if parsed != value {
			t.Errorf("Parse(%q) round trip produced a different value", spelling)
		}

		text, err := value.MarshalText()
		if err != nil {
			t.Errorf("MarshalText(%s) = %v", spelling, err)
			continue
		}
		if string(text) != spelling {
			t.Errorf("MarshalText(%s) = %q", spelling, text)
		}
	}

	// The zero value is invalid and never round-trips. Nothing may default to
	// a real value.
	if zero.Valid() {
		t.Error("the zero value reports itself Valid")
	}
	if got := zero.String(); got != "unknown" {
		t.Errorf("the zero value String() = %q, want \"unknown\"", got)
	}
	if _, err := zero.MarshalText(); err == nil {
		t.Error("the zero value marshalled without an error")
	}

	// Nothing outside the set parses, including near-misses, case variants and
	// the "unknown" spelling itself.
	for _, bad := range []string{"", "unknown", "UNKNOWN", "Active", " active", "active ", "nonsense"} {
		if _, err := parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded", bad)
		} else if !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) = %v, want ErrInvalid", bad, err)
		}
	}

	// A rejected value is echoed truncated, never unbounded: this input arrives
	// from the database or a request body and an unbounded echo into a log line
	// is how a log becomes an attack surface.
	long := strings.Repeat("x", 4096)
	_, err := parse(long)
	if err == nil {
		t.Fatal("Parse of a 4096-byte value succeeded")
	}
	if len(err.Error()) > 256 {
		t.Errorf("the rejection message is %d bytes; the value is not being truncated", len(err.Error()))
	}
}
