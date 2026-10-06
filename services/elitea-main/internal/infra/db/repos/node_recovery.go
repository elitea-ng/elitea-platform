package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/noderecovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NodeRecoveryEffectProofProvider is implemented by an effect owner on this transaction.
// Browser input cannot satisfy this interface. Absence refuses effect recovery.
type NodeRecoveryEffectProofProvider interface {
	VerifyNodeRecoveryEffect(context.Context, sqlExecutor, string, uint64, domain.Receipt, string) (json.RawMessage, error)
}

type recoveryPermissionCheck func(context.Context, sqlExecutor, app.Selector, string) error

type NodeRecoveryRepository struct {
	projects    projectStore
	shared      sharedStore
	permissions recoveryPermissionCheck
	effects     NodeRecoveryEffectProofProvider
}

func NewNodeRecoveryRepository(pool *pgxpool.Pool, effects NodeRecoveryEffectProofProvider) (*NodeRecoveryRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	shared, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return &NodeRecoveryRepository{projects: projects, shared: shared, permissions: checkRecoveryPermission, effects: effects}, nil
}

func checkRecoveryPermission(ctx context.Context, tx sqlExecutor, selector app.Selector, permission string) error {
	e, ok := tx.(pgxExecutor)
	if !ok {
		return app.ErrNotAllowed
	}
	result, err := legacyrbac.NewTransactionResolver(e.queryer).ResolvePermissions(ctx,
		auth.User{UserID: strconv.FormatInt(selector.ActorUserID, 10)}, auth.PermissionModeDefault, strconv.FormatInt(selector.ProjectID, 10))
	if errors.Is(err, auth.ErrPermissionDenied) {
		return app.ErrNotAllowed
	}
	if err != nil {
		return err
	}
	if result.UserID != selector.ActorUserID {
		return app.ErrNotAllowed
	}
	for _, granted := range result.Permissions {
		if granted == permission {
			return nil
		}
	}
	return app.ErrNotAllowed
}

type recoveryPublicScope struct {
	executionID string
	generation  uint64
	desired     string
}

func lockPublicRecoveryScope(ctx context.Context, tx sqlExecutor, selector app.Selector) (recoveryPublicScope, error) {
	var scope recoveryPublicScope
	err := tx.QueryRow(ctx, `
SELECT j.execution_id,j.generation,j.desired_state
FROM chat_message_group response
JOIN chat_conversations conversation ON conversation.id=response.conversation_id
JOIN chat_message_group question ON question.id=response.reply_to_id AND question.conversation_id=conversation.id
JOIN chat_participants author ON author.id=question.author_participant_id AND author.entity_name='user'
JOIN elitea_runtime.agent_execution_jobs binding ON binding.client_message_id=response.uuid::text
 AND binding.execution_id=response.task_id AND binding.client_execution_generation=response.meta->>'execution_generation'
JOIN elitea_runtime.execution_jobs j USING(execution_id,generation)
JOIN elitea_runtime.command_outbox command USING(execution_id,generation)
JOIN centry.project project ON project.id=j.resource_project_id
WHERE response.uuid=$1::uuid AND j.tenant_id=$2::integer::text
 AND j.resource_project_id=$2 AND j.projection_project_id=$2
 AND j.actor_id=$3::bigint::text AND j.capability_id=binding.capability_id
 AND j.capability_id IN ('agent.execute.application.v1','agent.execute.adhoc.v1')
 AND j.state='RUNNING' AND j.desired_state IN ('SUSPENDED','RUNNING')
 AND project.suspended=FALSE AND command.retired_at IS NULL
 AND command.authority_granted_at IS NOT NULL AND command.deadline>clock_timestamp()
 AND (conversation.author_id=$3 OR (author.entity_meta->>'id' ~ '^[1-9][0-9]*$' AND (author.entity_meta->>'id')::bigint=$3))
 AND NOT EXISTS(SELECT 1 FROM elitea_runtime.output_inbox terminal WHERE terminal.execution_id=j.execution_id AND terminal.generation=j.generation)
FOR UPDATE OF j,command,response,conversation`, selector.ResponseMessageID, selector.ProjectID, selector.ActorUserID).
		Scan(&scope.executionID, &scope.generation, &scope.desired)
	if errors.Is(err, pgx.ErrNoRows) {
		return recoveryPublicScope{}, app.ErrNotAllowed
	}
	return scope, err
}

func (r *NodeRecoveryRepository) Read(ctx context.Context, selector app.Selector) (app.State, error) {
	if r == nil || r.projects == nil || r.permissions == nil || selector.Validate() != nil {
		return app.State{}, app.ErrInvalid
	}
	var state app.State
	err := r.projects.WithinProjectTx(ctx, selector.ProjectID, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		if err := r.permissions(ctx, tx, selector, "models.applications.task.get"); err != nil {
			return err
		}
		scope, err := lockPublicRecoveryScope(ctx, tx, selector)
		if err != nil {
			return err
		}
		var status string
		var raw, digest []byte
		err = tx.QueryRow(ctx, `SELECT status,receipt_json,receipt_digest FROM elitea_runtime.node_recovery_visits
WHERE execution_id=$1 AND generation=$2 ORDER BY created_at DESC,journal_revision DESC LIMIT 1`, scope.executionID, scope.generation).Scan(&status, &raw, &digest)
		if errors.Is(err, pgx.ErrNoRows) {
			return app.ErrNotAllowed
		}
		if err != nil {
			return err
		}
		if _, err := domain.DecodeReceipt(raw); err != nil {
			return app.ErrNotAllowed
		}
		wanted := sha256.Sum256(raw)
		if !bytes.Equal(digest, wanted[:]) {
			return app.ErrNotAllowed
		}
		publicStatus := "running"
		if status == "SUSPENDED" {
			publicStatus = "suspended"
		}
		if status == "AUTHORIZED" {
			publicStatus = "authorized"
		}
		if status == "CANCELLED" {
			return app.ErrNotAllowed
		}
		state = app.State{Schema: "elitea.pipeline.node-recovery-state.v1", ExecutionID: scope.executionID, Generation: scope.generation, Status: publicStatus, Receipt: bytes.Clone(raw)}
		return nil
	})
	return state, err
}

type recoveryAction struct {
	RequestID        string          `json:"request_id"`
	ActivationID     string          `json:"activation_id"`
	ExpectedRevision uint64          `json:"expected_revision"`
	LastAttempt      uint16          `json:"last_attempt"`
	Action           string          `json:"action"`
	ReceiptSHA256    string          `json:"receipt_sha256"`
	OwnerProof       json.RawMessage `json:"owner_proof"`
}

func recoveryOutcome(request domain.Request, replay bool) app.Outcome {
	return app.Outcome{Schema: "elitea.pipeline.node-recovery-accepted.v1", ExecutionID: request.ExecutionID, Generation: request.Generation, RequestID: request.RequestID, ActivationID: request.ActivationID, ExpectedRevision: request.ExpectedRevision, Action: request.Action, Replay: replay}
}

func (r *NodeRecoveryRepository) Submit(ctx context.Context, submission app.Submission) (app.Outcome, error) {
	if r == nil || r.projects == nil || r.permissions == nil || submission.Validate() != nil {
		return app.Outcome{}, app.ErrInvalid
	}
	request := submission.Request
	requestBytes, err := json.Marshal(request)
	if err != nil {
		return app.Outcome{}, err
	}
	var outcome app.Outcome
	err = r.projects.WithinProjectTx(ctx, submission.ProjectID, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		if err := r.permissions(ctx, tx, submission.Selector, "models.chat.messages.create"); err != nil {
			return err
		}
		scope, err := lockPublicRecoveryScope(ctx, tx, submission.Selector)
		if err != nil {
			return err
		}
		if scope.executionID != request.ExecutionID || scope.generation != request.Generation {
			return app.ErrNotAllowed
		}
		var existing []byte
		err = tx.QueryRow(ctx, `SELECT request_json FROM elitea_runtime.node_recovery_control_outbox WHERE execution_id=$1 AND generation=$2 AND request_id=$3`, scope.executionID, scope.generation, request.RequestID).Scan(&existing)
		if err == nil {
			if !bytes.Equal(existing, requestBytes) {
				return app.ErrNotAllowed
			}
			outcome = recoveryOutcome(request, true)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if scope.desired != "SUSPENDED" {
			return app.ErrNotAllowed
		}
		var raw, digest []byte
		var status string
		err = tx.QueryRow(ctx, `SELECT receipt_json,receipt_digest,status FROM elitea_runtime.node_recovery_visits
WHERE execution_id=$1 AND generation=$2 AND activation_id=$3 AND journal_revision=$4 FOR UPDATE`, scope.executionID, scope.generation, request.ActivationID, int64(request.ExpectedRevision)).Scan(&raw, &digest, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return app.ErrNotAllowed
		}
		if err != nil {
			return err
		}
		receipt, err := domain.DecodeReceipt(raw)
		if err != nil {
			return err
		}
		wanted := sha256.Sum256(raw)
		if status != "SUSPENDED" || receipt.ActivationID != request.ActivationID || receipt.JournalRevision != request.ExpectedRevision || !bytes.Equal(digest, wanted[:]) || receipt.AllowedActions[0] != request.Action {
			return app.ErrNotAllowed
		}
		var proof json.RawMessage
		if request.Action != "retry" || receipt.ReplaySafety.Kind != "no_external_effect" {
			if r.effects == nil {
				return app.ErrNotAllowed
			}
			proof, err = r.effects.VerifyNodeRecoveryEffect(ctx, tx, scope.executionID, scope.generation, receipt, request.Action)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return app.ErrNotAllowed
			}
			if len(proof) == 0 || len(proof) > 4096 || !json.Valid(proof) || bytes.Equal(bytes.TrimSpace(proof), []byte("null")) {
				return app.ErrNotAllowed
			}
			if _, err := domain.DecodeOwnerProof(proof, scope.executionID, scope.generation, receipt, request.Action); err != nil {
				return app.ErrNotAllowed
			}
		}
		actionBytes, err := json.Marshal(recoveryAction{RequestID: request.RequestID, ActivationID: request.ActivationID, ExpectedRevision: request.ExpectedRevision, LastAttempt: receipt.Attempt, Action: request.Action, ReceiptSHA256: hex.EncodeToString(wanted[:]), OwnerProof: proof})
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO elitea_runtime.node_recovery_control_outbox
(execution_id,generation,activation_id,expected_revision,request_id,request_json,action_json,actor_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, scope.executionID, scope.generation, request.ActivationID, int64(request.ExpectedRevision), request.RequestID, requestBytes, actionBytes, strconv.FormatInt(submission.ActorUserID, 10))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return app.ErrNotAllowed
		}
		tag, err = tx.Exec(ctx, `UPDATE elitea_runtime.node_recovery_visits SET status='AUTHORIZED',updated_at=clock_timestamp()
WHERE execution_id=$1 AND generation=$2 AND activation_id=$3 AND journal_revision=$4 AND status='SUSPENDED'`, scope.executionID, scope.generation, request.ActivationID, int64(request.ExpectedRevision))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return app.ErrNotAllowed
		}
		_, err = tx.Exec(ctx, `INSERT INTO elitea_runtime.node_recovery_audit
(execution_id,generation,request_id,transition,actor_id,activation_id,journal_revision)
VALUES($1,$2,$3,'AUTHORIZED',$4,$5,$6)`, scope.executionID, scope.generation, request.RequestID, strconv.FormatInt(submission.ActorUserID, 10), request.ActivationID, int64(request.ExpectedRevision))
		if err != nil {
			return err
		}
		outcome = recoveryOutcome(request, false)
		return nil
	})
	if err != nil {
		return app.Outcome{}, fmt.Errorf("node recovery authorization: %w", err)
	}
	return outcome, nil
}

var _ app.Store = (*NodeRecoveryRepository)(nil)
