package artifacts_test

// Review round 2: a turn's Office host reaches this public object API under
// the actor's own PAT and names its claim. Under a named claim a SYSTEM
// bucket is reachable only under the project's attachment bucket and the
// claim's own conversation, for read and write; a person's own requests (no
// claim named) keep the per-bucket access list.

import (
	"bytes"
	"context"
	"encoding/base64"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

const (
	ownConversation   = "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1"
	otherConversation = "0f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1"
	turnActor         = int64(7)
)

// claimResolver answers one live claim (fence "a" x32) for turnActor in
// project 1, conversation ownConversation, and refuses every other.
type claimResolver struct{}

func (claimResolver) ResolveRuntimeClaim(_ context.Context, claim artifacts.RuntimeClaim) (artifacts.RuntimeClaimScope, error) {
	if claim.ClaimID != "claim-1" || string(claim.FenceToken) != strings.Repeat("a", 32) {
		return artifacts.RuntimeClaimScope{}, artifacts.ErrRuntimeClaimRefused
	}
	return artifacts.RuntimeClaimScope{ProjectID: 1, ActorID: turnActor, ConversationID: ownConversation}, nil
}

func withClaim(req *http.Request, claimID string) *http.Request {
	req.Header.Set(artifacts.RuntimeClaimExecutionHeader, "execution-1")
	req.Header.Set(artifacts.RuntimeClaimGenerationHeader, "1")
	req.Header.Set(artifacts.RuntimeClaimIDHeader, claimID)
	req.Header.Set(artifacts.RuntimeClaimFenceHeader, base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("a", 32))))
	return req
}

func systemBucketFixture(t *testing.T) http.Handler {
	t.Helper()
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
	h := artifacts.NewHandler(repo, newFakeStore()).WithRuntimeClaims(claimResolver{})
	return newObjectTestRouter(h)
}

func upload(t *testing.T, router http.Handler, bucket, key string, claimed bool) int {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", key)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("data"))
	_ = form.Close()
	req := httptest.NewRequest(http.MethodPost, "/objects/1/"+bucket, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req = withMember(req, turnActor)
	if claimed {
		req = withClaim(req, "claim-1")
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr.Code
}

func download(t *testing.T, router http.Handler, path string, claimID string) int {
	t.Helper()
	req := withMember(httptest.NewRequest(http.MethodGet, path, nil), turnActor)
	if claimID != "" {
		req = withClaim(req, claimID)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr.Code
}

func TestATurnReachesOnlyItsOwnConversationInASystemBucket(t *testing.T) {
	t.Parallel()
	router := systemBucketFixture(t)
	attachments := storage.DefaultAttachmentBucket

	// Own conversation: write (a data_analysis output next to its source) and read.
	if code := upload(t, router, attachments, ownConversation+"/report.xlsx", true); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("own-conversation write = %d", code)
	}
	if code := download(t, router, "/objects/1/"+attachments+"/"+ownConversation+"/report.xlsx", "claim-1"); code != http.StatusOK {
		t.Fatalf("own-conversation read = %d", code)
	}
	if code := download(t, router, "/objects/1/"+attachments+"?prefix="+ownConversation+"/", "claim-1"); code != http.StatusOK {
		t.Fatalf("own-conversation listing = %d", code)
	}

	// Another conversation: refused for read and write.
	if code := upload(t, router, attachments, otherConversation+"/x.xlsx", true); code != http.StatusForbidden {
		t.Fatalf("other-conversation write = %d, want 403", code)
	}
	if code := download(t, router, "/objects/1/"+attachments+"/"+otherConversation+"/x.xlsx", "claim-1"); code != http.StatusForbidden {
		t.Fatalf("other-conversation read = %d, want 403", code)
	}
	// An unscoped listing, another system bucket, a claim that is not live.
	for path, claimID := range map[string]string{
		"/objects/1/" + attachments:                                          "claim-1",
		"/objects/1/index-artifacts/" + ownConversation + "/result.json":     "claim-1",
		"/objects/1/" + attachments + "/" + ownConversation + "/report.xlsx": "forged",
	} {
		if code := download(t, router, path, claimID); code != http.StatusForbidden {
			t.Fatalf("%s (%s) = %d, want 403", path, claimID, code)
		}
	}
	if code := upload(t, router, "index-artifacts", ownConversation+"/x.json", true); code != http.StatusForbidden {
		t.Fatalf("index-artifacts write = %d, want 403", code)
	}

	// A user bucket is not judged by the system-bucket rule.
	if code := upload(t, router, "reports", "anything.txt", true); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("user bucket write under a claim = %d", code)
	}
	// A person's own request (no claim named) is unaffected.
	if code := download(t, router, "/objects/1/"+attachments+"/"+ownConversation+"/report.xlsx", ""); code != http.StatusOK {
		t.Fatalf("user session read = %d", code)
	}
}

// Review round 2: the platform's system bucket names are reserved.
func TestCreateBucketRefusesReservedNames(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	policyBucket := "team-uploads"
	repo.setPolicy(repos.ProjectStoragePolicy{ProjectID: 1, AttachmentBucket: &policyBucket})
	router := newTestRouter(artifacts.NewHandler(repo, newFakeStore()))
	for _, name := range []string{storage.DefaultAttachmentBucket, "index-artifacts", policyBucket} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/buckets/1", strings.NewReader(`{"name":"`+name+`"}`)))
		if rr.Code != http.StatusConflict {
			t.Fatalf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/buckets/1", strings.NewReader(`{"name":"my-files"}`)))
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("an ordinary name: %d %s", rr.Code, rr.Body.String())
	}
}

// claimed adds the turn's claim and the actor to a request.
func claimed(req *http.Request) *http.Request {
	return withClaim(withMember(req, turnActor), "claim-1")
}

// systemVerbsFixture: the attachment, index and a provisioned `reports`
// system bucket, every verb routed, on a store with native multipart.
func systemVerbsFixture(t *testing.T) (http.Handler, *fakeStore) {
	t.Helper()
	repo := newFakeRepo()
	for _, name := range []string{storage.DefaultAttachmentBucket, "index-artifacts", "reports"} {
		if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
			ProjectID: 1, Name: name, DisplayName: name, BucketType: "system",
		}); err != nil {
			t.Fatalf("seed bucket: %v", err)
		}
	}
	store := newFakeStore()
	store.multipart = true
	h := artifacts.NewHandler(repo, store).WithRuntimeClaims(claimResolver{})
	r := chi.NewRouter()
	r.Get("/buckets/{projectID}/{bucket}", h.GetBucket)
	r.Patch("/buckets/{projectID}/{bucket}", h.UpdateBucket)
	r.Delete("/buckets/{projectID}/{bucket}", h.DeleteBucket)
	r.Get("/objects/{projectID}/{bucket}", h.ListObjects)
	r.Post("/objects/{projectID}/{bucket}", h.UploadObject)
	r.Post("/objects/{projectID}/{bucket}:batchDelete", h.BatchDeleteObjects)
	r.Get("/objects/{projectID}/{bucket}/*", h.DownloadObject)
	r.Post("/grants/{projectID}/{bucket}", h.CreateTransferGrant)
	r.Post("/grants/{projectID}/{grantID}:commit", h.CommitTransferGrant)
	r.Post("/grants/{projectID}/{grantID}/parts/{partNumber}", h.PresignUploadPart)
	r.Post("/grants/{projectID}/{grantID}:completeMultipart", h.CompleteMultipartUpload)
	r.Post("/grants/{projectID}/{grantID}:abortMultipart", h.AbortMultipartUpload)
	return r, store
}

func serve(router http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// Review round 3: every verb under a claim is judged on what it addresses.
func TestEveryVerbIsJudgedUnderAClaim(t *testing.T) {
	t.Parallel()
	router, _ := systemVerbsFixture(t)
	attachments := storage.DefaultAttachmentBucket
	own := ownConversation + "/notes.txt"
	other := otherConversation + "/notes.txt"

	// Bucket-level verbs address no key: refused for a confined bucket,
	// the access list's for `reports`.
	for _, verb := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
		body := strings.NewReader(`{}`)
		if rr := serve(router, claimed(httptest.NewRequest(verb, "/buckets/1/"+attachments, body))); rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s under a claim = %d, want 403", verb, attachments, rr.Code)
		}
	}
	if rr := serve(router, claimed(httptest.NewRequest(http.MethodGet, "/buckets/1/reports", nil))); rr.Code != http.StatusOK {
		t.Fatalf("GET reports under a claim = %d, want 200", rr.Code)
	}

	// `?prefix=` scopes a LISTING only.
	if rr := serve(router, claimed(httptest.NewRequest(http.MethodGet, "/objects/1/"+attachments+"?prefix="+ownConversation+"/", nil))); rr.Code != http.StatusOK {
		t.Fatalf("own-conversation listing = %d", rr.Code)
	}
	if rr := serve(router, claimed(httptest.NewRequest(http.MethodGet, "/buckets/1/"+attachments+"?prefix="+ownConversation+"/", nil))); rr.Code != http.StatusForbidden {
		t.Fatalf("a bucket verb scoped by ?prefix= = %d, want 403", rr.Code)
	}

	// BatchDeleteObjects: every key.
	batch := func(keys string) int {
		req := httptest.NewRequest(http.MethodPost, "/objects/1/"+attachments+":batchDelete", strings.NewReader(`{"keys":`+keys+`}`))
		return serve(router, claimed(req)).Code
	}
	if code := batch(`["` + own + `","` + other + `"]`); code != http.StatusForbidden {
		t.Fatalf("a batch with another conversation's key = %d, want 403", code)
	}
	if code := batch(`["` + own + `"]`); code != http.StatusOK {
		t.Fatalf("an own-conversation batch = %d, want 200", code)
	}

	// CreateTransferGrant reserves a server-derived key, never under the
	// conversation: refused in the attachment bucket, the ACL's in `reports`.
	grant := func(bucket string, claim bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/grants/1/"+bucket,
			strings.NewReader(`{"method":"PUT","content_type":"application/octet-stream","max_bytes":104857601}`))
		req.Header.Set("Content-Type", "application/json")
		req = withMember(req, turnActor)
		if claim {
			req = withClaim(req, "claim-1")
		}
		return serve(router, req)
	}
	if rr := grant(attachments, true); rr.Code != http.StatusForbidden {
		t.Fatalf("a grant into the attachment bucket under a claim = %d, want 403", rr.Code)
	}
	if rr := grant("reports", true); rr.Code != http.StatusOK {
		t.Fatalf("a grant into reports under a claim = %d: %s", rr.Code, rr.Body.String())
	}

	// A multipart grant a person started in the attachment bucket: its
	// continuations under a claim are judged on the grant's key.
	started := grant(attachments, false)
	if started.Code != http.StatusOK {
		t.Fatalf("a user's grant = %d: %s", started.Code, started.Body.String())
	}
	var issued struct {
		GrantID string `json:"grant_id"`
	}
	decodeJSON(t, started.Body, &issued)
	for _, path := range []string{
		"/grants/1/" + issued.GrantID + "/parts/1",
		"/grants/1/" + issued.GrantID + ":completeMultipart",
		"/grants/1/" + issued.GrantID + ":abortMultipart",
		"/grants/1/" + issued.GrantID + ":commit",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"parts":[]}`))
		if rr := serve(router, claimed(req)); rr.Code != http.StatusForbidden {
			t.Fatalf("%s under a claim = %d, want 403", path, rr.Code)
		}
	}
	// Without a claim the person's own continuation still works.
	if rr := serve(router, withMember(httptest.NewRequest(http.MethodPost, "/grants/1/"+issued.GrantID+"/parts/1", nil), turnActor)); rr.Code != http.StatusOK {
		t.Fatalf("the person's own part presign = %d: %s", rr.Code, rr.Body.String())
	}

	// A provisioned `reports` system bucket: upload and read under a claim.
	if code := upload(t, router, "reports", "q3/summary.xlsx", true); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("reports write under a claim = %d", code)
	}
	if code := download(t, router, "/objects/1/reports/q3/summary.xlsx", "claim-1"); code != http.StatusOK {
		t.Fatalf("reports read under a claim = %d", code)
	}
}

// The execution id alone is the request-log tag every SDK platform call
// carries (api_extra_headers), not a claim: a claim-less attachment read
// tagged with it keeps the person's access. A claim id or fence without the
// rest of the claim is malformed and refused.
func TestAnExecutionTagAloneIsNotAClaim(t *testing.T) {
	t.Parallel()
	router := systemBucketFixture(t)
	path := "/objects/1/" + storage.DefaultAttachmentBucket + "/" + otherConversation + "/report.pdf"
	if code := upload(t, router, storage.DefaultAttachmentBucket, otherConversation+"/report.pdf", false); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("seed upload = %d", code)
	}
	read := func(headers map[string]string) int {
		req := withMember(httptest.NewRequest(http.MethodGet, path, nil), turnActor)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		return serve(router, req).Code
	}
	if code := read(map[string]string{artifacts.RuntimeClaimExecutionHeader: "0123456789abcdef0123456789abcdef"}); code != http.StatusOK {
		t.Fatalf("a read tagged only with the execution id = %d, want 200", code)
	}
	if code := read(map[string]string{
		artifacts.RuntimeClaimExecutionHeader:  "0123456789abcdef0123456789abcdef",
		artifacts.RuntimeClaimGenerationHeader: "1",
	}); code != http.StatusOK {
		t.Fatalf("a read tagged with execution and generation = %d, want 200", code)
	}
	for _, partial := range []map[string]string{
		{artifacts.RuntimeClaimIDHeader: "claim-1"},
		{artifacts.RuntimeClaimFenceHeader: "AAAA"},
		{artifacts.RuntimeClaimExecutionHeader: "execution-1", artifacts.RuntimeClaimIDHeader: "claim-1"},
	} {
		if code := read(partial); code != http.StatusForbidden {
			t.Fatalf("a partial claim %v = %d, want 403", partial, code)
		}
	}
}
