package artifacts_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// These tests pin the S3-shaped listing the SDK's artifact toolkit consumes.
//
// They deliberately decode into map[string]any rather than a struct with
// json tags: a struct would silently accept a renamed or re-cased field
// (encoding/json just leaves the zero value), which is precisely the
// regression that produced the original defect — a listing that decodes to
// "no files" makes an index run report success having indexed nothing. Only
// asserting on the raw key set catches that.

func newS3TestRouter(h *artifacts.Handler) chi.Router {
	r := chi.NewRouter()
	r.Get("/artifacts/s3/{bucket}", h.ListObjectsS3)
	// Mounted exactly as production mounts it (mountArtifactRoutes in
	// internal/api/router.go): a trailing wildcard, so a key containing
	// slashes is one key rather than an unmatched path.
	r.Get("/artifacts/s3/{bucket}/*", h.DownloadObjectS3)
	// The write verbs share that wildcard path exactly as production mounts
	// them, so a key containing slashes is one key for every verb.
	r.Put("/artifacts/s3/{bucket}/*", h.UploadObjectS3)
	r.Delete("/artifacts/s3/{bucket}/*", h.DeleteObjectS3)
	r.Head("/artifacts/s3/{bucket}/*", h.StatObjectS3)
	return r
}

// getS3Listing performs the request and returns the decoded body as a raw
// map, so tests can assert on exact key names.
func getS3Listing(t *testing.T, h *artifacts.Handler, target string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rr := httptest.NewRecorder()
	newS3TestRouter(h).ServeHTTP(rr, req)

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (body=%s)", err, rr.Body.String())
	}
	return rr.Code, body
}

// TestListObjectsS3_UsesExactSDKKeyNames is the shape-regression guard. It
// asserts the camelCase envelope and item field names the SDK reads by exact
// name (elitea-sdk runtime/clients/artifact.py:123-127, :203-217) and, just
// as importantly, that the native route's snake_case names are NOT what this
// route emits — the two representations share a query but must not share a
// vocabulary.
func TestListObjectsS3_UsesExactSDKKeyNames(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seed("1", "reports", "a.png", 10)

	code, body := getS3Listing(t, h, "/artifacts/s3/reports?project_id=1&format=json&list-type=2")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}

	if _, ok := body["contents"]; !ok {
		t.Fatalf(`response has no "contents" key; keys=%v`, keysOf(body))
	}
	if _, ok := body["commonPrefixes"]; !ok {
		t.Fatalf(`response has no "commonPrefixes" key; keys=%v`, keysOf(body))
	}
	// The native route's names must not leak into this representation.
	for _, forbidden := range []string{"objects", "common_prefixes"} {
		if _, ok := body[forbidden]; ok {
			t.Errorf("response carries native-route key %q; the SDK reads camelCase only", forbidden)
		}
	}

	contents, ok := body["contents"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("contents = %v, want exactly 1 entry", body["contents"])
	}
	item, ok := contents[0].(map[string]any)
	if !ok {
		t.Fatalf("contents[0] is not an object: %v", contents[0])
	}
	// Every field the SDK reads, by exact name and with the right value.
	if got := item["key"]; got != "a.png" {
		t.Errorf(`item["key"] = %v, want "a.png"`, got)
	}
	if got := item["size"]; got != float64(10) {
		t.Errorf(`item["size"] = %v, want 10`, got)
	}
	if _, ok := item["lastModified"]; !ok {
		t.Errorf(`item has no "lastModified" key; keys=%v`, keysOf(item))
	}
	for _, forbidden := range []string{"size_bytes", "modified_at", "last_modified", "LastModified"} {
		if _, ok := item[forbidden]; ok {
			t.Errorf("item carries %q; the SDK reads lastModified/size only", forbidden)
		}
	}
}

// TestListObjectsS3_EmptyBucketEmitsArraysNotNull proves an empty listing is
// [] rather than null. The SDK reads result.get('contents', []) and iterates
// it — a JSON null decodes to Python None and raises on iteration instead of
// yielding an empty listing.
func TestListObjectsS3_EmptyBucketEmitsArraysNotNull(t *testing.T) {
	h, _, _ := newObjectTestHandler(t)

	code, body := getS3Listing(t, h, "/artifacts/s3/reports?project_id=1&list-type=2")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}
	for _, key := range []string{"contents", "commonPrefixes"} {
		v, ok := body[key].([]any)
		if !ok {
			t.Fatalf("%s = %v (%T), want an array", key, body[key], body[key])
		}
		if len(v) != 0 {
			t.Errorf("%s = %v, want empty", key, v)
		}
	}
}

// TestListObjectsS3_DelimiterGroupsFolders covers the folder-listing call the
// SDK makes: prefix with a trailing slash plus delimiter=/. Nested keys must
// roll up into commonPrefixes as plain strings (the shape this route commits
// to) and must NOT appear in contents.
func TestListObjectsS3_DelimiterGroupsFolders(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seed("1", "reports", "docs/top.txt", 5)
	store.seed("1", "reports", "docs/nested/deep.txt", 7)

	code, body := getS3Listing(t, h, "/artifacts/s3/reports?project_id=1&list-type=2&prefix=docs%2F&delimiter=%2F")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}

	contents, _ := body["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents = %v, want only the non-nested key", body["contents"])
	}
	if got := contents[0].(map[string]any)["key"]; got != "docs/top.txt" {
		t.Errorf("listed key = %v, want docs/top.txt", got)
	}

	prefixes, _ := body["commonPrefixes"].([]any)
	if len(prefixes) != 1 {
		t.Fatalf("commonPrefixes = %v, want exactly one group", body["commonPrefixes"])
	}
	// The chosen shape is a plain string, not {"prefix": "..."}. The SDK
	// tolerates both, so only an explicit type assertion pins which one this
	// route actually emits.
	got, ok := prefixes[0].(string)
	if !ok {
		t.Fatalf("commonPrefixes[0] = %v (%T), want a plain string", prefixes[0], prefixes[0])
	}
	if got != "docs/nested/" {
		t.Errorf("commonPrefixes[0] = %q, want docs/nested/", got)
	}
}

// TestListObjectsS3_OmittedDelimiterListsRecursively covers the other call
// shape: the SDK omits delimiter entirely for a recursive listing
// (client.py:1109 only sets it when truthy). An absent delimiter must flatten
// every nested key into contents and group nothing — this is the call the
// indexing path makes, so getting it wrong reproduces the zero-document run.
func TestListObjectsS3_OmittedDelimiterListsRecursively(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seed("1", "reports", "docs/top.txt", 5)
	store.seed("1", "reports", "docs/nested/deep.txt", 7)

	code, body := getS3Listing(t, h, "/artifacts/s3/reports?project_id=1&list-type=2&prefix=docs%2F")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}

	contents, _ := body["contents"].([]any)
	if len(contents) != 2 {
		t.Fatalf("contents = %v, want both keys listed recursively", body["contents"])
	}
	seen := map[string]bool{}
	for _, entry := range contents {
		seen[entry.(map[string]any)["key"].(string)] = true
	}
	if !seen["docs/top.txt"] || !seen["docs/nested/deep.txt"] {
		t.Errorf("recursive listing = %v, want both docs/top.txt and docs/nested/deep.txt", seen)
	}
	if prefixes, _ := body["commonPrefixes"].([]any); len(prefixes) != 0 {
		t.Errorf("commonPrefixes = %v, want none when no delimiter is given", prefixes)
	}
}

// TestListObjectsS3_ScopesBucketLookupToQueryProject proves the handler
// resolves the bucket in the project named by project_id and nowhere else.
// The bucket "reports" exists only in project 1; asking for it as project 2
// must not find it. This is the handler-level half of the cross-tenant
// guarantee — the RBAC half (a caller who cannot even name project 2) is
// proven at the router level in internal/api.
func TestListObjectsS3_ScopesBucketLookupToQueryProject(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seed("1", "reports", "secret.txt", 42)

	code, body := getS3Listing(t, h, "/artifacts/s3/reports?project_id=2&list-type=2")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for another project's bucket; body=%v", code, body)
	}
	if got := errorCodeOf(t, body); got != "NoSuchBucket" {
		t.Errorf("error.code = %q, want NoSuchBucket", got)
	}
}

// TestListObjectsS3_ErrorEnvelopeMatchesWhatTheSDKReads pins the error shape
// _handle_s3_error parses: a JSON body with error.code, in the S3 code
// vocabulary its S3_ERROR_MESSAGES table can phrase (client.py:1060-1067).
// A code outside that table degrades to "S3 error: <code>", so the exact
// strings matter.
func TestListObjectsS3_ErrorEnvelopeMatchesWhatTheSDKReads(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing bucket",
			target:     "/artifacts/s3/nope?project_id=1&list-type=2",
			wantStatus: http.StatusNotFound,
			wantCode:   "NoSuchBucket",
		},
		{
			// storage.ValidateKeyPrefix rejects "..", which surfaces as
			// InvalidKey internally and must reach the SDK as the S3
			// InvalidArgument it knows how to phrase.
			name:       "traversal prefix",
			target:     "/artifacts/s3/reports?project_id=1&prefix=..%2Fetc",
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidArgument",
		},
		{
			name:       "missing project_id",
			target:     "/artifacts/s3/reports?list-type=2",
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidArgument",
		},
		{
			name:       "non-numeric project_id",
			target:     "/artifacts/s3/reports?project_id=notanumber",
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidArgument",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newObjectTestHandler(t)
			code, body := getS3Listing(t, h, tc.target)
			if code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%v", code, tc.wantStatus, body)
			}
			if got := errorCodeOf(t, body); got != tc.wantCode {
				t.Errorf("error.code = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

// TestListObjectsS3_AcceptsLowercasedBucketName mirrors the SDK, which
// lower-cases the bucket before building the URL (client.py:1105).
func TestListObjectsS3_AcceptsLowercasedBucketName(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seed("1", "reports", "a.png", 10)

	// "REPORTS" stands in for a caller that did not lower-case; the stored
	// bucket is "reports".
	code, body := getS3Listing(t, h, "/artifacts/s3/REPORTS?project_id=1&list-type=2")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}
	if contents, _ := body["contents"].([]any); len(contents) != 1 {
		t.Errorf("contents = %v, want the seeded object", body["contents"])
	}
}

// ---------------------------------------------------------------------------
// Object GET — the other half of an index run. The listing enumerates the
// bucket; this reads each listed key's bytes (elitea-sdk
// runtime/tools/artifact.py: _base_loader lists, _extend_data downloads).
// Without this route a run lists files correctly and indexes every one of
// them with empty content.
// ---------------------------------------------------------------------------

// getS3Object performs the request and returns the recorder, so tests can
// assert on the RAW body and headers. Deliberately not decoded: the success
// contract here is bytes, not JSON.
func getS3Object(t *testing.T, h *artifacts.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rr := httptest.NewRecorder()
	newS3TestRouter(h).ServeHTTP(rr, req)
	return rr
}

// TestDownloadObjectS3_ServesRawBytes pins the success contract the SDK
// actually consumes: download_artifact_s3 returns response.content verbatim
// (client.py:1184) and hands it to a binary parser. A JSON envelope — even a
// correct-looking one — would be indexed as its own serialisation, so this
// asserts the body is the exact stored bytes AND that it is not JSON-shaped.
func TestDownloadObjectS3_ServesRawBytes(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	content := []byte("the quick brown fox\n")
	store.seedContent("1", "reports", "notes.txt", content, "text/plain; charset=utf-8")

	rr := getS3Object(t, h, "/artifacts/s3/reports/notes.txt?project_id=1&format=json")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Body.Bytes(); !bytes.Equal(got, content) {
		t.Errorf("body = %q, want the stored bytes %q", got, content)
	}
	// format=json is sent on every S3 call (_s3_params, client.py:1074) and
	// must NOT switch the representation — the SDK never decodes a success.
	var envelope map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err == nil {
		t.Errorf("body decoded as a JSON object (%v); the SDK expects raw bytes", envelope)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the stored type", got)
	}
	if got := rr.Header().Get("Content-Length"); got != strconv.Itoa(len(content)) {
		t.Errorf("Content-Length = %q, want %d", got, len(content))
	}
}

// TestDownloadObjectS3_FallsBackToExtensionContentType covers a stored object
// with no recorded type — the local backend's common case. requests never
// needs it, but a wrong type here would reach the SDK's parse_file_content
// fallback path, so it is pinned rather than left incidental.
func TestDownloadObjectS3_FallsBackToExtensionContentType(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seedContent("1", "reports", "diagram.png", []byte("\x89PNG"), "")

	rr := getS3Object(t, h, "/artifacts/s3/reports/diagram.png?project_id=1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "image/png") {
		t.Errorf("Content-Type = %q, want image/png derived from the extension", got)
	}
}

// TestDownloadObjectS3_KeyWithSlashes is the route-shape guard. The SDK quotes
// the key with safe='/' (client.py:1176), so a nested key arrives as literal
// path segments; a route capturing a single {key} segment would 404 every
// file in a folder — and the indexer's own comment (artifact.py:_extend_data)
// says it downloads by full key precisely because folders are the normal
// case.
func TestDownloadObjectS3_KeyWithSlashes(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	content := []byte("nested content")
	store.seedContent("1", "reports", "folder/sub/file.txt", content, "text/plain")

	rr := getS3Object(t, h, "/artifacts/s3/reports/folder/sub/file.txt?project_id=1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a key containing slashes; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Body.Bytes(); !bytes.Equal(got, content) {
		t.Errorf("body = %q, want %q — the whole remaining path must be the key", got, content)
	}
}

// TestDownloadObjectS3_AcceptsLowercasedBucketName mirrors the SDK, which
// lower-cases the bucket before building the URL (client.py:1176).
func TestDownloadObjectS3_AcceptsLowercasedBucketName(t *testing.T) {
	h, _, store := newObjectTestHandler(t)
	store.seedContent("1", "reports", "a.txt", []byte("x"), "text/plain")

	rr := getS3Object(t, h, "/artifacts/s3/REPORTS/a.txt?project_id=1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// TestDownloadObjectS3_ScopesObjectLookupToQueryProject proves the handler
// reads within the project named by project_id and nowhere else. The object
// exists only in project 1; asking for it as project 2 must miss. This is the
// handler-level half of the cross-tenant guarantee — the RBAC half (a caller
// who cannot even name project 2) is proven at the router level in
// internal/api.
func TestDownloadObjectS3_ScopesObjectLookupToQueryProject(t *testing.T) {
	h, repo, store := newObjectTestHandler(t)
	store.seedContent("1", "reports", "secret.txt", []byte("classified"), "text/plain")
	// Give project 2 a same-named bucket, so the request gets PAST the bucket
	// check and the 404 can only come from the object lookup being scoped to
	// the project — otherwise this would pass for the weaker reason that
	// project 2 has no bucket at all.
	if _, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: 2, Name: "reports", DisplayName: "reports", BucketType: "local",
	}); err != nil {
		t.Fatalf("seed CreateBucket: %v", err)
	}

	rr := getS3Object(t, h, "/artifacts/s3/reports/secret.txt?project_id=2")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for another project's object; body=%s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); strings.Contains(body, "classified") {
		t.Fatalf("response leaked the other project's bytes: %s", body)
	}
}

// TestDownloadObjectS3_ErrorEnvelopeMatchesWhatTheSDKReads pins the error
// shape _handle_s3_error parses (client.py:1078-1089): a JSON body with
// error.code, in the vocabulary S3_ERROR_MESSAGES can phrase. Note the
// missing-key case in particular: a download has already confirmed the
// bucket, so a miss is NoSuchKey ("File 'x' not found"), never the
// NoSuchBucket the listing reports for the same underlying storage error.
func TestDownloadObjectS3_ErrorEnvelopeMatchesWhatTheSDKReads(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing key in an existing bucket",
			target:     "/artifacts/s3/reports/absent.txt?project_id=1",
			wantStatus: http.StatusNotFound,
			wantCode:   "NoSuchKey",
		},
		{
			name:       "missing bucket",
			target:     "/artifacts/s3/nope/a.txt?project_id=1",
			wantStatus: http.StatusNotFound,
			wantCode:   "NoSuchBucket",
		},
		{
			// A traversal key is rejected by storage.NewObjectRef and must
			// reach the SDK as the S3 InvalidArgument it knows how to phrase,
			// not as an unmapped code that degrades to "S3 error: ...".
			// Written unescaped on purpose: nothing in the chain cleans the
			// path (Go's http server only cleans through ServeMux, which this
			// router does not use), so this is the form that actually
			// reaches the handler.
			name:       "traversal key",
			target:     "/artifacts/s3/reports/../../etc/passwd?project_id=1",
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidArgument",
		},
		{
			name:       "missing project_id",
			target:     "/artifacts/s3/reports/a.txt",
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidArgument",
		},
		{
			name:       "non-numeric project_id",
			target:     "/artifacts/s3/reports/a.txt?project_id=notanumber",
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidArgument",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newObjectTestHandler(t)
			rr := getS3Object(t, h, tc.target)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body is not JSON: %v (body=%s)", err, rr.Body.String())
			}
			if got := errorCodeOf(t, body); got != tc.wantCode {
				t.Errorf("error.code = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

// errorCodeOf pulls error.code out of the typed envelope, failing the test
// if the body is not shaped the way _handle_s3_error expects.
func errorCodeOf(t *testing.T, body map[string]any) string {
	t.Helper()
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf(`body has no "error" object; keys=%v`, keysOf(body))
	}
	code, ok := errObj["code"].(string)
	if !ok {
		t.Fatalf(`error has no string "code"; keys=%v`, keysOf(errObj))
	}
	return code
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
