package repos

import (
	"context"
	"strconv"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/runtoken"
)

// TestPostgresRunTokenLeaseFollowsTheClaim proves a governed run's
// run token (#1461) is live exactly while its claim's lease is: a renewal
// carries it past the lease it was issued under, and an expired or released
// lease ends it. Real PostgreSQL; skipped without the integration database.
func TestPostgresRunTokenLeaseFollowsTheClaim(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	frame := postgresValidationFrame(t, "model-run-token-lease")
	seed := seedPostgresValidationExecution(t, pool, frame, runtimedomain.DesiredRunning)
	resetPostgresExecutionToNoAuthority(t, pool, frame, seed, true, runtimedomain.DesiredRunning,
		time.Now().UTC().Add(time.Hour))
	claims, err := NewClaimsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, err := executionapp.NewClaimService(claims, func() time.Time { return time.Now().UTC() },
		executionapp.MaxClaimLeaseTTLMillis.Duration())
	if err != nil {
		t.Fatal(err)
	}
	decision, err := service.Claim(ctx, executionapp.ClaimRequest{
		CommandID:            frame.Fence.CommandID,
		OutboxID:             seed.outboxID,
		ExecutionID:          frame.Fence.ExecutionID,
		Generation:           frame.Fence.Generation,
		CapabilityID:         executiondomain.ConfigurationValidationCapability,
		SignedEnvelopeDigest: seed.envelopeDigest,
		WorkloadIdentity:     "spiffe://elitea.test/workload/model-run-token",
		WorkloadSessionID:    "model-run-token-session",
		ProducerID:           "model-run-token-producer",
	})
	if err != nil || decision.Disposition != executionapp.ClaimAccepted {
		t.Fatalf("claim = %+v, %v", decision, err)
	}
	var projectID int64
	var actor string
	if err := pool.QueryRow(ctx, `
SELECT resource_project_id, actor_id FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND generation = $2`,
		frame.Fence.ExecutionID, int64(frame.Fence.Generation)).Scan(&projectID, &actor); err != nil {
		t.Fatal(err)
	}
	actorID, err := strconv.ParseInt(actor, 10, 64)
	if err != nil || actorID <= 0 {
		// The validation frame's actor is not a user id; give the job one so
		// the binding's actor predicate is exercised.
		actorID = 7
		if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs SET actor_id = '7'
WHERE execution_id = $1 AND generation = $2`, frame.Fence.ExecutionID, int64(frame.Fence.Generation)); err != nil {
			t.Fatal(err)
		}
	}
	leases, err := runtoken.NewPostgresLeases(pool)
	if err != nil {
		t.Fatal(err)
	}
	binding := runtoken.Binding{
		ExecutionID: decision.Lease.Fence.ExecutionID,
		Generation:  decision.Lease.Fence.Generation,
		ClaimID:     decision.Lease.ClaimID,
		FenceToken:  append([]byte(nil), decision.Lease.Fence.Token[:]...),
		ProjectID:   projectID,
		UserID:      actorID,
		TokenID:     1,
	}
	live := func(b runtoken.Binding) bool {
		t.Helper()
		ok, err := leases.ClaimLive(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !live(binding) {
		t.Fatal("a fresh claim's token is not live")
	}
	for name, mutate := range map[string]func(*runtoken.Binding){
		"another fence":   func(b *runtoken.Binding) { b.FenceToken = make([]byte, 32) },
		"another project": func(b *runtoken.Binding) { b.ProjectID++ },
		"another actor":   func(b *runtoken.Binding) { b.UserID++ },
		"another claim":   func(b *runtoken.Binding) { b.ClaimID += "x" },
	} {
		other := binding
		mutate(&other)
		if live(other) {
			t.Fatalf("%s: a binding the claim does not match is live", name)
		}
	}

	// The lease the token was issued under is about to lapse; a renewal
	// carries the same token past it.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_claims
SET lease_expires_at = clock_timestamp() + interval '300 milliseconds'
WHERE claim_id = $1`, binding.ClaimID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Renew(ctx, decision.Lease.Fence); err != nil {
		t.Fatalf("renew: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if !live(binding) {
		t.Fatal("a renewed lease lost model access")
	}

	// A lapsed lease ends it.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_claims
SET lease_expires_at = clock_timestamp() - interval '1 millisecond'
WHERE claim_id = $1`, binding.ClaimID); err != nil {
		t.Fatal(err)
	}
	if live(binding) {
		t.Fatal("an expired lease kept model access")
	}

	// So does a released one, even with time left on it.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_claims
SET lease_expires_at = clock_timestamp() + interval '30 seconds', released_at = clock_timestamp(),
    release_reason = 'WORKER_RELEASED'
WHERE claim_id = $1`, binding.ClaimID); err != nil {
		t.Fatal(err)
	}
	if live(binding) {
		t.Fatal("a released claim kept model access")
	}
}
