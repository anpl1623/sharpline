// The per-connection sequence counter, and the gap accounting that makes a
// discarded buffer visible on the wire.
//
// # Why this is its own file for forty lines of arithmetic
//
// D3 is the decision that determines whether this service HAS a resync
// mechanism or merely has a resync frame. It is one increment in one place, and
// getting it wrong is invisible in every test that does not deliberately
// overflow a queue — so the reasoning lives beside the counter rather than in a
// comment on the field.
//
// # Assigned at ENQUEUE, never at write
//
// The number advances for EVERY frame the server puts on a connection's send
// queue: hello, ack, snapshot, delta, resync, error and pong alike.
//
// D4 discards a slow client's pending buffer. If the number were stamped as a
// frame left for the socket, the discarded frames would never have consumed
// numbers and the client would see 4, 5, 6 with nothing missing — a hole in the
// odds board that only the server knows about, on the one client whose
// connection is already unhealthy. Stamping at enqueue makes the same discard
// show up as seq 4 followed by seq 41, which is a gap the client can SEE and
// act on without being told. The `resync` frame that follows is the courtesy;
// the gap is the mechanism.
//
// # Not safe for concurrent use, deliberately
//
// [sequence] has no lock, and the owner must serialise it. That is not an
// optimisation and it is not laziness about data races: the number must be
// assigned IN THE SAME CRITICAL SECTION that hands the frame to the queue.
//
// An atomic counter would be race-free and still wrong. Two publishers could
// take 5 and 6 and then reach the channel in the opposite order, and the client
// would receive 6 before 5 — a gap that never healed, on a connection where
// nothing was actually lost. So the counter is a plain uint64 guarded by the
// connection's send mutex, and its type carries no lock precisely so that a
// future caller cannot conclude from `sequence.next()` alone that it is safe to
// call from anywhere.
//
// # Per connection, not per hub
//
// There is no shared counter for ten thousand connections to contend on, and no
// cross-replica ordering problem to invent. Each connection announces its own
// `connection_id` in its `hello` frame and a client resets its expectation for
// each connection, so a reconnect starting again at 1 is not an epoch problem —
// it is a different connection.
package wsgw

// sequence is one connection's monotonic frame counter.
//
// The zero value is ready to use and produces 1 as its first number. Starting
// at 1 rather than 0 is deliberate: a client that tracks "the last sequence I
// saw" in a zero-valued variable must not have that value collide with a real
// frame, or the first frame of the connection is indistinguishable from "no
// frame yet".
type sequence struct {
	n uint64
}

// next assigns the following sequence number. The caller MUST hold the lock
// that also covers the hand-off to the send queue; see the file comment.
func (s *sequence) next() uint64 {
	s.n++
	return s.n
}

// last reports the highest number assigned so far, or 0 when none has been. It
// exists for the gap accounting and for tests; it is not part of any frame.
func (s *sequence) last() uint64 { return s.n }

// gap is the inclusive range of sequence numbers a client did not receive, and
// how many frames that range represents.
//
// It is the payload of a slow-consumer [ResyncFrame], and it is computed from
// the frames that were ACTUALLY removed from the queue rather than from a
// subtraction of the endpoints. That distinction is load-bearing: the writer
// goroutine may pop a frame while the discard is in progress, in which case
// that frame really was written and must not be reported as lost. Because only
// what is removed is recorded, the range is exact by construction and can never
// claim a frame the client received.
type gap struct {
	from    uint64
	to      uint64
	dropped int
}

// extend records that the frame numbered seq was discarded.
//
// Frames are discarded in queue order, so `to` only ever grows; `from` is set
// once, by the first call, because that is the oldest number the client is
// missing. A zero seq is ignored — it cannot name a frame ([sequence] starts at
// 1), so accepting one would produce a range that begins before the connection
// did.
func (g *gap) extend(seq uint64) {
	if seq == 0 {
		return
	}
	if g.from == 0 || seq < g.from {
		g.from = seq
	}
	if seq > g.to {
		g.to = seq
	}
	g.dropped++
}

// empty reports whether nothing was discarded. A resync with an empty gap would
// tell a client to throw away state it has not lost, which costs a full
// snapshot per channel for no reason, so the caller checks this before sending
// one.
func (g gap) empty() bool { return g.dropped == 0 }
