package veyro

import (
	"context"
	"net/http"
)

type (
	stateKey struct{}
	traceKey struct{}
)

type responseState struct {
	w      http.ResponseWriter
	r      *http.Request
	status int
}

func (s *responseState) statusOr(def int) int {
	if s.status != 0 {
		return s.status
	}
	return def
}

func state(ctx context.Context) *responseState {
	s, _ := ctx.Value(stateKey{}).(*responseState)
	return s
}

// RequestFrom returns the *http.Request of the current handler, or nil when
// ctx did not come from a veyro handler.
func RequestFrom(ctx context.Context) *http.Request {
	if s := state(ctx); s != nil {
		return s.r
	}
	return nil
}

// Header returns the response headers of the current handler so they can be
// set before the body is written. Returns nil outside a veyro handler.
func Header(ctx context.Context) http.Header {
	if s := state(ctx); s != nil {
		return s.w.Header()
	}
	return nil
}

// SetStatus sets the status code used when the handler's result is encoded.
func SetStatus(ctx context.Context, code int) {
	if s := state(ctx); s != nil {
		s.status = code
	}
}

// SetCookie adds a Set-Cookie header to the response.
func SetCookie(ctx context.Context, c *http.Cookie) {
	if s := state(ctx); s != nil {
		http.SetCookie(s.w, c)
	}
}

// Created sets 201 Created and, when location is non-empty, the Location header.
func Created(ctx context.Context, location string) {
	SetStatus(ctx, http.StatusCreated)
	if location != "" {
		if h := Header(ctx); h != nil {
			h.Set("Location", location)
		}
	}
}

// TraceID returns the request id for ctx. It is set for every request served
// by an App and echoed in problem responses and the X-Request-Id header.
func TraceID(ctx context.Context) string {
	id, _ := ctx.Value(traceKey{}).(string)
	return id
}
