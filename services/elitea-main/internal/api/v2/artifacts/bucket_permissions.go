package artifacts

// Per-bucket access lists — the surface legacy/plugins/artifacts calls
// `bucket_permissions`, ported whole: the read/write routes that author the
// exceptions, and the enforcement that makes them mean something on every
// object verb.
//
// # The legacy contract, in one paragraph
//
// A bucket carries no owner and no public/private flag. Access is expressed as
// EXCEPTIONS to a default of full access, one map per project member:
//
//	{"reports": ["read"], "secrets": []}
//
// utils/utils.py:99-132 states the semantics in its own docstring, and this
// file reproduces them exactly in accessPermitted below:
//
//	empty map                     -> unrestricted
//	bucket absent from the map    -> no restriction for that bucket (ALLOW)
//	bucket present with []        -> explicitly blocked (DENY)
//	bucket present with ["read"]  -> read only
//	bucket present with ["read","write"] -> full access
//
// The reference UI says the same thing on screen: its table is headed
// "Exceptions" and its banner reads "All users have read/write permissions by
// default" (frontends/EliteaUI [fsd]/features/artifacts/ui/bucket-access/).
//
// # Three deliberate differences from the legacy behaviour
//
//  1. THE STORE. Legacy keeps the map inside an `s3_api_credentials`
//     configuration row keyed by a per-user S3 access key. This platform issues
//     no such key, so the map is a table —
//     migrations/shared/0118_artifact_bucket_permissions.sql carries the full
//     argument. The wire shape still serves the legacy `bucket_permissions`
//     map, so the contract the reference page reads is unchanged; only
//     `access_key_id` is gone, because there is no access key to name.
//
//  2. DELETING A BUCKET IS A WRITE. legacy's `DELETE
//     /api/v2/artifacts/buckets/default/{pid}?name=X` (api/v2/buckets.py:284-293)
//     carries the RBAC permission and NO bucket-permission decorator, while
//     every other write verb has one. A member explicitly blocked on bucket X
//     can therefore delete bucket X. AGENTS.md: compatibility never requires
//     preserving a vulnerability. DeleteBucket is gated on `write` here.
//
//  3. THE PROJECT-ADMIN BYPASS APPLIES EVERYWHERE. Legacy applies it only in
//     the bucket listing (api/v2/buckets.py:64-75 calls
//     `admin_check_user_is_admin`); the object verbs have none, so an exception
//     written against an administrator's own account locks that administrator
//     out with no way back. See ArtifactBucketPermissionsRepository.IsProjectAdmin.
//
// # And one legacy self-contradiction, resolved
//
// Legacy filters the bucket LISTING two different ways. api/v2/buckets.py:64-75
// is a WHITELIST — once a member has any entry at all, they see only the
// buckets named in it, INCLUDING ones set to `[]` that they cannot open.
// s3/handlers/bucket.py:53-59 is a BLACKLIST — it hides only the buckets set to
// `[]`. The two answer differently for the same member, and the reference UI
// reads the S3 one, so the blacklist is the behaviour users actually have. It
// is also the only one that agrees with the enforcement in this file: a
// listing that hides a bucket the caller may read is a listing that lies. This
// port uses the blacklist.

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	platformauth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// The two verbs an exception can carry. They are the legacy strings and the
// only two values api/v2/bucket_permissions.py:96-99 accepts.
const (
	accessRead  = "read"
	accessWrite = "write"
)

// bucketPermissionRow is the JSON-facing shape of one member's exceptions
// (components/schemas/BucketPermissionRow in api/openapi/v2.yaml).
type bucketPermissionRow struct {
	UserID int64  `json:"user_id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	// BucketPermissions is the legacy map, verbatim. An empty slice means "no
	// access" and MUST NOT be marshalled as null — see the repository.
	BucketPermissions map[string][]string `json:"bucket_permissions"`
}

// setBucketPermissionsRequest is the PUT body. Legacy names the subject
// `access_key_id`; this platform names the member directly.
type setBucketPermissionsRequest struct {
	UserID            *int64              `json:"user_id"`
	BucketPermissions map[string][]string `json:"bucket_permissions"`
}

// deleteBucketPermissionRequest is the DELETE body, which legacy also carries
// in the body rather than the query (api/v2/bucket_permissions.py:130-163).
type deleteBucketPermissionRequest struct {
	UserID *int64  `json:"user_id"`
	Bucket *string `json:"bucket"`
}

// accessPermitted is legacy's check_bucket_perm_from_dict
// (utils/utils.py:99-132), one exception at a time.
//
// found=false is the no-exception case and ALLOWS. The quirk on the last line
// is legacy's and is kept: ANY non-empty permission list grants read, so an
// exception of ["write"] reads as well as it writes. Narrowing it here would
// deny a caller that the reference platform allows, on data an operator
// authored under the reference's rules.
func accessPermitted(permissions []string, found bool, need string) bool {
	if !found {
		return true
	}
	if len(permissions) == 0 {
		return false
	}
	if need == accessRead {
		return true
	}
	return slices.Contains(permissions, accessWrite)
}

// callerUserID is the member an exception can name: the owning user behind the
// authenticated principal.
//
// ok is false for a principal with no owning user — a workload identity, or an
// unauthenticated context in a test. The check is then SKIPPED, which is
// legacy's own guard (`if user_id and bucket and not check_bucket_permission…`,
// utils/utils.py:177-238): an exception is a statement about a person, and
// there is no person to match. Route-level RBAC still applies; this is the
// second gate, not the first.
func callerUserID(ctx context.Context) (int64, bool) {
	user, ok := platformauth.UserFromContext(ctx)
	if !ok {
		return 0, false
	}
	return user.OwningUserID()
}

// authorizeBucket answers whether the caller may act on one bucket.
//
// It reads the exception FIRST and asks about project-admin membership only
// when the exception refuses, so the common path — no exception — costs one
// indexed lookup and the admin query never runs.
func (h *Handler) authorizeBucket(
	ctx context.Context, projectID int64, bucket, need string,
) (bool, error) {
	userID, ok := callerUserID(ctx)
	if !ok {
		return true, nil
	}
	permissions, found, err := h.repo.GetBucketPermission(ctx, projectID, userID, bucket)
	if err != nil {
		return false, err
	}
	if accessPermitted(permissions, found, need) {
		return true, nil
	}
	return h.repo.IsProjectAdmin(ctx, projectID, userID)
}

// visibleBuckets applies the listing filter — legacy's blacklist, see this
// file's header. A project admin sees every bucket.
func (h *Handler) visibleBuckets(
	ctx context.Context, projectID int64, rows []repos.BucketRow,
) ([]repos.BucketRow, error) {
	userID, ok := callerUserID(ctx)
	if !ok {
		return rows, nil
	}
	exceptions, err := h.repo.ListUserBucketPermissions(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	if len(exceptions) == 0 {
		return rows, nil
	}
	isAdmin, err := h.repo.IsProjectAdmin(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	if isAdmin {
		return rows, nil
	}
	visible := make([]repos.BucketRow, 0, len(rows))
	for _, row := range rows {
		permissions, found := exceptions[row.Name]
		if accessPermitted(permissions, found, accessRead) {
			visible = append(visible, row)
		}
	}
	return visible, nil
}

// ListBucketPermissions serves the ACL read —
// GET /api/v2/artifacts/bucket_permissions/{projectID}.
//
// It answers every exception in the project, not the exceptions for one
// bucket, because that is what the legacy route answers
// (api/v2/bucket_permissions.py:42-54) and what the reference page needs: it
// renders one bucket's exceptions and must still know which members already
// carry an exception elsewhere before it writes a replacement map.
func (h *Handler) ListBucketPermissions(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	rows, err := h.repo.ListBucketPermissions(r.Context(), projectID)
	if err != nil {
		h.writeInternal(w, r, "list bucket permissions", err)
		return
	}
	out := make([]bucketPermissionRow, 0, len(rows))
	for _, row := range rows {
		permissions := row.BucketPermissions
		if permissions == nil {
			permissions = map[string][]string{}
		}
		out = append(out, bucketPermissionRow{
			UserID:            row.UserID,
			Name:              row.Name,
			Email:             row.Email,
			BucketPermissions: permissions,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(out), "rows": out})
}

// validateBucketPermissions reproduces the legacy validation
// (api/v2/bucket_permissions.py:83-99) and its messages' meaning: the map must
// be a map, each value must be a list, and every value must be `read` or
// `write`.
func validateBucketPermissions(permissions map[string][]string) (string, bool) {
	for bucket, verbs := range permissions {
		if !bucketNamePattern.MatchString(bucket) {
			return "invalid bucket name " + bucket, false
		}
		for _, verb := range verbs {
			if verb != accessRead && verb != accessWrite {
				return "invalid permission for bucket " + bucket + "; allowed: read, write", false
			}
		}
	}
	return "", true
}

// SetBucketPermissions serves the ACL write —
// PUT /api/v2/artifacts/bucket_permissions/{projectID}.
//
// It REPLACES the member's whole exception map, which is what legacy's PUT
// does (rpc/s3_credentials.py:362) and what the reference UI depends on: the
// only way that page removes an exception is to send the map without the key.
func (h *Handler) SetBucketPermissions(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	var body setBucketPermissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid request body")
		return
	}
	if body.UserID == nil || *body.UserID <= 0 {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "user_id is required")
		return
	}
	if body.BucketPermissions == nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "bucket_permissions is required")
		return
	}
	if message, valid := validateBucketPermissions(body.BucketPermissions); !valid {
		writeError(w, http.StatusBadRequest, "InvalidArgument", message)
		return
	}
	if err := h.repo.ReplaceUserBucketPermissions(
		r.Context(), projectID, *body.UserID, body.BucketPermissions,
	); err != nil {
		h.writeInternal(w, r, "set bucket permissions", err)
		return
	}
	writeJSON(w, http.StatusOK, bucketPermissionRow{
		UserID:            *body.UserID,
		BucketPermissions: body.BucketPermissions,
	})
}

// DeleteBucketPermission removes ONE exception —
// DELETE /api/v2/artifacts/bucket_permissions/{projectID}.
//
// The subject travels in the body, as it does in legacy
// (api/v2/bucket_permissions.py:130-163). 404 when there was no exception to
// remove: reporting success over an absent row is how a page comes to show a
// restriction that is not there.
func (h *Handler) DeleteBucketPermission(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	var body deleteBucketPermissionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid request body")
		return
	}
	if body.UserID == nil || *body.UserID <= 0 {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "user_id is required")
		return
	}
	if body.Bucket == nil || *body.Bucket == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "bucket is required")
		return
	}
	removed, err := h.repo.DeleteUserBucketPermission(r.Context(), projectID, *body.UserID, *body.Bucket)
	if err != nil {
		h.writeInternal(w, r, "delete bucket permission", err)
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "NotFound", "no exception for this member and bucket")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// denyMessage is the sentence legacy answers with, per verb
// (utils/utils.py:177-238).
func denyMessage(need string) string {
	if need == accessWrite {
		return "You have read-only permission for this bucket"
	}
	return "You do not have access to this bucket"
}
