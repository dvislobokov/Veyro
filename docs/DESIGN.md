# Veyro — дизайн фреймворка

Статус: ядро реализовано (см. корень репозитория). Проверено на `go1.27.0 linux/amd64`.

## Идея в одном абзаце

Veyro — тонкий слой над `net/http`, который использует **generic-методы Go 1.27**,
чтобы хендлеры были обычными типизированными функциями, а не `func(w, r)`.
Фреймворк берёт на себя декодирование запроса, валидацию, кодирование ответа и
ошибки, но остаётся полностью совместимым с `http.Handler` и экосистемой middleware.

```go
type CreateUser struct {
    Name string `json:"name"`
}

type User struct {
    ID   uuid.UUID `json:"id"`
    Name string    `json:"name"`
}

func createUser(ctx context.Context, in CreateUser) (User, error) {
    return User{ID: uuid.NewV7(), Name: in.Name}, nil // uuid — stdlib в Go 1.27
}

app := veyro.New()
app.Post("/users", createUser) // In=CreateUser, Out=User выведены компилятором
app.Get("/users/{id}", getUser)
```

До Go 1.27 это было невозможно: методы не могли иметь собственные параметры типов,
поэтому все «типизированные» фреймворки использовали пакетные функции
(`huma.Register(api, ...)`, `Get[In, Out](r, ...)`) — неудобно и не читается как
fluent API.

## Какие возможности Go мы используем и зачем

| Версия | Возможность | Где применяется |
|---|---|---|
| 1.27 | Generic-методы | `app.Get/Post/Put/Delete/Handle` с выводом `In`/`Out` |
| 1.27 | Вывод типов функций во всех контекстах присваивания | Регистрация generic-хендлеров в слайсах/мапах маршрутов без явных типов |
| 1.27 | Ключи struct-литералов как селекторы вложенных полей | Конфиг: `veyro.Config{Server.ReadTimeout: 5*time.Second}` |
| 1.27 | `encoding/json/v2`, `jsontext` | Кодек по умолчанию: строгий, быстрее v1, стриминговый `MarshalWrite`/`UnmarshalRead` |
| 1.27 | `uuid` в stdlib | Request ID, идентификаторы без внешних зависимостей |
| 1.27 | `httptest.NewTestServer` + `testing/synctest` | Детерминированные тесты таймаутов и graceful shutdown без реальной сети |
| 1.26 | `new(expr)` | Опциональные поля в DTO и опциях |
| 1.26 | Самоссылающиеся constraints `T Binder[T]` | Интерфейсы кодеков/биндеров |
| 1.23 | Итераторы `iter.Seq` | Обход маршрутов, заголовков, параметров, SSE-поток |
| 1.22 | Паттерны `ServeMux` (`"GET /users/{id}"`) | Синтаксис маршрутов, совместимость с stdlib |

Ограничение Go 1.27, которое надо помнить: **методы интерфейсов не могут быть
generic**. Поэтому `Router` — конкретный тип, а не интерфейс; расширяемость
достигается композицией (группы, middleware), а не наследованием интерфейса.

## Архитектура

```
veyro/            — публичный API: App, Group, Handler, Error, Ctx
  router/         — сопоставление маршрутов (на старте — обёртка над http.ServeMux)
  codec/          — Codec interface; json (v2), form, query, path-params
  bind/           — сбор In из body + path + query + headers по struct-тегам
  validate/       — опциональный интерфейс Validate() error на In
  middleware/     — recover, requestid (uuid), logger (slog), cors, timeout, compress
  render/         — JSON, SSE (iter.Seq), stream
  openapi/        — генерация спеки из типов In/Out через reflect (позже)
```

### Хендлеры

Единственная «настоящая» форма — `func(c *veyro.Context, in In) (Out, error)`.
`*Context` реализует `context.Context` (встраивает контекст запроса), поэтому его
можно передавать в БД и клиенты напрямую, а `veyro.ContextFrom(ctx)` находит его
из любого производного контекста. Это аналог `HttpContext`: `Request`, `User()`,
`Status()`, `Header()`, `SetCookie()`, `TraceID()`, `Logger()`.
Удобные формы сводятся к ней generic-методами:

```go
func (g *Group) Handle[In, Out any](pattern string, h func(context.Context, In) (Out, error))
func (g *Group) Get[Out any](pattern string, h func(context.Context) (Out, error))        // без тела
func (g *Group) GetIn[In, Out any](pattern string, h func(context.Context, In) (Out, error)) // In из path/query
func (g *Group) Post[In, Out any](pattern string, h func(context.Context, In) (Out, error))
func (g *Group) Raw(pattern string, h http.Handler)                                        // полный доступ к w/r
```

### Пользователь и авторизация

`User` повторяет `ClaimsPrincipal`: имя, схема (`AuthType`) и список `Claim{Type, Value}`
с методами `IsAuthenticated`, `ID`, `IsInRole`, `HasClaim`, `FindFirst`.
`Authentication(auths...)` — middleware, перебирает схемы (`Bearer(...)` встроен,
остальные через интерфейс `Authenticator`); без учётных данных запрос идёт как
анонимный. `Authorize(policies...)` даёт 401 с `WWW-Authenticate` или 403.
Fluent-API на маршрутах и группах: `.RequireAuthorization()`, `.RequireRole()`,
`.RequireClaim()`, `.AllowAnonymous()`, `.Use()`. Цепочка middleware собирается лениво
при первом запросе (`Route.ServeHTTP` + `sync.Once`), поэтому порядок вызовов не важен.

### Биндинг `In`

Одна структура описывает весь запрос, как `[AsParameters]` в ASP.NET Core.
Отдельного поля для тела нет: всё, что не помечено `path`/`query`/`header`/`cookie`,
декодируется из JSON-тела.

```go
type UpdateUser struct {
    ID     uuid.UUID `path:"id"`
    Expand []string  `query:"expand"`
    Trace  string    `header:"X-Trace-Id"`

    Name   string    `json:"name" validate:"required,minlen=2"` // из тела
    Email  string    `json:"email" validate:"email"`             // из тела
}
```

Биндер строится один раз на тип (reflect при регистрации маршрута, не на каждый
запрос) и кэшируется в `sync.Map`/generic-кэше по `reflect.Type`.

### Валидация

Собственный движок (`validate.go`), без зависимостей. Правила в теге `validate` с
синтаксисом validator v10 (presence, числа/длины, строки, форматы, cross-field,
`dive`, `unique`) плюс `enum` без учёта регистра с канонизацией, как в `sconf`.
План компилируется один раз на тип при регистрации маршрута: каждое правило —
замыкание, специализированное по kind поля, значения читаются по смещению через
`unsafe`, `reflect.Value` на горячем пути нет. Валидный вход — ноль аллокаций,
примерно в 6 раз быстрее validator v10 (см. `bench/`). `Validate(ctx) error` и
`Validate() error` вызываются на каждом вложенном значении. Ошибки тегов — паника
при регистрации.

### Ошибки

Формат ответа — RFC 9457 `application/problem+json` в точности как
`ProblemDetails`/`ValidationProblemDetails` в ASP.NET Core: `type` указывает на
секцию RFC 9110, `title` — стандартная фраза, ошибки валидации лежат в
`errors: {"field": ["message"]}`, ошибки разбора JSON — под ключом `$`,
везде есть `traceId`.

```go
type Problem struct {
    Type, Title, Detail, Instance string
    Status  int
    Errors  ValidationErrors   // map[string][]string
    TraceID string
    Extensions map[string]any  `json:",embed"`
}
```

`*Problem` реализует `error`: хендлер возвращает `veyro.NotFound("...")`, а всё
незнакомое превращается в 500 без деталей и логируется через `slog`.

### Middleware

Стандартная сигнатура `func(http.Handler) http.Handler` — чтобы работали все
существующие middleware (chi, gorilla, otelhttp). Плюс типизированный вариант
для случаев, где нужен доступ к `In`/`Out`:
`func (g *Group) UseTyped[In, Out any](mw func(Handler[In, Out]) Handler[In, Out])`.

### Роутер

Собственное дерево с синтаксисом паттернов `http.ServeMux` (Go 1.22+), см. раздел
«Принятые решения». Начинали с обёртки над `ServeMux`; публичный API при замене
не изменился.

## Чего не делаем (на старте)

- `Context`, который *не* является `context.Context` (как в Gin/Echo/Fiber): наш
  `*Context` встраивает контекст запроса, так что идиомы stdlib сохраняются.
- Зависимости вне stdlib в ядре, кроме логгера `srog` (решение владельца: единый
  структурированный логгер во всех проектах). Остальное — подпакеты. Валидация, OpenAPI — отдельные пакеты.
- Поддержка Go < 1.27: весь смысл фреймворка в generic-методах.

## Принятые решения

1. Роутер — своё дерево (`router.go`) с синтаксисом паттернов `ServeMux`:
   `{id}`, `{path...}`, `{$}`, префиксы `/static/`, приоритет литерал → параметр →
   catch-all с откатом, HEAD через GET, 405 с `Allow`, редиректы на очищенный путь и
   trailing slash. Host-паттерны не поддерживаются. Поиск без аллокаций, примерно
   вдвое быстрее `ServeMux.Handler` на том же наборе маршрутов; `ServeMux` остался
   только как эталон в бенчмарке.
2. Биндинг — struct-теги в одной структуре, по образцу ASP.NET Core.
3. OpenAPI генерируется из типов при первом запросе `/docs/openapi.json`.
   Правило обязательности полей: `validate:"required"`; в типах, которые встречаются
   только в ответах, обязательны все не-nullable поля (не указатель, не slice/map,
   без `omitzero`/`omitempty`). Тип, достижимый из любого `In`, считается входным.
4. `default` из тега применяется и при биндинге: для параметров — когда источник пуст,
   для тела — до декодирования JSON, чтобы отсутствующие поля получили значение.

### Наблюдаемость

Подпакет `telemetry` на OpenTelemetry: `Tracing()` даёт серверный span на запрос с
именем по шаблону маршрута и атрибутами HTTP semconv, продолжает трейс из
`traceparent`; `Metrics()` пишет стабильные метрики `http.server.*`; `Setup()`
поднимает SDK с OTLP (gRPC/HTTP) и/или Prometheus и ставит `srogotel`, чтобы логи
несли `trace_id`/`span_id`. Пробы в ядре: `AddCheck`, `/probe/liveness`,
`/probe/healthy`. Версии OTel пришпилены к набору, совместимому с `srogotel`
(`otel` v1.44, `otel/log` v0.20): `otel/log` v1.47 ломает его API.

### JWT

Подпакет `jwt` на stdlib `crypto`: проверка подписи RS/PS/ES/EdDSA (HS только явно),
claims `exp`/`nbf`/`iat`/`iss`/`aud` с leeway, ключи из `StaticKeys`, PEM или `Remote`
(JWKS и OIDC discovery) с кэшем, фоновым обновлением и однократным refresh по
неизвестному `kid` с rate-limit. Для service mesh: `Issuer` в токене и адрес
получения ключей независимы, `Rewrite` для `jwks_uri` из discovery, `HostHeader`,
`TLS` (SNI, mTLS), свои заголовки. Claims маппятся в `User`: роли Keycloak
(`realm_access`, `resource_access.<client>`), группы, scope, остальное плоско.

## Следующие шаги

- ~~OpenAPI 3.1 из типов `In`/`Out` и правил `validate`.~~ Сделано: `openapi.go`,
  `app.OpenAPI()` / `app.ServeDocs()`, теги `doc`/`example`/`default`/`enum`/`format`/
  `deprecated`, интерфейс `Enum`. UI — подпакет `scalar` со встроенным через `embed`
  бандлом (gzip, ~1.3 МБ): никаких CDN, шрифтов, прокси и телеметрии. Ядро про UI не
  знает, принимает `DocsUI`; `nil` — только JSON. Метаданные документа — в
  `app.OpenAPIConfig()` (дефолты: title «API», version «1.0.0», сервер `/`, путь `/docs`)
  с хелперами схем безопасности `Basic/Bearer/BearerJWT/APIKey/OIDC/OAuth2*`;
  аутентификаторы `Bearer/Basic/APIKey` регистрируют свои схемы сами через
  `SecuritySchemer`, явная конфигурация имеет приоритет.
- Form и multipart биндинг для тела.
- SSE/стриминг через `iter.Seq` и `Renderer`.
- ~~Бенчмарки против ServeMux и chi, затем решение о собственном роутере.~~ Сделано.

## Проверенный прототип

Код ниже компилируется и работает на `go1.27.0` (запрос `POST /users` → `{"id":1,"name":"ann"}`):

```go
type Handler[In, Out any] func(ctx context.Context, in In) (Out, error)

type Router struct{ mux *http.ServeMux }

func (r *Router) Handle[In, Out any](pattern string, h Handler[In, Out]) {
    r.mux.HandleFunc(pattern, func(w http.ResponseWriter, req *http.Request) {
        var in In
        if req.ContentLength != 0 {
            if err := json.UnmarshalRead(req.Body, &in); err != nil { // encoding/json/v2
                http.Error(w, err.Error(), http.StatusBadRequest)
                return
            }
        }
        out, err := h(req.Context(), in)
        if err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        w.Header().Set("Content-Type", "application/json")
        _ = json.MarshalWrite(w, out)
    })
}

func (r *Router) Post[In, Out any](pattern string, h Handler[In, Out]) { r.Handle("POST "+pattern, h) }

r.Post("/users", createUser) // func(context.Context, CreateUser) (User, error)
```
