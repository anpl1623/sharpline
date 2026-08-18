package writer_test

import (
	"encoding/json"
	"testing"

	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/writer"
)

// TestMessageTypeMatchesTheProducer.
//
// The wire STRUCT is now shared — writer.Record is an alias for
// normalizer.NormalizedMarket — so a renamed field is a compile error. The
// envelope TYPE is still two constants in two packages, and it is the one
// remaining string both sides have to spell identically.
//
// Getting it wrong is silent in the worst way: the consumer rejects every record
// with ErrWrongMessageType, the metrics show outcome=invalid, and a service that
// is writing nothing at all still passes its health check. That is the shape of
// the defect this file was rewritten to prevent, so it gets an assertion rather
// than a comment.
func TestMessageTypeMatchesTheProducer(t *testing.T) {
	if writer.MessageType != normalizer.MessageType {
		t.Fatalf("the writer accepts %q but the normalizer publishes %q; every record on "+
			"odds.normalized would be rejected as the wrong message type",
			writer.MessageType, normalizer.MessageType)
	}
}

// TestRecordIsTheProducersType pins the alias.
//
// If someone re-introduces a writer-owned copy of the wire structs, this stops
// compiling — which is the point. The previous copy diverged on three fields
// (`quotes` vs `prices`, `market.observed_at` vs `market.updated_at`, and an
// `event.observed_at` the producer never sent), and both packages' tests passed
// throughout because each marshalled and unmarshalled its own spelling.
func TestRecordIsTheProducersType(t *testing.T) {
	var rec writer.Record
	// Assignment in both directions only compiles for identical types. The
	// second is what makes it an ALIAS assertion rather than an assignability
	// one: a distinct named type with the same underlying struct would satisfy
	// neither direction without a conversion.
	produced := normalizer.NormalizedMarket(rec)
	rec = writer.Record(produced)

	// And the JSON the producer emits is the JSON the consumer reads, field for
	// field, because there is only one set of tags.
	a, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	b, err := json.Marshal(produced)
	if err != nil {
		t.Fatalf("marshal normalized market: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("the two spellings of one contract disagree:\n  writer:     %s\n  normalizer: %s", a, b)
	}
}
