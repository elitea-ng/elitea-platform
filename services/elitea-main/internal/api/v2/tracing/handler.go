// Package tracing serves the Go successor to the legacy tracing plugin's
// ingest surface (legacy/plugins/tracing/api/v2/{collect,otlp,status}.py):
//
//   - `status` reports whether this deployment exports OpenTelemetry traces —
//     the Go equivalent of models/admin.tracing.view + models.monitoring.
//     tracing.view.
//   - `otlp` reverse-proxies browser-originated OTLP/HTTP payloads to the
//     configured collector, avoiding a browser-to-collector CORS hop exactly
//     as the legacy original did against Jaeger (legacy otlp.py).
//   - `collect` accepts the UI's own span-batch shape (legacy collect.py) and
//     re-emits it as real spans through the process-wide TracerProvider —
//     the same one libs/go/observability wires up for elitea-main's own
//     instrumentation, so UI spans and server spans land in one collector.
//
// # Architecture decision (issue #250)
//
// Option (a) of the two the issue offers: elitea-main proxies collect/otlp to
// an OTEL collector rather than pointing clients straight at a collector
// service. This is a straight port of the legacy shape (both collect.py and
// otlp.py already proxy through the legacy main service), keeps the UI/worker
// client surface unchanged, and needs no collector-facing CORS or
// authentication configuration in deploy/* — the collector only ever talks to
// elitea-main.
//
// # Authorization
//
// collect/otlp use apimw.RequireProjectAccess, the same project-membership
// gate internal/api/v2/social uses for its own plugin surface — telemetry
// ingestion is not sensitive enough to warrant a dedicated permission grant,
// and adding one would need a new seeded migration (the admin-rbac-seeding
// pattern .../migrations/shared/0062_budgets_quota_statistics.sql follows)
// for a surface with no data-exposure risk. The `status/administration` read
// reuses the already-seeded "runtime.plugins" central permission — the same
// gate router.go applies to the other admin runtime/plugin status reads
// (system_info.py, plugin_config_*.py, maintenance.py, runtime_*.py) — rather
// than seeding the legacy "models.admin.tracing.view" permission fresh.
package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/elitea-ng/elitea-platform/libs/go/observability"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Config configures the tracing ingest surface.
type Config struct {
	// Enabled mirrors the observability SDK's own kill switch
	// (OTEL_SDK_DISABLED != "true"): with nothing behind it to export to,
	// ingest answers a discoverable "tracing is disabled" (503, matching
	// the legacy runtime's `if not self.module.enabled` branches) instead of silently
	// accepting and dropping every payload.
	Enabled bool
	// CollectorHTTPEndpoint is the OTLP/HTTP collector base URL `otlp`
	// forwards to, e.g. "http://otel-collector:4318". Read from
	// OTEL_EXPORTER_OTLP_ENDPOINT — the same env var libs/go/observability
	// reads for elitea-main's own span export, so both paths point at one
	// collector by construction.
	CollectorHTTPEndpoint string
	ServiceName           string
}

// ConfigFromEnv derives Enabled from observability.ConfigFromEnv (the exact
// function elitea-main's own span export uses) rather than re-reading
// OTEL_SDK_DISABLED independently, so the two can never disagree about
// whether tracing is on. CollectorHTTPEndpoint reads OTEL_EXPORTER_OTLP_ENDPOINT
// directly because it needs its own default ("http://otel-collector:4318")
// distinct from observability.Config.OTLPEndpoint's empty-means-defer-to-the-
// exporter's-own-env-handling default.
func ConfigFromEnv(serviceName string) Config {
	return Config{
		Enabled:               observability.ConfigFromEnv(serviceName, "").Enabled,
		CollectorHTTPEndpoint: envOr("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
		ServiceName:           serviceName,
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Handler serves collect/otlp/status. tracer is a func rather than a stored
// trace.Tracer so tests can swap in noop.Tracer{} without depending on the
// global TracerProvider observability.New installs at process startup.
type Handler struct {
	cfg    Config
	pool   *pgxpool.Pool
	tracer func() trace.Tracer
	client *http.Client
}

func NewHandler(pool *pgxpool.Pool, cfg Config, tracer func() trace.Tracer) *Handler {
	return &Handler{
		cfg:    cfg,
		pool:   pool,
		tracer: tracer,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// Routes mounts collect/otlp (project-access gated, legacy DEFAULT mode) and
// status (both prompt_lib and administration modes). requireAdminStatus is
// injected by router.go so this package does not need its own
// PermissionResolver wiring — see the "runtime.plugins" note in the package
// doc comment.
func (h *Handler) Routes(requireAdminStatus func(http.Handler) http.Handler) chi.Router {
	r := chi.NewRouter()

	// No-project-id variant: the legacy runtime's url_params included a bare '' alongside
	// '<int:project_id>' (PromptLibAPI.post(self, project_id: int = None,
	// **kwargs)) for callers that have not resolved a project yet (e.g. the
	// UI's earliest boot-time spans). Authenticated only — there is no
	// project to check membership against.
	r.Group(func(r chi.Router) {
		r.Use(requireAuthenticatedUser)
		r.Post("/collect/prompt_lib", h.Collect)
		r.Post("/otlp/prompt_lib", h.OTLP)
	})

	r.Group(func(r chi.Router) {
		r.Use(apimw.RequireProjectAccess(h.pool))
		r.Post("/collect/prompt_lib/{projectID}", h.Collect)
		r.Post("/otlp/prompt_lib/{projectID}", h.OTLP)
		r.Get("/status/prompt_lib/{projectID}", h.StatusProject)
	})

	r.Group(func(r chi.Router) {
		r.Use(requireAdminStatus)
		r.Get("/status/administration", h.StatusAdmin)
	})

	return r
}

func requireAuthenticatedUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.UserFromContext(r.Context()); !ok {
			apierr.WriteStatus(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- collect -----------------------------------------------------------

type collectRequest struct {
	Traces []traceBatch `json:"traces"`
}

type traceBatch struct {
	TraceID  string         `json:"trace_id"`
	Name     string         `json:"name"`
	Metadata map[string]any `json:"metadata"`
	Spans    []spanData     `json:"spans"`
}

type spanData struct {
	Name       string         `json:"name"`
	Metadata   map[string]any `json:"metadata"`
	DurationMs *float64       `json:"duration_ms"`
}

// MaxCollectBodyBytes bounds the collect request body the same way
// social/avatar.go and social/feedback.go bound theirs — without it, a single
// authenticated (this route needs no project membership, only
// authentication — see requireAuthenticatedUser) request could decode an
// unbounded JSON body and turn its size directly into a synchronous span
// creation loop.
const MaxCollectBodyBytes = 1 << 20 // 1 MiB

// MaxCollectTraces and MaxCollectSpansPerTrace cap the fan-out of a single
// decoded request, independent of raw body size (a small body can still
// encode a very large traces/spans array).
const (
	MaxCollectTraces        = 500
	MaxCollectSpansPerTrace = 1000
)

// Collect accepts a batch of UI/worker-reported spans and re-emits them as
// real OTel spans through the process TracerProvider — the Go equivalent of
// legacy collect.py's `tracer.start_as_current_span` loop.
func (h *Handler) Collect(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Tracing is disabled", "received": 0})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxCollectBodyBytes)

	var body collectRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "No data provided", "received": 0})
		return
	}

	if len(body.Traces) > MaxCollectTraces {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "too many traces in one request", "received": 0})
		return
	}
	for _, tb := range body.Traces {
		if len(tb.Spans) > MaxCollectSpansPerTrace {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "too many spans in one trace", "received": 0})
			return
		}
	}

	projectID := chi.URLParam(r, "projectID")
	user, _ := auth.UserFromContext(r.Context())
	ctx := r.Context()
	tracer := h.tracer()

	spansCreated := 0
	for _, tb := range body.Traces {
		// projectID comes ONLY from the URL — apimw.RequireProjectAccess has
		// verified the caller belongs to it. tb.Metadata["project_id"] is
		// client-supplied and unverified: trusting it here would let any
		// authenticated caller on the no-project-id route
		// (POST /tracing/collect/prompt_lib) attribute fabricated spans to a
		// project it has no membership in. An empty project.id attribute is
		// the honest answer when the caller resolved no verified project,
		// not a reason to fall back to an unverified claim.
		attrs := []attribute.KeyValue{
			attribute.String("trace.source", "ui"),
			attribute.String("trace.id", tb.TraceID),
			attribute.String("project.id", projectID),
			attribute.String("user.id", user.ID),
		}
		attrs = append(attrs, metadataAttrs(tb.Metadata)...)

		parentCtx, parentSpan := tracer.Start(ctx, "ui:"+tb.Name, trace.WithAttributes(attrs...))
		spansCreated++

		for _, sd := range tb.Spans {
			childAttrs := []attribute.KeyValue{attribute.String("span.source", "ui")}
			if sd.DurationMs != nil {
				childAttrs = append(childAttrs, attribute.Float64("duration_ms", *sd.DurationMs))
			}
			childAttrs = append(childAttrs, metadataAttrs(sd.Metadata)...)

			// parentCtx, not ctx: this is what makes the child span a CHILD
			// of parentSpan in the exported trace, rather than a sibling
			// parented to whatever span was already active on the request
			// (e.g. apimw.OtelMiddleware's own per-request span).
			_, childSpan := tracer.Start(parentCtx, sd.Name, trace.WithAttributes(childAttrs...))
			spansCreated++
			if errMsg, ok := sd.Metadata["error"]; ok && errMsg != nil && errMsg != "" {
				childSpan.SetStatus(codes.Error, fmt.Sprintf("%v", errMsg))
			} else {
				childSpan.SetStatus(codes.Ok, "")
			}
			childSpan.End()
		}
		parentSpan.End()
	}

	writeJSON(w, http.StatusOK, map[string]any{"received": len(body.Traces), "spans_created": spansCreated})
}

func metadataAttrs(metadata map[string]any) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(metadata))
	for k, v := range metadata {
		if v == nil {
			continue
		}
		attrs = append(attrs, attribute.String("ui."+k, fmt.Sprintf("%v", v)))
	}
	return attrs
}

// --- otlp --------------------------------------------------------------

// OTLP reverse-proxies the raw OTLP/HTTP payload to the configured collector,
// exactly as legacy otlp.py proxied to Jaeger — same error-status mapping
// (connection refused -> 503, timeout -> 504, anything else -> 500).
func (h *Handler) OTLP(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Tracing is disabled"})
		return
	}

	target := h.cfg.CollectorHTTPEndpoint + "/v1/traces"
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, r.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Internal error"})
		return
	}
	proxyReq.Header.Set("Content-Type", contentType)

	resp, err := h.client.Do(proxyReq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": "OTLP endpoint timeout"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "OTLP endpoint unavailable"})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	respContentType := resp.Header.Get("Content-Type")
	if respContentType == "" {
		respContentType = "application/json"
	}
	w.Header().Set("Content-Type", respContentType)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func isTimeout(err error) bool {
	type timeoutError interface{ Timeout() bool }
	var te timeoutError
	return errors.As(err, &te) && te.Timeout()
}

// --- status --------------------------------------------------------------

// StatusProject serves GET /tracing/status/prompt_lib/{projectID} — the
// project-scoped read legacy status.py's PromptLibAPI served
// (models.monitoring.tracing.view).
func (h *Handler) StatusProject(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":    h.cfg.Enabled,
		"project_id": chi.URLParam(r, "projectID"),
	})
}

// StatusAdmin serves GET /tracing/status/administration — legacy status.py's
// AdminAPI (models.admin.tracing.view).
func (h *Handler) StatusAdmin(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": h.cfg.Enabled,
		"config": map[string]any{
			"collector_endpoint": h.cfg.CollectorHTTPEndpoint,
			"service_name":       h.cfg.ServiceName,
		},
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
