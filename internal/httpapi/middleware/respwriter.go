package middleware

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
)

// recorder wraps an http.ResponseWriter to capture the status code and the
// number of body bytes written, which every observability layer in this package
// needs and net/http does not expose.
//
// # Why Unwrap, Flush, Hijack and ReadFrom are all here
//
// Wrapping a ResponseWriter silently removes whatever optional interfaces the
// original satisfied. For this API that would break three things:
//
//   - Unwrap is what http.ResponseController (Go 1.20+) follows to reach the
//     real writer, so a handler calling SetWriteDeadline or Flush through the
//     controller keeps working.
//   - Flush is the older, direct interface; some code type-asserts for it
//     instead of using the controller.
//   - Hijack matters because CLAUDE.md §11 puts a WebSocket gateway in this
//     system. `stream` is a separate binary today, but the moment any handler
//     behind this chain upgrades a connection, a wrapper without Hijack turns
//     that into a runtime failure that only appears under the one code path
//     nobody unit tests.
//   - ReadFrom preserves the io.Copy fast path (sendfile) for a handler that
//     streams a file.
type recorder struct {
	http.ResponseWriter

	status      int
	written     int64
	wroteHeader bool
}

// ensureRecorder returns w as a recorder, wrapping it only if it is not one
// already.
//
// Four middlewares in this chain need the final status code, and each of them
// has to work standalone as well as inside NewStack. Wrapping unconditionally
// would produce four nested recorders whose written-byte counts all describe the
// same bytes; this way the outermost one is the only one, and every inner layer
// observes exactly what went on the wire.
func ensureRecorder(w http.ResponseWriter) *recorder {
	if rec, ok := w.(*recorder); ok {
		return rec
	}
	return newRecorder(w)
}

func newRecorder(w http.ResponseWriter) *recorder {
	// A handler that writes a body without calling WriteHeader has implicitly
	// sent 200; seeding the field means a request that is never explicitly
	// statused is still recorded as the 200 it was.
	return &recorder{ResponseWriter: w, status: http.StatusOK}
}

// WriteHeader implements http.ResponseWriter. A second call is ignored, exactly
// as net/http ignores it, so the recorded status is the one actually on the
// wire rather than the last one attempted.
func (r *recorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	// 1xx responses are informational: net/http sends them and keeps the
	// connection in the pre-header state, so they must not be recorded as the
	// final status.
	if status >= 100 && status < 200 {
		r.ResponseWriter.WriteHeader(status)
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

// Write implements http.ResponseWriter.
func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// ReadFrom preserves the io.Copy fast path when the underlying writer has one.
func (r *recorder) ReadFrom(src io.Reader) (int64, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		r.written += n
		return n, err
	}
	n, err := io.Copy(r.ResponseWriter, src)
	r.written += n
	return n, err
}

// Unwrap lets http.ResponseController reach the original writer.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush implements http.Flusher when the underlying writer does.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		if !r.wroteHeader {
			r.WriteHeader(http.StatusOK)
		}
		f.Flush()
	}
}

// Hijack implements http.Hijacker when the underlying writer does.
func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("middleware: %w", errNotHijackable)
	}
	conn, rw, err := h.Hijack()
	if err == nil {
		// A hijacked connection leaves HTTP semantics behind: nothing more will
		// be written through this writer, so freeze the recorded status at
		// whatever the handler chose (101 for an upgrade, or 200 by default).
		r.wroteHeader = true
	}
	return conn, rw, err
}

var errNotHijackable = errors.New("underlying ResponseWriter does not implement http.Hijacker")

// statusClass renders a status as its class — "2xx", "4xx" — for a label that
// must stay small. Not used for the primary status label, which carries the
// exact code, but for the places where the exact code would multiply series for
// no analytical gain.
func statusClass(status int) string {
	switch {
	case status >= 100 && status < 200:
		return "1xx"
	case status < 300:
		return "2xx"
	case status < 400:
		return "3xx"
	case status < 500:
		return "4xx"
	case status < 600:
		return "5xx"
	default:
		return "unknown"
	}
}
