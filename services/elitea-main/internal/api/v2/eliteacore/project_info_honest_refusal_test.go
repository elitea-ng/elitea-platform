package eliteacore_test

// The prototype project-info read and the project-icon write path.
//
// Every test in this file fails against the code as it stood before this
// change, and each one fails on a 200 — which is the point: the defect these
// cover was never a crash or a 500, it was a success status carrying a value
// nothing had produced. A test that only asserted "2xx" passed throughout.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// listingIconStore adds the one method fakeIconObjectStore answers
// ErrNotSupported for. It is a separate type rather than a change to that
// fake so the S20b tests keep measuring exactly what they measured.
type listingIconStore struct {
	*fakeIconObjectStore
}

func newListingIconStore() *listingIconStore {
	return &listingIconStore{fakeIconObjectStore: newFakeIconObjectStore()}
}

func (s *listingIconStore) List(
	_ context.Context,
	query storage.ListQuery,
) (storage.ListPage, error) {
	prefix := query.Bucket.ProjectID() + "/" + iconBucketNameForTest + "/"
	var page storage.ListPage
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, data := range s.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		name := strings.TrimPrefix(key, prefix)
		if !strings.HasPrefix(name, query.KeyPrefix) {
			continue
		}
		page.Objects = append(page.Objects, storage.ObjectInfo{
			Key:  name,
			Size: int64(len(data)),
		})
	}
	return page, nil
}

// The bucket name is not exported by the handler package; it is a constant of
// the storage layout, and DownloadIcon's route is what pins it.
const iconBucketNameForTest = "icons"

// newHandlerWithUnreachablePool returns a handler whose pool is real but whose
// server does not exist, so every query fails at connect time.
//
// A nil pool would not do: the handlers these tests cover PANIC on nil, and a
// panic is a different failure from the one under test. What is under test is
// the branch that used to swallow a query error and answer 200 anyway — which
// is only reachable with a pool that exists and cannot answer. pgxpool.New
// connects lazily, so nothing here dials until the handler does.
func newHandlerWithUnreachablePool(t *testing.T) *eliteacore.Handler {
	t.Helper()
	pool, err := pgxpool.New(
		context.Background(),
		"postgres://elitea:elitea@127.0.0.1:1/elitea?connect_timeout=1",
	)
	if err != nil {
		t.Fatalf("build unreachable pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return eliteacore.NewHandler(pool)
}

func decodeCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	body := decodeObj(t, recorder)
	code, _ := body["code"].(string)
	return code
}

// TestProjectInfoRefusesInsteadOfInventingAShape
//
// RED against the previous handler, on a 200: with a database it could not
// reach, it answered {"name":"","icon_meta":null} — an empty name masking the
// connection failure, a nil icon that was a literal rather than a read, and no
// teammates_count at all, which the web client renders as 0.
func TestProjectInfoRefusesInsteadOfInventingAShape(t *testing.T) {
	recorder := httptest.NewRecorder()
	newHandlerWithUnreachablePool(t).ProjectInfo(
		recorder,
		newRequest(http.MethodGet, "/", map[string]string{"projectID": "7"}, nil),
	)

	if recorder.Code == http.StatusOK {
		t.Fatalf("status = 200 — a deployment with no project-info source must not " +
			"answer as though it had one")
	}
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", recorder.Code, recorder.Body.String())
	}
	if code := decodeCode(t, recorder); code != "project_info_not_available" {
		t.Fatalf("code = %q, want a machine-readable project_info_not_available", code)
	}
}

// TestUpdateProjectInfoRefusesToReportAWriteItDidNotMake
//
// RED against the previous handler on both rows, both on a 200:
//
//   - icon_meta was never read at all, so the ONLY thing the web client ever
//     PUTs here was parsed, dropped, and answered {"ok":true};
//   - the rename discarded its Exec error and answered {"ok":true} too.
func TestUpdateProjectInfoRefusesToReportAWriteItDidNotMake(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"icon", `{"icon_meta":{"name":"a.png","url":"/icons/7/a.png"}}`},
		{"rename", `{"name":"Renamed"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			newHandlerWithUnreachablePool(t).UpdateProjectInfo(
				recorder,
				newRequest(
					http.MethodPut, "/",
					map[string]string{"projectID": "7"},
					strings.NewReader(test.body),
				),
			)

			if recorder.Code == http.StatusOK {
				t.Fatalf("status = 200 — a write the database never accepted was "+
					"reported as saved; body=%s", recorder.Body.String())
			}
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body=%s", recorder.Code, recorder.Body.String())
			}
			if code := decodeCode(t, recorder); code != "project_info_write_failed" {
				t.Fatalf("code = %q", code)
			}
		})
	}
}

// TestUpdateProjectInfoRejectsAnUnusableIconMeta proves the request is
// actually inspected: the previous handler read only body["name"] and would
// have answered 200 to any icon_meta at all, including this one.
func TestUpdateProjectInfoRejectsAnUnusableIconMeta(t *testing.T) {
	body := strings.NewReader(`{"icon_meta":{"url":42}}`)
	recorder := httptest.NewRecorder()
	newHandler().UpdateProjectInfo(
		recorder,
		newRequest(http.MethodPut, "/", map[string]string{"projectID": "7"}, body),
	)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	if code := decodeCode(t, recorder); code != "invalid_icon_meta" {
		t.Fatalf("code = %q, want invalid_icon_meta", code)
	}
}

// TestProjectIconRoutesRefuseWithoutStorage
//
// RED against the previous handlers on all three rows: the listing answered
// 200 with an empty page, the create answered 200 with a fabricated URL, and
// the delete answered 204 — none of them had storage, and none said so.
func TestProjectIconRoutesRefuseWithoutStorage(t *testing.T) {
	handler := newHandler()

	t.Run("list", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.ListProjectIcons(
			recorder,
			newRequest(http.MethodGet, "/", map[string]string{"projectID": "7"}, nil),
		)
		if recorder.Code == http.StatusOK {
			t.Fatal("status = 200 with an empty page — an empty grid is indistinguishable " +
				"from a store that was never consulted")
		}
		if recorder.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501", recorder.Code)
		}
		if code := decodeCode(t, recorder); code != "icon_storage_not_configured" {
			t.Fatalf("code = %q", code)
		}
	})

	t.Run("create", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.CreateProjectIcon(recorder, iconUploadRequest(t, "7", "icon.png", []byte("x")))
		if recorder.Code == http.StatusOK {
			t.Fatalf("status = 200 — the upload was discarded and a URL invented; body=%s",
				recorder.Body.String())
		}
		if recorder.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501", recorder.Code)
		}
	})

	t.Run("delete", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.DeleteProjectIcon(
			recorder,
			newRequest(http.MethodDelete, "/", map[string]string{"projectID": "7", "name": "pi_a.png"}, nil),
		)
		if recorder.Code == http.StatusNoContent {
			t.Fatal("status = 204 — nothing was deleted and the caller was told otherwise")
		}
		if recorder.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501", recorder.Code)
		}
	})
}

// TestProjectIconUploadIsStoredListedAndDeleted is the write path this change
// builds: the bytes survive the request, the listing finds them, the URL is
// one DownloadIcon already serves, and a delete really removes the object.
//
// RED against the previous handlers at the second step: the listing was a
// hardcoded empty page, so an uploaded icon was never listed.
func TestProjectIconUploadIsStoredListedAndDeleted(t *testing.T) {
	store := newListingIconStore()
	handler := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	content := []byte("PNGDATA")
	recorder := httptest.NewRecorder()
	handler.CreateProjectIcon(recorder, iconUploadRequest(t, "7", "logo.png", content))
	if recorder.Code != http.StatusOK {
		t.Fatalf("create status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	created := decodeObj(t, recorder)
	name, _ := created["name"].(string)
	iconURL, _ := created["url"].(string)
	if !strings.HasPrefix(name, "pi_") || !strings.HasSuffix(name, ".png") {
		t.Fatalf("stored name = %q", name)
	}
	if iconURL != "/icons/7/"+name {
		t.Fatalf("url = %q — must be the path DownloadIcon serves", iconURL)
	}

	// The bytes are really there, at the ref DownloadIcon builds.
	download := httptest.NewRecorder()
	eliteacore.DownloadIcon(store).ServeHTTP(
		download,
		newRequest(http.MethodGet, "/", map[string]string{"projectID": "7", "filename": name}, nil),
	)
	if download.Code != http.StatusOK {
		t.Fatalf("download status = %d — the upload was not stored where it is served from",
			download.Code)
	}
	if got, _ := io.ReadAll(download.Body); !bytes.Equal(got, content) {
		t.Fatalf("downloaded %q, want %q", got, content)
	}

	listed := httptest.NewRecorder()
	handler.ListProjectIcons(
		listed,
		newRequest(http.MethodGet, "/", map[string]string{"projectID": "7"}, nil),
	)
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d", listed.Code)
	}
	listing := decodeObj(t, listed)
	rows, _ := listing["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v — an uploaded icon must appear in the picker", listing["rows"])
	}
	row, _ := rows[0].(map[string]any)
	if row["name"] != name || row["url"] != iconURL {
		t.Fatalf("listed row = %v, want the created icon", row)
	}

	deleted := httptest.NewRecorder()
	handler.DeleteProjectIcon(
		deleted,
		newRequest(http.MethodDelete, "/", map[string]string{"projectID": "7", "name": name}, nil),
	)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", deleted.Code)
	}

	relisted := httptest.NewRecorder()
	handler.ListProjectIcons(
		relisted,
		newRequest(http.MethodGet, "/", map[string]string{"projectID": "7"}, nil),
	)
	after, _ := decodeObj(t, relisted)["rows"].([]any)
	if len(after) != 0 {
		t.Fatalf("rows after delete = %v — the icon came back", after)
	}
}

// TestListProjectIconsIgnoresApplicationIcons pins the prefix that keeps the
// two families apart inside the one bucket DownloadIcon can serve.
func TestListProjectIconsIgnoresApplicationIcons(t *testing.T) {
	store := newListingIconStore()
	handler := eliteacore.NewHandler(nil, eliteacore.WithObjectStore(store))

	uploaded := httptest.NewRecorder()
	handler.UploadIcon(uploaded, iconUploadRequest(t, "7", "app.png", []byte("app")))
	if uploaded.Code != http.StatusOK {
		t.Fatalf("application icon upload status = %d", uploaded.Code)
	}

	listed := httptest.NewRecorder()
	handler.ListProjectIcons(
		listed,
		newRequest(http.MethodGet, "/", map[string]string{"projectID": "7"}, nil),
	)
	rows, _ := decodeObj(t, listed)["rows"].([]any)
	if len(rows) != 0 {
		t.Fatalf("rows = %v — an application icon is not a project icon", rows)
	}
}
