package artifacts_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
