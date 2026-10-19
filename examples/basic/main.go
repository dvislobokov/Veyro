// Command basic is a minimal Veyro service.
//
//	go run ./examples/basic
//	curl -X POST localhost:8080/users -H 'Content-Type: application/json' -d '{"name":"Ann","email":"ann@example.com","age":30}'
//	curl localhost:8080/users/<id>?expand=profile
//	curl -X POST localhost:8080/users -H 'Content-Type: application/json' -d '{"name":"A"}'   # validation problem
package main

import (
	"context"
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

func (s *store) create(ctx context.Context, in CreateUser) (User, error) {
	u := User{ID: uuid.NewV7(), Name: in.Name, Email: in.Email, Age: in.Age}
	s.mu.Lock()
	s.users[u.ID] = u
	s.mu.Unlock()
	veyro.Created(ctx, "/users/"+u.ID.String())
	return u, nil
}

func (s *store) get(ctx context.Context, in GetUser) (User, error) {
	s.mu.RLock()
	u, ok := s.users[in.ID]
	s.mu.RUnlock()
	if !ok {
		return User{}, veyro.NotFound("user " + in.ID.String() + " does not exist")
	}
	return u, nil
}

func (s *store) list(ctx context.Context, in ListUsers) ([]User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	return out, nil
}

func (s *store) remove(ctx context.Context, in GetUser) (veyro.None, error) {
	s.mu.Lock()
	delete(s.users, in.ID)
	s.mu.Unlock()
	return veyro.None{}, nil
}

func main() {
	db := &store{users: map[uuid.UUID]User{}}

	app := veyro.New()
	app.Use(veyro.Logger(slog.Default()))

	users := app.Group("/users")
	users.Post("", db.create)
	users.Get("", db.list)
	users.Get("/{id}", db.get)
	users.Delete("/{id}", db.remove)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	slog.Info("listening", "addr", ":8080")
	if err := app.Run(ctx, ":8080"); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
