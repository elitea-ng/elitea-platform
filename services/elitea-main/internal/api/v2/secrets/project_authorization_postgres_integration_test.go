package secrets

// Acceptance for the PROJECT secret vault's authorisation (handler.go).
//
// Before this, `Routes()` registered all seven project routes bare. Only
// authentication stood in front of them, so ANY authenticated caller could name
// ANY `{projectID}` in the path and read that project's secret VALUES in
// plaintext — the mode-ful GET and the mode-less SDK GET both return
// `SecretDetail.Value` — and could create, edit, delete and hide them.
//
// The failure mode this file exists to make impossible is therefore not "the
// endpoint 500s". It is "the endpoint answered 200 to someone who should not
// have been able to ask". So every test here proves two things per route, not
// one:
//
//  1. The caller is REFUSED — 403, through the handler's real `Routes()`, with
//     the real mode dispatch and the real in-package gate. Nothing here
//     re-implements the routing, so a test cannot pass against a route table the
//     server does not have.
//  2. NOTHING MOVED AND NOTHING LEAKED. Reads are checked against the response
//     body: a refused list must not name the secret, a refused reveal must not
//     carry its value. Writes are checked against the raw stored blobs, so
//     "unchanged" is independent of any handler that might be reading the wrong
//     row.
//
// The cross-project case is the reported vulnerability stated directly: a caller
// fully permitted on their OWN project is refused on someone else's, which is
// only true if the gate resolves the `{projectID}` in the PATH rather than some
// ambient project.
//
// No test asserts on a secret value that means anything; the fixtures are
// marker strings.
//
// Requires a PostgreSQL to create an isolated database in; skipped otherwise,
// like every other *_postgres_integration_test.go in this service.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

/* ── fixtures ──────────────────────────────────────────────────────────── */

// Marker values. None of these is or resembles a credential.
//
// `residentName`/`residentValue` and `probeName`/`probeValue` are shared with
// admin_postgres_integration_test.go, as are the `permissionResolverFunc`,
// `grantingResolver`, `secretsRouter`, `do`, `vaultRowDigest` and
// `newSecretsPool` helpers — same package, same meaning, deliberately not
// duplicated.
const (
	// The project under test. `residentName` lives in it before every test, so
	// "the vault still has its contents" is assertable after a refusal.
	ownedProject = "1"

	// A SECOND project, owned by someone else. This is the one the reported
	// vulnerability let any authenticated caller read.
	otherProject      = "2"
	otherProjectName  = "other_project_marker"
	otherProjectValue = "marker-other-project"
)

/* ── harness ───────────────────────────────────────────────────────────── */

// allProjectSecretPermissions is the full set the legacy ProjectAPI declares —
// what a `default`-mode admin/editor holds on the reference deployment.
func allProjectSecretPermissions() []string {
	return []string{
		permSecretList, permSecretCreate, permSecretUnsecret,
		permSecretEdit, permSecretDelete, permSecretHide,
	}
}

// grantingResolverForProject answers with the permissions given ONLY for one
// project id, and with nothing for every other — a real RBAC table's shape.
// The gate must consult the project id in the PATH for this to bite.
func grantingResolverForProject(grantedProjectID string, permissions ...string) permissionResolverFunc {
	return func(_ context.Context, _ auth.User, _ string, projectID string) (auth.PermissionResolution, error) {
		if projectID != grantedProjectID {
			return auth.PermissionResolution{UserID: 1, Permissions: []string{}}, nil
		}
		return auth.PermissionResolution{UserID: 1, Permissions: permissions}, nil
	}
}

// withoutPermission is every project secret permission EXCEPT one. A caller
// refused under this resolver is refused because of the permission under test
// and not because they were handed an empty set.
func withoutPermission(excluded string) permissionResolverFunc {
	kept := make([]string, 0, len(allProjectSecretPermissions()))
	for _, permission := range allProjectSecretPermissions() {
		if permission != excluded {
			kept = append(kept, permission)
		}
	}
	return grantingResolver(kept...)
}

/* ── the route table ───────────────────────────────────────────────────── */

// projectRoute is one route of the seven, paired with the permission the legacy
// runtime declares for it and a request that exercises it against `projectID`.
type projectRoute struct {
	name       string
	permission string
	method     string
	path       func(projectID string) string
	body       any
	// writes is false for the two reads, whose "nothing happened" evidence is
	// the response body rather than the stored blob.
	writes bool
}

func projectRoutes() []projectRoute {
	return []projectRoute{
		{
			name:       "list",
			permission: permSecretList,
			method:     http.MethodGet,
			path:       func(id string) string { return "/secrets/secrets/default/" + id },
		},
		{
			name:       "create",
			permission: permSecretCreate,
			method:     http.MethodPost,
			path:       func(id string) string { return "/secrets/secrets/default/" + id },
			body:       map[string]string{"name": probeName, "value": probeValue},
			writes:     true,
		},
		{
			name:       "get (plaintext)",
			permission: permSecretUnsecret,
			method:     http.MethodGet,
			path:       func(id string) string { return "/secrets/secret/default/" + id + "/" + residentName },
		},
		{
			// The mode-LESS variant elitea-sdk calls. In the legacy runtime
			// this IS the mode-ful GET (with_modes + proxy_method's
			// mode='default' default), so it carries the same permission.
			name:       "get (plaintext, mode-less — the elitea-sdk route)",
			permission: permSecretUnsecret,
			method:     http.MethodGet,
			path:       func(id string) string { return "/secrets/secret/" + id + "/" + residentName },
		},
		{
			name:       "update",
			permission: permSecretEdit,
			method:     http.MethodPut,
			path:       func(id string) string { return "/secrets/secret/default/" + id + "/" + residentName },
			body:       map[string]string{"name": residentName, "value": probeValue},
			writes:     true,
		},
		{
			name:       "delete",
			permission: permSecretDelete,
			method:     http.MethodDelete,
			path:       func(id string) string { return "/secrets/secret/default/" + id + "/" + residentName },
			writes:     true,
		},
		{
			name:       "hide",
			permission: permSecretHide,
			method:     http.MethodPost,
			path:       func(id string) string { return "/secrets/hide/default/" + id + "/" + residentName },
			writes:     true,
		},
	}
}

/* ── the refusals ──────────────────────────────────────────────────────── */

// Every route refuses a caller who holds every OTHER secret permission but not
// this route's, and neither reads nor writes anything on the way out.
func TestProjectSecretsRefuseACallerWithoutTheRoutePermission(t *testing.T) {
	pool := newSecretsPool(t)
	prepareProjectVaults(t, pool)

	for _, route := range projectRoutes() {
		t.Run(route.name, func(t *testing.T) {
			router := secretsRouter(t, pool, withoutPermission(route.permission))
			before := vaultRowDigest(t, pool, dbKey(ownedProject))

			recorder := do(t, router, route.method, route.path(ownedProject), route.body)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("%s %s without %s status = %d, want 403 (body %s)",
					route.method, route.path(ownedProject), route.permission,
					recorder.Code, recorder.Body.String())
			}

			// Nothing was READ: the refusal body carries neither the secret's
			// value nor, for the listing, even its name.
			assertNoSecretMaterial(t, recorder.Body.String())

			// Nothing was WRITTEN: the stored blobs are byte-identical.
			if after := vaultRowDigest(t, pool, dbKey(ownedProject)); after != before {
				t.Fatalf("a refused %s changed the vault anyway", route.name)
			}
		})
	}

	// And the vault is still intact and readable afterwards, through the
	// product's own routes, under a fully-permitted caller.
	open := secretsRouter(t, pool, grantingResolver(allProjectSecretPermissions()...))
	if value, found := readSecret(t, open, ownedProject, residentName); !found || value != residentValue {
		t.Fatalf("the resident secret changed under refused calls: (%q, %v)", value, found)
	}
	if _, found := readSecret(t, open, ownedProject, probeName); found {
		t.Fatalf("a refused create wrote the secret anyway")
	}
}

// The reported vulnerability, stated directly: a caller fully permitted on
// their OWN project may not touch someone else's by naming it in the path.
func TestProjectSecretsRefuseACallerPermittedOnADifferentProject(t *testing.T) {
	pool := newSecretsPool(t)
	prepareProjectVaults(t, pool)

	// Fully permitted on project 1, nothing at all on project 2.
	router := secretsRouter(t, pool,
		grantingResolverForProject(ownedProject, allProjectSecretPermissions()...))

	for _, route := range projectRoutes() {
		t.Run(route.name, func(t *testing.T) {
			before := vaultRowDigest(t, pool, dbKey(otherProject))

			recorder := do(t, router, route.method, route.path(otherProject), route.body)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("%s against project %s status = %d, want 403 — a caller "+
					"permitted only on project %s reached another project's vault (body %s)",
					route.method, otherProject, recorder.Code, ownedProject, recorder.Body.String())
			}
			if body := recorder.Body.String(); strings.Contains(body, otherProjectValue) ||
				strings.Contains(body, otherProjectName) {
				t.Fatalf("a refused cross-project %s disclosed the other project's secret: %s",
					route.name, body)
			}
			if after := vaultRowDigest(t, pool, dbKey(otherProject)); after != before {
				t.Fatalf("a refused cross-project %s changed the other project's vault", route.name)
			}
		})
	}

	// The same caller still works on the project they ARE permitted on, so the
	// gate is scoping by project rather than refusing everything.
	if code := do(t, router, http.MethodGet, "/secrets/secrets/default/"+ownedProject, nil).Code; code != http.StatusOK {
		t.Fatalf("listing the caller's OWN project status = %d, want 200", code)
	}
}

// A Handler built with no resolver at all — the two programmatic constructors
// in applications/ and conversations/ — must expose nothing over HTTP.
func TestProjectSecretsFailClosedWithoutAResolver(t *testing.T) {
	pool := newSecretsPool(t)
	prepareProjectVaults(t, pool)
	router := secretsRouter(t, pool, nil)

	for _, route := range projectRoutes() {
		t.Run(route.name, func(t *testing.T) {
			recorder := do(t, router, route.method, route.path(ownedProject), route.body)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("%s with no resolver status = %d, want 403", route.name, recorder.Code)
			}
			assertNoSecretMaterial(t, recorder.Body.String())
		})
	}
}

// The counterpart every refusal test needs: with the permission, each route
// still does its job. Without this, a gate that refused unconditionally would
// pass every test above.
func TestProjectSecretsAllowACallerHoldingTheRoutePermission(t *testing.T) {
	pool := newSecretsPool(t)
	prepareProjectVaults(t, pool)
	router := secretsRouter(t, pool, grantingResolver(allProjectSecretPermissions()...))

	if code := do(t, router, http.MethodPost, "/secrets/secrets/default/"+ownedProject,
		map[string]string{"name": probeName, "value": probeValue}).Code; code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if value, found := readSecret(t, router, ownedProject, probeName); !found || value != probeValue {
		t.Fatalf("after create, GET returned (%q, %v); want the value that was written", value, found)
	}
	// The mode-less SDK route reveals it too — the runtime's `unsecret()` call.
	if value, found := readSecretModeless(t, router, ownedProject, probeName); !found || value != probeValue {
		t.Fatalf("the mode-less SDK route returned (%q, %v); the runtime cannot resolve secrets", value, found)
	}
	if code := do(t, router, http.MethodPost,
		"/secrets/hide/default/"+ownedProject+"/"+probeName, nil).Code; code != http.StatusOK {
		t.Fatalf("hide status = %d, want 200", code)
	}
	if code := do(t, router, http.MethodDelete,
		"/secrets/secret/default/"+ownedProject+"/"+probeName, nil).Code; code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", code)
	}
}

// Gating must not have swallowed the mode dispatch, and the two modes must keep
// SEPARATE gates rather than one gate in front of both.
//
// The project gate is passed as withModes' `project` branch, not wrapped around
// it, so:
//   - an invented mode is still a 404, not a 403 that explains nothing;
//   - `administration` is authorised by adminGate against the CENTRAL
//     permissions (it ignores the {projectID} entirely), so a caller holding
//     every PROJECT permission and no administration one is refused there — and
//     a caller holding the administration ones is served, from the global vault.
//
// That second pair is what proves the gates are not shared: one resolver, two
// different answers on the same URL shape.
func TestProjectSecretsModeDispatchSurvivesTheGate(t *testing.T) {
	pool := newSecretsPool(t)
	prepareProjectVaults(t, pool)

	projectOnly := secretsRouter(t, pool, grantingResolver(allProjectSecretPermissions()...))

	// An invented mode is a 404 before any permission question is asked.
	if code := do(t, projectOnly, http.MethodGet,
		"/secrets/secrets/prompt_lib/"+ownedProject, nil).Code; code != http.StatusNotFound {
		t.Errorf("GET an invented mode status = %d, want 404", code)
	}
	// Every project permission does not buy the administration surface.
	if code := do(t, projectOnly, http.MethodGet,
		"/secrets/secrets/administration/"+ownedProject, nil).Code; code != http.StatusForbidden {
		t.Errorf("GET administration with only project permissions status = %d, want 403", code)
	}
	// And the administration permission does not buy the project surface.
	adminOnly := secretsRouter(t, pool, grantingResolver(permSecretView))
	if code := do(t, adminOnly, http.MethodGet,
		"/secrets/secrets/default/"+ownedProject, nil).Code; code != http.StatusForbidden {
		t.Errorf("GET the project listing with only the admin view permission status = %d, want 403", code)
	}
	if code := do(t, adminOnly, http.MethodGet,
		"/secrets/secrets/administration/"+ownedProject, nil).Code; code != http.StatusOK {
		t.Errorf("GET administration with the admin view permission status = %d, want 200", code)
	}
}

/* ── assertions ────────────────────────────────────────────────────────── */

// assertNoSecretMaterial fails if a refusal body carries anything from the
// vault. A 403 that still returned the value would satisfy a status-only test.
func assertNoSecretMaterial(t *testing.T, body string) {
	t.Helper()
	for _, leaked := range []string{residentValue, residentName, otherProjectValue, otherProjectName} {
		if strings.Contains(body, leaked) {
			t.Fatalf("a refusal body disclosed vault contents (%q): %s", leaked, body)
		}
	}
}

/* ── product-level reads ───────────────────────────────────────────────── */

// readSecret re-reads one secret through the PRODUCT's mode-ful GET.
func readSecret(t *testing.T, router chi.Router, projectID, name string) (string, bool) {
	t.Helper()
	return decodeSecretDetail(t, router, "/secrets/secret/default/"+projectID+"/"+name)
}

// readSecretModeless re-reads through the mode-LESS GET — elitea-sdk's route.
func readSecretModeless(t *testing.T, router chi.Router, projectID, name string) (string, bool) {
	t.Helper()
	return decodeSecretDetail(t, router, "/secrets/secret/"+projectID+"/"+name)
}

func decodeSecretDetail(t *testing.T, router chi.Router, path string) (string, bool) {
	t.Helper()
	recorder := do(t, router, http.MethodGet, path, nil)
	if recorder.Code == http.StatusNotFound {
		return "", false
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200 or 404 (body %s)", path, recorder.Code, recorder.Body.String())
	}
	var body SecretDetail
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s body %q: %v", path, recorder.Body.String(), err)
	}
	return body.Value, true
}

// prepareProjectVaults seeds the caller's own project vault and a SECOND project's,
// through the handler's own writers, so the fixtures are in exactly the format
// the product reads.
func prepareProjectVaults(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	handler := NewHandler(pool)

	// The other project holds `residentName` TOO, under a different value.
	// That is deliberate: it means the cross-project requests below address a
	// secret that really exists over there, so an ungated route answers by
	// disclosing or mutating it rather than incidentally 404ing. Without this,
	// several of those subtests would pass for the wrong reason.
	for projectID, secrets := range map[string]map[string]string{
		ownedProject: {residentName: residentValue},
		otherProject: {residentName: otherProjectValue, otherProjectName: otherProjectValue},
	} {
		if err := handler.writeVaultCtx(ctx, projectID, vaultData{
			Secrets:       secrets,
			HiddenSecrets: map[string]string{},
		}); err != nil {
			t.Fatalf("seed project %s vault: %v", projectID, err)
		}
	}
}
