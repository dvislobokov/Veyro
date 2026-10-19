package veyro_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/dvislobokov/veyro"
)

type Address struct {
	City string `json:"city" validate:"required"`
}

type UpdateUser struct {
	ID      uuid.UUID     `path:"id"`
	Expand  []string      `query:"expand"`
	Page    int           `query:"page" validate:"min=1"`
	Trace   string        `header:"X-Trace"`
	Session string        `cookie:"sid"`
	Timeout time.Duration `query:"timeout" json:"-"`

	Name    string   `json:"name" validate:"required,minlen=2"`
	Age     *int     `json:"age" validate:"min=18"`
	Email   string   `json:"email" validate:"email"`
	Role    string   `json:"role" validate:"oneof=admin user"`
	Address *Address `json:"address"`
	Tags    []string `json:"tags" validate:"maxlen=2"`
}

type echo struct {
	UpdateUser
	Method  string  `json:"method"`
	Seconds float64 `json:"seconds"`
}

func newApp(t *testing.T) *veyro.App {
	t.Helper()
	return veyro.New(veyro.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), veyro.ExposeErrors(true))
}

func do(t *testing.T, h http.Handler, method, target, body string, hdr ...string) (*http.Response, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	if len(b) > 0 {
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("response is not JSON: %v\n%s", err, b)
		}
	}
	return resp, out
}

func TestBindAllSourcesIntoOneStruct(t *testing.T) {
	app := newApp(t)
	app.Put("/users/{id}", func(ctx context.Context, in UpdateUser) (echo, error) {
		return echo{UpdateUser: in, Method: veyro.RequestFrom(ctx).Method, Seconds: in.Timeout.Seconds()}, nil
	})

	id := uuid.NewV7()
	resp, out := do(t, app, http.MethodPut,
		"/users/"+id.String()+"?expand=a&expand=b&page=3&timeout=1m30s",
		`{"name":"Ann","age":30,"email":"a@b.c","role":"admin","tags":["x"],"id":"00000000-0000-0000-0000-000000000000","page":99}`,
		"X-Trace", "t-1", "Cookie", "sid=s-1")

	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content-type %q", ct)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("missing X-Request-Id")
	}
	want := map[string]any{
		"ID": id.String(), "Page": float64(3), "Trace": "t-1", "Session": "s-1",
		"seconds": float64(90), "name": "Ann", "age": float64(30),
		"email": "a@b.c", "role": "admin", "method": "PUT",
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("%s = %v, want %v", k, out[k], v)
		}
	}
	if exp, _ := out["Expand"].([]any); len(exp) != 2 || exp[0] != "a" || exp[1] != "b" {
		t.Errorf("Expand = %v", out["Expand"])
	}
}

func TestValidationProblemShape(t *testing.T) {
	app := newApp(t)
	app.Put("/users/{id}", func(ctx context.Context, in UpdateUser) (veyro.None, error) {
		return veyro.None{}, nil
	})

	resp, out := do(t, app, http.MethodPut, "/users/not-a-uuid?page=-1",
		`{"name":"A","age":5,"email":"nope","role":"root","tags":["1","2","3"],"address":{"city":""}}`)

	if resp.StatusCode != 400 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json; charset=utf-8" {
		t.Errorf("content-type %q", ct)
	}
	if out["title"] != "One or more validation errors occurred." ||
		out["status"] != float64(400) ||
		out["type"] != "https://tools.ietf.org/html/rfc9110#section-15.5.1" ||
		out["traceId"] == "" {
		t.Errorf("problem envelope: %v", out)
	}
	errs, _ := out["errors"].(map[string]any)
	want := map[string]string{
		"id":           "The value 'not-a-uuid' is not valid.",
		"page":         "The field page must be greater than or equal to 1.",
		"name":         "The field name must be a string or array type with a minimum length of '2'.",
		"age":          "The field age must be greater than or equal to 18.",
		"email":        "The email field is not a valid e-mail address.",
		"role":         "The field role must be one of: admin, user.",
		"tags":         "The field tags must be a string or array type with a maximum length of '2'.",
		"address.city": "The address.city field is required.",
	}
	for k, msg := range want {
		got, _ := errs[k].([]any)
		if len(got) != 1 || got[0] != msg {
			t.Errorf("errors[%q] = %v, want [%q]", k, got, msg)
		}
	}
	if len(errs) != len(want) {
		t.Errorf("unexpected extra errors: %v", errs)
	}
}

type bodyOnly struct {
	Name string `json:"name" validate:"required"`
	Age  int    `json:"age"`
}

func TestMalformedJSON(t *testing.T) {
	app := newApp(t)
	app.Post("/u", func(ctx context.Context, in bodyOnly) (veyro.None, error) { return veyro.None{}, nil })

	resp, out := do(t, app, http.MethodPost, "/u", `{"name": "A",`)
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	errs := out["errors"].(map[string]any)
	if _, ok := errs["$"]; !ok {
		t.Errorf("want error under $, got %v", errs)
	}

	resp, out = do(t, app, http.MethodPost, "/u", `{"name":"Ann","role":"user","age":"thirty"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	errs = out["errors"].(map[string]any)
	if got, _ := errs["age"].([]any); len(got) != 1 || !strings.HasPrefix(got[0].(string), "The JSON value could not be converted to") {
		t.Errorf("errors = %v", errs)
	}
}

func TestUnsupportedMediaType(t *testing.T) {
	app := newApp(t)
	app.Post("/u", func(ctx context.Context, in bodyOnly) (veyro.None, error) { return veyro.None{}, nil })
	req := httptest.NewRequest(http.MethodPost, "/u", strings.NewReader("name=a"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != 415 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestNotFoundAndMethodNotAllowedAreProblems(t *testing.T) {
	app := newApp(t)
	app.Get("/only-get", func(ctx context.Context, _ veyro.None) (veyro.None, error) { return veyro.None{}, nil })

	resp, out := do(t, app, http.MethodGet, "/nope", "")
	if resp.StatusCode != 404 || out["title"] != "Not Found" || out["status"] != float64(404) {
		t.Errorf("404: %d %v", resp.StatusCode, out)
	}
	resp, out = do(t, app, http.MethodPost, "/only-get", "")
	if resp.StatusCode != 405 || out["status"] != float64(405) {
		t.Errorf("405: %d %v", resp.StatusCode, out)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q", allow)
	}
}

func TestNoneOutputAndCreated(t *testing.T) {
	app := newApp(t)
	app.Delete("/x/{id}", func(ctx context.Context, in struct {
		ID int `path:"id"`
	}) (veyro.None, error) {
		return veyro.None{}, nil
	})
	app.Post("/x", func(ctx context.Context, _ veyro.None) (map[string]int, error) {
		veyro.Created(ctx, "/x/7")
		return map[string]int{"id": 7}, nil
	})

	resp, _ := do(t, app, http.MethodDelete, "/x/5", "")
	if resp.StatusCode != 204 {
		t.Errorf("delete status %d", resp.StatusCode)
	}
	resp, out := do(t, app, http.MethodPost, "/x", "")
	if resp.StatusCode != 201 || resp.Header.Get("Location") != "/x/7" || out["id"] != float64(7) {
		t.Errorf("create: %d %v %v", resp.StatusCode, resp.Header, out)
	}
}

type signup struct {
	Password string `json:"password" validate:"required"`
	Confirm  string `json:"confirm"`
}

func (s *signup) Validate(ctx context.Context) error {
	if s.Password != s.Confirm {
		return veyro.Invalid("confirm", "Passwords do not match.")
	}
	return nil
}

func TestValidatorInterfaceMergesErrors(t *testing.T) {
	app := newApp(t)
	app.Post("/s", func(ctx context.Context, in signup) (veyro.None, error) { return veyro.None{}, nil })
	resp, out := do(t, app, http.MethodPost, "/s", `{"password":"a","confirm":"b"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	errs := out["errors"].(map[string]any)
	if got, _ := errs["confirm"].([]any); len(got) != 1 || got[0] != "Passwords do not match." {
		t.Errorf("errors = %v", errs)
	}
}

func TestErrorsFromHandler(t *testing.T) {
	app := veyro.New(veyro.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	app.Get("/p", func(ctx context.Context, _ veyro.None) (veyro.None, error) {
		return veyro.None{}, veyro.NotFound("no such thing").With("resource", "thing")
	})
	app.Get("/boom", func(ctx context.Context, _ veyro.None) (veyro.None, error) {
		return veyro.None{}, errors.New("db down")
	})
	app.Get("/panic", func(ctx context.Context, _ veyro.None) (veyro.None, error) { panic("x") })

	resp, out := do(t, app, http.MethodGet, "/p", "")
	if resp.StatusCode != 404 || out["detail"] != "no such thing" || out["resource"] != "thing" {
		t.Errorf("problem: %d %v", resp.StatusCode, out)
	}
	resp, out = do(t, app, http.MethodGet, "/boom", "")
	if resp.StatusCode != 500 || out["detail"] != nil {
		t.Errorf("500 must not leak detail: %d %v", resp.StatusCode, out)
	}
	resp, out = do(t, app, http.MethodGet, "/panic", "")
	if resp.StatusCode != 500 || out["status"] != float64(500) {
		t.Errorf("panic: %d %v", resp.StatusCode, out)
	}
}

func TestGroupsAndMiddleware(t *testing.T) {
	app := newApp(t)
	var order []string
	mw := func(name string) veyro.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	app.Use(mw("app"))
	api := app.Group("/api", mw("api"))
	v1 := api.Group("/v1")
	v1.Use(mw("v1"))
	v1.Get("/ping", func(ctx context.Context, _ veyro.None) (string, error) { return "pong", nil })

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `"pong"` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if strings.Join(order, ",") != "app,api,v1" {
		t.Errorf("order = %v", order)
	}
	order = nil
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", nil))
	if strings.Join(order, ",") != "app" {
		t.Errorf("app middleware must wrap 404s, got %v", order)
	}
}

func TestMisconfiguredRoutePanicsAtRegistration(t *testing.T) {
	app := newApp(t)
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(r.(string), `path:"id"`) {
			t.Fatalf("want registration panic, got %v", r)
		}
	}()
	app.Get("/users", func(ctx context.Context, in struct {
		ID int `path:"id"`
	}) (veyro.None, error) {
		return veyro.None{}, nil
	})
}

type redirect string

func (r redirect) Render(w http.ResponseWriter, req *http.Request) error {
	http.Redirect(w, req, string(r), http.StatusFound)
	return nil
}

func TestRenderer(t *testing.T) {
	app := newApp(t)
	app.Get("/old", func(ctx context.Context, _ veyro.None) (redirect, error) { return "/new", nil })
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/old", nil))
	if rec.Code != 302 || rec.Header().Get("Location") != "/new" {
		t.Errorf("%d %v", rec.Code, rec.Header())
	}
}
