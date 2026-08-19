package auth

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// Instrumentation for the authentication core.
//
// # Names are a contract
//
// deploy/observability/prometheus.yml states the rule the rest of this repo
// follows: "every application series is prefixed `sharpline_`". These follow it
// under the `sharpline_auth_` subsystem, and the PromQL a panel or an alert
// needs is written next to each definition so nobody has to reverse-engineer
// the label set from the code.
//
// # The one series that is a security control rather than an observation
//
//	sharpline_auth_refresh_total{outcome="reuse"}
//
// A non-zero rate on that series means a refresh token was presented twice,
// which means either a token was stolen or a client is buggy. It is the ONLY
// externally-visible signal that a session was compromised — reuse detection
// revokes the lineage silently, and the user just sees themselves logged out —
// so it wants an alert rather than a panel:
//
//	increase(sharpline_auth_refresh_total{outcome="reuse"}[15m]) > 0
//
// A separate dedicated counter was considered and rejected: two series counting
// one event drift the moment somebody increments one and not the other.
//
// # Labels this package deliberately does NOT set
//
//   - No user id, email, session id or token id, anywhere. Every one of them is
//     unbounded cardinality, and the last two are also credentials-adjacent.
//     Per-user forensics belongs in the audit log (CLAUDE.md §6, Platform),
//     which is a table, not a time series.
//   - No `service`. prometheus.yml attaches that as a target label; a metric
//     label of the same name would be renamed `exported_service` on ingest.
const (
	metricNamespace = "sharpline"
	metricSubsystem = "auth"
)

// Outcome label values. A closed set: every value below is written by exactly
// one branch in service.go and nothing else is ever written to these labels.
const (
	outcomeOK    = "ok"
	outcomeError = "error"

	loginBadCredentials  = "bad_credentials"
	login2FARequired     = "second_factor_required"
	login2FAInvalid      = "second_factor_invalid"
	loginNotPermitted    = "not_permitted"
	registerEmailTaken   = "email_taken"
	registerInvalidInput = "invalid_input"

	factorTOTP     = "totp"
	factorRecovery = "recovery_code"

	hashOpDerive = "derive"
	hashOpVerify = "verify"
)

// Latency buckets.
//
// hashBuckets are chosen around the measured cost of one argon2id hash under
// [DefaultParams] — tens of milliseconds — and reach 5 seconds because the
// observation INCLUDES the wait on the hasher's concurrency limiter. That is
// the point: a bucket distribution that suddenly grows a tail at 1s and above
// is the limiter queueing, which is the early warning that a login flood is
// under way, and it is invisible if the histogram tops out near the compute
// cost.
var hashBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 1, 2.5, 5,
}

// loginBuckets cover a whole login: one or two argon2id hashes plus the store
// round trips.
var loginBuckets = []float64{
	0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

type metrics struct {
	logins        *prometheus.CounterVec // outcome
	loginDuration *prometheus.HistogramVec
	registrations *prometheus.CounterVec // outcome
	refreshes     *prometheus.CounterVec // outcome
	revocations   *prometheus.CounterVec // reason
	secondFactor  *prometheus.CounterVec // factor, outcome
	hashDuration  *prometheus.HistogramVec
	rehashes      prometheus.Counter
	accessTokens  *prometheus.CounterVec // outcome
}

// newMetrics builds the collectors and registers them on reg.
//
// reg may be nil, which builds the collectors and registers nothing — correct
// for a unit test and for any caller that serves no /metrics endpoint. The
// observe calls stay live and cost nanoseconds, so no call site needs a nil
// check. This mirrors internal/platform/postgres exactly, deliberately: two
// packages with two different conventions for the same thing is how a nil-check
// gets forgotten.
//
// A registration failure is returned rather than swallowed. Two Services
// sharing one registry is a programming error and it should fail at startup,
// not produce two services' numbers under one series.
func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		logins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem,
			Name: "logins_total",
			Help: "Password login attempts by outcome: ok, bad_credentials, second_factor_required, " +
				"second_factor_invalid, not_permitted (suspended or closed), error (the store failed). " +
				"Credential-stuffing panel: sum(rate(sharpline_auth_logins_total{outcome=\"bad_credentials\"}[5m])).",
		}, []string{"outcome"}),

		loginDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem,
			Name: "login_duration_seconds",
			Help: "End-to-end duration of a login attempt by outcome. " +
				"The ok and bad_credentials series should have the SAME distribution — " +
				"a visible gap between them is the user-enumeration timing oracle reappearing.",
			Buckets: loginBuckets,
		}, []string{"outcome"}),

		registrations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem,
			Name: "registrations_total",
			Help: "Registration attempts by outcome: ok, email_taken, invalid_input, error.",
		}, []string{"outcome"}),

		refreshes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem,
			Name: "refresh_total",
			Help: "Refresh-token redemptions by outcome: ok, not_found, expired, revoked, reuse, " +
				"credential_change, error. ALERT on outcome=\"reuse\": " +
				"increase(sharpline_auth_refresh_total{outcome=\"reuse\"}[15m]) > 0 means a token was " +
				"presented twice and a session lineage was revoked as compromised.",
		}, []string{"outcome"}),

		revocations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem,
			Name: "sessions_revoked_total",
			Help: "Refresh-token families revoked, by refresh_token_families.revoked_reason: " +
				"logout, reuse_detected, credential_change, operator.",
		}, []string{"reason"}),

		secondFactor: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem,
			Name: "second_factor_total",
			Help: "Second-factor verifications by factor (totp, recovery_code) and outcome " +
				"(ok, second_factor_invalid, error). A code that verified but had already been " +
				"consumed inside its own 30-second step counts as second_factor_invalid — " +
				"that is the replay guard firing.",
		}, []string{"factor", "outcome"}),

		hashDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem,
			Name: "password_hash_duration_seconds",
			Help: "argon2id work by operation (derive, verify). INCLUDES the wait on the hasher's " +
				"concurrency limiter, so a growing tail above ~1s is queueing rather than a slower CPU. " +
				"Panel: histogram_quantile(0.99, sum by (le, operation) " +
				"(rate(sharpline_auth_password_hash_duration_seconds_bucket[$__rate_interval]))).",
			Buckets: hashBuckets,
		}, []string{"operation"}),

		rehashes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem,
			Name: "password_rehashes_total",
			Help: "Stored hashes upgraded to the current cost parameters during a successful login. " +
				"After a parameter bump this rises and then decays to zero as the active user base " +
				"cycles through; a value that never reaches zero means users who never log in, " +
				"not a bug.",
		}),

		accessTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Subsystem: metricSubsystem,
			Name: "access_token_verifications_total",
			Help: "Access-token verifications by outcome: ok, error. " +
				"The failure reason is deliberately NOT a label — it is attacker-controlled and " +
				"unbounded, and the reasons are in the log line.",
		}, []string{"outcome"}),
	}

	if reg == nil {
		return m, nil
	}

	collectors := []prometheus.Collector{
		m.logins, m.loginDuration, m.registrations, m.refreshes,
		m.revocations, m.secondFactor, m.hashDuration, m.rehashes, m.accessTokens,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("auth: register metrics collector: %w", err)
		}
	}
	return m, nil
}
