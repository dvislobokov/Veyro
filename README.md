<div align="center">

# Veyro

**Typed HTTP handlers for Go 1.27. No boilerplate, no magic context, no codegen.**

[![Go 1.27+](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/doc/go1.27)
[![Go Reference](https://pkg.go.dev/badge/github.com/dvislobokov/veyro.svg)](https://pkg.go.dev/github.com/dvislobokov/veyro)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Zero dependencies](https://img.shields.io/badge/dependencies-0-success)](go.mod)

</div>

```go
type CreateUser struct {
    Name  string `json:"name"  validate:"required,minlen=2,maxlen=64"`
    Email string `json:"email" validate:"required,email"`
}

func createUser(ctx context.Context, in CreateUser) (User, error) {
    u := User{ID: uuid.NewV7(), Name: in.Name, Email: in.Email}
    veyro.Created(ctx, "/users/"+u.ID.String())
    return u, nil
}

app := veyro.New()
app.Post("/users", createUser) // In and Out inferred from the handler
app.Run(ctx, ":8080")
```

That's a complete endpoint: JSON decoding, validation, status code, error
handling and a problem-details response for anything that goes wrong.

---

## Why Veyro

Go 1.27 added [**generic methods**](https://go.dev/doc/go1.27). Veyro is the
first framework built on them: `app.Post` carries its own type parameters, so a
handler is just a function from input to output. No `Register[In, Out](...)`
helpers, no `c.Bind(&req)`, no `func(w, r)`.

| | Veyro | Gin / Echo / Fiber | Huma | net/http |
|---|:---:|:---:|:---:|:---:|
| Typed input and output | ✅ | ❌ | ✅ | ❌ |
| Fluent `app.Post(path, fn)` | ✅ | ✅ | ❌ | ✅ |
| Plain `context.Context` in handlers | ✅ | ❌ | ✅ | ✅ |
| Path, query, headers and body in one struct | ✅ | ❌ | partially | ❌ |
| Problem details (RFC 9457) everywhere | ✅ | ❌ | ✅ | ❌ |
| Works with any `net/http` middleware | ✅ | ❌ | ✅ | ✅ |
| Dependencies | 0 | many | some | 0 |

## Install

```sh
go get github.com/dvislobokov/veyro
```

Requires Go 1.27 or newer.

## One struct per request

Describe the whole request in a single type. Tagged fields come from the URL,
headers or cookies; everything else is the JSON body.

```go
type UpdateUser struct {
    ID      uuid.UUID `path:"id"`
    Expand  []string  `query:"expand"`
    IfMatch string    `header:"If-Match"`
    Session string    `cookie:"sid"`

    Name  string  `json:"name"  validate:"required,minlen=2"`
    Age   *int    `json:"age"   validate:"min=18"`
    Role  string  `json:"role"  validate:"oneof=admin user"`
}

app.Put("/users/{id}", func(ctx context.Context, in UpdateUser) (User, error) { ... })
```

Supported for `path`, `query`, `header` and `cookie`: strings, bools, ints,
uints, floats, `time.Duration`, any `encoding.TextUnmarshaler` (`uuid.UUID`,
`time.Time`, `netip.Addr`, ...), pointers, and slices for repeated values.
A `path` tag with no matching `{name}` in the pattern panics at startup,
not in production.

Use `veyro.None` for handlers without input; return `veyro.None` for
`204 No Content`.

## Validation

Rules live in the `validate` tag and run before your handler is called.

| Rule | Meaning |
|---|---|
| `required` | not the zero value |
| `min=N`, `max=N` | numeric bounds |
| `minlen=N`, `maxlen=N` | length of a string, slice or map |
| `oneof=a b c` | one of the listed values |
| `email` | looks like an e-mail address |

A zero value means "not provided", so only `required` complains about it;
use a pointer when zero itself must be checked. Nested structs, slices and
maps are validated recursively. For cross-field rules implement `Validator`:

```go
func (s *Signup) Validate(ctx context.Context) error {
    if s.Password != s.Confirm {
        return veyro.Invalid("confirm", "Passwords do not match.")
    }
    return nil
}
```

## Errors are problem details

Every error response is `application/problem+json` (RFC 9457), in exactly the
shape ASP.NET Core clients already understand:

```json
{
  "type": "https://tools.ietf.org/html/rfc9110#section-15.5.1",
  "title": "One or more validation errors occurred.",
  "status": 400,
  "errors": {
    "id":           ["The value 'abc' is not valid."],
    "name":         ["The field name must be a string or array type with a minimum length of '2'."],
    "address.city": ["The address.city field is required."],
    "$":            ["The request body is not valid JSON: unexpected EOF."]
  },
  "traceId": "01994c0e-6b7a-7c4e-9d2b-1f3a5e0c8b2d"
}
```

Return a problem from a handler to pick the status:

```go
return User{}, veyro.NotFound("user does not exist").With("userId", id)
```

Any other error becomes a `500` with no detail leaked (turn on
`veyro.ExposeErrors(true)` in development). Unknown routes, wrong methods,
unsupported media types, oversized bodies and panics all answer with problems
too, and every response carries an `X-Request-Id`.

## Shaping the response

```go
veyro.SetStatus(ctx, http.StatusAccepted)
veyro.Header(ctx).Set("Cache-Control", "no-store")
veyro.SetCookie(ctx, &http.Cookie{Name: "sid", Value: sid})
veyro.Created(ctx, "/orders/42")
```

Need full control? Return a type that implements `Renderer` and write the
response yourself: redirects, files, server-sent events, HTML.

## Groups and middleware

Middleware is plain `func(http.Handler) http.Handler`, so chi, gorilla and
otelhttp middleware work unchanged.

```go
app := veyro.New(veyro.WithLogger(logger))
app.Use(veyro.Logger(logger))          // every request, 404s included

api := app.Group("/api/v1", auth)
api.Get("/me", me)
api.Raw("GET", "/metrics", promhttp.Handler())
```

Routes use `net/http.ServeMux` patterns: `/users/{id}`, `/files/{path...}`,
`/{$}`. `App` itself is an `http.Handler`, so it drops into any server.

## Example

```sh
go run ./examples/basic
curl -X POST localhost:8080/users -H 'Content-Type: application/json' \
     -d '{"name":"Ann","email":"ann@example.com","age":30}'
```

## Roadmap

- [ ] OpenAPI 3.1 generated from `In`/`Out` types and `validate` rules
- [ ] Form and multipart request bodies
- [ ] Server-sent events over `iter.Seq`
- [ ] Benchmarks, then a custom radix router behind the same API

See [docs/DESIGN.md](docs/DESIGN.md) for the design notes and decisions.

## License

[MIT](LICENSE)
