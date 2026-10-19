package veyro

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"reflect"
	"sync"
)

// Handler is the shape of every veyro handler. *Context implements
// context.Context, so c can be passed on to anything that needs one.
type Handler[In, Out any] func(c *Context, in In) (Out, error)

// None marks "no input" or "no output". A handler returning None answers
// 204 No Content unless SetStatus was called.
type None struct{}

// Renderer lets an output type write the response itself (redirects, files,
// streams, HTML). When Out implements Renderer, JSON encoding is skipped.
type Renderer interface {
	Render(w http.ResponseWriter, r *http.Request) error
}

// Validator is implemented by input types that need custom validation.
// Return a ValidationErrors to report field errors, a *Problem to answer with
// a specific status, or any other error to fail with 500.
type Validator interface {
	Validate(ctx context.Context) error
}

var noneType = reflect.TypeFor[None]()

func adapt[In, Out any](app *App, pattern string, h Handler[In, Out]) (http.Handler, *binder) {
	b := newBinder(reflect.TypeFor[In](), pattern, &app.cfg)
	outNone := reflect.TypeFor[Out]() == noneType

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := newContext(app, w, r)
		r = c.Request

		var in In
		if !b.isNone {
			errs, err := b.bind(r, reflect.ValueOf(&in).Elem())
			if err != nil {
				app.writeError(w, r, err)
				return
			}
			validateValue(reflect.ValueOf(&in).Elem(), "", &errs)
			if v, ok := any(&in).(Validator); ok {
				if err := v.Validate(c); err != nil {
					var ve ValidationErrors
					if errors.As(err, &ve) {
						if errs == nil {
							errs = ve
						} else {
							errs.merge(ve)
						}
					} else {
						app.writeError(w, r, err)
						return
					}
				}
			}
			if len(errs) > 0 {
				app.writeProblem(w, r, errs.Problem())
				return
			}
		}

		out, err := h(c, in)
		if err != nil {
			app.writeError(w, r, err)
			return
		}
		app.render(c, out, outNone)
	}), b
}

func (a *App) render(c *Context, out any, outNone bool) {
	w, r := c.w, c.Request
	if rd, ok := out.(Renderer); ok {
		if err := rd.Render(w, r); err != nil {
			a.cfg.logger.ErrorContext(r.Context(), "veyro: render failed", "err", err)
		}
		return
	}
	if outNone {
		w.WriteHeader(c.statusOr(http.StatusNoContent))
		return
	}
	buf := getBuffer()
	defer putBuffer(buf)
	if err := json.MarshalWrite(buf, out, a.cfg.jsonOpts...); err != nil {
		a.writeError(w, r, err)
		return
	}
	h := w.Header()
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(c.statusOr(http.StatusOK))
	_, _ = w.Write(buf.Bytes())
}

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func getBuffer() *bytes.Buffer {
	b := bufPool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

func putBuffer(b *bytes.Buffer) {
	if b.Cap() <= 64<<10 { // do not keep huge buffers alive
		bufPool.Put(b)
	}
}

// writeError maps an error to a problem response.
func (a *App) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		p   *Problem
		ve  ValidationErrors
		mbe *http.MaxBytesError
	)
	switch {
	case errors.As(err, &p):
	case errors.As(err, &ve):
		p = ve.Problem()
	case errors.As(err, &mbe):
		p = NewProblem(http.StatusRequestEntityTooLarge, "")
	default:
		a.cfg.logger.ErrorContext(r.Context(), "veyro: unhandled error",
			"err", err, "method", r.Method, "path", r.URL.Path, "traceId", TraceID(r.Context()))
		p = NewProblem(http.StatusInternalServerError, "")
		if a.cfg.exposeErrors {
			p.Detail = err.Error()
		}
	}
	a.writeProblem(w, r, p)
}

func (a *App) writeProblem(w http.ResponseWriter, r *http.Request, p *Problem) {
	q := *p // never mutate the caller's value
	if q.Status == 0 {
		q.Status = http.StatusInternalServerError
	}
	if q.TraceID == "" {
		q.TraceID = TraceID(r.Context())
	}
	buf := getBuffer()
	defer putBuffer(buf)
	if err := json.MarshalWrite(buf, &q, json.Deterministic(true)); err != nil {
		a.cfg.logger.ErrorContext(r.Context(), "veyro: cannot encode problem", "err", err)
		http.Error(w, http.StatusText(q.Status), q.Status)
		return
	}
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(q.Status)
	_, _ = w.Write(buf.Bytes())
}
