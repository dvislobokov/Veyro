// Package veyro is a small HTTP framework for Go 1.27+.
//
// Handlers are plain typed functions:
//
//	func createUser(c *veyro.Context, in CreateUser) (User, error)
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
	"reflect"
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
	mux     *http.ServeMux
	cfg     config
	once    sync.Once
	chain   http.Handler
	routes  []*Route
	schemes []Authenticator
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

type appKey struct{}

func appFrom(ctx context.Context) *App {
	a, _ := ctx.Value(appKey{}).(*App)
	return a
}

// UseAuthentication installs Authentication(auths...) as app middleware and
// records the schemes so the OpenAPI document can describe them.
func (a *App) UseAuthentication(auths ...Authenticator) *App {
	a.schemes = append(a.schemes, auths...)
	a.Use(Authentication(auths...))
	return a
}

// Router is a set of routes sharing a path prefix, middleware and
// authorization requirements. App embeds the root Router.
type Router struct {
	app          *App
	parent       *Router
	prefix       string
	mw           []Middleware
	requiresAuth bool
	tags         []string
}

func (g *Router) isRoot() bool { return g == &g.app.Router }

// Group creates a sub-router. Middleware and requirements added to any
// ancestor apply, whenever they are added; the root App's middleware wraps
// every request including 404s.
func (g *Router) Group(prefix string, mw ...Middleware) *Router {
	return &Router{app: g.app, parent: g, prefix: g.prefix + prefix, mw: mw}
}

// Use appends middleware to this router.
func (g *Router) Use(mw ...Middleware) *Router {
	g.mw = append(g.mw, mw...)
	return g
}

// RequireAuthorization requires an authenticated user that satisfies every
// policy for all routes in this router.
func (g *Router) RequireAuthorization(policies ...Policy) *Router {
	g.requiresAuth = true
	return g.Use(Authorize(policies...))
}

// RequireRole requires an authenticated user with one of the roles.
func (g *Router) RequireRole(roles ...string) *Router {
	return g.RequireAuthorization(InRole(roles...))
}

// RequireClaim requires an authenticated user with the claim.
func (g *Router) RequireClaim(typ string, values ...string) *Router {
	return g.RequireAuthorization(HasClaim(typ, values...))
}

// Tags sets the OpenAPI tags for every route in this router.
func (g *Router) Tags(tags ...string) *Router {
	g.tags = append(g.tags, tags...)
	return g
}

func (g *Router) inheritsAuth() bool {
	for r := g; r != nil; r = r.parent {
		if r.requiresAuth {
			return true
		}
	}
	return false
}

func (g *Router) inheritedTags() []string {
	if g.isRoot() {
		return g.tags
	}
	return append(g.parent.inheritedTags(), g.tags...)
}

// collect returns the middleware of this router and its ancestors, outermost
// first, excluding the root (applied in App.ServeHTTP).
func (g *Router) collect() []Middleware {
	if g.isRoot() {
		return nil
	}
	return append(g.parent.collect(), g.mw...)
}

// Handle registers a typed handler. The pattern uses net/http.ServeMux syntax
// ("/users/{id}", "/files/{path...}", "/{$}"). In and Out are inferred from h.
//
// In is bound from the request: fields tagged path, query, header or cookie
// come from those sources; everything else is decoded from the JSON body.
// Use None for handlers without input; returning None yields 204 No Content.
func (g *Router) Handle[In, Out any](method, pattern string, h Handler[In, Out]) *Route {
	full := g.prefix + pattern
	hh, b := adapt(g.app, full, h)
	rt := g.route(method, full, hh)
	rt.in, rt.out, rt.binder = reflect.TypeFor[In](), reflect.TypeFor[Out](), b
	return rt
}

// Get registers a GET route.
func (g *Router) Get[In, Out any](pattern string, h Handler[In, Out]) *Route {
	return g.Handle(http.MethodGet, pattern, h)
}

// Post registers a POST route.
func (g *Router) Post[In, Out any](pattern string, h Handler[In, Out]) *Route {
	return g.Handle(http.MethodPost, pattern, h)
}

// Put registers a PUT route.
func (g *Router) Put[In, Out any](pattern string, h Handler[In, Out]) *Route {
	return g.Handle(http.MethodPut, pattern, h)
}

// Patch registers a PATCH route.
func (g *Router) Patch[In, Out any](pattern string, h Handler[In, Out]) *Route {
	return g.Handle(http.MethodPatch, pattern, h)
}

// Delete registers a DELETE route.
func (g *Router) Delete[In, Out any](pattern string, h Handler[In, Out]) *Route {
	return g.Handle(http.MethodDelete, pattern, h)
}

// Raw registers a plain http.Handler. An empty method matches all methods.
func (g *Router) Raw(method, pattern string, h http.Handler) *Route {
	return g.route(method, g.prefix+pattern, h)
}

func (g *Router) route(method, full string, h http.Handler) *Route {
	rt := &Route{router: g, Method: method, Pattern: full, handler: h}
	if method != "" {
		full = method + " " + full
	}
	g.app.mux.Handle(full, rt)
	g.app.routes = append(g.app.routes, rt)
	return rt
}

// Route is a registered endpoint. Its methods configure the endpoint fluently
// until the first request arrives:
//
//	app.Get("/admin", dashboard).RequireRole("admin")
type Route struct {
	Method  string
	Pattern string

	router       *Router
	handler      http.Handler
	mw           []Middleware
	anonymous    bool
	requiresAuth bool

	// OpenAPI metadata.
	in, out reflect.Type
	binder  *binder
	meta    routeMeta

	once  sync.Once
	chain http.Handler
}

type routeMeta struct {
	summary, description, operationID string
	tags                              []string
	success                           int
	errors                            []int
	deprecated, hidden                bool
}

type routeKey struct{}

// RouteFrom returns the matched route for a request context, or nil.
func RouteFrom(ctx context.Context) *Route {
	rt, _ := ctx.Value(routeKey{}).(*Route)
	return rt
}

// Use adds middleware to this route only.
func (rt *Route) Use(mw ...Middleware) *Route {
	rt.mw = append(rt.mw, mw...)
	return rt
}

// RequireAuthorization requires an authenticated user satisfying every policy.
func (rt *Route) RequireAuthorization(policies ...Policy) *Route {
	rt.requiresAuth = true
	return rt.Use(Authorize(policies...))
}

// RequireRole requires an authenticated user with one of the roles.
func (rt *Route) RequireRole(roles ...string) *Route {
	return rt.RequireAuthorization(InRole(roles...))
}

// RequireClaim requires an authenticated user with the claim.
func (rt *Route) RequireClaim(typ string, values ...string) *Route {
	return rt.RequireAuthorization(HasClaim(typ, values...))
}

// Summary sets the OpenAPI summary.
func (rt *Route) Summary(s string) *Route { rt.meta.summary = s; return rt }

// Description sets the OpenAPI description.
func (rt *Route) Description(s string) *Route { rt.meta.description = s; return rt }

// OperationID sets the OpenAPI operationId (default: derived from method and path).
func (rt *Route) OperationID(id string) *Route { rt.meta.operationID = id; return rt }

// Tags adds OpenAPI tags.
func (rt *Route) Tags(tags ...string) *Route { rt.meta.tags = append(rt.meta.tags, tags...); return rt }

// Success declares the success status code documented for this route
// (default 200, or 204 when Out is None). It does not change behaviour;
// use c.Status or c.Created in the handler for that.
func (rt *Route) Success(status int) *Route { rt.meta.success = status; return rt }

// Errors documents additional problem responses, e.g. Errors(404, 409).
func (rt *Route) Errors(statuses ...int) *Route {
	rt.meta.errors = append(rt.meta.errors, statuses...)
	return rt
}

// Deprecated marks the operation deprecated in the spec.
func (rt *Route) Deprecated() *Route { rt.meta.deprecated = true; return rt }

// Hide excludes the route from the OpenAPI document.
func (rt *Route) Hide() *Route { rt.meta.hidden = true; return rt }

func (rt *Route) secured() bool {
	return !rt.anonymous && (rt.requiresAuth || rt.router.inheritsAuth())
}

// AllowAnonymous exempts this route from Authorize requirements inherited
// from its routers, like [AllowAnonymous] in ASP.NET Core.
func (rt *Route) AllowAnonymous() *Route {
	rt.anonymous = true
	return rt
}

// ServeHTTP builds the middleware chain on first use and serves the request.
func (rt *Route) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt.once.Do(func() {
		mw := append(rt.router.collect(), rt.mw...)
		rt.chain = wrap(rt.handler, mw)
	})
	r = r.WithContext(context.WithValue(r.Context(), routeKey{}, rt))
	rt.chain.ServeHTTP(w, r)
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
	ctx := context.WithValue(r.Context(), traceKey{}, id)
	ctx = context.WithValue(ctx, appKey{}, a)
	r = r.WithContext(ctx)

	defer func() {
		if rec := recover(); rec != nil {
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			a.cfg.logger.ErrorContext(ctx, "veyro: panic",
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
