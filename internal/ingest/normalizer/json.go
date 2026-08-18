package normalizer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// errEmptyPayload is returned for a record whose value decodes to nothing.
var errEmptyPayload = errors.New("empty payload")

// decodeJSON unmarshals a provider payload.
//
// It deliberately does NOT set DisallowUnknownFields. A provider adds fields to
// its responses without warning — The Odds API added market-level `last_update`
// to a response that previously carried only a bookmaker-level one — and a
// decoder that treats an added field as an error turns a routine provider change
// into a total ingestion outage. Unknown fields are ignored, which is what
// encoding/json does by default and what forward compatibility requires.
//
// What it does add is a rejection of the two payloads that would otherwise
// unmarshal "successfully" into nothing: an empty value, and the four bytes
// `null`. Both leave the target at its zero value, and a zero RawEvent fails
// validation several layers later with an error that names a missing event id
// rather than a missing payload.
func decodeJSON(payload []byte, v any) error {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return errEmptyPayload
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return errEmptyPayload
	}
	if err := json.Unmarshal(trimmed, v); err != nil {
		return err
	}
	return nil
}

// strictUnmarshal is decodeJSON under the name raw.go calls it by.
func strictUnmarshal(payload []byte, v any) error {
	if err := decodeJSON(payload, v); err != nil {
		return fmt.Errorf("json: %w", err)
	}
	return nil
}
