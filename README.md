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

func createUser(c *veyro.Context, in CreateUser) (User, error) {
    u := User{ID: uuid.NewV7(), Name: in.Name, Email: in.Email, Owner: c.User().ID()}
    c.Created("/users/" + u.ID.String())
    return u, nil
}

app := veyro.New()
app.UseAuthentication(veyro.Bearer(verifyToken))
app.Post("/users", createUser).RequireRole("admin") // In and Out inferred
app.ServeDocs("/docs", veyro.Info{Title: "Users API", Version: "1.0.0"})
app.Run(ctx, ":8080")
```

That's a complete endpoint: authentication, authorization, JSON decoding,
validation, status code, a problem-details response for anything that goes
wrong, and an OpenAPI 3.1 document with an interactive reference at `/docs`.

---

## Why Veyro

Go 1.27 added [**generic methods**](https://go.dev/doc/go1.27). Veyro is the
first framework built on them: `app.Post` carries its own type parameters, so a
handler is just a function from input to output. No `Register[In, Out](...)`
helpers, no `c.Bind(&req)`, no `func(w, r)`. The request context, user and
response controls live on `*veyro.Context`, which is itself a `context.Context`.

| | Veyro | Gin / Echo / Fiber | Huma | net/http |
|---|:---:|:---:|:---:|:---:|
| Typed input and output | ✅ | ❌ | ✅ | ❌ |
| Fluent `app.Post(path, fn)` | ✅ | ✅ | ❌ | ✅ |
| Handler context usable as `context.Context` | ✅ | ❌ | ✅ | ✅ |
| Built-in `User`, authentication, `RequireRole` | ✅ | ❌ | ❌ | ❌ |
| OpenAPI 3.1 from types, no annotations or codegen | ✅ | ❌ | ✅ | ❌ |
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

app.Put("/users/{id}", func(c *veyro.Context, in UpdateUser) (User, error) { ... })
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

## The Context

`*veyro.Context` is Veyro's `HttpContext`. It embeds the request context, so
`db.QueryContext(c, ...)` and `context.WithTimeout(c, ...)` just work, and
`veyro.ContextFrom(ctx)` finds it again from any derived context.

```go
c.Request                       // *http.Request
c.User()                        // never nil; anonymous when unauthenticated
c.Status(http.StatusAccepted)   // status for the encoded result
c.Created("/orders/42")         // 201 + Location
c.Header().Set("Cache-Control", "no-store")
c.SetCookie(&http.Cookie{Name: "sid", Value: sid})
c.TraceID()                     // request id
c.Logger()                      // slog.Logger with traceId attached
```

Need full control over the body? Return a type that implements `Renderer`
and write the response yourself: redirects, files, server-sent events, HTML.

## Users, authentication and authorization

`veyro.User` is a `ClaimsPrincipal`: a name, the scheme that authenticated
it and a list of claims, with `IsAuthenticated`, `ID`, `IsInRole`, `HasClaim`,
`FindFirst` and friends.

```go
func verifyToken(ctx context.Context, token string) (*veyro.User, error) {
    claims, err := jwt.Verify(token)          // your JWT library
    if err != nil {
        return nil, err                       // 401 with this detail where auth is required
    }
    return veyro.NewUser("Bearer", claims.Name).
        AddClaim(veyro.ClaimSubject, claims.Sub).
        AddClaim(veyro.ClaimRole, "admin"), nil
}

app.UseAuthentication(veyro.Bearer(verifyToken))

app.Get("/me", me).RequireAuthorization()
app.Get("/admin", dashboard).RequireRole("admin", "owner")
app.Get("/tenant", tenant).RequireClaim("tenant", "acme")

admin := app.Group("/admin").RequireRole("admin")
admin.Get("/health", health).AllowAnonymous()  // opt out, like [AllowAnonymous]
```

Requests without credentials run as anonymous until a requirement is hit:
then they get a `401` problem with `WWW-Authenticate`; authenticated users
failing a policy get `403`. Write your own `Policy` for anything else, and
plug any scheme by implementing `Authenticator`.

## OpenAPI from your types

```go
app.ServeDocs("/docs", veyro.Info{Title: "Users API", Version: "1.0.0"})
```

That serves `/docs/openapi.json` (OpenAPI 3.1) and an interactive reference
at `/docs`. Everything comes from the routes you already registered: `In`
types become parameters and request bodies, `Out` types become responses,
`Require*` adds security and `401`/`403`, and errors are documented as
problem details. Refine it with tags on fields and metadata on routes:

```go
type Role string
func (Role) EnumValues() []string { return []string{"admin", "user"} }

type ListUsers struct {
    Page  int    `query:"page" default:"1" validate:"min=1" doc:"1-based page number"`
    Sort  string `query:"sort" enum:"name,age" default:"name"`
    Role  Role   `query:"role"`                     // enum from the type
    Since *time.Time `query:"since" example:"2026-01-01T00:00:00Z"`
}

users.Get("", list).Summary("List users").Tags("users").Errors(404)
users.Post("", create).Success(201).Deprecated()
app.Get("/internal", probe).Hide()
```

| Tag | Effect |
|---|---|
| `doc:"..."` | description |
| `example:"..."` | example, parsed as the field type |
| `default:"..."` | default in the spec **and** applied when the request omits the value |
| `enum:"a,b,c"` | allowed values, documented and validated |
| `format:"..."` | JSON Schema format (`uri`, `email`, `date`, ...) |
| `deprecated:"true"` | marks the field deprecated |
| `validate:"..."` | `required`, `min`/`max`, `minlen`/`maxlen`, `oneof`, `email` become schema keywords |

Types implementing `EnumValues() []string` are string enums everywhere they
appear, including inside slices. `uuid.UUID`, `time.Time`, `time.Duration`,
`[]byte` and any `encoding.TextMarshaler` get the right string format.
Pointers are nullable; fields are required when `validate:"required"` says
so, and in output-only types every non-nullable field is required.

## Groups and middleware

Middleware is plain `func(http.Handler) http.Handler`, so chi, gorilla and
otelhttp middleware work unchanged.

```go
app := veyro.New(veyro.WithLogger(logger))
app.Use(veyro.Logger(logger))          // every request, 404s included

api := app.Group("/api/v1", rateLimit)
api.Get("/me", me).Use(cache)          // per-route middleware
api.Raw("GET", "/metrics", promhttp.Handler())
```

Middleware chains are built lazily on the first request, so `Use` and
`Require*` can be called in any order relative to route registration.

Routes use `net/http.ServeMux` patterns: `/users/{id}`, `/files/{path...}`,
`/{$}`. `App` itself is an `http.Handler`, so it drops into any server.

## Example

```sh
go run ./examples/basic
curl -X POST localhost:8080/users \
     -H 'Authorization: Bearer ann' -H 'Content-Type: application/json' \
     -d '{"name":"Ann","email":"ann@example.com","age":30}'
curl localhost:8080/me                 # 401 problem with WWW-Authenticate: Bearer
```

## Roadmap

- [x] OpenAPI 3.1 generated from `In`/`Out` types and `validate` rules
- [ ] Form and multipart request bodies
- [ ] Server-sent events over `iter.Seq`
- [ ] Benchmarks, then a custom radix router behind the same API

See [docs/DESIGN.md](docs/DESIGN.md) for the design notes and decisions.

## License

[MIT](LICENSE)
