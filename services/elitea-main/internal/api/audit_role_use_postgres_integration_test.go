package api

// The role-use columns of an administration-mode audit row (ADR-0059
// decision 8, story R1.4a; shared 0268), end to end through the production
// router against a real PostgreSQL: a row written by a role holder, by a
// token, by a Form session and under an elevation carries acting_roles,
// credential_kind and elevation_id. The emitter harness of
// audit_emitter_postgres_integration_test.go is reused, so the row is the one
// the recorder really writes.
//
// Skipped without ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// roleUseFormSessions stands in for the Form plane's cookie reader: every
// request carrying the cookie authenticates as user 1, as a Form session
// would (the Auth middleware sets the kind `form`).
type roleUseFormSessions struct{ user auth.User }

func (f roleUseFormSessions) AuthenticateFormSession(r *http.Request) (auth.User, error) {
	if _, err := r.Cookie("elitea_form_test"); err != nil {
		return auth.User{}, apimw.ErrFormSessionAbsent
	}
	return f.user, nil
}

type roleUseRow struct {
	ActingRoles    []string
	CredentialKind *string
	ElevationID    *string
	UserID         *int64
	StatusCode     *int32
}

func readRoleUseRows(t *testing.T, pool *pgxpool.Pool) []roleUseRow {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
	defer cancel()
	rows, err := pool.Query(ctx, `
SELECT acting_roles, credential_kind, elevation_id::text, user_id, status_code
FROM centry.audit_events ORDER BY id`)
	if err != nil {
		t.Fatalf("read the role-use columns: %v", err)
	}
	defer rows.Close()
	var out []roleUseRow
	for rows.Next() {
		var row roleUseRow
		if err := rows.Scan(&row.ActingRoles, &row.CredentialKind, &row.ElevationID, &row.UserID, &row.StatusCode); err != nil {
			t.Fatalf("scan a role-use row: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAdministrationModeRowsRecordTheRoleUse(t *testing.T) {
	pool := newAuditEmitterPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
	defer cancel()
	// The token row's admin_access (shared 0265), an `inherit` token of user
	// 1, and user 2, the account the requests suspend.
	body, err := platformmigrations.Files.ReadFile("shared/0265_token_admin_access.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply 0265: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, name) VALUES (2, 'target@test.local', 'Target')
ON CONFLICT (id) DO NOTHING;
INSERT INTO public.auth_core__token (id, uuid, user_id, name, admin_access)
VALUES (41, '00000000-0000-0000-0000-000000000041', 1, 'automation', 'inherit')`); err != nil {
		t.Fatalf("seed the target and the token: %v", err)
	}

	recorder := audit.NewPostgresRecorder(pool, nil)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
		defer closeCancel()
		if err := recorder.Close(closeCtx); err != nil {
			t.Errorf("close the audit recorder: %v", err)
		}
	})
	tokenUser := auth.User{
		ID: "41", UserID: "1", TokenID: "41", Email: "admin@test.local", AuthType: "token",
		CredentialKind: auth.CredentialKindToken,
	}
	router := NewRouter(RouterConfig{
		Pool:               pool,
		SessionSecret:      consoleTestSessionSecret,
		AuthValidator:      testTokenValidator{user: tokenUser},
		PrincipalValidator: testPrincipalValidator{},
		AuditRecorder:      recorder,
		Auth: AuthDeps{FormSessions: roleUseFormSessions{user: auth.User{
			ID: "1", UserID: "1", Email: "admin@test.local", AuthType: "session",
		}}},
	})
	session := asConsoleSession(router, privilegedAuditTestUser())

	suspend := func() *http.Request {
		request := httptest.NewRequest(http.MethodPut, "/api/v2/admin/user_suspend/administration/2",
			strings.NewReader(`{"suspended":true}`))
		request.Header.Set("Content-Type", "application/json")
		return request
	}
	const elevation = "0192f0c1-7b6e-7c3a-8f00-0000000000e1"
	steps := []struct {
		name  string
		serve func(*httptest.ResponseRecorder)
	}{
		{"role holder (session)", func(w *httptest.ResponseRecorder) { session.ServeHTTP(w, suspend()) }},
		{"token", func(w *httptest.ResponseRecorder) { router.ServeHTTP(w, testAuthHeader(suspend())) }},
		{"form session", func(w *httptest.ResponseRecorder) {
			request := suspend()
			request.AddCookie(&http.Cookie{Name: "elitea_form_test", Value: "1"})
			router.ServeHTTP(w, request)
		}},
		// Until R2.1 produces an elevation, the id is set through the
		// context accessor.
		{"under an elevation", func(w *httptest.ResponseRecorder) {
			request := suspend()
			session.ServeHTTP(w, request.WithContext(audit.ContextWithElevationID(request.Context(), elevation)))
		}},
	}
	for _, step := range steps {
		response := httptest.NewRecorder()
		step.serve(response)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: PUT user_suspend = %d, want 200: %s", step.name, response.Code, response.Body.String())
		}
	}
	flushCtx, flushCancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
	defer flushCancel()
	if err := recorder.Flush(flushCtx); err != nil {
		t.Fatalf("flush the audit recorder: %v", err)
	}
	if dropped := recorder.Dropped(); dropped != 0 {
		t.Fatalf("the recorder dropped %d events", dropped)
	}

	rows := readRoleUseRows(t, pool)
	if len(rows) != len(steps) {
		t.Fatalf("the trail holds %d rows, want %d", len(rows), len(steps))
	}
	want := []struct {
		kind      string
		elevation string
	}{
		{audit.CredentialKindSession, ""},
		{audit.CredentialKindToken, ""},
		{audit.CredentialKindForm, ""},
		{audit.CredentialKindSession, elevation},
	}
	for i, row := range rows {
		name := steps[i].name
		// 001_initial.sql gives user 1 the administration `admin` role; an
		// `inherit` token acts with its owner's roles.
		if strings.Join(row.ActingRoles, ",") != "admin" {
			t.Errorf("%s: acting_roles = %v, want {admin}", name, row.ActingRoles)
		}
		if row.CredentialKind == nil || *row.CredentialKind != want[i].kind {
			t.Errorf("%s: credential_kind = %v, want %q", name, row.CredentialKind, want[i].kind)
		}
		switch {
		case want[i].elevation == "" && row.ElevationID != nil:
			t.Errorf("%s: elevation_id = %q, want NULL", name, *row.ElevationID)
		case want[i].elevation != "" && (row.ElevationID == nil || *row.ElevationID != want[i].elevation):
			t.Errorf("%s: elevation_id = %v, want %q", name, row.ElevationID, want[i].elevation)
		}
		if row.UserID == nil || *row.UserID != 1 {
			t.Errorf("%s: user_id = %v, want 1", name, row.UserID)
		}
	}
}

// A refused request is an administration-mode row too: the resolution ran,
// found no role, and the row says so with {} rather than NULL.
func TestARefusedAdministrationRequestRecordsAnEmptyRoleSet(t *testing.T) {
	pool := newAuditEmitterPool(t)
	recorder := audit.NewPostgresRecorder(pool, nil)
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
		defer cancel()
		_ = recorder.Close(closeCtx)
	})
	router := asConsoleSession(NewRouter(RouterConfig{
		Pool:               pool,
		SessionSecret:      consoleTestSessionSecret,
		AuthValidator:      testTokenValidator{user: unprivilegedAuditTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		AuditRecorder:      recorder,
	}), unprivilegedAuditTestUser())
	ctx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, name) VALUES (9001, 'nobody@test.local', 'Nobody')
ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/v2/admin/user_suspend/administration/1",
		strings.NewReader(`{"suspended":true}`)))
	if response.Code != http.StatusForbidden {
		t.Fatalf("PUT user_suspend = %d, want 403", response.Code)
	}
	if err := recorder.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rows := readRoleUseRows(t, pool)
	if len(rows) != 1 {
		t.Fatalf("the trail holds %d rows, want 1", len(rows))
	}
	if rows[0].ActingRoles == nil || len(rows[0].ActingRoles) != 0 {
		t.Errorf("acting_roles = %v, want {}", rows[0].ActingRoles)
	}
	if rows[0].CredentialKind == nil || *rows[0].CredentialKind != audit.CredentialKindSession {
		t.Errorf("credential_kind = %v, want session", rows[0].CredentialKind)
	}
}
