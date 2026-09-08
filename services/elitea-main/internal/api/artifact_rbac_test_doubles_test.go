package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	v2artifacts "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// testTokenValidator satisfies apimw.TokenValidator, returning a fixed
// principal for any token. It replaces the removed AUTH_DEV_MODE bypass
// (ADR-0017) as the way router tests obtain an authenticated identity: the
// bypass injected a principal from an environment variable read inside
// production middleware, so every test that used it was exercising a code
// path no deployment runs. Injecting a validator through RouterConfig
// exercises the real credential path instead — Auth() still parses the
// header, still calls validatePrincipal, and still rejects requests that
// present nothing.
//
// Pair it with testAuthHeader on each request. authenticatedTestUser is a
// deliberately ordinary member principal: unlike the old dev user it claims
// no roles, so a test that passes only because of ambient privilege fails.
type testTokenValidator struct {
	user auth.User
}

// testAuthToken is the only token testTokenValidator accepts. It rejects
// everything else rather than accepting any string, so tests that enumerate
// credential shapes keep a genuine invalid case — a blanket-accept stub makes
// "invalid" indistinguishable from a valid token and silently retires that
// coverage.
const testAuthToken = "test-token"

func (t testTokenValidator) ValidateToken(_ context.Context, token string) (auth.User, error) {
	if token != testAuthToken {
		return auth.User{}, fmt.Errorf("test validator: unexpected token %q", token)
	}
	return t.user, nil
}

var _ apimw.TokenValidator = testTokenValidator{}

func authenticatedTestUser() auth.User {
	return auth.User{ID: "1", UserID: "1", Email: "member@test.local", AuthType: "token"}
}

// testAuthHeader presents a credential that testTokenValidator accepts.
// Requests without it are unauthenticated and must 401 — several tests below
// depend on exactly that.
func testAuthHeader(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer "+testAuthToken)
	return r
}

// fakePermissionResolver grants exactly the configured permissions without
// touching a database. S11 gates every artifact route with RBAC
// unconditionally (even the still-stubbed grant routes), so router-level
// tests need a deterministic way to control that outcome — a real
// legacyrbac.PostgresResolver against an unreachable pool always denies
// (query error -> 403), which collapses the "unauthorized" and "authorized"
// cases into the same observable result.
type fakePermissionResolver struct {
	granted []string
	// forProject, when non-empty, scopes granted to exactly that projectID —
	// any other requested projectID resolves with zero permissions, proving
	// a principal authorized for one project is still denied on another
	// (S11: "a request for project 8's object with a principal scoped to
	// project 7 returns 403").
	forProject string
}

func (f fakePermissionResolver) ResolvePermissions(_ context.Context, _ auth.User, _ string, projectID string) (auth.PermissionResolution, error) {
	if f.forProject != "" && projectID != f.forProject {
		return auth.PermissionResolution{UserID: 1}, nil
	}
	return auth.PermissionResolution{UserID: 1, Permissions: f.granted}, nil
}

var _ auth.PermissionResolver = fakePermissionResolver{}

// alwaysSucceedsArtifactRepo satisfies v2artifacts.Repository, returning a
// valid response for any input. newArtifactHandler always builds real
// Postgres-backed repositories from RouterConfig.Pool with no injection
// seam of its own, so proving a genuine 2xx through the full auth/RBAC/
// handler chain (RouterConfig.ArtifactHandler, S11) needs a working fake
// Repository, not just a not-yet-implemented stub.
type alwaysSucceedsArtifactRepo struct{}

func (alwaysSucceedsArtifactRepo) ListBuckets(context.Context, int64) ([]repos.BucketRow, error) {
	return []repos.BucketRow{}, nil
}

func (alwaysSucceedsArtifactRepo) GetBucket(_ context.Context, projectID int64, name string) (repos.BucketRow, error) {
	now := time.Now()
	return repos.BucketRow{
		ID: 1, ProjectID: projectID, Name: name, DisplayName: name, BucketType: "local",
		Tags: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (alwaysSucceedsArtifactRepo) CreateBucket(_ context.Context, input repos.NewBucketInput) (repos.BucketRow, error) {
	now := time.Now()
	return repos.BucketRow{
		ID: 1, ProjectID: input.ProjectID, Name: input.Name, DisplayName: input.DisplayName,
		BucketType: input.BucketType, Tags: json.RawMessage(`{}`),
		RetentionDays: input.RetentionDays, ExpiresAt: input.ExpiresAt, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (alwaysSucceedsArtifactRepo) UpdateBucketRetention(_ context.Context, id int64, retentionDays *int32, expiresAt *time.Time) (repos.BucketRow, error) {
	now := time.Now()
	return repos.BucketRow{
		ID: id, Name: "reports", BucketType: "local", Tags: json.RawMessage(`{}`),
		RetentionDays: retentionDays, ExpiresAt: expiresAt, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (alwaysSucceedsArtifactRepo) SetBucketPinned(_ context.Context, id int64, pinned bool) (repos.BucketRow, error) {
	now := time.Now()
	return repos.BucketRow{
		ID: id, Name: "reports", BucketType: "local", IsPinned: pinned,
		Tags: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (alwaysSucceedsArtifactRepo) UpdateBucketTags(_ context.Context, id int64, tags json.RawMessage) (repos.BucketRow, error) {
	now := time.Now()
	if len(tags) == 0 {
		tags = json.RawMessage(`{}`)
	}
	return repos.BucketRow{
		ID: id, Name: "reports", BucketType: "local", Tags: tags, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (alwaysSucceedsArtifactRepo) SoftDeleteBucket(context.Context, int64) error { return nil }

func (alwaysSucceedsArtifactRepo) SumBucketBytes(context.Context, int64) (int64, error) {
	return 0, nil
}

func (alwaysSucceedsArtifactRepo) CountBucketObjects(context.Context, int64) (int64, error) {
	return 0, nil
}

func (alwaysSucceedsArtifactRepo) GetProjectStoragePolicy(_ context.Context, projectID int64) (repos.ProjectStoragePolicy, error) {
	return repos.ProjectStoragePolicy{ProjectID: projectID}, nil
}

func (alwaysSucceedsArtifactRepo) UpsertObject(_ context.Context, input repos.NewObjectInput) (repos.ObjectRow, error) {
	now := time.Now()
	return repos.ObjectRow{
		ID: 1, BucketID: input.BucketID, Key: input.Key, ByteLength: input.ByteLength,
		MediaType: input.MediaType, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (alwaysSucceedsArtifactRepo) DeleteObjects(context.Context, int64, []string) error { return nil }

func (alwaysSucceedsArtifactRepo) SumProjectBytes(context.Context, int64) (int64, error) {
	return 0, nil
}

func (alwaysSucceedsArtifactRepo) GetBucketByID(_ context.Context, id int64) (repos.BucketRow, error) {
	now := time.Now()
	return repos.BucketRow{
		ID: id, ProjectID: 1, Name: "reports", DisplayName: "reports", BucketType: "local",
		Tags: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (alwaysSucceedsArtifactRepo) CreateTransferGrant(_ context.Context, input repos.NewTransferGrantInput) (repos.TransferGrantRow, error) {
	return repos.TransferGrantRow{
		ID: input.ID, ProjectID: input.ProjectID, BucketID: input.BucketID, Key: input.Key,
		Method: input.Method, ContentType: input.ContentType, MaxBytes: input.MaxBytes,
		DigestAlg: input.DigestAlg, Digest: input.Digest, ExpiresAt: input.ExpiresAt, CreatedAt: time.Now(),
	}, nil
}

// fixedMultipartUploadID is the upload_id alwaysSucceedsArtifactRepo's fixed
// grants carry — alwaysSucceedsArtifactStore's multipart methods (S16)
// accept any UploadID unconditionally, so the exact value is only ever
// echoed back, never validated.
var fixedMultipartUploadID = "fixed-upload-id"

// GetTransferGrant returns a fixed PUT grant with no digest declared. Its
// ContentType matches alwaysSucceedsArtifactStore.Get's fixed ContentType
// (both "application/octet-stream") so CommitTransferGrant's mandatory
// media-type check passes and its digest check (skipped when the grant
// declared none) is a no-op, letting the router-level "succeeds with exact
// permission" tests exercise a genuine 200 without needing a real upload to
// have happened first. UploadID is always set (S16) so the same fixed grant
// also satisfies requireOwnedMultipartGrant for the multipart continuation
// routes — CommitTransferGrant itself never inspects UploadID, so this has
// no effect on the pre-S16 single-shot commit tests.
func (alwaysSucceedsArtifactRepo) GetTransferGrant(_ context.Context, id string, projectID int64) (repos.TransferGrantRow, error) {
	return repos.TransferGrantRow{
		ID: id, ProjectID: projectID, BucketID: 1, Key: id, Method: "PUT",
		ContentType: "application/octet-stream", MaxBytes: 1 << 20,
		UploadID:  &fixedMultipartUploadID,
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	}, nil
}

func (alwaysSucceedsArtifactRepo) MarkTransferGrantConsumed(context.Context, string) error {
	return nil
}

// GetTransferGrantByID (S16) returns the same fixed grant as
// GetTransferGrant, ignoring the projectID scoping GetTransferGrant applies
// — router-level tests exercising the multipart continuation routes rely on
// this to reach a genuine 200 through the real handler chain.
func (alwaysSucceedsArtifactRepo) GetTransferGrantByID(_ context.Context, id string) (repos.TransferGrantRow, error) {
	return repos.TransferGrantRow{
		ID: id, ProjectID: 1, BucketID: 1, Key: id, Method: "PUT",
		ContentType: "application/octet-stream", MaxBytes: 1 << 20,
		UploadID:  &fixedMultipartUploadID,
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	}, nil
}

// The per-bucket access list. This double answers "no exception anywhere",
// which is the state every RBAC test here means: those tests are about the
// ROUTE gate (RequireResolvedPermissions), and an exception seeded here would
// silently move the subject of the test to the second gate.
func (alwaysSucceedsArtifactRepo) ListBucketPermissions(context.Context, int64) ([]repos.BucketPermissionRow, error) {
	return nil, nil
}

func (alwaysSucceedsArtifactRepo) GetBucketPermission(context.Context, int64, int64, string) ([]string, bool, error) {
	return nil, false, nil
}

func (alwaysSucceedsArtifactRepo) ListUserBucketPermissions(context.Context, int64, int64) (map[string][]string, error) {
	return nil, nil
}

func (alwaysSucceedsArtifactRepo) ReplaceUserBucketPermissions(context.Context, int64, int64, map[string][]string) error {
	return nil
}

func (alwaysSucceedsArtifactRepo) DeleteUserBucketPermission(context.Context, int64, int64, string) (bool, error) {
	return true, nil
}

func (alwaysSucceedsArtifactRepo) IsProjectAdmin(context.Context, int64, int64) (bool, error) {
	return false, nil
}

var _ v2artifacts.Repository = alwaysSucceedsArtifactRepo{}

// alwaysSucceedsArtifactStore satisfies storage.ObjectStore, returning an
// empty/zero-value success for any input. Pairs with
// alwaysSucceedsArtifactRepo.
type alwaysSucceedsArtifactStore struct{}

func (alwaysSucceedsArtifactStore) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, _ storage.PutOptions) (storage.ObjectInfo, error) {
	_, _ = io.Copy(io.Discard, body)
	return storage.ObjectInfo{Key: ref.Key(), LastModified: time.Now()}, nil
}

func (alwaysSucceedsArtifactStore) Get(_ context.Context, ref storage.ObjectRef, _ *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	return io.NopCloser(strings.NewReader("")), storage.ObjectInfo{
		Key: ref.Key(), ContentType: "application/octet-stream", LastModified: time.Now(),
	}, nil
}

func (alwaysSucceedsArtifactStore) Stat(_ context.Context, ref storage.ObjectRef) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{Key: ref.Key(), LastModified: time.Now()}, nil
}

func (alwaysSucceedsArtifactStore) Delete(context.Context, storage.ObjectRef) error { return nil }

func (alwaysSucceedsArtifactStore) DeleteBatch(_ context.Context, refs []storage.ObjectRef) (storage.BatchResult, error) {
	deleted := make([]string, len(refs))
	for i, ref := range refs {
		deleted[i] = ref.Key()
	}
	return storage.BatchResult{Deleted: deleted}, nil
}

func (alwaysSucceedsArtifactStore) List(context.Context, storage.ListQuery) (storage.ListPage, error) {
	return storage.ListPage{}, nil
}

func (alwaysSucceedsArtifactStore) PresignGet(context.Context, storage.ObjectRef, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}

func (alwaysSucceedsArtifactStore) PresignPut(context.Context, storage.ObjectRef, time.Duration, storage.PutOptions) (string, error) {
	return "", storage.ErrNotSupported
}

// StartMultipart stays ErrNotSupported: Capabilities().NativeMultipart is
// false for this store, so CreateTransferGrant's S16 multipart-start gate
// never calls it — router-level tests that need a multipart-shaped grant
// construct one directly via the fixed GetTransferGrant/GetTransferGrantByID
// row instead (see fixedMultipartUploadID) and exercise PresignPart/
// CompleteMultipart/AbortMultipart below directly.
func (alwaysSucceedsArtifactStore) StartMultipart(context.Context, storage.ObjectRef, storage.PutOptions) (storage.UploadID, error) {
	return "", storage.ErrNotSupported
}

func (alwaysSucceedsArtifactStore) PresignPart(_ context.Context, ref storage.ObjectRef, _ storage.UploadID, part int32, _ time.Duration) (string, error) {
	return fmt.Sprintf("https://presigned.example.test/part/%s/%d", ref.Key(), part), nil
}

func (alwaysSucceedsArtifactStore) CompleteMultipart(_ context.Context, ref storage.ObjectRef, _ storage.UploadID, _ []storage.Part) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{Key: ref.Key(), LastModified: time.Now()}, nil
}

func (alwaysSucceedsArtifactStore) AbortMultipart(context.Context, storage.ObjectRef, storage.UploadID) error {
	return nil
}

func (alwaysSucceedsArtifactStore) Capabilities() storage.Capabilities { return storage.Capabilities{} }

var _ storage.ObjectStore = alwaysSucceedsArtifactStore{}

// testPrincipalValidator passes the authenticated principal through unchanged.
//
// It exists because validatePrincipal FAILS CLOSED: a router composed with a
// token validator and no principal validator now refuses every request, which
// is the point of that change. Before it, these harnesses authenticated
// through the open default — 43 RouterConfig literals across 23 files set
// `AuthValidator` and none set this, so every one of them was exercising a
// deployment shape that should not exist.
//
// Pass-through is the right double here and not a weakening: these suites test
// ROUTING and RBAC, and the real validator's job is to reload the principal
// from a database they do not have. What it must not do is accept a principal
// the test did not authenticate, which is why it echoes rather than invents.
type testPrincipalValidator struct{}

func (testPrincipalValidator) ValidatePrincipal(_ context.Context, user auth.User) (auth.User, error) {
	return user, nil
}
