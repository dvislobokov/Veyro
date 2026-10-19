package telemetry

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/dvislobokov/srog/srogotel"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// Config describes the OpenTelemetry SDK Setup installs.
type Config struct {
	// ServiceName is service.name on every span and metric. Required.
	ServiceName string
	// ServiceVersion is service.version; empty omits it.
	ServiceVersion string
	// Environment is deployment.environment.name; empty omits it.
	Environment string

	// Endpoint is the OTLP collector, host:port without scheme
	// ("otel-collector:4317"). Empty disables OTLP export; traces and
	// metrics then stay in-process unless Prometheus is on.
	Endpoint string
	// Protocol is "grpc" (default) or "http".
	Protocol string
	// Insecure disables TLS to the collector.
	Insecure bool
	// Headers are sent with every export (authentication tokens).
	Headers map[string]string

	// SampleRatio samples traces: 1 keeps everything (default), 0.1 keeps
	// a tenth; sampled parents are always honoured.
	SampleRatio float64
	// MetricInterval is the push interval for OTLP metrics. Default 30s.
	MetricInterval time.Duration

	// Prometheus also exposes metrics for scraping; see PrometheusHandler.
	Prometheus bool
	// Logs forwards srog logs to the collector through OTLP as well.
	Logs bool
	// DisableTraces or DisableMetrics turn one signal off.
	DisableTraces, DisableMetrics bool
}

var promRegistry = prometheus.NewRegistry()

// PrometheusHandler serves the metrics collected when Config.Prometheus is
// on, in Prometheus text format:
//
//	app.Raw("GET", "/metrics", telemetry.PrometheusHandler()).Hide()
func PrometheusHandler() http.Handler {
	return promhttp.HandlerFor(promRegistry, promhttp.HandlerOpts{})
}

// Setup installs the global TracerProvider, MeterProvider and W3C
// propagators according to cfg, and registers trace correlation for srog
// logs. The returned function flushes and shuts everything down; call it
// before the process exits.
func Setup(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("telemetry: Config.ServiceName is required")
	}
	if cfg.Protocol == "" {
		cfg.Protocol = "grpc"
	}
	if cfg.SampleRatio <= 0 || cfg.SampleRatio > 1 {
		cfg.SampleRatio = 1
	}
	if cfg.MetricInterval <= 0 {
		cfg.MetricInterval = 30 * time.Second
	}

	attrs := []attributeKV{semconv.ServiceName(cfg.ServiceName)}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, attribute.String("deployment.environment.name", cfg.Environment))
	}
	if host, err := os.Hostname(); err == nil {
		attrs = append(attrs, semconv.HostName(host))
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL, attrs...))
	if err != nil {
		return nil, err
	}

	var closers []func(context.Context) error
	shutdown = func(ctx context.Context) error {
		var errs []error
		for i := len(closers) - 1; i >= 0; i-- {
			errs = append(errs, closers[i](ctx))
		}
		return errors.Join(errs...)
	}
	fail := func(err error) (func(context.Context) error, error) {
		_ = shutdown(ctx)
		return nil, err
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	if !cfg.DisableTraces {
		opts := []sdktrace.TracerProviderOption{
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		}
		if cfg.Endpoint != "" {
			exp, err := traceExporter(ctx, cfg)
			if err != nil {
				return fail(err)
			}
			opts = append(opts, sdktrace.WithBatcher(exp))
		}
		tp := sdktrace.NewTracerProvider(opts...)
		otel.SetTracerProvider(tp)
		closers = append(closers, tp.Shutdown)
	}

	if !cfg.DisableMetrics {
		opts := []sdkmetric.Option{sdkmetric.WithResource(res)}
		if cfg.Endpoint != "" {
			exp, err := metricExporter(ctx, cfg)
			if err != nil {
				return fail(err)
			}
			opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(cfg.MetricInterval))))
		}
		if cfg.Prometheus {
			reader, err := promexporter.New(promexporter.WithRegisterer(promRegistry))
			if err != nil {
				return fail(err)
			}
			opts = append(opts, sdkmetric.WithReader(reader))
		}
		mp := sdkmetric.NewMeterProvider(opts...)
		otel.SetMeterProvider(mp)
		closers = append(closers, mp.Shutdown)
	}

	srogotel.Install()
	if cfg.Logs && cfg.Endpoint != "" {
		_, sink, err := srogotel.WithLogs(ctx, srogotel.Config{
			Endpoint: cfg.Endpoint, Protocol: cfg.Protocol, Insecure: cfg.Insecure, Headers: cfg.Headers,
		})
		if err != nil {
			return fail(err)
		}
		closers = append(closers, func(context.Context) error { return sink.Close() })
	}
	return shutdown, nil
}

type attributeKV = attribute.KeyValue

func traceExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	if cfg.Protocol == "http" {
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint), otlptracehttp.WithHeaders(cfg.Headers)}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		return otlptracehttp.New(ctx, opts...)
	}
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint), otlptracegrpc.WithHeaders(cfg.Headers)}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	return otlptracegrpc.New(ctx, opts...)
}

func metricExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	if cfg.Protocol == "http" {
		opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(cfg.Endpoint), otlpmetrichttp.WithHeaders(cfg.Headers)}
		if cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		return otlpmetrichttp.New(ctx, opts...)
	}
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint), otlpmetricgrpc.WithHeaders(cfg.Headers)}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	return otlpmetricgrpc.New(ctx, opts...)
}
