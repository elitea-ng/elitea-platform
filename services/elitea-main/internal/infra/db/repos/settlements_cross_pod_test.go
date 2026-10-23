package repos

import (
	"context"
	"strings"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPrepareSettlementAllowsExpiredPredecessorFromDifferentProducer(t *testing.T) {
	frame := testValidationFrame(t)
	replacement := frame.Settlement
	replacement.Fence = runtimedomain.Fence{
		CommandID:         frame.Fence.CommandID,
		ExecutionID:       frame.Fence.ExecutionID,
		Generation:        frame.Fence.Generation,
		WorkloadIdentity:  "spiffe://elitea.test/worker/replacement",
		WorkloadSessionID: "replacement-session",
		ProducerID:        "replacement-producer",
		ClaimAttempt:      frame.Fence.ClaimAttempt + 1,
		LeaseEpoch:        frame.Fence.LeaseEpoch + 1,
		Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("replacement-token"))),
	}

	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			{values: []any{replacement.Fence.CommandID}},
			{err: pgx.ErrNoRows},
			{values: []any{"replacement-claim"}},
			{values: []any{replacement.TerminalEventID, "RUNTIME_FAILURE", []byte(nil)}},
		},
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("INSERT 0 1"),
			pgconn.NewCommandTag("UPDATE 1"),
			pgconn.NewCommandTag("UPDATE 1"),
		},
	}}
	repository, err := newSettlementsRepository(store, func() (string, error) {
		return "replacement-receipt", nil
	})
	if err != nil {
		t.Fatal(err)
	}

	receipt, err := repository.PrepareSettlement(context.Background(), replacement)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ID != "replacement-receipt" || receipt.Outcome != executionapp.SettlementSucceeded {
		t.Fatalf("unexpected replacement receipt: %+v", receipt)
	}
	if len(store.rowCalls) != 4 {
		t.Fatalf("unexpected settlement query count: %d", len(store.rowCalls))
	}

	claimCall := store.rowCalls[2]
	for _, predicate := range []string{
		"workload_identity = $3",
		"workload_session_id = $4",
		"producer_id = $5",
		"claim_attempt = $6",
		"lease_epoch = $7",
		"fence_token = $8",
		"released_at IS NULL",
		"lease_expires_at > clock_timestamp()",
	} {
		if !strings.Contains(claimCall.sql, predicate) {
			t.Fatalf("replacement claim verification is missing %q", predicate)
		}
	}
	if claimCall.args[2] != replacement.Fence.WorkloadIdentity || claimCall.args[4] != replacement.Fence.ProducerID {
		t.Fatalf("settlement did not authenticate the replacement claimant: identity=%v producer=%v", claimCall.args[2], claimCall.args[4])
	}

	outputCall := store.rowCalls[3]
	for _, predicate := range []string{
		"o.logical_output_id = $4",
		"o.event_id = $5",
		"o.sequence = $6",
		"o.payload_digest = $7",
		"o.settlement_proposal_id = $14",
		"o.settlement_outcome = $15",
		"o.settlement_proposal_bytes = $16",
		"o.settlement_proposal_digest = $17",
		"o.settlement_idempotency_key = $18",
	} {
		if !strings.Contains(outputCall.sql, predicate) {
			t.Fatalf("terminal output verification lost %q", predicate)
		}
	}

	branchStart := strings.Index(outputCall.sql, "  AND (\n      (")
	branchSplit := strings.Index(outputCall.sql, "\n      )\n      OR\n      (")
	if branchStart < 0 || branchSplit <= branchStart {
		t.Fatalf("terminal output authority branches are not explicit: %s", outputCall.sql)
	}
	prefix := outputCall.sql[:branchStart]
	currentClaim := outputCall.sql[branchStart:branchSplit]
	predecessor := outputCall.sql[branchSplit:]
	if strings.Contains(prefix, "o.workload_identity = $9") || strings.Contains(prefix, "o.producer_id = $11") {
		t.Fatal("predecessor recovery is still globally bound to the replacement identity")
	}
	for _, predicate := range []string{
		"o.claim_id = $3",
		"o.fence_token = $8",
		"o.workload_identity = $9",
		"o.workload_session_id = $10",
		"o.producer_id = $11",
		"o.claim_attempt = $12",
		"o.lease_epoch = $13",
	} {
		if !strings.Contains(currentClaim, predicate) {
			t.Fatalf("current-claim output branch is missing %q", predicate)
		}
	}
	if strings.Contains(predecessor, "o.workload_identity = $9") || strings.Contains(predecessor, "o.producer_id = $11") {
		t.Fatal("expired predecessor branch remained pod-identity bound")
	}
	for _, predicate := range []string{
		"o.claim_attempt < $12",
		"source_claim.released_at IS NOT NULL",
		"source_claim.release_reason = 'LEASE_EXPIRED'",
	} {
		if !strings.Contains(predecessor, predicate) {
			t.Fatalf("expired predecessor branch is missing %q", predicate)
		}
	}
}

func TestClaimRecoversPreparedSettlementAcrossProducerReplacementAfterIssuingClaim(t *testing.T) {
	publishedDigest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	replacementToken := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("replacement-token")))
	leaseExpires := time.Date(2030, time.July, 16, 13, 0, 0, 0, time.UTC)
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			claimExecutionRow("RUNNING", "SUCCEEDED", true, publishedDigest[:], publishedDigest[:], true),
			{err: pgx.ErrNoRows},
			{values: []any{int64(2)}},
			{values: []any{leaseExpires, leaseExpires.Add(-executionapp.MaxClaimLeaseTTLMillis.Duration()), int64(0)}},
			{values: []any{"predecessor-receipt", string(executionapp.SettlementSucceeded)}},
		},
		execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1")},
	}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "replacement-claim", nil },
		func() (runtimedomain.FenceToken, error) { return replacementToken, nil },
	)
	if err != nil {
		t.Fatal(err)
	}

	request := executionapp.ClaimRequest{
		CommandID:            "command-1",
		OutboxID:             "outbox-1",
		ExecutionID:          "execution-1",
		Generation:           1,
		CapabilityID:         executiondomain.ConfigurationValidationCapability,
		SignedEnvelopeDigest: publishedDigest,
		WorkloadIdentity:     "spiffe://elitea.test/workload/replacement",
		WorkloadSessionID:    "replacement-session",
		ProducerID:           "replacement-producer",
	}
	decision, err := repository.ClaimValidation(context.Background(), request, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != executionapp.ClaimRecoverSettlement || decision.SettlementRecovery == nil || decision.SettlementRecovery.Receipt == nil {
		t.Fatalf("prepared settlement was not recovered: %+v", decision)
	}
	if decision.SettlementRecovery.Receipt.ID != "predecessor-receipt" || decision.SettlementRecovery.Receipt.Outcome != executionapp.SettlementSucceeded {
		t.Fatalf("unexpected prepared settlement: %+v", decision.SettlementRecovery.Receipt)
	}
	if decision.Lease.ClaimID != "replacement-claim" || decision.Lease.Fence.WorkloadIdentity != request.WorkloadIdentity || decision.Lease.Fence.ProducerID != request.ProducerID {
		t.Fatalf("prepared settlement recovery was not rebound to the replacement claim: %+v", decision.Lease)
	}
	if len(store.rowCalls) != 5 {
		t.Fatalf("unexpected claim query count: %d", len(store.rowCalls))
	}
	claimInsert := store.rowCalls[3]
	if !strings.Contains(claimInsert.sql, "INSERT INTO elitea_runtime.execution_claims") || claimInsert.args[4] != request.WorkloadIdentity || claimInsert.args[5] != request.ProducerID {
		t.Fatalf("replacement claim was not issued before recovery: sql=%s args=%v", claimInsert.sql, claimInsert.args)
	}
	call := store.rowCalls[4]
	if len(call.args) != 2 || call.args[0] != "execution-1" || call.args[1] != int64(1) {
		t.Fatalf("prepared settlement was not keyed only by execution generation: %v", call.args)
	}
	if strings.Contains(call.sql, "workload_identity") || strings.Contains(call.sql, "producer_id") {
		t.Fatalf("prepared settlement remained bound to predecessor pod identity: %s", call.sql)
	}
}
