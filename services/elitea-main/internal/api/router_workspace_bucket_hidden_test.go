package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	v2artifacts "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// The hidden workspace bucket (ADR-0034 decision 3) is refused with a 404 by
// every user-facing route that addresses a bucket by name.
//
// The test walks the COMPOSED router (chi.Walk) and keeps every route whose
// pattern carries a {bucket} parameter, so a bucket route added later is
// covered without being listed here. The repository fake answers GetBucket
// for ANY name and the resolver grants every artifact permission, so the
// only thing that can produce a 404 is the workspace-bucket gate. The walk must
// find routes, or the test proves nothing.
func TestWorkspaceBucketIsAbsentOnEveryArtifactBucketRoute(t *testing.T) {
	router := NewRouter(RouterConfig{
		AuthValidator:              testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:         testPrincipalValidator{},
		AppsRepo:                   struct{ applications.Repository }{},
		ArtifactHandler:            v2artifacts.NewHandler(alwaysSucceedsArtifactRepo{}, alwaysSucceedsArtifactStore{}),
		ArtifactPermissionResolver: fakePermissionResolver{granted: allArtifactPermissions},
	})
	routes, ok := router.(chi.Routes)
	if !ok {
		t.Fatalf("router %T is not walkable", router)
	}
	type route struct{ method, pattern string }
	var bucketRoutes []route
	if err := chi.Walk(routes, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// The support assistant's attachment read answers 503 for every
		// request when no support project is composed (as here); it reads
		// through the artifact download walked below, which refuses the
		// hidden bucket.
		if strings.Contains(pattern, "{bucket}") && !strings.HasPrefix(pattern, "/api/v2/support_assistant/") {
			bucketRoutes = append(bucketRoutes, route{method, pattern})
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(bucketRoutes) < 15 {
		t.Fatalf("the walk found %d bucket routes; the artifact surface has at least 15", len(bucketRoutes))
	}
	for _, rt := range bucketRoutes {
		path := strings.ReplaceAll(rt.pattern, "{projectID}", "7")
		path = strings.ReplaceAll(path, "{bucket}", storage.WorkspaceBucketName)
		path = strings.ReplaceAll(path, "/*", "/t/0b6f9a52-1c2d-4e5f-8a9b-0c1d2e3f4a5b/seg/x.seg")
		if strings.HasPrefix(path, "/artifacts/s3/") {
			path += "?project_id=7"
		}
		t.Run(rt.method+" "+rt.pattern, func(t *testing.T) {
			var body *bytes.Reader
			switch {
			case strings.Contains(rt.pattern, "/grants/"):
				body = bytes.NewReader([]byte(`{"method":"PUT","content_type":"application/x-git-bundle","max_bytes":10}`))
			case strings.Contains(rt.pattern, ":batchDelete"):
				body = bytes.NewReader([]byte(`{"keys":["a"]}`))
			default:
				body = bytes.NewReader([]byte(`{}`))
			}
			req := testAuthHeader(httptest.NewRequest(rt.method, path, body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 for the hidden workspace bucket; body=%s", rec.Code, rec.Body.String())
			}
		})
		// The same route answers something other than 404 for an ordinary
		// bucket, so the 404 above is the reserved-name gate and not the
		// route itself.
		if rt.method == http.MethodGet && !strings.Contains(rt.pattern, "/*") {
			ordinary := strings.ReplaceAll(strings.ReplaceAll(rt.pattern, "{projectID}", "7"), "{bucket}", "reports")
			if strings.HasPrefix(ordinary, "/artifacts/s3/") {
				ordinary += "?project_id=7"
			}
			req := testAuthHeader(httptest.NewRequest(rt.method, ordinary, nil))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s %s answers 404 for an ordinary bucket too; the walk proves nothing", rt.method, ordinary)
			}
		}
	}
}

// TestWorkspaceBucketNameCannotBeCreatedOrGivenAnAccessList: a reserved name
// is an invalid name on create (400, never the 409 that would reveal the
// hidden bucket), and the access-list routes neither store nor show it.
func TestWorkspaceBucketNameCannotBeCreatedOrGivenAnAccessList(t *testing.T) {
	router := NewRouter(RouterConfig{
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		AppsRepo:           struct{ applications.Repository }{},
		ArtifactHandler:    v2artifacts.NewHandler(alwaysSucceedsArtifactRepo{}, alwaysSucceedsArtifactStore{}),
		ArtifactPermissionResolver: fakePermissionResolver{granted: append(
			append([]string{}, allArtifactPermissions...), artifactPermissionACLView, artifactPermissionACLEdit)},
	})
	send := func(method, path string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := testAuthHeader(httptest.NewRequest(method, path, bytes.NewReader(raw)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := send(http.MethodPost, "/api/v2/artifacts/buckets/7", map[string]any{"name": storage.WorkspaceBucketName}); rec.Code != http.StatusBadRequest {
		t.Fatalf("create with the reserved name = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if rec := send(http.MethodPost, "/api/v2/artifacts/buckets/7", map[string]any{"name": "reports"}); rec.Code != http.StatusOK {
		t.Fatalf("create with an ordinary name = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec := send(http.MethodPut, "/api/v2/artifacts/bucket_permissions/7", map[string]any{
		"user_id": 3, "bucket_permissions": map[string][]string{storage.WorkspaceBucketName: {"read"}},
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("an access list for the reserved name = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if rec := send(http.MethodDelete, "/api/v2/artifacts/bucket_permissions/7", map[string]any{
		"user_id": 3, "bucket": storage.WorkspaceBucketName,
	}); rec.Code != http.StatusNotFound {
		t.Fatalf("deleting the reserved name's access list = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// hiddenSuffixedBucketRepo is a project where a user bucket called
// elitea-workspaces existed before ADR-0034, so the hidden bucket got a
// suffixed name. The gate finds the hidden bucket by TYPE.
type hiddenSuffixedBucketRepo struct{ alwaysSucceedsArtifactRepo }

const suffixedWorkspaceBucket = storage.WorkspaceBucketName + "-0a1b2c3d"

func (hiddenSuffixedBucketRepo) IsWorkspaceBucket(_ context.Context, _ int64, name string) (bool, error) {
	return name == suffixedWorkspaceBucket, nil
}

func (hiddenSuffixedBucketRepo) BucketGate(_ context.Context, _, _ int64, name string) (repos.BucketGate, error) {
	return repos.BucketGate{Workspace: name == suffixedWorkspaceBucket}, nil
}

// TestUserBucketOfTheReservedNameKeepsWorking: the gate is by type, never by
// name. A user bucket called elitea-workspaces answers as before; the hidden
// bucket under its suffixed name answers 404.
func TestUserBucketOfTheReservedNameKeepsWorking(t *testing.T) {
	router := NewRouter(RouterConfig{
		AuthValidator:              testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:         testPrincipalValidator{},
		AppsRepo:                   struct{ applications.Repository }{},
		ArtifactHandler:            v2artifacts.NewHandler(hiddenSuffixedBucketRepo{}, alwaysSucceedsArtifactStore{}),
		ArtifactPermissionResolver: fakePermissionResolver{granted: allArtifactPermissions},
	})
	get := func(path string) int {
		req := testAuthHeader(httptest.NewRequest(http.MethodGet, path, nil))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, path := range []string{
		"/api/v2/artifacts/buckets/7/" + storage.WorkspaceBucketName,
		"/api/v2/artifacts/objects/7/" + storage.WorkspaceBucketName,
		"/api/v2/artifacts/objects/7/" + storage.WorkspaceBucketName + "/report.txt",
	} {
		if code := get(path); code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200 for the user bucket", path, code)
		}
		hidden := strings.Replace(path, storage.WorkspaceBucketName, suffixedWorkspaceBucket, 1)
		if code := get(hidden); code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404 for the hidden bucket", hidden, code)
		}
	}
}
