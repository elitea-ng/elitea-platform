package eliteacore_test

// Unit A14 acceptance for the admin audit-trail READ surface (issue #200).
//
// The defect class here is not "the endpoint answers the wrong status". Two of
// these four routes already answered 200 before this unit — with an empty array
// and the request discarded. So a status assertion proves nothing, and neither
// does "the body has a `rows` key". Every case below seeds KNOWN rows and
// asserts the endpoint's answer against them: which rows come back, in which
// order, how many, and under which bucket.
//
// The two aggregation defects worth naming, both of which a "does it 200?" test
// passes straight through:
//
//   - ROW MULTIPLICATION. The trace listing joins the events table back onto
//     its own aggregate to resolve each trace's root span. If that join were a
//     plain LEFT JOIN, a trace with two parentless spans would occupy two rows
//     while the separate COUNT reported one — exactly what the admin user
//     listing did before A14. `TestAuditTracesDoNotMultiplyOnMultipleRootSpans`
//     is the guard.
//   - COUNTING SPANS AS TRACES. The trace heatmap must count traces; the span
//     heatmap must count spans. Seeded so the two answers CANNOT coincide.
//
// Requires a PostgreSQL to create an isolated database in; skipped otherwise,
// like every other *_postgres_integration_test.go in this service.
//
// No test prints a seeded row's contents on failure beyond the ids and counts
// under assertion — audit records are sensitive even when synthetic.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
)

/* ── harness ───────────────────────────────────────────────────────────── */

type auditSpanRow struct {
	ID         int64    `json:"id"`
	Timestamp  *string  `json:"timestamp"`
	UserEmail  *string  `json:"user_email"`
	ProjectID  *int64   `json:"project_id"`
	EventType  string   `json:"event_type"`
	Action     string   `json:"action"`
	StatusCode *int32   `json:"status_code"`
	DurationMS *float64 `json:"duration_ms"`
	IsError    bool     `json:"is_error"`
	TraceID    *string  `json:"trace_id"`
	ToolName   *string  `json:"tool_name"`
}

type auditSpanListing struct {
	Rows  []auditSpanRow `json:"rows"`
	Total int            `json:"total"`
}

type auditTraceRow struct {
	TraceID       string   `json:"trace_id"`
	StartTime     *string  `json:"start_time"`
	DurationMS    *float64 `json:"duration_ms"`
	SpanCount     int64    `json:"span_count"`
	ErrorCount    int64    `json:"error_count"`
	HasError      bool     `json:"has_error"`
	UserEmail     *string  `json:"user_email"`
	EventTypes    []string `json:"event_types"`
	RootAction    *string  `json:"root_action"`
	RootEventType *string  `json:"root_event_type"`
}

type auditTraceListing struct {
	Rows  []auditTraceRow `json:"rows"`
	Total int             `json:"total"`
}

type heatmapPoint struct {
	X int64 `json:"x"`
	// `y` is null for a bucket with no events, which is NOT the same as zero:
	// the client renders null as an empty cell.
	Y *int64 `json:"y"`
}

type heatmapSeries struct {
	ID   string         `json:"id"`
	Data []heatmapPoint `json:"data"`
}

type heatmapBody struct {
	Data     []heatmapSeries `json:"data"`
	Metadata struct {
		IntervalSeconds int64  `json:"interval_seconds"`
		IntervalLabel   string `json:"interval_label"`
		TotalEvents     int64  `json:"total_events"`
		TotalTraces     int64  `json:"total_traces"`
		BucketCount     int    `json:"bucket_count"`
		RangeSeconds    int64  `json:"range_seconds"`
	} `json:"metadata"`
}

// auditRouter mounts the four routes exactly as internal/api/router.go does,
// minus the route-level permission middleware (`TestRequireCentralPermissions*`
// in internal/api/middleware covers that layer on its own).
func auditRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Get("/elitea_core/audit/{mode}", handler.AuditTrail)
	router.Get("/elitea_core/audit_heatmap/{mode}", handler.AuditHeatmap)
	router.Get("/elitea_core/audit_traces/{mode}", handler.AuditTraces)
	router.Get("/elitea_core/audit_trace_heatmap/{mode}", handler.AuditTraceHeatmap)
	return router
}

func auditGet(t *testing.T, router chi.Router, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

func decodeOK[T any](t *testing.T, recorder *httptest.ResponseRecorder, what string) T {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s status = %d, want 200 (body %s)", what, recorder.Code, recorder.Body.String())
	}
	var decoded T
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode %s body: %v", what, err)
	}
	return decoded
}

func readSpans(t *testing.T, router chi.Router, query string) auditSpanListing {
	t.Helper()
	return decodeOK[auditSpanListing](t, auditGet(t, router, "/elitea_core/audit/administration?"+query), "GET audit")
}

func readTraces(t *testing.T, router chi.Router, query string) auditTraceListing {
	t.Helper()
	return decodeOK[auditTraceListing](t, auditGet(t, router, "/elitea_core/audit_traces/administration?"+query), "GET audit_traces")
}

/* ── fixture ───────────────────────────────────────────────────────────── */

// baseInstant anchors every seeded row. Fixed rather than relative to now(), so
// the bucket boundaries a heatmap assertion depends on cannot shift under a
// test that happens to run at a minute boundary.
var baseInstant = time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

type seedSpan struct {
	offset     time.Duration
	eventType  string
	action     string
	userEmail  string
	projectID  int64
	statusCode int32
	durationMS float64
	isError    bool
	traceID    string
	spanID     string
	parentSpan string
	toolName   string
}

func seedAuditEvents(t *testing.T, pool *pgxpool.Pool, spans []seedSpan) {
	t.Helper()
	ctx := context.Background()
	for _, span := range spans {
		var parent, tool any
		if span.parentSpan != "" {
			parent = span.parentSpan
		}
		if span.toolName != "" {
			tool = span.toolName
		}
		_, err := pool.Exec(ctx, `
INSERT INTO centry.audit_events
  (timestamp, user_id, user_email, project_id, event_type, action, http_method,
   status_code, duration_ms, is_error, tool_name, trace_id, span_id, parent_span_id)
VALUES ($1, 7, $2, $3, $4, $5, 'GET', $6, $7, $8, $9, $10, $11, $12)`,
			baseInstant.Add(span.offset), span.userEmail, span.projectID, span.eventType,
			span.action, span.statusCode, span.durationMS, span.isError, tool,
			span.traceID, span.spanID, parent)
		if err != nil {
			t.Fatalf("seed audit event: %v", err)
		}
	}
}

/* ── span listing ──────────────────────────────────────────────────────── */

func TestAuditTrailListsSeededSpansWithTheirRealColumns(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	seedAuditEvents(t, pool, []seedSpan{
		{offset: 0, eventType: "api", action: "list_agents", userEmail: "ada@autotest.local",
			projectID: 1, statusCode: 200, durationMS: 42.5, traceID: "t-alpha", spanID: "s1"},
		{offset: time.Minute, eventType: "llm", action: "completion", userEmail: "bo@autotest.local",
			projectID: 2, statusCode: 500, durationMS: 1500, isError: true, traceID: "t-beta", spanID: "s2"},
	})

	listing := readSpans(t, router, "limit=50&offset=0")
	if listing.Total != 2 || len(listing.Rows) != 2 {
		t.Fatalf("listing = %d rows / total %d, want 2 / 2", len(listing.Rows), listing.Total)
	}

	// Default sort is timestamp DESC, so the later row leads.
	newest := listing.Rows[0]
	if newest.Action != "completion" || newest.EventType != "llm" {
		t.Fatalf("first row = %s/%s, want completion/llm", newest.Action, newest.EventType)
	}
	// The columns the page renders must arrive populated, not as nulls — every
	// one of them exists in centry.audit_events.
	if newest.StatusCode == nil || *newest.StatusCode != 500 {
		t.Fatalf("status_code = %v, want 500", newest.StatusCode)
	}
	if newest.DurationMS == nil || *newest.DurationMS != 1500 {
		t.Fatalf("duration_ms = %v, want 1500", newest.DurationMS)
	}
	if !newest.IsError {
		t.Fatalf("is_error = false, want true")
	}
	if newest.UserEmail == nil || *newest.UserEmail != "bo@autotest.local" {
		t.Fatalf("user_email = %v, want bo@autotest.local", newest.UserEmail)
	}
	if newest.TraceID == nil || *newest.TraceID != "t-beta" {
		t.Fatalf("trace_id = %v, want t-beta", newest.TraceID)
	}
	if newest.Timestamp == nil {
		t.Fatalf("timestamp is null")
	}
}

func TestAuditTrailPaginationIsStableAcrossTiedTimestamps(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	// Every row shares one timestamp. Without a tiebreaker, `ORDER BY timestamp
	// DESC LIMIT 1 OFFSET n` is free to return the same row twice across pages
	// and never return another — a real defect in the legacy original.
	spans := make([]seedSpan, 0, 6)
	for i := range 6 {
		spans = append(spans, seedSpan{
			eventType: "api", action: fmt.Sprintf("action_%d", i),
			userEmail: "ada@autotest.local", projectID: 1, statusCode: 200, durationMS: 5,
		})
	}
	seedAuditEvents(t, pool, spans)

	seen := map[int64]int{}
	for page := range 6 {
		listing := readSpans(t, router, fmt.Sprintf("limit=1&offset=%d", page))
		if len(listing.Rows) != 1 {
			t.Fatalf("page %d returned %d rows, want 1", page, len(listing.Rows))
		}
		seen[listing.Rows[0].ID]++
	}
	if len(seen) != 6 {
		t.Fatalf("paging through 6 tied rows one at a time yielded %d distinct rows, want 6", len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("row %d appeared on %d pages, want 1", id, count)
		}
	}
}

func TestAuditTrailFiltersAreAppliedByTheServer(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	seedAuditEvents(t, pool, []seedSpan{
		{eventType: "api", action: "list_agents", userEmail: "ada@autotest.local",
			projectID: 1, statusCode: 200, durationMS: 5, traceID: "t-a", spanID: "s1"},
		{offset: time.Minute, eventType: "tool", action: "invoke", userEmail: "bo@autotest.local",
			projectID: 2, statusCode: 500, durationMS: 900, isError: true, traceID: "t-b", spanID: "s2",
			toolName: "github_search"},
		{offset: 2 * time.Minute, eventType: "llm", action: "completion", userEmail: "cy@autotest.local",
			projectID: 1, statusCode: 200, durationMS: 12000, traceID: "t-c", spanID: "s3"},
	})

	for _, testCase := range []struct {
		name    string
		query   string
		wantIDs []string // matched on `action`
	}{
		{"single event_type", "event_type=llm", []string{"completion"}},
		{"comma-separated event_type set", "event_type=api,tool", []string{"invoke", "list_agents"}},
		{"errors only", "is_error=true", []string{"invoke"}},
		{"project", "project_id=2", []string{"invoke"}},
		{"user", "user_id=7", []string{"completion", "invoke", "list_agents"}},
		{"search matches the tool name, not just the action", "search=github_sea", []string{"invoke"}},
		{"search matches the user email", "search=cy@autotest", []string{"completion"}},
		{"duration lower bound", "duration_min=800", []string{"completion", "invoke"}},
		{"duration upper bound", "duration_max=800", []string{"list_agents"}},
		{"trace", "trace_id=t-c", []string{"completion"}},
		{"an unmatched filter returns nothing, not everything", "project_id=999", nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			listing := readSpans(t, router, "limit=50&"+testCase.query)
			got := make([]string, 0, len(listing.Rows))
			for _, row := range listing.Rows {
				got = append(got, row.Action)
			}
			if listing.Total != len(testCase.wantIDs) {
				t.Fatalf("total = %d, want %d (rows %v)", listing.Total, len(testCase.wantIDs), got)
			}
			if len(got) != len(testCase.wantIDs) {
				t.Fatalf("rows = %v, want %v", got, testCase.wantIDs)
			}
			for _, want := range testCase.wantIDs {
				if !containsString(got, want) {
					t.Fatalf("rows = %v, missing %q", got, want)
				}
			}
		})
	}
}

func TestAuditTrailDateRangeExcludesRowsOutsideIt(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	seedAuditEvents(t, pool, []seedSpan{
		{offset: -2 * time.Hour, eventType: "api", action: "too_early", projectID: 1, durationMS: 5},
		{offset: 0, eventType: "api", action: "inside", projectID: 1, durationMS: 5},
		{offset: 2 * time.Hour, eventType: "api", action: "too_late", projectID: 1, durationMS: 5},
	})

	listing := readSpans(t, router, fmt.Sprintf("limit=50&date_from=%s&date_to=%s",
		baseInstant.Add(-time.Hour).Format(time.RFC3339),
		baseInstant.Add(time.Hour).Format(time.RFC3339)))

	if listing.Total != 1 || len(listing.Rows) != 1 || listing.Rows[0].Action != "inside" {
		t.Fatalf("date-bounded listing returned %d rows (total %d), want only \"inside\"",
			len(listing.Rows), listing.Total)
	}
}

func TestAuditTrailRejectsAnUnknownSortColumnInsteadOfInterpolatingIt(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))
	seedAuditEvents(t, pool, []seedSpan{
		{eventType: "api", action: "survivor", projectID: 1, durationMS: 5},
	})

	listing := readSpans(t, router, "limit=50&sort_by="+
		"timestamp%3B+DROP+TABLE+centry.audit_events")
	if listing.Total != 1 {
		t.Fatalf("total = %d after an injected sort_by, want 1", listing.Total)
	}

	// The table must still be there: an interpolated ORDER BY would have run
	// the statement, and a listing that merely errored would also "pass" a
	// status-only assertion.
	var remaining int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM centry.audit_events`).Scan(&remaining); err != nil {
		t.Fatalf("audit_events is gone after an injected sort_by: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("audit_events holds %d rows, want 1", remaining)
	}
}

/* ── trace listing ─────────────────────────────────────────────────────── */

func TestAuditTracesGroupSpansAndAggregateThem(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	seedAuditEvents(t, pool, []seedSpan{
		{offset: 0, eventType: "api", action: "POST /chat", userEmail: "ada@autotest.local",
			projectID: 1, statusCode: 200, durationMS: 10, traceID: "t-one", spanID: "root"},
		{offset: time.Second, eventType: "llm", action: "completion", userEmail: "ada@autotest.local",
			projectID: 1, statusCode: 200, durationMS: 2000, traceID: "t-one", spanID: "child1",
			parentSpan: "root"},
		{offset: 3 * time.Second, eventType: "tool", action: "search", userEmail: "ada@autotest.local",
			projectID: 1, statusCode: 500, durationMS: 500, isError: true, traceID: "t-one",
			spanID: "child2", parentSpan: "root"},
		{offset: time.Minute, eventType: "api", action: "GET /agents", userEmail: "bo@autotest.local",
			projectID: 2, statusCode: 200, durationMS: 20, traceID: "t-two", spanID: "solo"},
	})

	listing := readTraces(t, router, "limit=50")
	if listing.Total != 2 || len(listing.Rows) != 2 {
		t.Fatalf("trace listing = %d rows / total %d, want 2 / 2 (4 spans, 2 traces)",
			len(listing.Rows), listing.Total)
	}

	trace := traceByID(t, listing, "t-one")
	if trace.SpanCount != 3 {
		t.Fatalf("span_count = %d, want 3", trace.SpanCount)
	}
	if trace.ErrorCount != 1 || !trace.HasError {
		t.Fatalf("error_count/has_error = %d/%v, want 1/true", trace.ErrorCount, trace.HasError)
	}
	// The root is the parentless span, and the row is labelled with ITS action —
	// not with whichever span happened to sort first.
	if trace.RootAction == nil || *trace.RootAction != "POST /chat" {
		t.Fatalf("root_action = %v, want POST /chat", trace.RootAction)
	}
	if trace.RootEventType == nil || *trace.RootEventType != "api" {
		t.Fatalf("root_event_type = %v, want api", trace.RootEventType)
	}
	// duration spans the whole trace: last span END (3s + 500ms) minus first
	// span START (0), in milliseconds.
	if trace.DurationMS == nil || *trace.DurationMS < 3499 || *trace.DurationMS > 3501 {
		t.Fatalf("duration_ms = %v, want ~3500", trace.DurationMS)
	}
	if len(trace.EventTypes) != 3 {
		t.Fatalf("event_types = %v, want three distinct types", trace.EventTypes)
	}
}

func TestAuditTracesDoNotMultiplyOnMultipleRootSpans(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	// THREE parentless spans in one trace. A plain LEFT JOIN to resolve the
	// root would emit this trace three times while `total` said one — the
	// row-multiplication defect the admin user listing shipped before A14.
	seedAuditEvents(t, pool, []seedSpan{
		{offset: 0, eventType: "api", action: "first_root", projectID: 1, durationMS: 10, traceID: "t-multi", spanID: "r1"},
		{offset: time.Second, eventType: "rpc", action: "second_root", projectID: 1, durationMS: 10, traceID: "t-multi", spanID: "r2"},
		{offset: 2 * time.Second, eventType: "rpc", action: "third_root", projectID: 1, durationMS: 10, traceID: "t-multi", spanID: "r3"},
	})

	listing := readTraces(t, router, "limit=50")
	if listing.Total != 1 {
		t.Fatalf("total = %d, want 1", listing.Total)
	}
	if len(listing.Rows) != 1 {
		t.Fatalf("trace with 3 parentless spans occupied %d rows, want 1", len(listing.Rows))
	}
	if listing.Rows[0].SpanCount != 3 {
		t.Fatalf("span_count = %d, want 3", listing.Rows[0].SpanCount)
	}
	// Ties between parentless spans resolve by timestamp, deterministically.
	// The legacy runtime resolved them by dict overwrite — whichever row came back last.
	if listing.Rows[0].RootAction == nil || *listing.Rows[0].RootAction != "first_root" {
		t.Fatalf("root_action = %v, want first_root (the earliest parentless span)", listing.Rows[0].RootAction)
	}
}

func TestAuditTracesFallBackToTheEarliestSpanWhenNoneIsParentless(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	seedAuditEvents(t, pool, []seedSpan{
		{offset: time.Second, eventType: "tool", action: "later_child", projectID: 1, durationMS: 10,
			traceID: "t-orphan", spanID: "c2", parentSpan: "missing"},
		{offset: 0, eventType: "llm", action: "earliest_child", projectID: 1, durationMS: 10,
			traceID: "t-orphan", spanID: "c1", parentSpan: "missing"},
	})

	listing := readTraces(t, router, "limit=50")
	if len(listing.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(listing.Rows))
	}
	if listing.Rows[0].RootAction == nil || *listing.Rows[0].RootAction != "earliest_child" {
		t.Fatalf("root_action = %v, want earliest_child", listing.Rows[0].RootAction)
	}
}

func TestAuditTracesDurationFilterAppliesToTheTraceNotTheSpan(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	// Every individual span is under 100ms, but the trace as a whole lasts 5s.
	// A duration filter applied to the SPANS would drop this trace from the
	// 1-10s band the heatmap drill-down asks for.
	seedAuditEvents(t, pool, []seedSpan{
		{offset: 0, eventType: "api", action: "start", projectID: 1, durationMS: 20, traceID: "t-slow", spanID: "r"},
		{offset: 5 * time.Second, eventType: "tool", action: "finish", projectID: 1, durationMS: 20,
			traceID: "t-slow", spanID: "c", parentSpan: "r"},
	})

	listing := readTraces(t, router, "limit=50&duration_min=1000&duration_max=10000")
	if listing.Total != 1 || len(listing.Rows) != 1 {
		t.Fatalf("trace listing = %d rows / total %d for the 1-10s band, want 1 / 1",
			len(listing.Rows), listing.Total)
	}
	if listing.Rows[0].TraceID != "t-slow" {
		t.Fatalf("trace_id = %s, want t-slow", listing.Rows[0].TraceID)
	}
}

func TestAuditTracesExcludeSpansWithNoTrace(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	if _, err := pool.Exec(context.Background(), `
INSERT INTO centry.audit_events (timestamp, event_type, action, duration_ms, is_error)
VALUES ($1, 'api', 'untraced', 5, false)`, baseInstant); err != nil {
		t.Fatalf("seed untraced span: %v", err)
	}
	seedAuditEvents(t, pool, []seedSpan{
		{eventType: "api", action: "traced", projectID: 1, durationMS: 5, traceID: "t-real", spanID: "s"},
	})

	if listing := readTraces(t, router, "limit=50"); listing.Total != 1 {
		t.Fatalf("total = %d, want 1 (the untraced span must not become a trace)", listing.Total)
	}
	// It is still a SPAN, though — the flat listing does not filter it out.
	if listing := readSpans(t, router, "limit=50"); listing.Total != 2 {
		t.Fatalf("span total = %d, want 2", listing.Total)
	}
}

/* ── heatmaps ──────────────────────────────────────────────────────────── */

func heatmapURL(path string, from, to time.Time, extra string) string {
	target := fmt.Sprintf("/elitea_core/%s/administration?date_from=%s&date_to=%s",
		path, from.Format(time.RFC3339), to.Format(time.RFC3339))
	if extra != "" {
		target += "&" + extra
	}
	return target
}

func TestAuditHeatmapBucketsSpansByTimeAndDurationBand(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	seedAuditEvents(t, pool, []seedSpan{
		{offset: 0, eventType: "api", action: "fast", projectID: 1, durationMS: 5},
		{offset: 90 * time.Second, eventType: "api", action: "also_fast", projectID: 1, durationMS: 9},
		{offset: 20 * time.Minute, eventType: "llm", action: "slow", projectID: 1, durationMS: 20000},
	})

	// A 30-minute window ⇒ 1-minute buckets (the ≤1h row of the interval table).
	body := decodeOK[heatmapBody](t,
		auditGet(t, router, heatmapURL("audit_heatmap", baseInstant.Add(-5*time.Minute), baseInstant.Add(25*time.Minute), "")),
		"GET audit_heatmap")

	if body.Metadata.IntervalSeconds != 60 || body.Metadata.IntervalLabel != "1min" {
		t.Fatalf("interval = %ds/%q, want 60s/\"1min\"", body.Metadata.IntervalSeconds, body.Metadata.IntervalLabel)
	}
	if body.Metadata.TotalEvents != 3 {
		t.Fatalf("total_events = %d, want 3", body.Metadata.TotalEvents)
	}
	// Five bands, slowest first — the client's Y axis, and the labels it maps
	// back to millisecond bounds on a cell click.
	if len(body.Data) != 5 {
		t.Fatalf("series = %d, want 5 bands", len(body.Data))
	}
	if body.Data[0].ID != ">10s" || body.Data[4].ID != "<10ms" {
		t.Fatalf("band order = %s … %s, want >10s … <10ms", body.Data[0].ID, body.Data[4].ID)
	}
	// The two 5ms/9ms spans share the "<10ms" band but are 90 seconds apart, so
	// they must occupy two DIFFERENT time buckets on the 1-minute grid — a
	// bucket expression that ignored the timestamp would pile them into one.
	fastest := seriesByID(t, body, "<10ms")
	if got := nonNullTotal(fastest); got != 2 {
		t.Fatalf("<10ms band holds %d events, want 2", got)
	}
	if buckets := nonNullBuckets(fastest); buckets != 2 {
		t.Fatalf("<10ms band occupies %d buckets, want 2 (the spans are 90s apart on a 1min grid)", buckets)
	}
	if got := nonNullTotal(seriesByID(t, body, ">10s")); got != 1 {
		t.Fatalf(">10s band holds %d events, want 1", got)
	}
	// Bands with nothing in them are still present, all-null: a missing series
	// would collapse the Y axis.
	if got := nonNullBuckets(seriesByID(t, body, "1-10s")); got != 0 {
		t.Fatalf("1-10s band holds %d non-null buckets, want 0", got)
	}

	// Every requested minute is represented, including those with no events —
	// otherwise the time axis silently compresses over quiet periods.
	if body.Metadata.BucketCount != len(body.Data[0].Data) {
		t.Fatalf("bucket_count = %d but the series carries %d points",
			body.Metadata.BucketCount, len(body.Data[0].Data))
	}
	if body.Metadata.BucketCount != 31 {
		t.Fatalf("bucket_count = %d, want 31 (a 30-minute window on a 1-minute grid)", body.Metadata.BucketCount)
	}
}

func TestAuditHeatmapExcludesSpansWithNoRecordedDuration(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	if _, err := pool.Exec(context.Background(), `
INSERT INTO centry.audit_events (timestamp, event_type, action, duration_ms, is_error)
VALUES ($1, 'api', 'no_duration', NULL, false)`, baseInstant); err != nil {
		t.Fatalf("seed duration-less span: %v", err)
	}

	body := decodeOK[heatmapBody](t,
		auditGet(t, router, heatmapURL("audit_heatmap", baseInstant.Add(-5*time.Minute), baseInstant.Add(5*time.Minute), "")),
		"GET audit_heatmap")

	// A NULL duration compares false against every bound, so an unguarded CASE
	// would drop it into the else branch and invent a ">10s" event.
	if body.Metadata.TotalEvents != 0 {
		t.Fatalf("total_events = %d, want 0", body.Metadata.TotalEvents)
	}
	if got := nonNullTotal(seriesByID(t, body, ">10s")); got != 0 {
		t.Fatalf(">10s band holds %d events, want 0", got)
	}
}

func TestAuditTraceHeatmapCountsTracesNotSpans(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	// ONE trace, FIVE spans. The two heatmaps must therefore disagree — which
	// is the whole point of having both, and what a stub returning `{}` hid.
	spans := make([]seedSpan, 0, 5)
	for i := range 5 {
		spans = append(spans, seedSpan{
			offset: time.Duration(i) * time.Second, eventType: "llm",
			action: fmt.Sprintf("span_%d", i), projectID: 1, durationMS: 30,
			traceID: "t-wide", spanID: fmt.Sprintf("s%d", i),
		})
	}
	seedAuditEvents(t, pool, spans)

	from, to := baseInstant.Add(-5*time.Minute), baseInstant.Add(5*time.Minute)

	traceBody := decodeOK[heatmapBody](t,
		auditGet(t, router, heatmapURL("audit_trace_heatmap", from, to, "")), "GET audit_trace_heatmap")
	if traceBody.Metadata.TotalTraces != 1 {
		t.Fatalf("total_traces = %d, want 1", traceBody.Metadata.TotalTraces)
	}
	// The trace lasts 4s + the last span's 30ms ⇒ the 1-10s band, even though
	// every individual span is 30ms (the 10-100ms band).
	if got := nonNullTotal(seriesByID(t, traceBody, "1-10s")); got != 1 {
		t.Fatalf("1-10s band holds %d traces, want 1", got)
	}
	if got := nonNullTotal(seriesByID(t, traceBody, "10-100ms")); got != 0 {
		t.Fatalf("10-100ms band holds %d traces, want 0 — that is the SPAN duration", got)
	}

	spanBody := decodeOK[heatmapBody](t,
		auditGet(t, router, heatmapURL("audit_heatmap", from, to, "")), "GET audit_heatmap")
	if spanBody.Metadata.TotalEvents != 5 {
		t.Fatalf("total_events = %d, want 5", spanBody.Metadata.TotalEvents)
	}
	if got := nonNullTotal(seriesByID(t, spanBody, "10-100ms")); got != 5 {
		t.Fatalf("span 10-100ms band holds %d events, want 5", got)
	}
}

func TestHeatmapIntervalWidensWithTheRequestedRange(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	for _, testCase := range []struct {
		window    time.Duration
		wantLabel string
	}{
		{30 * time.Minute, "1min"},
		{5 * time.Hour, "5min"},
		{20 * time.Hour, "15min"},
		{5 * 24 * time.Hour, "1h"},
		{20 * 24 * time.Hour, "4h"},
		{90 * 24 * time.Hour, "1d"},
	} {
		t.Run(testCase.wantLabel, func(t *testing.T) {
			body := decodeOK[heatmapBody](t,
				auditGet(t, router, heatmapURL("audit_heatmap", baseInstant, baseInstant.Add(testCase.window), "")),
				"GET audit_heatmap")
			if body.Metadata.IntervalLabel != testCase.wantLabel {
				t.Fatalf("a %s window bucketed at %q, want %q",
					testCase.window, body.Metadata.IntervalLabel, testCase.wantLabel)
			}
		})
	}
}

func TestHeatmapRejectsAnUnusableDateRange(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	for _, testCase := range []struct {
		name   string
		target string
	}{
		{"no bounds at all", "/elitea_core/audit_heatmap/administration"},
		{"only date_from", "/elitea_core/audit_heatmap/administration?date_from=" + baseInstant.Format(time.RFC3339)},
		{"inverted range", heatmapURL("audit_heatmap", baseInstant.Add(time.Hour), baseInstant, "")},
		{"a range too wide to chart", heatmapURL("audit_heatmap", baseInstant.AddDate(-200, 0, 0), baseInstant, "")},
		{"trace heatmap, same rule", "/elitea_core/audit_trace_heatmap/administration"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 400, not an empty 200: an unusable range is the caller's mistake,
			// and answering "no data" for it is indistinguishable from a quiet
			// period with the axis silently wrong.
			if recorder := auditGet(t, router, testCase.target); recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", recorder.Code)
			}
		})
	}
}

func TestHeatmapHonoursTheSameFiltersAsTheListing(t *testing.T) {
	pool := newAuditPool(t)
	router := auditRouter(eliteacore.NewHandler(pool))

	seedAuditEvents(t, pool, []seedSpan{
		{eventType: "api", action: "kept", projectID: 1, durationMS: 5, traceID: "t-a", spanID: "s1"},
		{offset: time.Minute, eventType: "llm", action: "filtered_out", projectID: 2, durationMS: 5,
			traceID: "t-b", spanID: "s2"},
	})

	from, to := baseInstant.Add(-5*time.Minute), baseInstant.Add(5*time.Minute)
	body := decodeOK[heatmapBody](t,
		auditGet(t, router, heatmapURL("audit_heatmap", from, to, "event_type=api&project_id=1")),
		"GET audit_heatmap")

	// The heatmap and the table must agree: a chart drawn over unfiltered data
	// while the table below it is filtered is a lie about the same query.
	if body.Metadata.TotalEvents != 1 {
		t.Fatalf("total_events = %d under event_type=api&project_id=1, want 1", body.Metadata.TotalEvents)
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

func containsString(haystack []string, needle string) bool {
	for _, candidate := range haystack {
		if candidate == needle {
			return true
		}
	}
	return false
}

func traceByID(t *testing.T, listing auditTraceListing, traceID string) auditTraceRow {
	t.Helper()
	for _, row := range listing.Rows {
		if row.TraceID == traceID {
			return row
		}
	}
	t.Fatalf("trace %s missing from the listing", traceID)
	return auditTraceRow{}
}

func seriesByID(t *testing.T, body heatmapBody, id string) heatmapSeries {
	t.Helper()
	for _, series := range body.Data {
		if series.ID == id {
			return series
		}
	}
	t.Fatalf("band %q missing from the heatmap", id)
	return heatmapSeries{}
}

func nonNullTotal(series heatmapSeries) int64 {
	var total int64
	for _, point := range series.Data {
		if point.Y != nil {
			total += *point.Y
		}
	}
	return total
}

func nonNullBuckets(series heatmapSeries) int {
	count := 0
	for _, point := range series.Data {
		if point.Y != nil {
			count++
		}
	}
	return count
}

// newAuditPool creates an isolated database and applies the REAL bootstrap
// migration — the same 001_initial.sql a fresh deployment gets — so the
// centry.audit_events DDL this unit added to it is what the tests read through,
// rather than a second copy that could drift from it.
func newAuditPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_audit_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	initial, err := os.ReadFile(filepath.Join("..", "..", "..", "infra", "db", "migrations", "001_initial.sql"))
	if err != nil {
		t.Fatalf("read 001_initial.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		t.Fatalf("apply 001_initial.sql: %v", err)
	}
	// The naive-clock probe the dbtest template installs, after the last
	// schema statement: a writer that stores the session-local clock in a
	// `timestamp` WITHOUT time zone column fails instead of passing unread.
	if err := dbtest.InstallNaiveClockProbe(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
