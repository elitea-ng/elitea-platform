package repos

import (
	"context"
	"strings"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestNodeRecoveryClaimReusesGenerationWithFreshInspectionFence(t *testing.T) {
	digest := runtime.SHA256([]byte("published-signed-envelope"))
	token := runtime.FenceToken(runtime.SHA256([]byte("recovery-token")))
	request := testClaimRequest(digest)
	request.NodeRecovery = true
	request.CapabilityID = executiondomain.AgentApplicationCapability
	_, receipt, receiptDigest := recoveryFixture(t)
	expires := time.Date(2030, 7, 16, 13, 0, 0, 0, time.UTC)
	observed := expires.Add(-executionapp.MaxClaimLeaseTTLMillis.Duration())
	e := &scriptedExecutor{rowResults: []scriptedRow{claimExecutionRow("SUSPENDED", "RUNNING", true, digest[:], digest[:], true), {err: pgx.ErrNoRows}, {values: []any{receipt, receiptDigest, "SUSPENDED"}}, {values: []any{int64(2)}}, {values: []any{expires, observed, int64(7)}}, {err: pgx.ErrNoRows}, {values: []any{false}}, {values: []any{receipt, receiptDigest, "SUSPENDED"}}}, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("UPDATE 1")}}
	s := &scriptedStore{scriptedExecutor: e}
	r, err := newClaimsRepository(s, func() (string, error) { return "recovery-claim-2", nil }, func() (runtime.FenceToken, error) { return token, nil })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := r.ClaimValidation(t.Context(), request, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != executionapp.ClaimRecoverNodeVisit || decision.Lease.Fence.Generation != request.Generation || decision.Lease.Fence.ExecutionID != request.ExecutionID || decision.Lease.Fence.ClaimAttempt != 2 || decision.Lease.Fence.LeaseEpoch != 2 || decision.Lease.DesiredState != runtime.DesiredSuspended || decision.NodeRecoveryReceipt != string(receipt) {
		t.Fatal(decision)
	}
	if len(e.execCalls) != 2 || !strings.Contains(e.execCalls[1].sql, "recovery_mode=$1") || e.execCalls[1].args[0] != "NODE_RECOVERY" {
		t.Fatal(e.execCalls)
	}
}

func TestNodeRecoveryClaimWithoutOptInNeverAllocatesAuthority(t *testing.T) {
	digest := runtime.SHA256([]byte("published-signed-envelope"))
	e := &scriptedExecutor{rowResults: []scriptedRow{claimExecutionRow("SUSPENDED", "RUNNING", true, digest[:], digest[:], true), {err: pgx.ErrNoRows}}}
	s := &scriptedStore{scriptedExecutor: e}
	r, err := newClaimsRepository(s, func() (string, error) { t.Fatal("ordinary claim generated"); return "", nil }, func() (runtime.FenceToken, error) {
		t.Fatal("ordinary fence generated")
		return runtime.FenceToken{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request := testClaimRequest(digest)
	request.CapabilityID = executiondomain.AgentApplicationCapability
	decision, err := r.ClaimValidation(context.Background(), request, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil || decision.Disposition != executionapp.ClaimRetryLaterNoACK || decision.Lease != (runtime.ActiveLease{}) || len(e.execCalls) != 0 {
		t.Fatal(decision, err)
	}
}

func TestNodeRecoveryClaimNeverOverridesTerminalSettlement(t *testing.T) {
	terminal := testValidationFrame(t)
	fence := terminal.Fence
	digest := runtime.SHA256([]byte("published"))
	token := runtime.FenceToken(runtime.SHA256([]byte("new-fence")))
	expires := time.Date(2030, 7, 16, 13, 0, 0, 0, time.UTC)
	e := &scriptedExecutor{rowResults: []scriptedRow{
		claimExecutionRow("RUNNING", "RUNNING", true, digest[:], digest[:], true),
		{values: []any{"old", "command-1", "execution-1", int64(1), fence.WorkloadIdentity, fence.WorkloadSessionID, fence.ProducerID, int64(1), int64(1), fence.Token[:], expires.Add(-time.Hour), "RUNNING", false, expires.Add(-time.Minute)}},
		{values: []any{int64(2)}}, {values: []any{expires, expires.Add(-executionapp.MaxClaimLeaseTTLMillis.Duration()), int64(0)}},
		{values: []any{terminal.Settlement.ProposalID, string(terminal.Settlement.Outcome), terminal.LogicalOutputID, terminal.EventID, int64(terminal.Sequence), terminal.PayloadDigest[:], terminal.EncodedSettlement, terminal.Settlement.ProposalDigest[:], terminal.Settlement.IdempotencyKey, int64(1)}},
	}, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("UPDATE 1")}}
	store := &scriptedStore{scriptedExecutor: e}
	repo, err := newClaimsRepository(store, func() (string, error) { return "replacement", nil }, func() (runtime.FenceToken, error) { return token, nil })
	if err != nil {
		t.Fatal(err)
	}
	request := testClaimRequest(digest)
	request.NodeRecovery = true
	request.CapabilityID = executiondomain.AgentApplicationCapability
	decision, err := repo.ClaimValidation(t.Context(), request, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil || decision.Disposition != executionapp.ClaimRecoverTerminalACK || decision.SettlementRecovery == nil || decision.NodeRecoveryReceipt != "" || len(e.rowCalls) != 5 || len(e.execCalls) != 2 {
		t.Fatal(decision, err)
	}
}
