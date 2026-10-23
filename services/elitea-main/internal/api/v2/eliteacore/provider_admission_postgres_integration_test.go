package eliteacore_test

// The provider-admission surface WITH migration 0107 applied (ADR-0012 P3).
//
// Its sibling, service_descriptors_postgres_integration_test.go, covers the
// other half and is UNCHANGED by this work: without 0107 there is no admission
// plane, and every route still refuses with the recorded reason. That is not a
// coincidence to note in passing — a deployment that has not migrated is
// exactly the state the refusal described, and the two files together are the
// claim that the surface tells the truth in both.
//
// What is proved here is the frozen contract, and every assertion exists
// because the shape it replaces got that thing wrong:
//
//   1. Registration answers 202, not 200, and its body names the revision, the
//      digest, the status and the reason. The handler this replaces answered
//      `{"ok": true}` to a body it discarded.
//   2. What is recorded is INACTIVE. This deployment cannot activate a
//      provider, and the route must not imply it did.
//   3. `healthy` is null when no probe has reported, true or false when one
//      has. The legacy runtime could only say true or false, so a provider
//      nobody had probed looked broken.
//   4. DELETE revokes and does NOT delete the row. An admission that was once
//      in force is a fact about what this deployment ran.

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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// admissionPool is the descriptor harness's pool with 0107 applied on top.
func admissionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newAuditPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// 0107 creates the plane; 0171 admits the grpcs:// listener origin.
	for _, file := range []string{"0107_provider_admitted_revisions.sql", "0171_provider_origin_grpcs.sql"} {
		sql, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "migrations", "shared", file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", file, err)
		}
	}
	return pool
}

const admissionOperator = "descriptor-operator@autotest.local"

func admissionRouterFor(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	return descriptorRouter(t, pool, auth.User{
		ID: "1", UserID: "1", Email: admissionOperator,
	})
}

func postDescriptor(t *testing.T, router http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, descriptorRegisterPath, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

const sampleDescriptor = `{"provider_name":"deepwiki","service_location_url":"https://elitea-deepwiki:8443",` +
	`"descriptor":{"name":"deepwiki","provided_toolkits":[{"name":"wikis"}]}}`

func TestRegistrationIsAcceptedAndRecordedInactive(t *testing.T) {
	pool := admissionPool(t)
	router := admissionRouterFor(t, pool)

	recorder := postDescriptor(t, router, sampleDescriptor)

	// 202, not 200. The provider is recorded and is NOT in force.
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", recorder.Code, recorder.Body.String())
	}

	var response struct {
		AdmittedProviderRevision string `json:"admitted_provider_revision"`
		PublishedManifestDigest  string `json:"published_manifest_digest"`
		Status                   string `json:"status"`
		Reason                   string `json:"reason"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, recorder.Body.String())
	}
	// Every field, because `{"ok": true}` is what this replaces: a caller must
	// learn what happened, not merely that something did.
	if response.AdmittedProviderRevision == "" {
		t.Error("no revision id in the response")
	}
	if len(response.PublishedManifestDigest) != 64 {
		t.Errorf("digest %q is not a sha256", response.PublishedManifestDigest)
	}
	if response.Status != "inactive" {
		t.Errorf("status = %q, want inactive — this deployment cannot activate a provider", response.Status)
	}
	if response.Reason == "" {
		t.Error("no reason given for the inactive status")
	}

	ctx := context.Background()
	var status, admittedBy string
	var overlay *string
	if err := pool.QueryRow(ctx, `
SELECT status, admitted_by, overlay_revision
  FROM provider_hub.provider_admitted_revision
 WHERE revision_id = $1`, response.AdmittedProviderRevision).Scan(&status, &admittedBy, &overlay); err != nil {
		t.Fatalf("reading the stored revision: %v", err)
	}
	if status != "inactive" || overlay != nil {
		t.Errorf("stored revision is status=%q overlay=%v, want inactive with no overlay", status, overlay)
	}
	// The audit columns carry a real actor, not a blank.
	if admittedBy != admissionOperator {
		t.Errorf("admitted_by = %q, want the acting operator", admittedBy)
	}
}

func TestTheSameDescriptorPublishesOneManifest(t *testing.T) {
	pool := admissionPool(t)
	router := admissionRouterFor(t, pool)

	// Content-addressed: registering identical bytes twice must not create a
	// second manifest, or the store grows one row per retry.
	first := postDescriptor(t, router, sampleDescriptor)
	second := postDescriptor(t, router, sampleDescriptor)
	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("statuses = %d, %d; want 202 twice", first.Code, second.Code)
	}

	var manifests int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM provider_hub.provider_published_manifest`).Scan(&manifests); err != nil {
		t.Fatalf("counting manifests: %v", err)
	}
	if manifests != 1 {
		t.Errorf("%d manifests stored for identical bytes, want 1", manifests)
	}
}

func TestHealthIsNullUntilSomethingProbes(t *testing.T) {
	pool := admissionPool(t)
	router := admissionRouterFor(t, pool)
	if code := postDescriptor(t, router, sampleDescriptor).Code; code != http.StatusAccepted {
		t.Fatalf("registration failed: %d", code)
	}

	read := func() *bool {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, descriptorListingPath, nil)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("listing status = %d (body %s)", recorder.Code, recorder.Body.String())
		}
		var body struct {
			Rows []struct {
				ProviderName string `json:"provider_name"`
				Healthy      *bool  `json:"healthy"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("listing is not JSON: %v", err)
		}
		if len(body.Rows) != 1 {
			t.Fatalf("%d rows, want 1", len(body.Rows))
		}
		return body.Rows[0].Healthy
	}

	// NOBODY HAS PROBED. Null, not false — the distinction the legacy page could
	// not express, and the reason an operator can tell a broken provider from
	// a probe that has stopped running.
	if healthy := read(); healthy != nil {
		t.Errorf("healthy = %v with no probe recorded, want null", *healthy)
	}

	if _, err := pool.Exec(context.Background(), `
INSERT INTO provider_hub.provider_health_projection (project_id, provider_id, healthy, detail)
VALUES (17, 'deepwiki', false, 'connection refused')`); err != nil {
		t.Fatalf("recording a probe: %v", err)
	}
	healthy := read()
	if healthy == nil || *healthy {
		t.Errorf("healthy = %v after a failing probe, want false", healthy)
	}

	// A STALE reading is null again. Backdated past the freshness window.
	if _, err := pool.Exec(context.Background(), `
UPDATE provider_hub.provider_health_projection
   SET observed_at = clock_timestamp() - interval '1 hour'`); err != nil {
		t.Fatalf("backdating the probe: %v", err)
	}
	if healthy := read(); healthy != nil {
		t.Errorf("healthy = %v for an hour-old probe, want null — a stale reading is "+
			"not a statement about the provider", *healthy)
	}
}

func TestRevokeMarksTheRevisionAndKeepsTheRow(t *testing.T) {
	pool := admissionPool(t)
	router := admissionRouterFor(t, pool)
	if code := postDescriptor(t, router, sampleDescriptor).Code; code != http.StatusAccepted {
		t.Fatalf("registration failed: %d", code)
	}

	request := httptest.NewRequest(http.MethodDelete,
		descriptorRegisterPath+"?provider_name=deepwiki&reason=decommissioned", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("revoke status = %d (body %s)", recorder.Code, recorder.Body.String())
	}

	ctx := context.Background()
	var count int
	var status, reason, revokedBy string
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM provider_hub.provider_admitted_revision`).Scan(&count); err != nil {
		t.Fatalf("counting revisions: %v", err)
	}
	// THE ROW SURVIVES. An audit that the audited surface can erase is not one.
	if count != 1 {
		t.Fatalf("%d revisions after revoke, want the row kept", count)
	}
	if err := pool.QueryRow(ctx, `
SELECT status, reason, coalesce(revoked_by, '')
  FROM provider_hub.provider_admitted_revision`).Scan(&status, &reason, &revokedBy); err != nil {
		t.Fatalf("reading the revoked revision: %v", err)
	}
	if status != "revoked" {
		t.Errorf("status = %q, want revoked", status)
	}
	if reason != "decommissioned" {
		t.Errorf("reason = %q, want the caller's reason recorded", reason)
	}
	if revokedBy != admissionOperator {
		t.Errorf("revoked_by = %q, want the acting operator", revokedBy)
	}
}

func TestRevokingAProviderNobodyRegisteredIsNotReportedAsDone(t *testing.T) {
	pool := admissionPool(t)
	router := admissionRouterFor(t, pool)

	request := httptest.NewRequest(http.MethodDelete,
		descriptorRegisterPath+"?provider_name=never-registered", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	// A misspelt provider name must not answer 200: the operator would leave
	// believing they had turned something off.
	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
	}
}

// The facades' registrar records a provider at its gRPC listener,
// grpcs://host:port, and the listing shows that origin; an operator who
// re-registers what the listing shows (DWIKI-013b re-admits a revoked provider
// exactly so) must be accepted. A path, or a scheme that is neither http(s)
// nor grpcs, is still refused with a message, not a constraint error.
func TestRegistrationAcceptsTheGRPCListenerOriginAndRefusesOthers(t *testing.T) {
	pool := admissionPool(t)
	router := admissionRouterFor(t, pool)
	accepted := postDescriptor(t, router, `{"provider_name":"wikis","service_location_url":"grpcs://elitea-deepwiki:9443","descriptor":{"name":"wikis"}}`)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("grpcs origin: %d %s", accepted.Code, accepted.Body.String())
	}
	for _, origin := range []string{"grpcs://elitea-deepwiki:9443/x", "grpc://elitea-deepwiki:9443", "grpcs://"} {
		refused := postDescriptor(t, router, `{"provider_name":"wikis","service_location_url":"`+origin+`","descriptor":{"name":"wikis"}}`)
		if refused.Code != http.StatusBadRequest {
			t.Fatalf("origin %q: %d %s, want 400", origin, refused.Code, refused.Body.String())
		}
	}
}
