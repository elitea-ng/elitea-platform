package repos

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type codeDebugWriterLocker interface {
	LockCurrentCodeDebugWriter(context.Context, storage.ContentClaim, storage.CodeDebugAdmission, storage.OriginalCodeVisit) (storage.CodeDebugAuthorization, func(), error)
}

// The concrete Code intent owner performs both short original-visit phases.
// Binary reads and all object-store operations run outside those callbacks.
type CodeDebugArtifactsRepository struct {
	pool      *pgxpool.Pool
	artifacts *CurrentRuntimeArtifactRepository
	content   *storage.PostgresContentRepository
	visits    storage.OriginalCodeVisitConsumer
	writers   codeDebugWriterLocker
}

func NewCodeDebugArtifactsRepository(pool *pgxpool.Pool, store storage.ObjectStore, visits storage.OriginalCodeVisitConsumer, writers *storage.PostgresCodeDebugAuthority) (*CodeDebugArtifactsRepository, error) {
	if visits == nil || writers == nil {
		return nil, errors.New("original Code debug authority is required")
	}
	artifacts, err := NewCurrentRuntimeArtifactRepository(pool, store)
	if err != nil {
		return nil, err
	}
	content, err := storage.NewPostgresContentRepository(pool)
	if err != nil {
		return nil, err
	}
	return &CodeDebugArtifactsRepository{pool: pool, artifacts: artifacts, content: content, visits: visits, writers: writers}, nil
}
func codeDebugBytes(s string) []byte { b, _ := hex.DecodeString(s); return b }

func (r *CodeDebugArtifactsRepository) withCodeDebugVisit(ctx context.Context, c storage.ContentClaim, a storage.CodeDebugAdmission, apply func(context.Context, storage.CodeTransaction, storage.CodeDebugAuthorization) error) error {
	var releaseWriter func()
	defer func() {
		if releaseWriter != nil {
			releaseWriter()
		}
	}()
	return r.visits.WithOriginalCodeVisit(ctx, c, a.OriginalVisit, "code_debug", func(ctx context.Context, tx storage.CodeTransaction, v storage.OriginalCodeVisit) error {
		if releaseWriter != nil {
			return storage.ErrContentRejected
		}
		auth, release, err := r.writers.LockCurrentCodeDebugWriter(ctx, c, a, v)
		if err != nil {
			return err
		}
		releaseWriter = release
		return apply(ctx, tx, auth)
	})
}
func (r *CodeDebugArtifactsRepository) StageCodeDebug(ctx context.Context, c storage.ContentClaim, a storage.CodeDebugAdmission) error {
	if storage.ValidateCodeDebugAdmission(a) != nil {
		return storage.ErrContentRejected
	}
	raw, err := json.Marshal(a)
	if err != nil || len(raw) > storage.MaxCodeDebugAdmissionBytes {
		return storage.ErrContentRejected
	}
	return r.withCodeDebugVisit(ctx, c, a, func(ctx context.Context, tx storage.CodeTransaction, auth storage.CodeDebugAuthorization) error {
		// Existing bucket authorization is read-only; no bucket or object is created here.
		if _, err := r.artifacts.authorizeBucket(ctx, auth.ProjectID, auth.ActorID, "code-debug", BucketAccessWrite); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,701))`, auth.TenantID+"/"+strconv.FormatInt(auth.ProjectID, 10)+"/"+c.ExecutionID+"/"+strconv.FormatUint(c.Generation, 10)); err != nil {
			return err
		}
		var stored []byte
		var state string
		err := tx.QueryRow(ctx, `SELECT admission_json,state FROM elitea_runtime.code_debug_artifacts WHERE tenant_id=$1 AND project_id=$2 AND execution_id=$3 AND original_generation=$4 AND original_visit_id=$5 AND original_visit_revision=$6 AND original_visit_digest=$7 FOR UPDATE`, auth.TenantID, auth.ProjectID, c.ExecutionID, int64(c.Generation), codeDebugBytes(a.OriginalVisit.VisitID), int64(a.OriginalVisit.Revision), codeDebugBytes(a.OriginalVisit.DigestSHA256)).Scan(&stored, &state)
		if err == nil {
			if !bytes.Equal(stored, raw) || state == "abandoned" {
				return storage.ErrContentRejected
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var count int64
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.code_debug_artifacts WHERE tenant_id=$1 AND project_id=$2 AND execution_id=$3 AND original_generation=$4`, auth.TenantID, auth.ProjectID, c.ExecutionID, int64(c.Generation)).Scan(&count); err != nil {
			return err
		}
		if count >= 1024 {
			return storage.ErrContentRejected
		}
		var random [32]byte
		if _, err = rand.Read(random[:]); err != nil {
			return err
		}
		key := hex.EncodeToString(random[:]) + ".json"
		_, err = tx.Exec(ctx, `INSERT INTO elitea_runtime.code_debug_artifacts(tenant_id,project_id,execution_id,original_generation,original_visit_id,original_visit_revision,original_visit_digest,activation_id,attempt,actor_id,admission_json,object_key,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'staging')`, auth.TenantID, auth.ProjectID, c.ExecutionID, int64(c.Generation), codeDebugBytes(a.OriginalVisit.VisitID), int64(a.OriginalVisit.Revision), codeDebugBytes(a.OriginalVisit.DigestSHA256), codeDebugBytes(a.ActivationID), int16(a.Attempt), auth.ActorID, raw, key)
		return err
	})
}
func (r *CodeDebugArtifactsRepository) loadCodeDebug(ctx context.Context, c storage.ContentClaim, visitID string) (storage.CodeDebugAdmission, error) {
	if _, err := r.content.AuthorizeAgentRuntimeContext(ctx, c); err != nil {
		return storage.CodeDebugAdmission{}, err
	}
	var raw []byte
	err := r.pool.QueryRow(ctx, `SELECT d.admission_json FROM elitea_runtime.code_debug_artifacts d JOIN elitea_runtime.execution_jobs j ON j.execution_id=d.execution_id AND j.generation=d.original_generation AND j.resource_project_id=d.project_id AND j.tenant_id=d.tenant_id WHERE d.execution_id=$1 AND d.original_generation=$2 AND d.original_visit_id=$3 AND d.state IN ('staging','committed')`, c.ExecutionID, int64(c.Generation), codeDebugBytes(visitID)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.CodeDebugAdmission{}, storage.ErrContentNotFound
	}
	if err != nil {
		return storage.CodeDebugAdmission{}, err
	}
	var a storage.CodeDebugAdmission
	if json.Unmarshal(raw, &a) != nil || storage.ValidateCodeDebugAdmission(a) != nil || a.OriginalVisit.VisitID != visitID {
		return a, storage.ErrContentRejected
	}
	return a, nil
}
func (r *CodeDebugArtifactsRepository) readCodeDebugReservation(ctx context.Context, tx storage.CodeTransaction, c storage.ContentClaim, auth storage.CodeDebugAuthorization, a storage.CodeDebugAdmission) (storage.CodeDebugUpload, error) {
	var stored []byte
	var key, state string
	var actor int64
	err := tx.QueryRow(ctx, `SELECT admission_json,object_key,state,actor_id FROM elitea_runtime.code_debug_artifacts WHERE tenant_id=$1 AND project_id=$2 AND execution_id=$3 AND original_generation=$4 AND original_visit_id=$5 AND original_visit_revision=$6 AND original_visit_digest=$7 FOR UPDATE`, auth.TenantID, auth.ProjectID, c.ExecutionID, int64(c.Generation), codeDebugBytes(a.OriginalVisit.VisitID), int64(a.OriginalVisit.Revision), codeDebugBytes(a.OriginalVisit.DigestSHA256)).Scan(&stored, &key, &state, &actor)
	raw, _ := json.Marshal(a)
	if err != nil || actor != auth.ActorID || !bytes.Equal(stored, raw) || (state != "staging" && state != "committed") || len(key) != 69 || !strings.HasSuffix(key, ".json") || !codeDebugDigest(key[:64]) {
		return storage.CodeDebugUpload{}, storage.ErrContentRejected
	}
	return storage.CodeDebugUpload{Admission: a, Authorization: auth, ObjectKey: key, State: state}, nil
}
func (r *CodeDebugArtifactsRepository) PrepareCodeDebugUpload(ctx context.Context, c storage.ContentClaim, visitID string) (storage.CodeDebugUpload, error) {
	a, err := r.loadCodeDebug(ctx, c, visitID)
	if err != nil {
		return storage.CodeDebugUpload{}, err
	}
	var out storage.CodeDebugUpload
	err = r.withCodeDebugVisit(ctx, c, a, func(ctx context.Context, tx storage.CodeTransaction, auth storage.CodeDebugAuthorization) error {
		if _, err := r.artifacts.authorizeBucket(ctx, auth.ProjectID, auth.ActorID, "code-debug", BucketAccessWrite); err != nil {
			return err
		}
		var err error
		out, err = r.readCodeDebugReservation(ctx, tx, c, auth, a)
		return err
	})
	return out, err
}

func (r *CodeDebugArtifactsRepository) CommitCodeDebugUpload(ctx context.Context, c storage.ContentClaim, upload storage.CodeDebugUpload, raw []byte) (storage.CodeDebugArtifactReference, error) {
	empty := storage.CodeDebugArtifactReference{}
	a := upload.Admission
	auth := upload.Authorization
	if storage.ValidateCodeDebugSnapshot(raw, a, auth.Language) != nil {
		return empty, storage.ErrContentRejected
	}
	// All object-store and metadata-write IO completes with no execution row locks.
	bucket, err := r.artifacts.authorizeBucket(ctx, auth.ProjectID, auth.ActorID, "code-debug", BucketAccessWrite)
	if err != nil {
		return empty, err
	}
	ref, err := storage.NewObjectRef(strconv.FormatInt(auth.ProjectID, 10), "code-debug", upload.ObjectKey)
	if err != nil {
		return empty, err
	}
	present, err := r.verifyCodeDebugObject(ctx, ref, a)
	if err != nil {
		return empty, err
	}
	if upload.State == "committed" && !present {
		return empty, storage.ErrContentRejected
	}
	if !present {
		if upload.State != "staging" {
			return empty, storage.ErrContentRejected
		}
		if _, err = r.artifacts.WriteRuntimeArtifact(ctx, auth.ProjectID, auth.ActorID, "code-debug", upload.ObjectKey, raw); err != nil {
			return empty, err
		}
		present, err = r.verifyCodeDebugObject(ctx, ref, a)
		if err != nil || !present {
			return empty, storage.ErrContentUnavailable
		}
	}
	row, found, err := r.artifacts.objectRow(ctx, bucket.ID, upload.ObjectKey)
	if err != nil {
		return empty, err
	}
	if !found && upload.State == "staging" {
		// Resume Put-before-metadata uncertainty under the original reserved object key.
		if _, err = r.artifacts.WriteRuntimeArtifact(ctx, auth.ProjectID, auth.ActorID, "code-debug", upload.ObjectKey, raw); err != nil {
			return empty, err
		}
		row, found, err = r.artifacts.objectRow(ctx, bucket.ID, upload.ObjectKey)
		if err != nil {
			return empty, err
		}
	}
	if !found || row.ByteLength != a.ByteLength || row.MediaType != "application/json" {
		return empty, storage.ErrContentUnavailable
	}
	// The final short transaction re-resolves exact purpose, live claim/writer and
	// reservation. No artifact ref is returned before that transaction commits.
	err = r.withCodeDebugVisit(ctx, c, a, func(ctx context.Context, tx storage.CodeTransaction, current storage.CodeDebugAuthorization) error {
		if current.TenantID != auth.TenantID || current.ProjectID != auth.ProjectID || current.ActorID != auth.ActorID || current.Language != auth.Language {
			return storage.ErrContentUnauthorized
		}
		reserved, err := r.readCodeDebugReservation(ctx, tx, c, current, a)
		if err != nil || reserved.ObjectKey != upload.ObjectKey {
			return storage.ErrContentRejected
		}
		if _, err = r.artifacts.authorizeBucket(ctx, current.ProjectID, current.ActorID, "code-debug", BucketAccessWrite); err != nil {
			return err
		}
		// Preserve classification, scanning and retention. Only bind the verified digest.
		tag, err := tx.Exec(ctx, `UPDATE elitea_storage.objects SET digest_alg='sha256',digest=$3,updated_at=now() WHERE bucket_id=$1 AND key=$2 AND byte_length=$4 AND media_type='application/json'`, bucket.ID, upload.ObjectKey, codeDebugBytes(a.SnapshotSHA256), a.ByteLength)
		if err != nil || tag.RowsAffected() != 1 {
			return storage.ErrContentRejected
		}
		tag, err = tx.Exec(ctx, `UPDATE elitea_runtime.code_debug_artifacts SET state='committed',committed_at=COALESCE(committed_at,clock_timestamp()) WHERE tenant_id=$1 AND project_id=$2 AND execution_id=$3 AND original_generation=$4 AND original_visit_id=$5 AND original_visit_revision=$6 AND original_visit_digest=$7 AND object_key=$8 AND state IN ('staging','committed')`, current.TenantID, current.ProjectID, c.ExecutionID, int64(c.Generation), codeDebugBytes(a.OriginalVisit.VisitID), int64(a.OriginalVisit.Revision), codeDebugBytes(a.OriginalVisit.DigestSHA256), upload.ObjectKey)
		if err != nil || tag.RowsAffected() != 1 {
			return storage.ErrContentRejected
		}
		return nil
	})
	if err != nil {
		return empty, err
	}
	return storage.CodeDebugArtifactReference{SchemaVersion: storage.CodeDebugArtifactSchema, ProjectID: auth.ProjectID, Bucket: "code-debug", Name: upload.ObjectKey, MediaType: "application/json", ByteLength: a.ByteLength, SHA256: a.SnapshotSHA256}, nil
}
func (r *CodeDebugArtifactsRepository) verifyCodeDebugObject(ctx context.Context, ref storage.ObjectRef, a storage.CodeDebugAdmission) (bool, error) {
	body, info, err := r.artifacts.store.Get(ctx, ref, nil)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(body, storage.MaxCodeDebugSnapshotBytes+1))
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		return false, storage.ErrContentUnavailable
	}
	if int64(len(raw)) != a.ByteLength || info.Size != a.ByteLength || storage.CodeDebugSHA256(raw) != a.SnapshotSHA256 {
		return false, storage.ErrContentRejected
	}
	return true, nil
}

var _ storage.CodeDebugArtifactRepository = (*CodeDebugArtifactsRepository)(nil)
