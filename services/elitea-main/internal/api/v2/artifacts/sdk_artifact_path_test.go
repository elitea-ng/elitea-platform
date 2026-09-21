package artifacts_test

// The SDK's by-filepath object read (#978).
//
// `elitea-sdk`'s client builds ONE url for a file argument —
// `{base}/api/v2/artifacts/artifact/default/{project_id}/{bucket}/{key}`
// (runtime/clients/client.py:126, :1235) — and `download_artifact` maps a 404
// on it to the literal sentence "Resource not found". Nothing served that
// path, so every toolkit that turns a `/{bucket}/{filename}` argument into
// bytes (`download_artifact_by_filepath` → `get_raw_content_by_filepath`, which
// is what qTest's `upload_attachment_to_test_run` and `add_file_to_test_case`
// call) answered that sentence for a valid path exactly as it did for a typo.
//
// The route is an ALIAS of DownloadObject, so what has to be proven is the
// things an alias can get wrong:
//
//   - the key is read from the trailing wildcard, so a nested key stays ONE
//     key rather than an unmatched path;
//   - the body is the RAW bytes (the SDK returns `data.content` and hands it
//     to the toolkit as the file — a JSON envelope would arrive as the file's
//     contents);
//   - the content type is the same extension-derived one the object plane
//     answers, because that is what a qTest blob-handle upload sends on;
//   - the per-bucket access list gates it, which is the one thing a second
//     registration of the same handler can silently lose.
//
// The alias is also asserted to be byte-for-byte the same answer as the
// `/objects/` route for the same object: a copy that drifts from the original
// is the failure mode a hand-written second handler would have.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
)

// newSDKArtifactTestRouter mounts the alias exactly as production mounts it
// (mountArtifactRoutes, internal/api/router.go): the literal `default` mode
// segment the SDK sends, and a trailing wildcard for the key.
func newSDKArtifactTestRouter(h *artifacts.Handler) chi.Router {
	r := chi.NewRouter()
	r.Get("/artifact/default/{projectID}/{bucket}/*", h.DownloadObject)
	return r
}

func TestSDKArtifactPathServesTheObjectBytes(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seedContent("1", "reports", "run-12/summary.txt", []byte("attach me"), "")

	rr := httptest.NewRecorder()
	newSDKArtifactTestRouter(h).ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/artifact/default/1/reports/run-12/summary.txt", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "attach me" {
		t.Fatalf("body = %q, want the raw object bytes", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want the extension-derived type", got)
	}
}

// The SDK sends the same key the user typed, and an absent one must stay
// distinguishable from an absent ROUTE only by the body — both are 404s, but
// the typed envelope is what says the endpoint exists.
func TestSDKArtifactPathAnswersTheTypedEnvelopeForAnAbsentKey(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)

	rr := httptest.NewRecorder()
	newSDKArtifactTestRouter(h).ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/artifact/default/1/reports/nothing-here.txt", nil))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeJSON(t, rr.Body, &envelope)
	if envelope.Error.Code == "" {
		t.Fatalf("404 body is not the typed error envelope: %s", rr.Body.String())
	}
}

// The alias answers exactly what the object plane answers, header and body.
func TestSDKArtifactPathMatchesTheObjectPlaneAnswer(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seedContent("1", "reports", "a/b/c.png", []byte{0x89, 'P', 'N', 'G'}, "")

	native := httptest.NewRecorder()
	newObjectTestRouter(h).ServeHTTP(native,
		httptest.NewRequest(http.MethodGet, "/objects/1/reports/a/b/c.png", nil))
	alias := httptest.NewRecorder()
	newSDKArtifactTestRouter(h).ServeHTTP(alias,
		httptest.NewRequest(http.MethodGet, "/artifact/default/1/reports/a/b/c.png", nil))

	if native.Code != alias.Code {
		t.Fatalf("alias status = %d, object-plane status = %d", alias.Code, native.Code)
	}
	if native.Body.String() != alias.Body.String() {
		t.Fatalf("alias body differs from the object-plane body")
	}
	if got, want := alias.Header().Get("Content-Type"), native.Header().Get("Content-Type"); got != want {
		t.Fatalf("alias Content-Type = %q, object-plane Content-Type = %q", got, want)
	}
}

// The per-bucket access list refuses the alias for a blocked member, which is
// the gate a second registration of the same handler is most likely to lose:
// it lives inside requireBucket, so it is only kept by reusing the handler.
func TestSDKArtifactPathIsRefusedForABlockedMember(t *testing.T) {
	h, repo, store := newACLFixture(t)
	store.seedContent("1", "reports", "a.txt", []byte("secret"), "")
	repo.setException(1, 7, "reports", []string{})

	rr := httptest.NewRecorder()
	newSDKArtifactTestRouter(h).ServeHTTP(rr,
		withMember(httptest.NewRequest(http.MethodGet, "/artifact/default/1/reports/a.txt", nil), 7))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a member the bucket blocks; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() == "secret" {
		t.Fatal("the refused read answered the object's bytes")
	}
}
