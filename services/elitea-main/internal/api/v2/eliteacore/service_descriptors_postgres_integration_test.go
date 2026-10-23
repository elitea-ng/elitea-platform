package eliteacore_test

// Unit A14 acceptance for the admin SERVICE DESCRIPTORS surface (issue #200).
//
// This page is deliberately unavailable, so the assertions are the mirror image
// of the other nine ports: there is no write to read back, and what has to be
// proved is that nothing pretends otherwise.
//
//	1. Every route REFUSES, with a reason — not 200, and not a bare 501 either.
//	   The listing this replaces answered 200 with three hardcoded rows naming
//	   `elitea_core`, `auth` and `indexer`; a status-only assertion would have
//	   passed against it, and so would "the body has a `rows` key".
//	2. The refusal happens with the permission HELD. A 403-for-everyone page
//	   looks identical to an unavailable one from the browser, and would hide a
//	   regression in either direction.
//	3. The gate is real, resolved from `auth_core__user_role` by the production
//	   resolver over real rows — not a stub that returns a fixed list.
//	4. The writes leave the DATABASE UNTOUCHED. Asserted by SQL over the full
//	   table catalogue before and after, because "the handler refused" and "the
//	   handler refused and also did not write" are different claims and the
//	   handler being replaced answered `{"ok": true}` to a discarded body.
//	5. The Configuration page's `service_descriptors` section states the SAME
//	   sentence, byte for byte. Two surfaces reach this subject and an operator
//	   must not be told two different things.
//
// Requires a PostgreSQL to create an isolated database in; skipped otherwise.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	adminapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

/* ── harness ───────────────────────────────────────────────────────────── */

const (
	descriptorListingPath  = "/api/v2/elitea_core/admin/administration"
	descriptorRegisterPath = "/api/v2/elitea_core/register_descriptor/17"
)

// descriptorRouter mounts the three routes exactly as internal/api/router.go
// does, WITH the production permission middleware and the production Postgres
// resolver. Substituting a stub resolver here would leave the thing most likely
// to be wrong — that the permission names match rows a real database holds —
// entirely uncovered.
//
// `administration` is mounted as a STATIC segment, which is the point: a static
// segment binds no `{mode}` parameter, so a handler inferring its mode from
// `chi.URLParam(r, "mode")` would see `""` on precisely the administration
// requests. That trap cost #207 a round.
func descriptorRouter(t *testing.T, pool *pgxpool.Pool, principal auth.User) chi.Router {
	t.Helper()
	handler := eliteacore.NewHandler(pool)
	resolver := legacyrbac.NewPostgresResolver(pool)

	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	// The exported constants, not literals: router.go registers with the same
	// two, so a typo in either place surfaces here as a 403 rather than passing
	// silently against a test that repeated the mistake.
	listing := apimw.RequireCentralPermissions(
		resolver, auth.PermissionModeAdministration, eliteacore.ServiceDescriptorListPermission)
	register := apimw.RequireCentralPermissions(
		resolver, auth.PermissionModeAdministration, eliteacore.ServiceDescriptorRegisterPermission)

	router.Route("/api/v2/elitea_core", func(r chi.Router) {
		r.With(listing).Get("/admin/administration", handler.ServiceDescriptors)
		r.With(register).Post("/register_descriptor/{projectID}", handler.RegisterDescriptor)
		r.With(register).Delete("/register_descriptor/{projectID}", handler.RegisterDescriptor)
	})
	return router
}

func descriptorDo(t *testing.T, router chi.Router, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func descriptorReason(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return body.Error
}

// grantDescriptorPermissions gives user 1 the administration-mode `admin` role
// that 001_initial.sql seeds with both permissions. The migration is the source
// of truth for the grant; this only attaches the caller to it.
func grantDescriptorPermissions(t *testing.T, pool *pgxpool.Pool, userID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
        INSERT INTO auth_core__user (id, email, name) VALUES ($1, 'descriptor-operator@autotest.local', 'Operator')
        ON CONFLICT (id) DO NOTHING`, userID); err != nil {
		t.Fatalf("seed operator: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
        INSERT INTO auth_core__user_role (user_id, role_id)
        SELECT $1, id FROM auth_core__role WHERE name = 'admin' AND mode = 'administration'
        ON CONFLICT (user_id, role_id) DO NOTHING`, userID); err != nil {
		t.Fatalf("grant administration admin role: %v", err)
	}
}

// tableCatalogue is every user table in the database, sorted. The registration
// endpoints have no store; if one ever acquires a silent one, this catches the
// CREATE as surely as it catches the INSERT below.
func tableCatalogue(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
        SELECT table_schema || '.' || table_name
        FROM information_schema.tables
        WHERE table_schema NOT IN ('pg_catalog', 'information_schema')`)
	if err != nil {
		t.Fatalf("read table catalogue: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table catalogue: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table catalogue: %v", err)
	}
	sort.Strings(names)
	return names
}

// totalRowsIn counts every row of every table named. Used across the write
// attempt so an INSERT into an EXISTING table is caught too.
func totalRowsIn(t *testing.T, pool *pgxpool.Pool, tables []string) int64 {
	t.Helper()
	var total int64
	for _, table := range tables {
		var count int64
		// The identifiers come from information_schema in this same database,
		// not from a request.
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		total += count
	}
	return total
}

/* ── the refusals ──────────────────────────────────────────────────────── */

// TestServiceDescriptorRoutesRefuseWithTheReason is the assertion the three
// hardcoded rows cannot pass, and neither can a bare 501.
func TestServiceDescriptorRoutesRefuseWithTheReason(t *testing.T) {
	pool := newAuditPool(t)
	grantDescriptorPermissions(t, pool, 1)
	router := descriptorRouter(t, pool, auth.User{ID: "1", UserID: "1", Email: "descriptor-operator@autotest.local"})

	for _, probe := range []struct {
		method string
		target string
		body   any
	}{
		{http.MethodGet, descriptorListingPath, nil},
		{http.MethodPost, descriptorRegisterPath, map[string]any{
			"name":                 "ImageGenServiceProvider",
			"service_location_url": "https://provider.example.com/imagegen",
			"configuration":        map[string]any{},
			"provided_toolkits":    []any{},
		}},
		{http.MethodDelete, descriptorRegisterPath +
			"?provider_name=ImageGenServiceProvider&service_location_url=https://provider.example.com/imagegen", nil},
	} {
		recorder := descriptorDo(t, router, probe.method, probe.target, probe.body)
		if recorder.Code != http.StatusNotImplemented {
			t.Errorf("%s %s status = %d, want 501 (body %s)",
				probe.method, probe.target, recorder.Code, recorder.Body.String())
			continue
		}
		reason := descriptorReason(t, recorder)
		if reason != eliteacore.ServiceDescriptorsUnavailableReason {
			t.Errorf("%s %s reason = %q, want the declared reason", probe.method, probe.target, reason)
		}
	}
}

// TestServiceDescriptorListingReturnsNoRows pins the specific regression. The
// handler this replaces answered 200 with a `rows` array of three objects whose
// keys (`name`, `status`, `version`, `description`) are not even this endpoint's
// keys — the client reads `project_id`, `provider_name`, `service_location_url`
// and `healthy`. An operator reading that listing would have concluded the
// platform had three healthy providers registered.
func TestServiceDescriptorListingReturnsNoRows(t *testing.T) {
	pool := newAuditPool(t)
	grantDescriptorPermissions(t, pool, 1)
	router := descriptorRouter(t, pool, auth.User{ID: "1", UserID: "1"})

	recorder := descriptorDo(t, router, http.MethodGet, descriptorListingPath, nil)

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	if _, present := body["rows"]; present {
		t.Errorf("the refusal carries a rows array: %s", recorder.Body.String())
	}
	// The invented rows' own fields, not their values: the REASON legitimately
	// names `elitea_core` (it is the plugin that owns the legacy endpoint), so a
	// substring test on the plugin name would fail against the correct answer.
	for _, invented := range []string{`"status"`, `"version"`, `"2.0.0"`, `"Core platform service"`} {
		if bytes.Contains(recorder.Body.Bytes(), []byte(invented)) {
			t.Errorf("the hardcoded plugin rows are still being served (%s): %s",
				invented, recorder.Body.String())
		}
	}
}

// TestServiceDescriptorWritesLeaveTheDatabaseUntouched — "it refused" and "it
// refused and wrote nothing" are different claims. The handler being replaced
// decoded its POST body and answered `{"ok": true}`; a future one that started
// storing descriptors somewhere while still answering 501 would be a silent
// half-implementation.
func TestServiceDescriptorWritesLeaveTheDatabaseUntouched(t *testing.T) {
	pool := newAuditPool(t)
	grantDescriptorPermissions(t, pool, 1)
	router := descriptorRouter(t, pool, auth.User{ID: "1", UserID: "1"})

	before := tableCatalogue(t, pool)
	rowsBefore := totalRowsIn(t, pool, before)

	descriptorDo(t, router, http.MethodPost, descriptorRegisterPath, map[string]any{
		"name":                 "inventory",
		"service_location_url": "http://provider.example.com/inventory",
		"configuration":        map[string]any{"auth_type": "none"},
		"provided_toolkits":    []any{},
	})
	descriptorDo(t, router, http.MethodDelete, descriptorRegisterPath+
		"?provider_name=inventory&service_location_url=http://provider.example.com/inventory", nil)

	after := tableCatalogue(t, pool)
	if len(before) != len(after) {
		t.Fatalf("the table catalogue changed across a refused registration: %v -> %v", before, after)
	}
	for index := range before {
		if before[index] != after[index] {
			t.Fatalf("the table catalogue changed across a refused registration: %v -> %v", before, after)
		}
	}
	if rowsAfter := totalRowsIn(t, pool, after); rowsAfter != rowsBefore {
		t.Errorf("a refused registration wrote %d row(s)", rowsAfter-rowsBefore)
	}
}

/* ── the gate ──────────────────────────────────────────────────────────── */

// TestServiceDescriptorRoutesRefuseACallerWithoutThePermission — the gate runs
// BEFORE the refusal, so an unauthorised caller does not learn whether this
// deployment serves the surface at all. Which subsystems a deployment runs is
// itself information about it.
//
// The permissions are resolved from real `auth_core__user_role` rows by the
// production resolver, so this also pins that the strings in router.go match the
// strings the migration grants — a typo in either would show up here as a 403 in
// the case below, and as a 403 in the 501 tests above.
func TestServiceDescriptorRoutesRefuseACallerWithoutThePermission(t *testing.T) {
	pool := newAuditPool(t)
	// User 9 exists and holds NO administration role.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO auth_core__user (id, email, name) VALUES (9, 'nobody@autotest.local', 'Nobody')
         ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed unprivileged user: %v", err)
	}
	router := descriptorRouter(t, pool, auth.User{ID: "9", UserID: "9", Email: "nobody@autotest.local"})

	for _, probe := range []struct {
		method string
		target string
	}{
		{http.MethodGet, descriptorListingPath},
		{http.MethodPost, descriptorRegisterPath},
		{http.MethodDelete, descriptorRegisterPath},
	} {
		recorder := descriptorDo(t, router, probe.method, probe.target, map[string]any{})
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s %s status = %d, want 403 for a caller without the permission (body %s)",
				probe.method, probe.target, recorder.Code, recorder.Body.String())
		}
		if bytes.Contains(recorder.Body.Bytes(), []byte("provider hub")) {
			t.Errorf("%s %s disclosed the availability reason to an unauthorised caller: %s",
				probe.method, probe.target, recorder.Body.String())
		}
	}
}

// TestServiceDescriptorListingHasNoOtherMode — the legacy runtime registers
// `administration` and nothing else on this path (`mode_handlers =
// {'administration': AdminAPI}`). The route it replaces was `/admin/{mode}`,
// which answered its three invented rows to any mode a caller cared to name.
func TestServiceDescriptorListingHasNoOtherMode(t *testing.T) {
	pool := newAuditPool(t)
	grantDescriptorPermissions(t, pool, 1)
	router := descriptorRouter(t, pool, auth.User{ID: "1", UserID: "1"})

	for _, mode := range []string{"prompt_lib", "default", "developer"} {
		recorder := descriptorDo(t, router, http.MethodGet, "/api/v2/elitea_core/admin/"+mode, nil)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("GET /elitea_core/admin/%s status = %d, want 404 (body %s)",
				mode, recorder.Code, recorder.Body.String())
		}
	}
}

/* ── one reason, two surfaces ──────────────────────────────────────────── */

// TestConfigurationSectionStatesTheSameReason — the admin Configuration page has
// a `service_descriptors` section, and an operator can arrive at this subject
// from either surface. Before this unit the section said "a page of their own in
// the admin port (issue #200)", which pointed at a page that did not exist; a
// pointer to a page that now says something DIFFERENT would be worse.
// The Configuration section defers to the page and does NOT repeat its refusal.
//
// This test used to assert the two strings were identical, and that was right
// while the page refused: an operator following the pointer had to be told the
// same thing at the other end. Migration 0107 made the page answer, so equality
// became the bug — the section would claim there is no descriptor store while
// the page it points at lists one.
//
// What is asserted now is the property equality was standing in for: the section
// names the page, and it does not carry the refusal.
func TestConfigurationSectionDefersToTheDescriptorsPage(t *testing.T) {
	pool := newAuditPool(t)
	principal := auth.User{ID: "1", UserID: "1"}
	handler := adminapi.NewHandler(pool)

	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.Get("/admin/plugin_config_schemas/{mode}", handler.PluginConfigSchemas)

	recorder := descriptorDo(t, router, http.MethodGet, "/admin/plugin_config_schemas/administration", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("schemas status = %d, want 200", recorder.Code)
	}
	var schema struct {
		Sections []struct {
			ID                string `json:"id"`
			UnavailableReason string `json:"unavailable_reason"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &schema); err != nil {
		t.Fatalf("decode schemas: %v", err)
	}
	for _, section := range schema.Sections {
		if section.ID != "service_descriptors" {
			continue
		}
		if section.UnavailableReason == "" {
			t.Fatal("the Configuration section explains nothing; an operator reaching this subject from " +
				"Configuration has to be told where it is administered")
		}
		if section.UnavailableReason == eliteacore.ServiceDescriptorsUnavailableReason {
			t.Fatal("the Configuration section still carries the endpoints' REFUSAL. That sentence says this " +
				"deployment has no descriptor store, and since migration 0107 the page it points at lists one.")
		}
		if !strings.Contains(section.UnavailableReason, "/admin/app/service-descriptors") {
			t.Fatalf("the Configuration section does not name the page that administers this:\n  %q",
				section.UnavailableReason)
		}
		return
	}
	t.Fatal("the Configuration schema no longer declares a service_descriptors section")
}
