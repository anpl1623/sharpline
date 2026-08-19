package middleware

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Forwarding headers this package understands.
//
// X-Real-IP is listed first because deploy/proxy/Caddyfile sets it with
// `header_up X-Real-IP {remote_host}` on both the /api and the /ws routes, and
// `header_up` with a single value REPLACES whatever the client sent. So behind
// this proxy it is authoritative in a way X-Forwarded-For is not.
const (
	headerRealIP       = "X-Real-IP"
	headerForwardedFor = "X-Forwarded-For"
	headerRequestID    = "X-Request-Id"
)

// TrustedProxies is the set of peers whose forwarding headers may be believed.
//
// # The rule this type exists to enforce
//
// A forwarded header is a claim made by whoever is on the other end of the
// socket. Believing it unconditionally means anyone who can reach the service
// can pick their own client IP, and per-IP rate limiting (CLAUDE.md §6) becomes
// an honour system: send a different X-Forwarded-For on every request and every
// request gets a fresh bucket.
//
// That is not hypothetical here. `api` listens on :8080 on the compose bridge
// network and on the pod network in Kubernetes. The proxy is the only PUBLISHED
// port (CLAUDE.md §9), but it is not the only thing that can open a socket to
// the API — any other container on the network can, and in a cluster a
// NetworkPolicy is what stops that rather than the absence of a route.
//
// So headers are believed only when the DIRECT PEER is in this set. An empty
// set means no header is ever believed and the peer address is used, which is
// the correct behaviour for a service reached directly and the safe default for
// one that is misconfigured.
//
// # What to put in it
//
// Exactly the hop in front of this service, and nothing else:
//
//	compose      the bridge subnet the `proxy` container sits on
//	Kubernetes   the ingress controller's pod CIDR
//
// Never a blanket private-range list. deploy/proxy/Caddyfile makes the same
// point about Caddy's own trusted_proxies and for the same reason: on a shared
// bridge network, "trust RFC1918" means "trust every container", which is every
// container that could be compromised.
type TrustedProxies []netip.Prefix

// ParseTrustedProxies parses CIDR strings into a TrustedProxies set. A bare
// address is accepted and treated as a single-host prefix.
//
// Configuration is validated at startup and fails loudly (CLAUDE.md §12), so a
// typo in a CIDR is a refusal to start rather than a limiter that silently
// buckets the whole internet under the proxy's address.
func ParseTrustedProxies(cidrs []string) (TrustedProxies, error) {
	out := make(TrustedProxies, 0, len(cidrs))
	for _, raw := range cidrs {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("%w: trusted proxy %q is neither a CIDR nor an address: %w",
				ErrInvalidOptions, s, err)
		}
		out = append(out, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
	}
	return out, nil
}

// contains reports whether addr is one of the trusted hops.
func (t TrustedProxies) contains(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	a := addr.Unmap()
	for _, p := range t {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientAddr determines the address to attribute a request to.
//
// The algorithm, stated so it can be argued with:
//
//  1. Parse the direct peer from RemoteAddr. If that fails, there is no address.
//  2. If the peer is NOT a trusted proxy, return it. Nothing it claims in a
//     header is believed, because a caller reaching this service directly can
//     claim anything.
//  3. The peer is trusted. Prefer X-Real-IP: this deployment's proxy overwrites
//     it, so a client-supplied value cannot survive the hop.
//  4. Otherwise walk X-Forwarded-For from the RIGHT, skipping entries that are
//     themselves trusted proxies, and take the first that is not. Right-to-left
//     is the only correct direction: entries are appended by each hop, so the
//     rightmost were written by infrastructure you control and the leftmost by
//     the client. Reading left-to-right — which is the common mistake — reads
//     the attacker's entry first.
//  5. If nothing usable is found, fall back to the peer.
func clientAddr(r *http.Request, trusted TrustedProxies) netip.Addr {
	peer := parseAddr(hostOf(r.RemoteAddr))
	if !peer.IsValid() || !trusted.contains(peer) {
		return peer
	}

	if a := parseAddr(strings.TrimSpace(r.Header.Get(headerRealIP))); a.IsValid() {
		return a
	}

	if xff := r.Header.Get(headerForwardedFor); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			a := parseAddr(strings.TrimSpace(parts[i]))
			if !a.IsValid() {
				continue
			}
			if trusted.contains(a) {
				continue
			}
			return a
		}
	}

	return peer
}

// hostOf strips the port from a RemoteAddr. A RemoteAddr with no port at all is
// what a unix-socket or an in-process listener produces, so it is handled rather
// than treated as malformed.
func hostOf(remoteAddr string) string {
	if remoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// parseAddr parses one address, normalising away the two representations that
// would otherwise split one client across two rate-limit buckets: an IPv6 zone
// suffix, and the IPv4-mapped IPv6 form (::ffff:1.2.3.4).
func parseAddr(s string) netip.Addr {
	if s == "" {
		return netip.Addr{}
	}
	s = strings.TrimPrefix(strings.TrimSuffix(s, "]"), "[")
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap().WithZone("")
}

// rateLimitSubject renders an address as the subject a per-IP bucket is keyed
// by.
//
// IPv4 is keyed per address. IPv6 is keyed per /64, and that difference is
// load-bearing rather than fussy: a residential IPv6 customer is typically
// delegated a /64 or larger, so per-address IPv6 limiting is defeated by
// incrementing the host part — an effectively unlimited supply of fresh buckets
// from one subscriber line. /64 is the smallest allocation an operator hands
// out, so it is the narrowest unit that still corresponds to "one customer".
//
// An invalid address (no RemoteAddr, no trusted header) collapses to a single
// "unknown" bucket. That is deliberate: the alternative — exempting it — turns
// an unparseable address into a bypass.
func rateLimitSubject(addr netip.Addr) string {
	if !addr.IsValid() {
		return "unknown"
	}
	if addr.Is4() {
		return addr.String()
	}
	p, err := addr.Prefix(64)
	if err != nil {
		return addr.String()
	}
	return p.String()
}
