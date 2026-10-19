package veyro

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func benchTree() (*tree, *http.ServeMux, []*http.Request) {
	tr := newTree()
	mux := http.NewServeMux()
	add := func(p string) {
		tr.add("GET", p, &Route{})
		mux.Handle("GET "+p, http.NotFoundHandler())
	}
	for i := range 30 {
		base := "/repos" + strconv.Itoa(i)
		for _, s := range []string{"", "/{owner}/{repo}", "/{owner}/{repo}/issues", "/{owner}/{repo}/issues/{number}",
			"/{owner}/{repo}/issues/{number}/comments", "/{owner}/{repo}/pulls/{number}/files", "/{owner}/{repo}/git/blobs/{sha}", "/{owner}/{repo}/contents/{path...}"} {
			add(base + s)
		}
	}
	add("/")
	add("/health")
	add("/static/")
	return tr, mux, []*http.Request{
		httptest.NewRequest("GET", "/health", nil),
		httptest.NewRequest("GET", "/repos15/golang/go/issues/123/comments", nil),
		httptest.NewRequest("GET", "/repos29/golang/go/contents/src/net/http/server.go", nil),
		httptest.NewRequest("GET", "/static/css/app.css", nil),
	}
}

func BenchmarkTreeMatch(b *testing.B) {
	tr, _, reqs := benchTree()
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		r := reqs[i%len(reqs)]
		res := tr.match(r.Method, r.URL.EscapedPath())
		if res.route == nil {
			b.Fatal("no match")
		}
	}
}

func BenchmarkServeMuxHandler(b *testing.B) {
	_, mux, reqs := benchTree()
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		r := reqs[i%len(reqs)]
		if _, pattern := mux.Handler(r); pattern == "" {
			b.Fatal("no match")
		}
	}
}
