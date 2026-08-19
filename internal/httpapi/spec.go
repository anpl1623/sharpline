package httpapi

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel/trace"
)

// The OpenAPI document, embedded and served.
//
// # Why the server serves its own spec
//
// A client generating a stub from a copied openapi.yaml is generating from a
// file that may be older than the server it will talk to, and nothing tells it
// so. `GET /api/openapi.yaml` is always the running build's own contract,
// byte-for-byte the file `make codegen-check-openapi` diffs the generated types
// against.
//
// It is embedded rather than read from disk because the runtime image is
// gcr.io/distroless/static:nonroot with a read-only root filesystem and nothing
// in it but the binary — there is no file to read, and a handler that tried
// would work in a test and 500 in production.

//go:embed openapi.yaml
var specFS embed.FS

// ErrNotFound reports an entity that does not exist.
//
// Declared HERE, by the consumer, rather than in the store: internal/httpapi is
// what has to distinguish "no such event" (404) from "the database is down"
// (500), so it owns the sentinel that expresses the difference. A store adapter
// wraps pgx.ErrNoRows into this; a store that returned pgx.ErrNoRows raw would
// make every consumer import a database driver to classify an absence.
var ErrNotFound = errors.New("httpapi: not found")

// ErrConflict reports a write that lost a race with a concurrent one — a limit
// superseded between the read and the update.
//
// Declared here for the same reason as [ErrNotFound]: this package is what has
// to turn the condition into a 409, so it owns the sentinel. The adapter wraps
// the store's own signal (a zero row count from a guarded UPDATE, or a unique
// violation on the partial index) into this.
var ErrConflict = errors.New("httpapi: conflict")

// SpecBytes returns the embedded OpenAPI document.
func SpecBytes() ([]byte, error) {
	b, err := specFS.ReadFile("openapi.yaml")
	if err != nil {
		return nil, fmt.Errorf("read embedded openapi.yaml: %w", err)
	}
	return b, nil
}

// handleSpec serves the OpenAPI document.
//
// `application/yaml` is the registered media type (RFC 9512). It is served
// unauthenticated and uncacheable-by-default: the contract is public — it is
// published documentation, not a capability — and a stale cached copy is exactly
// the failure this endpoint exists to remove.
func (a *API) handleSpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(a.specs)
	_ = r
}

// spanIDs is the pair of W3C Trace Context identifiers an audit row carries.
type spanIDs struct {
	trace string
	span  string
}

// traceIDs extracts the current span's ids in the lowercase hex form Jaeger
// displays and migration 00007's CHECK constraint enforces.
//
// An unsampled or absent span yields empty strings rather than the all-zero id:
// storing "00000000000000000000000000000000" would satisfy the CHECK and be
// permanently unjoinable to anything, which is worse than NULL because it looks
// like data.
func traceIDs(ctx context.Context) spanIDs {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return spanIDs{}
	}
	return spanIDs{trace: sc.TraceID().String(), span: sc.SpanID().String()}
}
