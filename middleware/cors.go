package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dvislobokov/veyro"
)

// CORSOptions configures CORS. The zero value allows nothing; use
// CORSAllowAll for development.
type CORSOptions struct {
	// AllowOrigins lists allowed origins exactly ("https://app.example.com")
	// or with a single leading wildcard ("https://*.example.com"). "*" allows
	// any origin (then credentials cannot be allowed).
	AllowOrigins []string
	// AllowOriginFunc decides dynamically; it wins over AllowOrigins when set.
	AllowOriginFunc func(origin string) bool
	// AllowMethods defaults to GET, HEAD, POST, PUT, PATCH, DELETE.
	AllowMethods []string
	// AllowHeaders lists request headers clients may send; "*" reflects
	// whatever the preflight asks for.
	AllowHeaders []string
	// ExposeHeaders lists response headers scripts may read.
	ExposeHeaders []string
	// AllowCredentials sets Access-Control-Allow-Credentials.
	AllowCredentials bool
	// MaxAge caches preflight results in the browser.
	MaxAge time.Duration
}

// CORSAllowAll is a permissive configuration for local development.
func CORSAllowAll() CORSOptions {
	return CORSOptions{AllowOrigins: []string{"*"}, AllowHeaders: []string{"*"}, MaxAge: 12 * time.Hour}
}

// CORS answers preflight requests and adds CORS headers to responses.
// Install it on the App so preflights for unknown routes are handled too.
func CORS(o CORSOptions) veyro.Middleware {
	if len(o.AllowMethods) == 0 {
		o.AllowMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}
	}
	methods := strings.Join(o.AllowMethods, ", ")
	headers := strings.Join(o.AllowHeaders, ", ")
	expose := strings.Join(o.ExposeHeaders, ", ")
	anyOrigin := false
	for _, a := range o.AllowOrigins {
		if a == "*" {
			anyOrigin = true
		}
	}
	allowed := func(origin string) bool {
		switch {
		case o.AllowOriginFunc != nil:
			return o.AllowOriginFunc(origin)
		case anyOrigin:
			return true
		}
		for _, a := range o.AllowOrigins {
			if a == origin {
				return true
			}
			if scheme, host, ok := strings.Cut(a, "://*"); ok {
				if strings.HasPrefix(origin, scheme+"://") && strings.HasSuffix(origin, host) && len(origin) > len(scheme)+3+len(host) {
					return true
				}
			}
		}
		return false
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Add("Vary", "Origin")
			if !allowed(origin) {
				next.ServeHTTP(w, r)
				return
			}
			if anyOrigin && !o.AllowCredentials && o.AllowOriginFunc == nil {
				h.Set("Access-Control-Allow-Origin", "*")
			} else {
				h.Set("Access-Control-Allow-Origin", origin)
			}
			if o.AllowCredentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				h.Set("Access-Control-Allow-Methods", methods)
				if headers == "*" {
					if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
						h.Set("Access-Control-Allow-Headers", req)
					}
				} else if headers != "" {
					h.Set("Access-Control-Allow-Headers", headers)
				}
				if o.MaxAge > 0 {
					h.Set("Access-Control-Max-Age", strconv.Itoa(int(o.MaxAge.Seconds())))
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if expose != "" {
				h.Set("Access-Control-Expose-Headers", expose)
			}
			next.ServeHTTP(w, r)
		})
	}
}
