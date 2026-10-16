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
