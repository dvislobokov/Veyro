package scalar_test

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/dvislobokov/veyro"
	"github.com/dvislobokov/veyro/scalar"
)

func TestNoExternalRequests(t *testing.T) {
	app := veyro.New()
	app.Get("/ping", func(c *veyro.Context, _ veyro.None) (string, error) { return "pong", nil })
	app.ServeDocs("/docs", veyro.Info{Title: "T <api>", Version: "1"}, scalar.UI)

	get := func(path string, hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		return rec
	}

	page := get("/docs")
	if page.Code != 200 {
		t.Fatalf("page: %d", page.Code)
	}
	body := page.Body.String()
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Errorf("page references an external URL:\n%s", body)
	}
	for _, want := range []string{`src="/docs/scalar.js"`, `withDefaultFonts&#34;:false`, `proxyUrl&#34;:&#34;&#34;`, `telemetry&#34;:false`, `<title>T &lt;api&gt;</title>`, `api-reference-toolbar`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	for _, p := range []string{"/docs/", "/docs/openapi.json"} {
		if rec := get(p); rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}

	// Script: gzip when accepted, plain otherwise, 304 on ETag.
	gz := get("/docs/scalar.js", "Accept-Encoding", "gzip")
	if gz.Code != 200 || gz.Header().Get("Content-Encoding") != "gzip" || gz.Header().Get("ETag") == "" {
		t.Fatalf("gzip script: %d %v", gz.Code, gz.Header())
	}
	zr, err := gzip.NewReader(gz.Body)
	if err != nil {
		t.Fatal(err)
	}
	unzipped, _ := io.ReadAll(zr)
	plain := get("/docs/scalar.js")
	if plain.Header().Get("Content-Encoding") != "" || plain.Body.Len() != len(unzipped) || plain.Body.Len() < 1<<20 {
		t.Errorf("plain script: enc=%q len=%d unzipped=%d", plain.Header().Get("Content-Encoding"), plain.Body.Len(), len(unzipped))
	}
	if !strings.Contains(plain.Header().Get("Content-Type"), "javascript") {
		t.Errorf("content-type %q", plain.Header().Get("Content-Type"))
	}
	if rec := get("/docs/scalar.js", "If-None-Match", gz.Header().Get("ETag")); rec.Code != 304 {
		t.Errorf("etag revalidation: %d", rec.Code)
	}
	if rec := get("/docs/nope.js"); rec.Code != 404 {
		t.Errorf("unknown asset: %d", rec.Code)
	}

	// The embedded bundle must not load anything over the network on start:
	// no <script src=...> / <link href=...> injection with remote URLs.
	loader := regexp.MustCompile(`(?i)(fonts\.googleapis|fonts\.gstatic|cdn\.jsdelivr|unpkg\.com)`)
	if m := loader.FindString(string(unzipped)); m != "" {
		t.Errorf("bundle references %q", m)
	}
}

func TestOptions(t *testing.T) {
	h := scalar.Options{Theme: "moon", DarkMode: true, Layout: "classic", ShowScalarTools: true, Configuration: map[string]any{"hideModels": true}}.UI("x", "/d/openapi.json", "/d")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/d", nil))
	for _, want := range []string{`theme&#34;:&#34;moon`, `darkMode&#34;:true`, `layout&#34;:&#34;classic`, `hideModels&#34;:true`, `url&#34;:&#34;/d/openapi.json`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("missing %q in %s", want, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), "api-reference-toolbar") {
		t.Error("ShowScalarTools must not inject the hiding CSS")
	}
}
