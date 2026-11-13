package artifacts_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

func moveRouter(h *artifacts.Handler) chi.Router {
	r := chi.NewRouter()
	r.Post("/objects/{projectID}/{bucket}:move", h.MoveObject)
	return r
}

func postMove(r http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/objects/1/reports:move", strings.NewReader(body))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func newObjectInput(bucketID int64, key string, size int64) repos.NewObjectInput {
	return repos.NewObjectInput{BucketID: bucketID, Key: key, ByteLength: size, MediaType: "application/octet-stream"}
}

func storeHas(s *fakeStore, key string) bool {
	ref, err := storage.NewObjectRef("1", "reports", key)
	if err != nil {
		panic(err)
	}
	_, err = s.Stat(context.Background(), ref)
	return err == nil
}

func TestMoveObject_RenamesAndKeepsTheBytes(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	store.seedContent("1", "reports", "dir/old.png", []byte("PNGDATA"), "image/png")
	row, _ := repo.GetBucket(t.Context(), 1, "reports")
	if _, err := repo.UpsertObject(t.Context(), newObjectInput(row.ID, "dir/old.png", 7)); err != nil {
		t.Fatal(err)
	}

	rr := postMove(moveRouter(h), `{"from":"dir/old.png","to":"dir/new.png"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Key       string `json:"key"`
		SizeBytes int64  `json:"size_bytes"`
		MediaType string `json:"media_type"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Key != "dir/new.png" || got.SizeBytes != 7 || got.MediaType != "image/png" {
		t.Fatalf("response = %+v", got)
	}
	if storeHas(store, "dir/old.png") || !storeHas(store, "dir/new.png") {
		t.Fatal("expected the object under the new key only")
	}
	if total, _ := repo.SumBucketBytes(t.Context(), row.ID); total != 7 {
		t.Fatalf("bucket bytes = %d, want 7 (old metadata row gone, new one present)", total)
	}
}

func TestMoveObject_TargetExistsIsConflictUnlessOverwrite(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seedContent("1", "reports", "a.txt", []byte("AAA"), "text/plain")
	store.seedContent("1", "reports", "b.txt", []byte("BBBB"), "text/plain")
	r := moveRouter(h)

	rr := postMove(r, `{"from":"a.txt","to":"b.txt"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rr.Code, rr.Body.String())
	}
	if !storeHas(store, "a.txt") {
		t.Fatal("a refused move must leave the source in place")
	}

	rr = postMove(r, `{"from":"a.txt","to":"b.txt","overwrite":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("overwrite status = %d: %s", rr.Code, rr.Body.String())
	}
	if storeHas(store, "a.txt") || store.objectCount() != 1 {
		t.Fatal("overwrite must replace the target and remove the source")
	}
}

func TestMoveObject_Refusals(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seedContent("1", "reports", "a.txt", []byte("A"), "")
	r := moveRouter(h)

	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"missing source": {`{"from":"nope.txt","to":"x.txt"}`, http.StatusNotFound},
		"empty to":       {`{"from":"a.txt","to":""}`, http.StatusBadRequest},
		"same key":       {`{"from":"a.txt","to":"a.txt"}`, http.StatusBadRequest},
		"dotdot target":  {`{"from":"a.txt","to":"../x.txt"}`, http.StatusBadRequest},
		"bad json":       {`{`, http.StatusBadRequest},
	} {
		if rr := postMove(r, tc.body); rr.Code != tc.want {
			t.Errorf("%s: status = %d, want %d: %s", name, rr.Code, tc.want, rr.Body.String())
		}
	}
	if !storeHas(store, "a.txt") || store.objectCount() != 1 {
		t.Fatal("refusals must not change the store")
	}
}

func TestMoveObject_UnknownBucketIs404(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/objects/1/nobucket:move", strings.NewReader(`{"from":"a","to":"b"}`))
	rr := httptest.NewRecorder()
	moveRouter(h).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func seedMovable(t *testing.T, repo *fakeRepo, store *fakeStore, key string, data []byte) int64 {
	t.Helper()
	store.seedContent("1", "reports", key, data, "text/plain")
	row, _ := repo.GetBucket(t.Context(), 1, "reports")
	if _, err := repo.UpsertObject(t.Context(), newObjectInput(row.ID, key, int64(len(data)))); err != nil {
		t.Fatal(err)
	}
	return row.ID
}

func TestMoveObject_RenameKeepsRetentionAndAge(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	bucketID := seedMovable(t, repo, store, "a.txt", []byte("AAA"))
	created := time.Now().Add(-48 * time.Hour).UTC()
	expires := time.Now().Add(24 * time.Hour).UTC()
	in := newObjectInput(bucketID, "a.txt", 3)
	in.CreatedAt, in.ExpiresAt = &created, &expires
	if _, err := repo.UpsertObject(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	// A bucket retention that would give a later deadline if recomputed.
	days := int32(30)
	if _, err := repo.UpdateBucketRetention(t.Context(), bucketID, &days, nil); err != nil {
		t.Fatal(err)
	}
	if rr := postMove(moveRouter(h), `{"from":"a.txt","to":"b.txt"}`); rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	rows, _ := repo.ListObjects(t.Context(), bucketID, "b.txt")
	if len(rows) != 1 || !rows[0].CreatedAt.Equal(created) || rows[0].ExpiresAt == nil || !rows[0].ExpiresAt.Equal(expires) {
		t.Fatalf("renamed row = %+v, want created %v expires %v", rows, created, expires)
	}
}

func TestMoveObject_ClaimedTargetIsConflictEvenWithoutBytes(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	bucketID := seedMovable(t, repo, store, "a.txt", []byte("AAA"))
	// A racing writer holds the name in the metadata table; bytes not yet there.
	if got, _ := repo.ReserveObjectKey(t.Context(), bucketID, "b.txt", "text/plain", nil); !got {
		t.Fatal("seed claim")
	}
	if rr := postMove(moveRouter(h), `{"from":"a.txt","to":"b.txt"}`); rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	if !storeHas(store, "a.txt") || len(repo.objectKeys(bucketID)) != 2 {
		t.Fatal("source and the other writer's claim must both survive")
	}
}

func TestMoveObject_CopyFailureReleasesOnlyItsOwnClaim(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	bucketID := seedMovable(t, repo, store, "a.txt", []byte("AAA"))
	store.failPutKey = "b.txt"
	if rr := postMove(moveRouter(h), `{"from":"a.txt","to":"b.txt"}`); rr.Code == http.StatusOK {
		t.Fatal("expected a failure")
	}
	if keys := repo.objectKeys(bucketID); len(keys) != 1 || keys[0] != "a.txt" || !storeHas(store, "a.txt") {
		t.Fatalf("keys = %v: the claim must be released and the source kept", keys)
	}
}

func TestMoveObject_OverwriteFailureNeverDeletesTheTarget(t *testing.T) {
	for name, setup := range map[string]func(*fakeRepo, *fakeStore){
		"copy fails":    func(_ *fakeRepo, s *fakeStore) { s.failPutKey = "b.txt" },
		"upsert fails":  func(r *fakeRepo, _ *fakeStore) { r.failUpsertKey = "b.txt" },
		"src row fails": func(r *fakeRepo, _ *fakeStore) { r.failDeleteKey = "a.txt" },
	} {
		t.Run(name, func(t *testing.T) {
			h, repo, store := newObjectTestHandler(t)
			bucketID := seedMovable(t, repo, store, "a.txt", []byte("AAA"))
			seedMovable(t, repo, store, "b.txt", []byte("BBBB"))
			setup(repo, store)
			if rr := postMove(moveRouter(h), `{"from":"a.txt","to":"b.txt","overwrite":true}`); rr.Code == http.StatusOK {
				t.Fatal("expected a failure")
			}
			if !storeHas(store, "b.txt") || !storeHas(store, "a.txt") {
				t.Fatal("the pre-existing target and the source must both survive")
			}
			if keys := repo.objectKeys(bucketID); len(keys) != 2 {
				t.Fatalf("keys = %v, want a.txt and b.txt rows", keys)
			}
		})
	}
}

func TestMoveObject_FailureAfterClaimTakesTheCopyBack(t *testing.T) {
	for name, setup := range map[string]func(*fakeRepo){
		"upsert fails":  func(r *fakeRepo) { r.failUpsertKey = "b.txt" },
		"src row fails": func(r *fakeRepo) { r.failDeleteKey = "a.txt" },
	} {
		t.Run(name, func(t *testing.T) {
			h, repo, store := newObjectTestHandler(t)
			bucketID := seedMovable(t, repo, store, "a.txt", []byte("AAA"))
			setup(repo)
			if rr := postMove(moveRouter(h), `{"from":"a.txt","to":"b.txt"}`); rr.Code == http.StatusOK {
				t.Fatal("expected a failure")
			}
			if storeHas(store, "b.txt") || !storeHas(store, "a.txt") {
				t.Fatal("the copy this call made must go, the source must stay")
			}
			if keys := repo.objectKeys(bucketID); len(keys) != 1 || keys[0] != "a.txt" {
				t.Fatalf("keys = %v, want only a.txt", keys)
			}
		})
	}
}

func TestMoveObject_SourceBytesDeleteFailureStillSucceeds(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	bucketID := seedMovable(t, repo, store, "a.txt", []byte("AAA"))
	store.failDeleteKey = "a.txt"
	if rr := postMove(moveRouter(h), `{"from":"a.txt","to":"b.txt"}`); rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	if keys := repo.objectKeys(bucketID); len(keys) != 1 || keys[0] != "b.txt" || !storeHas(store, "b.txt") {
		t.Fatalf("keys = %v, want only b.txt", keys)
	}
}

func TestMoveObject_FallbackCopyIsCappedAtTheUploadLimit(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	bucketID := seedMovable(t, repo, store, "a.txt", []byte("0123456789"))
	limit := int64(4)
	repo.setPolicy(repos.ProjectStoragePolicy{ProjectID: 1, MaxObjectBytes: &limit})
	rr := postMove(moveRouter(h), `{"from":"a.txt","to":"b.txt"}`)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rr.Code, rr.Body.String())
	}
	if storeHas(store, "b.txt") || len(repo.objectKeys(bucketID)) != 1 {
		t.Fatal("a refused copy must leave no target bytes or claim")
	}
}

func TestMoveObject_CancelledContextStopsTheCopy(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	bucketID := seedMovable(t, repo, store, "a.txt", []byte("AAA"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/objects/1/reports:move", strings.NewReader(`{"from":"a.txt","to":"b.txt"}`)).WithContext(ctx)
	rr := httptest.NewRecorder()
	moveRouter(h).ServeHTTP(rr, req)
	if rr.Code == http.StatusOK || storeHas(store, "b.txt") || len(repo.objectKeys(bucketID)) != 1 {
		t.Fatalf("status = %d: a cancelled move must change nothing", rr.Code)
	}
}
