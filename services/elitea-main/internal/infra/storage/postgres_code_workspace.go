package storage

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"math"
)

// The original visit owner supplies the live-claim transaction.
// This repository never opens another transaction or redeems credentials.
type PostgresCodeWorkspaceReceipts struct{}

func NewPostgresCodeWorkspaceReceipts() *PostgresCodeWorkspaceReceipts {
	return &PostgresCodeWorkspaceReceipts{}
}

func validCodeWorkspaceKey(visit OriginalCodeVisit, key CodeWorkspaceReceiptKey) bool {
	return key.ExecutionID != "" && len(key.ExecutionID) <= 256 && key.Generation > 0 && key.Generation <= math.MaxInt64 &&
		key.ExecutionID == visit.ExecutionID && key.Generation == visit.OriginalGeneration && key.VisitID == visit.Reference.VisitID && key.VisitDigestSHA256 == visit.Reference.DigestSHA256 && key.ActivationID == visit.ActivationID &&
		workspaceHex(key.VisitID, 64) && workspaceHex(key.VisitDigestSHA256, 64) && workspaceHex(key.ActivationID, 64) && workspaceHex(key.BasePreparedSHA256, 64) && workspaceHex(key.SelectionSHA256, 64) && workspaceHex(key.PolicySHA256, 64) && visit.TenantID != "" && visit.ResourceProjectID > 0 && visit.ActorID > 0
}

func (p *PostgresCodeWorkspaceReceipts) Reserve(ctx context.Context, tx CodeTransaction, visit OriginalCodeVisit, key CodeWorkspaceReceiptKey) (string, bool, error) {
	if p == nil || tx == nil || !validCodeWorkspaceKey(visit, key) {
		return "", false, ErrContentUnauthorized
	}
	_, err := tx.Exec(ctx, `INSERT INTO elitea_runtime.code_workspace_snapshots
 (execution_id,generation,visit_id,visit_digest_sha256,activation_id,tenant_id,resource_project_id,actor_id,base_prepared_sha256,selection_sha256,policy_sha256)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
 ON CONFLICT (execution_id,generation,visit_id) DO NOTHING`, key.ExecutionID, key.Generation, key.VisitID, key.VisitDigestSHA256, key.ActivationID, visit.TenantID, visit.ResourceProjectID, visit.ActorID, key.BasePreparedSHA256, key.SelectionSHA256, key.PolicySHA256)
	if err != nil {
		return "", false, ErrCodeWorkspaceUnavailable
	}
	return p.read(ctx, tx, visit, key, true)
}

func (p *PostgresCodeWorkspaceReceipts) read(ctx context.Context, tx CodeTransaction, visit OriginalCodeVisit, key CodeWorkspaceReceiptKey, exclusive bool) (string, bool, error) {
	if p == nil || tx == nil || !validCodeWorkspaceKey(visit, key) {
		return "", false, ErrContentUnauthorized
	}
	query := `SELECT visit_digest_sha256,activation_id,tenant_id,resource_project_id,actor_id,base_prepared_sha256,selection_sha256,policy_sha256,manifest_sha256
 FROM elitea_runtime.code_workspace_snapshots WHERE execution_id=$1 AND generation=$2 AND visit_id=$3 FOR SHARE`
	if exclusive {
		query = `SELECT visit_digest_sha256,activation_id,tenant_id,resource_project_id,actor_id,base_prepared_sha256,selection_sha256,policy_sha256,manifest_sha256
 FROM elitea_runtime.code_workspace_snapshots WHERE execution_id=$1 AND generation=$2 AND visit_id=$3 FOR UPDATE`
	}
	var digest, activation, tenant, base, selection, policy string
	var project, actor int64
	var root *string
	err := tx.QueryRow(ctx, query, key.ExecutionID, key.Generation, key.VisitID).Scan(&digest, &activation, &tenant, &project, &actor, &base, &selection, &policy, &root)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrContentUnauthorized
	}
	if err != nil {
		return "", false, ErrCodeWorkspaceUnavailable
	}
	if digest != key.VisitDigestSHA256 || activation != key.ActivationID || tenant != visit.TenantID || project != visit.ResourceProjectID || actor != visit.ActorID || base != key.BasePreparedSHA256 || selection != key.SelectionSHA256 || policy != key.PolicySHA256 {
		return "", false, ErrContentUnauthorized
	}
	if root == nil {
		return "", false, nil
	}
	if !workspaceHex(*root, 64) {
		return "", false, ErrCodeWorkspaceInvalid
	}
	return *root, true, nil
}

func (p *PostgresCodeWorkspaceReceipts) Commit(ctx context.Context, tx CodeTransaction, visit OriginalCodeVisit, key CodeWorkspaceReceiptKey, root string) error {
	if !workspaceHex(root, 64) {
		return ErrCodeWorkspaceInvalid
	}
	existing, ready, err := p.read(ctx, tx, visit, key, true)
	if err != nil {
		return err
	}
	if ready {
		if existing != root {
			return ErrContentUnauthorized
		}
		return nil
	}
	var stored string
	err = tx.QueryRow(ctx, `UPDATE elitea_runtime.code_workspace_snapshots
 SET manifest_sha256=$4,ready_at=clock_timestamp()
 WHERE execution_id=$1 AND generation=$2 AND visit_id=$3 AND manifest_sha256 IS NULL RETURNING manifest_sha256`, key.ExecutionID, key.Generation, key.VisitID, root).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrContentUnauthorized
	}
	if err != nil {
		return ErrCodeWorkspaceUnavailable
	}
	if stored != root {
		return ErrCodeWorkspaceInvalid
	}
	return nil
}

func (p *PostgresCodeWorkspaceReceipts) Verify(ctx context.Context, tx CodeTransaction, visit OriginalCodeVisit, key CodeWorkspaceReceiptKey, root string) error {
	if !workspaceHex(root, 64) {
		return ErrContentUnauthorized
	}
	existing, ready, err := p.read(ctx, tx, visit, key, false)
	if err != nil {
		return err
	}
	if !ready || existing != root {
		return ErrContentUnauthorized
	}
	return nil
}
