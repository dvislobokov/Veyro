package telemetry

import (
	"net/http"
	"time"

	"github.com/dvislobokov/veyro"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// MetricsOption customises Metrics.
type MetricsOption func(*metricsConfig)

type metricsConfig struct {
	provider metric.MeterProvider
	skip     func(*http.Request) bool
}

// WithMeterProvider uses mp instead of the global provider.
func WithMeterProvider(mp metric.MeterProvider) MetricsOption {
	return func(c *metricsConfig) { c.provider = mp }
}

// SkipMetrics excludes requests from the metrics.
func SkipMetrics(fn func(*http.Request) bool) MetricsOption {
	return func(c *metricsConfig) { c.skip = fn }
}

// Metrics records the stable HTTP server metrics of the semantic
// conventions, attributed by http.request.method, http.route,
// http.response.status_code and url.scheme:
//
//	http.server.request.duration   histogram, seconds
//	http.server.active_requests    up-down counter
//	http.server.request.body.size  histogram, bytes
//	http.server.response.body.size histogram, bytes
//
// Grafana's and the Collector's built-in HTTP dashboards read exactly these.
func Metrics(opts ...MetricsOption) veyro.Middleware {
	cfg := metricsConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	provider := cfg.provider
	if provider == nil {
		provider = otel.GetMeterProvider()
	}
	meter := provider.Meter(scope)
	duration, _ := meter.Float64Histogram("http.server.request.duration",
		metric.WithUnit("s"), metric.WithDescription("Duration of HTTP server requests."),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10))
	active, _ := meter.Int64UpDownCounter("http.server.active_requests",
		metric.WithUnit("{request}"), metric.WithDescription("Number of active HTTP server requests."))
	reqSize, _ := meter.Int64Histogram("http.server.request.body.size",
		metric.WithUnit("By"), metric.WithDescription("Size of HTTP server request bodies."))
	respSize, _ := meter.Int64Histogram("http.server.response.body.size",
		metric.WithUnit("By"), metric.WithDescription("Size of HTTP server response bodies."))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.skip != nil && cfg.skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			ctx := r.Context()
			activeAttrs := metric.WithAttributes(semconv.HTTPRequestMethodKey.String(r.Method), semconv.URLScheme(scheme(r)))
			active.Add(ctx, 1, activeAttrs)
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			elapsed := time.Since(start)
			active.Add(ctx, -1, activeAttrs)

			attrs := []attribute.KeyValue{
				semconv.HTTPRequestMethodKey.String(r.Method),
				semconv.HTTPResponseStatusCode(sw.Status()),
				semconv.URLScheme(scheme(r)),
			}
			if rt := veyro.RouteFrom(ctx); rt != nil {
				attrs = append(attrs, semconv.HTTPRoute(rt.Pattern))
			}
			if sw.Status() >= 500 {
				attrs = append(attrs, semconv.ErrorTypeKey.String(http.StatusText(sw.Status())))
			}
			set := metric.WithAttributeSet(attribute.NewSet(attrs...))
			duration.Record(ctx, elapsed.Seconds(), set)
			if r.ContentLength > 0 {
				reqSize.Record(ctx, r.ContentLength, set)
			}
			respSize.Record(ctx, sw.bytes, set)
		})
	}
}
