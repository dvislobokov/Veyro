# Benchmarks

```sh
cd bench && go test -bench . -benchmem -count 5
```

This is a separate module so Echo, Chi and Gin never become dependencies of
Veyro itself. Two scenarios, see `bench_test.go` for the exact handlers:

- **Route**: a GitHub-like table of about 240 routes; requests alternate
  between a static route, a route with four wildcards, a catch-all and a
  subtree. Handlers are empty, so this is routing plus per-request overhead.
- **JSON**: `POST /users/{id}` with a JSON body bound into a struct, three
  validation rules and a JSON response, written the idiomatic way for each
  framework (Gin runs go-playground/validator, Echo uses a hand-written
  validator, ServeMux and Chi validate by hand).

Results on the shared 4-core container this was developed in (Go 1.27.0,
numbers vary by about 10% between runs; treat them as ranks, not constants):

| | Route ns/op | allocs | JSON ns/op | allocs |
|---|---:|---:|---:|---:|
| Veyro | 740 | 3 | 2 800 | 10 |
| Echo v4 | 95 | 0 | 2 700 | 11 |
| Gin | 90 | 0 | 3 700 | 16 |
| Chi v5 | 650 | 3 | 2 500 | 12 |
| net/http ServeMux | 520 | 3 | 2 100 | 11 |

**Validation** (`BenchmarkValidate_*`): the same nested struct (two
addresses, three items, slices with `dive`, a cross-field rule, formats)
validated by Veyro and by go-playground/validator v10 with equivalent tags:

| | valid input ns/op | allocs | invalid input ns/op | allocs |
|---|---:|---:|---:|---:|
| Veyro | 1 050 | 0 | 3 800 | 44 |
| validator v10 | 6 700 | 51 | 7 300 | 77 |

Veyro compiles each type's rules once into checks specialised for the
field's kind and reads values through field offsets, so the happy path
touches no `reflect.Value` and allocates nothing; errors cost allocations
only when they occur (keys and messages are built lazily).

How to read this:

- On a realistic endpoint Veyro is level with Echo and ahead of Gin, while
  doing more per request: reflection-driven binding of path and body into
  one struct, tag validation, request id, panic recovery and problem-details
  errors.
- On bare routing Echo and Gin win by a wide margin because they never touch
  the request context: parameters live in a pooled framework context. Veyro
  deliberately uses the real `context.Context` (so `c` can be passed to
  databases and cancelled), which costs one context allocation and one
  request copy per request. Veyro's tree lookup itself is allocation-free
  and about three times faster than `ServeMux.Handler`; the rest of the
  difference is that overhead.
- The remaining three allocations on the Route scenario are the context,
  the `*http.Request` copy that carries it, and `SetPathValue` inside
  net/http for Raw handlers (typed handlers read parameters without it).
