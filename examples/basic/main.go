// Command basic is a minimal Veyro service.
//
//	go run ./examples/basic
//	curl -X POST localhost:8080/users -H 'Authorization: Bearer ann' -H 'Content-Type: application/json' -d '{"name":"Ann","email":"ann@example.com","age":30}'
//	curl localhost:8080/users/<id>?expand=profile
//	curl -X POST localhost:8080/users -H 'Authorization: Bearer ann' -H 'Content-Type: application/json' -d '{"name":"A"}'   # validation problem
//	curl localhost:8080/me                                  # 401 with WWW-Authenticate: Bearer
//	open http://localhost:8080/docs                         # API reference (admin token required)
//	curl localhost:8080/probe/healthy                       # health checks
//	curl localhost:8080/metrics                             # Prometheus metrics
//	curl -X DELETE localhost:8080/users/<id> -H 'Authorization: Bearer ann'   # 403
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/dvislobokov/srog"
	"github.com/dvislobokov/veyro"
	"github.com/dvislobokov/veyro/middleware"
	"github.com/dvislobokov/veyro/scalar"
	"github.com/dvislobokov/veyro/telemetry"
)

// Role is a string enum: validated on input, documented in OpenAPI.
type Role string

func (Role) EnumValues() []string { return []string{"admin", "user"} }

type User struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name" example:"Ann"`
	Email string    `json:"email" format:"email"`
	Age   int       `json:"age" example:"30"`
	Role  Role      `json:"role"`
}

type CreateUser struct {
	Name  string `json:"name" validate:"required,minlen=2,maxlen=64" example:"Ann" doc:"Display name"`
	Email string `json:"email" validate:"required,email" example:"ann@example.com"`
	Age   int    `json:"age" validate:"min=18,max=130" example:"30"`
	Role  Role   `json:"role" default:"user"`
}

type GetUser struct {
	ID     uuid.UUID `path:"id" doc:"User id (UUIDv7)"`
	Expand []string  `query:"expand" enum:"profile,settings" doc:"Related objects to include"`
}

type ListUsers struct {
	Page    int    `query:"page" default:"1" validate:"min=1" doc:"1-based page number"`
	PerPage int    `query:"per_page" default:"20" validate:"min=1,max=100"`
	Sort    string `query:"sort" enum:"name,age" default:"name"`
}

type store struct {
	mu    sync.RWMutex
	users map[uuid.UUID]User
}

func (s *store) create(c *veyro.Context, in CreateUser) (User, error) {
	u := User{ID: uuid.NewV7(), Name: in.Name, Email: in.Email, Age: in.Age, Role: in.Role}
	s.mu.Lock()
	s.users[u.ID] = u
	s.mu.Unlock()
	c.Created("/users/" + u.ID.String())
	return u, nil
}

func (s *store) get(c *veyro.Context, in GetUser) (User, error) {
	s.mu.RLock()
	u, ok := s.users[in.ID]
	s.mu.RUnlock()
	if !ok {
		return User{}, veyro.NotFound("user " + in.ID.String() + " does not exist")
	}
	return u, nil
}

func (s *store) list(c *veyro.Context, in ListUsers) ([]User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	return out, nil
}

func (s *store) remove(c *veyro.Context, in GetUser) (veyro.None, error) {
	s.mu.Lock()
	delete(s.users, in.ID)
	s.mu.Unlock()
	return veyro.None{}, nil
}

// lookupToken is a stand-in for JWT verification or an API-key lookup.
func lookupToken(ctx context.Context, token string) (*veyro.User, error) {
	switch token {
	case "admin":
		return veyro.NewUser("", "root").AddClaim(veyro.ClaimSubject, "1").AddClaim(veyro.ClaimRole, "admin"), nil
	case "ann":
		return veyro.NewUser("", "ann").AddClaim(veyro.ClaimSubject, "2").AddClaim(veyro.ClaimRole, "user"), nil
	}
	return nil, errors.New("unknown token")
}

func main() {
	db := &store{users: map[uuid.UUID]User{}}

	log := srog.NewDevelopment()
	defer log.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Traces and metrics stay in-process here; set Endpoint to ship them
	// to an OpenTelemetry Collector.
	shutdown, err := telemetry.Setup(ctx, telemetry.Config{ServiceName: "users-example", Prometheus: true})
	if err != nil {
		log.Fatal(err, "Telemetry setup failed")
	}
	defer shutdown(context.Background())

	app := veyro.New(veyro.WithLogger(log))
	probe := func(r *http.Request) bool {
		return strings.HasPrefix(r.URL.Path, "/probe/") || r.URL.Path == "/metrics"
	}
	app.Use(
		middleware.SecureHeaders(middleware.SecureHeadersOptions{}),
		telemetry.Tracing(telemetry.SkipTracing(probe)),
		telemetry.Metrics(telemetry.SkipMetrics(probe)),
		middleware.Logger(middleware.Skip(probe)),
		middleware.CORS(middleware.CORSAllowAll()),
		middleware.Gzip(middleware.GzipOptions{}),
		middleware.RateLimit(middleware.RateLimitOptions{Rate: 50, Burst: 100}),
		middleware.Timeout(10*time.Second),
	)
	app.UseAuthentication(veyro.Bearer(lookupToken))
	app.Probes()
	app.AddCheck("store", func(ctx context.Context) error { return nil })
	app.Raw("GET", "/metrics", telemetry.PrometheusHandler()).Hide()

	users := app.Group("/users").Tags("users").RequireAuthorization()
	users.Post("", db.create).Summary("Create a user").Success(201)
	users.Get("", db.list).Summary("List users").AllowAnonymous()
	users.Get("/{id}", db.get).Summary("Get a user").Errors(404).AllowAnonymous()
	users.Delete("/{id}", db.remove).Summary("Delete a user").RequireRole("admin")

	app.OpenAPIConfig().
		Title("Users API").Version("1.0.0").
		Description("Example service built with Veyro.").
		Tag("users", "User accounts").Tag("auth", "Who am I").
		BearerJWT()
	app.ServeDocs(scalar.UI).RequireRole("admin") // docs for admins only

	app.Get("/me", func(c *veyro.Context, _ veyro.None) (*veyro.User, error) {
		c.Logger().Information("Whoami for {User}", c.User().Name)
		return c.User(), nil
	}).Tags("auth").Summary("Current user").RequireAuthorization()

	log.Information("Listening on {Addr}", ":8080")
	if err := app.Run(ctx, ":8080"); err != nil {
		log.Error(err, "Server stopped")
		os.Exit(1)
	}
}
