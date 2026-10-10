package repos

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The per-claim worker token for elitea-vector (ADR-0031 decision 1,
// shared/0160) against the real execution kernel: minted only for a live
// claim of a vector capability, bound to the execution's own project and
// actor, stored as a hash, and inactive as soon as the claim settles, is
// cancelled, is lost or is taken over.

var vectorClaimCapabilities = []string{
	executiondomain.IndexIngestCapability,
	executiondomain.ToolkitCallToolCapability,
	executiondomain.AgentApplicationCapability,
	executiondomain.AgentAdhocCapability,
}

type vectorClaimFixture struct {
	pool  *pgxpool.Pool
	store *VectorClaimTokens
	fence runtimedomain.Fence
	seed  postgresValidationSeed
}

func newVectorClaimFixture(t *testing.T, name string) vectorClaimFixture {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	frame := postgresValidationFrame(t, name)
	seed := seedPostgresValidationExecution(t, pool, frame, runtimedomain.DesiredRunning)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A toolkit call by user 42, as the invocation-fence test shapes one.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET capability_id = $3,
    capability_version = '1',
    actor_id = '42',
    configuration_revision_id = NULL,
    configuration_type = NULL,
    catalog_revision = NULL,
    catalog_digest = NULL,
    schema_id = NULL,
    schema_revision = NULL,
    schema_digest = NULL,
    settings_entry_id = NULL
WHERE execution_id = $1 AND generation = $2`,
		frame.Fence.ExecutionID, int64(frame.Fence.Generation), executiondomain.ToolkitCallToolCapability,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.workload_sessions
    (workload_session_id, workload_identity, producer_id, issued_at, expires_at)
VALUES ($1, $2, $3, clock_timestamp() - interval '3 hours', clock_timestamp() + interval '1 day')`,
		frame.Fence.WorkloadSessionID, frame.Fence.WorkloadIdentity, frame.Fence.ProducerID,
	); err != nil {
		t.Fatal(err)
	}
	return vectorClaimFixture{pool: pool, store: NewVectorClaimTokens(pool), fence: frame.Fence, seed: seed}
}

func (f vectorClaimFixture) mint(t *testing.T, fence runtimedomain.Fence, bearer string) (VectorClaimTokenMinted, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return f.store.Mint(ctx, VectorClaimTokenMint{
		Fence:        fence,
		TokenSHA256:  sha256.Sum256([]byte(bearer)),
		Capabilities: vectorClaimCapabilities,
		Sources:      []string{"toolkit_index"},
		MaxLifetime:  6 * time.Hour,
	})
}

func (f vectorClaimFixture) active(t *testing.T, bearer string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := f.store.Facts(ctx, sha256.Sum256([]byte(bearer)))
	if errors.Is(err, ErrVectorClaimTokenFactsNotFound) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func (f vectorClaimFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := f.pool.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresVectorClaimTokenIsBoundToTheLiveClaim(t *testing.T) {
	f := newVectorClaimFixture(t, "vector-claim-mint")
	before := time.Now()
	minted, err := f.mint(t, f.fence, "bearer-one")
	if err != nil {
		t.Fatal(err)
	}
	// The binding comes from the execution, never from the caller.
	if !minted.Minted || minted.ProjectID != 1 || minted.ActorID != 42 ||
		!slices.Equal(minted.Sources, []string{"toolkit_index"}) {
		t.Fatalf("unexpected mint %+v", minted)
	}
	// The command deadline (seeded one hour out) is below the 6 h cap.
	if !minted.ExpiresAt.After(before.Add(50*time.Minute)) || minted.ExpiresAt.After(before.Add(61*time.Minute)) {
		t.Fatalf("expiry %v is not the command deadline", minted.ExpiresAt)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	facts, err := f.store.Facts(ctx, sha256.Sum256([]byte("bearer-one")))
	if err != nil {
		t.Fatal(err)
	}
	if facts.ProjectID != 1 || facts.ActorID != 42 || !facts.ExpiresAt.Equal(minted.ExpiresAt) ||
		!slices.Equal(facts.Sources, []string{"toolkit_index"}) {
		t.Fatalf("unexpected facts %+v", facts)
	}

	// Only the hash is stored.
	var stored []byte
	var rows int
	if err := f.pool.QueryRow(ctx, `
SELECT count(*) OVER (), token_sha256 FROM elitea_runtime.vector_claim_tokens`).Scan(&rows, &stored); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("bearer-one"))
	if rows != 1 || !slices.Equal(stored, hash[:]) {
		t.Fatalf("stored %d rows, hash %x", rows, stored)
	}

	// A repeated claim receipt replaces the token: only the latest works.
	if _, err := f.mint(t, f.fence, "bearer-two"); err != nil {
		t.Fatal(err)
	}
	if f.active(t, "bearer-one") || !f.active(t, "bearer-two") {
		t.Fatal("a replaced token is still active, or its replacement is not")
	}

	// A fence that is not the live claim's mints nothing.
	stale := f.fence
	stale.Token[0] ^= 0xff
	if _, err := f.mint(t, stale, "bearer-stale"); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("stale fence: %v", err)
	}
	stale = f.fence
	stale.LeaseEpoch++
	if _, err := f.mint(t, stale, "bearer-stale"); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("other lease epoch: %v", err)
	}
	if f.active(t, "bearer-stale") {
		t.Fatal("a stale mint left an active token")
	}
}

func TestPostgresVectorClaimTokenIsNotMintedForOtherExecutions(t *testing.T) {
	f := newVectorClaimFixture(t, "vector-claim-ineligible")
	for name, update := range map[string]string{
		"another capability": `UPDATE elitea_runtime.execution_jobs SET capability_id = 'toolkit.execute.read.v1' WHERE execution_id = $1`,
		"a non-user actor":   `UPDATE elitea_runtime.execution_jobs SET actor_id = 'system' WHERE execution_id = $1`,
		"a passed deadline":  `UPDATE elitea_runtime.command_outbox SET deadline = clock_timestamp() - interval '1 minute' WHERE execution_id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			f.exec(t, `UPDATE elitea_runtime.execution_jobs SET capability_id = 'toolkit.call_tool.v1', actor_id = '42' WHERE execution_id = $1`, f.fence.ExecutionID)
			f.exec(t, `UPDATE elitea_runtime.command_outbox SET deadline = clock_timestamp() + interval '1 hour' WHERE execution_id = $1`, f.fence.ExecutionID)
			f.exec(t, update, f.fence.ExecutionID)
			minted, err := f.mint(t, f.fence, "bearer-"+name)
			if err != nil || minted.Minted {
				t.Fatalf("minted %+v, %v", minted, err)
			}
			if f.active(t, "bearer-"+name) {
				t.Fatal("an ineligible execution has an active token")
			}
		})
	}
}

func TestPostgresVectorClaimTokenDiesWithItsClaim(t *testing.T) {
	t.Run("settled", func(t *testing.T) {
		f := newVectorClaimFixture(t, "vector-claim-settled")
		if _, err := f.mint(t, f.fence, "bearer"); err != nil {
			t.Fatal(err)
		}
		if !f.active(t, "bearer") {
			t.Fatal("not active before settlement")
		}
		// The two writes SettlementsRepository.PrepareSettlement makes in its
		// transaction (settlements.go): the terminal state, then the release.
		f.exec(t, `
UPDATE elitea_runtime.execution_jobs SET state = 'SUCCEEDED', settled_at = clock_timestamp()
WHERE execution_id = $1 AND generation = $2`, f.fence.ExecutionID, int64(f.fence.Generation))
		f.exec(t, `
UPDATE elitea_runtime.execution_claims SET released_at = clock_timestamp(), release_reason = 'SETTLED'
WHERE claim_id = $1 AND released_at IS NULL`, f.seed.claimID)
		if f.active(t, "bearer") {
			t.Fatal("a settled execution's token is active")
		}
	})

	t.Run("settling", func(t *testing.T) {
		f := newVectorClaimFixture(t, "vector-claim-settling")
		if _, err := f.mint(t, f.fence, "bearer"); err != nil {
			t.Fatal(err)
		}
		f.exec(t, `UPDATE elitea_runtime.execution_jobs SET state = 'SETTLING' WHERE execution_id = $1`, f.fence.ExecutionID)
		if f.active(t, "bearer") {
			t.Fatal("a settling execution's token is active")
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		f := newVectorClaimFixture(t, "vector-claim-cancelled")
		if _, err := f.mint(t, f.fence, "bearer"); err != nil {
			t.Fatal(err)
		}
		jobs, err := NewAdminBackgroundJobsRepository(f.pool)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := jobs.CancelRuntimeJob(ctx, f.fence.ExecutionID); err != nil {
			t.Fatal(err)
		}
		if f.active(t, "bearer") {
			t.Fatal("a cancelled execution's token is active")
		}
	})

	t.Run("lost and taken over", func(t *testing.T) {
		f := newVectorClaimFixture(t, "vector-claim-lost")
		if _, err := f.mint(t, f.fence, "bearer-old"); err != nil {
			t.Fatal(err)
		}
		// The worker stops renewing: the lease lapses.
		expirePostgresClaim(t, f.pool, f.seed.claimID)
		if f.active(t, "bearer-old") {
			t.Fatal("a lost claim's token is active")
		}
		// Another worker takes the execution over. The lost claim's token
		// stays dead, and only the new claim can mint.
		repository, err := NewClaimsRepository(f.pool)
		if err != nil {
			t.Fatal(err)
		}
		service, err := executionapp.NewClaimService(repository, time.Now, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		const (
			identity = "spiffe://elitea.test/workload/vector-replacement"
			session  = "vector-replacement-session"
			producer = "vector-replacement-producer"
		)
		f.exec(t, `
INSERT INTO elitea_runtime.workload_sessions
    (workload_session_id, workload_identity, producer_id, issued_at, expires_at)
VALUES ($1, $2, $3, clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 day')`,
			session, identity, producer)
		decision, err := service.Claim(ctx, executionapp.ClaimRequest{
			CommandID: f.fence.CommandID, OutboxID: f.seed.outboxID,
			ExecutionID: f.fence.ExecutionID, Generation: f.fence.Generation,
			CapabilityID:         executiondomain.ToolkitCallToolCapability,
			SignedEnvelopeDigest: f.seed.envelopeDigest,
			WorkloadIdentity:     identity, WorkloadSessionID: session, ProducerID: producer,
		})
		if err != nil {
			t.Fatal(err)
		}
		if decision.Disposition != executionapp.ClaimAccepted {
			t.Fatalf("takeover disposition %q", decision.Disposition)
		}
		if _, err := f.mint(t, f.fence, "bearer-stale"); !errors.Is(err, runtimedomain.ErrStaleFence) {
			t.Fatalf("the lost claim minted: %v", err)
		}
		minted, err := f.mint(t, decision.Lease.Fence, "bearer-new")
		if err != nil || !minted.Minted {
			t.Fatalf("takeover mint: %+v %v", minted, err)
		}
		if f.active(t, "bearer-old") || !f.active(t, "bearer-new") {
			t.Fatal("after takeover only the new claim's token may be active")
		}
	})

	t.Run("workload session revoked", func(t *testing.T) {
		f := newVectorClaimFixture(t, "vector-claim-session")
		if _, err := f.mint(t, f.fence, "bearer"); err != nil {
			t.Fatal(err)
		}
		f.exec(t, `UPDATE elitea_runtime.workload_sessions SET revoked_at = clock_timestamp() WHERE workload_session_id = $1`, f.fence.WorkloadSessionID)
		if f.active(t, "bearer") {
			t.Fatal("a revoked workload session's token is active")
		}
	})

	t.Run("project suspended", func(t *testing.T) {
		f := newVectorClaimFixture(t, "vector-claim-suspended")
		if _, err := f.mint(t, f.fence, "bearer"); err != nil {
			t.Fatal(err)
		}
		f.exec(t, `UPDATE centry.project SET suspended = true WHERE id = 1`)
		if f.active(t, "bearer") {
			t.Fatal("a suspended project's token is active")
		}
	})

	t.Run("expired", func(t *testing.T) {
		f := newVectorClaimFixture(t, "vector-claim-expired")
		if _, err := f.mint(t, f.fence, "bearer"); err != nil {
			t.Fatal(err)
		}
		f.exec(t, `UPDATE elitea_runtime.vector_claim_tokens SET created_at = clock_timestamp() - interval '2 hours', expires_at = clock_timestamp() - interval '1 second'`)
		if f.active(t, "bearer") {
			t.Fatal("an expired token is active")
		}
	})
}
