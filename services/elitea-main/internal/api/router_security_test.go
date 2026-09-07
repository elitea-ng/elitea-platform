package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	v2artifacts "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
)

// TestObjectDownloadRejectsRawTraversalKey proves the new artifact API
// rejects a raw ".." path segment before it ever reaches the metadata
// repository. It follows the same lazy-pgxpool pattern as
// TestArtifactBucketRoutesWireToRealHandlerWhenConfigured and
// TestArtifactBucketRoutesStayStubbedWithoutObjectStore in
// artifact_stub_routes_test.go: pgxpool.New against an address nothing
// listens on succeeds without dialing (pgx v5 connects lazily), so only
// checks that happen before any repository/store round-trip can be asserted
// this way. That's exactly what this test needs — DownloadObject must
// validate the key (storage.NewObjectRef) and reject it with InvalidKey
// ahead of the requireBucket lookup that would otherwise hang or 500
// against the unreachable pool.
func TestObjectDownloadRejectsRawTraversalKey(t *testing.T) {

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

	req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/api/v2/artifacts/objects/1/reports/../escape.txt", nil))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
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
	if envelope.Error.Code != "InvalidKey" {
		t.Fatalf("error.code = %q, want InvalidKey; body=%s", envelope.Error.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// S11 acceptance criteria — "Production mount, auth, RBAC"
// (docs/plans/storage-migration-plan.md, search "## S11").
// ---------------------------------------------------------------------------

// artifactRoutePermission is one of the 13 mounted artifact routes together
// with the single permission string the S11 mapping requires for it.
// Literal (method, path) pairs are kept identical to
// TestArtifactStubRoutesReturn501WithTypedEnvelope in artifact_stub_routes_test.go
// so both tests exercise exactly the same route surface.
type artifactRoutePermission struct {
	method     string
	path       string
	permission string
}

var artifactRoutePermissions = []artifactRoutePermission{
	{method: http.MethodGet, path: "/api/v2/artifacts/buckets/1", permission: artifactPermissionView},
	{method: http.MethodPost, path: "/api/v2/artifacts/buckets/1", permission: artifactPermissionCreate},
	{method: http.MethodGet, path: "/api/v2/artifacts/buckets/1/reports", permission: artifactPermissionView},
	{method: http.MethodPatch, path: "/api/v2/artifacts/buckets/1/reports", permission: artifactPermissionEdit},
	{method: http.MethodDelete, path: "/api/v2/artifacts/buckets/1/reports", permission: artifactPermissionDelete},
	{method: http.MethodGet, path: "/api/v2/artifacts/objects/1/reports", permission: artifactPermissionView},
	{method: http.MethodPost, path: "/api/v2/artifacts/objects/1/reports", permission: artifactPermissionCreate},
	{method: http.MethodPost, path: "/api/v2/artifacts/objects/1/reports:batchDelete", permission: artifactPermissionDelete},
	{method: http.MethodGet, path: "/api/v2/artifacts/objects/1/reports/a/b/c.png", permission: artifactPermissionView},
	{method: http.MethodHead, path: "/api/v2/artifacts/objects/1/reports/a/b/c.png", permission: artifactPermissionView},
	{method: http.MethodDelete, path: "/api/v2/artifacts/objects/1/reports/a/b/c.png", permission: artifactPermissionDelete},
	{method: http.MethodPost, path: "/api/v2/artifacts/grants/1/reports", permission: artifactPermissionCreate},
	{method: http.MethodPost, path: "/api/v2/artifacts/grants/1/abc123:commit", permission: artifactPermissionCreate},
	// The S3-shaped listing. It is listed here so it inherits the same
	// unauthenticated-401 and no-permission-403 guarantees as every other
	// artifact route, despite naming its project in the query string rather
	// than the path — the whole risk of that route is that it becomes a
	// softer way in, and these two tests are what forbid it.
	{method: http.MethodGet, path: "/artifacts/s3/reports?project_id=1", permission: artifactPermissionView},
	// The S3-shaped object read, with a nested key — same query-parameter
	// project and therefore the same risk, and it carries bytes rather than
	// metadata, so it is the one that must not be a softer way in.
	{method: http.MethodGet, path: "/artifacts/s3/reports/folder/sub/file.txt?project_id=1", permission: artifactPermissionView},
	// The S3-shaped write verbs. Same query-parameter project, same risk, and
	// higher stakes than the reads: an unauthenticated or unpermissioned
	// caller reaching these would not merely read another tenant's artifacts
	// but create or destroy them. Their permission tiers mirror the native
	// object plane exactly — upload is create, delete is delete, an existence
	// check is view.
	{method: http.MethodPut, path: "/artifacts/s3/reports/folder/sub/file.txt?project_id=1", permission: artifactPermissionCreate},
	{method: http.MethodDelete, path: "/artifacts/s3/reports/folder/sub/file.txt?project_id=1", permission: artifactPermissionDelete},
	{method: http.MethodHead, path: "/artifacts/s3/reports/folder/sub/file.txt?project_id=1", permission: artifactPermissionView},
}

// allArtifactPermissions is used where a test wants authorization to be a
// non-issue (e.g. proving 401 happens before RBAC ever runs).
var allArtifactPermissions = []string{
	artifactPermissionView, artifactPermissionCreate, artifactPermissionEdit, artifactPermissionDelete,
}

// TestArtifactRoutesRequireAuthentication proves S11's first acceptance bullet:
// every one of the 13 artifact routes returns 401 when the caller is
// unauthenticated, even though a real handler is wired and the resolver would
// grant every permission — an auth bypass would show up here as a 2xx/501,
// not a silent pass, because RBAC is never given the chance to run first.
// A validator IS wired below, and the resolver grants everything, so a 401
// here can only come from the absent credential — not from a missing
// validator, which would make the assertion pass for the wrong reason.
// Requests deliberately omit testAuthHeader.
func TestArtifactRoutesRequireAuthentication(t *testing.T) {
	handler := v2artifacts.NewHandler(alwaysSucceedsArtifactRepo{}, alwaysSucceedsArtifactStore{})
	router := NewRouter(RouterConfig{
		AuthValidator:              testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:         testPrincipalValidator{},
		AppsRepo:                   struct{ applications.Repository }{},
		ArtifactHandler:            handler,
		ArtifactPermissionResolver: fakePermissionResolver{granted: allArtifactPermissions},
	})

	for _, rp := range artifactRoutePermissions {
		t.Run(rp.method+" "+rp.path, func(t *testing.T) {
			req := httptest.NewRequest(rp.method, rp.path, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
			}
		})
	}
}

// TestArtifactRoutesRequirePermission proves S11's first acceptance bullet's
// other half: every one of the 13 routes returns 403 for an authenticated
// principal holding none of the artifact permissions — including the two
// still-stubbed grant routes, which S11 gates unconditionally even though
// S15 hasn't implemented their handlers yet.
func TestArtifactRoutesRequirePermission(t *testing.T) {

	handler := v2artifacts.NewHandler(alwaysSucceedsArtifactRepo{}, alwaysSucceedsArtifactStore{})
	router := NewRouter(RouterConfig{
		AuthValidator:              testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:         testPrincipalValidator{},
		AppsRepo:                   struct{ applications.Repository }{},
		ArtifactHandler:            handler,
		ArtifactPermissionResolver: fakePermissionResolver{granted: nil},
	})

	for _, rp := range artifactRoutePermissions {
		t.Run(rp.method+" "+rp.path, func(t *testing.T) {
			req := testAuthHeader(httptest.NewRequest(rp.method, rp.path, nil))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
			}
		})
	}
}

// artifactJSONBody returns a body-builder for a fixed JSON request body.
func artifactJSONBody(body string) func(t *testing.T) (io.Reader, string) {
	return func(t *testing.T) (io.Reader, string) {
		return strings.NewReader(body), "application/json"
	}
}

// artifactRawBody returns a body-builder for a raw (non-multipart) request
// body — the shape the SDK's S3 PUT sends (requests.put(url, data=data)),
// where the native upload route sends multipart/form-data.
func artifactRawBody(body string) func(t *testing.T) (io.Reader, string) {
	return func(t *testing.T) (io.Reader, string) {
		return strings.NewReader(body), "application/octet-stream"
	}
}

// artifactMultipartUploadBody builds a genuine multipart/form-data body with
// a "file" field carrying a filename, matching what UploadObject
// (internal/api/v2/artifacts/objects.go) requires before it ever reaches the
// store.
func artifactMultipartUploadBody(t *testing.T) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "photo.png")
	if err != nil {
		t.Fatalf("create multipart form file: %v", err)
	}
	if _, err := part.Write([]byte("data")); err != nil {
		t.Fatalf("write multipart form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// artifactSuccessCase is one of the 11 real-handler artifact routes (5
// bucket-plane + 6 object-plane), the exact permission the S11 mapping
// requires, and the 2xx status the real handler
// (alwaysSucceedsArtifactRepo/alwaysSucceedsArtifactStore) returns once RBAC
// lets the request through.
type artifactSuccessCase struct {
	desc       string
	method     string
	path       string
	permission string
	wantStatus int
	newBody    func(t *testing.T) (io.Reader, string) // nil => no request body
}

var artifactSuccessCases = []artifactSuccessCase{
	{desc: "list buckets", method: http.MethodGet, path: "/api/v2/artifacts/buckets/1", permission: artifactPermissionView, wantStatus: http.StatusOK},
	{desc: "create bucket", method: http.MethodPost, path: "/api/v2/artifacts/buckets/1", permission: artifactPermissionCreate, wantStatus: http.StatusOK, newBody: artifactJSONBody(`{"name":"reports"}`)},
	{desc: "get bucket", method: http.MethodGet, path: "/api/v2/artifacts/buckets/1/reports", permission: artifactPermissionView, wantStatus: http.StatusOK},
	{desc: "update bucket", method: http.MethodPatch, path: "/api/v2/artifacts/buckets/1/reports", permission: artifactPermissionEdit, wantStatus: http.StatusOK, newBody: artifactJSONBody(`{"is_pinned":true}`)},
	{desc: "delete bucket", method: http.MethodDelete, path: "/api/v2/artifacts/buckets/1/reports", permission: artifactPermissionDelete, wantStatus: http.StatusNoContent},
	{desc: "list objects", method: http.MethodGet, path: "/api/v2/artifacts/objects/1/reports", permission: artifactPermissionView, wantStatus: http.StatusOK},
	// overwrite=true: alwaysSucceedsArtifactStore.Stat always succeeds, so
	// without it UploadObject's own pre-existing-key check (objects.go) would
	// report 409 AlreadyExists instead of ever reaching Put — a genuine
	// handler behavior, not an RBAC concern, but it means this is the one
	// case that needs the query parameter to observe the real 201.
	{desc: "upload object", method: http.MethodPost, path: "/api/v2/artifacts/objects/1/reports?overwrite=true", permission: artifactPermissionCreate, wantStatus: http.StatusCreated, newBody: artifactMultipartUploadBody},
	{desc: "batch delete objects", method: http.MethodPost, path: "/api/v2/artifacts/objects/1/reports:batchDelete", permission: artifactPermissionDelete, wantStatus: http.StatusOK, newBody: artifactJSONBody(`{"keys":["a.png"]}`)},
	{desc: "download object", method: http.MethodGet, path: "/api/v2/artifacts/objects/1/reports/a/b/c.png", permission: artifactPermissionView, wantStatus: http.StatusOK},
	{desc: "stat object", method: http.MethodHead, path: "/api/v2/artifacts/objects/1/reports/a/b/c.png", permission: artifactPermissionView, wantStatus: http.StatusOK},
	{desc: "delete object", method: http.MethodDelete, path: "/api/v2/artifacts/objects/1/reports/a/b/c.png", permission: artifactPermissionDelete, wantStatus: http.StatusNoContent},
	// The S3-shaped listing, at its real root-level path. Before this route
	// existed the same request produced a 404 that the SDK swallowed into an
	// empty listing, so a 200 here is exactly the behaviour change that
	// turns a vacuously green index run into a real one.
	{desc: "list objects (s3)", method: http.MethodGet, path: "/artifacts/s3/reports?project_id=1", permission: artifactPermissionView, wantStatus: http.StatusOK},
	// The S3-shaped object read. Before this route existed the same request
	// 404'd, which the SDK logs and swallows — leaving an index run that
	// listed the right files and indexed every one of them empty.
	{desc: "download object (s3)", method: http.MethodGet, path: "/artifacts/s3/reports/folder/sub/file.txt?project_id=1", permission: artifactPermissionView, wantStatus: http.StatusOK},
	// The write verbs, at their real root-level paths. Before these routes
	// existed each of these requests 404'd, which the SDK turns into an
	// unactionable "S3 error: HTTP_404" (upload/delete) or a plain
	// {"exists": False} (head) — the last of which is the dangerous one, since
	// it reads as "nothing is there" rather than "the server has no such
	// route". No overwrite parameter appears here, unlike the native upload
	// case above: an S3 PUT is an upsert by contract, so it must reach a 200
	// even though alwaysSucceedsArtifactStore.Stat reports the key as already
	// present.
	{desc: "upload object (s3)", method: http.MethodPut, path: "/artifacts/s3/reports/folder/sub/file.txt?project_id=1", permission: artifactPermissionCreate, wantStatus: http.StatusOK, newBody: artifactRawBody("payload")},
	{desc: "delete object (s3)", method: http.MethodDelete, path: "/artifacts/s3/reports/folder/sub/file.txt?project_id=1", permission: artifactPermissionDelete, wantStatus: http.StatusNoContent},
	{desc: "stat object (s3)", method: http.MethodHead, path: "/artifacts/s3/reports/folder/sub/file.txt?project_id=1", permission: artifactPermissionView, wantStatus: http.StatusOK},
}

// TestArtifactRoutesSucceedWithExactRequiredPermission proves S11's second
// acceptance bullet: each of the 11 routes with a real handler (5 bucket-plane
// from S8, 6 object-plane from S9) returns a genuine 2xx — not just "past the
// stub" — once the principal holds exactly the permission the S11 mapping
// assigns it, exercised through the full auth/RBAC/handler chain.
func TestArtifactRoutesSucceedWithExactRequiredPermission(t *testing.T) {

	for _, sc := range artifactSuccessCases {
		t.Run(sc.desc, func(t *testing.T) {
			handler := v2artifacts.NewHandler(alwaysSucceedsArtifactRepo{}, alwaysSucceedsArtifactStore{})
			router := NewRouter(RouterConfig{
				AuthValidator:              testTokenValidator{user: authenticatedTestUser()},
				PrincipalValidator:         testPrincipalValidator{},
				AppsRepo:                   struct{ applications.Repository }{},
				ArtifactHandler:            handler,
				ArtifactPermissionResolver: fakePermissionResolver{granted: []string{sc.permission}},
			})

			var body io.Reader
			var contentType string
			if sc.newBody != nil {
				body, contentType = sc.newBody(t)
			}
			req := testAuthHeader(httptest.NewRequest(sc.method, sc.path, body))
			if contentType != "" {
				req.Header.Set("Content-Type", contentType)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != sc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, sc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestArtifactGrantRoutesResolveThroughRealHandlerWhenConfigured is S15's
// router-level check the plan text explicitly asks for: internal/api/v2/
// artifacts's own tests (grants_test.go) exercise CreateTransferGrant/
// CommitTransferGrant directly and never touch the mounted route, so only a
// test in this package can prove router.go's stub replacement actually
// happened — a handler-level Verify command passing here would mean
// nothing about whether the routes are still wired to notImplementedArtifact.
// Both routes need the create permission per the S11 mapping (see
// artifactRoutePermissions); grant creation returns 200 with a real grant_id
// (not NotImplemented), and committing that exact grant_id — a genuine,
// syntactically valid UUID the fake handler chain accepts unconditionally —
// returns 200, not 501, proving CommitTransferGrant is reachable too.
func TestArtifactGrantRoutesResolveThroughRealHandlerWhenConfigured(t *testing.T) {

	handler := v2artifacts.NewHandler(alwaysSucceedsArtifactRepo{}, alwaysSucceedsArtifactStore{})
	router := NewRouter(RouterConfig{
		AuthValidator:              testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:         testPrincipalValidator{},
		AppsRepo:                   struct{ applications.Repository }{},
		ArtifactHandler:            handler,
		ArtifactPermissionResolver: fakePermissionResolver{granted: []string{artifactPermissionCreate}},
	})

	createReq := testAuthHeader(httptest.NewRequest(http.MethodPost, "/api/v2/artifacts/grants/1/reports", strings.NewReader(`{"method":"PUT","content_type":"image/png","max_bytes":1024}`)))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	router.ServeHTTP(createRec, createReq)
	if createRec.Code == http.StatusNotImplemented {
		t.Fatalf("createTransferGrant route still resolves to the S7 stub; body=%s", createRec.Body.String())
	}
	if createRec.Code != http.StatusOK {
		t.Fatalf("create grant: status = %d, want 200; body=%s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		GrantID string `json:"grant_id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create grant response: %v (body=%s)", err, createRec.Body.String())
	}
	if created.GrantID == "" {
		t.Fatal("expected a non-empty grant_id")
	}

	commitReq := testAuthHeader(httptest.NewRequest(http.MethodPost, "/api/v2/artifacts/grants/1/"+created.GrantID+":commit", nil))
	commitRec := httptest.NewRecorder()
	router.ServeHTTP(commitRec, commitReq)
	if commitRec.Code == http.StatusNotImplemented {
		t.Fatalf("commitTransferGrant route still resolves to the S7 stub; body=%s", commitRec.Body.String())
	}
	if commitRec.Code != http.StatusOK {
		t.Fatalf("commit grant: status = %d, want 200; body=%s", commitRec.Code, commitRec.Body.String())
	}
}

// TestArtifactMultipartRoutesResolveThroughRealHandlerWhenConfigured is
// S16's equivalent of the S15 check above: internal/api/v2/artifacts's own
// tests (multipart_test.go) exercise PresignUploadPart/
// CompleteMultipartUpload/AbortMultipartUpload directly and never touch the
// mounted route, so only a test in this package can prove router.go's S16
// wiring actually happened. Notably, unlike S15's own plan section, S16's
// plan text never names this requirement and its literal Verify command
// never runs this package at all — an omission relative to S15's own
// explicit warning about exactly this failure mode, recorded as a plan
// defect in docs/plans/storage-migration-plan.md's S16 section (added
// after this gap was found and closed here, not present in the plan's
// original S16 text). alwaysSucceedsArtifactRepo's fixed
// grant always carries an upload_id (fixedMultipartUploadID), so any
// syntactically valid grant id resolves through requireOwnedMultipartGrant
// without needing to create a real grant first.
func TestArtifactMultipartRoutesResolveThroughRealHandlerWhenConfigured(t *testing.T) {

	handler := v2artifacts.NewHandler(alwaysSucceedsArtifactRepo{}, alwaysSucceedsArtifactStore{})
	router := NewRouter(RouterConfig{
		AuthValidator:              testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:         testPrincipalValidator{},
		AppsRepo:                   struct{ applications.Repository }{},
		ArtifactHandler:            handler,
		ArtifactPermissionResolver: fakePermissionResolver{granted: []string{artifactPermissionCreate}},
	})
	const grantID = "11111111-1111-4111-8111-111111111111"

	partReq := testAuthHeader(httptest.NewRequest(http.MethodPost, "/api/v2/artifacts/grants/1/"+grantID+"/parts/1", nil))
	partRec := httptest.NewRecorder()
	router.ServeHTTP(partRec, partReq)
	if partRec.Code == http.StatusNotImplemented {
		t.Fatalf("presignUploadPart route still resolves to the S7 stub; body=%s", partRec.Body.String())
	}
	if partRec.Code != http.StatusOK {
		t.Fatalf("presign part: status = %d, want 200; body=%s", partRec.Code, partRec.Body.String())
	}

	completeReq := testAuthHeader(httptest.NewRequest(http.MethodPost, "/api/v2/artifacts/grants/1/"+grantID+":completeMultipart",
		strings.NewReader(`{"parts":[{"part_number":1,"etag":"x"}]}`)))
	completeReq.Header.Set("Content-Type", "application/json")
	completeRec := httptest.NewRecorder()
	router.ServeHTTP(completeRec, completeReq)
	if completeRec.Code == http.StatusNotImplemented {
		t.Fatalf("completeMultipartUpload route still resolves to the S7 stub; body=%s", completeRec.Body.String())
	}
	if completeRec.Code != http.StatusOK {
		t.Fatalf("complete multipart: status = %d, want 200; body=%s", completeRec.Code, completeRec.Body.String())
	}

	abortReq := testAuthHeader(httptest.NewRequest(http.MethodPost, "/api/v2/artifacts/grants/1/"+grantID+":abortMultipart", nil))
	abortRec := httptest.NewRecorder()
	router.ServeHTTP(abortRec, abortReq)
	if abortRec.Code == http.StatusNotImplemented {
		t.Fatalf("abortMultipartUpload route still resolves to the S7 stub; body=%s", abortRec.Body.String())
	}
	// alwaysSucceedsArtifactRepo.MarkTransferGrantConsumed always succeeds
	// and never actually marks the fixed in-memory grant consumed (it isn't
	// a real, stateful repository), so this call reaches AbortMultipartUpload
	// fully, past requireOwnedMultipartGrant's already-consumed check —
	// unlike the stateful fakeRepo-backed tests in multipart_test.go, this
	// double cannot exercise the "already consumed by the prior complete
	// call above" case.
	if abortRec.Code != http.StatusNoContent {
		t.Fatalf("abort multipart: status = %d, want 204; body=%s", abortRec.Code, abortRec.Body.String())
	}
}

// TestArtifactBucketPatchRequiresEditPermissionNotCreate proves S11's third
// acceptance bullet: PATCH on a bucket is gated on the edit permission, not
// create — a principal holding only create (which authorizes bucket/object
// creation, a materially different operation) is denied, while edit alone is
// sufficient.
func TestArtifactBucketPatchRequiresEditPermissionNotCreate(t *testing.T) {

	cases := []struct {
		name       string
		granted    []string
		wantStatus int
	}{
		{name: "only create granted is insufficient", granted: []string{artifactPermissionCreate}, wantStatus: http.StatusForbidden},
		{name: "edit granted succeeds", granted: []string{artifactPermissionEdit}, wantStatus: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := v2artifacts.NewHandler(alwaysSucceedsArtifactRepo{}, alwaysSucceedsArtifactStore{})
			router := NewRouter(RouterConfig{
				AuthValidator:              testTokenValidator{user: authenticatedTestUser()},
				PrincipalValidator:         testPrincipalValidator{},
				AppsRepo:                   struct{ applications.Repository }{},
				ArtifactHandler:            handler,
				ArtifactPermissionResolver: fakePermissionResolver{granted: tc.granted},
			})

			req := testAuthHeader(httptest.NewRequest(http.MethodPatch, "/api/v2/artifacts/buckets/1/reports", strings.NewReader(`{"is_pinned":true}`)))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestArtifactObjectRouteDeniesPermissionScopedToDifferentProject proves
// S11's fourth acceptance bullet: a request for project 8's object from a
// principal whose granted permissions are scoped to project 7 is denied.
// This goes through the real mounted router (NewRouter), not the handler or
// fakePermissionResolver directly, so it actually proves chi's {projectID}
// URL param threads into the RBAC check correctly — not just that
// fakePermissionResolver's own forProject logic works in isolation.
func TestArtifactObjectRouteDeniesPermissionScopedToDifferentProject(t *testing.T) {

	handler := v2artifacts.NewHandler(alwaysSucceedsArtifactRepo{}, alwaysSucceedsArtifactStore{})
	router := NewRouter(RouterConfig{
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		AppsRepo:           struct{ applications.Repository }{},
		ArtifactHandler:    handler,
		ArtifactPermissionResolver: fakePermissionResolver{
			granted:    []string{artifactPermissionView},
			forProject: "7",
		},
	})

	req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/api/v2/artifacts/objects/8/reports", nil))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}
