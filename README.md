<div align="center">

# Veyro

**Typed HTTP handlers for Go 1.27. No boilerplate, no magic context, no codegen.**

[![Go 1.27+](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/doc/go1.27)
[![Go Reference](https://pkg.go.dev/badge/github.com/dvislobokov/veyro.svg)](https://pkg.go.dev/github.com/dvislobokov/veyro)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Dependencies](https://img.shields.io/badge/dependencies-srog-success)](go.mod)

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
app.OpenAPIConfig().Title("Users API").Version("1.0.0").BearerJWT()
app.ServeDocs(scalar.UI)
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
| Dependencies | 1 (logging) | many | some | 0 |

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

Rules live in the `validate` tag, with validator v10 syntax, and run before
your handler is called. Veyro's validator is its own: rules are compiled
once per type into type-specialised checks, values are read in place, and
a valid input is validated with **zero allocations**, about six times
faster than validator v10 on the same struct (see `bench/`).

| Group | Rules |
|---|---|
| Presence | `required`, `required_if=Field value`, `required_unless`, `required_with=F1 F2`, `required_with_all`, `required_without`, `required_without_all` |
| Numbers and lengths | `min`, `max`, `len`, `gt`, `gte`, `lt`, `lte`, `eq`, `ne` (value for numbers, length for strings, slices, maps) |
| Strings | `eq`, `ne`, `contains`, `excludes`, `startswith`, `endswith`, `alpha`, `alphanum`, `alphaunicode`, `alphanumunicode`, `numeric`, `lowercase`, `uppercase`, `ascii`, `printascii`, `boolean`, `oneof=a b c`, `regex=…` |
| Formats | `email`, `url`, `http_url`, `uri`, `uuid`, `ip`, `ipv4`, `ipv6`, `cidr`, `mac`, `hostname`, `fqdn`, `datetime=layout`, `base64`, `json`, `hexadecimal`, `hexcolor`, `e164` |
| Cross-field | `eqfield`, `nefield`, `gtfield`, `gtefield`, `ltfield`, `ltefield` (numbers, strings, `time.Time`) |
| Collections | `dive` (rules after it apply to elements), `unique`; a string rule on `[]string` applies to every element |

A zero value means "not provided": only the presence rules complain about
it, so optional fields with rules work without pointers (`omitempty` is
accepted and does nothing). Use a pointer when zero must be checked.
Nested structs, slices and maps are validated recursively with keys like
`items[2].price`. Enums (`enum:"a,b"` tag or an `EnumValues()` type) match
case-insensitively and are rewritten to the canonical spelling.

For cross-field logic implement `Validate(ctx) error` or, sconf-style,
`Validate() error`; it runs on every nested value after its fields:

```go
func (s *Signup) Validate(ctx context.Context) error {
    if taken(ctx, s.Email) {
        return veyro.Invalid("email", "This e-mail is already registered.")
    }
    return nil
}
```

Custom rules are a `veyro.RegisterRule("even", veyro.Rule{Check: ..., Message: ...})`
away, and `veyro.Validate(ctx, &v)` validates anything outside a handler.

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
`FindFirst` and friends. Built-in schemes: `Bearer`, `Basic` and `APIKey`
(header, query or cookie); anything else implements `Authenticator`.

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
import "github.com/dvislobokov/veyro/scalar"

app.OpenAPIConfig().
    Title("Users API").Version("1.0.0").
    Server("https://api.example.com", "production").
    Tag("users", "User accounts").
    BearerJWT()
app.ServeDocs(scalar.UI)
```

`OpenAPIConfig` starts with sensible defaults (title "API", version "1.0.0",
server `/`, docs at `/docs`) and has helpers for every common security
scheme: `Basic()`, `Bearer(format)`, `BearerJWT()`, `APIKey(name, in)`,
`OIDC(discoveryURL)`, `OAuth2AuthorizationCode(...)`,
`OAuth2ClientCredentials(...)` and raw `SecurityScheme(name, scheme)`.
Authenticators passed to `UseAuthentication` (`Bearer`, `Basic`, `APIKey`)
register their scheme automatically. Secured routes accept any registered
scheme; `.Security("oauth2", "read:users")` narrows it per route.

That serves `/docs/openapi.json` (OpenAPI 3.1) and an interactive
[Scalar](https://github.com/scalar/scalar) reference at `/docs`. The UI is
embedded in your binary and makes **no external requests**: no CDN, no web
fonts, no request proxy, no telemetry. It works on an air-gapped network.
Pass `nil` instead of `scalar.UI` to serve only the JSON, or implement
`veyro.DocsUI` to plug in another viewer. `ServeDocs` returns the docs
router group, so `app.ServeDocs(scalar.UI).RequireRole("admin")` keeps
the reference private. Everything in the document comes
from the routes you already registered: `In`
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
appear, including inside slices. Types implementing `Example() any` get a
whole-object example on their schema, on request bodies and on responses,
which the reference UI uses as the ready-made "Try it" payload:

```go
func (CreateUser) Example() any {
    return CreateUser{Name: "Ann", Email: "ann@example.com", Age: 30}
}
``` `uuid.UUID`, `time.Time`, `time.Duration`,
`[]byte` and any `encoding.TextMarshaler` get the right string format.
Pointers are nullable; fields are required when `validate:"required"` says
so, and in output-only types every non-nullable field is required.

## Probes

```go
app.Probes()                                   // GET /probe/liveness, GET /probe/healthy
app.AddCheck("db", func(ctx context.Context) error { return db.PingContext(ctx) })
app.AddCheck("cache", cache.Ping)
```

`/probe/liveness` answers `{"status":"ok"}` while the process can serve.
`/probe/healthy` runs every check concurrently under a timeout and answers
`200` or `503` with each check's result:

```json
{"status":"fail","checks":{"db":{"status":"fail","error":"connection refused","duration":"2.1ms"},
                           "cache":{"status":"ok","duration":"310µs"}}}
```

Probes are public even when the App requires authorization, and hidden
from the OpenAPI document.

## Logging

Veyro logs through [srog](https://github.com/dvislobokov/srog), a
Serilog-style structured logger. `c.Logger()` is the App logger enriched
with the request id, and `srog.Ctx(ctx)` picks the id up from any context
derived from a request:

```go
app := veyro.New(veyro.WithLogger(srog.NewDevelopment()))

func get(c *veyro.Context, in GetUser) (User, error) {
    c.Logger().Information("Loading {UserId}", in.ID)   // RequestId attached
    return repo.Get(c, in.ID)                            // srog.Ctx(ctx) inside works too
}
```

## Middleware

The `middleware` package ships the usual set, all plain
`func(http.Handler) http.Handler` and all answering with problem details:

| | |
|---|---|
| `Logger(...)` | one srog line per request, level by status, skip lists |
| `CORS(opts)` | origins with wildcards, preflight, credentials, max-age |
| `Timeout(d)` | deadline on the context, `504` problem if exceeded |
| `Gzip(opts)` | by content type and size, streaming-safe |
| `RateLimit(opts)` | token bucket per client, `429` with `Retry-After` |
| `RealIP(proxies...)` | trusts `X-Forwarded-For` only from listed proxies |
| `SecureHeaders(opts)` | nosniff, frame options, referrer policy, HSTS, CSP |

Request ids and panic recovery are built into the App. Anything written
for chi, gorilla or otelhttp works unchanged.

## Groups

```go
app.Use(middleware.Logger(), middleware.CORS(cors))   // every request, 404s included

api := app.Group("/api/v1", middleware.RateLimit(limits))
api.Get("/me", me).Use(cache)                         // per-route middleware
api.Raw("GET", "/metrics", promhttp.Handler())
```

Middleware chains are built lazily on the first request, so `Use` and
`Require*` can be called in any order relative to route registration.

Routes use `net/http.ServeMux` pattern syntax on Veyro's own radix router:
`/users/{id}`, `/files/{path...}`, `/static/`, `/{$}`. Literal segments win
over wildcards, wrong methods get `405` with `Allow`, and lookups allocate
nothing. `App` itself is an `http.Handler`, so it drops into any server.

## Performance

On a typical JSON endpoint (bind path + body, validate, respond) Veyro is
level with Echo and ahead of Gin while doing more per request; its router
lookup is allocation-free and about three times faster than `ServeMux`.
Numbers, methodology and caveats are in [bench/README.md](bench/README.md).

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
- [x] Custom radix router with `ServeMux` pattern syntax

See [docs/DESIGN.md](docs/DESIGN.md) for the design notes and decisions.

## License

[MIT](LICENSE)
