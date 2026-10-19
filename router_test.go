package veyro_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dvislobokov/veyro"
)

func routerApp(t *testing.T) *veyro.App {
	t.Helper()
	app := newApp(t)
	echoRoute := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Route", name)
			_, _ = w.Write([]byte(name + " id=" + r.PathValue("id") + " path=" + r.PathValue("path") + " name=" + r.PathValue("name")))
		})
	}
	app.Raw("GET", "/{$}", echoRoute("root"))
	app.Raw("GET", "/users", echoRoute("users"))
	app.Raw("POST", "/users", echoRoute("create"))
	app.Raw("GET", "/users/{id}", echoRoute("user"))
	app.Raw("GET", "/users/me", echoRoute("me"))
	app.Raw("GET", "/users/{id}/posts/{name}", echoRoute("post"))
	app.Raw("GET", "/files/{path...}", echoRoute("files"))
	app.Raw("GET", "/static/", echoRoute("static"))
	app.Raw("GET", "/exact/{$}", echoRoute("exact-slash"))
	app.Raw("", "/any", echoRoute("any"))
	app.Raw("GET", "/a/{id}/b", echoRoute("a-id-b"))
	app.Raw("GET", "/a/x/c", echoRoute("a-x-c"))
	return app
}

func TestRouterMatching(t *testing.T) {
	app := routerApp(t)
	cases := []struct {
		method, path string
		status       int
		body         string // substring expected in body
		location     string
	}{
		{"GET", "/", 200, "root", ""},
		{"GET", "/users", 200, "users id=", ""},
		{"POST", "/users", 200, "create", ""},
		{"HEAD", "/users", 200, "", ""},
		{"GET", "/users/42", 200, "user id=42", ""},
		{"GET", "/users/me", 200, "me id=", ""}, // literal beats param
		{"GET", "/users/42/posts/hello", 200, "post id=42 path= name=hello", ""},
		{"GET", "/users/a%2Fb", 200, "user id=a/b", ""}, // escaped slash stays one segment
		{"GET", "/users/caf%C3%A9", 200, "user id=café", ""},
		{"GET", "/files/", 200, "files id= path=", ""}, // empty catch-all
		{"GET", "/files/a/b/c.txt", 200, "path=a/b/c.txt", ""},
		{"GET", "/files/a/b/", 200, "path=a/b/", ""},
		{"GET", "/files", 404, "Not Found", ""},
		{"GET", "/static/", 200, "static", ""},
		{"GET", "/static/css/app.css", 200, "static", ""},
		{"GET", "/static", 307, "", "/static/"}, // subtree redirect
		{"GET", "/exact/", 200, "exact-slash", ""},
		{"GET", "/exact", 404, "Not Found", ""}, // {$} does not redirect
		{"PATCH", "/any", 200, "any", ""},
		{"DELETE", "/users", 405, "Method Not Allowed", ""},
		{"GET", "/a/x/c", 200, "a-x-c", ""},
		{"GET", "/a/x/b", 200, "a-id-b", ""}, // backtracking: literal x has no b, param does
		{"GET", "/a/y/c", 404, "Not Found", ""},
		{"GET", "/users//42", 307, "", "/users/42"}, // path cleaning
		{"GET", "/users/42/../me?x=1", 307, "", "/users/me?x=1"},
		{"GET", "/nope", 404, "Not Found", ""},
		{"GET", "/users/", 404, "Not Found", ""}, // no subtree, no {$}
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.status {
			t.Errorf("%s %s: status %d, want %d (%s)", tc.method, tc.path, rec.Code, tc.status, rec.Body)
			continue
		}
		if tc.body != "" && !strings.Contains(rec.Body.String(), tc.body) {
			t.Errorf("%s %s: body %q, want %q", tc.method, tc.path, rec.Body, tc.body)
		}
		if tc.location != "" && rec.Header().Get("Location") != tc.location {
			t.Errorf("%s %s: Location %q, want %q", tc.method, tc.path, rec.Header().Get("Location"), tc.location)
		}
		if tc.status == 405 && rec.Header().Get("Allow") != "GET, HEAD, POST" {
			t.Errorf("%s %s: Allow %q", tc.method, tc.path, rec.Header().Get("Allow"))
		}
	}
}

func TestRouterRegistrationPanics(t *testing.T) {
	cases := map[string]func(app *veyro.App){
		"duplicate":        func(app *veyro.App) { app.Raw("GET", "/x", nil); app.Raw("GET", "/x", nil) },
		"wildcard name":    func(app *veyro.App) { app.Raw("GET", "/x/{a}", nil); app.Raw("GET", "/x/{b}", nil) },
		"catch-all middle": func(app *veyro.App) { app.Raw("GET", "/x/{a...}/y", nil) },
		"dollar middle":    func(app *veyro.App) { app.Raw("GET", "/x/{$}/y", nil) },
		"host pattern":     func(app *veyro.App) { app.Raw("GET", "example.com/x", nil) },
		"empty segment":    func(app *veyro.App) { app.Raw("GET", "/x//y", nil) },
	}
	for name, register := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil || !strings.HasPrefix(r.(string), "veyro: pattern") {
					t.Fatalf("want pattern panic, got %v", r)
				}
			}()
			register(newApp(t))
		})
	}
}
