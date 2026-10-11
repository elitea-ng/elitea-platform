package repos

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

type SettlementsRepository struct {
	store        sharedStore
	newReceiptID func() (string, error)
}

func NewSettlementsRepository(pool *pgxpool.Pool) (*SettlementsRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return newSettlementsRepository(store, randomClaimID)
}

func newSettlementsRepository(store sharedStore, newReceiptID func() (string, error)) (*SettlementsRepository, error) {
	if store == nil || newReceiptID == nil {
		return nil, errors.New("settlement database and receipt generator are required")
	}
	return &SettlementsRepository{store: store, newReceiptID: newReceiptID}, nil
}

func (r *SettlementsRepository) PrepareSettlement(ctx context.Context, proposal executionapp.SettlementProposal) (executionapp.SettlementReceipt, error) {
	if err := proposal.Validate(); err != nil {
		return executionapp.SettlementReceipt{}, err
	}
	proposalBytes, err := settlementProposalBytes(proposal)
	if err != nil {
		return executionapp.SettlementReceipt{}, err
	}

	var receipt executionapp.SettlementReceipt
	err = r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		var commandID string
		if err := tx.QueryRow(ctx, `
SELECT command_id
FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND generation = $2
FOR UPDATE`, proposal.Fence.ExecutionID, int64(proposal.Fence.Generation)).Scan(&commandID); errors.Is(err, pgx.ErrNoRows) {
			return runtimedomain.ErrStaleFence
		} else if err != nil {
			return fmt.Errorf("lock execution for settlement: %w", err)
		}
		if commandID != proposal.Fence.CommandID {
			return runtimedomain.ErrStaleFence
		}

		existing, err := loadSettlement(ctx, tx, proposal.Fence.ExecutionID, proposal.Fence.Generation, commandID)
		if err == nil {
			// Idempotent PrepareSettlement replay belongs to the exact claim that
			// durably prepared it. Cross-pod recovery uses the separate
			// post-claim loadSettlementForExecution path and must not weaken this
			// ordinary RPC authorization boundary.
			if !sameSettlementFence(existing.Fence, proposal.Fence) {
				return runtimedomain.ErrStaleFence
			}
			if existing.IdempotencyKey != proposal.IdempotencyKey || existing.ProposalDigest != proposal.ProposalDigest || existing.Receipt.Outcome != proposal.Outcome {
				return executionapp.ErrSettlementConflict
			}
			receipt = existing.Receipt
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("load existing execution settlement: %w", err)
		}

		var claimID string
		err = tx.QueryRow(ctx, `
SELECT claim_id
FROM elitea_runtime.execution_claims
WHERE execution_id = $1
  AND generation = $2
  AND workload_identity = $3
  AND workload_session_id = $4
  AND producer_id = $5
  AND claim_attempt = $6
  AND lease_epoch = $7
  AND fence_token = $8
  AND released_at IS NULL
  AND lease_expires_at > clock_timestamp()
FOR UPDATE`,
			proposal.Fence.ExecutionID,
			int64(proposal.Fence.Generation),
			proposal.Fence.WorkloadIdentity,
			proposal.Fence.WorkloadSessionID,
			proposal.Fence.ProducerID,
			int64(proposal.Fence.ClaimAttempt),
			int64(proposal.Fence.LeaseEpoch),
			proposal.Fence.Token[:],
		).Scan(&claimID)
		if errors.Is(err, pgx.ErrNoRows) {
			return runtimedomain.ErrStaleFence
		}
		if err != nil {
			return fmt.Errorf("verify settlement claim: %w", err)
		}

		var terminalEventID string
		err = tx.QueryRow(ctx, `
SELECT o.event_id
FROM elitea_runtime.output_inbox AS o
JOIN elitea_runtime.execution_claims AS source_claim
  ON source_claim.claim_id = o.claim_id
WHERE o.execution_id = $1
  AND o.generation = $2
  AND o.logical_output_id = $4
  AND o.event_id = $5
  AND o.sequence = $6
  AND o.payload_digest = $7
  AND o.settlement_proposal_id = $14
  AND o.settlement_outcome = $15
  AND o.settlement_proposal_bytes = $16
  AND o.settlement_proposal_digest = $17
  AND o.settlement_idempotency_key = $18
  AND o.projected_at IS NOT NULL
  AND (
      (
          o.claim_id = $3
          AND o.fence_token = $8
          AND o.workload_identity = $9
          AND o.workload_session_id = $10
          AND o.producer_id = $11
          AND o.claim_attempt = $12
          AND o.lease_epoch = $13
      )
      OR
      (
          o.claim_attempt < $12
          AND source_claim.released_at IS NOT NULL
          AND source_claim.release_reason = 'LEASE_EXPIRED'
      )
  )`,
			proposal.Fence.ExecutionID,
			int64(proposal.Fence.Generation),
			claimID,
			proposal.TerminalLogicalOutputID,
			proposal.TerminalEventID,
			int64(proposal.TerminalSequence),
			proposal.TerminalPayloadDigest[:],
			proposal.Fence.Token[:],
			proposal.Fence.WorkloadIdentity,
			proposal.Fence.WorkloadSessionID,
			proposal.Fence.ProducerID,
			int64(proposal.Fence.ClaimAttempt),
			int64(proposal.Fence.LeaseEpoch),
			proposal.ProposalID,
			string(proposal.Outcome),
			proposalBytes,
			proposal.ProposalDigest[:],
			proposal.IdempotencyKey,
		).Scan(&terminalEventID)
		if errors.Is(err, pgx.ErrNoRows) {
			return executionapp.ErrTerminalOutputNotReady
		}
		if err != nil {
			return fmt.Errorf("verify durable terminal output: %w", err)
		}

		receiptID, err := r.newReceiptID()
		if err != nil || receiptID == "" {
			return errors.New("generate settlement receipt ID")
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.execution_settlements (
    execution_id, generation, claim_id, fence_token, workload_identity,
    workload_session_id, producer_id, claim_attempt, lease_epoch, settlement_receipt_id,
    proposal_id, proposal_bytes, proposal_digest, idempotency_key,
    disposition, final_logical_output_id, terminal_event_id,
    terminal_sequence, terminal_payload_digest, committed_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
    $15, $16, $17, $18, $19, clock_timestamp()
)`,
			proposal.Fence.ExecutionID,
			int64(proposal.Fence.Generation),
			claimID,
			proposal.Fence.Token[:],
			proposal.Fence.WorkloadIdentity,
			proposal.Fence.WorkloadSessionID,
			proposal.Fence.ProducerID,
			int64(proposal.Fence.ClaimAttempt),
			int64(proposal.Fence.LeaseEpoch),
			receiptID,
			proposal.ProposalID,
			proposalBytes,
			proposal.ProposalDigest[:],
			proposal.IdempotencyKey,
			string(proposal.Outcome),
			proposal.TerminalLogicalOutputID,
			terminalEventID,
			int64(proposal.TerminalSequence),
			proposal.TerminalPayloadDigest[:],
		); err != nil {
			return fmt.Errorf("insert execution settlement: %w", err)
		}

		jobState := settlementJobState(proposal.Outcome)
		if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET state = $3,
    settled_at = CASE WHEN $3 = 'SETTLING' THEN NULL ELSE clock_timestamp() END
WHERE execution_id = $1 AND generation = $2`, proposal.Fence.ExecutionID, int64(proposal.Fence.Generation), jobState); err != nil {
			return fmt.Errorf("mark execution settled: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_claims
SET released_at = clock_timestamp(), release_reason = 'SETTLED'
WHERE claim_id = $1 AND released_at IS NULL`, claimID); err != nil {
			return fmt.Errorf("release settled execution claim: %w", err)
		}
		receipt = executionapp.SettlementReceipt{ID: receiptID, Outcome: proposal.Outcome}
		return nil
	})
	if err != nil {
		return executionapp.SettlementReceipt{}, err
	}
	return receipt, nil
}

type storedSettlement struct {
	Receipt        executionapp.SettlementReceipt
	IdempotencyKey string
	ProposalDigest runtimedomain.Digest
	Fence          runtimedomain.Fence
}

func loadSettlement(ctx context.Context, tx sqlExecutor, executionID string, generation uint64, commandID string) (storedSettlement, error) {
	var stored storedSettlement
	var outcome, releaseReason string
	var digestBytes, tokenBytes []byte
	var claimAttempt, leaseEpoch int64
	var claimReleased bool
	err := tx.QueryRow(ctx, `
SELECT s.settlement_receipt_id,
       s.disposition,
       s.idempotency_key,
       s.proposal_digest,
       s.fence_token,
       s.workload_identity,
       s.workload_session_id,
       s.producer_id,
       s.claim_attempt,
       s.lease_epoch,
       c.released_at IS NOT NULL,
       COALESCE(c.release_reason, '')
FROM elitea_runtime.execution_settlements AS s
JOIN elitea_runtime.execution_claims AS c
  ON c.claim_id = s.claim_id
 AND c.execution_id = s.execution_id
 AND c.generation = s.generation
 AND c.fence_token = s.fence_token
 AND c.workload_identity = s.workload_identity
 AND c.workload_session_id = s.workload_session_id
 AND c.producer_id = s.producer_id
 AND c.claim_attempt = s.claim_attempt
 AND c.lease_epoch = s.lease_epoch
WHERE s.execution_id = $1 AND s.generation = $2`, executionID, int64(generation)).Scan(
		&stored.Receipt.ID,
		&outcome,
		&stored.IdempotencyKey,
		&digestBytes,
		&tokenBytes,
		&stored.Fence.WorkloadIdentity,
		&stored.Fence.WorkloadSessionID,
		&stored.Fence.ProducerID,
		&claimAttempt,
		&leaseEpoch,
		&claimReleased,
		&releaseReason,
	)
	if err != nil {
		return storedSettlement{}, err
	}
	if stored.Receipt.ID == "" || commandID == "" || claimAttempt <= 0 || leaseEpoch <= 0 || len(tokenBytes) != len(stored.Fence.Token) || !claimReleased || releaseReason != "SETTLED" {
		return storedSettlement{}, errors.New("execution settlement contains invalid persisted authority")
	}
	stored.Receipt.Outcome = executionapp.SettlementOutcome(outcome)
	stored.Fence.CommandID = commandID
	stored.Fence.ExecutionID = executionID
	stored.Fence.Generation = generation
	stored.Fence.ClaimAttempt = uint64(claimAttempt)
	stored.Fence.LeaseEpoch = uint64(leaseEpoch)
	copy(stored.Fence.Token[:], tokenBytes)
	if err := stored.Fence.Validate(); err != nil {
		return storedSettlement{}, errors.New("execution settlement contains invalid persisted fence")
	}
	stored.ProposalDigest, err = storedDigest(digestBytes)
	if err != nil {
		return storedSettlement{}, err
	}
	return stored, nil
}

func sameSettlementFence(stored, requested runtimedomain.Fence) bool {
	return stored.CommandID == requested.CommandID &&
		stored.ExecutionID == requested.ExecutionID &&
		stored.Generation == requested.Generation &&
		stored.WorkloadIdentity == requested.WorkloadIdentity &&
		stored.WorkloadSessionID == requested.WorkloadSessionID &&
		stored.ProducerID == requested.ProducerID &&
		stored.ClaimAttempt == requested.ClaimAttempt &&
		stored.LeaseEpoch == requested.LeaseEpoch &&
		subtle.ConstantTimeCompare(stored.Token[:], requested.Token[:]) == 1
}

// loadSettlementForExecution is only called after ClaimValidation has inserted
// the authenticated replacement claim in the same transaction. At that point a
// prepared settlement is execution authority, not predecessor-pod authority.
func loadSettlementForExecution(ctx context.Context, tx sqlExecutor, executionID string, generation uint64) (executionapp.SettlementReceipt, error) {
	var receipt executionapp.SettlementReceipt
	var outcome string
	err := tx.QueryRow(ctx, `
SELECT settlement_receipt_id, disposition
FROM elitea_runtime.execution_settlements
WHERE execution_id = $1
  AND generation = $2`, executionID, int64(generation)).Scan(&receipt.ID, &outcome)
	if err != nil {
		return executionapp.SettlementReceipt{}, err
	}
	receipt.Outcome = executionapp.SettlementOutcome(outcome)
	return receipt, nil
}

func settlementProposalBytes(proposal executionapp.SettlementProposal) ([]byte, error) {
	wireOutcome, err := settlementOutcomeProto(proposal.Outcome)
	if err != nil {
		return nil, err
	}
	wire := &runtimev1.SettlementProposalV1{
		ProposalId:              proposal.ProposalID,
		RequestedOutcome:        wireOutcome,
		TerminalLogicalOutputId: proposal.TerminalLogicalOutputID,
		TerminalEventId:         proposal.TerminalEventID,
		TerminalSequence:        proposal.TerminalSequence,
		PrepareIdempotencyKey:   proposal.IdempotencyKey,
		TerminalPayloadDigest: &runtimev1.DigestV1{
			Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
			Value:     append([]byte(nil), proposal.TerminalPayloadDigest[:]...),
		},
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("encode settlement proposal: %w", err)
	}
	if runtimedomain.SHA256(encoded) != proposal.ProposalDigest {
		return nil, executionapp.ErrInvalidSettlement
	}
	return encoded, nil
}

func settlementOutcomeProto(outcome executionapp.SettlementOutcome) (runtimev1.ExecutionOutcomeV1, error) {
	switch outcome {
	case executionapp.SettlementSucceeded:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED, nil
	case executionapp.SettlementFailed:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_FAILED, nil
	case executionapp.SettlementCancelled:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_CANCELLED, nil
	case executionapp.SettlementOutcomeUnknown:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_OUTCOME_UNKNOWN, nil
	default:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_UNSPECIFIED, executionapp.ErrInvalidSettlement
	}
}

func settlementJobState(outcome executionapp.SettlementOutcome) string {
	switch outcome {
	case executionapp.SettlementSucceeded:
		return "SUCCEEDED"
	case executionapp.SettlementFailed:
		return "FAILED"
	case executionapp.SettlementCancelled:
		return "CANCELLED"
	default:
		return "SETTLING"
	}
}

var _ executionapp.SettlementRepository = (*SettlementsRepository)(nil)
