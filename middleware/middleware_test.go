package middleware_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/dvislobokov/srog"
	"github.com/dvislobokov/veyro"
	"github.com/dvislobokov/veyro/middleware"
)

func newApp(logOut io.Writer) *veyro.App {
	if logOut == nil {
		logOut = io.Discard
	}
	return veyro.New(veyro.WithLogger(srog.MustNew(srog.WithWriter(logOut, srog.AsJSON()))))
}

func get(h http.Handler, method, target string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "203.0.113.9:1234"
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLogger(t *testing.T) {
	var out bytes.Buffer
	app := newApp(&out)
	app.Use(middleware.Logger(middleware.SkipPaths("/health"), middleware.WithFields(func(r *http.Request, status int) map[string]any {
		return map[string]any{"Extra": "x"}
	})))
	app.Get("/ok", func(c *veyro.Context, _ veyro.None) (string, error) { return "hi", nil })
	app.Get("/health", func(c *veyro.Context, _ veyro.None) (string, error) { return "up", nil })
	app.Get("/boom", func(c *veyro.Context, _ veyro.None) (string, error) { return "", veyro.Conflict("no") })

	get(app, "GET", "/health")
	if out.Len() != 0 {
		t.Fatalf("skipped path must not log: %s", out.String())
	}
	rec := get(app, "GET", "/ok?x=1", "User-Agent", "test-agent")
	var ev map[string]any
	if err := json.Unmarshal(out.Bytes(), &ev); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, out.String())
	}
	id := rec.Header().Get("X-Request-Id")
	if ev["level"] != "info" || ev["Method"] != "GET" || ev["Path"] != "/ok" || ev["Status"] != float64(200) ||
		ev["RequestId"] != id || ev["Query"] != "x=1" || ev["UserAgent"] != "test-agent" || ev["Extra"] != "x" || ev["Bytes"] != float64(4) {
		t.Errorf("event: %v", ev)
	}
	if msg, _ := ev["message"].(string); !strings.Contains(msg, "GET /ok responded 200") {
		t.Errorf("rendered message: %q", msg)
	}
	out.Reset()
	get(app, "GET", "/boom")
	_ = json.Unmarshal(out.Bytes(), &ev)
	if ev["level"] != "warn" || ev["Status"] != float64(409) {
		t.Errorf("4xx must be a warning: %v", ev)
	}
}

func TestCORS(t *testing.T) {
	app := newApp(nil)
	app.Use(middleware.CORS(middleware.CORSOptions{
		AllowOrigins:     []string{"https://app.example.com", "https://*.preview.example.com"},
		AllowHeaders:     []string{"*"},
		ExposeHeaders:    []string{"X-Request-Id"},
		AllowCredentials: true,
		MaxAge:           time.Hour,
	}))
	app.Post("/items", func(c *veyro.Context, _ veyro.None) (string, error) { return "ok", nil })

	pre := get(app, "OPTIONS", "/items", "Origin", "https://app.example.com",
		"Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "content-type, x-token")
	h := pre.Header()
	if pre.Code != 204 || h.Get("Access-Control-Allow-Origin") != "https://app.example.com" ||
		h.Get("Access-Control-Allow-Credentials") != "true" || h.Get("Access-Control-Allow-Headers") != "content-type, x-token" ||
		!strings.Contains(h.Get("Access-Control-Allow-Methods"), "POST") || h.Get("Access-Control-Max-Age") != "3600" {
		t.Errorf("preflight: %d %v", pre.Code, h)
	}
	actual := get(app, "POST", "/items", "Origin", "https://pr-12.preview.example.com")
	if actual.Code != 200 || actual.Header().Get("Access-Control-Allow-Origin") != "https://pr-12.preview.example.com" ||
		actual.Header().Get("Access-Control-Expose-Headers") != "X-Request-Id" {
		t.Errorf("wildcard origin: %d %v", actual.Code, actual.Header())
	}
	denied := get(app, "POST", "/items", "Origin", "https://evil.example.org")
	if denied.Header().Get("Access-Control-Allow-Origin") != "" || !strings.Contains(denied.Header().Get("Vary"), "Origin") {
		t.Errorf("denied origin must get no CORS headers: %v", denied.Header())
	}
	// Preflight for an unknown route is still answered when CORS is on the App.
	if rec := get(app, "OPTIONS", "/nope", "Origin", "https://app.example.com", "Access-Control-Request-Method", "GET"); rec.Code != 204 {
		t.Errorf("preflight on unknown route: %d", rec.Code)
	}
}

func TestTimeout(t *testing.T) {
	app := newApp(nil)
	app.Use(middleware.Timeout(50 * time.Millisecond))
	app.Get("/fast", func(c *veyro.Context, _ veyro.None) (string, error) {
		c.Header().Set("X-Fast", "1")
		c.Status(202)
		return "done", nil
	})
	app.Get("/slow", func(c *veyro.Context, _ veyro.None) (string, error) {
		select {
		case <-time.After(time.Second):
			return "late", nil
		case <-c.Done():
			return "", c.Err()
		}
	})
	app.Get("/panic", func(c *veyro.Context, _ veyro.None) (string, error) { panic("boom") })

	if rec := get(app, "GET", "/fast"); rec.Code != 202 || rec.Header().Get("X-Fast") != "1" || rec.Body.String() != `"done"` {
		t.Errorf("fast: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	rec := get(app, "GET", "/slow")
	var p map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != 504 || p["status"] != float64(504) || rec.Header().Get("Content-Type") != "application/problem+json; charset=utf-8" {
		t.Errorf("slow: %d %s", rec.Code, rec.Body)
	}
	if rec := get(app, "GET", "/panic"); rec.Code != 500 {
		t.Errorf("panic inside timeout must still be a 500 problem: %d %s", rec.Code, rec.Body)
	}
}

func TestGzip(t *testing.T) {
	app := newApp(nil)
	app.Use(middleware.Gzip(middleware.GzipOptions{MinSize: 100}))
	big := strings.Repeat("abcdefghij", 200)
	app.Get("/big", func(c *veyro.Context, _ veyro.None) (string, error) { return big, nil })
	app.Get("/small", func(c *veyro.Context, _ veyro.None) (string, error) { return "x", nil })
	app.Raw("GET", "/png", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte(big))
	}))
	app.Get("/none", func(c *veyro.Context, _ veyro.None) (veyro.None, error) { return veyro.None{}, nil })

	rec := get(app, "GET", "/big", "Accept-Encoding", "gzip")
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Content-Length") != "" || rec.Body.Len() >= len(big) {
		t.Fatalf("big: %v len=%d", rec.Header(), rec.Body.Len())
	}
	zr, _ := gzip.NewReader(rec.Body)
	if unz, _ := io.ReadAll(zr); string(unz) != `"`+big+`"` {
		t.Error("gzip payload mismatch")
	}
	if rec := get(app, "GET", "/big"); rec.Header().Get("Content-Encoding") != "" {
		t.Error("no Accept-Encoding must not compress")
	}
	if rec := get(app, "GET", "/small", "Accept-Encoding", "gzip"); rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != `"x"` {
		t.Errorf("small body must stay uncompressed: %v %s", rec.Header(), rec.Body)
	}
	if rec := get(app, "GET", "/png", "Accept-Encoding", "gzip"); rec.Header().Get("Content-Encoding") != "" {
		t.Error("image must not be compressed")
	}
	if rec := get(app, "GET", "/none", "Accept-Encoding", "gzip"); rec.Code != 204 || rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("204: %d %v", rec.Code, rec.Header())
	}
}

func TestRateLimit(t *testing.T) {
	app := newApp(nil)
	app.Use(middleware.RateLimit(middleware.RateLimitOptions{Rate: 1, Burst: 2}))
	app.Get("/", func(c *veyro.Context, _ veyro.None) (string, error) { return "ok", nil })
	codes := []int{}
	for range 3 {
		codes = append(codes, get(app, "GET", "/").Code)
	}
	rec := get(app, "GET", "/")
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 || rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Errorf("codes %v then %d retry-after=%q", codes, rec.Code, rec.Header().Get("Retry-After"))
	}
	other := httptest.NewRequest("GET", "/", nil)
	other.RemoteAddr = "198.51.100.7:9"
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, other)
	if rr.Code != 200 {
		t.Error("limit must be per client")
	}
}

func TestRealIP(t *testing.T) {
	app := newApp(nil)
	app.Use(middleware.RealIP(netip.MustParsePrefix("10.0.0.0/8")))
	app.Get("/", func(c *veyro.Context, _ veyro.None) (string, error) { return middleware.ClientIP(c.Request), nil })

	trusted := httptest.NewRequest("GET", "/", nil)
	trusted.RemoteAddr = "10.1.2.3:555"
	trusted.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.1")
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, trusted)
	if rec.Body.String() != `"198.51.100.7"` {
		t.Errorf("via trusted proxy: %s", rec.Body)
	}
	forged := httptest.NewRequest("GET", "/", nil)
	forged.RemoteAddr = "203.0.113.9:555"
	forged.Header.Set("X-Forwarded-For", "1.1.1.1")
	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, forged)
	if rec.Body.String() != `"203.0.113.9"` {
		t.Errorf("untrusted peer must be ignored: %s", rec.Body)
	}
}

func TestSecureHeaders(t *testing.T) {
	app := newApp(nil)
	app.Use(middleware.SecureHeaders(middleware.SecureHeadersOptions{FrameOptions: "-", HSTSMaxAge: time.Hour, ContentSecurityPolicy: "default-src 'self'"}))
	app.Get("/", func(c *veyro.Context, _ veyro.None) (string, error) { return "ok", nil })
	h := get(app, "GET", "/").Header()
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "" || h.Get("Content-Security-Policy") != "default-src 'self'" ||
		h.Get("Referrer-Policy") == "" || h.Get("Strict-Transport-Security") != "" {
		t.Errorf("headers: %v", h)
	}
	if h := get(app, "GET", "/", "X-Forwarded-Proto", "https").Header(); h.Get("Strict-Transport-Security") != "max-age=3600" {
		t.Errorf("hsts behind proxy: %v", h)
	}
}

func TestDocsRequireRole(t *testing.T) {
	app := newApp(nil)
	app.UseAuthentication(veyro.Bearer(func(ctx context.Context, token string) (*veyro.User, error) {
		if token == "admin" {
			return veyro.NewUser("", "root").AddClaim(veyro.ClaimRole, "admin"), nil
		}
		return veyro.NewUser("", "ann"), nil
	}))
	app.ServeDocs(nil).RequireRole("admin")
	if rec := get(app, "GET", "/docs/openapi.json"); rec.Code != 401 {
		t.Errorf("anonymous: %d", rec.Code)
	}
	if rec := get(app, "GET", "/docs/openapi.json", "Authorization", "Bearer user"); rec.Code != 403 {
		t.Errorf("non-admin: %d", rec.Code)
	}
	if rec := get(app, "GET", "/docs/openapi.json", "Authorization", "Bearer admin"); rec.Code != 200 {
		t.Errorf("admin: %d", rec.Code)
	}
}
