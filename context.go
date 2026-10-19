package veyro

import (
	"context"
	"log/slog"
	"net/http"
)

type ctxKey struct{}

// Context is the per-request context handed to handlers. It implements
// context.Context (it is the request context plus a reference to itself), so
// it can be passed straight to databases, clients and anything else that
// takes a context.Context.
//
// It plays the role of HttpContext in ASP.NET Core: request, response
// controls, the authenticated User, the request id and a logger.
type Context struct {
	context.Context

	// Request is the incoming request.
	Request *http.Request

	app    *App
	w      http.ResponseWriter
	status int
	user   *User
}

func newContext(app *App, w http.ResponseWriter, r *http.Request) *Context {
	c := &Context{app: app, w: w}
	c.Context = context.WithValue(r.Context(), ctxKey{}, c)
	c.Request = r.WithContext(c.Context)
	return c
}

// ContextFrom returns the *Context stored in ctx, or nil when ctx did not
// originate from a Veyro handler. It works on derived contexts too
// (context.WithTimeout(c, ...) and friends).
func ContextFrom(ctx context.Context) *Context {
	c, _ := ctx.Value(ctxKey{}).(*Context)
	return c
}

// Writer exposes the underlying http.ResponseWriter for cases the typed API
// does not cover. Writing to it directly bypasses JSON encoding.
func (c *Context) Writer() http.ResponseWriter { return c.w }

// Header returns the response headers.
func (c *Context) Header() http.Header { return c.w.Header() }

// Status sets the status code used when the handler result is encoded.
func (c *Context) Status(code int) *Context {
	c.status = code
	return c
}

// Created sets 201 Created and, when location is non-empty, the Location header.
func (c *Context) Created(location string) *Context {
	c.status = http.StatusCreated
	if location != "" {
		c.w.Header().Set("Location", location)
	}
	return c
}

// SetCookie adds a Set-Cookie header.
func (c *Context) SetCookie(cookie *http.Cookie) { http.SetCookie(c.w, cookie) }

// TraceID returns the request id, also sent as X-Request-Id and in problems.
func (c *Context) TraceID() string { return TraceID(c.Context) }

// Logger returns the application logger with the trace id attached.
func (c *Context) Logger() *slog.Logger {
	return c.app.cfg.logger.With("traceId", c.TraceID())
}

// User returns the authenticated user, or an anonymous user when no
// Authentication middleware set one. It never returns nil.
func (c *Context) User() *User {
	if c.user != nil {
		return c.user
	}
	if u := UserFrom(c.Context); u != nil {
		return u
	}
	return anonymous
}

// SetUser replaces the user for the rest of this request (sign-in flows).
func (c *Context) SetUser(u *User) { c.user = u }

// Param returns a path parameter by name.
func (c *Context) Param(name string) string { return c.Request.PathValue(name) }

// Query returns the first query value for key.
func (c *Context) Query(key string) string { return c.Request.URL.Query().Get(key) }

func (c *Context) statusOr(def int) int {
	if c.status != 0 {
		return c.status
	}
	return def
}

// TraceID returns the request id for any context derived from a request
// served by an App. Prefer Context.TraceID inside handlers.
func TraceID(ctx context.Context) string {
	if i := infoFrom(ctx); i != nil {
		return i.traceID
	}
	return ""
}
