package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

const (
	// defaultMaxObjectBytes matches the legacy vault-sourced
	// chat_max_file_upload_size_mb default (150 MiB), used when the
	// project's storage policy has no max_object_bytes and
	// ARTIFACT_MAX_OBJECT_BYTES is unset (S12).
	defaultMaxObjectBytes int64 = 150 * 1024 * 1024

	// artifactStreamDeadline bounds a single upload/download body once past
	// the header phase, replacing the global http.Server{ReadTimeout,
	// WriteTimeout} S12 removes (see cmd/elitea-main/http_server.go).
	// Generous enough to comfortably cover both stated targets (a 100 MiB
	// upload over 30s, a 300s download) without acting as a de facto
	// unlimited timeout.
	artifactStreamDeadline = 10 * time.Minute
)

// objectSummary is the JSON-facing shape of one listed object
// (components/schemas/ObjectSummary in api/openapi/v2.yaml).
type objectSummary struct {
	Key        string    `json:"key"`
	SizeBytes  int64     `json:"size_bytes"`
	MediaType  string    `json:"media_type"`
	ETag       string    `json:"etag"`
	ModifiedAt time.Time `json:"modified_at"`
}

// batchDeleteFailure is one entry of BatchDeleteObjectsResponse.failed.
type batchDeleteFailure struct {
	Key     string `json:"key"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// storageErrorCode maps an ObjectStore sentinel error (internal/infra/storage
// errors.go) onto the artifact API's typed error code enum
// (components/schemas/Error in api/openapi/v2.yaml).
func storageErrorCode(err error) string {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return "NotFound"
	case errors.Is(err, storage.ErrAlreadyExists):
		return "AlreadyExists"
	case errors.Is(err, storage.ErrAccessDenied):
		return "AccessDenied"
	case errors.Is(err, storage.ErrInvalidKey):
		return "InvalidKey"
	case errors.Is(err, storage.ErrTooLarge):
		return "TooLarge"
	case errors.Is(err, storage.ErrPreconditionFailed):
		return "PreconditionFailed"
	case errors.Is(err, storage.ErrNotSupported):
		return "NotImplemented"
	default:
		return "Internal"
	}
}

func statusForCode(code string) int {
	switch code {
	case "NotFound":
		return http.StatusNotFound
	case "AlreadyExists":
		return http.StatusConflict
	case "AccessDenied":
		return http.StatusForbidden
	case "InvalidKey", "InvalidArgument":
		return http.StatusBadRequest
	case "TooLarge":
		return http.StatusRequestEntityTooLarge
	case "PreconditionFailed":
		return http.StatusPreconditionFailed
	case "QuotaExceeded":
		return http.StatusForbidden
	case "NotImplemented":
		return http.StatusNotImplemented
	case "RangeNotSatisfiable":
		return http.StatusRequestedRangeNotSatisfiable
	case "DigestMismatch", "MediaTypeMismatch":
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// writeStorageError maps a storage.ObjectStore error onto the typed error
// envelope, choosing both the HTTP status and the error code from the same
// sentinel classification. An unclassified cause never reaches the caller —
// see classifyStorageError.
func (h *Handler) writeStorageError(ctx context.Context, w http.ResponseWriter, op string, err error) {
	code, message := h.classifyStorageError(ctx, op, err)
	writeError(w, statusForCode(code), code, message)
}

// isMaxBytesError reports whether err (or something it wraps) is
// http.MaxBytesReader's limit-exceeded error. It can surface either from
// multipart.Reader.NextPart (scanning boundary/headers) or later from a
// part's own Read calls (inside ObjectStore.Put) — both mean the same
// thing, S12's per-object size limit, and both must map to 413/TooLarge
// rather than whatever generic error the caller would otherwise report.
func isMaxBytesError(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}

func mimeFromExtension(key string) string {
	if ext := path.Ext(key); ext != "" {
		if mt := mime.TypeByExtension(ext); mt != "" {
			return mt
		}
	}
	return "application/octet-stream"
}

// requireBucket 404s (typed envelope) when the bucket doesn't exist and
// 500s on any other repository error, writing the response itself. Callers
// return immediately when ok is false. Returns the fetched row so callers
// that need the bucket's database ID (object metadata read/write, S12)
// don't have to look it up a second time.
//
// `need` is the per-bucket access list check (bucket_permissions.go): pass
// accessRead for a verb that only observes the bucket and accessWrite for one
// that changes it. THE ACCESS LIST IS CHECKED BEFORE THE BUCKET IS FETCHED,
// which is legacy's order — its decorator runs ahead of the handler
// (utils/utils.py:177-238) — and it keeps a refused caller from learning
// whether a bucket they may not touch exists.
func (h *Handler) requireBucket(w http.ResponseWriter, r *http.Request, projectID int64, bucket, need string) (repos.BucketRow, bool) {
	allowed, err := h.authorizeBucket(r.Context(), projectID, bucket, need)
	if err != nil {
		h.writeInternal(w, r, "authorize bucket", err)
		return repos.BucketRow{}, false
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "Forbidden", denyMessage(need))
		return repos.BucketRow{}, false
	}
	row, err := h.repo.GetBucket(r.Context(), projectID, bucket)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NotFound", "bucket not found")
		} else {
			h.writeInternal(w, r, "get bucket", err)
		}
		return repos.BucketRow{}, false
	}
	return row, true
}

// requireBucketNoBody is requireBucket for HEAD responses, which must not
// carry a body.
func (h *Handler) requireBucketNoBody(w http.ResponseWriter, r *http.Request, projectID int64, bucket, need string) (repos.BucketRow, bool) {
	allowed, err := h.authorizeBucket(r.Context(), projectID, bucket, need)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return repos.BucketRow{}, false
	}
	if !allowed {
		w.WriteHeader(http.StatusForbidden)
		return repos.BucketRow{}, false
	}
	row, err := h.repo.GetBucket(r.Context(), projectID, bucket)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			w.WriteHeader(http.StatusNotFound)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
		return repos.BucketRow{}, false
	}
	return row, true
}

func objectKeyFromRequest(r *http.Request) string {
	return strings.TrimPrefix(chi.URLParam(r, "*"), "/")
}

// artifactMaxObjectBytesFromEnv reads ARTIFACT_MAX_OBJECT_BYTES — the
// fallback used when a project has no explicit max_object_bytes policy
// (S12). A missing, non-numeric, or non-positive value means "no override."
func artifactMaxObjectBytesFromEnv() (int64, bool) {
	raw := os.Getenv("ARTIFACT_MAX_OBJECT_BYTES")
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// listObjectsQuery is the parsed, validated form of a bucket listing
// request. It exists because elitea-main serves the same underlying listing
// through two representations — the native route (ListObjects) and the
// S3-shaped route the SDK's artifact toolkit speaks (ListObjectsS3, s3.go).
// Only the wire rendering differs between them, so everything up to and
// including the storage query is shared here rather than written twice:
// a divergence between the two would be invisible until an index run
// silently listed nothing.
type listObjectsQuery struct {
	prefix    string
	delimiter string
	maxKeys   int32
	cursor    string
}

// parseListObjectsQuery validates the listing parameters both
// representations accept. A non-empty code means the request is bad and the
// caller must render (code, message) in its own error vocabulary — the two
// representations use different code sets (artifact codes vs S3 codes), so
// classification is shared here while rendering stays with the caller.
func parseListObjectsQuery(q url.Values) (listObjectsQuery, string, string) {
	prefix := q.Get("prefix")
	if err := storage.ValidateKeyPrefix(prefix); err != nil {
		return listObjectsQuery{}, "InvalidKey", err.Error()
	}

	var maxKeys int32
	if limitStr := q.Get("limit"); limitStr != "" {
		limit, err := strconv.ParseInt(limitStr, 10, 32)
		if err != nil || limit < 0 {
			return listObjectsQuery{}, "InvalidArgument", "limit must be a non-negative integer"
		}
		maxKeys = int32(limit)
	}

	return listObjectsQuery{
		prefix:    prefix,
		delimiter: q.Get("delimiter"),
		maxKeys:   maxKeys,
		cursor:    q.Get("cursor"),
	}, "", ""
}

// errInvalidBucketRef marks a NewBucketRef failure inside listBucketObjects.
// Callers reach that point only after requireBucket has confirmed the bucket
// row exists, so a ref that will not build means the stored bucket name
// violates the storage ref pattern — an internal inconsistency, not caller
// error, and both representations must map it to "internal" rather than to
// the InvalidKey they'd infer from the wrapped storage.ErrInvalidKey.
var errInvalidBucketRef = errors.New("build bucket ref")

// listBucketObjects runs the storage query shared by both listing
// representations. The caller has already resolved and authorized the
// project and confirmed the bucket exists.
func (h *Handler) listBucketObjects(ctx context.Context, projectIDStr, bucket string, q listObjectsQuery) (storage.ListPage, error) {
	bucketRef, err := storage.NewBucketRef(projectIDStr, bucket)
	if err != nil {
		return storage.ListPage{}, fmt.Errorf("%w: %w", errInvalidBucketRef, err)
	}
	return h.store.List(ctx, storage.ListQuery{
		Bucket:            bucketRef,
		KeyPrefix:         q.prefix,
		Delimiter:         q.delimiter,
		MaxKeys:           q.maxKeys,
		ContinuationToken: q.cursor,
	})
}

func (h *Handler) ListObjects(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	projectIDStr := chi.URLParam(r, "projectID")
	bucket := chi.URLParam(r, "bucket")
	if _, ok := h.requireBucket(w, r, projectID, bucket, accessRead); !ok {
		return
	}

	q, code, message := parseListObjectsQuery(r.URL.Query())
	if code != "" {
		writeError(w, statusForCode(code), code, message)
		return
	}

	page, err := h.listBucketObjects(r.Context(), projectIDStr, bucket, q)
	if err != nil {
		if errors.Is(err, errInvalidBucketRef) {
			h.writeInternal(w, r, "list objects", err)
			return
		}
		h.writeStorageError(r.Context(), w, "list objects", err)
		return
	}

	objects := make([]objectSummary, 0, len(page.Objects))
	for _, o := range page.Objects {
		mediaType := o.ContentType
		if mediaType == "" {
			mediaType = mimeFromExtension(o.Key)
		}
		objects = append(objects, objectSummary{
			Key:        o.Key,
			SizeBytes:  o.Size,
			MediaType:  mediaType,
			ETag:       o.ETag,
			ModifiedAt: o.LastModified,
		})
	}
	commonPrefixes := page.CommonPrefixes
	if commonPrefixes == nil {
		commonPrefixes = []string{}
	}

	resp := map[string]any{"objects": objects, "common_prefixes": commonPrefixes}
	if page.IsTruncated {
		resp["next_cursor"] = page.NextContinuationToken
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveObjectSizeLimit reads the project's storage policy and derives the
// per-object byte cap from it, in the precedence S12 fixed: an explicit
// project policy first, then ARTIFACT_MAX_OBJECT_BYTES, then the legacy
// 150 MiB default. The policy itself is returned alongside because the
// caller needs its MaxTotalBytes for the post-write quota check, and reading
// it twice would let the two halves of the same decision disagree.
//
// Shared by both upload representations — the native multipart route
// (UploadObject) and the S3-shaped raw-body one (UploadObjectS3, s3.go) — so
// the S3 verb cannot become a way to write past a limit the native route
// enforces.
func (h *Handler) resolveObjectSizeLimit(ctx context.Context, projectID int64) (repos.ProjectStoragePolicy, int64, error) {
	policy, err := h.repo.GetProjectStoragePolicy(ctx, projectID)
	if err != nil {
		return repos.ProjectStoragePolicy{}, 0, err
	}
	maxObjectBytes := defaultMaxObjectBytes
	if policy.MaxObjectBytes != nil {
		maxObjectBytes = *policy.MaxObjectBytes
	} else if envLimit, ok := artifactMaxObjectBytesFromEnv(); ok {
		maxObjectBytes = envLimit
	}
	return policy, maxObjectBytes, nil
}

// storeObjectInput is everything storeObject needs that the two upload
// representations resolve differently: only how the bytes and the key are
// carried on the wire differs between them (a multipart "file" part with a
// Content-Disposition filename versus a raw body with the key in the path).
type storeObjectInput struct {
	projectID   int64
	bucketRow   repos.BucketRow
	ref         storage.ObjectRef
	body        io.Reader
	contentType string
	policy      repos.ProjectStoragePolicy
}

// storeObject performs the write half shared by both upload representations:
// the streaming Put, the object's metadata row, and the project-wide quota
// check with its two-part rollback. Everything that decides whether bytes
// survive a request lives here, so the S3 verb cannot silently skip the
// quota accounting the native route takes seriously.
//
// Like streamObject, it writes nothing itself: on failure it returns a code
// in THIS package's vocabulary and the caller renders it in its own (the
// native artifact codes, or the S3 codes the SDK can phrase). A non-empty
// code means nothing has been written to the response yet.
//
// The caller has already resolved and authorized the project, confirmed the
// bucket exists, validated the key, and wrapped the body in
// http.MaxBytesReader — the cap can only be enforced where the raw
// http.Request is in scope, which is the caller.
//
// The quota check runs after the write, not before, because the object's
// exact byte length isn't known until the stream has been fully read.
func (h *Handler) storeObject(ctx context.Context, in storeObjectInput) (storage.ObjectInfo, string, string) {
	info, err := h.store.Put(ctx, in.ref, in.body, storage.PutOptions{
		ContentType:   in.contentType,
		ContentLength: -1,
	})
	if err != nil {
		if isMaxBytesError(err) {
			return storage.ObjectInfo{}, "TooLarge", "object exceeds the project's max_object_bytes limit"
		}
		code, message := h.classifyStorageError(ctx, "put object", err)
		return storage.ObjectInfo{}, code, message
	}

	// The object's own retention window starts now. It does not inherit the
	// bucket's frozen expires_at — see computeExpiresAt. An overwrite
	// restarts the window, because UpsertArtifactObject's ON CONFLICT branch
	// refreshes expires_at; that matches an S3 lifecycle rule, which also
	// measures age from the newest version.
	if _, err := h.repo.UpsertObject(ctx, repos.NewObjectInput{
		BucketID:   in.bucketRow.ID,
		Key:        info.Key,
		ByteLength: info.Size,
		MediaType:  in.contentType,
		ExpiresAt:  computeExpiresAt(in.bucketRow.RetentionDays),
	}); err != nil {
		h.logInternal(ctx, "record object metadata", err)
		return storage.ObjectInfo{}, "Internal", "record object metadata"
	}

	if in.policy.MaxTotalBytes != nil {
		total, err := h.repo.SumProjectBytes(ctx, in.projectID)
		if err != nil {
			h.logInternal(ctx, "sum project bytes", err)
			return storage.ObjectInfo{}, "Internal", "sum project bytes"
		}
		if total > *in.policy.MaxTotalBytes {
			_ = h.repo.DeleteObjects(ctx, in.bucketRow.ID, []string{info.Key})
			_ = h.store.Delete(ctx, in.ref)
			return storage.ObjectInfo{}, "TooLarge", "upload would exceed the project's storage quota"
		}
	}

	return info, "", ""
}

// UploadObject streams the multipart "file" part straight into
// ObjectStore.Put. It deliberately avoids every net/http form-parsing helper
// that buffers the body to memory or spills it to os.TempDir (ADR-0016) —
// MultipartReader is the one API here that does neither.
//
// S12 wraps the body in http.MaxBytesReader (per-object cap) and sets a
// per-request read deadline, then — after a successful write — records the
// object's metadata row (UpsertObject) and enforces the project-wide quota
// against the resulting SumProjectBytes, rolling back both the physical
// object and its metadata row on violation. That post-decode half lives in
// storeObject above, shared with the S3-shaped PUT.
func (h *Handler) UploadObject(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	projectIDStr := chi.URLParam(r, "projectID")
	bucket := chi.URLParam(r, "bucket")
	bucketRow, ok := h.requireBucket(w, r, projectID, bucket, accessWrite)
	if !ok {
		return
	}

	policy, maxObjectBytes, err := h.resolveObjectSizeLimit(r.Context(), projectID)
	if err != nil {
		h.writeInternal(w, r, "get project storage policy", err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxObjectBytes)

	// Best-effort: httptest.ResponseRecorder and similar test writers don't
	// support this (http.ErrNotSupported), which is expected and harmless —
	// the deadline is a defense-in-depth ceiling, not a correctness
	// requirement checked here.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(artifactStreamDeadline))

	overwrite := r.URL.Query().Get("overwrite") == "true"

	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "request is not multipart/form-data")
		return
	}

	var part *multipart.Part
	for {
		p, err := mr.NextPart()
		if err != nil {
			if isMaxBytesError(err) {
				writeError(w, http.StatusRequestEntityTooLarge, "TooLarge", "object exceeds the project's max_object_bytes limit")
				return
			}
			writeError(w, http.StatusBadRequest, "InvalidArgument", "file field is required")
			return
		}
		if p.FormName() == "file" {
			part = p
			break
		}
		_ = p.Close()
	}
	defer func() { _ = part.Close() }()

	// Not part.FileName(): per RFC 7578 it runs the client-declared filename
	// through filepath.Base, silently truncating a multi-segment key like
	// "a/b/c.png" to "c.png". Parsing Content-Disposition directly recovers
	// the raw value; storage.NewObjectRef's own validateKey (S1) is the
	// actual traversal guard — it rejects ".." segments, leading/trailing
	// slashes, and empty segments, which is what FileName's stripping
	// exists to prevent, so bypassing it here does not reopen that gap.
	_, dispositionParams, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid Content-Disposition on file field")
		return
	}
	filename := dispositionParams["filename"]
	if filename == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "file field must include a filename")
		return
	}

	ref, err := storage.NewObjectRef(projectIDStr, bucket, filename)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidKey", err.Error())
		return
	}

	if !overwrite {
		if _, statErr := h.store.Stat(r.Context(), ref); statErr == nil {
			writeError(w, http.StatusConflict, "AlreadyExists", "key already exists; pass overwrite=true to replace it")
			return
		} else if !errors.Is(statErr, storage.ErrNotFound) {
			h.writeStorageError(r.Context(), w, "stat object", statErr)
			return
		}
	}

	contentType := part.Header.Get("Content-Type")
	if contentType == "" {
		contentType = mimeFromExtension(filename)
	}

	info, code, message := h.storeObject(r.Context(), storeObjectInput{
		projectID:   projectID,
		bucketRow:   bucketRow,
		ref:         ref,
		body:        part,
		contentType: contentType,
		policy:      policy,
	})
	if code != "" {
		writeError(w, statusForCode(code), code, message)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"key":        info.Key,
		"size_bytes": info.Size,
		"media_type": contentType,
		"etag":       info.ETag,
		"created_at": info.LastModified,
	})
}

func (h *Handler) BatchDeleteObjects(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	projectIDStr := chi.URLParam(r, "projectID")
	bucket := chi.URLParam(r, "bucket")
	bucketRow, ok := h.requireBucket(w, r, projectID, bucket, accessWrite)
	if !ok {
		return
	}

	var req struct {
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid request body")
		return
	}
	if len(req.Keys) == 0 {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "keys must not be empty")
		return
	}

	refs := make([]storage.ObjectRef, 0, len(req.Keys))
	failed := make([]batchDeleteFailure, 0)
	for _, key := range req.Keys {
		ref, err := storage.NewObjectRef(projectIDStr, bucket, key)
		if err != nil {
			failed = append(failed, batchDeleteFailure{Key: key, Code: "InvalidKey", Message: err.Error()})
			continue
		}
		refs = append(refs, ref)
	}

	deleted := []string{}
	if len(refs) > 0 {
		result, err := h.store.DeleteBatch(r.Context(), refs)
		if err != nil {
			h.writeStorageError(r.Context(), w, "delete objects", err)
			return
		}
		deleted = result.Deleted
		if deleted == nil {
			deleted = []string{}
		}
		for _, f := range result.Failed {
			code, message := h.classifyStorageError(r.Context(), "delete object "+f.Key, f.Err)
			failed = append(failed, batchDeleteFailure{Key: f.Key, Code: code, Message: message})
		}
		if len(deleted) > 0 {
			if err := h.repo.DeleteObjects(r.Context(), bucketRow.ID, deleted); err != nil {
				h.writeInternal(w, r, "delete object metadata", err)
				return
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "failed": failed})
}

// streamObject performs the object read shared by both download
// representations — the native route (DownloadObject) and the S3-shaped one
// the SDK's artifact toolkit speaks (DownloadObjectS3, s3.go). Everything
// that decides what bytes go on the wire lives here: the Range parse, the
// storage Get, the write deadline, the entity headers and the copy. Only the
// error vocabulary differs between the two, so on failure this writes
// nothing and returns a code in THIS package's vocabulary for the caller to
// render in its own — the same split parseListObjectsQuery/listBucketObjects
// already use for the two listings, and for the same reason: a divergence
// between the representations would be invisible until an index run silently
// downloaded nothing.
//
// A non-empty code means nothing has been written yet and the caller must
// render the error. An empty code means the response is complete.
//
// The caller has already resolved and authorized the project and confirmed
// the bucket exists.
func (h *Handler) streamObject(w http.ResponseWriter, r *http.Request, ref storage.ObjectRef, key string) (code, message string) {
	status := http.StatusOK
	var rng *storage.ByteRange
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		parsed, ok := parseRangeHeader(rangeHeader)
		if !ok {
			return "InvalidArgument", "invalid Range header"
		}
		rng = &parsed
		status = http.StatusPartialContent
	}

	body, info, err := h.store.Get(r.Context(), ref, rng)
	if err != nil {
		return h.classifyStorageError(r.Context(), "get object", err)
	}
	defer func() { _ = body.Close() }()

	// S12: replaces the global http.Server WriteTimeout (120s), which
	// otherwise caps every download at 120 seconds regardless of size —
	// see cmd/elitea-main/http_server.go. Best-effort for the same reason
	// as the read deadline above.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(artifactStreamDeadline))

	contentType := info.ContentType
	if contentType == "" {
		contentType = mimeFromExtension(key)
	}
	w.Header().Set("Content-Type", contentType)
	// Advertise range support on every download, not only on a 206. A client
	// that probes with a HEAD or a first full GET decides from this header
	// whether it may resume.
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	if info.ETag != "" {
		w.Header().Set("ETag", info.ETag)
	}
	if rng != nil {
		// RFC 7233 makes Content-Range mandatory on a 206. Without it a
		// browser media element cannot place the bytes in the timeline and
		// aborts, and a resuming downloader fails.
		total := info.TotalSize
		if total <= 0 {
			total = rng.Start + info.Size
		}
		if info.Size <= 0 || rng.Start >= total {
			// An unsatisfiable range answers 416 and states the current
			// length. The caller renders the body; this header survives,
			// because writeError has not written the head yet.
			w.Header().Del("Content-Length")
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(total, 10))
			return "RangeNotSatisfiable", "requested range is not satisfiable"
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.Start, rng.Start+info.Size-1, total))
	}
	w.WriteHeader(status)
	_, _ = io.Copy(w, body)
	return "", ""
}

func (h *Handler) DownloadObject(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	projectIDStr := chi.URLParam(r, "projectID")
	bucket := chi.URLParam(r, "bucket")
	key := objectKeyFromRequest(r)

	// Deliberately before the bucket lookup, as it always has been: a
	// malformed key is caller error and must 400 regardless of whether the
	// bucket happens to exist.
	ref, err := storage.NewObjectRef(projectIDStr, bucket, key)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidKey", err.Error())
		return
	}

	if _, ok := h.requireBucket(w, r, projectID, bucket, accessRead); !ok {
		return
	}

	if code, message := h.streamObject(w, r, ref, key); code != "" {
		writeError(w, statusForCode(code), code, message)
	}
}

func (h *Handler) StatObject(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	projectIDStr := chi.URLParam(r, "projectID")
	bucket := chi.URLParam(r, "bucket")
	key := objectKeyFromRequest(r)

	ref, err := storage.NewObjectRef(projectIDStr, bucket, key)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

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
	// A client that HEADs first to decide whether it may resume never sees
	// the download response's headers, so this one belongs here too.
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	if info.ETag != "" {
		w.Header().Set("ETag", info.ETag)
	}
	if !info.LastModified.IsZero() {
		w.Header().Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	}
	w.WriteHeader(http.StatusOK)
}

// DeleteObject stats before deleting so a missing key 404s — Delete itself
// is documented idempotent (S1 errors.go), so it would not otherwise error
// on an already-absent object.
func (h *Handler) DeleteObject(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	projectIDStr := chi.URLParam(r, "projectID")
	bucket := chi.URLParam(r, "bucket")
	key := objectKeyFromRequest(r)

	ref, err := storage.NewObjectRef(projectIDStr, bucket, key)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidKey", err.Error())
		return
	}

	bucketRow, ok := h.requireBucket(w, r, projectID, bucket, accessWrite)
	if !ok {
		return
	}

	if _, err := h.store.Stat(r.Context(), ref); err != nil {
		h.writeStorageError(r.Context(), w, "stat object", err)
		return
	}

	if err := h.store.Delete(r.Context(), ref); err != nil {
		h.writeStorageError(r.Context(), w, "delete object", err)
		return
	}

	if err := h.repo.DeleteObjects(r.Context(), bucketRow.ID, []string{key}); err != nil {
		h.writeInternal(w, r, "delete object metadata", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// parseRangeHeader supports a single "bytes=start-end" or "bytes=start-"
// range, matching what storage.ByteRange can express. Multi-range and
// suffix ("bytes=-N") requests are rejected rather than guessed at.
func parseRangeHeader(h string) (storage.ByteRange, bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(h, prefix) {
		return storage.ByteRange{}, false
	}
	spec := strings.TrimPrefix(h, prefix)
	if strings.Contains(spec, ",") {
		return storage.ByteRange{}, false
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 || parts[0] == "" {
		return storage.ByteRange{}, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 {
		return storage.ByteRange{}, false
	}
	end := int64(-1)
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return storage.ByteRange{}, false
		}
	}
	return storage.ByteRange{Start: start, End: end}, true
}
