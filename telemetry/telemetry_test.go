package telemetry_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dvislobokov/srog"
	"github.com/dvislobokov/veyro"
	"github.com/dvislobokov/veyro/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func newApp() *veyro.App {
	return veyro.New(veyro.WithLogger(srog.MustNew(srog.WithWriter(io.Discard))))
}

func attr(attrs []attribute.KeyValue, key string) any {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value.AsInterface()
		}
	}
	return nil
}

func TestTracing(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prop := propagation.TraceContext{}

	app := newApp()
	app.Use(telemetry.Tracing(telemetry.WithTracerProvider(tp), telemetry.WithPropagator(prop),
		telemetry.SkipTracing(func(r *http.Request) bool { return r.URL.Path == "/probe/liveness" })))
	app.UseAuthentication(veyro.Bearer(func(ctx context.Context, tok string) (*veyro.User, error) {
		return veyro.NewUser("", "ann").AddClaim(veyro.ClaimSubject, "u42"), nil
	}))
	app.Probes()
	app.Get("/users/{id}", func(c *veyro.Context, in struct {
		ID int `path:"id"`
	}) (map[string]int, error) {
		_, span := telemetry.StartSpan(c, "repo.GetUser")
		span.End()
		if !telemetry.Span(c).SpanContext().IsValid() {
			t.Error("handler context must carry the server span")
		}
		return map[string]int{"id": in.ID}, nil
	})
	app.Get("/boom", func(c *veyro.Context, _ veyro.None) (veyro.None, error) { return veyro.None{}, errors.New("x") })

	// Continues an incoming trace.
	parent := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	req := httptest.NewRequest(http.MethodGet, "/users/7?expand=1", nil)
	req.Header.Set("traceparent", parent)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("User-Agent", "test-agent")
	req.RemoteAddr = "203.0.113.9:4567"
	app.ServeHTTP(httptest.NewRecorder(), req)
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/probe/liveness", nil))
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope", nil))

	spans := rec.Ended()
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = s
	}
	server := byName["GET /users/{id}"]
	if server == nil {
		t.Fatalf("server span named by route pattern missing; have %v", names(spans))
	}
	if server.SpanKind() != trace.SpanKindServer || server.SpanContext().TraceID().String() != "0af7651916cd43dd8448eb211c80319c" ||
		server.Parent().SpanID().String() != "b7ad6b7169203331" {
		t.Errorf("trace continuation: kind=%v trace=%s parent=%s", server.SpanKind(), server.SpanContext().TraceID(), server.Parent().SpanID())
	}
	a := server.Attributes()
	want := map[string]any{"http.request.method": "GET", "http.route": "/users/{id}", "url.path": "/users/7", "url.query": "expand=1",
		"http.response.status_code": int64(200), "user_agent.original": "test-agent", "client.address": "203.0.113.9", "enduser.id": "u42", "url.scheme": "http"}
	for k, v := range want {
		if got := attr(a, k); got != v {
			t.Errorf("attr %s = %v, want %v", k, got, v)
		}
	}
	if id := attr(a, "veyro.request_id"); id == nil || id == "" {
		t.Error("veyro.request_id attribute missing")
	}
	if child := byName["repo.GetUser"]; child == nil || child.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Error("StartSpan must create a child of the server span")
	}
	boom := byName["GET /boom"]
	if boom == nil || boom.Status().Code != codes.Error || attr(boom.Attributes(), "http.response.status_code") != int64(500) || attr(boom.Attributes(), "error.type") != "500" {
		t.Errorf("5xx span: %+v", boom)
	}
	if byName["GET /probe/liveness"] != nil {
		t.Error("skipped request must not produce a span")
	}
	if nf := byName["GET"]; nf == nil || attr(nf.Attributes(), "http.response.status_code") != int64(404) || attr(nf.Attributes(), "http.route") != nil {
		t.Errorf("404 keeps the method-only name and no route: %v", names(spans))
	}
}

func names(spans []sdktrace.ReadOnlySpan) []string {
	var out []string
	for _, s := range spans {
		out = append(out, s.Name())
	}
	return out
}

func TestMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	app := newApp()
	app.Use(telemetry.Metrics(telemetry.WithMeterProvider(mp)))
	app.Post("/items", func(c *veyro.Context, in struct {
		Name string `json:"name"`
	}) (map[string]string, error) {
		return map[string]string{"name": in.Name}, nil
	})
	for range 3 {
		req := httptest.NewRequest(http.MethodPost, "/items", strings.NewReader(`{"name":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		app.ServeHTTP(httptest.NewRecorder(), req)
	}
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", nil))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			found[m.Name] = true
			if m.Name == "http.server.request.duration" {
				h := m.Data.(metricdata.Histogram[float64])
				var okCount uint64
				for _, dp := range h.DataPoints {
					route, _ := dp.Attributes.Value("http.route")
					status, _ := dp.Attributes.Value("http.response.status_code")
					if route.AsString() == "/items" && status.AsInt64() == 200 {
						okCount = dp.Count
					}
					if status.AsInt64() == 404 && dp.Attributes.HasValue("http.route") {
						t.Error("404 must not carry a route attribute")
					}
				}
				if okCount != 3 {
					t.Errorf("duration count for POST /items = %d, want 3", okCount)
				}
				if m.Unit != "s" {
					t.Errorf("duration unit %q", m.Unit)
				}
			}
			if m.Name == "http.server.active_requests" {
				for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
					if dp.Value != 0 {
						t.Errorf("active requests must return to 0, got %d", dp.Value)
					}
				}
			}
		}
	}
	for _, name := range []string{"http.server.request.duration", "http.server.active_requests", "http.server.request.body.size", "http.server.response.body.size"} {
		if !found[name] {
			t.Errorf("metric %s not recorded (have %v)", name, found)
		}
	}
}

func TestSetupWithoutEndpoint(t *testing.T) {
	shutdown, err := telemetry.Setup(context.Background(), telemetry.Config{ServiceName: "t", Prometheus: true})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(context.Background())

	app := newApp()
	app.Use(telemetry.Tracing(), telemetry.Metrics())
	app.Raw("GET", "/metrics", telemetry.PrometheusHandler())
	app.Get("/x", func(c *veyro.Context, _ veyro.None) (string, error) { return "ok", nil })
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "http_server_request_duration_seconds") {
		t.Errorf("prometheus output: %d\n%.400s", rec.Code, rec.Body.String())
	}
	if _, err := telemetry.Setup(context.Background(), telemetry.Config{}); err == nil {
		t.Error("ServiceName must be required")
	}
}
