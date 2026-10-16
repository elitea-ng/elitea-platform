package artifacts

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// presignPartResponse is PresignUploadPartResponse's JSON shape
// (api/openapi/v2.yaml).
type presignPartResponse struct {
	PartNumber int32     `json:"part_number"`
	URL        string    `json:"url"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// completedPart is one entry of CompleteMultipartRequest.parts
// (api/openapi/v2.yaml) — the caller reports back the part number and the
// ETag the backend returned for that part's own presigned PUT.
type completedPart struct {
	PartNumber int32  `json:"part_number"`
	ETag       string `json:"etag"`
}

// completeMultipartRequest is CompleteMultipartRequest's JSON shape
// (api/openapi/v2.yaml).
type completeMultipartRequest struct {
	Parts []completedPart `json:"parts"`
}

// requireOwnedMultipartGrant fetches a grant by id alone (unscoped) and
// enforces every check the three multipart continuation endpoints (part
// presign, complete, abort) share before touching the store: the grant must
// belong to projectID — 403, not 404, since the id itself is real and the
// plan's own S16 acceptance criterion ("a part call with another project's
// grant returns 403") requires exactly that distinction from a genuinely
// unknown id — must be a PUT grant carrying an upload_id (i.e. one
// CreateTransferGrant actually started as native multipart, not a
// single-shot grant or a GET grant), and must not already be consumed.
// Callers still need to apply their own expiry check afterward — see
// AbortMultipartUpload for why that one deliberately does not.
func (h *Handler) requireOwnedMultipartGrant(w http.ResponseWriter, r *http.Request, projectID int64, grantID string) (repos.TransferGrantRow, bool) {
	if !looksLikeGrantID(grantID) {
		writeError(w, http.StatusNotFound, "NotFound", "grant not found")
		return repos.TransferGrantRow{}, false
	}

	grant, err := h.repo.GetTransferGrantByID(r.Context(), grantID)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "NotFound", "grant not found")
		return repos.TransferGrantRow{}, false
	}
	if err != nil {
		h.writeInternal(w, r, "get transfer grant", err)
		return repos.TransferGrantRow{}, false
	}
	if grant.ProjectID != projectID {
		writeError(w, http.StatusForbidden, "AccessDenied", "grant belongs to a different project")
		return repos.TransferGrantRow{}, false
	}
	if grant.Method != methodPut || grant.UploadID == nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "grant is not a native multipart upload")
		return repos.TransferGrantRow{}, false
	}
	if grant.ConsumedAt != nil {
		writeError(w, http.StatusConflict, "AlreadyExists", "grant has already been committed or aborted")
		return repos.TransferGrantRow{}, false
	}
	// The system-bucket rule on the grant's key, BEFORE any continuation
	// changes state (a complete or abort consumes the grant).
	bucketRow, err := h.repo.GetBucketByID(r.Context(), grant.BucketID)
	if err != nil {
		h.writeInternal(w, r, "get grant bucket", err)
		return repos.TransferGrantRow{}, false
	}
	if !h.allowKeys(w, r, projectID, bucketRow, []string{grant.Key}) {
		return repos.TransferGrantRow{}, false
	}
	return grant, true
}

// multipartObjectRef resolves grant.BucketID to a bucket name and builds
// the ObjectRef every multipart continuation call needs — shared by all
// three handlers below.
func (h *Handler) multipartObjectRef(w http.ResponseWriter, r *http.Request, projectID int64, grant repos.TransferGrantRow) (storage.ObjectRef, repos.BucketRow, bool) {
	bucketRow, err := h.repo.GetBucketByID(r.Context(), grant.BucketID)
	if err != nil {
		h.writeInternal(w, r, "get grant bucket", err)
		return storage.ObjectRef{}, repos.BucketRow{}, false
	}
	ref, err := storage.NewObjectRef(strconv.FormatInt(projectID, 10), bucketRow.Name, grant.Key)
	if err != nil {
		h.writeInternal(w, r, "build object ref", err)
		return storage.ObjectRef{}, repos.BucketRow{}, false
	}
	// The system-bucket rule, on the grant's key: every continuation (part
	// presign, complete, abort) is judged as the create was.
	if !h.allowKeys(w, r, projectID, bucketRow, []string{grant.Key}) {
		return storage.ObjectRef{}, repos.BucketRow{}, false
	}
	return ref, bucketRow, true
}

// PresignUploadPart issues a presigned URL for one part of a native
// multipart upload (S16). The part's own expiry never outlives the grant's:
// minting a fresh 15-minute window on every part call would let a caller
// keep the effective upload session alive indefinitely, defeating the
// grant's own TTL boundary.
func (h *Handler) PresignUploadPart(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	grantID := chi.URLParam(r, "grantID")
	grant, ok := h.requireOwnedMultipartGrant(w, r, projectID, grantID)
	if !ok {
		return
	}
	if time.Now().After(grant.ExpiresAt) {
		writeError(w, http.StatusPreconditionFailed, "PreconditionFailed", "grant has expired")
		return
	}

	partNumber, err := strconv.ParseInt(chi.URLParam(r, "partNumber"), 10, 32)
	if err != nil || partNumber < 1 || partNumber > maxPartNumber {
		writeError(w, http.StatusBadRequest, "InvalidArgument", fmt.Sprintf("part number must be between 1 and %d", maxPartNumber))
		return
	}

	ref, _, ok := h.multipartObjectRef(w, r, projectID, grant)
	if !ok {
		return
	}

	// Recomputed here rather than reused from the expiry check above: a real
	// (if normally sub-millisecond) gap separates that check from this line
	// — multipartObjectRef makes its own database round trip in between. A
	// non-positive ttl at this point means the grant crossed its own
	// expires_at during that gap; treat it exactly like the earlier check
	// would have, rather than handing a zero/negative duration to the
	// backend's own presign call.
	ttl := time.Until(grant.ExpiresAt)
	if ttl <= 0 {
		writeError(w, http.StatusPreconditionFailed, "PreconditionFailed", "grant has expired")
		return
	}
	url, err := h.store.PresignPart(r.Context(), ref, storage.UploadID(*grant.UploadID), int32(partNumber), ttl)
	if err != nil {
		h.writeStorageError(r.Context(), w, "presign upload part", err)
		return
	}

	writeJSON(w, http.StatusOK, presignPartResponse{
		PartNumber: int32(partNumber),
		URL:        url,
		ExpiresAt:  grant.ExpiresAt,
	})
}

// CompleteMultipartUpload finalizes a native multipart upload (S16) and
// applies the exact same size/digest/media-type/quota verification a
// single-shot CommitTransferGrant does (finalizeGrantCommit) — completing
// via multipart is not a lesser-verified path.
//
// Claims the grant (MarkTransferGrantConsumed) BEFORE calling
// store.CompleteMultipart, not after — an adversarial-review finding
// confirmed that claiming afterward (this handler's first-draft shape, and
// still CommitTransferGrant's shape today) races against a concurrent
// AbortMultipartUpload on the same grant: both call an irreversible,
// mutually exclusive backend operation on the same upload_id, and with no
// prior claim, two concurrent same-project requests could both observe
// consumed_at == nil and then race on which backend call lands first — if
// abort wins, it can delete the parts complete is about to assemble, and
// there is no way to recover a legitimately-uploaded transfer. Claiming
// first makes the two mutually exclusive: whichever of complete/abort
// wins the claim proceeds; the other gets a clean 409 without ever
// touching the store.
//
// Trade-off, and it is a real one: a completion that fails verification
// (digest/media-type/quota, inside finalizeGrantCommit) is no longer
// retriable in place the way a single-shot CommitTransferGrant is — the
// grant is already consumed by the time verification runs, unlike
// CommitTransferGrant's claim-after-verification order. A caller whose
// large upload fails verification must start a new grant. Judged
// acceptable: multipart exists specifically for objects above the 64 MiB
// threshold, where closing the concurrent-abort race matters more than
// in-place retry convenience after a failed check.
func (h *Handler) CompleteMultipartUpload(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	grantID := chi.URLParam(r, "grantID")
	grant, ok := h.requireOwnedMultipartGrant(w, r, projectID, grantID)
	if !ok {
		return
	}
	if time.Now().After(grant.ExpiresAt) {
		writeError(w, http.StatusPreconditionFailed, "PreconditionFailed", "grant has expired")
		return
	}

	var req completeMultipartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid request body")
		return
	}
	if len(req.Parts) == 0 {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "parts must be non-empty")
		return
	}
	parts := make([]storage.Part, len(req.Parts))
	for i, p := range req.Parts {
		if p.PartNumber < 1 || p.PartNumber > maxPartNumber {
			writeError(w, http.StatusBadRequest, "InvalidArgument", fmt.Sprintf("part_number must be between 1 and %d", maxPartNumber))
			return
		}
		if p.ETag == "" {
			writeError(w, http.StatusBadRequest, "InvalidArgument", "etag is required for every part")
			return
		}
		parts[i] = storage.Part{Number: p.PartNumber, ETag: p.ETag}
	}

	if err := h.repo.MarkTransferGrantConsumed(r.Context(), grant.ID); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, "AlreadyExists", "grant has already been committed or aborted")
			return
		}
		h.writeInternal(w, r, "mark transfer grant consumed", err)
		return
	}

	ref, bucketRow, ok := h.multipartObjectRef(w, r, projectID, grant)
	if !ok {
		return
	}

	if _, err := h.store.CompleteMultipart(r.Context(), ref, storage.UploadID(*grant.UploadID), parts); err != nil {
		h.writeStorageError(r.Context(), w, "complete multipart upload", err)
		return
	}

	h.finalizeGrantCommit(r.Context(), w, projectID, grant, bucketRow, ref, true)
}

// AbortMultipartUpload cancels a native multipart upload (S16) and marks
// the grant terminal via the same consumed_at column CommitTransferGrant
// uses — repurposed here as "no longer usable" rather than adding a
// dedicated aborted_at column for what is, from every other endpoint's
// point of view, an identical outcome: no further part/complete/abort call
// on this grant should succeed.
//
// Claims the grant BEFORE calling store.AbortMultipart, the other half of
// CompleteMultipartUpload's own race fix (see its doc comment for the full
// reasoning): if a concurrent CompleteMultipartUpload wins the claim
// first, this call must not touch the store at all — a partial or
// completed upload must never be silently deleted just because an abort
// request happened to be in flight at the same time. Losing the claim is
// therefore a hard 409, not a tolerated no-op: the earlier "already-done,
// treat as success" version of this check was itself part of the bug the
// review found, since it could report success on an abort that in fact
// lost to a concurrent, successful completion.
//
// Deliberately no expiry check, unlike PresignUploadPart/
// CompleteMultipartUpload: an expired multipart session must still be
// abortable. Gating cleanup on the same TTL that already blocks new
// uploads would leave an expired-but-still-open provider-side upload with
// no way to release it through this API (S14's lifecycle-rule GC would
// eventually reap it, but that is a fallback, not a substitute for a
// caller being able to clean up on demand).
func (h *Handler) AbortMultipartUpload(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	grantID := chi.URLParam(r, "grantID")
	grant, ok := h.requireOwnedMultipartGrant(w, r, projectID, grantID)
	if !ok {
		return
	}

	if err := h.repo.MarkTransferGrantConsumed(r.Context(), grant.ID); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, "AlreadyExists", "grant has already been committed or aborted")
			return
		}
		h.writeInternal(w, r, "mark transfer grant consumed", err)
		return
	}

	ref, _, ok := h.multipartObjectRef(w, r, projectID, grant)
	if !ok {
		return
	}

	// The grant is already claimed at this point — if the store-side abort
	// itself now fails, there is no way to retry through this grant (its
	// own already-consumed check above would reject a second attempt).
	// Bounded, not permanent: S14's incomplete-multipart-upload lifecycle
	// rule (S3/GCS; Azure documented gap) eventually reclaims an orphaned
	// backend-side session regardless.
	if err := h.store.AbortMultipart(r.Context(), ref, storage.UploadID(*grant.UploadID)); err != nil {
		h.writeStorageError(r.Context(), w, "abort multipart upload", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
