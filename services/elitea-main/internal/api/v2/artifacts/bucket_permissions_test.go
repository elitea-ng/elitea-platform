package artifacts_test

// Per-bucket access lists: the routes that author them, and the enforcement on
// every object and bucket verb.
//
// EVERY TEST HERE CARRIES AN AUTHENTICATED PRINCIPAL. That is the point of
// withMember: an exception names a PERSON, so a request with no owning user
// skips the second gate entirely (bucket_permissions.go, callerUserID). A test
// that forgot the principal would pass for the wrong reason on every deny case
// — it would be measuring the skip, not the refusal.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	platformauth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// withMember attaches an authenticated principal owned by userID.
func withMember(req *http.Request, userID int64) *http.Request {
	user := platformauth.User{ID: strconv.FormatInt(userID, 10), UserID: strconv.FormatInt(userID, 10)}
	return req.WithContext(platformauth.ContextWithUser(req.Context(), user))
}

func newACLTestRouter(h *artifacts.Handler) chi.Router {
	r := chi.NewRouter()
	r.Get("/bucket_permissions/{projectID}", h.ListBucketPermissions)
	r.Put("/bucket_permissions/{projectID}", h.SetBucketPermissions)
	r.Delete("/bucket_permissions/{projectID}", h.DeleteBucketPermission)
	return r
}

// newACLFixture seeds one project with one bucket and returns the repository
// so a test can author exceptions against it.
func newACLFixture(t *testing.T) (*artifacts.Handler, *fakeRepo, *fakeStore) {
	t.Helper()
	repo := newFakeRepo()
	if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 1, Name: "reports", DisplayName: "reports", BucketType: "local",
	}); err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}
	store := newFakeStore()
	return artifacts.NewHandler(repo, store), repo, store
}

/* ── the authoring routes ─────────────────────────────────────────────── */

func TestListBucketPermissions_EmptyProjectAnswersEmptyRows(t *testing.T) {
	h, _, _ := newACLFixture(t)
	rr := httptest.NewRecorder()
	newACLTestRouter(h).ServeHTTP(rr, withMember(httptest.NewRequest(http.MethodGet, "/bucket_permissions/1", nil), 7))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Total int              `json:"total"`
		Rows  []map[string]any `json:"rows"`
	}
	decodeJSON(t, rr.Body, &body)
	if body.Total != 0 {
		t.Errorf("expected total 0, got %d", body.Total)
	}
	if body.Rows == nil {
		t.Error("expected rows to be an empty array, not null")
	}
}

func TestSetBucketPermissions_StoresAndListsTheException(t *testing.T) {
	h, repo, _ := newACLFixture(t)
	router := newACLTestRouter(h)

	payload := bytes.NewBufferString(`{"user_id":7,"bucket_permissions":{"reports":["read"]}}`)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, withMember(httptest.NewRequest(http.MethodPut, "/bucket_permissions/1", payload), 1))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	permissions, found, err := repo.GetBucketPermission(t.Context(), 1, 7, "reports")
	if err != nil {
		t.Fatalf("GetBucketPermission: %v", err)
	}
	if !found || len(permissions) != 1 || permissions[0] != "read" {
		t.Fatalf("stored exception = %v (found=%v), want [read]", permissions, found)
	}

	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, withMember(httptest.NewRequest(http.MethodGet, "/bucket_permissions/1", nil), 1))
	var body struct {
		Total int `json:"total"`
		Rows  []struct {
			UserID            int64               `json:"user_id"`
			BucketPermissions map[string][]string `json:"bucket_permissions"`
		} `json:"rows"`
	}
	decodeJSON(t, rr.Body, &body)
	if body.Total != 1 || len(body.Rows) != 1 || body.Rows[0].UserID != 7 {
		t.Fatalf("listing = %+v, want one row for user 7", body)
	}
	if got := body.Rows[0].BucketPermissions["reports"]; len(got) != 1 || got[0] != "read" {
		t.Errorf("listed exception = %v, want [read]", got)
	}
}

// The empty ARRAY is the legacy "no access" encoding. It must survive the
// round trip as `[]` and never as `null`, because null decodes back as "no
// exception at all" — the opposite decision.
func TestSetBucketPermissions_NoAccessSurvivesAsEmptyArray(t *testing.T) {
	h, _, _ := newACLFixture(t)
	router := newACLTestRouter(h)

	payload := bytes.NewBufferString(`{"user_id":7,"bucket_permissions":{"reports":[]}}`)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, withMember(httptest.NewRequest(http.MethodPut, "/bucket_permissions/1", payload), 1))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, withMember(httptest.NewRequest(http.MethodGet, "/bucket_permissions/1", nil), 1))
	raw := rr.Body.String()
	if !bytes.Contains([]byte(raw), []byte(`"reports":[]`)) {
		t.Fatalf("listing did not encode no-access as an empty array: %s", raw)
	}
}

// The PUT REPLACES the member's whole map. The reference UI removes an
// exception by sending the map without that key, so a merge would make removal
// impossible.
func TestSetBucketPermissions_ReplacesRatherThanMerges(t *testing.T) {
	h, repo, _ := newACLFixture(t)
	router := newACLTestRouter(h)
	repo.setException(1, 7, "reports", []string{"read"})

	payload := bytes.NewBufferString(`{"user_id":7,"bucket_permissions":{}}`)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, withMember(httptest.NewRequest(http.MethodPut, "/bucket_permissions/1", payload), 1))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	_, found, err := repo.GetBucketPermission(t.Context(), 1, 7, "reports")
	if err != nil {
		t.Fatalf("GetBucketPermission: %v", err)
	}
	if found {
		t.Error("the replaced map still carries the removed exception")
	}
}

func TestSetBucketPermissions_RejectsAnUnknownVerb(t *testing.T) {
	h, _, _ := newACLFixture(t)
	payload := bytes.NewBufferString(`{"user_id":7,"bucket_permissions":{"reports":["admin"]}}`)
	rr := httptest.NewRecorder()
	newACLTestRouter(h).ServeHTTP(rr, withMember(httptest.NewRequest(http.MethodPut, "/bucket_permissions/1", payload), 1))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSetBucketPermissions_RequiresUserID(t *testing.T) {
	h, _, _ := newACLFixture(t)
	payload := bytes.NewBufferString(`{"bucket_permissions":{}}`)
	rr := httptest.NewRecorder()
	newACLTestRouter(h).ServeHTTP(rr, withMember(httptest.NewRequest(http.MethodPut, "/bucket_permissions/1", payload), 1))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDeleteBucketPermission_RemovesOneEntry(t *testing.T) {
	h, repo, _ := newACLFixture(t)
	repo.setException(1, 7, "reports", []string{"read"})

	payload := bytes.NewBufferString(`{"user_id":7,"bucket":"reports"}`)
	rr := httptest.NewRecorder()
	newACLTestRouter(h).ServeHTTP(rr, withMember(httptest.NewRequest(http.MethodDelete, "/bucket_permissions/1", payload), 1))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	if _, found, _ := repo.GetBucketPermission(t.Context(), 1, 7, "reports"); found {
		t.Error("the exception is still stored")
	}
}

func TestDeleteBucketPermission_AbsentEntryIs404(t *testing.T) {
	h, _, _ := newACLFixture(t)
	payload := bytes.NewBufferString(`{"user_id":7,"bucket":"reports"}`)
	rr := httptest.NewRecorder()
	newACLTestRouter(h).ServeHTTP(rr, withMember(httptest.NewRequest(http.MethodDelete, "/bucket_permissions/1", payload), 1))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

/* ── enforcement, per verb ────────────────────────────────────────────── */

// objectVerb names one request against the object plane, so the deny/allow
// matrix below runs the SAME set of verbs for each subject.
type objectVerb struct {
	name    string
	request func(t *testing.T) *http.Request
	// write reports whether the verb needs `write`, which decides what a
	// read-only exception should do to it.
	write bool
}

func objectVerbs() []objectVerb {
	return []objectVerb{
		{name: "list", write: false, request: func(*testing.T) *http.Request {
			return httptest.NewRequest(http.MethodGet, "/objects/1/reports", nil)
		}},
		{name: "download", write: false, request: func(*testing.T) *http.Request {
			return httptest.NewRequest(http.MethodGet, "/objects/1/reports/a.txt", nil)
		}},
		{name: "stat", write: false, request: func(*testing.T) *http.Request {
			return httptest.NewRequest(http.MethodHead, "/objects/1/reports/a.txt", nil)
		}},
		{name: "upload", write: true, request: func(t *testing.T) *http.Request {
			return newUploadRequest(t, "/objects/1/reports", "a.txt", []byte("hello"))
		}},
		{name: "delete", write: true, request: func(*testing.T) *http.Request {
			return httptest.NewRequest(http.MethodDelete, "/objects/1/reports/a.txt", nil)
		}},
		{name: "batchDelete", write: true, request: func(*testing.T) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/objects/1/reports:batchDelete",
				bytes.NewBufferString(`{"keys":["a.txt"]}`))
		}},
	}
}

// serveObjectVerb runs one verb through the real object router and answers the
// status code.
func serveObjectVerb(t *testing.T, h *artifacts.Handler, verb objectVerb, userID int64) int {
	t.Helper()
	rr := httptest.NewRecorder()
	newObjectTestRouter(h).ServeHTTP(rr, withMember(verb.request(t), userID))
	return rr.Code
}

// A member with NO exception keeps full access — the legacy default, and the
// state every project starts in.
func TestObjectVerbs_MemberWithNoExceptionIsAllowed(t *testing.T) {
	for _, verb := range objectVerbs() {
		t.Run(verb.name, func(t *testing.T) {
			h, _, _ := newACLFixture(t)
			if code := serveObjectVerb(t, h, verb, 7); code == http.StatusForbidden {
				t.Fatalf("%s answered 403 with no exception stored", verb.name)
			}
		})
	}
}

// A member listed with read+write keeps full access — the exception exists but
// refuses nothing.
func TestObjectVerbs_MemberListedWithReadWriteIsAllowed(t *testing.T) {
	for _, verb := range objectVerbs() {
		t.Run(verb.name, func(t *testing.T) {
			h, repo, _ := newACLFixture(t)
			repo.setException(1, 7, "reports", []string{"read", "write"})
			if code := serveObjectVerb(t, h, verb, 7); code == http.StatusForbidden {
				t.Fatalf("%s answered 403 for a read/write exception", verb.name)
			}
		})
	}
}

// A member blocked outright (the legacy `[]`) is refused on EVERY verb,
// including the reads.
func TestObjectVerbs_BlockedMemberIsRefusedOnEveryVerb(t *testing.T) {
	for _, verb := range objectVerbs() {
		t.Run(verb.name, func(t *testing.T) {
			h, repo, _ := newACLFixture(t)
			repo.setException(1, 7, "reports", []string{})
			if code := serveObjectVerb(t, h, verb, 7); code != http.StatusForbidden {
				t.Fatalf("%s answered %d, want 403 for a blocked member", verb.name, code)
			}
		})
	}
}

// A read-only member reads and does not write. This is the case that proves
// the verb argument reaches the check: a single boolean gate would answer the
// same for both halves.
func TestObjectVerbs_ReadOnlyMemberReadsButDoesNotWrite(t *testing.T) {
	for _, verb := range objectVerbs() {
		t.Run(verb.name, func(t *testing.T) {
			h, repo, _ := newACLFixture(t)
			repo.setException(1, 7, "reports", []string{"read"})
			code := serveObjectVerb(t, h, verb, 7)
			if verb.write && code != http.StatusForbidden {
				t.Fatalf("%s answered %d, want 403 for a read-only member", verb.name, code)
			}
			if !verb.write && code == http.StatusForbidden {
				t.Fatalf("%s answered 403 for a read-only member on a read verb", verb.name)
			}
		})
	}
}

// A project admin passes every verb even with a blocking exception against
// their own account.
func TestObjectVerbs_ProjectAdminBypassesTheException(t *testing.T) {
	for _, verb := range objectVerbs() {
		t.Run(verb.name, func(t *testing.T) {
			h, repo, _ := newACLFixture(t)
			repo.setException(1, 7, "reports", []string{})
			repo.setProjectAdmin(1, 7)
			if code := serveObjectVerb(t, h, verb, 7); code == http.StatusForbidden {
				t.Fatalf("%s answered 403 for a project admin", verb.name)
			}
		})
	}
}

// An exception on ONE bucket says nothing about another bucket.
func TestObjectVerbs_AnExceptionIsScopedToItsBucket(t *testing.T) {
	h, repo, _ := newACLFixture(t)
	if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 1, Name: "datasets", DisplayName: "datasets", BucketType: "local",
	}); err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}
	repo.setException(1, 7, "reports", []string{})

	rr := httptest.NewRecorder()
	newObjectTestRouter(h).ServeHTTP(rr,
		withMember(httptest.NewRequest(http.MethodGet, "/objects/1/datasets", nil), 7))
	if rr.Code == http.StatusForbidden {
		t.Fatalf("a `reports` exception refused a `datasets` read")
	}
}

/* ── enforcement on the bucket plane ──────────────────────────────────── */

func TestBucketVerbs_BlockedMemberCannotReadUpdateOrDeleteTheBucket(t *testing.T) {
	cases := []struct {
		name    string
		request func() *http.Request
	}{
		{"get", func() *http.Request { return httptest.NewRequest(http.MethodGet, "/buckets/1/reports", nil) }},
		{"patch", func() *http.Request {
			return httptest.NewRequest(http.MethodPatch, "/buckets/1/reports", bytes.NewBufferString(`{"is_pinned":true}`))
		}},
		{"delete", func() *http.Request { return httptest.NewRequest(http.MethodDelete, "/buckets/1/reports", nil) }},
	}
	for _, verb := range cases {
		t.Run(verb.name, func(t *testing.T) {
			h, repo, _ := newACLFixture(t)
			repo.setException(1, 7, "reports", []string{})
			rr := httptest.NewRecorder()
			newTestRouter(h).ServeHTTP(rr, withMember(verb.request(), 7))
			if rr.Code != http.StatusForbidden {
				t.Fatalf("%s answered %d, want 403", verb.name, rr.Code)
			}
		})
	}
}

// DELETE is the verb legacy leaves ungated (api/v2/buckets.py:284-293). A
// read-only member must not be able to delete the bucket they may only read.
func TestDeleteBucket_ReadOnlyMemberIsRefused(t *testing.T) {
	h, repo, _ := newACLFixture(t)
	repo.setException(1, 7, "reports", []string{"read"})
	rr := httptest.NewRecorder()
	newTestRouter(h).ServeHTTP(rr,
		withMember(httptest.NewRequest(http.MethodDelete, "/buckets/1/reports", nil), 7))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

/* ── the bucket listing filter ────────────────────────────────────────── */

func listedBucketNames(t *testing.T, h *artifacts.Handler, userID int64) []string {
	t.Helper()
	rr := httptest.NewRecorder()
	newTestRouter(h).ServeHTTP(rr, withMember(httptest.NewRequest(http.MethodGet, "/buckets/1", nil), userID))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Buckets []artifacts.Bucket `json:"buckets"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	names := make([]string, 0, len(body.Buckets))
	for _, bucket := range body.Buckets {
		names = append(names, bucket.Name)
	}
	return names
}

func TestListBuckets_HidesOnlyTheBlockedBucket(t *testing.T) {
	h, repo, _ := newACLFixture(t)
	for _, name := range []string{"datasets", "logs"} {
		if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
			ProjectID: 1, Name: name, DisplayName: name, BucketType: "local",
		}); err != nil {
			t.Fatalf("seed CreateBucket: %v", err)
		}
	}
	// One blocked, one read-only. The read-only one stays VISIBLE: the legacy
	// whitelist would have shown only the two named buckets and hidden `logs`,
	// which this member may read.
	repo.setException(1, 7, "reports", []string{})
	repo.setException(1, 7, "datasets", []string{"read"})

	names := listedBucketNames(t, h, 7)
	for _, name := range names {
		if name == "reports" {
			t.Fatalf("the blocked bucket is listed: %v", names)
		}
	}
	if len(names) != 2 {
		t.Fatalf("listed %v, want datasets and logs", names)
	}
}

func TestListBuckets_ProjectAdminSeesEveryBucket(t *testing.T) {
	h, repo, _ := newACLFixture(t)
	repo.setException(1, 7, "reports", []string{})
	repo.setProjectAdmin(1, 7)
	if names := listedBucketNames(t, h, 7); len(names) != 1 {
		t.Fatalf("listed %v, want the blocked bucket to stay visible for an admin", names)
	}
}

/* ── enforcement on the S3 representation the SDK speaks ──────────────── */

// The S3 routes reach the SAME objects as the native ones. An exception that
// refuses a verb there must refuse it here, or the S3 representation is a
// softer way in rather than a second rendering of one policy.
func TestS3Verbs_BlockedMemberIsRefusedOnEveryVerb(t *testing.T) {
	cases := []struct {
		name    string
		request func() *http.Request
	}{
		{"list", func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/artifacts/s3/reports?project_id=1", nil)
		}},
		{"download", func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/artifacts/s3/reports/a.txt?project_id=1", nil)
		}},
		{"stat", func() *http.Request {
			return httptest.NewRequest(http.MethodHead, "/artifacts/s3/reports/a.txt?project_id=1", nil)
		}},
		{"upload", func() *http.Request {
			return httptest.NewRequest(http.MethodPut, "/artifacts/s3/reports/a.txt?project_id=1",
				bytes.NewBufferString("hello"))
		}},
		{"delete", func() *http.Request {
			return httptest.NewRequest(http.MethodDelete, "/artifacts/s3/reports/a.txt?project_id=1", nil)
		}},
	}
	for _, verb := range cases {
		t.Run(verb.name, func(t *testing.T) {
			h, repo, _ := newACLFixture(t)
			repo.setException(1, 7, "reports", []string{})
			rr := httptest.NewRecorder()
			newS3TestRouter(h).ServeHTTP(rr, withMember(verb.request(), 7))
			if rr.Code != http.StatusForbidden {
				t.Fatalf("%s answered %d, want 403", verb.name, rr.Code)
			}
		})
	}
}

func TestS3Verbs_ReadOnlyMemberReadsButDoesNotWrite(t *testing.T) {
	h, repo, _ := newACLFixture(t)
	repo.setException(1, 7, "reports", []string{"read"})
	router := newS3TestRouter(h)

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, withMember(
		httptest.NewRequest(http.MethodGet, "/artifacts/s3/reports?project_id=1", nil), 7))
	if rr.Code == http.StatusForbidden {
		t.Fatalf("the S3 listing answered 403 for a read-only member")
	}

	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, withMember(
		httptest.NewRequest(http.MethodPut, "/artifacts/s3/reports/a.txt?project_id=1",
			bytes.NewBufferString("hello")), 7))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("the S3 upload answered %d, want 403 for a read-only member", rr.Code)
	}
}

// A principal with no owning user — a workload identity — is not a person an
// exception can name, so the second gate does not apply to it. This is the
// legacy guard (`if user_id and bucket …`), and it is what keeps an index run
// working while its owner carries an unrelated exception.
func TestObjectVerbs_PrincipalWithNoOwningUserSkipsTheCheck(t *testing.T) {
	h, repo, _ := newACLFixture(t)
	repo.setException(1, 7, "reports", []string{})

	rr := httptest.NewRecorder()
	newObjectTestRouter(h).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/objects/1/reports", nil))
	if rr.Code == http.StatusForbidden {
		t.Fatalf("an unowned principal was refused by an exception naming user 7")
	}
}
