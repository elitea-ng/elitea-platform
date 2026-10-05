package api

// The audit trail END TO END, against a real PostgreSQL: an administrative
// action through the production router leaves a row in `centry.audit_events`,
// and the admin Audit Trail's own read endpoint gives that row back.
//
// # Why the loop has to be closed here and not in two places
//
// A writer test that asserts "a row exists" and a reader test that asserts "a
// seeded row comes back" can both pass while the page stays empty: they can
// disagree about the column vocabulary, and neither would notice. The readers
// filter on `event_type` (the SPA sends the whole tab set), bucket on
// `duration_ms`, and group on `trace_id IS NOT NULL`. A writer that emitted
// `event_type = 'http'`, or left duration NULL, would satisfy a row-count
// assertion and render nothing. So the assertions below go through
// `GET /elitea_core/audit/administration` — the endpoint the page calls, with
// the parameters the page sends.
//
// Skipped without ELITEA_TEST_DATABASE_URL, like every other
// *_postgres_integration_test.go here. A skip prints the same "ok" as a pass,
// so run it with the variable set before believing it.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const auditEmitterDeadline = 60 * time.Second

/* ── the shape the SPA reads ────────────────────────────────────────────── */

// emittedSpan mirrors `AuditSpanRow` in
// apps/elitea-web/src/pages/admin/api/adminAuditApi.ts field for field, so a
// decode that loses a field here is a field the page would render as undefined.
type emittedSpan struct {
	ID         int64    `json:"id"`
	Timestamp  *string  `json:"timestamp"`
	UserID     *int64   `json:"user_id"`
	UserEmail  *string  `json:"user_email"`
	ProjectID  *int64   `json:"project_id"`
	EventType  string   `json:"event_type"`
	Action     string   `json:"action"`
	HTTPMethod *string  `json:"http_method"`
	HTTPRoute  *string  `json:"http_route"`
	StatusCode *int32   `json:"status_code"`
	DurationMS *float64 `json:"duration_ms"`
	IsError    bool     `json:"is_error"`
	EntityName *string  `json:"entity_name"`
	TraceID    *string  `json:"trace_id"`
	SpanID     *string  `json:"span_id"`
}

type emittedListing struct {
	Rows  []emittedSpan `json:"rows"`
	Total int           `json:"total"`
}

// readEmittedTrail calls the endpoint the Audit Trail page calls, with the
// event-type filter the page's "user" tab sends (USER_EVENT_TYPES in
// adminAuditApi.ts). A row this does not return is a row the page cannot show.
func readEmittedTrail(t *testing.T, pool *pgxpool.Pool) emittedListing {
	t.Helper()
	reader := chi.NewRouter()
	reader.Get("/elitea_core/audit/{mode}", eliteacore.NewHandler(pool).AuditTrail)

	response := httptest.NewRecorder()
	reader.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/elitea_core/audit/administration?limit=50&offset=0"+
			"&event_type=api,socketio,rpc,agent,tool,llm", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET audit = %d, want 200: %s", response.Code, response.Body.String())
	}
	var listing emittedListing
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode the audit listing: %v", err)
	}
	return listing
}

/* ── the test ───────────────────────────────────────────────────────────── */

// An administrative write through the production router lands in
// `centry.audit_events` and comes back out of the trail's own read endpoint.
//
// BEFORE this change nothing wrote that table in production: the only writers
// in the tree were the E2E seeder and test fixtures, and
// internal/api/generated/api.gen.go stated it — "READ-ONLY from this service.
// Never emitted today." Against that tree this test fails at the row count with
// 0 rows, which is the whole defect stated as an assertion.
func TestAdministrativeWriteLandsInTheAuditTrailTheAdminPageReads(t *testing.T) {
	pool := newAuditEmitterPool(t)
	recorder := audit.NewPostgresRecorder(pool, nil)
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
		defer cancel()
		if err := recorder.Close(closeCtx); err != nil {
			t.Errorf("close the audit recorder: %v", err)
		}
	})

	router := NewRouter(RouterConfig{
		Pool:               pool,
		AuthValidator:      testTokenValidator{user: privilegedAuditTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		AuditRecorder:      recorder,
	})

	// 001_initial.sql seeds user 1 with the administration `admin` role, and
	// privilegedAuditTestUser() IS user 1 — so this request is authorised and
	// actually suspends a user, rather than testing the refusal path twice.
	response := httptest.NewRecorder()
	request := testAuthHeader(httptest.NewRequest(
		http.MethodPut, "/api/v2/admin/user_suspend/administration/1",
		strings.NewReader(`{"suspended":true}`)))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT user_suspend = %d, want 200: %s", response.Code, response.Body.String())
	}

	// The write is asynchronous by design (DECISION 3), so the test waits on
	// the recorder's own barrier rather than sleeping.
	flushCtx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
	defer cancel()
	if err := recorder.Flush(flushCtx); err != nil {
		t.Fatalf("flush the audit recorder: %v", err)
	}
	if dropped := recorder.Dropped(); dropped != 0 {
		t.Fatalf("the recorder dropped %d events; the trail has holes", dropped)
	}

	listing := readEmittedTrail(t, pool)
	if listing.Total != 1 || len(listing.Rows) != 1 {
		t.Fatalf("the audit trail holds %d rows (total %d), want exactly 1 — "+
			"0 is the pre-change state, where nothing wrote this table",
			len(listing.Rows), listing.Total)
	}
	row := listing.Rows[0]

	// event_type: must be a value the page's tab filter asks for. The query
	// above already applied that filter, so reaching this line proves it.
	if row.EventType != "api" {
		t.Errorf("event_type = %q, want api", row.EventType)
	}
	// action: the handler's annotation, not the bare route. This is what makes
	// the trail say WHAT happened rather than which URL was called.
	if row.Action != "user.suspend" {
		t.Errorf("action = %q, want user.suspend (the handler's audit.Annotate)", row.Action)
	}
	if row.EntityName != nil {
		t.Errorf("entity_name = %v, want NULL; this handler annotates no name", *row.EntityName)
	}
	if row.UserID == nil || *row.UserID != 1 {
		t.Errorf("user_id = %v, want 1", row.UserID)
	}
	if row.UserEmail == nil || *row.UserEmail != "admin@test.local" {
		t.Errorf("user_email = %v, want the acting principal's address", row.UserEmail)
	}
	if row.HTTPMethod == nil || *row.HTTPMethod != http.MethodPut {
		t.Errorf("http_method = %v, want PUT", row.HTTPMethod)
	}
	// The route PATTERN. A raw target here would put user id 1 in a column the
	// page groups and sorts on.
	const wantRoute = "/api/v2/admin/user_suspend/{mode}/{userID}"
	if row.HTTPRoute == nil || *row.HTTPRoute != wantRoute {
		t.Errorf("http_route = %v, want %q", row.HTTPRoute, wantRoute)
	}
	if row.StatusCode == nil || *row.StatusCode != http.StatusOK {
		t.Errorf("status_code = %v, want 200", row.StatusCode)
	}
	if row.IsError {
		t.Error("is_error = true on a 200")
	}
	// duration_ms drives the heatmap's five bands; NULL removes the row from
	// half the page (spanHeatmapSQL's `duration_ms IS NOT NULL`).
	if row.DurationMS == nil || *row.DurationMS <= 0 {
		t.Errorf("duration_ms = %v, want a positive measurement", row.DurationMS)
	}
	// trace_id/span_id: the Traces view groups on `trace_id IS NOT NULL`.
	if row.TraceID == nil || len(*row.TraceID) != 32 {
		t.Errorf("trace_id = %v, want 32 hex characters", row.TraceID)
	}
	if row.SpanID == nil || len(*row.SpanID) != 16 {
		t.Errorf("span_id = %v, want 16 hex characters", row.SpanID)
	}
	if row.Timestamp == nil {
		t.Error("timestamp is NULL")
	}

	// The entity column is what the annotation is FOR: the trail must say which
	// account was suspended, and the endpoint above does not project entity_id.
	assertEntity(t, pool, row.ID, "user", 1)
}

// A refused administrative request is recorded too — with is_error and the
// refusing status — because "who tried to reach something they could not" is
// the question this table is most often opened to answer.
func TestRefusedAdministrativeRequestIsRecorded(t *testing.T) {
	pool := newAuditEmitterPool(t)
	recorder := audit.NewPostgresRecorder(pool, nil)
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
		defer cancel()
		if err := recorder.Close(closeCtx); err != nil {
			t.Errorf("close the audit recorder: %v", err)
		}
	})

	// User 9001 exists in no auth_core__user_role row, so the central
	// permission gate denies. Nothing else about the request changes.
	router := NewRouter(RouterConfig{
		Pool:               pool,
		AuthValidator:      testTokenValidator{user: unprivilegedAuditTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		AuditRecorder:      recorder,
	})

	response := httptest.NewRecorder()
	request := testAuthHeader(httptest.NewRequest(
		http.MethodPut, "/api/v2/admin/user_suspend/administration/1",
		strings.NewReader(`{"suspended":true}`)))
	router.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("PUT user_suspend as an unprivileged principal = %d, want 403", response.Code)
	}

	flushCtx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
	defer cancel()
	if err := recorder.Flush(flushCtx); err != nil {
		t.Fatalf("flush the audit recorder: %v", err)
	}

	listing := readEmittedTrail(t, pool)
	if len(listing.Rows) != 1 {
		t.Fatalf("the trail holds %d rows, want 1 for the refusal", len(listing.Rows))
	}
	row := listing.Rows[0]
	if !row.IsError || row.StatusCode == nil || *row.StatusCode != http.StatusForbidden {
		t.Fatalf("is_error/%v status/%v, want true/403", row.IsError, row.StatusCode)
	}
	if row.UserID == nil || *row.UserID != 9001 {
		t.Errorf("user_id = %v, want 9001; the refused caller must be named", row.UserID)
	}
	// No annotation: the handler never ran. The row degrades to the route,
	// which is exactly the documented degradation and not an absent row.
	if row.Action != "PUT /api/v2/admin/user_suspend/{mode}/{userID}" {
		t.Errorf("action = %q, want the route-level fallback", row.Action)
	}
}

// Content traffic must leave the table alone: DECISION 2's negative half,
// asserted against the real database rather than against a capture double.
func TestContentTrafficWritesNoAuditRows(t *testing.T) {
	pool := newAuditEmitterPool(t)
	recorder := audit.NewPostgresRecorder(pool, nil)
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
		defer cancel()
		if err := recorder.Close(closeCtx); err != nil {
			t.Errorf("close the audit recorder: %v", err)
		}
	})

	router := NewRouter(RouterConfig{
		Pool:               pool,
		AuthValidator:      testTokenValidator{user: privilegedAuditTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		AuditRecorder:      recorder,
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, testAuthHeader(httptest.NewRequest(
		http.MethodPost, "/api/v2/elitea_core/pin/prompt_lib/1/agent/2", strings.NewReader(`{}`))))

	flushCtx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
	defer cancel()
	if err := recorder.Flush(flushCtx); err != nil {
		t.Fatalf("flush the audit recorder: %v", err)
	}

	var rows int64
	if err := pool.QueryRow(flushCtx, `SELECT COUNT(*) FROM centry.audit_events`).Scan(&rows); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("content traffic wrote %d audit rows, want 0", rows)
	}
}

func assertEntity(t *testing.T, pool *pgxpool.Pool, rowID int64, wantType string, wantID int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
	defer cancel()

	var entityType *string
	var entityID *int64
	if err := pool.QueryRow(ctx,
		`SELECT entity_type, entity_id FROM centry.audit_events WHERE id = $1`, rowID,
	).Scan(&entityType, &entityID); err != nil {
		t.Fatalf("read the audit row's entity columns: %v", err)
	}
	if entityType == nil || *entityType != wantType {
		t.Errorf("entity_type = %v, want %q", entityType, wantType)
	}
	if entityID == nil || *entityID != wantID {
		t.Errorf("entity_id = %v, want %d", entityID, wantID)
	}
}

// privilegedAuditTestUser is user 1, whom 001_initial.sql seeds with the
// administration `admin` role — the account that holds `admin.auth.users`.
//
// NOT authenticatedTestUser(): that principal declares AuthType "token" with an
// empty TokenID, which legacyrbac.PostgresResolver refuses outright (a token id
// it cannot look up is not a principal it will resolve). Reusing it here would
// have made every case in this file a 403 and quietly retired the authorised
// path.
func privilegedAuditTestUser() auth.User {
	return auth.User{ID: "1", UserID: "1", Email: "admin@test.local", AuthType: "session"}
}

// unprivilegedAuditTestUser holds no role at all, so the central permission
// gate denies it. Same shape otherwise, so the refusal is the only difference.
func unprivilegedAuditTestUser() auth.User {
	return auth.User{ID: "9001", UserID: "9001", Email: "nobody@test.local", AuthType: "session"}
}

/* ── harness ───────────────────────────────────────────────────────────── */

func newAuditEmitterPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the audit emitter integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open the PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_audit_emit_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		adminPool.Close()
		t.Fatalf("create the isolated integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quoted+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open the isolated integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the isolated integration database: %v", err)
		}
		adminPool.Close()
	})

	source := filepath.Join("..", "infra", "db", "migrations", "001_initial.sql")
	initial, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		t.Fatalf("apply %s: %v", source, err)
	}
	// A suspension revokes the account's browser sessions and native device
	// sessions in the same transaction (ADR-0025 WP3), so their tables exist
	// here as they do on every migrated deployment.
	for _, migration := range []string{"0117_browser_sessions.sql", "0141_native_auth.sql"} {
		file := filepath.Join("..", "..", "migrations", "shared", migration)
		sql, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", file, err)
		}
	}

	// The table must start empty, or "1 row" below could be a fixture. The
	// migration creates it and inserts nothing; this asserts that rather than
	// assuming it.
	var existing int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM centry.audit_events`).Scan(&existing); err != nil {
		t.Fatalf("count the pre-existing audit rows: %v", err)
	}
	if existing != 0 {
		t.Fatalf("centry.audit_events starts with %d rows; the assertions below assume none", existing)
	}
	return pool
}
