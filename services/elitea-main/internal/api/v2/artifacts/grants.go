package artifacts

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

const (
	// defaultGrantTTL is S15's fixed grant lifetime. The plan text names a
	// "maximum 60" minutes alongside "default 15," but
	// CreateTransferGrantRequest (api/openapi/v2.yaml) has no caller-facing
	// TTL field at all — every grant gets exactly this TTL; there is no
	// override to cap.
	defaultGrantTTL = 15 * time.Minute

	// grantDigestAlgorithm is the only digest algorithm this stage
	// supports. The plan is explicit that commit verification works by
	// "streaming the object through a SHA-256 hasher" — not a
	// generic multi-algorithm scheme — so a grant request naming any other
	// algorithm is rejected at creation time, not silently accepted and
	// left unverifiable at commit.
	grantDigestAlgorithm = "sha256"

	// methodPut is the only transfer method a grant can carry.
	//
	// DEFECT, fixed here: "GET" was accepted too. A grant always derives its
	// storage key from a freshly generated grant id. No request field names
	// an existing object. A GET grant therefore presigned a key that by
	// construction held nothing. Every download URL it issued answered 404.
	//
	// A GET request is now refused at creation. An authenticated download
	// already has an endpoint: GET
	// /api/v2/artifacts/objects/{projectID}/{bucket}/{key} (objects.go's
	// DownloadObject).
	methodPut = "PUT"

	// multipartThreshold is S16's "objects above 64 MiB" gate: a PUT grant
	// request at or below this size always gets a single-shot grant (the
	// S15 behavior), even when the backend supports native multipart.
	// Multipart exists to avoid re-uploading a huge object from scratch
	// after a partial-network failure — it is not a benefit for small
	// objects, and every additional part/complete round trip has a real
	// cost (ownership-checked database round trips this package would
	// rather not pay for a 10 KiB upload).
	multipartThreshold = 64 << 20

	// maxPartNumber matches S3's own hard limit (parts are numbered
	// 1-10000) — the plan does not name a number, but Azure block blobs
	// share the same ceiling (50000 blocks, though this codebase's own Put
	// path documents an 8 MiB chunk size elsewhere) and GCS never reaches
	// this code path at all (NativeMultipart is always false). Using S3's
	// tighter number as the shared validation ceiling rejects a
	// pathological part count before it ever reaches a real backend.
	maxPartNumber = 10000
)

// transferGrantRequest is CreateTransferGrantRequest's JSON shape
// (api/openapi/v2.yaml).
type transferGrantRequest struct {
	Method string `json:"method"`
	// DisplayName is accepted per the schema but intentionally unused: the
	// storage key is always the server-derived grant ID (generateGrantID),
	// never anything caller-supplied, so there is nothing for a display
	// name to influence.
	DisplayName string  `json:"display_name"`
	ContentType string  `json:"content_type"`
	MaxBytes    int64   `json:"max_bytes"`
	DigestAlg   *string `json:"digest_alg"`
	Digest      *string `json:"digest"`
}

// transferGrantResponse is TransferGrantResponse's JSON shape
// (api/openapi/v2.yaml). Exactly one of URL and UploadID is present (S16):
// a single-shot grant carries url and no upload_id; a native-multipart
// grant carries upload_id and no url — there is no one presigned URL for
// the whole object, only per-part ones a caller requests separately via
// PresignUploadPart.
type transferGrantResponse struct {
	GrantID     string    `json:"grant_id"`
	URL         string    `json:"url,omitempty"`
	Method      string    `json:"method"`
	ExpiresAt   time.Time `json:"expires_at"`
	ContentType string    `json:"content_type"`
	MaxBytes    int64     `json:"max_bytes"`
	UploadID    *string   `json:"upload_id,omitempty"`
}

// generateGrantID returns a random UUIDv4, following the same crypto/rand
// construction used elsewhere in this codebase (e.g.
// internal/api/v2/auth/util.go's generateUUID) — package-local here since
// that one is unexported and lives in a different package. This value
// doubles as the grant's server-derived object key (see CreateTransferGrant):
// it must be unpredictable for the same reason a grant ID must be — a
// caller-guessable key would let a third party overwrite it before the
// legitimate presigned PUT lands.
func generateGrantID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:4]) + "-" +
		hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" +
		hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:]), nil
}

// looksLikeGrantID checks the same UUIDv4 shape generateGrantID produces.
// CommitTransferGrant checks a path-supplied grantID against this before
// ever querying the database — GetArtifactTransferGrant casts its id
// argument to ::uuid, so a malformed grantID would otherwise surface as a
// generic Postgres "invalid input syntax" 500 instead of a clean 404.
func looksLikeGrantID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}

// CreateTransferGrant issues a short-lived, single-use upload grant bound to
// a server-derived key. The caller never supplies a key directly. That matches
// S9's upload path convention of deriving identity from server state rather
// than trusting client input for anything that becomes a storage key. See
// docs/plans/storage-migration-plan.md S15.
//
// A grant is always an upload. A download grant is refused, because the key
// a grant reserves is new and therefore empty — see methodPut.
func (h *Handler) CreateTransferGrant(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	projectIDStr := chi.URLParam(r, "projectID")
	bucket := chi.URLParam(r, "bucket")
	// accessWrite: every grant this route issues is a PUT (see methodPut), so
	// issuing one is authorising a write to the bucket.
	bucketRow, ok := h.requireBucketChecked(w, r, projectID, bucket, accessWrite, scopeCallerKeys)
	if !ok {
		return
	}

	var req transferGrantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid request body")
		return
	}
	if req.Method != methodPut {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "method must be PUT")
		return
	}
	if req.ContentType == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "content_type is required")
		return
	}
	if req.MaxBytes <= 0 {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "max_bytes must be positive")
		return
	}

	digestAlg, digest, ok := parseGrantDigest(w, req)
	if !ok {
		return
	}

	grantID, err := generateGrantID()
	if err != nil {
		h.writeInternal(w, r, "generate grant id", err)
		return
	}
	ref, err := storage.NewObjectRef(projectIDStr, bucket, grantID)
	if err != nil {
		h.writeInternal(w, r, "build object ref", err)
		return
	}
	// The system-bucket rule, on the key the grant reserves.
	if !h.allowKeys(w, r, projectID, bucketRow, []string{grantID}) {
		return
	}
	expiresAt := time.Now().Add(defaultGrantTTL)

	// S16: "for objects above 64 MiB, gated on Capabilities().NativeMultipart"
	// — a PUT request past the threshold on a backend that supports it
	// starts a native multipart upload instead of a single presigned PUT.
	// GCS (NativeMultipart always false) and any PUT at or under the
	// threshold transparently fall through to the existing S15 path — same
	// endpoint, same request shape, different response.
	var url string
	var uploadID *string
	// grantKey is what the grant row records and the grant URL addresses.
	// A presigned single-shot PUT addresses a STAGING key, never grantID
	// itself: see grantStagingKey.
	grantKey := grantID
	if req.MaxBytes > multipartThreshold && h.store.Capabilities().NativeMultipart {
		id, err := h.store.StartMultipart(r.Context(), ref, storage.PutOptions{ContentType: req.ContentType})
		if err != nil {
			h.writeStorageError(r.Context(), w, "start multipart upload", err)
			return
		}
		idStr := string(id)
		uploadID = &idStr
	} else {
		if h.store.Capabilities().Presign {
			grantKey = grantStagingKey(grantID)
		}
		url, err = h.grantURL(r.Context(), projectIDStr, bucket, grantKey, req.ContentType)
		if err != nil {
			h.writeStorageError(r.Context(), w, "presign transfer grant", err)
			return
		}
	}

	if _, err := h.repo.CreateTransferGrant(r.Context(), repos.NewTransferGrantInput{
		ID:          grantID,
		ProjectID:   projectID,
		BucketID:    bucketRow.ID,
		Key:         grantKey,
		Method:      req.Method,
		ContentType: req.ContentType,
		MaxBytes:    req.MaxBytes,
		DigestAlg:   digestAlg,
		Digest:      digest,
		UploadID:    uploadID,
		ExpiresAt:   expiresAt,
	}); err != nil {
		if uploadID != nil {
			// StartMultipart above already created live, billed backend-side
			// state (e.g. S3 CreateMultipartUpload) before this metadata
			// write failed — the caller never received a grant_id/upload_id
			// to clean it up with, and nothing else in this codebase will
			// ever reference this specific upload_id again. Best-effort
			// compensation: if this abort itself fails too, S14's own
			// incomplete-multipart-upload lifecycle rule (S3/GCS; Azure
			// documented gap — see S14) eventually reclaims it, matching
			// this package's existing tolerance for bounded, non-permanent
			// leaks elsewhere (e.g. rejectCommit's own cleanup-error
			// handling).
			_ = h.store.AbortMultipart(r.Context(), ref, storage.UploadID(*uploadID))
		}
		h.writeInternal(w, r, "create transfer grant", err)
		return
	}

	// S18: "every grant issuance" — the one audit line this handler is
	// responsible for, covering both the single-shot and native-multipart
	// paths above (they share this one success point, so one call here
	// covers both, matching how they share the rest of this handler).
	storage.LogAudit(r.Context(), "grant_issued", bucket, grantID, projectIDStr, "success")

	writeJSON(w, http.StatusOK, transferGrantResponse{
		GrantID:     grantID,
		URL:         url,
		Method:      req.Method,
		ExpiresAt:   expiresAt,
		ContentType: req.ContentType,
		MaxBytes:    req.MaxBytes,
		UploadID:    uploadID,
	})
}

// parseGrantDigest validates digest_alg/digest are both present or both
// absent — a partial declaration is a caller error, not something to
// silently accept and leave unverifiable at commit — and, if present,
// that digest_alg is the one algorithm this stage supports and digest is
// valid hex. Writes the response itself on failure, matching requireBucket's
// convention.
func parseGrantDigest(w http.ResponseWriter, req transferGrantRequest) (alg *string, digest []byte, ok bool) {
	if req.DigestAlg == nil && req.Digest == nil {
		return nil, nil, true
	}
	if req.DigestAlg == nil || req.Digest == nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "digest_alg and digest must both be supplied, or neither")
		return nil, nil, false
	}
	normalized := strings.ToLower(*req.DigestAlg)
	if normalized != grantDigestAlgorithm {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "digest_alg must be \"sha256\"")
		return nil, nil, false
	}
	decoded, err := hex.DecodeString(*req.Digest)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "digest must be hex-encoded")
		return nil, nil, false
	}
	if len(decoded) != sha256.Size {
		writeError(w, http.StatusBadRequest, "InvalidArgument", fmt.Sprintf("digest must be %d bytes for sha256", sha256.Size))
		return nil, nil, false
	}
	return &normalized, decoded, true
}

// grantURL returns the URL the grant response carries — a genuine presigned
// URL when the backend supports it, or the streaming facade's own endpoint
// otherwise (S15: "return the facade URL, not an error").
//
// For a facade fallback, the returned URL is this same package's existing
// multipart upload endpoint (objects.go's UploadObject). That URL does not
// itself pin the key the grant reserved. That endpoint derives its key from
// the multipart request's own Content-Disposition filename, not the URL.
//
// A client using the facade path must upload with grantID as that filename.
// The object then lands at the key CommitTransferGrant will verify. This
// limitation of the fallback is real and deliberate, not a bug.
// Presign-capable backends do not have it, because the key is baked into the
// signed URL itself.
//
// There is no download branch. See methodPut for why a GET grant cannot
// address an object.
func (h *Handler) grantURL(ctx context.Context, projectIDStr, bucket, key, contentType string) (string, error) {
	if h.store.Capabilities().Presign {
		ref, err := storage.NewObjectRef(projectIDStr, bucket, key)
		if err != nil {
			return "", err
		}
		return h.store.PresignPut(ctx, ref, defaultGrantTTL, storage.PutOptions{ContentType: contentType})
	}
	return fmt.Sprintf("/api/v2/artifacts/objects/%s/%s", projectIDStr, bucket), nil
}

// grantStagingKey is the key a presigned single-shot PUT grant addresses.
//
// DEFECT this closes (follow-up to #1290): the grant URL used to address
// grantID, the key the committed object is served from. A presigned URL
// cannot be revoked and stays valid for defaultGrantTTL after a commit, so
// the caller could PUT other bytes over the committed object out of band —
// past the digest, size and quota checks, and leaving byte_length stale.
//
// The URL now addresses grantID+".upload" only. Commit copies that staging
// object to grantID inside the store and verifies the COPY, which no URL
// can address, then deletes the staging object. A PUT after the commit only
// recreates a staging object nothing serves; the retention sweep reclaims it
// once the URL has expired (sweepCommittedGrantStaging).
//
// The grant row's key records which shape a grant has: a staged grant's key
// differs from its id. The facade fallback (no presign) and native multipart
// keep key == id: the facade upload is an authenticated, quota-checked write,
// and a completed multipart upload id accepts no further parts.
func grantStagingKey(grantID string) string {
	return grantID + repos.GrantStagingSuffix
}

// rejectCommit deletes the uploaded object and writes the typed error
// response for a failed commit verification. A failed cleanup delete is not
// silently discarded: it's folded into the response's details field, since
// a rejected commit never gets a metadata row (UpsertObject runs only after
// every check passes) — an object that fails verification AND fails to
// delete would otherwise be a physical, un-tracked orphan invisible to
// S14's retention sweeper forever (it works off elitea_storage.objects
// rows, and this object never gets one).
func (h *Handler) rejectCommit(ctx context.Context, w http.ResponseWriter, ref storage.ObjectRef, status int, code, message string) {
	if delErr := h.store.Delete(ctx, ref); delErr != nil {
		// The cause goes to the log, not to the caller: an unclassified
		// object-store error carries the endpoint and the provider text.
		// The caller only needs to know that the rejected upload is still
		// present.
		h.logInternal(ctx, "delete rejected upload", delErr)
		_, cleanupMessage := h.classifyStorageError(ctx, "delete rejected upload", delErr)
		writeJSON(w, status, map[string]any{
			"error": map[string]any{
				"code":    code,
				"message": message,
				"details": map[string]any{"cleanup_error": cleanupMessage},
			},
		})
		return
	}
	writeError(w, status, code, message)
}

// CommitTransferGrant verifies size, digest, and media type against the
// grant row before the uploaded object becomes tracked (UpsertObject) and
// stamps the grant consumed. Only PUT grants can be committed — a GET
// grant has nothing for commit to verify or register; see
// docs/plans/storage-migration-plan.md S15 for why this endpoint's
// semantics for a GET grant are otherwise undefined by this stage.
//
// Digest verification cannot use Stat/HeadObject — see the plan's own note,
// confirmed empirically against a real backend, that no cheap provider-side
// call yields a SHA-256 for an object that landed via presigned PUT on any
// backend. This calls Get and streams the full object through a SHA-256
// hasher, which is also how the object's actual byte count is learned (used
// for the max_bytes check) — one full read from the store, paid once per
// commit, not once per verification.
func (h *Handler) CommitTransferGrant(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	grantID := chi.URLParam(r, "grantID")
	if !looksLikeGrantID(grantID) {
		writeError(w, http.StatusNotFound, "NotFound", "grant not found")
		return
	}

	grant, err := h.repo.GetTransferGrant(r.Context(), grantID, projectID)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "NotFound", "grant not found")
		return
	}
	if err != nil {
		h.writeInternal(w, r, "get transfer grant", err)
		return
	}
	if grant.Method != methodPut {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "only PUT grants can be committed")
		return
	}
	if grant.ConsumedAt != nil {
		writeError(w, http.StatusConflict, "AlreadyExists", "grant has already been committed")
		return
	}
	if time.Now().After(grant.ExpiresAt) {
		writeError(w, http.StatusPreconditionFailed, "PreconditionFailed", "grant has expired")
		return
	}

	bucketRow, err := h.repo.GetBucketByID(r.Context(), grant.BucketID)
	if err != nil {
		h.writeInternal(w, r, "get grant bucket", err)
		return
	}
	projectIDStr := strconv.FormatInt(projectID, 10)
	if !repos.IsGrantStagingKey(grant.ID, grant.Key) {
		// The facade fallback: the object already sits at its final key.
		ref, err := storage.NewObjectRef(projectIDStr, bucketRow.Name, grant.Key)
		if err != nil {
			h.writeInternal(w, r, "build object ref", err)
			return
		}
		if !h.allowKeys(w, r, projectID, bucketRow, []string{grant.Key}) {
			return
		}
		claimedAt, ok := h.claimGrantForCommit(w, r, grant.ID)
		if !ok {
			return
		}
		if !h.finalizeGrantCommit(r.Context(), w, projectID, grant, bucketRow, ref, true) {
			h.releaseGrantClaim(r.Context(), grant.ID, claimedAt)
		}
		return
	}

	// A staged grant (grantStagingKey): copy the staging object to the final
	// key FIRST and verify the copy. Verifying the staging object and then
	// copying it would leave a window in which the still-valid URL replaces
	// the verified bytes before the copy reads them.
	staging, err := storage.NewObjectRef(projectIDStr, bucketRow.Name, grant.Key)
	if err != nil {
		h.writeInternal(w, r, "build object ref", err)
		return
	}
	final, err := storage.NewObjectRef(projectIDStr, bucketRow.Name, grant.ID)
	if err != nil {
		h.writeInternal(w, r, "build object ref", err)
		return
	}
	if !h.allowKeys(w, r, projectID, bucketRow, []string{grant.ID}) {
		return
	}
	// Claim BEFORE the copy. The consumed_at read above is only an early
	// exit: two concurrent commits can both pass it, and without a claim the
	// later copy would overwrite the object the earlier one had already
	// verified and registered, or (on a mismatch) rejectCommit would delete
	// it. Only the claim holder ever writes or deletes final, and a failed
	// commit hands the claim back so the caller can retry on this grant.
	claimedAt, ok := h.claimGrantForCommit(w, r, grant.ID)
	if !ok {
		return
	}
	if err := h.copyGrantUpload(r.Context(), staging, final); err != nil {
		h.releaseGrantClaim(r.Context(), grant.ID, claimedAt)
		h.writeStorageError(r.Context(), w, "copy uploaded object", err)
		return
	}
	if !h.finalizeGrantCommit(r.Context(), w, projectID, grant, bucketRow, final, true) {
		h.releaseGrantClaim(r.Context(), grant.ID, claimedAt)
		return
	}
	// Committed. A failed delete leaves bytes nothing serves; the retention
	// sweep reclaims them after the grant expires.
	if err := h.store.Delete(r.Context(), staging); err != nil && !errors.Is(err, storage.ErrNotFound) {
		h.logInternal(r.Context(), "delete committed grant staging object", err)
	}
}

// claimGrantForCommit takes CommitTransferGrant's single-use claim. Losing it
// to a concurrent commit (or to the retention sweeper) is the same 409 as a
// grant committed earlier.
func (h *Handler) claimGrantForCommit(w http.ResponseWriter, r *http.Request, grantID string) (time.Time, bool) {
	claimedAt, err := h.repo.ClaimTransferGrant(r.Context(), grantID)
	if errors.Is(err, storage.ErrAlreadyExists) {
		writeError(w, http.StatusConflict, "AlreadyExists", "grant has already been committed")
		return time.Time{}, false
	}
	if err != nil {
		h.writeInternal(w, r, "claim transfer grant", err)
		return time.Time{}, false
	}
	return claimedAt, true
}

// releaseGrantClaim hands back a failed commit's claim. It runs detached from
// the request's cancellation: a client that hung up mid-commit must not
// leave the grant consumed with nothing committed. A release that still
// fails leaves the grant unusable, not inconsistent: the retention sweep
// reclaims its staging object once the URL expires.
func (h *Handler) releaseGrantClaim(ctx context.Context, grantID string, claimedAt time.Time) {
	if _, err := h.repo.ReleaseTransferGrantClaim(context.WithoutCancel(ctx), grantID, claimedAt); err != nil {
		h.logInternal(ctx, "release transfer grant claim", err)
	}
}

// copyGrantUpload copies a staged grant's upload to its final key inside the
// store (S3 CopyObject, GCS rewrite, Azure Copy Blob). Only a backend with no
// server-side copy, or one that refuses this source, reads the bytes back
// through Main; finalizeGrantCommit verifies the result either way.
func (h *Handler) copyGrantUpload(ctx context.Context, staging, final storage.ObjectRef) error {
	_, err := storage.CopyObject(ctx, h.store, staging, final)
	if !errors.Is(err, storage.ErrNotSupported) {
		return err
	}
	body, info, err := h.store.Get(ctx, staging, nil)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	_, err = h.store.Put(ctx, final, body, storage.PutOptions{ContentType: info.ContentType, ContentLength: info.Size})
	return err
}

// finalizeGrantCommit is the verification and metadata-recording shared by
// CommitTransferGrant (a single-shot presigned PUT) and
// CompleteMultipartUpload (S16, a native-multipart upload) — from this
// point on in either flow, the object is a single opaque blob the store can
// Get, and the plan's size/digest/media-type/quota checks apply identically
// regardless of how the bytes arrived.
//
// claimed reports whether the caller has already called
// MarkTransferGrantConsumed on this grant before invoking this method:
// CompleteMultipartUpload does this to close a race against a concurrent
// AbortMultipartUpload (see its own doc comment) — when true, this method
// skips its own MarkTransferGrantConsumed call at the end, since a second
// call would only ever return ErrAlreadyExists and turn an otherwise
// successful commit into a hollow 409.
//
// ref is the object's FINAL key, which the metadata row and the response
// name. It reports whether the commit succeeded; it has written the response
// either way.
func (h *Handler) finalizeGrantCommit(ctx context.Context, w http.ResponseWriter, projectID int64, grant repos.TransferGrantRow, bucketRow repos.BucketRow, ref storage.ObjectRef, claimed bool) bool {
	body, info, err := h.store.Get(ctx, ref, nil)
	if err != nil {
		h.writeStorageError(ctx, w, "get uploaded object", err)
		return false
	}
	defer func() { _ = body.Close() }()

	// Cheap early exit before paying for a full read+hash: Get's own
	// response already reports the object's size. The authoritative check
	// stays on n below (what was actually read and hashed), not this one —
	// this is purely an optimization for the common oversized case, not a
	// replacement for it.
	if info.Size > grant.MaxBytes {
		h.rejectCommit(ctx, w, ref, http.StatusRequestEntityTooLarge, "TooLarge", "uploaded object exceeds the grant's max_bytes")
		return false
	}

	hasher := sha256.New()
	n, err := io.Copy(hasher, body)
	if err != nil {
		h.writeInternalCtx(ctx, w, "read uploaded object", err)
		return false
	}

	if n > grant.MaxBytes {
		h.rejectCommit(ctx, w, ref, http.StatusRequestEntityTooLarge, "TooLarge", "uploaded object exceeds the grant's max_bytes")
		return false
	}
	// No "info.ContentType != ''" exemption: the plan is explicit that this
	// check is "mandatory, not defensive." An empty reported ContentType is
	// not a signal to skip verification — grant.ContentType is always
	// non-empty (CreateTransferGrant rejects an empty content_type), so an
	// empty info.ContentType can never legitimately match it. Backends
	// legitimately report an empty ContentType when a client's out-of-band
	// presigned PUT omits the Content-Type header entirely — exactly the
	// case this check exists to catch, not exempt.
	if info.ContentType != grant.ContentType {
		h.rejectCommit(ctx, w, ref, http.StatusConflict, "MediaTypeMismatch", "uploaded object's media type does not match the grant")
		return false
	}
	if grant.DigestAlg != nil {
		actual := hasher.Sum(nil)
		if !bytes.Equal(actual, grant.Digest) {
			h.rejectCommit(ctx, w, ref, http.StatusConflict, "DigestMismatch", "uploaded object's digest does not match the grant")
			return false
		}
	}

	// Same rule as storeObject: the retention window starts at the write,
	// not at the bucket's frozen expires_at. See computeExpiresAt.
	row, err := h.repo.UpsertObject(ctx, repos.NewObjectInput{
		BucketID:   grant.BucketID,
		Key:        ref.Key(),
		ByteLength: n,
		MediaType:  grant.ContentType,
		ExpiresAt:  computeExpiresAt(bucketRow.RetentionDays),
	})
	if err != nil {
		h.writeInternalCtx(ctx, w, "record object metadata", err)
		return false
	}

	// S12's project-wide storage quota applies to every write path, not
	// just UploadObject — a caller was never exempt from max_total_bytes
	// just because the bytes arrived via a presigned PUT instead of a
	// multipart POST. Mirrors UploadObject's identical check-after-write,
	// roll-back-on-violation shape (objects.go): the object's exact byte
	// length isn't known until the upload is read, so the check can only
	// run after UpsertObject, not before it.
	policy, err := h.repo.GetProjectStoragePolicy(ctx, projectID)
	if err != nil {
		h.writeInternalCtx(ctx, w, "get project storage policy", err)
		return false
	}
	if policy.MaxTotalBytes != nil {
		total, err := h.repo.SumProjectBytes(ctx, projectID)
		if err != nil {
			h.writeInternalCtx(ctx, w, "sum project bytes", err)
			return false
		}
		if total > *policy.MaxTotalBytes {
			_ = h.repo.DeleteObjects(ctx, grant.BucketID, []string{ref.Key()})
			h.rejectCommit(ctx, w, ref, http.StatusRequestEntityTooLarge, "TooLarge", "commit would exceed the project's storage quota")
			return false
		}
	}

	if !claimed {
		if err := h.repo.MarkTransferGrantConsumed(ctx, grant.ID); err != nil {
			if errors.Is(err, storage.ErrAlreadyExists) {
				writeError(w, http.StatusConflict, "AlreadyExists", "grant has already been committed")
				return false
			}
			h.writeInternalCtx(ctx, w, "mark transfer grant consumed", err)
			return false
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"key":        ref.Key(),
		"size_bytes": n,
		"media_type": grant.ContentType,
		"etag":       info.ETag,
		"created_at": row.CreatedAt,
		"updated_at": row.UpdatedAt,
	})
	return true
}
