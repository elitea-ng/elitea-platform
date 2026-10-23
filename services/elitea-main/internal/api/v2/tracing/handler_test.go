package tracing_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/tracing"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

func allowAll(next http.Handler) http.Handler { return next }

func withUser(r *http.Request, id string) *http.Request {
	return r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: id, Email: "u@example.com"}))
}

func newRecordingTracer(t *testing.T) (func() trace.Tracer, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	return func() trace.Tracer { return tp.Tracer("test") }, exporter
}

func newHandler(t *testing.T, enabled bool) (*tracing.Handler, *tracetest.InMemoryExporter) {
	t.Helper()
	tracerFn, exporter := newRecordingTracer(t)
	h := tracing.NewHandler(nil, tracing.Config{
		Enabled:               enabled,
		CollectorHTTPEndpoint: "http://collector.invalid:4318",
		ServiceName:           "elitea-main",
	}, tracerFn)
	return h, exporter
}

func TestRoutes_AllPathsRegistered(t *testing.T) {
	h, _ := newHandler(t, true)
	r := h.Routes(allowAll)

	paths := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/collect/prompt_lib"},
		{http.MethodPost, "/collect/prompt_lib/1"},
		{http.MethodPost, "/otlp/prompt_lib"},
		{http.MethodPost, "/otlp/prompt_lib/1"},
		{http.MethodGet, "/status/prompt_lib/1"},
		{http.MethodGet, "/status/administration"},
	}
	for _, tc := range paths {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code == http.StatusNotFound {
			t.Errorf("route %s %s not registered (404)", tc.method, tc.path)
		}
	}
}

// TestCollect_CreatesRealSpans proves Collect does not just return 200 — it
// asserts on the actual spans recorded by the TracerProvider, discriminating
// between "handler wired" and "handler forwards to the tracer".
func TestCollect_CreatesRealSpans(t *testing.T) {
	h, exporter := newHandler(t, true)
	r := h.Routes(allowAll)

	body := `{"traces":[{"trace_id":"t1","name":"page_load","metadata":{"route":"/home"},"spans":[{"name":"render","duration_ms":12.5,"metadata":{}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/collect/prompt_lib", strings.NewReader(body))
	req = withUser(req, "42")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["spans_created"] != float64(2) {
		t.Fatalf("expected spans_created=2 (1 parent + 1 child), got %v", resp["spans_created"])
	}

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected 2 exported spans, got %d", len(spans))
	}
	names := map[string]bool{}
	for _, s := range spans {
		names[s.Name] = true
	}
	if !names["ui:page_load"] || !names["render"] {
		t.Fatalf("unexpected span names: %v", names)
	}

	// Proves the child span is actually parented to the trace's parent span
	// in the exported trace — not merely that both were created. A handler
	// that starts every span off the same background context (discarding the
	// context Start() returns) would still pass the count/name assertions
	// above while producing two unrelated root-ish spans.
	var parent, child tracetest.SpanStub
	for _, s := range spans {
		switch s.Name {
		case "ui:page_load":
			parent = s
		case "render":
			child = s
		}
	}
	if child.Parent.SpanID() != parent.SpanContext.SpanID() {
		t.Fatalf("expected child span %q to be parented to %q (span id %s), got parent span id %s",
			child.Name, parent.Name, parent.SpanContext.SpanID(), child.Parent.SpanID())
	}
	if child.SpanContext.TraceID() != parent.SpanContext.TraceID() {
		t.Fatalf("expected child span to share the parent's trace id")
	}
}

// TestCollect_NoProjectRoute_IgnoresClientSuppliedProjectID proves the
// no-project-id route does NOT trust a client-supplied metadata.project_id
// for span attribution — apimw.RequireProjectAccess only runs on the
// {projectID} URL variant, so honoring a body-supplied project_id here would
// let any authenticated caller (regardless of project membership) attribute
// fabricated spans to a project it has no access to.
func TestCollect_NoProjectRoute_IgnoresClientSuppliedProjectID(t *testing.T) {
	h, exporter := newHandler(t, true)
	r := h.Routes(allowAll)

	body := `{"traces":[{"trace_id":"t1","name":"spoofed","metadata":{"project_id":"999-not-my-project"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/collect/prompt_lib", strings.NewReader(body))
	req = withUser(req, "1")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 exported span, got %d", len(spans))
	}
	for _, attr := range spans[0].Attributes {
		if attr.Key == "project.id" && attr.Value.AsString() != "" {
			t.Fatalf("expected empty project.id (unverified), got %q from client-supplied metadata", attr.Value.AsString())
		}
	}
}

// TestCollect_TooManyTracesRejected proves the request is capped before the
// span-creation loop runs — not just that a huge body eventually succeeds.
func TestCollect_TooManyTracesRejected(t *testing.T) {
	h, exporter := newHandler(t, true)
	r := h.Routes(allowAll)

	var b strings.Builder
	b.WriteString(`{"traces":[`)
	for i := 0; i <= tracing.MaxCollectTraces; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"x"}`)
	}
	b.WriteString(`]}`)

	req := httptest.NewRequest(http.MethodPost, "/collect/prompt_lib", strings.NewReader(b.String()))
	req = withUser(req, "1")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a request over MaxCollectTraces, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(exporter.GetSpans()) != 0 {
		t.Fatalf("expected no spans created for a rejected request, got %d", len(exporter.GetSpans()))
	}
}

// TestCollect_OversizedBodyRejected proves the MaxBytesReader cap is wired:
// a body larger than MaxCollectBodyBytes must fail to decode rather than be
// accepted and processed.
func TestCollect_OversizedBodyRejected(t *testing.T) {
	h, exporter := newHandler(t, true)
	r := h.Routes(allowAll)

	// One trace whose metadata value alone exceeds the cap; valid JSON, so a
	// missing MaxBytesReader would let it decode successfully.
	oversized := strings.Repeat("a", tracing.MaxCollectBodyBytes+1)
	body := `{"traces":[{"name":"x","metadata":{"pad":"` + oversized + `"}}]}`

	req := httptest.NewRequest(http.MethodPost, "/collect/prompt_lib", strings.NewReader(body))
	req = withUser(req, "1")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a body over MaxCollectBodyBytes, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(exporter.GetSpans()) != 0 {
		t.Fatalf("expected no spans created for a rejected request, got %d", len(exporter.GetSpans()))
	}
}

func TestCollect_DisabledReturns503AndCreatesNoSpans(t *testing.T) {
	h, exporter := newHandler(t, false)
	r := h.Routes(allowAll)

	req := httptest.NewRequest(http.MethodPost, "/collect/prompt_lib", strings.NewReader(`{"traces":[{"name":"x"}]}`))
	req = withUser(req, "1")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
	if len(exporter.GetSpans()) != 0 {
		t.Fatalf("expected no spans exported while disabled, got %d", len(exporter.GetSpans()))
	}
}

func TestCollect_UnauthenticatedNoProjectRejected(t *testing.T) {
	h, _ := newHandler(t, true)
	r := h.Routes(allowAll)

	req := httptest.NewRequest(http.MethodPost, "/collect/prompt_lib", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated caller, got %d", rr.Code)
	}
}

// TestCollect_WithProjectID_RequiresAuthentication proves the project-scoped
// route runs apimw.RequireProjectAccess (which checks authentication before
// touching the pool) rather than being open — a bare 404-vs-not check would
// not catch a route that was mounted without the gate.
//
// NOTE: RequireProjectAccess's own no-pool behavior is not exercised here — a
// nil *pgxpool.Pool passed through its *pgxpool.Pool parameter becomes a
// non-nil interface value (Go interface-from-nil-concrete-pointer), so its
// internal `pool == nil` check never trips and it panics on Acquire instead
// of failing closed. That is a pre-existing defect in
// internal/api/middleware/project_authorization.go, not introduced by this
// package, and out of scope for issue #250 — flagged separately.
func TestCollect_WithProjectID_RequiresAuthentication(t *testing.T) {
	h, _ := newHandler(t, true)
	r := h.Routes(allowAll)

	req := httptest.NewRequest(http.MethodPost, "/collect/prompt_lib/1", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated caller on project-scoped route, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestOTLP_ForwardsToCollector proves the otlp route actually reverse-proxies
// the request body/content-type to the configured collector endpoint and
// relays its response — not just a 200 stub.
func TestOTLP_ForwardsToCollector(t *testing.T) {
	var gotPath, gotContentType, gotBody string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		buf, _ := io.ReadAll(r.Body)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("ok"))
	}))
	defer collector.Close()

	tracerFn, _ := newRecordingTracer(t)
	h := tracing.NewHandler(nil, tracing.Config{
		Enabled:               true,
		CollectorHTTPEndpoint: collector.URL,
		ServiceName:           "elitea-main",
	}, tracerFn)
	r := h.Routes(allowAll)

	req := httptest.NewRequest(http.MethodPost, "/otlp/prompt_lib", strings.NewReader(`{"resourceSpans":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req = withUser(req, "1")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if gotPath != "/v1/traces" {
		t.Fatalf("expected proxied path /v1/traces, got %q", gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("expected forwarded content-type application/json, got %q", gotContentType)
	}
	if gotBody != `{"resourceSpans":[]}` {
		t.Fatalf("expected forwarded body, got %q", gotBody)
	}
	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected relayed 202, got %d", rr.Code)
	}
	if rr.Body.String() != "ok" {
		t.Fatalf("expected relayed body 'ok', got %q", rr.Body.String())
	}
}

func TestOTLP_CollectorUnreachableReturns503(t *testing.T) {
	tracerFn, _ := newRecordingTracer(t)
	h := tracing.NewHandler(nil, tracing.Config{
		Enabled:               true,
		CollectorHTTPEndpoint: "http://127.0.0.1:1", // nothing listens here
		ServiceName:           "elitea-main",
	}, tracerFn)
	r := h.Routes(allowAll)

	req := httptest.NewRequest(http.MethodPost, "/otlp/prompt_lib", strings.NewReader(`{}`))
	req = withUser(req, "1")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when collector unreachable, got %d", rr.Code)
	}
}

func TestStatusAdmin_ReflectsConfig(t *testing.T) {
	h, _ := newHandler(t, true)
	r := h.Routes(allowAll)

	req := httptest.NewRequest(http.MethodGet, "/status/administration", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["enabled"] != true {
		t.Fatalf("expected enabled=true, got %v", resp["enabled"])
	}
	cfg, ok := resp["config"].(map[string]any)
	if !ok {
		t.Fatalf("expected config object, got %T", resp["config"])
	}
	if cfg["collector_endpoint"] != "http://collector.invalid:4318" {
		t.Fatalf("expected configured collector endpoint echoed back, got %v", cfg["collector_endpoint"])
	}
}

func TestStatusAdmin_GatedByInjectedMiddleware(t *testing.T) {
	h, _ := newHandler(t, true)
	deny := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		})
	}
	r := h.Routes(deny)

	req := httptest.NewRequest(http.MethodGet, "/status/administration", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected the injected admin middleware to run (403), got %d", rr.Code)
	}
}
