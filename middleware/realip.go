package middleware

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/dvislobokov/veyro"
)

// RealIP rewrites r.RemoteAddr to the client address taken from
// X-Forwarded-For (right-most untrusted hop) or X-Real-IP, but only when
// the direct peer is one of the trusted proxies. Without trusted proxies the
// headers are ignored, since clients can forge them.
//
//	app.Use(middleware.RealIP(netip.MustParsePrefix("10.0.0.0/8")))
func RealIP(trusted ...netip.Prefix) veyro.Middleware {
	isTrusted := func(ip netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(ip.Unmap()) {
				return true
			}
		}
		return false
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			peer, err := netip.ParseAddr(host)
			if err != nil || !isTrusted(peer) {
				next.ServeHTTP(w, r)
				return
			}
			if ip := clientFromForwarded(r.Header.Values("X-Forwarded-For"), isTrusted); ip != "" {
				r.RemoteAddr = net.JoinHostPort(ip, "0")
			} else if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
				if _, err := netip.ParseAddr(ip); err == nil {
					r.RemoteAddr = net.JoinHostPort(ip, "0")
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientFromForwarded walks X-Forwarded-For from the right, skipping
// trusted proxies, and returns the first untrusted address.
func clientFromForwarded(values []string, isTrusted func(netip.Addr) bool) string {
	var hops []string
	for _, v := range values {
		for h := range strings.SplitSeq(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		ip, err := netip.ParseAddr(hops[i])
		if err != nil {
			return ""
		}
		if !isTrusted(ip) {
			return ip.String()
		}
	}
	if len(hops) > 0 { // every hop trusted: the left-most is the client
		return hops[0]
	}
	return ""
}
