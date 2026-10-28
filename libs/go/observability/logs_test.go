package observability

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

// collector is an in-process OTLP/HTTP logs receiver.
type collector struct {
	mu       sync.Mutex
	requests []*collogs.ExportLogsServiceRequest
	paths    []string
	status   int
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status != 0 {
		w.WriteHeader(c.status)
		return
	}
	req := &collogs.ExportLogsServiceRequest{}
	if err := proto.Unmarshal(body, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	c.requests = append(c.requests, req)
	c.paths = append(c.paths, r.URL.Path)
	w.Header().Set("Content-Type", "application/x-protobuf")
	out, _ := proto.Marshal(&collogs.ExportLogsServiceResponse{})
	_, _ = w.Write(out)
}

func TestLogsExporterSendsEnvelopeAsBodyAndAttributes(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c)
	defer srv.Close()

	exp, err := NewLogsExporter(context.Background(), LogsConfig{
		ServiceName: "elitea-main", ServiceVersion: "test", Endpoint: srv.URL + "/otlp/",
		Headers: map[string]string{"x-api-key": "k"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exp.Shutdown(context.Background()) }()

	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	err = exp.Export(context.Background(), []LogEntry{
		{EventName: "elitea.audit.event", Time: at, Severity: otellog.SeverityInfo, Body: `{"schema":"elitea.audit.v1"}`,
			Attributes: map[string]string{"elitea.audit.event_id": "e1"}},
		{EventName: "elitea.audit.event", Time: at, Severity: otellog.SeverityWarn, Body: `{"n":2}`},
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) != 1 {
		t.Fatalf("collector saw %d requests, want 1 (one batch, one request)", len(c.requests))
	}
	if c.paths[0] != "/otlp/v1/logs" {
		t.Fatalf("path = %q, want the logs path appended under the base prefix", c.paths[0])
	}
	rl := c.requests[0].GetResourceLogs()
	if len(rl) != 1 {
		t.Fatalf("resource logs = %d", len(rl))
	}
	var service string
	for _, kv := range rl[0].GetResource().GetAttributes() {
		if kv.GetKey() == "service.name" {
			service = kv.GetValue().GetStringValue()
		}
	}
	if service != "elitea-main" {
		t.Fatalf("service.name = %q", service)
	}
	records := rl[0].GetScopeLogs()[0].GetLogRecords()
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if got := records[0].GetBody().GetStringValue(); got != `{"schema":"elitea.audit.v1"}` {
		t.Fatalf("body = %q", got)
	}
	if records[0].GetEventName() != "elitea.audit.event" {
		t.Fatalf("event name = %q", records[0].GetEventName())
	}
	if records[0].GetTimeUnixNano() != uint64(at.UnixNano()) {
		t.Fatalf("time = %d", records[0].GetTimeUnixNano())
	}
	if len(records[0].GetAttributes()) != 1 || records[0].GetAttributes()[0].GetKey() != "elitea.audit.event_id" {
		t.Fatalf("attributes = %v", records[0].GetAttributes())
	}
}

func TestLogsExporterReportsCollectorRejection(t *testing.T) {
	c := &collector{status: http.StatusServiceUnavailable}
	srv := httptest.NewServer(c)
	defer srv.Close()
	exp, err := NewLogsExporter(context.Background(), LogsConfig{ServiceName: "x", Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exp.Shutdown(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exp.Export(ctx, []LogEntry{{EventName: "e", Time: time.Now(), Body: "{}"}}); err == nil {
		t.Fatal("a collector 503 must surface as an error, so the caller does not acknowledge the batch")
	}
}

func TestLogsExporterRejectsBadConfig(t *testing.T) {
	if _, err := NewLogsExporter(context.Background(), LogsConfig{}); err == nil {
		t.Fatal("empty endpoint accepted")
	}
	if _, err := NewLogsExporter(context.Background(), LogsConfig{Endpoint: "collector:4318"}); err == nil {
		t.Fatal("non-URL http endpoint accepted")
	}
	if _, err := NewLogsExporter(context.Background(), LogsConfig{Endpoint: "http://c", Protocol: "udp"}); err == nil {
		t.Fatal("unknown protocol accepted")
	}
}
