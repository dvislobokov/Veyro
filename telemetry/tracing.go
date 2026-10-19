// Package telemetry instruments Veyro with OpenTelemetry: server spans and
// HTTP metrics following the semantic conventions, plus Setup for the SDK
// with OTLP or Prometheus exporters and trace-correlated srog logs.
//
//	shutdown, err := telemetry.Setup(ctx, telemetry.Config{ServiceName: "users", Endpoint: "otel-collector:4317", Insecure: true})
//	defer shutdown(ctx)
//
//	app := veyro.New()
//	app.Use(telemetry.Tracing(), telemetry.Metrics())
//
// Inside handlers *veyro.Context is a context.Context, so any instrumented
// client (otelhttp, otelpgx, ...) continues the trace automatically, and
// telemetry.StartSpan(c, "repo.GetUser") adds child spans.
package telemetry

import (
	"context"
	"net"
	"net/http"
	"strconv"

	"github.com/dvislobokov/veyro"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

const scope = "github.com/dvislobokov/veyro/telemetry"

// TracingOption customises Tracing.
type TracingOption func(*tracingConfig)

type tracingConfig struct {
	provider   trace.TracerProvider
	propagator propagation.TextMapPropagator
	skip       func(*http.Request) bool
	attrs      func(*http.Request) []attribute.KeyValue
}

// WithTracerProvider uses tp instead of the global provider.
func WithTracerProvider(tp trace.TracerProvider) TracingOption {
	return func(c *tracingConfig) { c.provider = tp }
}

// WithPropagator uses p instead of the global propagator.
func WithPropagator(p propagation.TextMapPropagator) TracingOption {
	return func(c *tracingConfig) { c.propagator = p }
}

// SkipTracing excludes requests (probes, metrics scrapes).
func SkipTracing(fn func(*http.Request) bool) TracingOption {
	return func(c *tracingConfig) { c.skip = fn }
}

// WithSpanAttributes adds attributes to every server span.
func WithSpanAttributes(fn func(*http.Request) []attribute.KeyValue) TracingOption {
	return func(c *tracingConfig) { c.attrs = fn }
}

// Tracing starts a server span per request named "<METHOD> <route>" (the
// route pattern, not the raw path), continues a trace from incoming W3C
// traceparent/baggage headers, and records the HTTP semantic-convention
// attributes, the Veyro request id and the authenticated user. 5xx
// responses mark the span as an error.
func Tracing(opts ...TracingOption) veyro.Middleware {
	cfg := tracingConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.skip != nil && cfg.skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			provider := cfg.provider
			if provider == nil {
				provider = otel.GetTracerProvider()
			}
			propagator := cfg.propagator
			if propagator == nil {
				propagator = otel.GetTextMapPropagator()
			}
			ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

			attrs := requestAttributes(r)
			if cfg.attrs != nil {
				attrs = append(attrs, cfg.attrs(r)...)
			}
			if id := veyro.TraceID(ctx); id != "" {
				attrs = append(attrs, attribute.String("veyro.request_id", id))
			}
			ctx, span := provider.Tracer(scope).Start(ctx, r.Method,
				trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
			defer span.End()

			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r.WithContext(ctx))

			if rt := veyro.RouteFrom(ctx); rt != nil {
				span.SetName(r.Method + " " + rt.Pattern)
				span.SetAttributes(semconv.HTTPRoute(rt.Pattern))
			}
			status := sw.Status()
			span.SetAttributes(semconv.HTTPResponseStatusCode(status), semconv.HTTPResponseBodySize(int(sw.bytes)))
			if u := veyro.UserFrom(ctx); u.IsAuthenticated() {
				if id := u.ID(); id != "" {
					span.SetAttributes(semconv.EnduserID(id))
				}
			}
			if status >= 500 {
				span.SetStatus(codes.Error, http.StatusText(status))
				span.SetAttributes(semconv.ErrorTypeKey.String(strconv.Itoa(status)))
			}
		})
	}
}

// StartSpan starts a child span of the request (or any) context, using the
// provider of the current span so it lands in the same pipeline.
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	provider := trace.SpanFromContext(ctx).TracerProvider()
	if provider == nil {
		provider = otel.GetTracerProvider()
	}
	return provider.Tracer(scope).Start(ctx, name, opts...)
}

// Span returns the current span of ctx (a no-op span when there is none).
func Span(ctx context.Context) trace.Span { return trace.SpanFromContext(ctx) }

// RequestID returns the W3C trace id of the current span, for use with
// veyro.WithRequestID when logs and responses should carry the trace id.
// It falls back to Veyro's own generator when no span is active, so it
// only makes sense when Tracing runs before the request id is read (the
// id is taken when the App starts handling the request, so use
// WithRequestID only together with an upstream proxy that sets
// traceparent, or prefer the veyro.request_id span attribute).
func RequestID(r *http.Request) string {
	if sc := trace.SpanContextFromContext(r.Context()); sc.HasTraceID() {
		return sc.TraceID().String()
	}
	if id := r.Header.Get("X-Request-Id"); id != "" {
		return id
	}
	return ""
}

func requestAttributes(r *http.Request) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(r.Method),
		semconv.URLPath(r.URL.Path),
		semconv.URLScheme(scheme(r)),
		semconv.NetworkProtocolVersion(protocolVersion(r)),
	}
	if host, port, err := net.SplitHostPort(r.Host); err == nil {
		attrs = append(attrs, semconv.ServerAddress(host))
		if p, err := strconv.Atoi(port); err == nil {
			attrs = append(attrs, semconv.ServerPort(p))
		}
	} else if r.Host != "" {
		attrs = append(attrs, semconv.ServerAddress(r.Host))
	}
	if q := r.URL.RawQuery; q != "" {
		attrs = append(attrs, semconv.URLQuery(q))
	}
	if ua := r.UserAgent(); ua != "" {
		attrs = append(attrs, semconv.UserAgentOriginal(ua))
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		attrs = append(attrs, semconv.ClientAddress(host))
	}
	if r.ContentLength > 0 {
		attrs = append(attrs, semconv.HTTPRequestBodySize(int(r.ContentLength)))
	}
	return attrs
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if s := r.Header.Get("X-Forwarded-Proto"); s != "" {
		return s
	}
	return "http"
}

func protocolVersion(r *http.Request) string {
	switch r.ProtoMajor {
	case 2:
		return "2"
	case 3:
		return "3"
	}
	return "1.1"
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusWriter) Status() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
