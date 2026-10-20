package artifacts_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// The hidden workspace bucket answers 404 BEFORE any access answer, even to
// a caller whose exception denies that name (a 403 would confirm the bucket
// exists). The workspace check and the caller's exception are ONE lookup
// (BucketGate), not a separate bucket-type query per request.
func TestWorkspaceBucketIsAbsentBeforeTheAccessListInOneLookup(t *testing.T) {
	repo := newFakeRepo()
	for _, bucket := range []repos.NewBucketInput{
		{ProjectID: 1, Name: "elitea-workspaces-0a1b2c3d", DisplayName: "w", BucketType: "workspace"},
		{ProjectID: 1, Name: "reports", DisplayName: "reports", BucketType: "local"},
	} {
		if _, err := repo.CreateBucket(t.Context(), bucket); err != nil {
			t.Fatal(err)
		}
	}
	// Member 7 is denied both buckets by an exception.
	if err := repo.ReplaceUserBucketPermissions(t.Context(), 1, 7, map[string][]string{
		"elitea-workspaces-0a1b2c3d": {}, "reports": {},
	}); err != nil {
		t.Fatal(err)
	}
	router := newObjectTestRouter(artifacts.NewHandler(repo, newFakeStore()))
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/objects/1/elitea-workspaces-0a1b2c3d", http.StatusNotFound},
		{http.MethodGet, "/objects/1/elitea-workspaces-0a1b2c3d/t/x.seg", http.StatusNotFound},
		{http.MethodHead, "/objects/1/elitea-workspaces-0a1b2c3d/t/x.seg", http.StatusNotFound},
		// The same exception on an ordinary bucket is a 403: the 404 above
		// is the workspace gate, not the access list.
		{http.MethodGet, "/objects/1/reports", http.StatusForbidden},
	} {
		before := repo.gateCalls
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, withMember(httptest.NewRequest(tc.method, tc.path, nil), 7))
		if rr.Code != tc.want {
			t.Fatalf("%s %s = %d, want %d: %s", tc.method, tc.path, rr.Code, tc.want, rr.Body.String())
		}
		if repo.gateCalls-before != 1 {
			t.Fatalf("%s %s made %d gate lookups, want 1", tc.method, tc.path, repo.gateCalls-before)
		}
	}
	if repo.workspaceCalls != 0 {
		t.Fatalf("%d separate bucket-type queries on the object routes, want 0 (folded into the gate)", repo.workspaceCalls)
	}
}
