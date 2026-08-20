// The results seam, declared and deliberately not implemented.
//
// # Why an unimplemented method rather than no method
//
// The alternative is to leave *Adapter without Results and let the composition
// root type-assert for provider.ResultsProvider. That would compile, and it
// would be worse in two specific ways.
//
// It would make the gap INVISIBLE. A missing method surfaces as a nil interface
// somewhere in cmd/ingest, at which point the deployment either silently runs
// with no results feed — the exact failure the whole results path was built to
// close, where wagers are placed against live prices and nothing ever settles
// them — or grows a special case for "the adapter that cannot do this", which is
// a branch nobody exercises. A method that returns a classified, self-explaining
// error surfaces the same fact as one WARN line naming the reason, once, at the
// moment the poller first asks.
//
// And it would make the seam theoretical. The compile-time assertion below is
// what proves provider.ResultsProvider is implementable by a real HTTP adapter
// and not just by the generator it was written alongside — the failure mode
// where an interface fits exactly one implementation and has to be redesigned
// the moment a second appears.
//
// # Why the endpoint is not simply written
//
// It is a separate billed call, and CLAUDE.md §13's first open decision — which
// provider, on which plan — is still open. ADR 0003 fixed the quota arithmetic
// for /odds alone: one /odds request covers a whole sport, cost is multiplicative
// in markets × regions, and the live cadence was bought against that budget.
// /scores is not on that budget. It has its own per-request cost, its own
// `daysFrom` lookback that is billed differently from a plain call, and it must
// be polled on its own cadence for every sport in scope whether or not a contest
// finished — so adding it changes the monthly credit total, which is the number
// the whole cadence ladder was sized from.
//
// That is an ADR with fresh quota math, exactly as ADR 0003 was, and it is not a
// thing to smuggle in behind a results poller. Writing the call here without it
// would silently spend a budget somebody else sized, and the first visible
// symptom would be the odds board going stale mid-slate because the credits went
// to scores.
package theoddsapi

import (
	"context"

	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// Compile-time proof that this adapter occupies the results seam. See the file
// comment: the assertion is the point, and removing it to "clean up" an
// unimplemented method would delete the only evidence that the interface fits
// something other than the generator.
var _ provider.ResultsProvider = (*Adapter)(nil)

// Results implements provider.ResultsProvider by declining, always.
//
// The error wraps provider.ErrNotSupported and is DispositionFatal, which is the
// classification that makes the decline behave correctly rather than merely be
// documented. provider.Classify maps ErrNotSupported to fatal, so the poller
// stops asking after the first call instead of retrying a capability that cannot
// appear at run time, and it says so once at WARN rather than every tick.
//
// It ignores its arguments and reads no clock, because there is no request to
// shape and nothing to time out. It does NOT check the context first: a
// cancelled context would classify as fatal too and produce a confusing "results
// path disabled" line whose stated reason was a cancellation, when the reason is
// and always will be this one.
//
// Implementing it is a scores-endpoint adapter plus the ADR the file comment
// describes. Until then, a deployment running with ODDS_API_KEY set has a live
// odds board and no settlement feed, and it is told so on the first poll.
func (a *Adapter) Results(context.Context, provider.ResultWindow) ([]provider.FinalResult, error) {
	return nil, provider.Newf("results", a.name, provider.DispositionFatal, provider.ErrNotSupported,
		"this adapter serves prices only: scores are a separate billed endpoint whose quota cost "+
			"is not in ADR 0003's arithmetic, and adding it is an ADR with fresh quota math "+
			"(CLAUDE.md §13, open decision 1) rather than a change to the results poller")
}
