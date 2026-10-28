package observability

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// logsPath is the OTLP/HTTP path of the logs signal.
const logsPath = "/v1/logs"

// LogsProtocol selects the OTLP transport of a LogsExporter.
type LogsProtocol string

const (
	// LogsProtocolHTTP is OTLP/HTTP with protobuf bodies (port 4318).
	LogsProtocolHTTP LogsProtocol = "http"
	// LogsProtocolGRPC is OTLP/gRPC (port 4317).
	LogsProtocolGRPC LogsProtocol = "grpc"
)

// LogsConfig configures a LogsExporter.
type LogsConfig struct {
	ServiceName    string
	ServiceVersion string
	// Endpoint is the collector's URL. For HTTP it is the BASE URL and the
	// logs path "/v1/logs" is appended, like the traces exporter; for gRPC it
	// is "host:port" or a URL whose scheme picks TLS (https) or plaintext
	// (http).
	Endpoint string
	Protocol LogsProtocol
	// Headers are sent with every request (an API key for a hosted SIEM).
	Headers map[string]string
}

// LogEntry is one log record to export. The audit exporter sends the whole
// evidence envelope as Body and the fields a SIEM filters on as Attributes.
type LogEntry struct {
	// EventName is the OpenTelemetry event name ("elitea.audit.event").
	EventName string
	Time      time.Time
	Severity  otellog.Severity
	// Body is the log body, a JSON document for audit evidence.
	Body       string
	Attributes map[string]string
}

// LogsExporter sends log records to an OTLP collector and tells the caller
// whether the collector accepted them.
//
// Unlike the SDK's batching processor, Export is synchronous and returns the
// transport's error, because a caller that must not lose a record (the audit
// relay) acknowledges its own source only after the collector answered. It is
// safe for concurrent use; calls are serialised.
type LogsExporter struct {
	mu       sync.Mutex
	exporter sdklog.Exporter
	capture  *captureProcessor
	provider *sdklog.LoggerProvider
	logger   otellog.Logger
}

// NewLogsExporter builds the exporter. It dials lazily: an unreachable
// collector surfaces as an Export error, not here.
func NewLogsExporter(ctx context.Context, cfg LogsConfig) (*LogsExporter, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("observability: OTLP logs endpoint is required")
	}
	var (
		exporter sdklog.Exporter
		err      error
	)
	switch cfg.Protocol {
	case LogsProtocolHTTP, "":
		exporter, err = newLogsHTTP(ctx, cfg)
	case LogsProtocolGRPC:
		exporter, err = newLogsGRPC(ctx, cfg)
	default:
		return nil, fmt.Errorf("observability: unknown OTLP logs protocol %q (want http or grpc)", cfg.Protocol)
	}
	if err != nil {
		return nil, fmt.Errorf("observability: create OTLP log exporter: %w", err)
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	))
	if err != nil {
		return nil, fmt.Errorf("observability: build resource attributes: %w", err)
	}
	capture := &captureProcessor{}
	provider := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(capture))
	return &LogsExporter{
		exporter: exporter,
		capture:  capture,
		provider: provider,
		logger:   provider.Logger("elitea.audit"),
	}, nil
}

func newLogsHTTP(ctx context.Context, cfg LogsConfig) (sdklog.Exporter, error) {
	endpoint, err := logsHTTPEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	opts := []otlploghttp.Option{otlploghttp.WithEndpointURL(endpoint)}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlploghttp.WithHeaders(cfg.Headers))
	}
	// The relay owns retry and backoff; the SDK's own retry would hold the
	// relay's transaction for minutes.
	opts = append(opts, otlploghttp.WithRetry(otlploghttp.RetryConfig{Enabled: false}))
	return otlploghttp.New(ctx, opts...)
}

func newLogsGRPC(ctx context.Context, cfg LogsConfig) (sdklog.Exporter, error) {
	opts := []otlploggrpc.Option{}
	if strings.Contains(cfg.Endpoint, "://") {
		opts = append(opts, otlploggrpc.WithEndpointURL(cfg.Endpoint))
	} else {
		opts = append(opts, otlploggrpc.WithEndpoint(cfg.Endpoint))
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlploggrpc.WithHeaders(cfg.Headers))
	}
	opts = append(opts, otlploggrpc.WithRetry(otlploggrpc.RetryConfig{Enabled: false}))
	return otlploggrpc.New(ctx, opts...)
}

// logsHTTPEndpoint appends the logs path to a base URL, under any prefix the
// base carries (the same rule as the traces endpoint).
func logsHTTPEndpoint(base string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("OTLP logs endpoint %q is not an http(s) URL", base)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + logsPath
	return parsed.String(), nil
}

// Export sends the entries as one OTLP request and returns nil only when the
// collector accepted it.
func (e *LogsExporter) Export(ctx context.Context, entries []LogEntry) error {
	if e == nil {
		return errors.New("observability: nil logs exporter")
	}
	if len(entries) == 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.capture.reset()
	for _, entry := range entries {
		var rec otellog.Record
		rec.SetEventName(entry.EventName)
		rec.SetTimestamp(entry.Time)
		rec.SetObservedTimestamp(time.Now())
		rec.SetSeverity(entry.Severity)
		rec.SetBody(attribute.StringValue(entry.Body))
		for key, value := range entry.Attributes {
			rec.AddAttributes(attribute.String(key, value))
		}
		e.logger.Emit(ctx, rec)
	}
	records := e.capture.take()
	if len(records) != len(entries) {
		return fmt.Errorf("observability: %d of %d log records were dropped before export", len(entries)-len(records), len(entries))
	}
	return e.exporter.Export(ctx, records)
}

// Shutdown stops the exporter.
func (e *LogsExporter) Shutdown(ctx context.Context) error {
	if e == nil {
		return nil
	}
	return errors.Join(e.provider.Shutdown(ctx), e.exporter.Shutdown(ctx))
}

// captureProcessor collects the records a LoggerProvider hands it, so the
// exporter can send them synchronously with the resource and scope the
// provider attached.
type captureProcessor struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (p *captureProcessor) reset() {
	p.mu.Lock()
	p.records = nil
	p.mu.Unlock()
}

func (p *captureProcessor) take() []sdklog.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.records
	p.records = nil
	return out
}

func (p *captureProcessor) OnEmit(_ context.Context, record *sdklog.Record) error {
	p.mu.Lock()
	p.records = append(p.records, record.Clone())
	p.mu.Unlock()
	return nil
}

func (*captureProcessor) Shutdown(context.Context) error   { return nil }
func (*captureProcessor) ForceFlush(context.Context) error { return nil }

var _ sdklog.Processor = (*captureProcessor)(nil)

func (*captureProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
