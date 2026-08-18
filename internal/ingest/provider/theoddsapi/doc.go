// Package theoddsapi is the real odds provider adapter: an HTTP client for
// The Odds API v4, plus the quota accounting the charter demands around it.
//
// ADR 0003 chose this provider and did the quota arithmetic. CLAUDE.md §5 sets
// the requirements this package exists to satisfy:
//
//	"Each provider gets an adapter behind one interface. […] Respect provider
//	 quotas via a token-bucket limiter with the budget as a config value, and
//	 expose remaining quota as a Prometheus gauge."
//
// The synthetic stochastic market maker is the sibling adapter behind the same
// interface. `ingest` picks between them at startup on the presence of
// ODDS_API_KEY. Nothing downstream can tell which is running.
//
// # The API key is the only real secret in this repository
//
// The Odds API takes it as a QUERY PARAMETER — `?apiKey=…` — which is the worst
// possible place for a credential from a leakage standpoint, because a URL ends
// up in log lines, in error strings, and in span attributes almost by accident.
// Three specific hazards, and what is done about each:
//
//   - net/http returns *url.Error from Do, and its Error() interpolates the FULL
//     request URL. Wrapping such an error with %w and logging it publishes the
//     key. sanitizeError unwraps *url.Error and rebuilds it around a redacted
//     URL, and every error this package returns additionally passes through
//     redactor.String as a belt-and-braces second pass.
//   - Span attributes. `url.full` is set to the redacted URL, never the real
//     one. There is no code path that puts the key in an attribute or a metric
//     label.
//   - Redirects. A 3xx to another host would hand the key to whoever controls
//     that host. The client's CheckRedirect refuses any cross-host redirect.
//
// redactor is a value type holding the key; TestRedaction asserts the key is
// absent from every error string, every log record, and every span attribute
// this package can produce. If you add a code path that formats a URL, route it
// through redactor first.
//
// # The quota model, and why the local count is not the source of truth
//
// The Odds API bills in CREDITS, not requests. One `/v4/sports/{sport}/odds`
// call costs `markets × regions` credits regardless of how many events come
// back; `/v4/sports` and `/v4/sports/{sport}/events` are free. Every response
// carries the provider's own accounting in three headers:
//
//	x-requests-remaining   credits left until the monthly quota resets
//	x-requests-used        credits consumed since the last reset
//	x-requests-last        the cost of the call that just returned
//
// ADR 0003 requires the gauge be fed from `x-requests-remaining` "the provider's
// own number, not from a local counter", because a local counter drifts: it
// cannot see spend from another process sharing the key, it cannot see the
// month roll over, and it cannot see a call whose cost differed from the one we
// predicted (the docs state that a request returning no events is not charged,
// and that the event-odds endpoint is billed on markets RETURNED rather than
// markets requested). So the header wins whenever it is present, the local
// estimate is only a fallback, and the two are reported separately so a
// divergence is visible rather than smoothed over.
//
// # Two token buckets, because there are two different limits
//
// CLAUDE.md §5 asks for "a token-bucket limiter with the budget as a config
// value". There are two distinct budgets and one bucket cannot express both:
//
//   - The MONTHLY CREDIT BUDGET. A long window — ADR 0003's recommended tier is
//     100,000 credits per 30 days, which is 0.0386 credits per second. A bucket
//     with that refill rate and a capacity of one day's worth paces spend across
//     the month while still permitting a day of catch-up after an outage. This
//     is the bucket that stops the board burning September's quota on the 3rd.
//   - The PER-SECOND FREQUENCY LIMIT. Documented at 30 requests/second, with the
//     provider warning that 429s occur even below it. A second bucket, in
//     requests rather than credits, keeps sweep bursts under that.
//
// Both are configuration (Config.MonthlyCredits, Config.RequestsPerSecond and
// their burst sizes). Neither sleeps: Reserve reports how long until the
// tokens exist and the caller decides, because the scheduler owns cadence and a
// limiter that blocks inside an adapter is a limiter that hides a stall.
//
// # Failure modes are distinct on purpose
//
// The Odds API's documented error codes collapse onto five status codes, and
// two very different failures share 401. Conflating them would be a real defect:
// "the key is wrong" is a deployment error a human must fix, while "the credits
// ran out" is an operational state that ADR 0003 says must fail loudly to a
// visible degraded mode rather than silently serving hour-old prices. So:
//
//	401 + INVALID_KEY/MISSING_KEY/DEACTIVATED_KEY  -> ErrUnauthenticated (fatal)
//	401 + OUT_OF_USAGE_CREDITS                     -> ErrQuotaExhausted   (not retryable, alert)
//	422 (any INVALID_*/MISSING_* param code)       -> ErrInvalidRequest   (fatal, config bug)
//	404                                            -> ErrNotFound         (skip this event)
//	429                                            -> ErrRateLimited      (retry after)
//	5xx                                            -> ErrProviderFailure  (retry)
//	network/timeout                                -> ErrTransport        (retry)
//	local budget empty                             -> ErrBudgetExhausted  (do not call)
//
// The provider documents the error CODE names but never publishes the JSON
// envelope they arrive in. This package therefore does not decode a guessed
// error body shape — it scans the raw body for the documented code tokens. That
// is why classifyErrorCode takes a string and not a struct.
//
// # Golden files
//
// CLAUDE.md §10 requires golden files for provider normalization.
// testdata/docsamples holds The Odds API's OWN PUBLISHED example responses,
// extracted from its documentation, with provenance recorded in
// testdata/docsamples/SOURCE.md. None of them was written by hand and none was
// recorded from a live call — this repository has no key. Read SOURCE.md before
// changing anything in that directory.
package theoddsapi
