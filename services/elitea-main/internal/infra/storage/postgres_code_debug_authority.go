package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This adapter reads graph writer identity only. Main never reads checkpoints
// or evaluates graph source/state. Saved declarations come from the Code owner.
type PostgresCodeDebugAuthority struct{ agentstate *pgxpool.Pool }

func NewPostgresCodeDebugAuthority(agentstate *pgxpool.Pool) (*PostgresCodeDebugAuthority, error) {
	if agentstate == nil {
		return nil, errors.New("code debug writer database is required")
	}
	return &PostgresCodeDebugAuthority{agentstate: agentstate}, nil
}
func debugBytes(s string) []byte { b, _ := hex.DecodeString(s); return b }

func ValidateCodeDebugOriginalVisit(claim ContentClaim, a CodeDebugAdmission, v OriginalCodeVisit) error {
	// This is the sole Main producer's complete pre-redemption frame identity,
	// not the separately admitted Rust graph definition digest used below.
	if v.OwningSourceDefinition.Validate() != nil || v.OwningSourceDefinition.YAMLSHA256 != a.YAMLSHA256 {
		return ErrContentUnauthorized
	}
	if ValidateCodeDebugAdmission(a) != nil || v.Reference != a.OriginalVisit || v.Attempt != a.Attempt || v.ExecutionID != claim.ExecutionID || v.OriginalGeneration != claim.Generation || v.ActivationID != a.ActivationID || v.NodeID != a.NodeID || v.GraphThread != a.GraphThreadID || strconv.FormatUint(v.Step, 10) != a.GraphStep || v.OwningYAMLSHA256 != a.YAMLSHA256 || v.PreWorkspacePreparedSHA256 != a.RequestSHA256 || v.SourceSHA256 != a.SourceSHA256 || v.InputSHA256 != a.InputSHA256 || v.NodeDigest != hex.EncodeToString(v.Declaration.ConfigurationDigest[:]) || v.Language != v.Declaration.Language || !v.Declaration.Debug || v.TenantID == "" || v.ResourceProjectID < 1 || v.ResourceProjectID > math.MaxInt32 || v.ActorID < 1 || v.ActorID > math.MaxInt32 || v.CurrentClaimAttempt < 1 || v.CurrentClaimAttempt > math.MaxInt64 || v.LeaseEpoch < 1 || v.LeaseEpoch > math.MaxInt64 {
		return ErrContentUnauthorized
	}
	h := sha256CodeConfiguration(a.ConfigurationJSON)
	if hex.EncodeToString(h[:]) != v.NodeDigest {
		return ErrContentUnauthorized
	}
	return nil
}
func sha256CodeConfiguration(configuration string) [32]byte {
	return sha256.Sum256(append([]byte("elitea.graph.code.config.v1\x00"), []byte(configuration)...))
}

// Acquire only inside a short Main original-visit metadata transaction and
// release immediately after that transaction completes. No binary body or
// object-store IO may occur until the returned release function has been called.
func (r *PostgresCodeDebugAuthority) LockCurrentCodeDebugWriter(ctx context.Context, claim ContentClaim, a CodeDebugAdmission, v OriginalCodeVisit) (CodeDebugAuthorization, func(), error) {
	empty := CodeDebugAuthorization{}
	if r == nil || r.agentstate == nil || ValidateCodeDebugOriginalVisit(claim, a, v) != nil {
		return empty, nil, ErrContentUnauthorized
	}
	tx, err := r.agentstate.Begin(ctx)
	if err != nil {
		return empty, nil, ErrContentUnavailable
	}
	release := func() { _ = tx.Rollback(ctx) }
	var writer string
	err = tx.QueryRow(ctx, `SELECT writer_claim_id FROM elitea_runtime.agent_graph_checkpoint_writers
 WHERE tenant_id=$1 AND resource_project_id=$2 AND definition_digest=$3 AND thread_id=$4 AND writer_execution_id=$5 AND writer_generation=$6 AND writer_claim_id=$7
 AND checkpoint_family='adk-graph.2.0.0.v1' AND projection_project_id=$8 AND capability_id IN ('agent.execute.application.v1','agent.execute.adhoc.v1') AND writer_claim_attempt=$9 AND writer_lease_epoch=$10 FOR SHARE`, v.TenantID, v.ResourceProjectID, debugBytes(a.DefinitionSHA256), a.GraphThreadID, claim.ExecutionID, int64(claim.Generation), claim.ClaimID, v.ProjectionProjectID, int64(v.CurrentClaimAttempt), int64(v.LeaseEpoch)).Scan(&writer)
	if errors.Is(err, pgx.ErrNoRows) {
		release()
		return empty, nil, ErrContentUnauthorized
	}
	if err != nil {
		release()
		return empty, nil, ErrContentUnavailable
	}
	if writer != claim.ClaimID {
		release()
		return empty, nil, ErrContentUnauthorized
	}
	return CodeDebugAuthorization{TenantID: v.TenantID, ProjectID: v.ResourceProjectID, ActorID: v.ActorID, Language: v.Language, ClaimAttempt: v.CurrentClaimAttempt, LeaseEpoch: v.LeaseEpoch}, release, nil
}
