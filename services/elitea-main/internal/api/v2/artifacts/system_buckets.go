package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// The SYSTEM buckets of a project (`bucket_type = 'system'`: the chat
// attachment bucket, the index artifact bucket) and an agent turn.
//
// An agent turn reaches this public object API through its Office host
// (data_analysis, the Office families, imagegen and the tracker attachment
// tools), under the turn's runtime credential, which is the ACTOR'S OWN PAT:
// the bearer alone cannot tell a turn from the person's own script. The
// worker therefore names the turn on every such request with the claim it
// runs under (RuntimeClaimHeaders), and when a request names one, the
// system-bucket rule below applies (review round 2): a turn reads and writes
// a system bucket only under `{project attachment bucket}/{the claim's own
// conversation}/...` — next to its own attachments, where data_analysis and
// the Office tools put their outputs — and nowhere else. A prompt-injected
// file name such as `/chat-attachments/<another conversation>/x.xlsx` is
// refused, for read and write alike.
//
// The claim is VERIFIED, not trusted: it must be a live claim of an agent
// turn, of this project, whose actor is the caller (the fence token is a
// secret only the worker holding the claim has). A request that names a claim
// it cannot prove is refused, so naming one never widens anything.
//
// A request that names no claim is a person's own session or script and
// keeps the per-bucket access list as before. The worker always names the
// claim, and a prompt cannot make it stop.

// RuntimeClaimHeaders are the request headers a turn's Office host sends.
const (
	RuntimeClaimExecutionHeader  = "X-Elitea-Execution-Id"
	RuntimeClaimGenerationHeader = "X-Elitea-Execution-Generation"
	RuntimeClaimIDHeader         = "X-Elitea-Claim-Id"
	RuntimeClaimFenceHeader      = "X-Elitea-Fence"
)

// RuntimeClaim is a claim as a request names it.
type RuntimeClaim struct {
	ExecutionID string
	Generation  uint64
	ClaimID     string
	FenceToken  []byte
}

// RuntimeClaimScope is what a verified claim allows into system buckets.
type RuntimeClaimScope struct {
	ProjectID      int64
	ActorID        int64
	ConversationID string
}

// RuntimeClaimResolver verifies a named claim: a live, unreleased claim of a
// running agent execution, with its fence. ErrRuntimeClaimRefused when it is
// not one.
type RuntimeClaimResolver interface {
	ResolveRuntimeClaim(ctx context.Context, claim RuntimeClaim) (RuntimeClaimScope, error)
}

// ErrRuntimeClaimRefused is a named claim that is not a live agent claim.
var ErrRuntimeClaimRefused = errors.New("the named runtime claim is not a live agent claim")

// WithRuntimeClaims wires the claim verification. Without it a request that
// names a claim reaches no system bucket (fail closed).
func (h *Handler) WithRuntimeClaims(resolver RuntimeClaimResolver) *Handler {
	h.claims = resolver
	return h
}

// requestRuntimeClaim reads the claim a request names: (claim, true, nil),
// (zero, false, nil) when it names none, or an error for a malformed one.
func requestRuntimeClaim(r *http.Request) (RuntimeClaim, bool, error) {
	execution := r.Header.Get(RuntimeClaimExecutionHeader)
	generation := r.Header.Get(RuntimeClaimGenerationHeader)
	claimID := r.Header.Get(RuntimeClaimIDHeader)
	fence := r.Header.Get(RuntimeClaimFenceHeader)
	if execution == "" && generation == "" && claimID == "" && fence == "" {
		return RuntimeClaim{}, false, nil
	}
	parsed, err := strconv.ParseUint(generation, 10, 64)
	token, decodeErr := base64.RawURLEncoding.DecodeString(fence)
	if execution == "" || len(execution) > 256 || claimID == "" || len(claimID) > 256 ||
		err != nil || parsed == 0 || decodeErr != nil || len(token) != sha256.Size {
		return RuntimeClaim{}, true, ErrRuntimeClaimRefused
	}
	return RuntimeClaim{ExecutionID: execution, Generation: parsed, ClaimID: claimID, FenceToken: token}, true, nil
}

// systemBucketScopeKey is the object key, or the listing prefix, a request
// addresses inside the bucket.
func systemBucketScopeKey(r *http.Request) string {
	if key := strings.TrimPrefix(chi.URLParam(r, "*"), "/"); key != "" {
		return key
	}
	return strings.TrimPrefix(r.URL.Query().Get("prefix"), "/")
}

// allowSystemBucket applies the rule above to a fetched bucket row, for a
// request whose key (or listing prefix) is in its route. It reports whether
// the request may go on. A request that addresses no key (a bucket-level verb,
// an unscoped listing) never reaches a system bucket under a claim.
func (h *Handler) allowSystemBucket(r *http.Request, projectID int64, row repos.BucketRow) (bool, error) {
	return h.allowSystemBucketKey(r, projectID, row, systemBucketScopeKey(r))
}

// allowSystemBucketKey is allowSystemBucket for a key the route does not
// carry (an upload names it in its multipart part).
func (h *Handler) allowSystemBucketKey(r *http.Request, projectID int64, row repos.BucketRow, key string) (bool, error) {
	prefix, scoped, err := h.systemBucketPrefix(r, projectID, row)
	if err != nil || !scoped {
		return err == nil && !scoped, err
	}
	// A listing prefix may be the conversation itself; an object key under
	// it is validated as a key by the route.
	return prefix != "" && strings.HasPrefix(key, prefix), nil
}

// systemBucketPrefix is the key prefix a claim-naming request is confined to
// in a system bucket: ("", false) when the rule does not apply (not a system
// bucket, or no claim named), ("", true) when the claim reaches nothing in it,
// and ("{conversation}/", true) for the claim's own conversation in the
// project's attachment bucket.
func (h *Handler) systemBucketPrefix(r *http.Request, projectID int64, row repos.BucketRow) (string, bool, error) {
	if row.BucketType != "system" {
		return "", false, nil
	}
	claim, named, err := requestRuntimeClaim(r)
	if !named {
		return "", false, nil
	}
	if err != nil || h.claims == nil {
		return "", true, nil
	}
	scope, err := h.claims.ResolveRuntimeClaim(r.Context(), claim)
	if errors.Is(err, ErrRuntimeClaimRefused) {
		return "", true, nil
	}
	if err != nil {
		return "", true, err
	}
	caller, ok := callerUserID(r.Context())
	if !ok || caller != scope.ActorID || scope.ProjectID != projectID || !canonicalUUID(scope.ConversationID) {
		return "", true, nil
	}
	attachmentBucket, err := h.attachmentBucketName(r.Context(), projectID)
	if err != nil {
		return "", true, err
	}
	if row.Name != attachmentBucket {
		return "", true, nil
	}
	return scope.ConversationID + "/", true, nil
}

// requireBucketForUpload is requireBucket for the multipart upload, whose key
// is the part's filename: the system-bucket rule is applied to it by
// allowUploadKey once the part is read.
func (h *Handler) requireBucketForUpload(w http.ResponseWriter, r *http.Request, projectID int64, bucket string) (repos.BucketRow, bool) {
	return h.requireBucketChecked(w, r, projectID, bucket, accessWrite, false)
}

// allowUploadKey answers the upload's system-bucket check for its key,
// writing the refusal itself.
func (h *Handler) allowUploadKey(w http.ResponseWriter, r *http.Request, projectID int64, row repos.BucketRow, key string) bool {
	allowed, err := h.allowSystemBucketKey(r, projectID, row, key)
	if err != nil {
		h.writeInternal(w, r, "authorize system bucket", err)
		return false
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "Forbidden", systemBucketRefusal)
		return false
	}
	return true
}

const systemBucketRefusal = "this turn may reach only its own conversation's files in a system bucket"

// attachmentBucketName is the project's attachment bucket: its storage
// policy's, else the default.
func (h *Handler) attachmentBucketName(ctx context.Context, projectID int64) (string, error) {
	policy, err := h.repo.GetProjectStoragePolicy(ctx, projectID)
	if err != nil {
		return "", err
	}
	if policy.AttachmentBucket != nil && strings.TrimSpace(*policy.AttachmentBucket) != "" {
		return strings.TrimSpace(*policy.AttachmentBucket), nil
	}
	return storage.DefaultAttachmentBucket, nil
}

// canonicalUUID accepts only a lowercase canonical UUID: it becomes an
// authorization prefix, so it must not be empty or a bare number.
func canonicalUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
