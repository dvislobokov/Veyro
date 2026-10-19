package veyro_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dvislobokov/veyro"
)

func TestStaticAndOpenAPICaching(t *testing.T) {
	body := []byte(strings.Repeat(`{"k":"v"}`, 500))
	st := veyro.NewStatic("application/json", "no-cache", body)

	get := func(h http.Handler, hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	plain := get(st)
	if plain.Code != 200 || !bytes.Equal(plain.Body.Bytes(), body) || plain.Header().Get("ETag") == "" ||
		plain.Header().Get("Cache-Control") != "no-cache" || plain.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("plain: %d %v", plain.Code, plain.Header())
	}
	gz := get(st, "Accept-Encoding", "br, gzip;q=0.8")
	if gz.Header().Get("Content-Encoding") != "gzip" || gz.Body.Len() >= len(body) {
		t.Fatalf("gzip: %v len=%d", gz.Header(), gz.Body.Len())
	}
	zr, _ := gzip.NewReader(gz.Body)
	if unz, _ := io.ReadAll(zr); !bytes.Equal(unz, body) {
		t.Error("gzip body differs")
	}
	if rec := get(st, "Accept-Encoding", "gzip;q=0"); rec.Header().Get("Content-Encoding") != "" {
		t.Error("q=0 must disable gzip")
	}
	etag := plain.Header().Get("ETag")
	for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		if rec := get(st, "If-None-Match", inm); rec.Code != 304 || rec.Body.Len() != 0 {
			t.Errorf("If-None-Match %q: %d", inm, rec.Code)
		}
	}
	if rec := get(st, "If-None-Match", `"nope"`); rec.Code != 200 {
		t.Error("mismatched etag must serve the body")
	}

	// openapi.json goes through the same path and is generated only once.
	app := newApp(t)
	app.Get("/a", func(c *veyro.Context, _ veyro.None) (string, error) { return "", nil })
	app.ServeDocs(nil)
	first := httptest.NewRecorder()
	app.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/docs/openapi.json", nil))
	app.Get("/b", func(c *veyro.Context, _ veyro.None) (string, error) { return "", nil }) // too late
	second := httptest.NewRecorder()
	app.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/docs/openapi.json", nil))
	if first.Header().Get("ETag") == "" || first.Header().Get("ETag") != second.Header().Get("ETag") ||
		!bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) || strings.Contains(second.Body.String(), `"/b"`) {
		t.Error("openapi.json must be generated once and served with a stable ETag")
	}
	req := httptest.NewRequest(http.MethodGet, "/docs/openapi.json", nil)
	req.Header.Set("If-None-Match", first.Header().Get("ETag"))
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != 304 {
		t.Errorf("openapi.json revalidation: %d", rec.Code)
	}
}
