package repos

import (
	"context"
	"errors"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"sync"
	"testing"
)

func TestPostgresModelCheckpointAuthority(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, capability string
		optIn, recover   bool
	}{
		{"application", executiondomain.AgentApplicationCapability, true, true},
		{"adhoc", executiondomain.AgentAdhocCapability, true, true},
		{"legacy", executiondomain.AgentApplicationCapability, false, false},
		{"toolkit", executiondomain.ToolkitCallToolCapability, true, false},
		{"index", executiondomain.IndexIngestCapability, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := postgresValidationFrame(t, "model-checkpoint-"+tc.name)
			seed := seedPostgresValidationExecution(t, pool, frame, runtimedomain.DesiredRunning)
			_, err := pool.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET capability_id=$2,
   state='RUNNING', invocation_state='MAY_HAVE_STARTED',
   configuration_revision_id=NULL,configuration_type=NULL,catalog_revision=NULL,
   catalog_digest=NULL,schema_id=NULL,schema_revision=NULL,schema_digest=NULL,settings_entry_id=NULL
   WHERE execution_id=$1`, frame.Fence.ExecutionID, tc.capability)
			if err != nil {
				t.Fatal(err)
			}
			expirePostgresClaim(t, pool, seed.claimID)
			repo, err := NewClaimsRepository(pool)
			if err != nil {
				t.Fatal(err)
			}
			request := executionapp.ClaimRequest{
				CommandID: frame.Fence.CommandID, OutboxID: seed.outboxID, ExecutionID: frame.Fence.ExecutionID,
				Generation: frame.Fence.Generation, CapabilityID: tc.capability, SignedEnvelopeDigest: seed.envelopeDigest,
				WorkloadIdentity: "spiffe://elitea.test/worker/replacement", WorkloadSessionID: "replacement-session", ProducerID: "replacement-producer",
				AgentModelCheckpointRecovery: tc.optIn,
			}
			decision, err := repo.ClaimValidation(ctx, request, executionapp.MaxClaimLeaseTTLMillis)
			if err != nil {
				t.Fatal(err)
			}
			if (decision.Disposition == executionapp.ClaimRecoverAgentModelCheckpoint) != tc.recover {
				t.Fatalf("unexpected claim: %s", decision.Disposition)
			}
			digest := runtimedomain.SHA256([]byte("worker-validated-checkpoint"))
			fence := decision.Lease.Fence
			if !tc.recover {
				if _, err := repo.AuthorizeAgentModelCheckpoint(ctx, fence, digest); !errors.Is(err, runtimedomain.ErrStaleFence) {
					t.Fatalf("ordinary claim authorized: %v", err)
				}
				return
			}
			if disposition, err := repo.AuthorizeInvocation(ctx, fence); err != nil || disposition != executionapp.AuthorizeInvocationAlready {
				t.Fatalf("ordinary invocation opened: %s %v", disposition, err)
			}
			if _, err := repo.AuthorizeAgentModelCheckpoint(ctx, frame.Fence, digest); !errors.Is(err, runtimedomain.ErrStaleFence) {
				t.Fatalf("old worker authorized: %v", err)
			}
			var wg sync.WaitGroup
			results := make(chan executionapp.AuthorizeInvocationDisposition, 2)
			failures := make(chan error, 2)
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					d, e := repo.AuthorizeAgentModelCheckpoint(ctx, fence, digest)
					results <- d
					failures <- e
				}()
			}
			wg.Wait()
			close(results)
			close(failures)
			for e := range failures {
				if e != nil {
					t.Fatal(e)
				}
			}
			granted := 0
			for d := range results {
				if d == executionapp.AuthorizeInvocationNow {
					granted++
				} else if d != executionapp.AuthorizeInvocationAlready {
					t.Fatalf("unexpected result: %s", d)
				}
			}
			if granted != 1 {
				t.Fatalf("granted %d attempts", granted)
			}
			if _, err := repo.AuthorizeAgentModelCheckpoint(ctx, fence, runtimedomain.SHA256([]byte("changed"))); !errors.Is(err, executionapp.ErrInvalidClaim) {
				t.Fatalf("digest change accepted: %v", err)
			}
			if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.execution_claims SET claimed_at=clock_timestamp()-interval '1 minute', lease_expires_at=clock_timestamp()-interval '1 second' WHERE claim_id=$1`, decision.Lease.ClaimID); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.AuthorizeAgentModelCheckpoint(ctx, fence, digest); !errors.Is(err, runtimedomain.ErrLeaseExpired) {
				t.Fatalf("expired claim authorized: %v", err)
			}
			if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.execution_claims SET lease_expires_at=clock_timestamp()+interval '30 seconds' WHERE claim_id=$1`, decision.Lease.ClaimID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET desired_state='CANCELLED' WHERE execution_id=$1`, fence.ExecutionID); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.AuthorizeAgentModelCheckpoint(ctx, fence, digest); !errors.Is(err, runtimedomain.ErrStaleFence) {
				t.Fatalf("cancelled claim authorized: %v", err)
			}
		})
	}
}
