package client

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails this package if any test leaves a goroutine behind.
//
// It is here for the SAME reason it is in internal/wsgw, applied to the other
// end of the socket. A *Stream owns a read pump and a reconnect loop, and it is
// the SDK — code other people embed in their own process. A leak in a server is
// a leak an operator eventually sees on a dashboard; a leak in an SDK is one
// that shows up in somebody else's service, attributed to somebody else's code,
// with no dashboard at all. A Close that returns while its reconnect loop keeps
// backing off is the specific shape to catch, because it is invisible: the
// caller has done everything right.
//
// The ignore list covers only the standard library's own asynchronous teardown
// — httptest servers close their transports after Close returns. Nothing
// belonging to this package is excused, and nothing belonging to it should ever
// be added.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
	)
}
