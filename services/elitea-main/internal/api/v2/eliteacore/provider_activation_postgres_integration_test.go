package eliteacore_test

// The ACTIVATION routes, with 0107 and 0109 applied and the production
// permission middleware in front of them (ADR-0012 phase P3).
//
// The assertion that matters most here is the PERMISSION SPLIT. Activation is
// gated on `provider_hub.descriptor.activate` and not on
// `provider_hub.descriptor.register`, because every facade registrar files a
// registration at boot while activation is the switch that lets agents call the
// provider. A split nothing asserts is a split the next composition loses by
// reusing the middleware variable one line above, which is exactly the shape
// router.go had before these routes existed — so a holder of `.register` alone
// is measured getting 403.
//
// The rest is the status vocabulary, and each code is a different thing for the
// operator to do: 404 fix the name, 409 look at the state, 422 re-read the
// manifest that changed, 501 apply the migration. One code for all of them
// would send every one of those to the same unhelpful place.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

const (
	activatePath   = "/api/v2/elitea_core/register_descriptor/17/activate?provider_name=deepwiki"
	deactivatePath = "/api/v2/elitea_core/register_descriptor/17/deactivate?provider_name=deepwiki"
)

// overlayPool is admissionPool with 0109 on top — the migration that gives this
// deployment an overlay issuer at all.
func overlayPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := admissionPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sql, err := os.ReadFile(filepath.Join(
		"..", "..", "..", "..", "migrations", "shared", "0109_provider_policy_overlay.sql"))
	if err != nil {
		t.Fatalf("read 0109: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply 0109: %v", err)
	}
	return pool
}

// activationRouter mounts all five verbs the way internal/api/router.go does,
// with the production resolver and the two DIFFERENT gates. Collapsing them
// onto one middleware here would make the permission-split test measure the
// test's own wiring instead of production's.
func activationRouter(t *testing.T, pool *pgxpool.Pool, principal auth.User) chi.Router {
	t.Helper()
	handler := eliteacore.NewHandler(pool)
	resolver := legacyrbac.NewPostgresResolver(pool)

	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	register := apimw.RequireCentralPermissions(
		resolver, auth.PermissionModeAdministration, eliteacore.ServiceDescriptorRegisterPermission)
	activate := apimw.RequireCentralPermissions(
		resolver, auth.PermissionModeAdministration, eliteacore.ServiceDescriptorActivatePermission)
	listing := apimw.RequireCentralPermissions(
		resolver, auth.PermissionModeAdministration, eliteacore.ServiceDescriptorListPermission)

	router.Route("/api/v2/elitea_core", func(r chi.Router) {
		r.With(listing).Get("/admin/administration", handler.ServiceDescriptors)
		r.With(register).Post("/register_descriptor/{projectID}", handler.RegisterDescriptor)
		r.With(register).Delete("/register_descriptor/{projectID}", handler.RegisterDescriptor)
		r.With(activate).Post("/register_descriptor/{projectID}/activate", handler.ActivateDescriptor)
		r.With(activate).Post("/register_descriptor/{projectID}/deactivate", handler.DeactivateDescriptor)
	})
	return router
}

func activationRouterFor(t *testing.T, pool *pgxpool.Pool) chi.Router {
	t.Helper()
	return activationRouter(t, pool, auth.User{ID: "1", UserID: "1", Email: admissionOperator})
}

func postActivation(t *testing.T, router http.Handler, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// registeredDigest posts the sample descriptor and returns the manifest digest
// the operator would have read off the listing.
func registeredDigest(t *testing.T, router http.Handler) string {
	t.Helper()
	recorder := postDescriptor(t, router, sampleDescriptor)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("registration failed: %d (%s)", recorder.Code, recorder.Body.String())
	}
	var response struct {
		PublishedManifestDigest string `json:"published_manifest_digest"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("registration body: %v", err)
	}
	return response.PublishedManifestDigest
}

func activationBody(digest, reason, overlay string) string {
	encodedReason, err := json.Marshal(reason)
	if err != nil {
		panic(err)
	}
	return `{"expected_digest":"` + digest + `","reason":` + string(encodedReason) +
		`,"overlay":` + overlay + `}`
}

/* ── the happy path ─────────────────────────────────────────────────────── */

func TestActivationPutsTheProviderInForceAndTheListingSaysSo(t *testing.T) {
	pool := overlayPool(t)
	router := activationRouterFor(t, pool)
	digest := registeredDigest(t, router)

	recorder := postActivation(t, router, activatePath,
		activationBody(digest, "reviewed the wikis toolkit", `{"egress_profile":"provider-only"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("activate = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	var response struct {
		OverlayRevision         string `json:"overlay_revision"`
		PublishedManifestDigest string `json:"published_manifest_digest"`
		Status                  string `json:"status"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("activate body: %v", err)
	}
	if response.Status != "active" {
		t.Errorf("status = %q, want active", response.Status)
	}
	// The response names the overlay that was issued. Without it a caller has
	// no handle on the policy their click put in force.
	if len(response.OverlayRevision) != 36 || response.OverlayRevision[:4] != "lpo_" {
		t.Errorf("overlay_revision = %q, want an lpo_ id", response.OverlayRevision)
	}
	// THE DIGEST DID NOT MOVE. Activation is not a republication.
	if response.PublishedManifestDigest != digest {
		t.Errorf("digest = %q, want %q unchanged by the activation",
			response.PublishedManifestDigest, digest)
	}

	rows := readDescriptorListing(t, router)
	if len(rows) != 1 || rows[0].Status != "active" {
		t.Fatalf("listing = %+v, want one active row", rows)
	}
	if rows[0].ManifestDigest == nil || *rows[0].ManifestDigest != digest {
		t.Errorf("listing digest = %v, want %q", rows[0].ManifestDigest, digest)
	}

	// And back again.
	if code := postActivation(t, router, deactivatePath+"&reason=paused", "").Code; code != http.StatusOK {
		t.Fatalf("deactivate = %d, want 200", code)
	}
	rows = readDescriptorListing(t, router)
	if len(rows) != 1 || rows[0].Status != "inactive" {
		t.Fatalf("listing after deactivate = %+v, want one inactive row", rows)
	}
}

// The listing carries the POSTURE. `inactive` means the provider still serves
// every invoke under `record` and is refused under `enforce`, so a page that
// showed the status without the posture would read identically in two
// deployments where the same row has opposite consequences.
func TestTheListingReportsTheAdmissionPosture(t *testing.T) {
	pool := overlayPool(t)
	router := activationRouterFor(t, pool)
	registeredDigest(t, router)

	if posture := readListingPosture(t, router); posture != "record" {
		t.Errorf("admission_posture = %q with the variable unset, want record", posture)
	}

	t.Setenv("ELITEA_PROVIDER_ADMISSION", "enforce")
	if posture := readListingPosture(t, router); posture != "enforce" {
		t.Errorf("admission_posture = %q under ELITEA_PROVIDER_ADMISSION=enforce, want enforce", posture)
	}
}

/* ── the refusals ───────────────────────────────────────────────────────── */

func TestActivationRefusalsCarryTheirOwnStatusCodes(t *testing.T) {
	pool := overlayPool(t)
	router := activationRouterFor(t, pool)
	digest := registeredDigest(t, router)

	cases := []struct {
		name   string
		target string
		body   string
		want   int
	}{
		{
			// 422: the manifest moved since the review. The request is well
			// formed and the state is fine; the caller's assertion about it is
			// what is wrong, which is what 422 says and 409 does not.
			name:   "a digest the revision does not cite",
			target: activatePath,
			body: activationBody(
				"0000000000000000000000000000000000000000000000000000000000000000",
				"reviewed", `{}`),
			want: http.StatusUnprocessableEntity,
		},
		{
			// 404: a provider nobody registered. Usually a misspelt name, and
			// answering 200 would send the operator away believing they had
			// turned something on.
			name:   "a provider nobody registered",
			target: "/api/v2/elitea_core/register_descriptor/17/activate?provider_name=nowhere",
			body:   activationBody(digest, "reviewed", `{}`),
			want:   http.StatusNotFound,
		},
		{
			// 400: no reason. An activation is a decision, and a decision with
			// no recorded reason cannot be reviewed later.
			name:   "no reason",
			target: activatePath,
			body:   `{"expected_digest":"` + digest + `","reason":"   ","overlay":{}}`,
			want:   http.StatusBadRequest,
		},
		{
			name:   "no expected digest",
			target: activatePath,
			body:   `{"reason":"reviewed"}`,
			want:   http.StatusBadRequest,
		},
		{
			name:   "an overlay that is not an object",
			target: activatePath,
			body:   `{"expected_digest":"` + digest + `","reason":"reviewed","overlay":[1,2,3]}`,
			want:   http.StatusBadRequest,
		},
		{
			name:   "no provider named",
			target: "/api/v2/elitea_core/register_descriptor/17/activate",
			body:   activationBody(digest, "reviewed", `{}`),
			want:   http.StatusBadRequest,
		},
		{
			// 409: nothing is in force, so there is nothing to stop.
			name:   "deactivating what was never activated",
			target: deactivatePath,
			body:   "",
			want:   http.StatusConflict,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := postActivation(t, router, testCase.target, testCase.body)
			if recorder.Code != testCase.want {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, testCase.want, recorder.Body.String())
			}
		})
	}

	// And nothing above activated anything.
	var active int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM provider_hub.provider_admitted_revision WHERE status = 'active'`).
		Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Errorf("%d revisions are active after seven refusals, want 0", active)
	}
}

// 409 on a revision that is already active. Read through the route rather than
// the store, because the mapping from a typed store error onto a status code is
// the thing this file is measuring.
func TestActivatingAnAlreadyActiveRevisionConflicts(t *testing.T) {
	pool := overlayPool(t)
	router := activationRouterFor(t, pool)
	digest := registeredDigest(t, router)

	body := activationBody(digest, "reviewed", `{}`)
	if code := postActivation(t, router, activatePath, body).Code; code != http.StatusOK {
		t.Fatalf("the first activation failed")
	}
	recorder := postActivation(t, router, activatePath, body)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", recorder.Code, recorder.Body.String())
	}
}

// 0107 applied and 0109 not: a deployment that can record and cannot activate —
// the state provider_admission.go's header described. 501, with the recorded
// reason, not a 500 that names no migration.
func TestActivationRefusesWithoutTheOverlayMigration(t *testing.T) {
	pool := admissionPool(t) // 0107 only
	// The grant lives in 0109 alongside the table, so a database without 0109
	// has neither. Granted by hand here so that the caller passes the GATE and
	// the assertion is about the missing MIGRATION — otherwise this test would
	// pass against a 403 and prove nothing about the refusal.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO auth_core__role_permission (role_id, permission)
SELECT id, 'provider_hub.descriptor.activate' FROM auth_core__role
 WHERE mode = 'administration' AND name = 'admin'
ON CONFLICT (role_id, permission) DO NOTHING`); err != nil {
		t.Fatalf("grant activate: %v", err)
	}
	router := activationRouterFor(t, pool)
	digest := registeredDigest(t, router)

	recorder := postActivation(t, router, activatePath, activationBody(digest, "reviewed", `{}`))
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (%s)", recorder.Code, recorder.Body.String())
	}
	if reason := descriptorReason(t, recorder); reason != eliteacore.ServiceDescriptorsUnavailableReason {
		t.Errorf("reason = %q, want the recorded one", reason)
	}
}

/* ── the permission split ───────────────────────────────────────────────── */

// A HOLDER OF `.register` ALONE IS REFUSED ACTIVATION.
//
// This is the whole reason activate has its own permission string, and it is
// the assertion that keeps it. The caller here holds `.register` and the
// listing permission and nothing else, so they can record a descriptor and see
// it — and cannot put it in force.
func TestRegisterAloneDoesNotGrantActivation(t *testing.T) {
	pool := overlayPool(t)
	ctx := context.Background()

	// A real role with a curated grant set, not a stub resolver: what is being
	// measured is that the two permission STRINGS differ in a database.
	const registrarUser = int64(4109)
	if _, err := pool.Exec(ctx, `
INSERT INTO auth_core__user (id, email, name) VALUES ($1, 'registrar@autotest.local', 'Registrar')
ON CONFLICT (id) DO NOTHING`, registrarUser); err != nil {
		t.Fatalf("seed the registrar: %v", err)
	}
	var roleID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO auth_core__role (name, mode) VALUES ('descriptor_registrar', 'administration')
RETURNING id`).Scan(&roleID); err != nil {
		t.Fatalf("seed the role: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO auth_core__role_permission (role_id, permission)
VALUES ($1, 'provider_hub.descriptor.register'), ($1, 'runtime.airun.serviceproviders')`,
		roleID); err != nil {
		t.Fatalf("grant the registrar: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO auth_core__user_role (user_id, role_id) VALUES ($1, $2)`,
		registrarUser, roleID); err != nil {
		t.Fatalf("attach the role: %v", err)
	}

	router := activationRouter(t, pool, auth.User{
		ID: "4109", UserID: "4109", Email: "registrar@autotest.local"})

	// The register verb WORKS for them, which is what makes the next assertion
	// about the permission rather than about a broken principal.
	if code := postDescriptor(t, router, sampleDescriptor).Code; code != http.StatusAccepted {
		t.Fatalf("registration = %d for a holder of .register, want 202", code)
	}
	digest := ""
	for _, row := range readDescriptorListing(t, router) {
		if row.ManifestDigest != nil {
			digest = *row.ManifestDigest
		}
	}

	recorder := postActivation(t, router, activatePath, activationBody(digest, "reviewed", `{}`))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("activate = %d for a holder of .register alone, want 403: recording a "+
			"descriptor and putting it in force must be separately grantable (%s)",
			recorder.Code, recorder.Body.String())
	}
	recorder = postActivation(t, router, deactivatePath, "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("deactivate = %d for a holder of .register alone, want 403", recorder.Code)
	}
}

/* ── listing readers ────────────────────────────────────────────────────── */

type activationListingRow struct {
	ProviderName   string  `json:"provider_name"`
	Status         string  `json:"status"`
	ManifestDigest *string `json:"published_manifest_digest"`
}

type activationListing struct {
	Rows             []activationListingRow `json:"rows"`
	AdmissionPosture string                 `json:"admission_posture"`
}

func readActivationListing(t *testing.T, router http.Handler) activationListing {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, descriptorListingPath, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("listing = %d (%s)", recorder.Code, recorder.Body.String())
	}
	var listing activationListing
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("listing body: %v", err)
	}
	return listing
}

func readDescriptorListing(t *testing.T, router http.Handler) []activationListingRow {
	t.Helper()
	return readActivationListing(t, router).Rows
}

func readListingPosture(t *testing.T, router http.Handler) string {
	t.Helper()
	return readActivationListing(t, router).AdmissionPosture
}
