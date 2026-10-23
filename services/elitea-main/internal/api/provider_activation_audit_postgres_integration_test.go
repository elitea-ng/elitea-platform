package api

// ACTIVATION IS AUDITED — through the production router, against a real
// database (ADR-0012 phase P3, migration 0109).
//
// Putting a provider in force is the single most consequential action this
// administration surface offers: after it, agents can call an external service.
// `/api/v2/elitea_core/register_descriptor/` is already an audited prefix, so
// the new routes are covered by construction — and "by construction" is exactly
// the kind of claim this repository has been wrong about before, because a
// prefix that stops matching audits nothing and reads identically to a surface
// with no traffic (the check-playwright-image-tag shape). So the row is read
// back out of the trail's own endpoint rather than assumed.
//
// The ROUTE PATTERN is asserted, not the target. A raw target would put the
// project id in a column the page groups and sorts on.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhub"
)

// admissionAuditPool is the emitter harness's database with the admission plane
// and its overlay issuer applied. 0109 also carries the grant that lets user 1
// — the administration `admin` 001_initial.sql seeds — reach the route at all,
// so applying the real file rather than an ad-hoc GRANT is what makes this test
// measure the migration.
func admissionAuditPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newAuditEmitterPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
	defer cancel()
	for _, name := range []string{
		"0107_provider_admitted_revisions.sql",
		"0109_provider_policy_overlay.sql",
	} {
		sql, err := os.ReadFile(filepath.Join("..", "..", "migrations", "shared", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	return pool
}

func TestActivatingAProviderWritesAnAuditRow(t *testing.T) {
	pool := admissionAuditPool(t)
	recorder := audit.NewPostgresRecorder(pool, nil)
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
		defer cancel()
		if err := recorder.Close(closeCtx); err != nil {
			t.Errorf("close the audit recorder: %v", err)
		}
	})

	// Registered through the store rather than through a first HTTP call, so
	// the trail holds exactly one row and the assertion below cannot be
	// satisfied by the registration's own audit event.
	ctx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
	defer cancel()
	admitted, err := providerhub.Register(ctx, pool, providerhub.Registration{
		ProjectID:  17,
		ProviderID: "deepwiki",
		Origin:     "https://elitea-deepwiki:8443",
		Manifest:   []byte(`{"name":"deepwiki","provided_toolkits":[{"name":"wikis"}]}`),
		Actor:      "boot-registrar",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	router := NewRouter(RouterConfig{
		Pool:               pool,
		AuthValidator:      testTokenValidator{user: privilegedAuditTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		AuditRecorder:      recorder,
	})

	response := httptest.NewRecorder()
	request := testAuthHeader(httptest.NewRequest(http.MethodPost,
		"/api/v2/elitea_core/register_descriptor/17/activate?provider_name=deepwiki",
		strings.NewReader(`{"expected_digest":"`+admitted.ManifestDigest+
			`","reason":"reviewed the wikis toolkit","overlay":{"egress_profile":"provider-only"}}`)))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("POST activate = %d, want 200: %s", response.Code, response.Body.String())
	}

	flushCtx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
	defer cancel()
	if err := recorder.Flush(flushCtx); err != nil {
		t.Fatalf("flush the audit recorder: %v", err)
	}
	if dropped := recorder.Dropped(); dropped != 0 {
		t.Fatalf("the recorder dropped %d events", dropped)
	}

	listing := readEmittedTrail(t, pool)
	if len(listing.Rows) != 1 {
		t.Fatalf("the trail holds %d rows, want 1 for the activation", len(listing.Rows))
	}
	row := listing.Rows[0]
	const wantRoute = "/api/v2/elitea_core/register_descriptor/{projectID}/activate"
	if row.HTTPRoute == nil || *row.HTTPRoute != wantRoute {
		t.Errorf("http_route = %v, want %q — a raw target would put the project id "+
			"in a column the page groups on", row.HTTPRoute, wantRoute)
	}
	if row.Action != "POST "+wantRoute {
		t.Errorf("action = %q, want the route-level fallback for an unannotated handler", row.Action)
	}
	if row.UserEmail == nil || *row.UserEmail != "admin@test.local" {
		t.Errorf("user_email = %v, want the acting operator named", row.UserEmail)
	}
	if row.StatusCode == nil || *row.StatusCode != http.StatusOK {
		t.Errorf("status_code = %v, want 200", row.StatusCode)
	}
	if row.ProjectID == nil || *row.ProjectID != 17 {
		t.Errorf("project_id = %v, want 17 from the {projectID} parameter", row.ProjectID)
	}
	if row.IsError {
		t.Error("is_error = true on a successful activation")
	}
}

// THE PRODUCTION ROUTER gates activation on its own permission.
//
// WHY THIS TEST EXISTS SEPARATELY FROM THE ACCEPTANCE SUITE. The eliteacore
// acceptance test builds its own chi router with the two gates spelled out, so
// it measures that the two permission STRINGS differ in a database — and it
// would pass unchanged if internal/api/router.go reused
// `requireDescriptorRegister` for the activate routes, which is one keystroke
// away and is exactly the mistake the split exists to prevent. Only a test that
// drives NewRouter can see it.
//
// The grant 0109 issues is REMOVED here, leaving `.register` in place. A caller
// who can still register and can no longer activate is the whole claim.
func TestTheProductionRouterGatesActivationOnItsOwnPermission(t *testing.T) {
	pool := admissionAuditPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), auditEmitterDeadline)
	defer cancel()

	if _, err := pool.Exec(ctx, `
DELETE FROM auth_core__role_permission
 WHERE permission = 'provider_hub.descriptor.activate'`); err != nil {
		t.Fatalf("withdraw the activate grant: %v", err)
	}

	router := NewRouter(RouterConfig{
		Pool:               pool,
		AuthValidator:      testTokenValidator{user: privilegedAuditTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})

	// REGISTRATION STILL WORKS, which is what makes the refusal below a
	// statement about the permission rather than about a broken principal.
	registered := httptest.NewRecorder()
	registerRequest := testAuthHeader(httptest.NewRequest(http.MethodPost,
		"/api/v2/elitea_core/register_descriptor/17",
		strings.NewReader(`{"provider_name":"deepwiki","service_location_url":"https://elitea-deepwiki:8443",`+
			`"descriptor":{"name":"deepwiki"}}`)))
	registerRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(registered, registerRequest)
	if registered.Code != http.StatusAccepted {
		t.Fatalf("POST register_descriptor = %d for a holder of .register, want 202: %s",
			registered.Code, registered.Body.String())
	}

	for _, verb := range []string{"activate", "deactivate"} {
		response := httptest.NewRecorder()
		request := testAuthHeader(httptest.NewRequest(http.MethodPost,
			"/api/v2/elitea_core/register_descriptor/17/"+verb+"?provider_name=deepwiki",
			strings.NewReader(`{"expected_digest":"`+strings.Repeat("a", 64)+`","reason":"x"}`)))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Errorf("POST %s = %d for a holder of .register alone, want 403: recording a "+
				"descriptor and putting it in force must be separately grantable (%s)",
				verb, response.Code, response.Body.String())
		}
	}
}
