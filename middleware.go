package veyro

import (
	"log/slog"
	"net/http"
	"time"
)

// Logger returns middleware that logs one line per request with slog.
func Logger(l *slog.Logger) Middleware {
	if l == nil {
		l = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			l.InfoContext(r.Context(), "request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status(),
				"duration", time.Since(start),
				"traceId", TraceID(r.Context()),
			)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) status() int {
	if s.code == 0 {
		return http.StatusOK
	}
	return s.code
}

func (s *statusWriter) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
