package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// bucketNamePattern matches api/openapi/v2.yaml's CreateBucketRequest.name
// pattern. A name that does not already match this is rejected, never
// normalised — see docs/plans/storage-migration-plan.md S8.
var bucketNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

// Bucket is the JSON-facing shape of one bucket
// (components/schemas/Bucket in api/openapi/v2.yaml).
type Bucket struct {
	Name          string          `json:"name"`
	Type          string          `json:"type"`
	IsPinned      bool            `json:"is_pinned"`
	Tags          json.RawMessage `json:"tags"`
	RetentionDays *int32          `json:"retention_days"`
	ExpiresAt     *time.Time      `json:"expires_at"`
	SizeBytes     int64           `json:"size_bytes"`
	ObjectCount   int64           `json:"object_count"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// Repository is the persistence dependency the bucket-plane handlers need —
// the union of the S6 bucket-metadata methods
// (internal/infra/db/repos.ArtifactBucketsRepository) and the object
// aggregate/policy methods (ArtifactObjectsRepository) they read. The two
// concrete repositories share no method names, so a struct embedding both
// satisfies this interface via promotion — see router.go's wiring.
// fake_repo_test.go provides the in-memory test double.
type Repository interface {
	ListBuckets(ctx context.Context, projectID int64) ([]repos.BucketRow, error)
	GetBucket(ctx context.Context, projectID int64, name string) (repos.BucketRow, error)
	CreateBucket(ctx context.Context, input repos.NewBucketInput) (repos.BucketRow, error)
	UpdateBucketRetention(ctx context.Context, id int64, retentionDays *int32, expiresAt *time.Time) (repos.BucketRow, error)
	SetBucketPinned(ctx context.Context, id int64, pinned bool) (repos.BucketRow, error)
	UpdateBucketTags(ctx context.Context, id int64, tags json.RawMessage) (repos.BucketRow, error)
	SoftDeleteBucket(ctx context.Context, id int64) error
	SumBucketBytes(ctx context.Context, bucketID int64) (int64, error)
	CountBucketObjects(ctx context.Context, bucketID int64) (int64, error)
	GetProjectStoragePolicy(ctx context.Context, projectID int64) (repos.ProjectStoragePolicy, error)
	// UpsertObject, DeleteObjects (metadata), and SumProjectBytes are S12's
	// additions — the object-plane handlers (objects.go) had no metadata
	// footprint at all before S12, which left SumBucketBytes/SumProjectBytes/
	// CountBucketObjects permanently zero for real uploads and made the
	// project-quota check below meaningless without them.
	UpsertObject(ctx context.Context, input repos.NewObjectInput) (repos.ObjectRow, error)
	DeleteObjects(ctx context.Context, bucketID int64, keys []string) error
	SumProjectBytes(ctx context.Context, projectID int64) (int64, error)
	// CreateTransferGrant, GetTransferGrant, MarkTransferGrantConsumed, and
	// GetBucketByID are S15's additions, backing grants.go. GetBucketByID
	// (added for S14's sweeper) resolves a grant's bucket_id back to a
	// bucket name — a grant row has no bucket name of its own, only the
	// internal database id.
	CreateTransferGrant(ctx context.Context, input repos.NewTransferGrantInput) (repos.TransferGrantRow, error)
	GetTransferGrant(ctx context.Context, id string, projectID int64) (repos.TransferGrantRow, error)
	MarkTransferGrantConsumed(ctx context.Context, id string) error
	GetBucketByID(ctx context.Context, id int64) (repos.BucketRow, error)
	// GetTransferGrantByID is S16's addition, backing multipart.go's
	// ownership check — see its own doc comment in
	// internal/infra/db/repos/artifact_transfer_grants.go for why the
	// multipart continuation endpoints need an unscoped lookup that
	// GetTransferGrant's project-scoped query cannot provide.
	GetTransferGrantByID(ctx context.Context, id string) (repos.TransferGrantRow, error)
	// The per-bucket access list (bucket_permissions.go). These six methods
	// are part of THIS interface, rather than an optional dependency the
	// handler may or may not hold, on purpose: an access check that a
	// composition root can forget to wire is an access check that is off in
	// production and green in every unit test. Putting them here makes the
	// omission a compile error — internal/api/router.go's artifactRepoAdapter
	// cannot satisfy Repository without embedding the repository that answers
	// them.
	ListBucketPermissions(ctx context.Context, projectID int64) ([]repos.BucketPermissionRow, error)
	GetBucketPermission(ctx context.Context, projectID, userID int64, bucket string) ([]string, bool, error)
	ListUserBucketPermissions(ctx context.Context, projectID, userID int64) (map[string][]string, error)
	ReplaceUserBucketPermissions(ctx context.Context, projectID, userID int64, permissions map[string][]string) error
	DeleteUserBucketPermission(ctx context.Context, projectID, userID int64, bucket string) (bool, error)
	IsProjectAdmin(ctx context.Context, projectID, userID int64) (bool, error)
}

type Handler struct {
	repo   Repository
	store  storage.ObjectStore
	logger *slog.Logger
}

func NewHandler(repo Repository, store storage.ObjectStore) *Handler {
	return &Handler{repo: repo, store: store, logger: slog.Default()}
}

// WithLogger replaces the logger the internal-error path writes the cause
// to. The default is slog.Default().
func (h *Handler) WithLogger(logger *slog.Logger) *Handler {
	if logger != nil {
		h.logger = logger
	}
	return h
}

func (h *Handler) log() *slog.Logger {
	if h.logger == nil {
		return slog.Default()
	}
	return h.logger
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the artifact API's typed error envelope
// (components/schemas/Error in api/openapi/v2.yaml).
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

// logInternal writes the cause of a failed artifact request to the log.
// AGENTS.md forbids a raw err.Error() across a trust boundary: a database
// error carries the host, the user and the SQLSTATE code, and an
// unclassified object-store error carries the endpoint and the bucket. The
// caller gets the operation name only, so the log is the sole record of the
// cause.
func (h *Handler) logInternal(ctx context.Context, op string, err error) {
	h.log().ErrorContext(ctx, "artifact request failed", "op", op, "error", err)
}

// writeInternalCtx logs the cause and answers 500 with the operation name.
func (h *Handler) writeInternalCtx(ctx context.Context, w http.ResponseWriter, op string, err error) {
	h.logInternal(ctx, op, err)
	writeError(w, http.StatusInternalServerError, "Internal", op)
}

// writeInternal is writeInternalCtx for a handler that still holds the
// request.
func (h *Handler) writeInternal(w http.ResponseWriter, r *http.Request, op string, err error) {
	h.writeInternalCtx(r.Context(), w, op, err)
}

// writeInternalS3 is writeInternalCtx in the S3 error vocabulary the SDK
// speaks. The code differs ("InternalError", not "Internal"); the rule that
// the cause stays in the log does not.
func (h *Handler) writeInternalS3(w http.ResponseWriter, r *http.Request, op string, err error) {
	h.logInternal(r.Context(), op, err)
	writeError(w, http.StatusInternalServerError, "InternalError", op)
}

// classifyStorageError maps an object-store error onto the typed code and
// the message the caller may see. A classified sentinel (NotFound,
// InvalidKey, TooLarge, ...) carries no backend detail, so its own text
// helps the caller. An unclassified error carries the endpoint, the
// credentials context and the provider text. It goes to the log, and the
// caller gets the operation name.
func (h *Handler) classifyStorageError(ctx context.Context, op string, err error) (code, message string) {
	code = storageErrorCode(err)
	if code == "Internal" {
		h.logInternal(ctx, op, err)
		return code, op
	}
	return code, err.Error()
}

func parseProjectID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// computeExpiresAt derives the concrete deadline S14's sweeper acts on from
// a policy retention_days value — nil in, nil out (no expiry).
//
// Every write path calls this at the moment it writes, never once per
// bucket. The bucket's own expires_at is a single absolute instant, frozen
// when the bucket was created or its retention was changed. An object that
// copied that instant was born expired as soon as the bucket passed its own
// deadline. The retention sweeper then deleted it minutes after a successful
// 201. Retention is an age per object, which is also what the legacy S3
// lifecycle rule expressed (Expiration.Days).
func computeExpiresAt(retentionDays *int32) *time.Time {
	if retentionDays == nil {
		return nil
	}
	t := time.Now().AddDate(0, 0, int(*retentionDays))
	return &t
}

func bucketFromRow(row repos.BucketRow, sizeBytes, objectCount int64) Bucket {
	tags := row.Tags
	if len(tags) == 0 {
		tags = json.RawMessage(`{}`)
	}
	return Bucket{
		Name:          row.Name,
		Type:          row.BucketType,
		IsPinned:      row.IsPinned,
		Tags:          tags,
		RetentionDays: row.RetentionDays,
		ExpiresAt:     row.ExpiresAt,
		SizeBytes:     sizeBytes,
		ObjectCount:   objectCount,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
}

// enrichBucket adds size_bytes/object_count — both single-aggregate queries
// (S6 SumBucketBytes/CountBucketObjects), never a full object listing.
func (h *Handler) enrichBucket(ctx context.Context, row repos.BucketRow) (Bucket, error) {
	sizeBytes, err := h.repo.SumBucketBytes(ctx, row.ID)
	if err != nil {
		return Bucket{}, fmt.Errorf("sum bucket bytes: %w", err)
	}
	objectCount, err := h.repo.CountBucketObjects(ctx, row.ID)
	if err != nil {
		return Bucket{}, fmt.Errorf("count bucket objects: %w", err)
	}
	return bucketFromRow(row, sizeBytes, objectCount), nil
}

func (h *Handler) ListBuckets(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}

	rows, err := h.repo.ListBuckets(r.Context(), projectID)
	if err != nil {
		h.writeInternal(w, r, "list buckets", err)
		return
	}
	// Hide the buckets this caller is explicitly blocked on. See
	// bucket_permissions.go on why the filter is a blacklist.
	rows, err = h.visibleBuckets(r.Context(), projectID, rows)
	if err != nil {
		h.writeInternal(w, r, "list buckets", err)
		return
	}

	buckets := make([]Bucket, 0, len(rows))
	for _, row := range rows {
		b, err := h.enrichBucket(r.Context(), row)
		if err != nil {
			h.writeInternal(w, r, "list buckets", err)
			return
		}
		buckets = append(buckets, b)
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": buckets})
}

func (h *Handler) GetBucket(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	name := chi.URLParam(r, "bucket")

	row, ok := h.requireBucket(w, r, projectID, name, accessRead)
	if !ok {
		return
	}

	b, err := h.enrichBucket(r.Context(), row)
	if err != nil {
		h.writeInternal(w, r, "get bucket", err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// checkRetentionLimit rejects a requested retention_days that exceeds the
// project's policy ceiling. Per S8: read RetentionMaxDays, not
// RetentionDefaultDays — the latter is what applies when a caller omits a
// value, not a ceiling on what they may request. A nil ceiling (or a
// missing policy row, which GetProjectStoragePolicy never errors on) means
// unlimited.
//
// UpdateBucket is the only caller. CreateBucket uses
// resolveCreateRetention, which also applies RetentionDefaultDays. On PATCH
// an explicit `"retention_days": null` means "clear the expiry", so the
// default must not apply there.
func (h *Handler) checkRetentionLimit(ctx context.Context, projectID int64, retentionDays *int32) (bool, error) {
	if retentionDays == nil {
		return true, nil
	}
	policy, err := h.repo.GetProjectStoragePolicy(ctx, projectID)
	if err != nil {
		return false, err
	}
	if policy.RetentionMaxDays != nil && *retentionDays > *policy.RetentionMaxDays {
		return false, nil
	}
	return true, nil
}

// resolveCreateRetention gives the retention_days a new bucket gets. It
// returns false when a caller-supplied value is above the project's
// ceiling, and the caller then answers 403.
//
// The project storage policy holds two separate fields, and CreateBucket
// read only one of them. RetentionMaxDays is the ceiling on a requested
// value. RetentionDefaultDays is the value a bucket gets when the caller
// omits retention_days — migrations/shared/0057_artifact_storage.sql:42-44
// states this. The handler ignored the default, so a POST without
// retention_days wrote retention_days = NULL and expires_at = NULL. The
// retention sweeper only deletes an object whose expires_at is set and
// past (ListExpiredArtifactObjects), so an operator's mandated retention
// applied to nothing the API created.
//
// A POST body cannot tell an omitted retention_days from an explicit null,
// because the field decodes into a *int32. Both take the default. That
// matches the documented meaning of retention_default_days.
//
// The default is clamped to the ceiling. A default above the ceiling is an
// operator configuration error, not a caller error, so it clamps instead
// of rejecting the request.
func (h *Handler) resolveCreateRetention(ctx context.Context, projectID int64, requested *int32) (*int32, bool, error) {
	policy, err := h.repo.GetProjectStoragePolicy(ctx, projectID)
	if err != nil {
		return nil, false, err
	}
	if requested != nil {
		if policy.RetentionMaxDays != nil && *requested > *policy.RetentionMaxDays {
			return nil, false, nil
		}
		return requested, true, nil
	}
	if policy.RetentionDefaultDays == nil {
		return nil, true, nil
	}
	days := *policy.RetentionDefaultDays
	if policy.RetentionMaxDays != nil && days > *policy.RetentionMaxDays {
		days = *policy.RetentionMaxDays
	}
	return &days, true, nil
}

func (h *Handler) CreateBucket(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}

	var req struct {
		Name          string `json:"name"`
		RetentionDays *int32 `json:"retention_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid request body")
		return
	}
	if !bucketNamePattern.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "bucket name must match ^[a-z][a-z0-9-]{1,62}$")
		return
	}

	retentionDays, allowed, err := h.resolveCreateRetention(r.Context(), projectID, req.RetentionDays)
	if err != nil {
		h.writeInternal(w, r, "get project storage policy", err)
		return
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "QuotaExceeded", "retention_days exceeds the project's policy ceiling")
		return
	}

	row, err := h.repo.CreateBucket(r.Context(), repos.NewBucketInput{
		ProjectID:     projectID,
		Name:          req.Name,
		DisplayName:   req.Name,
		BucketType:    "local",
		RetentionDays: retentionDays,
		ExpiresAt:     computeExpiresAt(retentionDays),
	})
	if errors.Is(err, storage.ErrAlreadyExists) {
		writeError(w, http.StatusConflict, "AlreadyExists", "a bucket with this name already exists in the project")
		return
	}
	if err != nil {
		h.writeInternal(w, r, "create bucket", err)
		return
	}

	b, err := h.enrichBucket(r.Context(), row)
	if err != nil {
		h.writeInternal(w, r, "create bucket", err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (h *Handler) UpdateBucket(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	name := chi.URLParam(r, "bucket")

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid request body")
		return
	}

	// accessWrite: legacy gates its PUT (retention) and PATCH (pin) on the
	// write half of the access list too (api/v2/buckets.py:211, :318).
	row, ok := h.requireBucket(w, r, projectID, name, accessWrite)
	if !ok {
		return
	}
	var err error

	if rawPinned, present := raw["is_pinned"]; present {
		var pinned bool
		if err := json.Unmarshal(rawPinned, &pinned); err != nil {
			writeError(w, http.StatusBadRequest, "InvalidArgument", "is_pinned must be a boolean")
			return
		}
		row, err = h.repo.SetBucketPinned(r.Context(), row.ID, pinned)
		if err != nil {
			h.writeInternal(w, r, "update bucket", err)
			return
		}
	}

	if rawTags, present := raw["tags"]; present {
		row, err = h.repo.UpdateBucketTags(r.Context(), row.ID, rawTags)
		if err != nil {
			h.writeInternal(w, r, "update bucket", err)
			return
		}
	}

	if rawRetention, present := raw["retention_days"]; present {
		var retentionDays *int32
		if !bytes.Equal(bytes.TrimSpace(rawRetention), []byte("null")) {
			var v int32
			if err := json.Unmarshal(rawRetention, &v); err != nil {
				writeError(w, http.StatusBadRequest, "InvalidArgument", "retention_days must be an integer")
				return
			}
			retentionDays = &v
		}

		allowed, err := h.checkRetentionLimit(r.Context(), projectID, retentionDays)
		if err != nil {
			h.writeInternal(w, r, "get project storage policy", err)
			return
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "QuotaExceeded", "retention_days exceeds the project's policy ceiling")
			return
		}

		row, err = h.repo.UpdateBucketRetention(r.Context(), row.ID, retentionDays, computeExpiresAt(retentionDays))
		if err != nil {
			h.writeInternal(w, r, "update bucket", err)
			return
		}
	}

	b, err := h.enrichBucket(r.Context(), row)
	if err != nil {
		h.writeInternal(w, r, "update bucket", err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// deleteAllObjects removes every physical object under bucketRef via
// ObjectStore.DeleteBatch, paginating List until exhausted. It also
// removes the metadata row of each object that DeleteBatch reports as
// deleted.
//
// The metadata cleanup was missing. On a partial DeleteBatch failure this
// function returned at once and discarded result.Deleted. The rows of the
// objects whose bytes were already destroyed stayed in
// elitea_storage.objects. DeleteBucket then answered 500 and never
// soft-deleted the bucket, so the bucket stayed active.
//
// Every aggregate (SumBucketBytes, CountBucketObjects, SumProjectBytes)
// joins buckets on deleted_at IS NULL. The bucket therefore kept reporting
// bytes that no longer exist, and the project quota stayed inflated.
//
// A retry could not repair it. ObjectStore.List no longer returns a
// destroyed key, and the retention sweeper only touches a row whose
// expires_at is set and past.
//
// artifactbootstrap.purgeObjects and runtimecomposition's
// deleteExpiredBucketGroup are the two sibling copies of this loop. Both
// already clean the metadata first. This copy now matches them.
func (h *Handler) deleteAllObjects(ctx context.Context, bucketRef storage.ObjectRef, bucketID int64) error {
	var token string
	for {
		page, err := h.store.List(ctx, storage.ListQuery{Bucket: bucketRef, ContinuationToken: token})
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}

		if len(page.Objects) > 0 {
			refs := make([]storage.ObjectRef, 0, len(page.Objects))
			for _, obj := range page.Objects {
				ref, err := storage.NewObjectRef(bucketRef.ProjectID(), bucketRef.Bucket(), obj.Key)
				if err != nil {
					return fmt.Errorf("build object ref for %q: %w", obj.Key, err)
				}
				refs = append(refs, ref)
			}
			result, batchErr := h.store.DeleteBatch(ctx, refs)

			// Clean up the metadata of every key the backend reports as
			// deleted, before you look at batchErr or result.Failed. A
			// row whose bytes are already gone must not survive.
			if len(result.Deleted) > 0 {
				if err := h.repo.DeleteObjects(ctx, bucketID, result.Deleted); err != nil {
					return fmt.Errorf("delete object metadata: %w", err)
				}
			}
			if batchErr != nil {
				return fmt.Errorf("delete batch: %w", batchErr)
			}
			if len(result.Failed) > 0 {
				first := result.Failed[0]
				return fmt.Errorf("delete batch: %d object(s) failed (first: %s: %v)", len(result.Failed), first.Key, first.Err)
			}
		}

		if !page.IsTruncated {
			return nil
		}
		token = page.NextContinuationToken
	}
}

func (h *Handler) DeleteBucket(w http.ResponseWriter, r *http.Request) {
	projectIDStr := chi.URLParam(r, "projectID")
	projectID, ok := parseProjectID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid project id")
		return
	}
	name := chi.URLParam(r, "bucket")

	// accessWrite. Legacy's own DELETE carries NO access-list check, which
	// lets a member blocked on a bucket delete that bucket — see
	// bucket_permissions.go, difference 2.
	row, ok := h.requireBucket(w, r, projectID, name, accessWrite)
	if !ok {
		return
	}

	bucketRef, err := storage.NewBucketRef(projectIDStr, name)
	if err != nil {
		h.writeInternal(w, r, "delete bucket", err)
		return
	}

	if err := h.deleteAllObjects(r.Context(), bucketRef, row.ID); err != nil {
		h.writeInternal(w, r, "delete bucket objects", err)
		return
	}

	if err := h.repo.SoftDeleteBucket(r.Context(), row.ID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NotFound", "bucket not found")
			return
		}
		h.writeInternal(w, r, "delete bucket", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
