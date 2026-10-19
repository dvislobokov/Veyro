package middleware

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/dvislobokov/veyro"
)

// Timeout bounds the time a handler may take. The handler runs with a
// context that is cancelled at the deadline; if it has not finished by
// then, the client gets a 504 problem and whatever the handler writes
// afterwards is discarded. Handlers that stream (SSE, large downloads)
// should not sit behind this middleware.
func Timeout(d time.Duration) veyro.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			r = r.WithContext(ctx)

			tw := &timeoutWriter{header: http.Header{}}
			done := make(chan struct{})
			panicked := make(chan any, 1)
			go func() {
				defer func() {
					if p := recover(); p != nil {
						panicked <- p
					}
					close(done)
				}()
				next.ServeHTTP(tw, r)
			}()

			select {
			case <-done:
				select {
				case p := <-panicked:
					panic(p) // let the App's recovery produce the 500
				default:
				}
				tw.mu.Lock()
				defer tw.mu.Unlock()
				dst := w.Header()
				for k, v := range tw.header {
					dst[k] = v
				}
				w.WriteHeader(tw.Status())
				_, _ = w.Write(tw.body.Bytes())
			case <-ctx.Done():
				tw.mu.Lock()
				tw.timedOut = true
				tw.mu.Unlock()
				writeProblem(w, r, veyro.NewProblem(http.StatusGatewayTimeout,
					"The request did not complete within "+d.String()+"."))
			}
		})
	}
}

// timeoutWriter buffers the response so nothing reaches the client before
// the handler is known to have finished in time.
type timeoutWriter struct {
	mu       sync.Mutex
	header   http.Header
	body     bytes.Buffer
	status   int
	timedOut bool
}

func (t *timeoutWriter) Header() http.Header { return t.header }

func (t *timeoutWriter) Status() int {
	if t.status == 0 {
		return http.StatusOK
	}
	return t.status
}

func (t *timeoutWriter) WriteHeader(code int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status == 0 && !t.timedOut {
		t.status = code
	}
}

func (t *timeoutWriter) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timedOut {
		return 0, http.ErrHandlerTimeout
	}
	if t.status == 0 {
		t.status = http.StatusOK
	}
	return t.body.Write(b)
}

// writeProblem answers with a problem through the App when there is one in
// the context, else with a minimal problem+json body.
func writeProblem(w http.ResponseWriter, r *http.Request, p *veyro.Problem) {
	veyro.WriteProblem(w, r, p)
}
