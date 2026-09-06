package observability

// Where the spans actually go.
//
// The defect: this package passed the GENERIC endpoint
// (OTEL_EXPORTER_OTLP_ENDPOINT, "http://otel-collector:4318") to
// `otlptracehttp.WithEndpointURL`, which takes the SIGNAL-SPECIFIC form and
// uses it verbatim — pinning a path-less URL to "/" so the default signal path
// is not appended. Every batch went to the collector's root, the collector
// answered 404, and elitea-main logged
//
//	traces export: failed to send to http://otel-collector:4318/: 404 Not Found
//
// every few seconds for the life of the process, having exported nothing.
//
// The first test asserts the derivation. The second one is the one that would
// have caught it: it stands a real HTTP server up as the collector and reads
// the path the exporter posts to.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

func TestTracesEndpointAppendsTheSignalPathToTheGenericEndpoint(t *testing.T) {
	for name, testCase := range map[string]struct {
		config Config
		want   string
	}{
		"the compose stack's collector": {
			config: Config{OTLPEndpoint: "http://otel-collector:4318"},
			want:   "http://otel-collector:4318/v1/traces",
		},
		"a trailing slash": {
			config: Config{OTLPEndpoint: "http://otel-collector:4318/"},
			want:   "http://otel-collector:4318/v1/traces",
		},
		"a path prefix keeps the signal path under it": {
			config: Config{OTLPEndpoint: "https://gateway.example.com/otlp"},
			want:   "https://gateway.example.com/otlp/v1/traces",
		},
		"the signal-specific endpoint is verbatim": {
			config: Config{OTLPTracesEndpoint: "https://collector.example.com/ingest/traces"},
			want:   "https://collector.example.com/ingest/traces",
		},
		"the signal-specific endpoint wins": {
			config: Config{
				OTLPEndpoint:       "http://otel-collector:4318",
				OTLPTracesEndpoint: "https://collector.example.com/ingest/traces",
			},
			want: "https://collector.example.com/ingest/traces",
		},
		"neither defers to the exporter's own environment handling": {
			config: Config{},
			want:   "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := testCase.config.tracesEndpoint(); got != testCase.want {
				t.Fatalf("tracesEndpoint() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestExporterPostsToTheTracesPath(t *testing.T) {
	// The end-to-end check. Nothing about the derivation is asserted here: the
	// server records what the OTLP exporter actually requested.
	paths := make(chan string, 4)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case paths <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	ctx := context.Background()
	provider, err := New(ctx, Config{
		ServiceName:    "observability-test",
		ServiceVersion: "0.0.0-test",
		OTLPEndpoint:   collector.URL,
		Enabled:        true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = provider.Shutdown(shutdownCtx)
	})

	_, span := otel.Tracer("observability-test").Start(ctx, "one-span")
	span.End()

	// Shutdown flushes the batcher, so the export happens before the read.
	flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := provider.Shutdown(flushCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case path := <-paths:
		if path != tracesPath {
			t.Fatalf("the exporter posted to %q, want %q", path, tracesPath)
		}
	default:
		t.Fatal("the collector received no export")
	}
}
