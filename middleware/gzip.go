package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/dvislobokov/veyro"
)

// GzipOptions configures Gzip.
type GzipOptions struct {
	// Level is a compress/gzip level; 0 means gzip.DefaultCompression.
	Level int
	// MinSize skips compression for bodies known to be smaller (via
	// Content-Length). Default 1024.
	MinSize int
	// ContentTypes restricts compression to these media types (prefix
	// match, parameters ignored). Default: text/*, JSON, JavaScript, XML,
	// SVG and problem+json.
	ContentTypes []string
}

var defaultGzipTypes = []string{
	"text/", "application/json", "application/problem+json", "application/javascript",
	"application/xml", "image/svg+xml", "application/x-ndjson",
}

// Gzip compresses responses for clients that send Accept-Encoding: gzip.
// Already-encoded responses, small responses and non-text media types are
// left alone. Streaming handlers keep working: Flush flushes the gzip
// stream.
func Gzip(o GzipOptions) veyro.Middleware {
	if o.Level == 0 {
		o.Level = gzip.DefaultCompression
	}
	if o.MinSize == 0 {
		o.MinSize = 1024
	}
	if len(o.ContentTypes) == 0 {
		o.ContentTypes = defaultGzipTypes
	}
	pool := sync.Pool{New: func() any {
		zw, _ := gzip.NewWriterLevel(io.Discard, o.Level)
		return zw
	}}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !acceptsGzip(r) || r.Header.Get("Range") != "" {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Add("Vary", "Accept-Encoding")
			gw := &gzipWriter{ResponseWriter: w, opts: &o, pool: &pool}
			defer gw.close()
			next.ServeHTTP(gw, r)
		})
	}
}

func acceptsGzip(r *http.Request) bool {
	for enc := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(enc), ";")
		if strings.TrimSpace(name) == "gzip" {
			q := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(params), "q="))
			return q != "0" && q != "0.0"
		}
	}
	return false
}

type gzipWriter struct {
	http.ResponseWriter
	opts    *GzipOptions
	pool    *sync.Pool
	zw      *gzip.Writer
	buf     []byte // bytes held back until MinSize is reached
	decided bool
	status  int
}

// decide runs once, when enough is known: at the first write that reaches
// MinSize, at a Flush, or when the handler returns. small says the whole
// body is known to be below MinSize.
func (g *gzipWriter) decide(small bool) {
	if g.decided {
		return
	}
	g.decided = true
	h := g.Header()
	compress := !small && h.Get("Content-Encoding") == ""
	if compress {
		if n, err := strconv.Atoi(h.Get("Content-Length")); err == nil && n < g.opts.MinSize {
			compress = false
		}
	}
	if compress {
		ct := strings.ToLower(h.Get("Content-Type"))
		compress = false
		for _, t := range g.opts.ContentTypes {
			if strings.HasPrefix(ct, t) {
				compress = true
				break
			}
		}
	}
	if compress {
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		g.zw = g.pool.Get().(*gzip.Writer)
		g.zw.Reset(g.ResponseWriter)
	}
	if g.status == 0 {
		g.status = http.StatusOK
	}
	g.ResponseWriter.WriteHeader(g.status)
	if len(g.buf) > 0 {
		_, _ = g.write(g.buf)
		g.buf = nil
	}
}

func (g *gzipWriter) write(b []byte) (int, error) {
	if g.zw != nil {
		return g.zw.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.decided {
		return
	}
	g.status = code
	if code == http.StatusNoContent || code == http.StatusNotModified || code < 200 {
		g.decided = true
		g.ResponseWriter.WriteHeader(code)
	}
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.decided {
		if len(g.buf)+len(b) < g.opts.MinSize {
			g.buf = append(g.buf, b...)
			return len(b), nil
		}
		g.decide(false)
	}
	return g.write(b)
}

func (g *gzipWriter) Flush() {
	g.decide(false)
	if g.zw != nil {
		_ = g.zw.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) close() {
	if !g.decided {
		if g.status != 0 || len(g.buf) > 0 {
			g.decide(true) // whole body held back: too small to compress
		}
		return
	}
	if g.zw != nil {
		_ = g.zw.Close()
		g.zw.Reset(io.Discard)
		g.pool.Put(g.zw)
		g.zw = nil
	}
}
