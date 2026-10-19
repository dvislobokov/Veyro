package veyro

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// Static is an in-memory asset served with conditional requests and
// content negotiation: ETag/If-None-Match, a precompressed gzip variant for
// clients that accept it, and a Cache-Control policy. Build one with
// NewStatic; it is safe for concurrent use.
type Static struct {
	ContentType  string
	CacheControl string
	ETag         string
	plain, gz    []byte
}

// NewStatic prepares body for serving. The gzip variant is produced once,
// here, and only kept when it is actually smaller.
func NewStatic(contentType, cacheControl string, body []byte) *Static {
	sum := sha256.Sum256(body)
	s := &Static{
		ContentType:  contentType,
		CacheControl: cacheControl,
		ETag:         `"` + hex.EncodeToString(sum[:16]) + `"`,
		plain:        body,
	}
	if len(body) >= 1024 {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if _, err := zw.Write(body); err == nil && zw.Close() == nil && buf.Len() < len(body) {
			s.gz = buf.Bytes()
		}
	}
	return s
}

// NewStaticGzip is NewStatic for a body whose gzip form already exists
// (an embedded asset, for example), so nothing is recompressed.
func NewStaticGzip(contentType, cacheControl string, body, gz []byte) *Static {
	sum := sha256.Sum256(body)
	return &Static{
		ContentType:  contentType,
		CacheControl: cacheControl,
		ETag:         `"` + hex.EncodeToString(sum[:16]) + `"`,
		plain:        body,
		gz:           gz,
	}
}

// ServeHTTP implements http.Handler.
func (s *Static) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", s.ContentType)
	if s.CacheControl != "" {
		h.Set("Cache-Control", s.CacheControl)
	}
	h.Set("ETag", s.ETag)
	if s.gz != nil {
		h.Set("Vary", "Accept-Encoding")
	}
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, s.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := s.plain
	if s.gz != nil && acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		body = s.gz
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

func etagMatches(header, etag string) bool {
	for tag := range strings.SplitSeq(header, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || tag == etag || strings.TrimPrefix(tag, "W/") == etag {
			return true
		}
	}
	return false
}

func acceptsGzip(r *http.Request) bool {
	for enc := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		enc = strings.TrimSpace(enc)
		if name, params, _ := strings.Cut(enc, ";"); strings.TrimSpace(name) == "gzip" {
			return !strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0,") &&
				!strings.HasSuffix(strings.ReplaceAll(params, " ", ""), "q=0")
		}
	}
	return false
}
