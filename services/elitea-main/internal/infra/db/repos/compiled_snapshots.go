package repos

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SnapshotQuota bounds all states, including pending upload and eviction. Main
// serializes reservation with one database advisory lock across its replicas.
type SnapshotQuota struct {
	GlobalEntries int64
	GlobalBytes   int64
	TenantEntries int64
	TenantBytes   int64
	PublishingTTL time.Duration
	ReadyTTL      time.Duration
}

func (q SnapshotQuota) Validate() error {
	if q.GlobalEntries < 1 || q.GlobalEntries > 100000 || q.TenantEntries < 1 || q.TenantEntries > q.GlobalEntries || q.GlobalBytes < 1 || q.GlobalBytes > 1<<40 || q.TenantBytes < 1 || q.TenantBytes > q.GlobalBytes || q.PublishingTTL < time.Second || q.PublishingTTL > 5*time.Minute || q.ReadyTTL < time.Second || q.ReadyTTL > 24*time.Hour {
		return domain.ErrSnapshotInvalid
	}
	return nil
}

const snapshotQuotaLock int64 = 0x454c495445414353

type CompiledSnapshotsRepository struct {
	pool  *pgxpool.Pool
	quota SnapshotQuota
}

func NewCompiledSnapshotsRepository(pool *pgxpool.Pool, quota SnapshotQuota) (*CompiledSnapshotsRepository, error) {
	if pool == nil || quota.Validate() != nil {
		return nil, domain.ErrSnapshotInvalid
	}
	return &CompiledSnapshotsRepository{pool, quota}, nil
}
func snapshotBytes(v string) []byte { b, _ := hex.DecodeString(v); return b }

type snapshotQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func snapshotCandidate(ctx context.Context, q snapshotQuery, scope domain.SnapshotScope, key, job string, complete bool) (domain.SnapshotCandidate, error) {
	c := domain.SnapshotCandidate{Scope: scope, Key: key, CompilationJobKey: job}
	if scope.Validate() != nil || !domain.SnapshotDigest(key) || !domain.SnapshotDigest(job) {
		return c, domain.ErrSnapshotInvalid
	}
	var descriptor, request, base, storedKey []byte
	var runtimeID, receipt *string
	var cleanup *time.Time
	var purpose, phase string
	var epoch, exportEpoch int64
	var exported, cancelled, live bool
	err := q.QueryRow(ctx, `SELECT compiled_purpose,compiled_snapshot_key,compiled_base_request_digest,compiled_descriptor_json,compiled_export_verified,compiled_export_lease_epoch,
 request_digest,runtime_id,lease_epoch,phase,result_json,runtime_cleanup_confirmed_at,cancellation_requested,lease_until>clock_timestamp()
 FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 FOR SHARE`, scope.TenantID, scope.ProjectID, snapshotBytes(job)).Scan(&purpose, &storedKey, &base, &descriptor, &exported, &exportEpoch, &request, &runtimeID, &epoch, &phase, &receipt, &cleanup, &cancelled, &live)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c, domain.ErrSnapshotUnavailable
		}
		return c, err
	}
	if purpose != "compile" || !bytes.Equal(storedKey, snapshotBytes(key)) || !exported || runtimeID == nil || exportEpoch <= 0 || epoch < exportEpoch || cancelled {
		return c, domain.ErrSnapshotUnavailable
	}
	c.Root = domain.SnapshotContentSHA256(descriptor)
	c.DescriptorJSON = append([]byte(nil), descriptor...)
	c.CompilationRequestDigest = hex.EncodeToString(request)
	c.CompilationRuntimeID = *runtimeID
	c.CompilationLeaseEpoch = uint64(exportEpoch)
	c.CurrentLeaseLive = live
	d, err := c.Descriptor()
	if err != nil || !bytes.Equal(base, snapshotBytes(d.Binding.BasePreparedRequestSHA256)) || strings.ContainsAny(c.CompilationRuntimeID, " \t\r\n\x00") {
		return c, domain.ErrSnapshotConflict
	}
	if phase == "completed" && receipt != nil && cleanup != nil && domain.SuccessfulSnapshotReceipt([]byte(*receipt), descriptor) == nil {
		c.ReceiptSHA256 = domain.SnapshotContentSHA256([]byte(*receipt))
		return c, nil
	}
	if !complete && phase == "dispatched" && receipt == nil && cleanup == nil {
		return c, nil
	}
	return c, domain.ErrSnapshotUnavailable
}
func (r *CompiledSnapshotsRepository) Candidate(ctx context.Context, scope domain.SnapshotScope, key, job string, complete bool) (domain.SnapshotCandidate, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.SnapshotCandidate{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := snapshotCandidate(ctx, tx, scope, key, job, complete)
	if err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}

const snapshotColumns = `descriptor_root,descriptor_json,compilation_job_key,compilation_request_digest,compilation_runtime_id,compilation_lease_epoch,compilation_receipt_sha256,state,expires_at>clock_timestamp()`

func snapshotRow(ctx context.Context, q snapshotQuery, scope domain.SnapshotScope, key, lock string) (domain.SnapshotCandidate, string, bool, error) {
	c := domain.SnapshotCandidate{Scope: scope, Key: key}
	if scope.Validate() != nil || !domain.SnapshotDigest(key) {
		return c, "", false, domain.ErrSnapshotInvalid
	}
	var root, job, request, receipt []byte
	var epoch int64
	var state string
	var live bool
	err := q.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM elitea_runtime.rust_compiled_snapshots WHERE tenant_id=$1 AND project_id=$2 AND snapshot_key=$3 `+lock, scope.TenantID, scope.ProjectID, snapshotBytes(key)).Scan(&root, &c.DescriptorJSON, &job, &request, &c.CompilationRuntimeID, &epoch, &receipt, &state, &live)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c, "", false, domain.ErrSnapshotMiss
		}
		return c, "", false, err
	}
	c.Root = hex.EncodeToString(root)
	c.CompilationJobKey = hex.EncodeToString(job)
	c.CompilationRequestDigest = hex.EncodeToString(request)
	c.CompilationLeaseEpoch = uint64(epoch)
	c.ReceiptSHA256 = hex.EncodeToString(receipt)
	if _, err := c.Descriptor(); err != nil || epoch <= 0 {
		return c, state, live, domain.ErrSnapshotConflict
	}
	return c, state, live, nil
}
func originalSnapshotExecution(ctx context.Context, q snapshotQuery, scope domain.SnapshotScope, key, job, root string) (domain.SnapshotExecution, bool, error) {
	result := domain.SnapshotExecution{Root: root, Key: key}
	if scope.Validate() != nil || !domain.SnapshotDigest(key) || !domain.SnapshotDigest(job) || !domain.SnapshotDigest(root) {
		return result, false, domain.ErrSnapshotInvalid
	}
	var purpose, runtimeID *string
	var storedKey, base, descriptor, request []byte
	var phase string
	err := q.QueryRow(ctx, `SELECT compiled_purpose,compiled_snapshot_key,compiled_base_request_digest,compiled_descriptor_json,request_digest,runtime_id,phase FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 FOR SHARE`, scope.TenantID, scope.ProjectID, snapshotBytes(job)).Scan(&purpose, &storedKey, &base, &descriptor, &request, &runtimeID, &phase)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, true, err
	}
	if purpose == nil || *purpose != "execute" || !bytes.Equal(storedKey, snapshotBytes(key)) {
		return result, true, domain.ErrSnapshotConflict
	}
	d, err := domain.ParseRustSnapshotDescriptor(descriptor, root)
	if err != nil || d.SnapshotKeySHA256 != key || d.Binding.TenantID != scope.TenantID || d.Binding.ProjectID != scope.ProjectID || !bytes.Equal(base, snapshotBytes(d.Binding.BasePreparedRequestSHA256)) {
		return result, true, domain.ErrSnapshotConflict
	}
	digest, err := domain.SnapshotJobDigest("execute", d.Binding, root)
	if err != nil || !bytes.Equal(request, snapshotBytes(digest)) {
		return result, true, domain.ErrSnapshotConflict
	}
	if phase == "reserved" {
		return result, false, nil
	}
	if runtimeID == nil || *runtimeID == "" || len(*runtimeID) > 512 || (phase != "dispatched" && phase != "completed" && phase != "failed" && phase != "cancelled" && phase != "uncertain") {
		return result, true, domain.ErrSnapshotUnavailable
	}
	result.DescriptorJSON = append([]byte(nil), descriptor...)
	result.RuntimeID = *runtimeID
	return result, true, nil
}
func (r *CompiledSnapshotsRepository) OriginalExecution(ctx context.Context, scope domain.SnapshotScope, key, job, root string) (domain.SnapshotExecution, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.SnapshotExecution{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, found, err := originalSnapshotExecution(ctx, tx, scope, key, job, root)
	if err != nil {
		return result, found, err
	}
	return result, found, tx.Commit(ctx)
}

func (r *CompiledSnapshotsRepository) Ready(ctx context.Context, scope domain.SnapshotScope, key string) (domain.SnapshotCandidate, error) {
	c, state, live, err := snapshotRow(ctx, r.pool, scope, key, "")
	if err != nil {
		return c, err
	}
	if state != "ready" || !live || !domain.SnapshotDigest(c.ReceiptSHA256) {
		return c, domain.ErrSnapshotUnavailable
	}
	return c, nil
}
func (r *CompiledSnapshotsRepository) Reserve(ctx context.Context, c domain.SnapshotCandidate) error {
	d, err := c.Descriptor()
	if err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, snapshotQuotaLock); err != nil {
		return err
	}
	authoritative, err := snapshotCandidate(ctx, tx, c.Scope, c.Key, c.CompilationJobKey, false)
	if err != nil {
		return err
	}
	if authoritative.ReceiptSHA256 == "" && !authoritative.CurrentLeaseLive {
		return domain.ErrSnapshotUnavailable
	}
	if !c.SameArtifact(authoritative) || c.CompilationLeaseEpoch != authoritative.CompilationLeaseEpoch {
		return domain.ErrSnapshotConflict
	}
	existing, state, live, err := snapshotRow(ctx, tx, c.Scope, c.Key, "FOR UPDATE")
	if err == nil {
		if !live || state == "evicting" || !existing.SameArtifact(c) {
			return domain.ErrSnapshotConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, domain.ErrSnapshotMiss) {
		return err
	}
	var count, total, tenantCount, tenantBytes int64
	if err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(content_bytes),0),count(*) FILTER(WHERE tenant_id=$1),COALESCE(sum(content_bytes) FILTER(WHERE tenant_id=$1),0) FROM elitea_runtime.rust_compiled_snapshots`, c.Scope.TenantID).Scan(&count, &total, &tenantCount, &tenantBytes); err != nil {
		return err
	}
	size := d.ExecutableBytes + int64(len(c.DescriptorJSON))
	q := r.quota
	if count >= q.GlobalEntries || tenantCount >= q.TenantEntries || total > q.GlobalBytes-size || tenantBytes > q.TenantBytes-size {
		return domain.ErrSnapshotQuota
	}
	_, err = tx.Exec(ctx, `INSERT INTO elitea_runtime.rust_compiled_snapshots(tenant_id,project_id,snapshot_key,descriptor_root,descriptor_json,content_bytes,compilation_job_key,compilation_request_digest,compilation_runtime_id,compilation_lease_epoch,state,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'publishing',clock_timestamp()+$11*interval '1 millisecond')`, c.Scope.TenantID, c.Scope.ProjectID, snapshotBytes(c.Key), snapshotBytes(c.Root), c.DescriptorJSON, size, snapshotBytes(c.CompilationJobKey), snapshotBytes(c.CompilationRequestDigest), c.CompilationRuntimeID, int64(c.CompilationLeaseEpoch), q.PublishingTTL.Milliseconds())
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (r *CompiledSnapshotsRepository) WithPublishing(ctx context.Context, c domain.SnapshotCandidate, fn func() error) error {
	if fn == nil {
		return domain.ErrSnapshotInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, state, live, err := snapshotRow(ctx, tx, c.Scope, c.Key, "FOR SHARE")
	if err != nil {
		return err
	}
	if !live || state != "publishing" || !row.SameArtifact(c) {
		return domain.ErrSnapshotUnavailable
	}
	current, err := snapshotCandidate(ctx, tx, c.Scope, c.Key, c.CompilationJobKey, false)
	if err != nil {
		return err
	}
	if !current.CurrentLeaseLive && current.ReceiptSHA256 == "" {
		return domain.ErrSnapshotUnavailable
	}
	if !current.SameArtifact(c) || current.CompilationLeaseEpoch != c.CompilationLeaseEpoch {
		return domain.ErrSnapshotConflict
	}
	if err = fn(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (r *CompiledSnapshotsRepository) WithReady(ctx context.Context, scope domain.SnapshotScope, key, root string, fn func(domain.SnapshotCandidate) error) error {
	if fn == nil || !domain.SnapshotDigest(root) {
		return domain.ErrSnapshotInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, state, live, err := snapshotRow(ctx, tx, scope, key, "FOR SHARE")
	if err != nil {
		return err
	}
	if state != "ready" || !live || c.Root != root || !domain.SnapshotDigest(c.ReceiptSHA256) {
		return domain.ErrSnapshotUnavailable
	}
	if err = fn(c); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (r *CompiledSnapshotsRepository) CommitReady(ctx context.Context, c domain.SnapshotCandidate, fn func() error) error {
	if fn == nil {
		return domain.ErrSnapshotInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, state, live, err := snapshotRow(ctx, tx, c.Scope, c.Key, "FOR UPDATE")
	if err != nil {
		return err
	}
	if !live || !row.SameArtifact(c) || state == "evicting" {
		return domain.ErrSnapshotConflict
	}
	proof, err := snapshotCandidate(ctx, tx, c.Scope, c.Key, c.CompilationJobKey, true)
	if err != nil {
		return err
	}
	if !proof.SameArtifact(c) || proof.CompilationLeaseEpoch != c.CompilationLeaseEpoch || proof.ReceiptSHA256 != c.ReceiptSHA256 || !domain.SnapshotDigest(c.ReceiptSHA256) {
		return domain.ErrSnapshotConflict
	}
	if state == "ready" {
		if row.ReceiptSHA256 != proof.ReceiptSHA256 {
			return domain.ErrSnapshotConflict
		}
		return tx.Commit(ctx)
	}
	if err = fn(); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE elitea_runtime.rust_compiled_snapshots SET state='ready',compilation_receipt_sha256=$4,compilation_lease_epoch=$5,expires_at=clock_timestamp()+$6*interval '1 millisecond' WHERE tenant_id=$1 AND project_id=$2 AND snapshot_key=$3`, c.Scope.TenantID, c.Scope.ProjectID, snapshotBytes(c.Key), snapshotBytes(c.ReceiptSHA256), int64(c.CompilationLeaseEpoch), r.quota.ReadyTTL.Milliseconds())
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (r *CompiledSnapshotsRepository) ClaimExpired(ctx context.Context, owner string, limit int, lease time.Duration) ([]domain.SnapshotEviction, error) {
	if owner == "" || len(owner) > 128 || strings.ContainsAny(owner, "\r\n\x00") || limit < 1 || limit > 100 || lease < time.Second || lease > time.Minute {
		return nil, domain.ErrSnapshotInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `WITH candidates AS (SELECT tenant_id,project_id,snapshot_key FROM elitea_runtime.rust_compiled_snapshots WHERE expires_at<=clock_timestamp() AND (eviction_until IS NULL OR eviction_until<=clock_timestamp()) ORDER BY expires_at,tenant_id,project_id,snapshot_key FOR UPDATE SKIP LOCKED LIMIT $1)
 UPDATE elitea_runtime.rust_compiled_snapshots s SET state='evicting',eviction_owner=$2,eviction_until=clock_timestamp()+$3*interval '1 millisecond',eviction_epoch=s.eviction_epoch+1 FROM candidates c WHERE s.tenant_id=c.tenant_id AND s.project_id=c.project_id AND s.snapshot_key=c.snapshot_key RETURNING s.tenant_id,s.project_id,s.snapshot_key,s.descriptor_root,s.descriptor_json,s.compilation_job_key,s.compilation_request_digest,s.compilation_runtime_id,s.compilation_lease_epoch,s.compilation_receipt_sha256,s.eviction_epoch`, limit, owner, lease.Milliseconds())
	if err != nil {
		return nil, err
	}
	result := make([]domain.SnapshotEviction, 0, limit)
	for rows.Next() {
		var c domain.SnapshotCandidate
		var key, root, job, request, receipt []byte
		var compileEpoch, evictionEpoch int64
		err = rows.Scan(&c.Scope.TenantID, &c.Scope.ProjectID, &key, &root, &c.DescriptorJSON, &job, &request, &c.CompilationRuntimeID, &compileEpoch, &receipt, &evictionEpoch)
		if err != nil {
			break
		}
		c.Key = hex.EncodeToString(key)
		c.Root = hex.EncodeToString(root)
		c.CompilationJobKey = hex.EncodeToString(job)
		c.CompilationRequestDigest = hex.EncodeToString(request)
		c.CompilationLeaseEpoch = uint64(compileEpoch)
		c.ReceiptSHA256 = hex.EncodeToString(receipt)
		if _, err = c.Descriptor(); err != nil {
			break
		}
		result = append(result, domain.SnapshotEviction{Candidate: c, Owner: owner, Epoch: uint64(evictionEpoch)})
	}
	err = errors.Join(err, rows.Err())
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}
func (r *CompiledSnapshotsRepository) CompleteEviction(ctx context.Context, row domain.SnapshotEviction, remove func() error) error {
	if row.Epoch == 0 || remove == nil {
		return domain.ErrSnapshotInvalid
	}
	c := row.Candidate
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var root []byte
	var owner string
	var epoch int64
	var live bool
	err = tx.QueryRow(ctx, `SELECT descriptor_root,eviction_owner,eviction_epoch,eviction_until>clock_timestamp() FROM elitea_runtime.rust_compiled_snapshots WHERE tenant_id=$1 AND project_id=$2 AND snapshot_key=$3 AND state='evicting' FOR UPDATE`, c.Scope.TenantID, c.Scope.ProjectID, snapshotBytes(c.Key)).Scan(&root, &owner, &epoch, &live)
	if err != nil || !live || owner != row.Owner || uint64(epoch) != row.Epoch || !bytes.Equal(root, snapshotBytes(c.Root)) {
		return domain.ErrSnapshotConflict
	}
	// Hold the row fence through deletion. A stale delayed deleter cannot erase
	// an object after a later reservation for the same immutable key is ready.
	if err = remove(); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM elitea_runtime.rust_compiled_snapshots WHERE tenant_id=$1 AND project_id=$2 AND snapshot_key=$3`, c.Scope.TenantID, c.Scope.ProjectID, snapshotBytes(c.Key))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var _ domain.RustSnapshotIndex = (*CompiledSnapshotsRepository)(nil)
var _ domain.RustSnapshotEvictor = (*CompiledSnapshotsRepository)(nil)
