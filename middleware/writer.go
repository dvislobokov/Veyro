package middleware

import (
	"bufio"
	"net"
	"net/http"
)

// statusWriter records the status code and bytes written. It unwraps for
// http.ResponseController and passes Flush/Hijack through.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func wrapWriter(w http.ResponseWriter) *statusWriter {
	if sw, ok := w.(*statusWriter); ok {
		return sw
	}
	return &statusWriter{ResponseWriter: w}
}

func (s *statusWriter) Status() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}
