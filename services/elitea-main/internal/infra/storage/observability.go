package storage

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// S18: instruments follow the house pattern in
// internal/api/middleware/otel.go — package-level instruments created in
// init() against the global default MeterProvider (otel.Meter("elitea-main")),
// not an injected one. Nothing in this codebase constructs an explicit
// MeterProvider; the global default is what every existing instrument
// already registers against, so a storage-specific one would silently
// export to a different (no-op, absent an explicit otel.SetMeterProvider
// call) provider than the rest of the service.
var (
	operationDuration metric.Float64Histogram
	operationErrors   metric.Int64Counter
	bytesInCounter    metric.Int64Counter
	bytesOutCounter   metric.Int64Counter
	projectBytesUsed  metric.Int64Gauge
)

func init() {
	meter := otel.Meter("elitea-main")

	operationDuration, _ = meter.Float64Histogram(
		"artifact.storage.operation.duration",
		metric.WithUnit("s"),
		metric.WithDescription("ObjectStore operation duration in seconds, labelled by backend and operation only"),
	)
	operationErrors, _ = meter.Int64Counter(
		"artifact.storage.operation.errors",
		metric.WithDescription("ObjectStore operation errors, labelled by backend, operation, and sentinel error_type"),
	)
	bytesInCounter, _ = meter.Int64Counter(
		"artifact.storage.bytes_in",
		metric.WithUnit("By"),
		metric.WithDescription("Bytes written to the object store (Put and multipart completion)"),
	)
	bytesOutCounter, _ = meter.Int64Counter(
		"artifact.storage.bytes_out",
		metric.WithUnit("By"),
		metric.WithDescription("Bytes read from the object store (Get)"),
	)
	// The one storage metric deliberately labelled by project identifier —
	// see RecordProjectByteUsage's own doc comment for why this does not
	// violate the "never by project identifier" rule the other instruments
	// above follow.
	projectBytesUsed, _ = meter.Int64Gauge(
		"artifact.storage.project_bytes_used",
		metric.WithUnit("By"),
		metric.WithDescription("Per-project total artifact bytes, updated on the S14 retention sweeper tick — never per request"),
	)
}

// errorTypeLabel maps a storage sentinel error onto a short, bounded-
// cardinality label for the operation-errors counter. Never the raw error
// string: that could embed a bucket/key and explode cardinality exactly the
// way a project_id or key label would.
func errorTypeLabel(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrAlreadyExists):
		return "already_exists"
	case errors.Is(err, ErrAccessDenied):
		return "access_denied"
	case errors.Is(err, ErrInvalidKey):
		return "invalid_key"
	case errors.Is(err, ErrTooLarge):
		return "too_large"
	case errors.Is(err, ErrPreconditionFailed):
		return "precondition_failed"
	case errors.Is(err, ErrNotSupported):
		return "not_supported"
	default:
		return "internal"
	}
}

// metricAttrs builds the shared backend+operation attribute set every
// instrumented metric uses — the bytes-in/bytes-out counters reuse this
// alongside recordOperation's own copy so both always agree on the exact
// same label set for the same call.
func metricAttrs(backend, operation string) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("backend", backend),
		attribute.String("operation", operation),
	)
}

// recordOperation is the one place duration+error attribution happens for
// every instrumented ObjectStore method — called with the operation's own
// start time and outcome, never with a project_id or key attribute (S18:
// "all labelled by backend and operation but never by project identifier or
// key, which would explode cardinality").
func recordOperation(ctx context.Context, backend, operation string, start time.Time, err error) {
	attrs := metric.WithAttributes(
		attribute.String("backend", backend),
		attribute.String("operation", operation),
	)
	operationDuration.Record(ctx, time.Since(start).Seconds(), attrs)
	if err != nil {
		operationErrors.Add(ctx, 1, metric.WithAttributes(
			attribute.String("backend", backend),
			attribute.String("operation", operation),
			attribute.String("error_type", errorTypeLabel(err)),
		))
	}
}

// RecordProjectByteUsage updates the per-project byte-usage gauge. Call
// exactly once per project per S14 sweeper tick — never from a per-request
// path. This is the one storage metric intentionally labelled by project
// identifier: unlike the per-operation instruments above (whose cardinality
// is unbounded — every request, every key), this gauge has exactly one
// sample per known project, refreshed on a bounded schedule (the sweeper's
// own tick interval), which is the cardinality profile a gauge is for.
func RecordProjectByteUsage(ctx context.Context, projectID int64, totalBytes int64) {
	projectBytesUsed.Record(ctx, totalBytes, metric.WithAttributes(
		attribute.Int64("project_id", projectID),
	))
}

// LogAudit emits the structured audit line S18 requires for every delete and
// every grant issuance: operation, bucket, key, project_id, principal,
// outcome. This is deliberately a structured slog line, not a write to a
// real audit-record store — there is no audit-record persistence mechanism
// anywhere in this codebase to hook (verified exhaustively; see the plan's
// S18 section), and inventing a bespoke one for this stage alone was
// explicitly out of scope. Whether artifact deletes and grants belong in a
// real, queryable audit trail — and if so, what shape — is left as an open
// question for an owner, the same treatment S13 gives its own missing
// integration point.
//
// principal is resolved from ctx via auth.RuntimePrincipalFromContext —
// "system" when ctx carries no authenticated principal (e.g. the S14
// sweeper's own background-triggered deletes, which never pass through the
// HTTP auth middleware at all), never a hard failure: an audit line with an
// honestly-unknown principal is far more useful than silently skipping it.
func LogAudit(ctx context.Context, operation, bucket, key, projectID, outcome string) {
	slog.InfoContext(ctx, "artifact audit",
		"operation", operation,
		"bucket", bucket,
		"key", key,
		"project_id", projectID,
		"principal", principalFromContext(ctx),
		"outcome", outcome,
	)
}

func principalFromContext(ctx context.Context) string {
	user, ok := auth.RuntimePrincipalFromContext(ctx)
	if !ok {
		return "system"
	}
	if user.Email != "" {
		return user.Email
	}
	if user.ID != "" {
		return user.ID
	}
	return "system"
}
