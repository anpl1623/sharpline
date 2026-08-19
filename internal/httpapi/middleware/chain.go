package middleware

import "net/http"

// Middleware is one link in the chain.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares to h so that the FIRST listed runs FIRST — i.e. it
// is the outermost wrapper.
//
// The reversal is the whole reason this helper exists. Written by hand, wrapping
// reads inside-out (`a(b(c(h)))` runs a, then b, then c) and the order in the
// source is the reverse of the order in execution, which is exactly the kind of
// thing that gets a rate limiter placed after authentication by accident.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		if mw[i] == nil {
			continue
		}
		h = mw[i](h)
	}
	return h
}
