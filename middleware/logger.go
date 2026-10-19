// Package middleware is Veyro's standard middleware set. Everything is a
// plain func(http.Handler) http.Handler, so it also works outside Veyro;
// error responses use Veyro problem details, and logging goes through srog.
package middleware

import (
	"net/http"
	"time"

	"github.com/dvislobokov/srog"
	"github.com/dvislobokov/veyro"
)

// LoggerOption customises Logger.
type LoggerOption func(*loggerConfig)

type loggerConfig struct {
	logger *srog.Logger
	skip   func(*http.Request) bool
	fields func(*http.Request, int) map[string]any
}

// WithLogger logs through l instead of the App logger.
func WithLogger(l *srog.Logger) LoggerOption { return func(c *loggerConfig) { c.logger = l } }

// Skip excludes requests for which fn returns true (health checks, metrics).
func Skip(fn func(*http.Request) bool) LoggerOption { return func(c *loggerConfig) { c.skip = fn } }

// SkipPaths excludes exact paths.
func SkipPaths(paths ...string) LoggerOption {
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	return Skip(func(r *http.Request) bool { return set[r.URL.Path] })
}

// WithFields adds extra structured fields to every completion log.
func WithFields(fn func(r *http.Request, status int) map[string]any) LoggerOption {
	return func(c *loggerConfig) { c.fields = fn }
}

// Logger logs one line per request through srog:
//
//	HTTP {Method} {Path} responded {Status} in {Elapsed}
//
// with RequestId, RemoteAddr, UserAgent, Bytes and, when authenticated,
// User. The level follows the status: 5xx Error, 4xx Warning, else
// Information. The logger comes from the App (veyro.LoggerFrom) unless
// WithLogger says otherwise.
func Logger(opts ...LoggerOption) veyro.Middleware {
	cfg := loggerConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.skip != nil && cfg.skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			sw := wrapWriter(w)
			next.ServeHTTP(sw, r)
			elapsed := time.Since(start)

			log := cfg.logger
			if log == nil {
				log = veyro.LoggerFrom(r.Context())
			} else if id := veyro.TraceID(r.Context()); id != "" {
				log = log.ForContext("RequestId", id)
			}
			fields := map[string]any{
				"RemoteAddr": r.RemoteAddr,
				"UserAgent":  r.UserAgent(),
				"Bytes":      sw.bytes,
			}
			if q := r.URL.RawQuery; q != "" {
				fields["Query"] = q
			}
			if u := veyro.UserFrom(r.Context()); u.IsAuthenticated() {
				fields["User"] = u.Name
			}
			if cfg.fields != nil {
				for k, v := range cfg.fields(r, sw.Status()) {
					fields[k] = v
				}
			}
			log = log.ForContextValues(fields)

			const tmpl = "HTTP {Method} {Path} responded {Status} in {Elapsed}"
			switch status := sw.Status(); {
			case status >= 500:
				log.Error(nil, tmpl, r.Method, r.URL.Path, status, elapsed)
			case status >= 400:
				log.Warning(tmpl, r.Method, r.URL.Path, status, elapsed)
			default:
				log.Information(tmpl, r.Method, r.URL.Path, status, elapsed)
			}
		})
	}
}
