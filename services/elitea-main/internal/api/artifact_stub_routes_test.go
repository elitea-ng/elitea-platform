package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// TestArtifactStubRoutesReturn501WithTypedEnvelope verifies S7's own
// acceptance criterion — every one of the 13 new artifact paths resolves to
// notImplementedArtifact and returns 501 with the typed error envelope
// (components/schemas/Error in api/openapi/v2.yaml), not a bare 404 or an
// untyped body. TestSpecRouterConformance (internal/api/oapiserver) already
// proves each spec operationId *resolves* to some route; this proves the
// route it resolves to actually behaves like the stub, end to end through
// the real mounted router.
func TestArtifactStubRoutesReturn501WithTypedEnvelope(t *testing.T) {
	// AppsRepo is one of the fields cmd/elitea-main/main.go always sets, so
	// this exercises the same newProductionRouter build path production
	// uses (see NewRouter's doc comment, #243). Requests carry a credential
	// (testAuthHeader) that the injected testTokenValidator accepts —
	// without one, the Auth middleware in front of this route group 401s
	// every request before it reaches notImplementedArtifact (AUTH_DEV_MODE
	// no longer exists as a bypass, #260/#261).

	cases := []struct {
		method, path string
		// perm is the exact permission RBAC requires for this route (S11's
		// mapping in router.go). Without a resolver granting it, every
		// request 403s before ever reaching notImplementedArtifact, which
		// this test is actually trying to observe — so each subtest builds
		// its own router with a resolver granting only its own permission,
		// which also self-evidently proves the RBAC gate is checking the
		// right permission per route (a wrong grant would still 403).
		perm string
	}{
		{http.MethodGet, "/api/v2/artifacts/buckets/1", artifactPermissionView},
		{http.MethodPost, "/api/v2/artifacts/buckets/1", artifactPermissionCreate},
		{http.MethodGet, "/api/v2/artifacts/buckets/1/reports", artifactPermissionView},
		{http.MethodPatch, "/api/v2/artifacts/buckets/1/reports", artifactPermissionEdit},
		{http.MethodDelete, "/api/v2/artifacts/buckets/1/reports", artifactPermissionDelete},
		{http.MethodGet, "/api/v2/artifacts/objects/1/reports", artifactPermissionView},
		{http.MethodPost, "/api/v2/artifacts/objects/1/reports", artifactPermissionCreate},
		{http.MethodPost, "/api/v2/artifacts/objects/1/reports:batchDelete", artifactPermissionDelete},
		{http.MethodGet, "/api/v2/artifacts/objects/1/reports/a/b/c.png", artifactPermissionView},
		{http.MethodHead, "/api/v2/artifacts/objects/1/reports/a/b/c.png", artifactPermissionView},
		{http.MethodDelete, "/api/v2/artifacts/objects/1/reports/a/b/c.png", artifactPermissionDelete},
		// The SDK's by-filepath alias of the object download (#978). Listed
		// here rather than in a file of its own because the one thing that
		// can go wrong with an alias is that it resolves somewhere else:
		// a missing registration answers 404 and a mis-tiered one 403, and
		// both are indistinguishable from the absent route the SDK used to
		// meet.
		{http.MethodGet, "/api/v2/artifacts/artifact/default/1/reports/a/b/c.png", artifactPermissionView},
		{http.MethodPost, "/api/v2/artifacts/grants/1/reports", artifactPermissionCreate},
		{http.MethodPost, "/api/v2/artifacts/grants/1/abc123:commit", artifactPermissionCreate},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			router := NewRouter(RouterConfig{
				AuthValidator:              testTokenValidator{user: authenticatedTestUser()},
				PrincipalValidator:         testPrincipalValidator{},
				AppsRepo:                   struct{ applications.Repository }{},
				ArtifactPermissionResolver: fakePermissionResolver{granted: []string{tc.perm}},
			})

			req := testAuthHeader(httptest.NewRequest(tc.method, tc.path, nil))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusNotImplemented, rec.Body.String())
			}
			if tc.method == http.MethodHead {
				// HEAD responses must not carry a body per net/http semantics.
				return
			}
			var envelope struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("response body is not the typed error envelope: %v (body=%s)", err, rec.Body.String())
			}
			if envelope.Error.Code != "NotImplemented" {
				t.Fatalf("error.code = %q, want NotImplemented", envelope.Error.Code)
			}
		})
	}
}

// noopObjectStore satisfies storage.ObjectStore without doing anything —
// TestArtifactBucketRoutesWireToRealHandlerWhenConfigured never lets a
// request reach it (GetBucket fails against the unreachable pool first), it
// only needs to be non-nil to clear newArtifactBucketHandlers' guard.
type noopObjectStore struct{ storage.ObjectStore }

// TestArtifactBucketRoutesWireToRealHandlerWhenConfigured proves S8's
// newArtifactBucketHandlers guard actually flips: with cfg.Pool and
// cfg.ObjectStore both set, the five bucket-plane routes must stop
// returning the S7 notImplementedArtifact stub body, even though the pool
// here never successfully connects (pgxpool.New does not dial eagerly) and
// every request still ends in an error. Distinguishing "wired to the real
// handler, which then failed" from "still the stub" is exactly the branch
// TestArtifactStubRoutesReturn501WithTypedEnvelope's AppsRepo-only config
// cannot exercise, because that config leaves cfg.Pool nil.
func TestArtifactBucketRoutesWireToRealHandlerWhenConfigured(t *testing.T) {

	pool, err := pgxpool.New(context.Background(), "postgres://nouser:nopass@127.0.0.1:1/nodb")
	if err != nil {
		t.Fatalf("pgxpool.New (lazy, must not dial): %v", err)
	}
	defer pool.Close()

	router := NewRouter(RouterConfig{
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		AppsRepo:           struct{ applications.Repository }{},
		Pool:               pool,
		ObjectStore:        noopObjectStore{},
		// This test's 5 subtests span view/create/edit/delete (the full
		// bucket-plane permission mapping), so grant all four up front —
		// what this test is actually checking is "real handler wired, not
		// the S7 stub," and RBAC gating per se is already proven by
		// TestArtifactStubRoutesReturn501WithTypedEnvelope's per-permission
		// subtests.
		ArtifactPermissionResolver: fakePermissionResolver{granted: []string{
			artifactPermissionView, artifactPermissionCreate, artifactPermissionEdit, artifactPermissionDelete,
		}},
	})

	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/v2/artifacts/buckets/1"},
		{http.MethodPost, "/api/v2/artifacts/buckets/1"},
		{http.MethodGet, "/api/v2/artifacts/buckets/1/reports"},
		{http.MethodPatch, "/api/v2/artifacts/buckets/1/reports"},
		{http.MethodDelete, "/api/v2/artifacts/buckets/1/reports"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := testAuthHeader(httptest.NewRequest(tc.method, tc.path, nil))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code == http.StatusNotImplemented {
				t.Fatalf("still the S7 stub (501) with Pool and ObjectStore configured; body=%s", rec.Body.String())
			}
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("response body is not the typed error envelope: %v (body=%s)", err, rec.Body.String())
			}
			if envelope.Error.Code == "NotImplemented" {
				t.Fatalf("still the S7 stub error code with Pool and ObjectStore configured; body=%s", rec.Body.String())
			}
		})
	}
}

// TestArtifactBucketRoutesStayStubbedWithoutObjectStore proves the other
// side of the same guard: cfg.Pool alone is not enough to activate the real
// handlers — newArtifactHandler requires both.
func TestArtifactBucketRoutesStayStubbedWithoutObjectStore(t *testing.T) {

	pool, err := pgxpool.New(context.Background(), "postgres://nouser:nopass@127.0.0.1:1/nodb")
	if err != nil {
		t.Fatalf("pgxpool.New (lazy, must not dial): %v", err)
	}
	defer pool.Close()

	router := NewRouter(RouterConfig{
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		AppsRepo:           struct{ applications.Repository }{},
		Pool:               pool,
		// ObjectStore deliberately left nil.
		//
		// This subtest only issues a GET, so grant view — without a grant
		// here the request would still 501, but that would prove nothing:
		// it could be RBAC 403-ing before ever reaching the
		// stub-vs-real-handler branch this test exists to check, rather
		// than the still-stubbed branch it claims to prove.
		ArtifactPermissionResolver: fakePermissionResolver{granted: []string{artifactPermissionView}},
	})

	req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/api/v2/artifacts/buckets/1", nil))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d (stub) when ObjectStore is unset; body=%s", rec.Code, http.StatusNotImplemented, rec.Body.String())
	}
}

// TestArtifactObjectRoutesWireToRealHandlerWhenConfigured mirrors
// TestArtifactBucketRoutesWireToRealHandlerWhenConfigured for S9's six
// object-plane routes — newArtifactHandler backs both planes with the same
// guard, so this proves that guard flips for the object routes too, not
// just the bucket ones S8 already covered.
func TestArtifactObjectRoutesWireToRealHandlerWhenConfigured(t *testing.T) {

	pool, err := pgxpool.New(context.Background(), "postgres://nouser:nopass@127.0.0.1:1/nodb")
	if err != nil {
		t.Fatalf("pgxpool.New (lazy, must not dial): %v", err)
	}
	defer pool.Close()

	router := NewRouter(RouterConfig{
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		AppsRepo:           struct{ applications.Repository }{},
		Pool:               pool,
		ObjectStore:        noopObjectStore{},
		// This test's 6 subtests span view/create/delete (the object-plane
		// permission mapping has no PATCH/edit route), so grant that union
		// up front — the point of this test is "real handler wired, not
		// the S7 stub," not RBAC gating itself, which is already covered by
		// TestArtifactStubRoutesReturn501WithTypedEnvelope's per-permission
		// subtests.
		ArtifactPermissionResolver: fakePermissionResolver{granted: []string{
			artifactPermissionView, artifactPermissionCreate, artifactPermissionDelete,
		}},
	})

	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/v2/artifacts/objects/1/reports"},
		{http.MethodPost, "/api/v2/artifacts/objects/1/reports"},
		{http.MethodPost, "/api/v2/artifacts/objects/1/reports:batchDelete"},
		{http.MethodGet, "/api/v2/artifacts/objects/1/reports/a/b/c.png"},
		{http.MethodHead, "/api/v2/artifacts/objects/1/reports/a/b/c.png"},
		{http.MethodDelete, "/api/v2/artifacts/objects/1/reports/a/b/c.png"},
		{http.MethodGet, "/api/v2/artifacts/artifact/default/1/reports/a/b/c.png"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := testAuthHeader(httptest.NewRequest(tc.method, tc.path, nil))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code == http.StatusNotImplemented {
				t.Fatalf("still the S7 stub (501) with Pool and ObjectStore configured; body=%s", rec.Body.String())
			}
			if tc.method == http.MethodHead {
				return
			}
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("response body is not the typed error envelope: %v (body=%s)", err, rec.Body.String())
			}
			if envelope.Error.Code == "NotImplemented" {
				t.Fatalf("still the S7 stub error code with Pool and ObjectStore configured; body=%s", rec.Body.String())
			}
		})
	}
}

// TestArtifactRouteReturnsInternalCodeWhenBackendFails pins the "Internal"
// error envelope code (storageErrorCode's default branch, objects.go) to an
// actual response through the real router — closing the other half of the
// plan's S19 acceptance criterion ("every error code in the envelope enum is
// reachable by some request") alongside
// TestArtifactStubRoutesReturn501WithTypedEnvelope's NotImplemented coverage
// above. TestArtifactBucketRoutesWireToRealHandlerWhenConfigured already
// drives a request into the real handler against this same unreachable
// pool, but only asserts the code is *not* still the S7 stub; this pins the
// exact code that failure produces. ListBuckets is the target because it is
// the one bucket-plane route with no request body to decode first — every
// other route in that table risks 400ing on InvalidArgument before ever
// reaching the pool, which would prove nothing about this specific branch.
func TestArtifactRouteReturnsInternalCodeWhenBackendFails(t *testing.T) {

	pool, err := pgxpool.New(context.Background(), "postgres://nouser:nopass@127.0.0.1:1/nodb")
	if err != nil {
		t.Fatalf("pgxpool.New (lazy, must not dial): %v", err)
	}
	defer pool.Close()

	router := NewRouter(RouterConfig{
		AuthValidator:              testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:         testPrincipalValidator{},
		AppsRepo:                   struct{ applications.Repository }{},
		Pool:                       pool,
		ObjectStore:                noopObjectStore{},
		ArtifactPermissionResolver: fakePermissionResolver{granted: []string{artifactPermissionView}},
	})

	req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/api/v2/artifacts/buckets/1", nil))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response body is not the typed error envelope: %v (body=%s)", err, rec.Body.String())
	}
	if envelope.Error.Code != "Internal" {
		t.Fatalf("error.code = %q, want Internal", envelope.Error.Code)
	}
}
