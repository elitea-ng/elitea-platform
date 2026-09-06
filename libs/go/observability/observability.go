// Package observability provides shared OpenTelemetry setup helpers for all
// Elitea Go services. It is the replacement for the legacy tracing plugin's
// server-side SDK wiring (legacy/plugins/tracing/): every service that calls
// New with Enabled=true exports its own spans as an OTLP/HTTP client, batched
// to a collector — the same collector elitea-main's tracing ingest routes
// (internal/api/v2/tracing) proxy browser/worker traces to, so all traces
// land in one place regardless of origin (issue #250).
package observability

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// tracesPath is the OTLP/HTTP path for the traces signal. The spec fixes it,
// and it is what an OTLP/HTTP receiver listens on.
const tracesPath = "/v1/traces"

// Config holds observability configuration.
type Config struct {
	ServiceName    string
	ServiceVersion string
	// OTLPEndpoint is the collector's OTLP/HTTP BASE URL, e.g.
	// "http://otel-collector:4318". It is the generic endpoint, so the signal
	// path is APPENDED to it: this exporter posts to
	// "http://otel-collector:4318/v1/traces".
	//
	// Empty defers to the otlptracehttp exporter's own environment handling,
	// so passing it through explicitly here is a convenience, not the only way
	// to configure it.
	OTLPEndpoint string
	// OTLPTracesEndpoint is the SIGNAL-SPECIFIC endpoint. The spec says it is
	// used VERBATIM — it already names the path — and it wins over
	// OTLPEndpoint. Set it when a collector serves traces somewhere other than
	// the conventional path.
	OTLPTracesEndpoint string
	Enabled            bool
}

// ConfigFromEnv reads the standard OTel environment variables. OTEL_SDK_DISABLED
// is the spec-defined kill switch (https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/);
// defaulting Enabled to true when it is unset or anything other than "true"
// means a deployment gets traces the moment OTEL_EXPORTER_OTLP_ENDPOINT points
// at a real collector, with no separate opt-in flag to remember.
//
// The two endpoint variables carry the same distinction as the two fields, and
// the OTLP specification defines it: the generic
// OTEL_EXPORTER_OTLP_ENDPOINT gets the signal path appended, and the
// signal-specific OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is used as it stands.
func ConfigFromEnv(serviceName, serviceVersion string) Config {
	return Config{
		ServiceName:        serviceName,
		ServiceVersion:     serviceVersion,
		OTLPEndpoint:       os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		OTLPTracesEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		Enabled:            os.Getenv("OTEL_SDK_DISABLED") != "true",
	}
}

// tracesEndpoint is the exact URL the exporter posts spans to.
//
// # The bug this function exists to stop
//
// `otlptracehttp.WithEndpointURL` takes the SIGNAL-SPECIFIC form: it uses the
// URL as given, and a URL with no path is explicitly pinned to "/" so that the
// default signal path is NOT appended (otlpconfig/options.go, WithEndpointURL).
// This package handed it the GENERIC endpoint, "http://otel-collector:4318".
// Every export therefore posted to "http://otel-collector:4318/", the collector
// answered 404, and elitea-main logged
//
//	traces export: failed to send to http://otel-collector:4318/: 404 Not Found
//
// every batch interval, for the life of the process. No span was ever received;
// the noise was the only symptom.
//
// Deriving the URL here, rather than passing the base through, keeps this
// library's behaviour identical to every other OTLP SDK reading the same two
// variables — including the Rust worker in the same compose stack, which lets
// its SDK do the appending.
func (c Config) tracesEndpoint() string {
	if c.OTLPTracesEndpoint != "" {
		return c.OTLPTracesEndpoint
	}
	if c.OTLPEndpoint == "" {
		return ""
	}
	parsed, err := url.Parse(c.OTLPEndpoint)
	if err != nil || parsed.Host == "" {
		// Not a URL this function can extend. Hand it to the exporter as it
		// stands and let the exporter report it.
		return c.OTLPEndpoint
	}
	// A base may carry a path prefix ("http://gateway:4318/otlp"), and the
	// signal path goes UNDER it.
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + tracesPath
	return parsed.String()
}

// Provider wraps the OTel SDK tracer provider. The zero-value-ish disabled
// Provider (returned when Config.Enabled is false) answers Tracer() with the
// global no-op tracer, so callers never need a nil check.
type Provider struct {
	cfg Config
	tp  *sdktrace.TracerProvider
}

// New sets up the OTel SDK and returns a Provider. It also installs the
// tracer provider as the process-wide default via otel.SetTracerProvider, so
// otel.Tracer(...) and otelhttp middleware anywhere in the process pick it up
// without threading the Provider through every call site.
//
// Call Shutdown on process exit to flush telemetry data.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	if !cfg.Enabled {
		slog.Info("observability: OTel disabled, using no-op providers")
		return &Provider{cfg: cfg}, nil
	}

	var opts []otlptracehttp.Option
	endpoint := cfg.tracesEndpoint()
	if endpoint != "" {
		opts = append(opts, otlptracehttp.WithEndpointURL(endpoint))
	}
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("observability: create OTLP trace exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	))
	if err != nil {
		return nil, fmt.Errorf("observability: build resource attributes: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// The resolved URL, not the base. An operator reading this line is
	// checking where the spans go, and the base does not say.
	slog.Info("observability: initialized",
		"service", cfg.ServiceName,
		"endpoint", endpoint,
	)
	return &Provider{cfg: cfg, tp: tp}, nil
}

// Shutdown flushes and stops all telemetry pipelines.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.tp == nil {
		return nil
	}
	return p.tp.Shutdown(ctx)
}
