// Package veyro is a small HTTP framework for Go 1.27+.
//
// Handlers are plain typed functions:
//
//	func createUser(ctx context.Context, in CreateUser) (User, error)
//
// Request data (path parameters, query, headers, cookies and the JSON body)
// is bound into a single input struct, validated, and the result is encoded
// as JSON. Errors are reported as RFC 9457 problem details in the same shape
// ASP.NET Core uses.
package veyro

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"
	"uuid"
)

// Middleware wraps an http.Handler. Any middleware written for net/http works.
type Middleware func(http.Handler) http.Handler

// Option configures an App.
type Option func(*config)

type config struct {
	logger       *slog.Logger
	jsonOpts     []json.Options
	exposeErrors bool
	maxBody      int64
	requestID    func(*http.Request) string
}

// WithLogger sets the logger used for unhandled errors and panics.
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.logger = l } }

// WithJSONOptions sets encoding/json/v2 options used for request and response bodies.
func WithJSONOptions(opts ...json.Options) Option {
	return func(c *config) { c.jsonOpts = append(c.jsonOpts, opts...) }
}

// ExposeErrors includes the text of unhandled errors in 500 responses.
// Useful in development, dangerous in production.
func ExposeErrors(on bool) Option { return func(c *config) { c.exposeErrors = on } }

// WithMaxBodyBytes limits the size of request bodies. Default is 4 MiB.
func WithMaxBodyBytes(n int64) Option { return func(c *config) { c.maxBody = n } }

// WithRequestID replaces the request/trace id generator. The default honours
// an incoming X-Request-Id header and otherwise generates a UUIDv7.
func WithRequestID(fn func(*http.Request) string) Option {
	return func(c *config) { c.requestID = fn }
}

// App is the root router. It implements http.Handler.
type App struct {
	Router
	mux   *http.ServeMux
	cfg   config
	once  sync.Once
	chain http.Handler
}

// New creates an App.
func New(opts ...Option) *App {
	app := &App{
		mux: http.NewServeMux(),
		cfg: config{
			logger:    slog.Default(),
			maxBody:   4 << 20,
			requestID: defaultRequestID,
		},
	}
	for _, o := range opts {
		o(&app.cfg)
	}
	app.Router = Router{app: app}
	return app
}

func defaultRequestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); id != "" {
		return id
	}
	return uuid.NewV7().String()
}

// Router is a set of routes sharing a path prefix and middleware. App embeds the root Router.
type Router struct {
	app    *App
	prefix string
	mw     []Middleware
}

func (g *Router) isRoot() bool { return g == &g.app.Router }

// Group creates a sub-group. Middleware of the parent (except the root App,
// whose middleware wraps every request) is inherited.
func (g *Router) Group(prefix string, mw ...Middleware) *Router {
	ng := &Router{app: g.app, prefix: g.prefix + prefix}
	if !g.isRoot() {
		ng.mw = slices.Clone(g.mw)
	}
	ng.mw = append(ng.mw, mw...)
	return ng
}

// Use appends middleware. On the App it applies to every request, including
// 404/405 responses; on a sub-group it applies to routes registered afterwards.
func (g *Router) Use(mw ...Middleware) { g.mw = append(g.mw, mw...) }

// Handle registers a typed handler. The pattern uses net/http.ServeMux syntax
// ("/users/{id}", "/files/{path...}", "/{$}"). In and Out are inferred from h.
//
// In is bound from the request: fields tagged path, query, header or cookie
// come from those sources; everything else is decoded from the JSON body.
// Use None for handlers without input; returning None yields 204 No Content.
func (g *Router) Handle[In, Out any](method, pattern string, h Handler[In, Out]) {
	full := g.prefix + pattern
	g.handle(method, full, adapt(g.app, full, h))
}

// Get registers a GET route.
func (g *Router) Get[In, Out any](pattern string, h Handler[In, Out]) {
	g.Handle(http.MethodGet, pattern, h)
}

// Post registers a POST route.
func (g *Router) Post[In, Out any](pattern string, h Handler[In, Out]) {
	g.Handle(http.MethodPost, pattern, h)
}

// Put registers a PUT route.
func (g *Router) Put[In, Out any](pattern string, h Handler[In, Out]) {
	g.Handle(http.MethodPut, pattern, h)
}

// Patch registers a PATCH route.
func (g *Router) Patch[In, Out any](pattern string, h Handler[In, Out]) {
	g.Handle(http.MethodPatch, pattern, h)
}

// Delete registers a DELETE route.
func (g *Router) Delete[In, Out any](pattern string, h Handler[In, Out]) {
	g.Handle(http.MethodDelete, pattern, h)
}

// Raw registers a plain http.Handler. An empty method matches all methods.
func (g *Router) Raw(method, pattern string, h http.Handler) {
	g.handle(method, g.prefix+pattern, h)
}

func (g *Router) handle(method, full string, h http.Handler) {
	if !g.isRoot() {
		h = wrap(h, g.mw)
	}
	if method != "" {
		full = method + " " + full
	}
	g.app.mux.Handle(full, h)
}

func wrap(h http.Handler, mw []Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// ServeHTTP implements http.Handler.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.once.Do(func() { a.chain = wrap(http.HandlerFunc(a.serve), a.mw) })

	id := a.cfg.requestID(r)
	w.Header().Set("X-Request-Id", id)
	r = r.WithContext(context.WithValue(r.Context(), traceKey{}, id))

	defer func() {
		if rec := recover(); rec != nil {
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			a.cfg.logger.ErrorContext(r.Context(), "veyro: panic",
				"panic", rec, "method", r.Method, "path", r.URL.Path, "traceId", id)
			a.writeError(w, r, errors.New("panic recovered"))
		}
	}()
	a.chain.ServeHTTP(w, r)
}

// serve dispatches to the mux, turning its plain-text 404/405 into problems.
func (a *App) serve(w http.ResponseWriter, r *http.Request) {
	h, pattern := a.mux.Handler(r)
	if pattern != "" {
		// ServeMux.ServeHTTP matches again, but it is the only way to get
		// path values populated on r.
		a.mux.ServeHTTP(w, r)
		return
	}
	probe := &probeWriter{header: http.Header{}}
	h.ServeHTTP(probe, r)
	switch probe.status {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		if allow := probe.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		a.writeProblem(w, r, NewProblem(probe.status, ""))
	default:
		h.ServeHTTP(w, r)
	}
}

// probeWriter records the status the mux would send without writing anything.
type probeWriter struct {
	header http.Header
	status int
}

func (p *probeWriter) Header() http.Header { return p.header }
func (p *probeWriter) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}
func (p *probeWriter) Write(b []byte) (int, error) {
	p.WriteHeader(http.StatusOK)
	return len(b), nil
}

// Run serves on addr until ctx is cancelled, then shuts down gracefully.
func (a *App) Run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           a,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
