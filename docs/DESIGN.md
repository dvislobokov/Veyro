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
| 1.26 | `slog.NewMultiHandler` | Логирование |
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

Единственная «настоящая» форма — `func(ctx context.Context, in In) (Out, error)`.
Удобные формы сводятся к ней generic-методами:

```go
func (g *Group) Handle[In, Out any](pattern string, h func(context.Context, In) (Out, error))
func (g *Group) Get[Out any](pattern string, h func(context.Context) (Out, error))        // без тела
func (g *Group) GetIn[In, Out any](pattern string, h func(context.Context, In) (Out, error)) // In из path/query
func (g *Group) Post[In, Out any](pattern string, h func(context.Context, In) (Out, error))
func (g *Group) Raw(pattern string, h http.Handler)                                        // полный доступ к w/r
```

Доступ к низкоуровневым вещам (заголовки, статус, cookies) — через контекст:
`veyro.RequestFrom(ctx)`, `veyro.SetStatus(ctx, 201)`, `veyro.SetHeader(ctx, ...)`.

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

Старт — обёртка над `http.ServeMux` (Go 1.22+): нулевые зависимости, паттерны с
методами и `{wildcards}`, 307-редиректы (1.26), приоритет по специфичности. Если
бенчмарки покажут необходимость — заменим на radix-дерево с тем же синтаксисом
паттернов; публичный API не изменится.

## Чего не делаем (на старте)

- Собственный `Context`-тип в сигнатуре хендлера (как в Gin/Echo/Fiber): он мешает
  переиспользованию хендлеров и ломает `context.Context`-идиомы.
- Зависимости вне stdlib в ядре. Валидация, OpenAPI — отдельные пакеты.
- Поддержка Go < 1.27: весь смысл фреймворка в generic-методах.

## Принятые решения

1. Роутер — `ServeMux` внутри. Известная цена: `ServeHTTP` делает два поиска
   маршрута (один для перехвата 404/405, второй в `ServeMux.ServeHTTP`, который
   заполняет path values). Уйдёт при переходе на своё дерево.
2. Биндинг — struct-теги в одной структуре, по образцу ASP.NET Core.
3. OpenAPI — следующий этап; все типы `In`/`Out` уже известны при регистрации.

## Следующие шаги

- OpenAPI 3.1 из типов `In`/`Out` и правил `validate`.
- Form и multipart биндинг для тела.
- SSE/стриминг через `iter.Seq` и `Renderer`.
- Бенчмарки против ServeMux и chi, затем решение о собственном роутере.

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
