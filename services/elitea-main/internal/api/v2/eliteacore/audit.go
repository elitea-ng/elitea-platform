package eliteacore

// The admin audit-trail read surface (unit A14, issue #200): four GETs, no
// writes at all.
//
//	GET /elitea_core/audit/{mode}                — individual spans, paginated
//	GET /elitea_core/audit_traces/{mode}         — spans grouped by trace_id
//	GET /elitea_core/audit_heatmap/{mode}        — span counts, time × duration
//	GET /elitea_core/audit_trace_heatmap/{mode}  — trace counts, time × duration
//
// ## What was here before
//
// Two of the four had a route but no implementation: `AuditTraces` answered
// `{"items":[],"total":0}` and `AuditTraceHeatmap` answered `{"data":[]}`, both
// with the request parameter discarded (`_ *http.Request`) and neither touching
// the database. The other two had no route at all. So the admin page's four
// queries produced, between them, two 404s and two empty lists — and because
// the client reads `rows` while the stub emitted `items`, even the stub's shape
// would not have rendered. Per the decision on #200 these are implemented for
// real rather than left as dead reads.
//
// ## Sensitivity
//
// Audit rows carry user emails, project ids and the actions people took. Every
// route here is gated in internal/api/router.go on the same permission its
// pylon counterpart declares (`models.admin.audit_trail.view`), resolved from
// the database per request. No record content is ever written to a log line.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/jackc/pgx/v5"
)

const (
	auditDefaultLimit = 50
	auditMaxLimit     = 200
)

func auditPagination(r *http.Request) (limit, offset int) {
	limit, offset = auditDefaultLimit, 0
	if parsed, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && parsed > 0 {
		limit = min(parsed, auditMaxLimit)
	}
	if parsed, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && parsed > 0 {
		offset = parsed
	}
	return limit, offset
}

// auditReadFailed reports a failed read as a failure.
//
// It deliberately does NOT fall back to an empty page: "this deployment has no
// audit history" and "the query blew up" render identically, and choosing the
// reassuring one is how the admin Users listing used to hide its own errors.
// The error itself is not echoed to the client — it can quote row values.
func auditReadFailed(w http.ResponseWriter, what string) {
	apierr.WriteStatus(w, http.StatusInternalServerError, fmt.Sprintf("failed to query %s", what))
}

/* ── spans ─────────────────────────────────────────────────────────────── */

// auditSpan is one `centry.audit_events` row, with exactly the fields audit.py
// serialises. Every column here exists in the table — verified against
// legacy/plugins/tracing/models/audit_event.py, which owned the write path in
// the legacy runtime, and against a live database.
//
// The write path in THIS service is internal/audit + internal/api/middleware/
// audit.go. Until that landed nothing in the Go product wrote this table, so
// every route in this file read an empty table in every real deployment and the
// empty state was indistinguishable from "nothing happened".
type auditSpan struct {
	ID           int64      `json:"id"`
	Timestamp    *time.Time `json:"timestamp"`
	UserID       *int64     `json:"user_id"`
	UserEmail    *string    `json:"user_email"`
	ProjectID    *int64     `json:"project_id"`
	EventType    string     `json:"event_type"`
	Action       string     `json:"action"`
	HTTPMethod   *string    `json:"http_method"`
	HTTPRoute    *string    `json:"http_route"`
	StatusCode   *int32     `json:"status_code"`
	DurationMS   *float64   `json:"duration_ms"`
	IsError      bool       `json:"is_error"`
	EntityName   *string    `json:"entity_name"`
	ToolName     *string    `json:"tool_name"`
	ModelName    *string    `json:"model_name"`
	TraceID      *string    `json:"trace_id"`
	SpanID       *string    `json:"span_id"`
	ParentSpanID *string    `json:"parent_span_id"`
}

// AuditTrail serves `GET /elitea_core/audit/{mode}` — the flat span listing.
func (h *Handler) AuditTrail(w http.ResponseWriter, r *http.Request) {
	limit, offset := auditPagination(r)
	filters, ok := parseAuditRequest(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()

	rows, total, err := h.listAuditSpans(r.Context(), filters, spanListing{
		limit:     limit,
		offset:    offset,
		sortBy:    query.Get("sort_by"),
		sortOrder: query.Get("sort_order"),
	})
	if err != nil {
		auditReadFailed(w, "audit trail")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "total": total})
}

type spanListing struct {
	limit     int
	offset    int
	sortBy    string
	sortOrder string
}

func (h *Handler) listAuditSpans(
	ctx context.Context, filters auditFilters, listing spanListing,
) ([]auditSpan, int64, error) {
	if h.pool == nil {
		return []auditSpan{}, 0, nil
	}

	args := &argList{}
	where := whereClause(filters.conditions("e", args, true))

	var total int64
	countSQL := fmt.Sprintf(`SELECT COUNT(*) FROM %s AS e WHERE %s`, auditTable, where)
	if err := h.pool.QueryRow(ctx, countSQL, args.values...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit spans: %w", err)
	}

	// No JOIN: one audit_events row is one span, so `total` and the page can
	// never disagree the way the admin user listing's LEFT JOIN made them.
	listSQL := fmt.Sprintf(`
SELECT e.id, e.timestamp, e.user_id, e.user_email, e.project_id, e.event_type, e.action,
       e.http_method, e.http_route, e.status_code, e.duration_ms, e.is_error, e.entity_name,
       e.tool_name, e.model_name, e.trace_id, e.span_id, e.parent_span_id
FROM %s AS e
WHERE %s
ORDER BY %s
LIMIT %s OFFSET %s`,
		auditTable, where,
		orderBy(listing.sortBy, listing.sortOrder, sortableSpanColumns, "timestamp", "e.id"),
		args.add(listing.limit), args.add(listing.offset))

	pgRows, err := h.pool.Query(ctx, listSQL, args.values...)
	if err != nil {
		return nil, 0, fmt.Errorf("list audit spans: %w", err)
	}
	defer pgRows.Close()

	spans := []auditSpan{}
	for pgRows.Next() {
		var span auditSpan
		if err := pgRows.Scan(
			&span.ID, &span.Timestamp, &span.UserID, &span.UserEmail, &span.ProjectID,
			&span.EventType, &span.Action, &span.HTTPMethod, &span.HTTPRoute, &span.StatusCode,
			&span.DurationMS, &span.IsError, &span.EntityName, &span.ToolName, &span.ModelName,
			&span.TraceID, &span.SpanID, &span.ParentSpanID,
		); err != nil {
			return nil, 0, fmt.Errorf("scan audit span: %w", err)
		}
		spans = append(spans, span)
	}
	if err := pgRows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read audit spans: %w", err)
	}
	return spans, total, nil
}

/* ── traces ────────────────────────────────────────────────────────────── */

// auditTrace is one trace: the aggregate over its spans, plus the root span's
// identity for the row label.
type auditTrace struct {
	TraceID        string     `json:"trace_id"`
	StartTime      *time.Time `json:"start_time"`
	DurationMS     *float64   `json:"duration_ms"`
	SpanCount      int64      `json:"span_count"`
	ErrorCount     int64      `json:"error_count"`
	HasError       bool       `json:"has_error"`
	UserEmail      *string    `json:"user_email"`
	ProjectID      *int64     `json:"project_id"`
	EventTypes     []string   `json:"event_types"`
	RootAction     *string    `json:"root_action"`
	RootEventType  *string    `json:"root_event_type"`
	RootHTTPMethod *string    `json:"root_http_method"`
	RootStatusCode *int32     `json:"root_status_code"`
}

// traceAggregateSQL groups the filtered spans into one row per trace.
//
// `duration_ms` is the wall-clock span of the trace: the latest span END
// (its timestamp plus its own duration) minus the earliest span START. A trace
// whose spans are all instantaneous is 0ms, not NULL.
const traceAggregateSQL = `
SELECT e.trace_id,
       MIN(e.timestamp) AS start_time,
       (MAX(EXTRACT(EPOCH FROM e.timestamp) + COALESCE(e.duration_ms, 0) / 1000)
        - MIN(EXTRACT(EPOCH FROM e.timestamp))) * 1000 AS duration_ms,
       COUNT(*) AS span_count,
       COUNT(*) FILTER (WHERE e.is_error) AS error_count,
       BOOL_OR(e.is_error) AS has_error,
       MIN(e.user_email) AS user_email,
       MIN(e.project_id) AS project_id,
       ARRAY_AGG(DISTINCT e.event_type) AS event_types
FROM %s AS e
WHERE e.trace_id IS NOT NULL AND %s
GROUP BY e.trace_id`

// AuditTraces serves `GET /elitea_core/audit_traces/{mode}` — spans grouped by
// trace_id. Replaces a stub that returned `{"items":[],"total":0}` — note the
// key, which the client has never read.
func (h *Handler) AuditTraces(w http.ResponseWriter, r *http.Request) {
	limit, offset := auditPagination(r)
	filters, ok := parseAuditRequest(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()

	rows, total, err := h.listAuditTraces(r.Context(), filters, spanListing{
		limit:     limit,
		offset:    offset,
		sortBy:    query.Get("sort_by"),
		sortOrder: query.Get("sort_order"),
	})
	if err != nil {
		auditReadFailed(w, "audit traces")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "total": total})
}

// traceDurationConditions applies duration_min/duration_max to the AGGREGATED
// trace duration. They are excluded from the span-level WHERE (see
// `conditions`) because they describe the trace, not the span — the heatmap
// drill-down sends the band the whole trace fell into.
func traceDurationConditions(filters auditFilters, args *argList) []string {
	var where []string
	if filters.durationMin != nil {
		where = append(where, "a.duration_ms >= "+args.add(*filters.durationMin))
	}
	if filters.durationMax != nil {
		where = append(where, "a.duration_ms < "+args.add(*filters.durationMax))
	}
	return where
}

func (h *Handler) listAuditTraces(
	ctx context.Context, filters auditFilters, listing spanListing,
) ([]auditTrace, int64, error) {
	if h.pool == nil {
		return []auditTrace{}, 0, nil
	}

	args := &argList{}
	aggregate := fmt.Sprintf(traceAggregateSQL, auditTable, whereClause(filters.conditions("e", args, false)))
	outerWhere := whereClause(traceDurationConditions(filters, args))

	var total int64
	countSQL := fmt.Sprintf("WITH a AS (%s) SELECT COUNT(*) FROM a WHERE %s", aggregate, outerWhere)
	if err := h.pool.QueryRow(ctx, countSQL, args.values...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit traces: %w", err)
	}

	// The root-span lookup is a LATERAL with LIMIT 1, so it contributes exactly
	// one row per trace and cannot multiply the page against `total`. It also
	// folds in pylon's fallback: `parent_span_id IS NULL` first, then the
	// earliest span. pylon ran that fallback as one extra query PER trace
	// missing a root, and resolved ties between several parentless spans by
	// dict overwrite — whichever row the driver happened to yield last.
	listSQL := fmt.Sprintf(`
WITH a AS (%s)
SELECT a.trace_id, a.start_time, a.duration_ms, a.span_count, a.error_count, a.has_error,
       a.user_email, a.project_id, a.event_types,
       root.action, root.event_type, root.http_method, root.status_code
FROM a
LEFT JOIN LATERAL (
    SELECT s.action, s.event_type, s.http_method, s.status_code
    FROM %s AS s
    WHERE s.trace_id = a.trace_id
    ORDER BY (s.parent_span_id IS NULL) DESC, s.timestamp ASC, s.id ASC
    LIMIT 1
) AS root ON TRUE
WHERE %s
ORDER BY %s
LIMIT %s OFFSET %s`,
		aggregate, auditTable, outerWhere,
		orderBy(listing.sortBy, listing.sortOrder, sortableTraceColumns, "start_time", "a.trace_id"),
		args.add(listing.limit), args.add(listing.offset))

	pgRows, err := h.pool.Query(ctx, listSQL, args.values...)
	if err != nil {
		return nil, 0, fmt.Errorf("list audit traces: %w", err)
	}
	defer pgRows.Close()

	traces, err := scanAuditTraces(pgRows)
	if err != nil {
		return nil, 0, err
	}
	return traces, total, nil
}

func scanAuditTraces(pgRows pgx.Rows) ([]auditTrace, error) {
	traces := []auditTrace{}
	for pgRows.Next() {
		var trace auditTrace
		if err := pgRows.Scan(
			&trace.TraceID, &trace.StartTime, &trace.DurationMS, &trace.SpanCount,
			&trace.ErrorCount, &trace.HasError, &trace.UserEmail, &trace.ProjectID,
			&trace.EventTypes, &trace.RootAction, &trace.RootEventType,
			&trace.RootHTTPMethod, &trace.RootStatusCode,
		); err != nil {
			return nil, fmt.Errorf("scan audit trace: %w", err)
		}
		if trace.EventTypes == nil {
			trace.EventTypes = []string{}
		}
		traces = append(traces, trace)
	}
	if err := pgRows.Err(); err != nil {
		return nil, fmt.Errorf("read audit traces: %w", err)
	}
	return traces, nil
}

/* ── heatmaps ──────────────────────────────────────────────────────────── */

// AuditHeatmap serves `GET /elitea_core/audit_heatmap/{mode}` — span counts
// bucketed by time and duration band.
func (h *Handler) AuditHeatmap(w http.ResponseWriter, r *http.Request) {
	h.serveHeatmap(w, r, spanHeatmapSQL, "total_events", "audit heatmap")
}

// AuditTraceHeatmap serves `GET /elitea_core/audit_trace_heatmap/{mode}`.
// Replaces a stub that returned `{"data":[]}` with no `metadata` at all — the
// client reads `metadata.interval_seconds` to turn a clicked cell back into a
// time range, so the stub's shape could not have driven the drill-down either.
func (h *Handler) AuditTraceHeatmap(w http.ResponseWriter, r *http.Request) {
	h.serveHeatmap(w, r, traceHeatmapSQL, "total_traces", "audit trace heatmap")
}

// spanHeatmapSQL counts SPANS. `duration_ms IS NOT NULL` matches pylon: a span
// with no recorded duration belongs to no band, and bucketing it into ">10s"
// (the CASE's else) would invent latency that was never measured.
const spanHeatmapSQL = `
SELECT %[3]s AS bucket, %[4]s AS band, COUNT(*)
FROM %[1]s AS e
WHERE e.duration_ms IS NOT NULL AND %[2]s
GROUP BY 1, 2`

// traceHeatmapSQL counts TRACES: it aggregates per trace first, then buckets by
// the trace's start time and total duration.
const traceHeatmapSQL = `
WITH a AS (
    SELECT e.trace_id,
           MIN(e.timestamp) AS start_time,
           (MAX(EXTRACT(EPOCH FROM e.timestamp) + COALESCE(e.duration_ms, 0) / 1000)
            - MIN(EXTRACT(EPOCH FROM e.timestamp))) * 1000 AS duration_ms
    FROM %[1]s AS e
    WHERE e.trace_id IS NOT NULL AND %[2]s
    GROUP BY e.trace_id
)
SELECT %[3]s AS bucket, %[4]s AS band, COUNT(*)
FROM a
GROUP BY 1, 2`

// heatmapColumns names the timestamp and duration expression each query buckets
// on: raw span columns for the span heatmap, the CTE's aggregates for traces.
var heatmapColumns = map[string][2]string{
	spanHeatmapSQL:  {"e.timestamp", "e.duration_ms"},
	traceHeatmapSQL: {"a.start_time", "a.duration_ms"},
}

func (h *Handler) serveHeatmap(w http.ResponseWriter, r *http.Request, template, totalKey, what string) {
	filters, ok := parseAuditRequest(w, r)
	if !ok {
		return
	}
	// Both bounds are required: without them there is no axis to draw and no
	// way to choose a bucket width. pylon answers 400 here too.
	if filters.dateFrom == nil || filters.dateTo == nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "date_from and date_to are required")
		return
	}
	rangeSeconds := int64(filters.dateTo.Sub(*filters.dateFrom).Seconds())
	if rangeSeconds <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "date_to must be after date_from")
		return
	}

	interval := pickInterval(rangeSeconds)
	slots := timeSlots(*filters.dateFrom, *filters.dateTo, interval)
	if len(slots) > maxHeatmapBuckets {
		apierr.WriteStatus(w, http.StatusBadRequest, "date range is too wide to chart")
		return
	}

	counts, total, err := h.countHeatmapCells(r.Context(), filters, template, interval)
	if err != nil {
		auditReadFailed(w, what)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data": heatmapSeries(counts, slots),
		"metadata": map[string]any{
			"interval_seconds": interval.seconds,
			"interval_label":   interval.label,
			totalKey:           total,
			"bucket_count":     len(slots),
			"range_seconds":    rangeSeconds,
		},
	})
}

func (h *Handler) countHeatmapCells(
	ctx context.Context, filters auditFilters, template string, interval bucketInterval,
) (map[[2]int64]int64, int64, error) {
	counts := map[[2]int64]int64{}
	if h.pool == nil {
		return counts, 0, nil
	}

	columns := heatmapColumns[template]
	args := &argList{}
	// The trace heatmap's duration filter applies to the aggregate, so like the
	// trace listing it is kept out of the span-level WHERE.
	spanLevelDuration := template == spanHeatmapSQL
	query := fmt.Sprintf(template,
		auditTable,
		whereClause(filters.conditions("e", args, spanLevelDuration)),
		bucketExpression(columns[0], interval),
		bandExpression(columns[1]))

	pgRows, err := h.pool.Query(ctx, query, args.values...)
	if err != nil {
		return nil, 0, fmt.Errorf("query heatmap: %w", err)
	}
	defer pgRows.Close()

	var total int64
	for pgRows.Next() {
		var bucket, band, count int64
		if err := pgRows.Scan(&bucket, &band, &count); err != nil {
			return nil, 0, fmt.Errorf("scan heatmap cell: %w", err)
		}
		counts[[2]int64{bucket, band}] = count
		total += count
	}
	if err := pgRows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read heatmap: %w", err)
	}
	return counts, total, nil
}
