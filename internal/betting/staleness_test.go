// The staleness horizon is coupled to two constants in other packages, and this
// file is what stops the three from drifting apart again.
//
// They already had. betting.DefaultMaxQuoteAge was derived from the poll cadence
// alone (two 90-second live intervals) while the thing that actually bounds how
// old the freshest stored quote gets is the NORMALIZER's suppression ceiling,
// which is five minutes. A three-minute horizon against a five-minute ceiling
// refuses a quiet market for the two minutes between them, and on a healthy
// pipeline that was measured at 230 of 978 selections refused at once.
//
// Nothing about that was visible from inside any one package. Each constant is
// individually well reasoned and documented; the defect is only in the
// RELATIONSHIP, which no compiler and no single-package test could see.
//
// # Why a test-only import of the ingest packages is acceptable here
//
// internal/betting has no production dependency on internal/ingest and must not
// gain one — placement reads stored quotes, not the pipeline that wrote them.
// But an assertion about two packages has to be able to see both, and the
// alternative is restating 5m and 90s as literals here, which is a fourth copy
// of the numbers and drifts exactly like the other three did.
//
// The import is therefore deliberate and confined to _test.go, where it costs
// nothing at build time and cannot become a runtime edge.
package betting_test

import (
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/betting"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/scheduler"
)

// pipelineLagMargin is the slack between "the provider observed it" and "the row
// is readable", covering the normalizer, the bus and the Timescale writer.
//
// It is not measured, it is a floor: the assertion below is that the horizon
// clears the worst case by SOMETHING, and a horizon that clears it by one second
// would pass arithmetic and fail in production on any slow poll.
const pipelineLagMargin = 15 * time.Second

// TestMaxQuoteAgeClearsTheSuppressionCeiling is the guard the comment on
// DefaultMaxQuoteAge points at.
//
// The worst age a QUIET market's freshest quote can reach is the suppression
// ceiling plus one live poll: the republish does not happen when the ceiling
// elapses, it happens on the first sweep after it. Anything below that refuses
// the system's own freshest data, which is the failure this bound exists to
// avoid rather than to cause.
func TestMaxQuoteAgeClearsTheSuppressionCeiling(t *testing.T) {
	ceiling := normalizer.DefaultRefreshAfter
	live := scheduler.DefaultTiers().Live.Interval

	worstCase := ceiling + live
	want := worstCase + pipelineLagMargin

	if betting.DefaultMaxQuoteAge < want {
		t.Errorf(
			"DefaultMaxQuoteAge is %s, but a quiet market's freshest quote reaches %s "+
				"(normalizer.DefaultRefreshAfter %s + one live poll %s) before it is republished.\n"+
				"A horizon below that refuses the freshest data the pipeline produces: every quiet "+
				"market becomes unplaceable for the difference, on a sawtooth that resets when the "+
				"ceiling fires.\n"+
				"Either raise DefaultMaxQuoteAge to at least %s, or lower normalizer.DefaultRefreshAfter "+
				"-- and note that lowering it below the live cadence makes suppression save nothing, "+
				"which is the trade its own comment describes.",
			betting.DefaultMaxQuoteAge, worstCase, ceiling, live, want,
		)
	}
}

// TestMaxQuoteAgeIsNotUnbounded keeps the other side honest.
//
// The horizon is a real check, not a formality: a quote older than the ceiling
// plus a couple of polls means the ceiling did not fire, which is a wedged
// poller or a provider that has stopped answering. A horizon of an hour would
// lay a line nobody has confirmed since the last deploy.
func TestMaxQuoteAgeIsNotUnbounded(t *testing.T) {
	ceiling := normalizer.DefaultRefreshAfter
	live := scheduler.DefaultTiers().Live.Interval

	// Two extra polls past the worst case is generous and still bounded.
	limit := ceiling + live + 2*live

	if betting.DefaultMaxQuoteAge > limit {
		t.Errorf(
			"DefaultMaxQuoteAge is %s, more than %s (the suppression ceiling %s, one live poll %s "+
				"to republish, and two further polls of slack).\n"+
				"At that width the check stops detecting a wedged poller, which is the condition it "+
				"exists for, and starts laying prices nothing has confirmed.",
			betting.DefaultMaxQuoteAge, limit, ceiling, live,
		)
	}
}
