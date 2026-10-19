package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/dvislobokov/veyro"
)

// SecureHeadersOptions configures SecureHeaders. Zero values use safe
// defaults; set a field to "-" to omit that header.
type SecureHeadersOptions struct {
	ContentTypeOptions      string        // default "nosniff"
	FrameOptions            string        // default "DENY"
	ReferrerPolicy          string        // default "strict-origin-when-cross-origin"
	ContentSecurityPolicy   string        // default none
	PermissionsPolicy       string        // default none
	CrossOriginOpenerPolicy string        // default "same-origin"
	HSTSMaxAge              time.Duration // default none; sent on TLS requests only
	HSTSIncludeSubdomains   bool
	HSTSPreload             bool
}

// SecureHeaders adds the usual browser hardening headers.
func SecureHeaders(o SecureHeadersOptions) veyro.Middleware {
	def := func(v, d string) string {
		switch v {
		case "":
			return d
		case "-":
			return ""
		}
		return v
	}
	cto := def(o.ContentTypeOptions, "nosniff")
	xfo := def(o.FrameOptions, "DENY")
	ref := def(o.ReferrerPolicy, "strict-origin-when-cross-origin")
	coop := def(o.CrossOriginOpenerPolicy, "same-origin")
	csp := def(o.ContentSecurityPolicy, "")
	pp := def(o.PermissionsPolicy, "")
	hsts := ""
	if o.HSTSMaxAge > 0 {
		hsts = "max-age=" + strconv.Itoa(int(o.HSTSMaxAge.Seconds()))
		if o.HSTSIncludeSubdomains {
			hsts += "; includeSubDomains"
		}
		if o.HSTSPreload {
			hsts += "; preload"
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			set := func(k, v string) {
				if v != "" {
					h.Set(k, v)
				}
			}
			set("X-Content-Type-Options", cto)
			set("X-Frame-Options", xfo)
			set("Referrer-Policy", ref)
			set("Cross-Origin-Opener-Policy", coop)
			set("Content-Security-Policy", csp)
			set("Permissions-Policy", pp)
			if hsts != "" && (r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https") {
				h.Set("Strict-Transport-Security", hsts)
			}
			next.ServeHTTP(w, r)
		})
	}
}
