package artifacts_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// #2167: indexing an artifacts toolkit made `index-artifacts` (and its
// `toolkit-discovery` files) appear among a person's buckets. It is the
// platform's own storage, so the listing and every bucket route leave it out,
// while an ordinary bucket and the attachment bucket are untouched.
func TestInternalIndexBucketIsNotListedNorReachable(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	for _, bucket := range []struct{ name, kind string }{
		{storage.DefaultAttachmentBucket, "system"},
		{"index-artifacts", "system"},
		{"reports", "local"},
	} {
		if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
			ProjectID: 1, Name: bucket.name, DisplayName: bucket.name, BucketType: bucket.kind,
		}); err != nil {
			t.Fatalf("seed bucket: %v", err)
		}
	}
	h := artifacts.NewHandler(repo, newFakeStore())
	router := chi.NewRouter()
	router.Get("/buckets/{projectID}", h.ListBuckets)
	router.Get("/buckets/{projectID}/{bucket}", h.GetBucket)
	router.Get("/objects/{projectID}/{bucket}", h.ListObjects)
	router.Get("/objects/{projectID}/{bucket}/*", h.DownloadObject)

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/buckets/1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Buckets []artifacts.Bucket `json:"buckets"`
	}
	decodeJSON(t, rr.Body, &resp)
	var names []string
	for _, bucket := range resp.Buckets {
		names = append(names, bucket.Name)
	}
	if strings.Join(names, ",") != storage.DefaultAttachmentBucket+",reports" && strings.Join(names, ",") != "reports,"+storage.DefaultAttachmentBucket {
		t.Fatalf("listed buckets = %v, want the attachment bucket and reports only", names)
	}
	for _, path := range []string{"/buckets/1/index-artifacts", "/objects/1/index-artifacts", "/objects/1/index-artifacts/toolkit-discovery/x"} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, rr.Code)
		}
	}
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/buckets/1/reports", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("an ordinary bucket = %d, want 200", rr.Code)
	}
}
