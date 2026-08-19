package wsgw

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaves a goroutine behind.
//
// # Why a package-level check rather than only a per-test one
//
// conn_test.go already asserts that serving twenty connections returns the
// process to its goroutine baseline, and that assertion is worth keeping — it
// localises a leak to the connection lifecycle. But it is a comparison of
// runtime.NumGoroutine() taken inside a package whose every other test calls
// t.Parallel(), so the number it samples includes whatever else happens to be
// running. That makes it good at catching a leak of twenty and blind to a leak
// of one, which is the size a real one starts at.
//
// goleak.VerifyTestMain runs after every test in the package has returned, when
// nothing parallel is left to perturb the count, and it reports the offending
// STACK rather than a number. The two are complementary: the per-test check says
// which lifecycle leaked, this one says whether anything leaked at all.
//
// # Why this package in particular
//
// A leak here is a leak PER CLIENT. CLAUDE.md §10 states the target as ten
// thousand concurrent subscribers on one node, and every one of them is a
// connection that starts a reader, a writer and a heartbeat. A goroutine that
// outlives its socket by one is ten thousand goroutines and ten thousand send
// buffers held against a node with 12GB, and it presents as a slow memory climb
// nobody attributes to the gateway — which is exactly the failure mode a leak
// detector exists to convert into a build failure.
//
// # The ignore list is deliberately short
//
// Only the runtime's own perpetual goroutines are excused. Nothing belonging to
// this package is on it: if a wsgw goroutine appears here, the answer is to stop
// leaking it, not to add a frame to this list. Adding one is the exact shape of
// "making a check pass by weakening it".
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// net/http's idle-connection reaper and the transport's read loops are
		// started by httptest servers in server_test.go and are torn down
		// asynchronously after Close returns. They belong to the standard
		// library, not to this package.
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
	)
}
