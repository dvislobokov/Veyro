package veyro_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/dvislobokov/veyro"
)

// The same GitHub-like API registered on both routers.
var benchPatterns = func() []string {
	var p []string
	for i := range 30 {
		base := "/repos" + strconv.Itoa(i)
		p = append(p,
			base,
			base+"/{owner}/{repo}",
			base+"/{owner}/{repo}/issues",
			base+"/{owner}/{repo}/issues/{number}",
			base+"/{owner}/{repo}/issues/{number}/comments",
			base+"/{owner}/{repo}/pulls/{number}/files",
			base+"/{owner}/{repo}/git/blobs/{sha}",
			base+"/{owner}/{repo}/contents/{path...}",
		)
	}
	return append(p, "/", "/health", "/static/")
}()

var noop = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_ = r.PathValue("repo")
})

func benchRequests() []*http.Request {
	return []*http.Request{
		httptest.NewRequest("GET", "/health", nil),
		httptest.NewRequest("GET", "/repos15/golang/go/issues/123/comments", nil),
		httptest.NewRequest("GET", "/repos29/golang/go/contents/src/net/http/server.go", nil),
		httptest.NewRequest("GET", "/static/css/app.css", nil),
	}
}

func BenchmarkVeyroRouter(b *testing.B) {
	app := veyro.New()
	for _, p := range benchPatterns {
		app.Raw("GET", p, noop)
	}
	runBench(b, app)
}

func BenchmarkServeMux(b *testing.B) {
	mux := http.NewServeMux()
	for _, p := range benchPatterns {
		mux.Handle("GET "+p, noop)
	}
	runBench(b, mux)
}

func runBench(b *testing.B, h http.Handler) {
	reqs := benchRequests()
	w := &discardWriter{h: http.Header{}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		h.ServeHTTP(w, reqs[i%len(reqs)])
	}
}

type discardWriter struct{ h http.Header }

func (d *discardWriter) Header() http.Header         { return d.h }
func (d *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardWriter) WriteHeader(int)             {}

// Same as BenchmarkVeyroRouter but with request ids taken from the client,
// isolating routing from UUID generation.
func BenchmarkVeyroRouterNoUUID(b *testing.B) {
	app := veyro.New(veyro.WithRequestID(func(r *http.Request) string { return "fixed" }))
	for _, p := range benchPatterns {
		app.Raw("GET", p, noop)
	}
	runBench(b, app)
}
