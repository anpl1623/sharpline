package theoddsapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// The results seam is declared here and deliberately not implemented; results.go
// carries the argument. These assert the two things that make the decline a real
// seam rather than a hole:
//
//  1. the interface is SATISFIED, so provider.ResultsProvider is proved
//     implementable by a real HTTP adapter and not only by the generator it was
//     written alongside;
//  2. the decline classifies as FATAL, which is what makes the poller disable
//     itself once at WARN instead of retrying, for ever, a capability that
//     cannot appear at run time.

func TestAdapterOccupiesTheResultsSeam(t *testing.T) {
	t.Parallel()

	stub := newProviderStub(t)
	h := newHarness(t, stub, nil)

	var p provider.ResultsProvider = h.Adapter
	if p.Name() != h.Adapter.Name() {
		t.Errorf("Name() = %q through the results seam, want %q", p.Name(), h.Adapter.Name())
	}
}

func TestResultsDeclinesFatallyAndIssuesNoRequest(t *testing.T) {
	t.Parallel()

	stub := newProviderStub(t)
	h := newHarness(t, stub, nil)

	got, err := h.Adapter.Results(context.Background(), provider.ResultWindow{
		Since: h.Now.Add(-24 * time.Hour),
		Until: h.Now,
	})
	if err == nil {
		t.Fatal("Results returned no error; this adapter serves prices only")
	}
	if got != nil {
		t.Errorf("Results returned %d outcomes alongside its refusal", len(got))
	}
	if !errors.Is(err, provider.ErrNotSupported) {
		t.Errorf("error = %v, want it to wrap provider.ErrNotSupported", err)
	}
	if d := provider.Classify(err); d != provider.DispositionFatal {
		t.Errorf("disposition = %s, want fatal; a retryable classification would make the poller "+
			"ask for ever for an endpoint that is not implemented", d)
	}

	// No quota is spent stating that a capability does not exist. /scores is a
	// separate billed endpoint whose cost is not in ADR 0003's arithmetic, and
	// the whole reason this is unimplemented is that spending against that
	// budget needs its own ADR.
	if n := len(stub.seen()); n != 0 {
		t.Errorf("the decline issued %d HTTP requests, want 0", n)
	}
}

// TestResultsDeclinesEvenOnACancelledContext. The reason is and always will be
// the same one, and a cancelled context would classify as fatal too — producing
// a "results path disabled" line whose stated cause was a cancellation.
func TestResultsDeclinesEvenOnACancelledContext(t *testing.T) {
	t.Parallel()

	stub := newProviderStub(t)
	h := newHarness(t, stub, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := h.Adapter.Results(ctx, provider.ResultWindow{
		Since: h.Now.Add(-24 * time.Hour),
		Until: h.Now,
	})
	if !errors.Is(err, provider.ErrNotSupported) {
		t.Errorf("error = %v, want provider.ErrNotSupported rather than a context error", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Error("the refusal reported a cancellation as its reason")
	}
}
