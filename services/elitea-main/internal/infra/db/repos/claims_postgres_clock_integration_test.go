package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type claimClockCaptureRepository struct {
	*ClaimsRepository
	renewObservedAt time.Time
}

func (r *claimClockCaptureRepository) RenewLease(ctx context.Context, fence runtimedomain.Fence, leaseTTL executionapp.ClaimLeaseTTLMillis) (runtimedomain.ActiveLease, time.Time, error) {
	lease, observedAt, err := r.ClaimsRepository.RenewLease(ctx, fence, leaseTTL)
	if err == nil {
		r.renewObservedAt = observedAt
	}
	return lease, observedAt, err
}

// TestPostgresLeaseAuthorityUsesDatabaseClockDespiteApplicationSkew is a real
// PostgreSQL 16 service-integration test. It proves lease minting and renewal
// use the state-owner clock; it does not claim transport E2E coverage.
func TestPostgresLeaseAuthorityUsesDatabaseClockDespiteApplicationSkew(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)

	for _, test := range []struct {
		name string
		skew time.Duration
	}{
		{name: "application_clock_plus_24_hours", skew: 24 * time.Hour},
		{name: "application_clock_minus_24_hours", skew: -24 * time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			frame := postgresValidationFrame(t, "database-lease-clock-"+test.name)
			seed := seedPostgresValidationExecution(t, pool, frame, runtimedomain.DesiredRunning)
			resetPostgresExecutionToNoAuthority(
				t,
				pool,
				frame,
				seed,
				true,
				runtimedomain.DesiredRunning,
				time.Now().UTC().Add(time.Hour),
			)

			claims, err := NewClaimsRepository(pool)
			if err != nil {
				t.Fatal(err)
			}
			captured := &claimClockCaptureRepository{ClaimsRepository: claims}
			service, err := executionapp.NewClaimService(
				captured,
				func() time.Time { return time.Now().UTC().Add(test.skew) },
				executionapp.MaxClaimLeaseTTLMillis.Duration(),
			)
			if err != nil {
				t.Fatal(err)
			}

			request := executionapp.ClaimRequest{
				CommandID:            frame.Fence.CommandID,
				OutboxID:             seed.outboxID,
				ExecutionID:          frame.Fence.ExecutionID,
				Generation:           frame.Fence.Generation,
				CapabilityID:         executiondomain.ConfigurationValidationCapability,
				SignedEnvelopeDigest: seed.envelopeDigest,
				WorkloadIdentity:     "spiffe://elitea.test/workload/database-clock-" + test.name,
				WorkloadSessionID:    "database-clock-session-" + test.name,
				ProducerID:           "database-clock-producer-" + test.name,
			}

			var databaseBefore time.Time
			if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseBefore); err != nil {
				t.Fatal(err)
			}
			decision, err := service.Claim(ctx, request)
			if err != nil {
				t.Fatalf("claim with skewed application clock: %v", err)
			}
			var databaseAfter time.Time
			if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseAfter); err != nil {
				t.Fatal(err)
			}
			if decision.Disposition != executionapp.ClaimAccepted {
				t.Fatalf("database-clock claim was not accepted: %+v", decision)
			}
			assertDatabaseAuthoredLeaseTiming(t, decision.Lease, decision.LeaseObservedAt, databaseBefore, databaseAfter)

			var claimedAt, storedExpiresAt time.Time
			if err := pool.QueryRow(ctx, `
SELECT claimed_at, lease_expires_at
FROM elitea_runtime.execution_claims
WHERE claim_id = $1`, decision.Lease.ClaimID).Scan(&claimedAt, &storedExpiresAt); err != nil {
				t.Fatal(err)
			}
			if !claimedAt.Equal(decision.LeaseObservedAt) || !storedExpiresAt.Equal(decision.Lease.ExpiresAt) || storedExpiresAt.Sub(claimedAt) != executionapp.MaxClaimLeaseTTLMillis.Duration() {
				t.Fatalf("claim timing was not authored from one database clock instant: observed=%s claimed=%s returned_expiry=%s stored_expiry=%s", decision.LeaseObservedAt, claimedAt, decision.Lease.ExpiresAt, storedExpiresAt)
			}

			replayed, err := service.Claim(ctx, request)
			if err != nil {
				t.Fatalf("replay active claim with skewed application clock: %v", err)
			}
			if replayed.Disposition != executionapp.ClaimActiveLeaseNoACK || replayed.Lease.ClaimID != decision.Lease.ClaimID || replayed.Lease.Fence != decision.Lease.Fence || !replayed.Lease.ExpiresAt.Equal(decision.Lease.ExpiresAt) {
				t.Fatalf("active-claim replay changed authority or idempotency: initial=%+v replay=%+v", decision, replayed)
			}
			if replayed.LeaseObservedAt.Before(decision.LeaseObservedAt) || !replayed.LeaseObservedAt.Before(replayed.Lease.ExpiresAt) {
				t.Fatalf("active-claim replay did not use a live database observation: initial=%s replay=%s expires=%s", decision.LeaseObservedAt, replayed.LeaseObservedAt, replayed.Lease.ExpiresAt)
			}

			renewed, err := service.Renew(ctx, decision.Lease.Fence)
			if err != nil {
				t.Fatalf("renew with skewed application clock: %v", err)
			}
			if renewed.Fence != decision.Lease.Fence || renewed.ClaimID != decision.Lease.ClaimID {
				t.Fatalf("renewal changed authority-bearing identity: before=%+v after=%+v", decision.Lease, renewed)
			}
			// The lease poll gates the cloud executor's take-back lookup on
			// the job's capability (#1311): an empty one would silently turn
			// the signal off for every agent turn.
			if renewed.CapabilityID != executiondomain.ConfigurationValidationCapability {
				t.Fatalf("renewal capability = %q, want the job's", renewed.CapabilityID)
			}
			if observed, err := service.ObserveDesiredState(ctx, decision.Lease.Fence); err != nil ||
				observed.CapabilityID != executiondomain.ConfigurationValidationCapability {
				t.Fatalf("observed execution = %+v err=%v, want the job's capability", observed, err)
			}
			if captured.renewObservedAt.IsZero() || renewed.ExpiresAt.Sub(captured.renewObservedAt) != executionapp.MaxClaimLeaseTTLMillis.Duration() {
				t.Fatalf("renewal was not exactly database clock plus 30000ms: observed=%s expires=%s", captured.renewObservedAt, renewed.ExpiresAt)
			}
			if renewed.ExpiresAt.Before(decision.Lease.ExpiresAt) {
				t.Fatalf("renewal shortened the live lease: before=%s after=%s", decision.Lease.ExpiresAt, renewed.ExpiresAt)
			}

			var storedRenewedExpiry time.Time
			var live, bounded bool
			if err := pool.QueryRow(ctx, `
SELECT lease_expires_at,
       lease_expires_at > clock_timestamp(),
       lease_expires_at <= clock_timestamp() + interval '30 seconds'
FROM elitea_runtime.execution_claims
WHERE claim_id = $1`, renewed.ClaimID).Scan(&storedRenewedExpiry, &live, &bounded); err != nil {
				t.Fatal(err)
			}
			if !storedRenewedExpiry.Equal(renewed.ExpiresAt) || !live || !bounded {
				t.Fatalf("renewed authority is not live and bounded by the selected profile: stored=%s returned=%s live=%t bounded=%t", storedRenewedExpiry, renewed.ExpiresAt, live, bounded)
			}

			stale := renewed.Fence
			stale.Token[0] ^= 0xff
			if _, err := service.Renew(ctx, stale); !errors.Is(err, runtimedomain.ErrStaleFence) {
				t.Fatalf("stale fence renewed authority: %v", err)
			}
			var afterStaleExpiry time.Time
			if err := pool.QueryRow(ctx, `SELECT lease_expires_at FROM elitea_runtime.execution_claims WHERE claim_id = $1`, renewed.ClaimID).Scan(&afterStaleExpiry); err != nil {
				t.Fatal(err)
			}
			if !afterStaleExpiry.Equal(storedRenewedExpiry) {
				t.Fatalf("stale renewal mutated lease expiry: before=%s after=%s", storedRenewedExpiry, afterStaleExpiry)
			}

			if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_claims
SET lease_expires_at = clock_timestamp() - interval '1 millisecond'
WHERE claim_id = $1`, renewed.ClaimID); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Renew(ctx, renewed.Fence); !errors.Is(err, runtimedomain.ErrLeaseExpired) {
				t.Fatalf("expired database lease was renewed: %v", err)
			}
		})
	}
}

func assertDatabaseAuthoredLeaseTiming(t *testing.T, lease runtimedomain.ActiveLease, observedAt, databaseBefore, databaseAfter time.Time) {
	t.Helper()
	if observedAt.Before(databaseBefore) || observedAt.After(databaseAfter) {
		t.Fatalf("lease observation is outside database-clock bounds: before=%s observed=%s after=%s", databaseBefore, observedAt, databaseAfter)
	}
	if lease.ExpiresAt.Sub(observedAt) != executionapp.MaxClaimLeaseTTLMillis.Duration() {
		t.Fatalf("lease is not exactly database clock plus 30000ms: observed=%s expires=%s", observedAt, lease.ExpiresAt)
	}
}
