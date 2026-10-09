package repos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	recoveryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/noderecovery"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
)

// ExecutionInterruptRepository is the per-interrupt HITL decision ledger
// (libs/proto/contracts/fanout-interrupt-decisions-v1.md §5-§7).
//
// Lock order, the same in every write: the chat_message_group response row
// (Raise, Decide; the caller's stop/regenerate transaction for Cancel and
// Supersede), then the execution_interrupt_responses row, then execution job
// and claim rows (Ack, Fetch; Wave 2 continuation admission), then card rows.
// Every write runs READ COMMITTED behind these row locks, so a waiting
// transaction re-reads the committed winner instead of failing with a
// serialization error: 50 tabs deciding one card produce one DECIDED row and
// 49 replays or 409s, never a 500. Every authority predicate (ownership,
// membership, RBAC, the private claim) is evaluated in a new statement after
// the lock it depends on is held, because a READ COMMITTED statement that
// waited for a lock does not re-read the tables it joined. So a revocation that
// commits while a request waits is seen.
type ExecutionInterruptRepository struct {
	projects    projectStore
	shared      sharedStore
	permissions interruptPermissionCheck
}

type interruptPermissionCheck func(ctx context.Context, tx sqlExecutor, projectID, actorID int64, permission string) error

func NewExecutionInterruptRepository(pool *pgxpool.Pool) (*ExecutionInterruptRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	shared, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return &ExecutionInterruptRepository{projects: projects, shared: shared, permissions: checkInterruptPermission}, nil
}

// checkInterruptPermission resolves the actor's project permissions on the
// caller's transaction (active user, active project, project role grants).
func checkInterruptPermission(ctx context.Context, tx sqlExecutor, projectID, actorID int64, permission string) error {
	err := checkRecoveryPermission(ctx, tx, recoveryapp.Selector{ProjectID: projectID, ActorUserID: actorID}, permission)
	if errors.Is(err, recoveryapp.ErrNotAllowed) {
		return domain.ErrNotAllowed
	}
	return err
}

var interruptWriteTx = pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}

// lockOwnedResponse checks that the actor may decide the response's cards:
// the conversation's author or the question's author, and still a user
// participant of the conversation (the ResolveCurrentContinuation rule). A
// missing response and a foreign one are indistinguishable (ErrNotAllowed).
//
// With lock set, the first statement locks the row only when the predicate
// holds, so a caller who may not decide never takes the lock. A READ COMMITTED
// statement that waited for the lock does not re-read the conversation and
// participant rows it joined, so the predicate is evaluated again in a new
// statement while the lock is held.
func lockOwnedResponse(ctx context.Context, tx sqlExecutor, selector domain.Selector, lock bool) (int64, error) {
	query := ownedResponseSQL
	if lock {
		query += "\nFOR UPDATE OF response"
	}
	conversationID, err := ownedResponse(ctx, tx, query, selector)
	if err != nil || !lock {
		return conversationID, err
	}
	return ownedResponse(ctx, tx, ownedResponseSQL, selector)
}

func ownedResponse(ctx context.Context, tx sqlExecutor, query string, selector domain.Selector) (int64, error) {
	var conversationID int64
	err := tx.QueryRow(ctx, query, selector.ResponseMessageID, selector.ActorUserID).Scan(&conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, domain.ErrNotAllowed
	}
	return conversationID, err
}

const ownedResponseSQL = `
SELECT conversation.id
FROM chat_message_group response
JOIN chat_conversations conversation ON conversation.id = response.conversation_id
JOIN chat_message_group question ON question.id = response.reply_to_id AND question.conversation_id = conversation.id
JOIN chat_participants question_author ON question_author.id = question.author_participant_id
 AND question_author.entity_name = 'user'
WHERE response.uuid = $1::uuid
 AND (conversation.author_id = $2
      OR (question_author.entity_meta->>'id' ~ '^[1-9][0-9]{0,17}$' AND (question_author.entity_meta->>'id')::bigint = $2))
 AND EXISTS (
   SELECT 1 FROM chat_participant_mapping mapping
   JOIN chat_participants participant ON participant.id = mapping.participant_id AND participant.entity_name = 'user'
   WHERE mapping.conversation_id = conversation.id
    AND participant.entity_meta->>'id' ~ '^[1-9][0-9]{0,17}$' AND (participant.entity_meta->>'id')::bigint = $2)`

// Raise records one card idempotently (contract §5). A byte-identical replay
// of the card and frontier is a no-op; different bytes under the same key, a
// reused source event id or an open interrupt id clash are ErrRaiseConflict;
// a seventeenth open card is ErrCapReached. Nothing is written on refusal.
func (r *ExecutionInterruptRepository) Raise(ctx context.Context, input domain.RaiseInput) (domain.RaiseResult, error) {
	if r == nil || r.projects == nil {
		return domain.RaiseResult{}, domain.ErrInvalidRaise
	}
	var result domain.RaiseResult
	err := r.projects.WithinProjectTx(ctx, input.ProjectID, interruptWriteTx, func(tx sqlExecutor) error {
		var err error
		result, err = raiseExecutionInterrupt(ctx, tx, input)
		return err
	})
	if err != nil {
		return domain.RaiseResult{}, fmt.Errorf("execution interrupt raise: %w", err)
	}
	return result, nil
}

// raiseExecutionInterrupt runs on the caller's project transaction (Wave 2:
// the transaction that accepts the agent_interrupt_pending frame).
func raiseExecutionInterrupt(ctx context.Context, tx sqlExecutor, input domain.RaiseInput) (domain.RaiseResult, error) {
	if input.ProjectID <= 0 || !domain.ValidResponseMessageID(input.RootResponseID) || !domain.ValidExecutionID(input.ExecutionID) ||
		input.Generation <= 0 || input.SourceEventID == "" || len(input.SourceEventID) > domain.MaxSourceEventIDBytes ||
		input.SourceClaimID == "" || len(input.SourceClaimID) > domain.MaxClaimIDBytes {
		return domain.RaiseResult{}, domain.ErrInvalidRaise
	}
	card, cardJSON, err := domain.ParseCard(input.Card)
	if err != nil {
		return domain.RaiseResult{}, err
	}
	frontier, err := input.Frontier.CanonicalJSON()
	if err != nil {
		return domain.RaiseResult{}, err
	}
	var conversationID int64
	err = tx.QueryRow(ctx, `SELECT conversation_id FROM chat_message_group WHERE uuid = $1::uuid FOR UPDATE`, input.RootResponseID).Scan(&conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RaiseResult{}, domain.ErrInvalidRaise
	}
	if err != nil {
		return domain.RaiseResult{}, err
	}
	// The raising execution must be an agent execution of this project bound
	// to this root response; a frame cannot raise into another response.
	var bound bool
	err = tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM elitea_runtime.agent_execution_jobs binding
  JOIN elitea_runtime.execution_jobs j USING (execution_id, generation)
  WHERE binding.execution_id = $1 AND binding.generation = $2 AND binding.client_message_id = $3
   AND j.resource_project_id = $4 AND j.projection_project_id = $4 AND j.tenant_id = $4::text
   AND j.capability_id IN ('agent.execute.application.v1', 'agent.execute.adhoc.v1') AND binding.capability_id = j.capability_id)`,
		input.ExecutionID, input.Generation, input.RootResponseID, input.ProjectID).Scan(&bound)
	if err != nil {
		return domain.RaiseResult{}, err
	}
	if !bound {
		return domain.RaiseResult{}, domain.ErrInvalidRaise
	}
	var storedProject, storedConversation int64
	err = tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.execution_interrupt_responses (root_response_id, project_id, conversation_id)
VALUES ($1::uuid, $2, $3)
ON CONFLICT (root_response_id) DO UPDATE SET updated_at = clock_timestamp()
RETURNING project_id, conversation_id`, input.RootResponseID, input.ProjectID, conversationID).Scan(&storedProject, &storedConversation)
	if err != nil {
		return domain.RaiseResult{}, err
	}
	if storedProject != input.ProjectID || storedConversation != conversationID {
		return domain.RaiseResult{}, domain.ErrLedgerFault
	}
	var open int
	var existingCard []byte
	var sameFrontier *bool
	err = tx.QueryRow(ctx, `
SELECT (SELECT count(*) FROM elitea_runtime.execution_interrupts
        WHERE root_response_id = $1::uuid AND state IN ('PENDING', 'DECIDED')),
       existing.card_json, existing.frontier = $3::jsonb
FROM (SELECT 1) AS one
LEFT JOIN elitea_runtime.execution_interrupts existing
  ON existing.root_response_id = $1::uuid AND existing.interrupt_key = $2`,
		input.RootResponseID, card.InterruptKey, string(frontier)).Scan(&open, &existingCard, &sameFrontier)
	if err != nil {
		return domain.RaiseResult{}, err
	}
	if existingCard != nil {
		if !bytes.Equal(existingCard, cardJSON) || sameFrontier == nil || !*sameFrontier {
			return domain.RaiseResult{}, domain.ErrRaiseConflict
		}
		return domain.RaiseResult{InterruptKey: card.InterruptKey, Replay: true}, nil
	}
	if open >= domain.MaxOpenInterrupts {
		return domain.RaiseResult{}, domain.ErrCapReached
	}
	actions := make([]string, len(card.AvailableActions))
	for index, action := range card.AvailableActions {
		actions[index] = string(action)
	}
	var inserted int
	err = tx.QueryRow(ctx, `
WITH raised AS (
  INSERT INTO elitea_runtime.execution_interrupts (
    root_response_id, interrupt_key, project_id, conversation_id, execution_id, generation,
    interrupt_id, kind, available_actions, frontier, card_json, payload_sha256, source_event_id,
    state, revision)
  VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13, 'PENDING', 1)
  ON CONFLICT DO NOTHING
  RETURNING root_response_id, interrupt_key
), audited AS (
  INSERT INTO elitea_runtime.execution_interrupt_audit (root_response_id, interrupt_key, transition, revision, claim_id)
  SELECT root_response_id, interrupt_key, 'RAISED', 1, $14 FROM raised
)
SELECT count(*) FROM raised`,
		input.RootResponseID, card.InterruptKey, input.ProjectID, conversationID, input.ExecutionID, input.Generation,
		card.InterruptID, string(card.Kind), actions, string(frontier), cardJSON, card.PayloadSHA256, input.SourceEventID,
		input.SourceClaimID).Scan(&inserted)
	if err != nil {
		return domain.RaiseResult{}, err
	}
	if inserted != 1 {
		// The key was checked under the response lock, so the conflict is a
		// reused source event id or an open interrupt id taken by another key.
		return domain.RaiseResult{}, domain.ErrRaiseConflict
	}
	return domain.RaiseResult{InterruptKey: card.InterruptKey}, nil
}

// List returns the open cards of a response the actor may see (contract §6
// GET). It refuses more than MaxOpenInterrupts open rows as a ledger fault
// instead of truncating.
func (r *ExecutionInterruptRepository) List(ctx context.Context, selector domain.Selector) (domain.List, error) {
	if r == nil || r.projects == nil || r.permissions == nil || !selector.Valid() {
		return domain.List{}, domain.ErrNotAllowed
	}
	list := domain.List{ResponseMessageID: selector.ResponseMessageID, Interrupts: []domain.ListedInterrupt{}}
	err := r.projects.WithinProjectTx(ctx, selector.ProjectID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx sqlExecutor) error {
		if _, err := lockOwnedResponse(ctx, tx, selector, false); err != nil {
			return err
		}
		if err := r.permissions(ctx, tx, selector.ProjectID, selector.ActorUserID, domain.PermissionList); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT decision_revision FROM elitea_runtime.execution_interrupt_responses
WHERE root_response_id = $1::uuid AND project_id = $2`, selector.ResponseMessageID, selector.ProjectID).Scan(&list.DecisionRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
SELECT card_json, state, revision FROM elitea_runtime.execution_interrupts
WHERE root_response_id = $1::uuid AND project_id = $2 AND state IN ('PENDING', 'DECIDED')
ORDER BY raised_at, interrupt_key
LIMIT $3`, selector.ResponseMessageID, selector.ProjectID, domain.MaxOpenInterrupts+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var card []byte
			var listed domain.ListedInterrupt
			if err := rows.Scan(&card, &listed.State, &listed.Revision); err != nil {
				return err
			}
			listed.Card = card
			list.Interrupts = append(list.Interrupts, listed)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(list.Interrupts) > domain.MaxOpenInterrupts {
			return domain.ErrLedgerFault
		}
		return nil
	})
	if err != nil {
		return domain.List{}, fmt.Errorf("execution interrupt list: %w", err)
	}
	return list, nil
}

type lockedInterrupt struct {
	state       domain.State
	revision    int64
	requestID   *string
	decision    []byte
	actions     []string
	interruptID string
}

// Decide records one decision exactly once (contract §6 POST): one
// transaction that locks the response, rechecks ownership, membership and
// RBAC, locks the card, then compare-and-sets PENDING at the expected
// revision to DECIDED, bumps the response decision_revision and writes the
// DECIDED audit row in one statement.
func (r *ExecutionInterruptRepository) Decide(ctx context.Context, input domain.DecideInput) (domain.DecideResult, error) {
	if r == nil || r.projects == nil || r.permissions == nil || !input.Selector.Valid() {
		return domain.DecideResult{}, domain.ErrNotAllowed
	}
	if !domain.ValidDigest(input.InterruptKey) {
		return domain.DecideResult{}, domain.ErrNotFound
	}
	// The stored bytes are the only source of the decision: re-derive it here
	// so a caller cannot pair a validated struct with different bytes.
	decision, canonical, err := domain.ParseDecisionRequest(input.Canonical)
	if err != nil || !bytes.Equal(canonical, input.Canonical) {
		return domain.DecideResult{}, domain.ErrInvalidDecision
	}
	selector := input.Selector
	var result domain.DecideResult
	err = r.projects.WithinProjectTx(ctx, selector.ProjectID, interruptWriteTx, func(tx sqlExecutor) error {
		if _, err := lockOwnedResponse(ctx, tx, selector, true); err != nil {
			return err
		}
		if err := r.permissions(ctx, tx, selector.ProjectID, selector.ActorUserID, domain.PermissionDecide); err != nil {
			return err
		}
		var decisionRevision int64
		err := tx.QueryRow(ctx, `SELECT decision_revision FROM elitea_runtime.execution_interrupt_responses
WHERE root_response_id = $1::uuid AND project_id = $2 FOR UPDATE`, selector.ResponseMessageID, selector.ProjectID).Scan(&decisionRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		var row lockedInterrupt
		err = tx.QueryRow(ctx, `
SELECT state, revision, request_id, decision_json, available_actions, interrupt_id
FROM elitea_runtime.execution_interrupts
WHERE root_response_id = $1::uuid AND interrupt_key = $2 AND project_id = $3
FOR UPDATE`, selector.ResponseMessageID, input.InterruptKey, selector.ProjectID).
			Scan(&row.state, &row.revision, &row.requestID, &row.decision, &row.actions, &row.interruptID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		result = domain.DecideResult{
			InterruptKey: input.InterruptKey, InterruptID: row.interruptID, Action: decision.Action,
			RequestID: decision.RequestID,
		}
		if row.state != domain.StatePending {
			replay := row.requestID != nil && *row.requestID == decision.RequestID && bytes.Equal(row.decision, canonical)
			if !replay || (row.state != domain.StateDecided && row.state != domain.StateConsumed) {
				return domain.ErrAlreadyResolved
			}
			result.State, result.Revision, result.Replay, result.DecisionRevision = row.state, row.revision, true, decisionRevision
			return nil
		}
		if !slices.Contains(row.actions, string(decision.Action)) {
			return domain.ErrInvalidDecision
		}
		if decision.ExpectedRevision != row.revision {
			return domain.ErrAlreadyResolved
		}
		err = tx.QueryRow(ctx, `
WITH decided AS (
  UPDATE elitea_runtime.execution_interrupts
  SET state = 'DECIDED', revision = revision + 1, request_id = $4, decision_json = $5,
      decided_by = $6, decided_at = clock_timestamp(), updated_at = clock_timestamp()
  WHERE root_response_id = $1::uuid AND interrupt_key = $2 AND state = 'PENDING' AND revision = $3
  RETURNING root_response_id, interrupt_key, revision
), bumped AS (
  UPDATE elitea_runtime.execution_interrupt_responses response
  SET decision_revision = response.decision_revision + 1, updated_at = clock_timestamp()
  FROM decided WHERE response.root_response_id = decided.root_response_id
  RETURNING response.decision_revision
), audited AS (
  INSERT INTO elitea_runtime.execution_interrupt_audit (root_response_id, interrupt_key, transition, revision, actor_id)
  SELECT root_response_id, interrupt_key, 'DECIDED', revision, $6 FROM decided
)
SELECT decided.revision, bumped.decision_revision FROM decided, bumped`,
			selector.ResponseMessageID, input.InterruptKey, row.revision, decision.RequestID, canonical,
			selector.ActorUserID).Scan(&result.Revision, &result.DecisionRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAlreadyResolved
		}
		if err != nil {
			return err
		}
		result.State = domain.StateDecided
		return nil
	})
	if err != nil {
		return domain.DecideResult{}, fmt.Errorf("execution interrupt decide: %w", err)
	}
	return result, nil
}

// interruptClaimAuthoritySQL locks the caller's live claim: an ordinary
// RUNNING claim (never NODE_RECOVERY) of a RUNNING agent job whose desired
// state is RUNNING, with a current workload session, a granted command inside
// its deadline, an immutable input manifest and no terminal result. It is the
// node recovery authority (node_recovery_control.go) with recovery_mode NONE.
const interruptClaimAuthoritySQL = `
SELECT j.resource_project_id, j.actor_id::bigint, binding.client_message_id, bundle.manifest_digest
FROM elitea_runtime.execution_jobs j
JOIN elitea_runtime.execution_claims c USING (execution_id, generation)
JOIN elitea_runtime.workload_sessions ws ON ws.workload_session_id = c.workload_session_id
 AND ws.workload_identity = c.workload_identity AND ws.producer_id = c.producer_id
JOIN elitea_runtime.command_outbox command USING (execution_id, generation)
JOIN elitea_runtime.agent_execution_jobs binding USING (execution_id, generation)
JOIN elitea_runtime.input_bundles bundle ON bundle.input_bundle_id = j.input_bundle_id
WHERE c.claim_id = $1 AND c.execution_id = $2 AND c.generation = $3 AND c.workload_identity = $4 AND c.fence_token = $5
 AND c.released_at IS NULL AND c.lease_expires_at > clock_timestamp() AND c.recovery_mode = 'NONE'
 AND ws.issued_at <= clock_timestamp() AND ws.expires_at > clock_timestamp() AND ws.revoked_at IS NULL
 AND j.state = 'RUNNING' AND j.desired_state = 'RUNNING'
 AND j.tenant_id = j.resource_project_id::text AND j.projection_project_id = j.resource_project_id
 AND j.actor_id ~ '^[1-9][0-9]{0,17}$'
 AND j.capability_id IN ('agent.execute.application.v1', 'agent.execute.adhoc.v1') AND binding.capability_id = j.capability_id
 AND command.retired_at IS NULL AND command.authority_granted_at IS NOT NULL AND command.deadline > clock_timestamp()
 AND NOT EXISTS (SELECT 1 FROM elitea_runtime.output_inbox terminal
                 WHERE terminal.execution_id = j.execution_id AND terminal.generation = j.generation)
FOR UPDATE OF j, c, ws, command`

type interruptClaimScope struct {
	projectID  int64
	actorID    int64
	responseID string
}

func (r *ExecutionInterruptRepository) lockInterruptClaim(ctx context.Context, tx sqlExecutor, fence domain.ClaimFence) (interruptClaimScope, error) {
	if !fence.Valid() {
		return interruptClaimScope{}, domain.ErrStaleFence
	}
	var scope interruptClaimScope
	var manifest []byte
	// The first lookup takes the locks; a READ COMMITTED statement that waited
	// does not re-read its joined rows, so the second lookup re-evaluates the
	// lease, session, deadline and terminal checks with the database clock
	// while the locks are held (contract §7; node_recovery_control.go).
	for range 2 {
		err := tx.QueryRow(ctx, interruptClaimAuthoritySQL, fence.ClaimID, fence.ExecutionID, fence.Generation, fence.WorkloadIdentity, fence.FenceToken).
			Scan(&scope.projectID, &scope.actorID, &scope.responseID, &manifest)
		if errors.Is(err, pgx.ErrNoRows) {
			return interruptClaimScope{}, domain.ErrStaleFence
		}
		if err != nil {
			return interruptClaimScope{}, err
		}
	}
	if len(manifest) != 32 || !domain.ValidResponseMessageID(scope.responseID) {
		return interruptClaimScope{}, domain.ErrStaleFence
	}
	// The original actor must still hold the decision permission.
	if err := r.permissions(ctx, tx, scope.projectID, scope.actorID, domain.PermissionDecide); err != nil {
		if errors.Is(err, domain.ErrNotAllowed) {
			return interruptClaimScope{}, domain.ErrStaleFence
		}
		return interruptClaimScope{}, err
	}
	return scope, nil
}

// FetchDecided returns every DECIDED card of the claim's root response
// (contract §7 fetch), at most MaxFetchDecisions entries of at most
// MaxFetchEntryBytes each. It never returns decided_by or the frontier.
func (r *ExecutionInterruptRepository) FetchDecided(ctx context.Context, fence domain.ClaimFence) (domain.Fetch, error) {
	if r == nil || r.shared == nil || r.permissions == nil {
		return domain.Fetch{}, domain.ErrStaleFence
	}
	fetch := domain.Fetch{Schema: domain.FetchSchema, ExecutionID: fence.ExecutionID, Generation: fence.Generation, Decisions: []domain.FetchedDecision{}}
	err := r.shared.WithinTx(ctx, interruptWriteTx, func(tx sqlExecutor) error {
		scope, err := r.lockInterruptClaim(ctx, tx, fence)
		if err != nil {
			return err
		}
		// One statement, one snapshot: the revision and the rows agree.
		rows, err := tx.Query(ctx, `
SELECT response.decision_revision, card.interrupt_key, card.interrupt_id, card.revision, card.request_id, card.decision_json
FROM elitea_runtime.execution_interrupt_responses response
LEFT JOIN LATERAL (
  SELECT interrupt_key, interrupt_id, revision, request_id, decision_json, decided_at
  FROM elitea_runtime.execution_interrupts
  WHERE root_response_id = response.root_response_id AND project_id = response.project_id AND state = 'DECIDED'
  ORDER BY decided_at, interrupt_key
  LIMIT $3
) card ON TRUE
WHERE response.root_response_id = $1::uuid AND response.project_id = $2
ORDER BY card.decided_at, card.interrupt_key`, scope.responseID, scope.projectID, domain.MaxFetchDecisions+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key, interruptID, requestID *string
			var revision *int64
			var stored []byte
			if err := rows.Scan(&fetch.DecisionRevision, &key, &interruptID, &revision, &requestID, &stored); err != nil {
				return err
			}
			if key == nil {
				continue // the response has no DECIDED card
			}
			if interruptID == nil || revision == nil || requestID == nil {
				return domain.ErrLedgerFault
			}
			entry := domain.FetchedDecision{InterruptKey: *key, InterruptID: *interruptID, Revision: *revision, RequestID: *requestID}
			decision, canonical, err := domain.ParseDecisionRequest(stored)
			if err != nil || !bytes.Equal(canonical, stored) || decision.RequestID != entry.RequestID {
				return domain.ErrLedgerFault
			}
			entry.Action, entry.Value, entry.CredentialRef = decision.Action, decision.Value, decision.CredentialRef
			digest, err := domain.DecisionSHA256(entry.InterruptKey, entry.RequestID, entry.Revision, entry.Action, entry.Value, entry.CredentialRef)
			if err != nil {
				return domain.ErrLedgerFault
			}
			entry.DecisionSHA256 = digest
			if size, err := entry.CanonicalSize(); err != nil || size > domain.MaxFetchEntryBytes {
				return domain.ErrLedgerFault
			}
			fetch.Decisions = append(fetch.Decisions, entry)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(fetch.Decisions) > domain.MaxFetchDecisions {
			return domain.ErrLedgerFault
		}
		return nil
	})
	if err != nil {
		return domain.Fetch{}, fmt.Errorf("execution interrupt fetch: %w", err)
	}
	return fetch, nil
}

// Ack consumes one fetched decision under the live claim (contract §7 ACK):
// applied -> CONSUMED, stale -> SUPERSEDED, revision+1, audit with the claim.
// The ACK must bind the fetched request_id, revision and decision_sha256. The
// canonical ACK bytes are stored; a byte-identical replay returns Replay and
// any other ACK of a closed card is ErrAckConflict.
func (r *ExecutionInterruptRepository) Ack(ctx context.Context, fence domain.ClaimFence, raw []byte) (domain.AckResult, error) {
	if r == nil || r.shared == nil || r.permissions == nil {
		return domain.AckResult{}, domain.ErrStaleFence
	}
	ack, canonical, err := domain.ParseAck(raw)
	if err != nil {
		return domain.AckResult{}, domain.ErrInvalidAck
	}
	result := domain.AckResult{InterruptKey: ack.InterruptKey}
	err = r.shared.WithinTx(ctx, interruptWriteTx, func(tx sqlExecutor) error {
		scope, err := r.lockInterruptClaim(ctx, tx, fence)
		if err != nil {
			return err
		}
		var row lockedInterrupt
		var stored []byte
		err = tx.QueryRow(ctx, `
SELECT state, revision, request_id, decision_json, ack_json
FROM elitea_runtime.execution_interrupts
WHERE root_response_id = $1::uuid AND interrupt_key = $2 AND project_id = $3
FOR UPDATE`, scope.responseID, ack.InterruptKey, scope.projectID).Scan(&row.state, &row.revision, &row.requestID, &row.decision, &stored)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		if stored != nil {
			if !bytes.Equal(stored, canonical) {
				return domain.ErrAckConflict
			}
			result.State, result.Revision, result.Replay = row.state, row.revision, true
			return nil
		}
		if row.state != domain.StateDecided || row.requestID == nil || *row.requestID != ack.RequestID || row.revision != ack.Revision {
			return domain.ErrAckConflict
		}
		decision, _, err := domain.ParseDecisionRequest(row.decision)
		if err != nil {
			return domain.ErrLedgerFault
		}
		digest, err := domain.DecisionSHA256(ack.InterruptKey, ack.RequestID, row.revision, decision.Action, decision.Value, decision.CredentialRef)
		if err != nil {
			return domain.ErrLedgerFault
		}
		if digest != ack.DecisionSHA256 {
			return domain.ErrAckConflict
		}
		next, transition := domain.StateConsumed, "CONSUMED"
		if ack.Outcome == domain.AckStale {
			next, transition = domain.StateSuperseded, "SUPERSEDED"
		}
		var checkpoint any
		if ack.ChildCheckpointID != nil {
			checkpoint = *ack.ChildCheckpointID
		}
		err = tx.QueryRow(ctx, `
WITH acked AS (
  UPDATE elitea_runtime.execution_interrupts
  SET state = $4, revision = revision + 1, ack_json = $5, updated_at = clock_timestamp(),
      consumed_claim_id = CASE WHEN $4 = 'CONSUMED' THEN $6 END,
      consumed_checkpoint_id = CASE WHEN $4 = 'CONSUMED' THEN $7::text END,
      consumed_at = CASE WHEN $4 = 'CONSUMED' THEN clock_timestamp() END,
      closed_at = CASE WHEN $4 = 'SUPERSEDED' THEN clock_timestamp() END
  WHERE root_response_id = $1::uuid AND interrupt_key = $2 AND state = 'DECIDED' AND revision = $3
  RETURNING root_response_id, interrupt_key, revision
), audited AS (
  INSERT INTO elitea_runtime.execution_interrupt_audit (root_response_id, interrupt_key, transition, revision, claim_id)
  SELECT root_response_id, interrupt_key, $8, revision, $6 FROM acked
)
SELECT revision FROM acked`, scope.responseID, ack.InterruptKey, row.revision, string(next), canonical, fence.ClaimID, checkpoint, transition).
			Scan(&result.Revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAckConflict
		}
		if err != nil {
			return err
		}
		result.State = next
		return nil
	})
	if err != nil {
		return domain.AckResult{}, fmt.Errorf("execution interrupt ack: %w", err)
	}
	return result, nil
}

// CancelAllForResponse moves every open card of the response to CANCELLED
// (stop, cancel, fail-after-drain). SupersedeForResponse moves them to
// SUPERSEDED (regenerate, rewind, new turn). Both run on the caller's
// transaction, which already holds the chat_message_group response row lock
// (pattern agent_cancel.sql), so closing is atomic with the stop or
// regenerate. One statement; at most MaxOpenInterrupts rows.
func (r *ExecutionInterruptRepository) CancelAllForResponse(ctx context.Context, tx sqlExecutor, rootResponseID string, by domain.Closer) ([]domain.Resolved, error) {
	return closeOpenExecutionInterrupts(ctx, tx, rootResponseID, domain.StateCancelled, by)
}

func (r *ExecutionInterruptRepository) SupersedeForResponse(ctx context.Context, tx sqlExecutor, rootResponseID string, by domain.Closer) ([]domain.Resolved, error) {
	return closeOpenExecutionInterrupts(ctx, tx, rootResponseID, domain.StateSuperseded, by)
}

func closeOpenExecutionInterrupts(ctx context.Context, tx sqlExecutor, rootResponseID string, state domain.State, by domain.Closer) ([]domain.Resolved, error) {
	if tx == nil || !domain.ValidResponseMessageID(rootResponseID) || !by.Valid() {
		return nil, domain.ErrInvalidClose
	}
	// Take the per-response lock that Raise takes, so a raise in flight
	// commits first and its card is closed by the UPDATE below.
	var locked int
	err := tx.QueryRow(ctx, `SELECT 1 FROM elitea_runtime.execution_interrupt_responses WHERE root_response_id = $1::uuid FOR UPDATE`, rootResponseID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return []domain.Resolved{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("execution interrupt close: %w", err)
	}
	var actor, claim any
	if by.ActorUserID > 0 {
		actor = by.ActorUserID
	} else {
		claim = by.ClaimID
	}
	rows, err := tx.Query(ctx, `
WITH closed AS (
  UPDATE elitea_runtime.execution_interrupts
  SET state = $2, revision = revision + 1, closed_at = clock_timestamp(), updated_at = clock_timestamp()
  WHERE root_response_id = $1::uuid AND state IN ('PENDING', 'DECIDED')
  RETURNING root_response_id, interrupt_key, interrupt_id, revision
), audited AS (
  INSERT INTO elitea_runtime.execution_interrupt_audit (root_response_id, interrupt_key, transition, revision, actor_id, claim_id)
  SELECT root_response_id, interrupt_key, $2, revision, $3, $4 FROM closed
)
SELECT interrupt_key, interrupt_id, revision FROM closed ORDER BY interrupt_key`, rootResponseID, string(state), actor, claim)
	if err != nil {
		return nil, fmt.Errorf("execution interrupt close: %w", err)
	}
	defer rows.Close()
	closed := []domain.Resolved{}
	for rows.Next() {
		resolved := domain.Resolved{State: state}
		if err := rows.Scan(&resolved.InterruptKey, &resolved.InterruptID, &resolved.Revision); err != nil {
			return nil, err
		}
		closed = append(closed, resolved)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("execution interrupt close: %w", err)
	}
	return closed, nil
}
