# Veyro

A small HTTP framework for Go 1.27+. Handlers are plain typed functions;
request binding, validation, JSON and error responses are handled for you.

```go
type CreateUser struct {
    Name  string `json:"name"  validate:"required,minlen=2"`
    Email string `json:"email" validate:"required,email"`
}

type GetUser struct {
    ID     uuid.UUID `path:"id"`
    Expand []string  `query:"expand"`
}

func create(ctx context.Context, in CreateUser) (User, error) { ... }
func get(ctx context.Context, in GetUser) (User, error)       { ... }

app := veyro.New()
app.Post("/users", create)     // In and Out are inferred from the handler
app.Get("/users/{id}", get)
app.Run(ctx, ":8080")
```

Veyro relies on Go 1.27 **generic methods**: `app.Post` has its own type
parameters, so no `Register[In, Out](app, ...)` helpers are needed.

## Request binding

One struct describes the whole request. Fields tagged `path`, `query`,
`header` or `cookie` come from those sources; every other exported field is
decoded from the JSON body (encoding/json/v2). Use `veyro.None` when there is
no input.

Supported field types for non-body sources: strings, bools, ints, uints,
floats, `time.Duration`, anything implementing `encoding.TextUnmarshaler`
(`uuid.UUID`, `time.Time`, `netip.Addr`, ...), pointers to those, and slices
for repeated query/header values. A `path` tag whose name is missing from the
route pattern panics at registration.

## Validation

Rules live in the `validate` tag: `required`, `min=`, `max=`, `minlen=`,
`maxlen=`, `oneof=a b c`, `email`. Zero values are "not provided": only
`required` complains about them, so use a pointer when zero must be checked.
Nested structs and slices are validated recursively. For cross-field logic
implement `Validate(ctx) error` and return `veyro.ValidationErrors`.

## Errors

Every error response is RFC 9457 `application/problem+json`, in the shape
ASP.NET Core produces:

```json
{
  "type": "https://tools.ietf.org/html/rfc9110#section-15.5.1",
  "title": "One or more validation errors occurred.",
  "status": 400,
  "errors": {
    "name": ["The name field is required."],
    "age": ["The field age must be greater than or equal to 18."],
    "$": ["The request body is not valid JSON: ..."]
  },
  "traceId": "01994c0e-6b7a-7c4e-9d2b-1f3a5e0c8b2d"
}
```

Handlers return `*veyro.Problem` (`veyro.NotFound(...)`, `veyro.Conflict(...)`,
`veyro.NewProblem(status, detail)`) for specific statuses; any other error is a
500 without detail (enable `veyro.ExposeErrors(true)` in development). Unknown
routes and wrong methods also answer with problems.

## Responses

The handler result is encoded as JSON with status 200. `veyro.None` yields 204.
`veyro.SetStatus`, `veyro.Created`, `veyro.Header` and `veyro.SetCookie` adjust
the response from inside a handler. A result type implementing `Renderer`
writes the response itself (redirects, files, streams).

## Middleware

`func(http.Handler) http.Handler`, so chi, gorilla and otelhttp middleware
work unchanged. `app.Use` wraps every request including 404s; `group.Use`
applies to routes registered on that group afterwards.

## Notes

- Routing is `net/http.ServeMux` (Go 1.22 patterns), so `{id}`, `{path...}`
  and `{$}` work as documented there.
- `encoding/json/v2` has no default encoding for `time.Duration`; tag such
  fields with a `format` option or exclude them with `json:"-"`.
