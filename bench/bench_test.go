// Package bench compares Veyro with popular routers and frameworks.
//
//	cd bench && go test -bench . -benchmem
//
// Two scenarios:
//
//   - Route*: a GitHub-like table of ~240 routes; the request hits a
//     static route, a 4-wildcard route, a catch-all and a subtree in turn.
//     Handlers do nothing. This measures routing plus per-request overhead.
//   - JSON*: POST /users/{id} with a JSON body bound into a struct, two
//     validation rules, and a JSON response. This is the typical endpoint
//     and what Veyro is built for; each framework uses its idiomatic API.
package bench

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/dvislobokov/veyro"
	"github.com/gin-gonic/gin"
	"github.com/go-chi/chi/v5"
	"github.com/labstack/echo/v4"
)

// --- route table ---

type pattern struct{ std, colon, gin string } // ServeMux, Echo and Gin syntaxes

var patterns = func() []pattern {
	var p []pattern
	for i := range 30 {
		base := "/repos" + strconv.Itoa(i)
		for _, s := range [][2]string{
			{"", ""},
			{"/{owner}/{repo}", "/:owner/:repo"},
			{"/{owner}/{repo}/issues", "/:owner/:repo/issues"},
			{"/{owner}/{repo}/issues/{number}", "/:owner/:repo/issues/:number"},
			{"/{owner}/{repo}/issues/{number}/comments", "/:owner/:repo/issues/:number/comments"},
			{"/{owner}/{repo}/pulls/{number}/files", "/:owner/:repo/pulls/:number/files"},
			{"/{owner}/{repo}/git/blobs/{sha}", "/:owner/:repo/git/blobs/:sha"},
			{"/{owner}/{repo}/contents/{path...}", "/:owner/:repo/contents/*"},
		} {
			p = append(p, pattern{base + s[0], base + s[1], strings.Replace(base+s[1], "/*", "/*path", 1)})
		}
	}
	return append(p, pattern{"/health", "/health", "/health"}, pattern{"/static/", "/static/*", "/static/*path"})
}()

var routeRequests = []*http.Request{
	httptest.NewRequest("GET", "/health", nil),
	httptest.NewRequest("GET", "/repos15/golang/go/issues/123/comments", nil),
	httptest.NewRequest("GET", "/repos29/golang/go/contents/src/net/http/server.go", nil),
	httptest.NewRequest("GET", "/static/css/app.css", nil),
}

type discard struct{ h http.Header }

func (d *discard) Header() http.Header         { return d.h }
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(int)             {}

func run(b *testing.B, h http.Handler, reqs []*http.Request, body []byte) {
	w := &discard{h: http.Header{}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		r := reqs[i%len(reqs)]
		if body != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		h.ServeHTTP(w, r)
	}
}

func BenchmarkRoute_Veyro(b *testing.B) {
	app := veyro.New(veyro.WithRequestID(func(r *http.Request) string { return "" }))
	for _, p := range patterns {
		app.Raw("GET", p.std, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	}
	run(b, app, routeRequests, nil)
}

func BenchmarkRoute_ServeMux(b *testing.B) {
	mux := http.NewServeMux()
	for _, p := range patterns {
		mux.HandleFunc("GET "+p.std, func(w http.ResponseWriter, r *http.Request) {})
	}
	run(b, mux, routeRequests, nil)
}

func BenchmarkRoute_Echo(b *testing.B) {
	e := echo.New()
	for _, p := range patterns {
		e.GET(p.colon, func(c echo.Context) error { return nil })
	}
	run(b, e, routeRequests, nil)
}

func BenchmarkRoute_Chi(b *testing.B) {
	r := chi.NewRouter()
	for _, p := range patterns {
		r.Get(p.std, func(w http.ResponseWriter, r *http.Request) {})
	}
	run(b, r, routeRequests, nil)
}

func BenchmarkRoute_Gin(b *testing.B) {
	gin.SetMode(gin.ReleaseMode)
	g := gin.New()
	for _, p := range patterns {
		g.GET(p.gin, func(c *gin.Context) {})
	}
	run(b, g, routeRequests, nil)
}

// --- typed JSON endpoint ---

type CreateUser struct {
	Name  string `json:"name" validate:"required,minlen=2" binding:"required,min=2"`
	Email string `json:"email" validate:"required,email" binding:"required,email"`
	Age   int    `json:"age" validate:"min=18" binding:"min=18"`
}

type UpdateUser struct { // Veyro: path parameter and body in one struct
	ID int `path:"id"`
	CreateUser
}

type User struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Age   int    `json:"age"`
}

var (
	jsonBody    = []byte(`{"name":"Ann","email":"ann@example.com","age":30}`)
	jsonRequest = func() *http.Request {
		r := httptest.NewRequest("POST", "/users/42", nil)
		r.Header.Set("Content-Type", "application/json")
		r.ContentLength = int64(len(jsonBody))
		return r
	}()
)

func validate(in *CreateUser) bool { // hand-written equivalent for ServeMux/Chi
	if len(in.Name) < 2 || in.Email == "" || in.Age < 18 {
		return false
	}
	at := bytes.IndexByte([]byte(in.Email), '@')
	return at > 0 && at < len(in.Email)-1
}

func BenchmarkJSON_Veyro(b *testing.B) {
	app := veyro.New(veyro.WithRequestID(func(r *http.Request) string { return "" }))
	app.Post("/users/{id}", func(c *veyro.Context, in UpdateUser) (User, error) {
		return User{ID: in.ID, Name: in.Name, Email: in.Email, Age: in.Age}, nil
	})
	run(b, app, []*http.Request{jsonRequest}, jsonBody)
}

func BenchmarkJSON_ServeMux(b *testing.B) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.Error(w, "bad id", 400)
			return
		}
		var in CreateUser
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !validate(&in) {
			http.Error(w, "bad body", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(User{ID: id, Name: in.Name, Email: in.Email, Age: in.Age})
	})
	run(b, mux, []*http.Request{jsonRequest}, jsonBody)
}

func BenchmarkJSON_Echo(b *testing.B) {
	e := echo.New()
	e.Validator = echoValidator{}
	e.POST("/users/:id", func(c echo.Context) error {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			return echo.NewHTTPError(400, "bad id")
		}
		var in CreateUser
		if err := c.Bind(&in); err != nil {
			return err
		}
		if err := c.Validate(&in); err != nil {
			return err
		}
		return c.JSON(200, User{ID: id, Name: in.Name, Email: in.Email, Age: in.Age})
	})
	run(b, e, []*http.Request{jsonRequest}, jsonBody)
}

type echoValidator struct{}

func (echoValidator) Validate(i any) error {
	if in, ok := i.(*CreateUser); ok && !validate(in) {
		return echo.NewHTTPError(400, "invalid")
	}
	return nil
}

func BenchmarkJSON_Chi(b *testing.B) {
	r := chi.NewRouter()
	r.Post("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(chi.URLParam(r, "id"))
		if err != nil {
			http.Error(w, "bad id", 400)
			return
		}
		var in CreateUser
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !validate(&in) {
			http.Error(w, "bad body", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(User{ID: id, Name: in.Name, Email: in.Email, Age: in.Age})
	})
	run(b, r, []*http.Request{jsonRequest}, jsonBody)
}

func BenchmarkJSON_Gin(b *testing.B) {
	gin.SetMode(gin.ReleaseMode)
	g := gin.New()
	g.POST("/users/:id", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.String(400, "bad id")
			return
		}
		var in CreateUser
		if err := c.ShouldBindJSON(&in); err != nil { // runs go-playground/validator via binding tags
			c.String(400, err.Error())
			return
		}
		c.JSON(200, User{ID: id, Name: in.Name, Email: in.Email, Age: in.Age})
	})
	run(b, g, []*http.Request{jsonRequest}, jsonBody)
}

// Sanity check that every JSON handler actually succeeds.
func TestJSONHandlersRespond(t *testing.T) {
	for name, bench := range map[string]func(*testing.B){} {
		_ = name
		_ = bench
	}
	handlers := map[string]http.Handler{}
	{
		app := veyro.New()
		app.Post("/users/{id}", func(c *veyro.Context, in UpdateUser) (User, error) {
			return User{ID: in.ID, Name: in.Name, Email: in.Email, Age: in.Age}, nil
		})
		handlers["veyro"] = app
	}
	for name, h := range handlers {
		r := httptest.NewRequest("POST", "/users/42", bytes.NewReader(jsonBody))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte(`"id":42`)) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}
