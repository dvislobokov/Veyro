// Command basic is a minimal Veyro service.
//
//	go run ./examples/basic
//	curl -X POST localhost:8080/users -H 'Authorization: Bearer ann' -H 'Content-Type: application/json' -d '{"name":"Ann","email":"ann@example.com","age":30}'
//	curl localhost:8080/users/<id>?expand=profile
//	curl -X POST localhost:8080/users -H 'Authorization: Bearer ann' -H 'Content-Type: application/json' -d '{"name":"A"}'   # validation problem
//	curl localhost:8080/me                                  # 401 with WWW-Authenticate: Bearer
//	curl -X DELETE localhost:8080/users/<id> -H 'Authorization: Bearer ann'   # 403
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"uuid"

	"github.com/dvislobokov/veyro"
)

type User struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Email string    `json:"email"`
	Age   int       `json:"age"`
}

type CreateUser struct {
	Name  string `json:"name" validate:"required,minlen=2,maxlen=64"`
	Email string `json:"email" validate:"required,email"`
	Age   int    `json:"age" validate:"min=18,max=130"`
}

type GetUser struct {
	ID     uuid.UUID `path:"id"`
	Expand []string  `query:"expand"`
}

type ListUsers struct {
	Page    int `query:"page" validate:"min=1"`
	PerPage int `query:"per_page" validate:"max=100"`
}

type store struct {
	mu    sync.RWMutex
	users map[uuid.UUID]User
}

func (s *store) create(c *veyro.Context, in CreateUser) (User, error) {
	u := User{ID: uuid.NewV7(), Name: in.Name, Email: in.Email, Age: in.Age}
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

	app := veyro.New()
	app.Use(veyro.Logger(slog.Default()))
	app.Use(veyro.Authentication(veyro.Bearer(lookupToken)))

	users := app.Group("/users").RequireAuthorization()
	users.Post("", db.create)
	users.Get("", db.list).AllowAnonymous()
	users.Get("/{id}", db.get).AllowAnonymous()
	users.Delete("/{id}", db.remove).RequireRole("admin")

	app.Get("/me", func(c *veyro.Context, _ veyro.None) (*veyro.User, error) {
		return c.User(), nil
	}).RequireAuthorization()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	slog.Info("listening", "addr", ":8080")
	if err := app.Run(ctx, ":8080"); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
