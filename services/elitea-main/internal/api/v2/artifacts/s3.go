package artifacts

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// This file serves the S3-shaped representation of the object surface — the
// one the Python SDK's artifact toolkit speaks. The listing and the object
// read are what an index run performs; the write verbs at the bottom (PUT,
// DELETE, HEAD) are what an agent's toolkit calls to produce, check and
// remove files.
//
// The routes are deliberately NOT under /api/v2. The worker builds the URL as
// `{platform_origin}/artifacts/s3/{bucket}` (elitea-sdk
// runtime/clients/client.py:115, :1105) where platform_origin is validated
// to be a bare origin carrying no path at all (elitea-worker-python
// config.py:187-199), and the platform edge forwards the path verbatim with
// no stripPrefix/addPrefix (deploy/runtime/platform-edge-dynamic.yml). So
// the path that actually arrives at elitea-main is /artifacts/s3/{bucket},
// and a route mounted under /api/v2 would 404 exactly as before.
//
// Despite the name, the response is JSON, not S3's XML: the SDK calls
// response.json() unconditionally (client.py:1118). The name reflects the
// legacy pylon route this replaces, not the wire format.
//
// Only the representation is new. The bucket resolution, prefix validation,
// delimiter handling and paging all come from the same helpers the native
// route uses (parseListObjectsQuery / listBucketObjects in objects.go), so
// the two listings cannot silently diverge.

// s3ObjectSummary is one entry of the S3 listing's "contents" array.
//
// The field names are load-bearing and camelCase, unlike the native route's
// snake_case objectSummary: the SDK reads item['key'], item['size'] and
// item['lastModified'] by exact name (elitea-sdk
// runtime/clients/artifact.py:123-127, :203-217). A casing regression here
// would not fail loudly — it would make every listing look empty and send
// an index run green having indexed nothing, which is the exact defect this
// route exists to fix.
type s3ObjectSummary struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
	// ETag is not read by the SDK today. It is included because it is free
	// (storage.ObjectInfo already carries it) and canonical for an S3
	// listing, and omitted when the backend supplies none.
	ETag string `json:"etag,omitempty"`
}

// s3ListResponse is the S3 listing envelope.
//
// commonPrefixes is a flat []string. The SDK accepts either a plain string
// or a {"prefix": "..."} object (artifact.py:203-217, the isinstance(dict)
// branch), so both would work; the plain string is chosen because it is
// what storage.ListPage.CommonPrefixes already is and what the native route
// already returns, making the two representations differ in key casing
// only. Both slices are always non-nil so the JSON carries [] rather than
// null — the SDK iterates them with .get(key, []) and a null would be
// indistinguishable from a missing key.
type s3ListResponse struct {
	Contents       []s3ObjectSummary `json:"contents"`
	CommonPrefixes []string          `json:"commonPrefixes"`
	// IsTruncated/NextContinuationToken are reported so a caller can page.
	// The SDK does not read them today, which means a bucket larger than
	// the backend's page cap lists only its first page — a real gap, but a
	// client-side one that cannot be closed from here.
	IsTruncated           bool   `json:"isTruncated"`
	NextContinuationToken string `json:"nextContinuationToken,omitempty"`
}

// s3ErrorCodes translates this package's artifact error vocabulary into the
// S3 codes the SDK's _handle_s3_error knows how to phrase
// (client.py:1060-1067, S3_ERROR_MESSAGES). It reads error.code out of the
// JSON body — the same {"error":{"code","message"}} envelope writeError
// already produces — and falls back to "S3 error: <code>" for anything
// unrecognised, so an unmapped code degrades to a vague message rather than
// a wrong one.
// "NotFound" is deliberately absent: S3 has two distinct not-found codes and
// which one applies depends on what the request addressed. A listing can only
// ever miss the bucket (NoSuchBucket); an object GET has already confirmed
// the bucket before it reads, so a miss there can only be the key
// (NoSuchKey). Each caller passes the one that is true for it — collapsing
// them into a single mapping here would tell the SDK the bucket is gone every
// time a file is merely absent.
//
// "TooLarge" maps to S3's own EntityTooLarge even though the SDK's table
// cannot phrase it (it degrades to "S3 error: EntityTooLarge"). That is
// deliberate: the alternative is the InternalError fallback, which would tell
// the caller the server broke when in fact its object exceeded a limit it can
// do something about. A vague-but-true code beats a precise lie, and both the
// per-object cap and the project quota (storeObject, objects.go) surface
// through it.
// "RangeNotSatisfiable" maps to S3's own InvalidRange. The alternative is
// the InternalError fallback. That fallback tells the caller the server
// broke. In fact its Range header addressed bytes the object does not
// have.
var s3ErrorCodes = map[string]string{
	"AccessDenied":        "AccessDenied",
	"InvalidKey":          "InvalidArgument",
	"InvalidArgument":     "InvalidArgument",
	"TooLarge":            "EntityTooLarge",
	"RangeNotSatisfiable": "InvalidRange",
}

func s3ErrorCode(code, notFoundAs string) string {
	if code == "NotFound" {
		return notFoundAs
	}
	if mapped, ok := s3ErrorCodes[code]; ok {
		return mapped
	}
	return "InternalError"
}

// s3ProjectID reads the `project_id` QUERY parameter both S3 routes carry.
//
// The project is named in the query string, not a path segment — that is the
// SDK's wire format (_s3_params, client.py:1072) and cannot be changed from
// this side. A query parameter is caller-controlled input and is NOT trusted
// here as an authorization claim: both routes are mounted behind
// RequireResolvedPermissionsForProject with ProjectIDFromQuery("project_id")
// (see mountArtifactRoutes in internal/api/router.go), which resolves the
// caller's permissions IN that named project before the handler runs. A
// caller who names a project they hold no role in resolves to zero
// permissions and is refused with 403, so changing the query string cannot
// reach another tenant's artifacts. The handlers then scope every subsequent
// lookup to that same id.
//
// Returns the id in both forms the handlers need — int64 for the repository,
// the original string for storage refs — and writes the error itself when it
// is unusable.
func s3ProjectID(w http.ResponseWriter, r *http.Request) (int64, string, bool) {
	projectID, projectIDStr, ok := parseS3ProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "project_id is required")
		return 0, "", false
	}
	return projectID, projectIDStr, true
}

// parseS3ProjectID is s3ProjectID without the response write, for HEAD, whose
// response must not carry a body at all — the SDK's head_artifact_s3 reads
// only the status and the entity headers (client.py:1206-1233), and a JSON
// error envelope on a HEAD is a body no client can read.
func parseS3ProjectID(r *http.Request) (int64, string, bool) {
	projectIDStr := r.URL.Query().Get("project_id")
	projectID, err := strconv.ParseInt(projectIDStr, 10, 64)
	if err != nil || projectID <= 0 {
		return 0, "", false
	}
	return projectID, projectIDStr, true
}

// s3Bucket reads the bucket path parameter. The SDK lower-cases the bucket
// before building the URL (client.py:1105, :1176), so normalise here too
// rather than letting a differently-cased stored name miss.
func s3Bucket(r *http.Request) string {
	return strings.ToLower(chi.URLParam(r, "bucket"))
}

// requireS3Bucket is requireBucket rendered in the S3 error vocabulary: the
// SDK phrases NoSuchBucket specifically (S3_ERROR_MESSAGES, client.py:1061).
// Callers return immediately when ok is false. It returns the fetched row for
// the same reason requireBucket does: the write verbs below need the bucket's
// database ID (object metadata upsert/delete) and its expires_at, and looking
// it up a second time could observe a different row.
func (h *Handler) requireS3Bucket(w http.ResponseWriter, r *http.Request, projectID int64, bucket, need string) (repos.BucketRow, bool) {
	// The per-bucket access list, checked first and in the S3 vocabulary. The
	// SDK reaches the same objects through these routes as the native ones, so
	// an exception that refuses a verb there must refuse it here; an S3
	// representation that is a softer way in is not a representation, it is a
	// second policy. AccessDenied is the code legacy's own S3 routes answer
	// with (routes/s3.py:88-90).
	allowed, err := h.authorizeBucket(r.Context(), projectID, bucket, need)
	if err != nil {
		h.writeInternalS3(w, r, "authorize bucket", err)
		return repos.BucketRow{}, false
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "AccessDenied", denyMessage(need))
		return repos.BucketRow{}, false
	}
	row, err := h.repo.GetBucket(r.Context(), projectID, bucket)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return repos.BucketRow{}, false
		}
		h.writeInternalS3(w, r, "get bucket", err)
		return repos.BucketRow{}, false
	}
	return row, true
}

// ListObjectsS3 answers the SDK's bucket listing.
func (h *Handler) ListObjectsS3(w http.ResponseWriter, r *http.Request) {
	projectID, projectIDStr, ok := s3ProjectID(w, r)
	if !ok {
		return
	}
	bucket := s3Bucket(r)

	// Same bucket resolution as the native route. A listing can only ever
	// fail on the bucket, never on a key.
	if _, ok := h.requireS3Bucket(w, r, projectID, bucket, accessRead); !ok {
		return
	}

	// list-type and format are accepted and ignored on purpose. The SDK
	// pins them to "2" and "json" on every call and this route serves
	// exactly one representation — a JSON, V2-style listing — so there is
	// nothing to select. Rejecting other values would add a failure mode
	// that only ever fires if the SDK changes its constants, and honouring
	// them would mean building an XML representation with no consumer.
	q, code, message := parseListObjectsQuery(r.URL.Query())
	if code != "" {
		writeError(w, statusForCode(code), s3ErrorCode(code, "NoSuchBucket"), message)
		return
	}

	// prefix and delimiter pass through unchanged. The SDK appends a
	// trailing "/" to a folder prefix itself, and omits delimiter entirely
	// for a recursive listing — an absent delimiter is the empty string,
	// which storage.List already treats as "do not group", so recursion
	// needs no special case here.
	page, err := h.listBucketObjects(r.Context(), projectIDStr, bucket, q)
	if err != nil {
		if errors.Is(err, errInvalidBucketRef) {
			h.writeInternalS3(w, r, "list objects", err)
			return
		}
		code, message := h.classifyStorageError(r.Context(), "list objects", err)
		writeError(w, statusForCode(code), s3ErrorCode(code, "NoSuchBucket"), message)
		return
	}

	contents := make([]s3ObjectSummary, 0, len(page.Objects))
	for _, o := range page.Objects {
		contents = append(contents, s3ObjectSummary{
			Key:          o.Key,
			Size:         o.Size,
			LastModified: o.LastModified,
			ETag:         o.ETag,
		})
	}
	commonPrefixes := page.CommonPrefixes
	if commonPrefixes == nil {
		commonPrefixes = []string{}
	}

	writeJSON(w, http.StatusOK, s3ListResponse{
		Contents:              contents,
		CommonPrefixes:        commonPrefixes,
		IsTruncated:           page.IsTruncated,
		NextContinuationToken: page.NextContinuationToken,
	})
}

// DownloadObjectS3 answers the SDK's object GET — the other half of an index
// run. The listing enumerates the bucket; this reads each file's bytes
// (elitea-sdk runtime/tools/artifact.py:_base_loader lists, then _extend_data
// downloads every listed key). Without it a run lists files correctly and
// then indexes them all with empty content, which is the same vacuous green
// as listing nothing.
//
// On success the body is the raw object bytes, NOT JSON: the SDK returns
// response.content unconditionally (client.py:1184) and hands it to a binary
// content parser. The `format=json` parameter it sends on every S3 call
// (_s3_params, client.py:1074) is therefore accepted and ignored here — this
// route has exactly one representation, and honouring `format` would mean
// base64-wrapping bytes no caller unwraps. JSON appears only in the error
// case, which is exactly what _handle_s3_error parses.
//
// Authorization is identical to the listing's — see s3ProjectID.
func (h *Handler) DownloadObjectS3(w http.ResponseWriter, r *http.Request) {
	projectID, projectIDStr, ok := s3ProjectID(w, r)
	if !ok {
		return
	}
	bucket := s3Bucket(r)

	// The key is the whole remaining path, captured by a trailing chi
	// wildcard rather than a single {key} segment: the SDK quotes the key
	// with safe='/' (client.py:1176), so a nested key like
	// "folder/sub/file.txt" arrives as literal path segments. This is the
	// same capture the native download route uses.
	key := objectKeyFromRequest(r)

	// Before the bucket lookup, matching DownloadObject: a malformed key is
	// caller error whether or not the bucket exists. storage.ErrInvalidKey
	// maps to the S3 InvalidArgument the SDK can phrase.
	ref, err := storage.NewObjectRef(projectIDStr, bucket, key)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", err.Error())
		return
	}

	if _, ok := h.requireS3Bucket(w, r, projectID, bucket, accessRead); !ok {
		return
	}

	// Past the bucket check, a not-found can only be the key — so this call
	// site, unlike the listing's, renders NotFound as NoSuchKey. The SDK
	// phrases that as "File '<key>' not found" (client.py:1062).
	if code, message := h.streamObject(w, r, ref, key); code != "" {
		writeError(w, statusForCode(code), s3ErrorCode(code, "NoSuchKey"), message)
	}
}

// ---------------------------------------------------------------------------
// Object write verbs — PUT, DELETE, HEAD.
//
// These complete the S3-shaped surface the SDK speaks. Unlike the two read
// routes above, which an index run drives, these are what an agent's artifact
// toolkit calls when it produces a file, replaces it, checks for it, or
// removes it (elitea-sdk runtime/clients/client.py: upload_artifact_s3,
// delete_artifact_s3, head_artifact_s3).
//
// Key sanitization is NOT performed here, on purpose. upload_artifact_s3 runs
// the key through _sanitize_artifact_name BEFORE building the URL
// (client.py:1140) and reports filepath/filename/sanitized_name/was_sanitized
// computed entirely from its own inputs — the server never sees the
// pre-sanitized key and could not report was_sanitized even if it wanted to.
// That split is right: sanitization is a lossy rewrite chosen for the
// indexer's benefit, while the server's job is to REJECT what it cannot store
// safely (storage.NewObjectRef's validateKey: traversal segments, leading and
// trailing slashes, empty segments). Rewriting a key server-side would also
// mean writing to a key the caller did not ask for and cannot predict, which
// a later download by the original key would then miss.
// ---------------------------------------------------------------------------

// s3PutResponse is the PUT success envelope.
//
// The SDK reads none of it — upload_artifact_s3 checks only status < 400 and
// then synthesises its own dict from the arguments it already has
// (client.py:1154-1163). The body is emitted anyway because the route is
// otherwise indistinguishable from a silent no-op to any other client, and
// the camelCase names keep it in the same vocabulary as the listing rather
// than inventing a third.
//
// LastModified is a pointer so it can be OMITTED rather than emitted as the
// zero instant. Not hypothetical: the S3 backend's Put reports no
// modification time at all (only List and Stat do), so a plain time.Time
// renders "0001-01-01T00:00:00Z" — a value a client would read as the object's
// real mtime. Measured against the running standalone stack, which is the only
// place the difference shows.
type s3PutResponse struct {
	Key          string     `json:"key"`
	Bucket       string     `json:"bucket"`
	Size         int64      `json:"size"`
	LastModified *time.Time `json:"lastModified,omitempty"`
	ETag         string     `json:"etag,omitempty"`
}

// UploadObjectS3 writes an object from a RAW request body.
//
// The wire shape differs from the native route's and only there: the SDK
// sends the bytes as the body itself with the Content-Type header carrying
// the media type (requests.put(url, data=data), client.py:1150-1151), where
// the native route takes a multipart "file" part. Everything downstream of
// that decode — the per-object cap, the read deadline, the streaming Put, the
// metadata row, and the project quota with its rollback — is the SAME code
// (resolveObjectSizeLimit + storeObject, objects.go), so this verb cannot
// become a way to write past a limit the native route enforces. Nothing here
// buffers the body: r.Body goes straight to Put, so ADR-0016's "never spill an
// upload to memory or os.TempDir" holds unchanged.
//
// Two deliberate differences from the native POST:
//
//   - It overwrites unconditionally. S3 PUT is an upsert and the SDK sends no
//     overwrite flag, so the native route's pre-existing-key Stat check (409
//     AlreadyExists) is skipped rather than translated. Keeping it would make
//     every second upload of the same key fail with a code the SDK's table
//     cannot phrase ("S3 error: AlreadyExists") and no way to opt out. The
//     `create` permission already authorizes the same overwrite natively, via
//     ?overwrite=true.
//   - It answers 200, not 201, matching S3's own PUT. The SDK accepts any
//     status below 400.
func (h *Handler) UploadObjectS3(w http.ResponseWriter, r *http.Request) {
	projectID, projectIDStr, ok := s3ProjectID(w, r)
	if !ok {
		return
	}
	bucket := s3Bucket(r)
	key := objectKeyFromRequest(r)

	// Before the bucket lookup, matching the native route and the S3
	// download: a malformed key is caller error whether or not the bucket
	// exists.
	ref, err := storage.NewObjectRef(projectIDStr, bucket, key)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", err.Error())
		return
	}

	bucketRow, ok := h.requireS3Bucket(w, r, projectID, bucket, accessWrite)
	if !ok {
		return
	}

	policy, maxObjectBytes, err := h.resolveObjectSizeLimit(r.Context(), projectID)
	if err != nil {
		h.writeInternalS3(w, r, "get project storage policy", err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxObjectBytes)

	// Best-effort, exactly as in UploadObject: httptest.ResponseRecorder and
	// similar test writers don't support deadlines, which is harmless — this
	// is a defense-in-depth ceiling, not a correctness requirement.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(artifactStreamDeadline))

	// The SDK always sends a Content-Type it detected itself
	// (detect_mime_type, client.py:1147); the extension fallback covers any
	// other client that does not.
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = mimeFromExtension(key)
	}

	info, code, message := h.storeObject(r.Context(), storeObjectInput{
		projectID:   projectID,
		bucketRow:   bucketRow,
		ref:         ref,
		body:        r.Body,
		contentType: contentType,
		policy:      policy,
	})
	if code != "" {
		// Past the bucket check, a NotFound could only be the key.
		writeError(w, statusForCode(code), s3ErrorCode(code, "NoSuchKey"), message)
		return
	}

	if info.ETag != "" {
		w.Header().Set("ETag", info.ETag)
	}
	resp := s3PutResponse{
		Key:    info.Key,
		Bucket: bucket,
		Size:   info.Size,
		ETag:   info.ETag,
	}
	if !info.LastModified.IsZero() {
		resp.LastModified = &info.LastModified
	}
	writeJSON(w, http.StatusOK, resp)
}

// DeleteObjectS3 removes one object, idempotently.
//
// An absent key is 204, not 404 — this follows real S3 rather than the native
// DeleteObject. The contract matters more than the diagnostic: every S3 client
// is written against an idempotent DELETE, and a redelivered request (a retry
// after a timeout, a replayed queue message) must not turn into an error
// because the first attempt already succeeded.
//
// A missing BUCKET is still an error. The bucket comes from the caller's own
// configuration rather than from a key it is iterating, so silence there would
// hide a misconfigured toolkit instead of absorbing a repeat.
//
// 204 with no body, matching both S3 and the native route. delete_artifact_s3
// never reads the body.
func (h *Handler) DeleteObjectS3(w http.ResponseWriter, r *http.Request) {
	projectID, projectIDStr, ok := s3ProjectID(w, r)
	if !ok {
		return
	}
	bucket := s3Bucket(r)
	key := objectKeyFromRequest(r)

	ref, err := storage.NewObjectRef(projectIDStr, bucket, key)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", err.Error())
		return
	}

	bucketRow, ok := h.requireS3Bucket(w, r, projectID, bucket, accessWrite)
	if !ok {
		return
	}

	// S3 DELETE is idempotent: an absent key is success, not 404. The native
	// route answers 404 because a browser deleting one selected row wants to
	// know the row was stale, but the S3 caller is a toolkit replaying a key it
	// already believes is gone, and a 404 there means only "already deleted".
	//
	// The metadata delete below still runs when the object is absent, so a row
	// orphaned by a partial earlier delete is healed rather than left to count
	// against the project's quota forever.
	if _, err := h.store.Stat(r.Context(), ref); err != nil {
		if code, message := h.classifyStorageError(r.Context(), "stat object", err); code != "NotFound" {
			writeError(w, statusForCode(code), s3ErrorCode(code, "NoSuchKey"), message)
			return
		}
	} else if err := h.store.Delete(r.Context(), ref); err != nil {
		// A concurrent delete between Stat and Delete lands here; it reached
		// the caller's intended state, so it is success too.
		if code, message := h.classifyStorageError(r.Context(), "delete object", err); code != "NotFound" {
			writeError(w, statusForCode(code), s3ErrorCode(code, "NoSuchKey"), message)
			return
		}
	}

	// The metadata row must go too, or the project's quota accounting keeps
	// counting bytes that no longer exist and a project that deleted
	// everything it uploaded stays permanently near its limit (S12).
	if err := h.repo.DeleteObjects(r.Context(), bucketRow.ID, []string{key}); err != nil {
		h.writeInternalS3(w, r, "delete object metadata", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// StatObjectS3 answers the existence check.
//
// head_artifact_s3 reads the status and four entity headers and nothing else:
// 404 means {"exists": False} — its most common answer, and not an error to
// the caller — while any other 4xx/5xx goes through _handle_s3_error, which
// on a bodyless response falls back to the code "HTTP_<status>"
// (client.py:1084-1085, the except branch). So this handler writes NO body at
// all, in every branch: a HEAD response's body is unreadable by definition,
// and a JSON envelope here would only inflate Content-Length and misreport
// the object's size to a client that trusts it.
//
// Content-Length, Last-Modified, Content-Type and ETag are all set because
// the SDK reads all four by name (client.py:1227-1232).
func (h *Handler) StatObjectS3(w http.ResponseWriter, r *http.Request) {
	projectID, projectIDStr, ok := parseS3ProjectID(r)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	bucket := s3Bucket(r)
	key := objectKeyFromRequest(r)

	ref, err := storage.NewObjectRef(projectIDStr, bucket, key)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// requireBucketNoBody, not requireS3Bucket: the S3 variant renders a JSON
	// envelope, which a HEAD must not carry. A missing bucket therefore
	// reaches the SDK as a bare 404, which it reports as {"exists": False} —
	// true, if less specific than the listing's NoSuchBucket.
	if _, ok := h.requireBucketNoBody(w, r, projectID, bucket, accessRead); !ok {
		return
	}

	info, err := h.store.Stat(r.Context(), ref)
	if err != nil {
		w.WriteHeader(statusForCode(storageErrorCode(err)))
		return
	}

	contentType := info.ContentType
	if contentType == "" {
		contentType = mimeFromExtension(key)
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	if info.ETag != "" {
		w.Header().Set("ETag", info.ETag)
	}
	if !info.LastModified.IsZero() {
		w.Header().Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	}
	w.WriteHeader(http.StatusOK)
}
