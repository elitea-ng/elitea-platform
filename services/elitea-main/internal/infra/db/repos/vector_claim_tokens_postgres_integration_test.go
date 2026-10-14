package repos

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	_, err := f.store.Facts(ctx, sha256.Sum256([]byte(bearer)), MemoryIntentNone)
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
	facts, err := f.store.Facts(ctx, sha256.Sum256([]byte("bearer-one")), MemoryIntentNone)
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

// A re-claim of a live claim whose execution no longer qualifies takes the
// earlier token away in the same statement.
func TestPostgresVectorClaimTokenIsRevokedWhenTheClaimNoLongerQualifies(t *testing.T) {
	for name, update := range map[string]string{
		"another capability": `UPDATE elitea_runtime.execution_jobs SET capability_id = 'toolkit.execute.read.v1' WHERE execution_id = $1`,
		"a non-user actor":   `UPDATE elitea_runtime.execution_jobs SET actor_id = 'system' WHERE execution_id = $1`,
		"a passed deadline":  `UPDATE elitea_runtime.command_outbox SET deadline = clock_timestamp() - interval '1 minute' WHERE execution_id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newVectorClaimFixture(t, "vector-claim-revoked")
			minted, err := f.mint(t, f.fence, "bearer-old")
			if err != nil || !minted.Minted || !f.active(t, "bearer-old") {
				t.Fatalf("first mint %+v, %v", minted, err)
			}
			f.exec(t, update, f.fence.ExecutionID)
			minted, err = f.mint(t, f.fence, "bearer-new")
			if err != nil || minted.Minted {
				t.Fatalf("re-claim minted %+v, %v", minted, err)
			}
			if f.active(t, "bearer-old") {
				t.Fatal("the earlier token is still active")
			}
			if f.active(t, "bearer-new") {
				t.Fatal("a non-qualifying mint left an active token")
			}
			var rows int
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.vector_claim_tokens`).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Fatalf("%d token rows remain", rows)
			}
		})
	}
}

// A stale fence is not a live claim: its mint touches nothing, including the
// live claim's token.
func TestPostgresVectorClaimTokenOfAnotherClaimSurvivesAStaleMint(t *testing.T) {
	f := newVectorClaimFixture(t, "vector-claim-survives")
	if _, err := f.mint(t, f.fence, "bearer-live"); err != nil {
		t.Fatal(err)
	}
	stale := f.fence
	stale.Token[0] ^= 0xff
	if _, err := f.mint(t, stale, "bearer-stale"); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("stale fence: %v", err)
	}
	if !f.active(t, "bearer-live") {
		t.Fatal("a stale mint revoked the live claim's token")
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

// memoryFixture is a claim of user 42 with a tenant schema holding the tables
// the personal-memory check reads, and helpers to shape the execution as an
// agent chat turn.
type memoryFixture struct {
	vectorClaimFixture
	schema string
}

func newMemoryFixture(t *testing.T, name string) memoryFixture {
	t.Helper()
	f := newVectorClaimFixture(t, name)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var projection int64
	if err := f.pool.QueryRow(ctx, `SELECT projection_project_id FROM elitea_runtime.execution_jobs WHERE execution_id = $1`, f.fence.ExecutionID).Scan(&projection); err != nil {
		t.Fatal(err)
	}
	m := memoryFixture{vectorClaimFixture: f, schema: fmt.Sprintf("p_%d", projection)}
	// The fixture database is private: its tenant schema is replaced by the
	// four tables the check reads. Actor is user 42.
	//  1: private chat, the actor alone (and an agent) -- the turn's response
	//     group is here when respond(1) is called.
	//  2: private, the actor and someone else.  3: the actor alone, PUBLIC.
	//  4: another person alone.  5: the actor alone, private, but an MCP one.
	for _, statement := range []string{
		`DROP SCHEMA IF EXISTS @S@ CASCADE`,
		`CREATE SCHEMA @S@`,
		`CREATE TABLE @S@.chat_conversations (id serial PRIMARY KEY, uuid uuid NOT NULL, is_private boolean NOT NULL DEFAULT false, source varchar(64) NOT NULL DEFAULT 'elitea')`,
		`CREATE TABLE @S@.chat_participants (id serial PRIMARY KEY, entity_name text NOT NULL, entity_meta jsonb NOT NULL)`,
		`CREATE TABLE @S@.chat_participant_mapping (conversation_id int NOT NULL, participant_id int NOT NULL)`,
		`CREATE TABLE @S@.chat_message_group (id serial PRIMARY KEY, conversation_id int NOT NULL, task_id varchar(64))`,
		`INSERT INTO @S@.chat_participants (id, entity_name, entity_meta) VALUES
		    (1, 'user', '{"id": 42}'), (2, 'user', '{"id": 43}'), (3, 'application', '{"id": 7}'), (4, 'dummy', '{}')`,
		`INSERT INTO @S@.chat_conversations (id, uuid, is_private, source) VALUES
		    (1, '11111111-1111-4111-8111-111111111111', true, 'elitea'),
		    (2, '22222222-2222-4222-8222-222222222222', true, 'elitea'),
		    (3, '33333333-3333-4333-8333-333333333333', false, 'elitea'),
		    (4, '44444444-4444-4444-8444-444444444444', true, 'elitea'),
		    (5, '55555555-5555-4555-8555-555555555555', true, 'mcp')`,
		`INSERT INTO @S@.chat_participant_mapping VALUES (1, 1), (1, 3), (1, 4), (2, 1), (2, 2), (2, 3), (3, 1), (4, 2), (5, 1), (5, 3)`,
	} {
		m.exec(t, m.tenant(statement))
	}
	return m
}

func (m memoryFixture) tenant(statement string) string {
	return strings.ReplaceAll(statement, "@S@", m.schema)
}

func (m memoryFixture) conversationUUID(id int) string {
	return fmt.Sprintf("%[1]d%[1]d%[1]d%[1]d%[1]d%[1]d%[1]d%[1]d-%[1]d%[1]d%[1]d%[1]d-4%[1]d%[1]d%[1]d-8%[1]d%[1]d%[1]d-%[1]d%[1]d%[1]d%[1]d%[1]d%[1]d%[1]d%[1]d%[1]d%[1]d%[1]d%[1]d", id)
}

// turn makes the execution an agent turn whose admission-written stream is
// `stream` ("" removes the row), started by `origin`, with its response group
// in conversation `respondIn` (0: none).
func (m memoryFixture) turn(t *testing.T, stream, origin string, respondIn int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// A bare agent_execution_jobs row (its foreign keys need the whole
	// admission kernel; the check reads two columns).
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Skipf("cannot relax foreign keys for the fixture: %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `SET session_replication_role = DEFAULT`) }()
	for _, statement := range []string{
		`DELETE FROM elitea_runtime.agent_execution_jobs WHERE execution_id = $1`,
	} {
		if _, err := conn.Exec(ctx, statement, m.fence.ExecutionID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, m.tenant(`DELETE FROM @S@.chat_message_group`)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET trigger_origin = $2 WHERE execution_id = $1`, m.fence.ExecutionID, origin); err != nil {
		t.Fatal(err)
	}
	if respondIn > 0 {
		if _, err := conn.Exec(ctx, m.tenant(`INSERT INTO @S@.chat_message_group (conversation_id, task_id) VALUES ($1, $2)`), respondIn, m.fence.ExecutionID); err != nil {
			t.Fatal(err)
		}
	}
	if stream == "" {
		return
	}
	if _, err := conn.Exec(ctx, `
INSERT INTO elitea_runtime.agent_execution_jobs
    (execution_id, generation, capability_id, input_bundle_id, request_entry_id,
     client_stream_id, client_message_id, client_execution_generation, sio_event)
VALUES ($1, $2, 'agent.execute.application.v1', 'bundle', 'entry', $3, 'm', 'g', 'chat_predict')`,
		m.fence.ExecutionID, int64(m.fence.Generation), stream); err != nil {
		t.Fatal(err)
	}
}

func (m memoryFixture) mintMemory(t *testing.T, bearer string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	minted, err := m.store.Mint(ctx, VectorClaimTokenMint{
		Fence:        m.fence,
		TokenSHA256:  sha256.Sum256([]byte(bearer)),
		Capabilities: vectorClaimCapabilities,
		Sources:      []string{"toolkit_index", "memory"},
		MaxLifetime:  6 * time.Hour,
	})
	if err != nil || !minted.Minted {
		t.Fatalf("mint: %+v %v", minted, err)
	}
}

func (m memoryFixture) facts(t *testing.T, bearer string, intent MemoryIntent) VectorClaimTokenFacts {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := m.store.Facts(ctx, sha256.Sum256([]byte(bearer)), intent)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Personal memory is allowed only for a person's own chat turn (an agent job
// started from the product, trigger_origin manual) in a PRIVATE chat
// conversation whose only user participant is the actor -- the conversation
// bound server-side (the admission's stream compared as a uuid AND the
// response group whose task_id is the execution) -- and that is re-checked at
// EVERY introspection (nothing is stored at mint). Everything else, and any
// doubt, is refused.
func TestPostgresVectorClaimTokenPersonalMemoryIsRecheckedAtEveryIntrospection(t *testing.T) {
	m := newMemoryFixture(t, "vector-claim-personal-memory")
	allowed := func(bearer string) bool {
		t.Helper()
		return m.facts(t, bearer, MemoryIntentPersonalRead).PersonalMemoryAllowed
	}

	// A direct toolkit call has no agent_execution_jobs row: not allowed.
	m.mintMemory(t, "bearer-call")
	if allowed("bearer-call") {
		t.Fatal("a call without a conversation was allowed personal memory")
	}

	m.mintMemory(t, "bearer")
	for name, tc := range map[string]struct {
		stream    string
		origin    string
		respondIn int
		allowed   bool
	}{
		"private, the actor alone and an agent": {m.conversationUUID(1), "manual", 1, true},
		"private, two people":                   {m.conversationUUID(2), "manual", 2, false},
		"public conversation":                   {m.conversationUUID(3), "manual", 3, false},
		"private, another single participant":   {m.conversationUUID(4), "manual", 4, false},
		"an MCP conversation":                   {m.conversationUUID(5), "manual", 5, false},
		"no such conversation":                  {"66666666-6666-4666-8666-666666666666", "manual", 1, false},
		"not a uuid":                            {"not-a-uuid", "manual", 1, false},
		"missing agent_execution_jobs row":      {"", "manual", 1, false},
		// The server-side binding: a stream naming the actor's private chat
		// while the turn's response is in another conversation is refused.
		"the stream names a conversation the turn did not answer in": {m.conversationUUID(1), "manual", 2, false},
		"no response group for the execution":                        {m.conversationUUID(1), "manual", 0, false},
		// Non-chat admission paths: an MCP tools/call or a PAT start (api), a
		// schedule, a webhook. Not allowed, whatever conversation they name.
		"an API or MCP start": {m.conversationUUID(1), "api", 1, false},
		"a schedule":          {m.conversationUUID(1), "schedule", 1, false},
		"a webhook":           {m.conversationUUID(1), "webhook", 1, false},
	} {
		t.Run(name, func(t *testing.T) {
			m.turn(t, tc.stream, tc.origin, tc.respondIn)
			if got := allowed("bearer"); got != tc.allowed {
				t.Fatalf("allowed = %v, want %v", got, tc.allowed)
			}
		})
	}

	t.Run("a participant added mid-run withdraws it at the next introspection", func(t *testing.T) {
		m.turn(t, m.conversationUUID(1), "manual", 1)
		if !allowed("bearer") {
			t.Fatal("precondition: allowed")
		}
		m.exec(t, m.tenant(`INSERT INTO @S@.chat_participant_mapping VALUES (1, 2)`))
		if allowed("bearer") {
			t.Fatal("still allowed after a second person joined")
		}
	})

	t.Run("a conversation made public mid-run withdraws it", func(t *testing.T) {
		m.turn(t, m.conversationUUID(1), "manual", 1)
		m.exec(t, m.tenant(`DELETE FROM @S@.chat_participant_mapping WHERE conversation_id = 1 AND participant_id = 2`))
		if !allowed("bearer") {
			t.Fatal("precondition: allowed again")
		}
		m.exec(t, m.tenant(`UPDATE @S@.chat_conversations SET is_private = false WHERE id = 1`))
		if allowed("bearer") {
			t.Fatal("still allowed after the conversation became public")
		}
		m.exec(t, m.tenant(`UPDATE @S@.chat_conversations SET is_private = true WHERE id = 1`))
	})

	t.Run("no intent, or a project intent, decides nothing personal", func(t *testing.T) {
		m.turn(t, m.conversationUUID(1), "manual", 1)
		for _, intent := range []MemoryIntent{MemoryIntentNone, MemoryIntentProjectWrite} {
			if m.facts(t, "bearer", intent).PersonalMemoryAllowed {
				t.Fatalf("intent %d answered a personal permission", intent)
			}
		}
	})

	t.Run("a failed lookup is a refusal", func(t *testing.T) {
		m.turn(t, m.conversationUUID(1), "manual", 1)
		m.exec(t, m.tenant(`DROP TABLE @S@.chat_participant_mapping`))
		if allowed("bearer") {
			t.Fatal("allowed although the lookup failed")
		}
	})

	t.Run("a token without the memory source is never asked", func(t *testing.T) {
		if _, err := m.mint(t, m.fence, "bearer-index-only"); err != nil {
			t.Fatal(err)
		}
		if allowed("bearer-index-only") {
			t.Fatal("an index-only token carries personal memory")
		}
	})
}

// The cross-scope rule (owner decision 2026-10-14): once a run has READ
// personal memory, its project-memory WRITES are refused, from any claim
// token of the execution. A personal write is not a read; a project write
// before any personal read is allowed; another run is unaffected.
func TestPostgresVectorClaimTokenProjectWriteIsRefusedAfterAPersonalRead(t *testing.T) {
	m := newMemoryFixture(t, "vector-claim-cross-scope")
	m.turn(t, m.conversationUUID(1), "manual", 1)
	m.mintMemory(t, "bearer")
	refused := func(bearer string) bool {
		t.Helper()
		return m.facts(t, bearer, MemoryIntentProjectWrite).ProjectMemoryWriteRefused
	}

	if refused("bearer") {
		t.Fatal("a project write before any personal read was refused")
	}
	if !m.facts(t, "bearer", MemoryIntentPersonalWrite).PersonalMemoryAllowed {
		t.Fatal("precondition: the personal write is allowed")
	}
	if refused("bearer") {
		t.Fatal("a personal WRITE counted as a read")
	}
	// A personal read that is NOT allowed (the conversation is shared) reads
	// nothing, so it does not mark the run.
	m.exec(t, m.tenant(`INSERT INTO @S@.chat_participant_mapping VALUES (1, 2)`))
	if m.facts(t, "bearer", MemoryIntentPersonalRead).PersonalMemoryAllowed {
		t.Fatal("precondition: refused in a shared conversation")
	}
	if refused("bearer") {
		t.Fatal("a refused personal read marked the run")
	}
	m.exec(t, m.tenant(`DELETE FROM @S@.chat_participant_mapping WHERE conversation_id = 1 AND participant_id = 2`))

	// The read marks the run.
	if !m.facts(t, "bearer", MemoryIntentPersonalRead).PersonalMemoryAllowed {
		t.Fatal("precondition: the personal read is allowed")
	}
	if !refused("bearer") {
		t.Fatal("a project write after a personal read was allowed")
	}
	// A project READ is unaffected (no decision is made for it).
	if got := m.facts(t, "bearer", MemoryIntentNone); got.ProjectMemoryWriteRefused {
		t.Fatalf("a non-write intent answered refused: %+v", got)
	}

	// Another token of the SAME execution (a later claim's re-mint keeps the
	// row; a second row of the execution is what a takeover leaves) is refused
	// too: the flag is read across the execution's rows.
	m.exec(t, `UPDATE elitea_runtime.vector_claim_tokens SET token_sha256 = $1 WHERE execution_id = $2`,
		func() []byte { h := sha256.Sum256([]byte("bearer-after-remint")); return h[:] }(), m.fence.ExecutionID)
	m.exec(t, `UPDATE elitea_runtime.vector_claim_tokens SET personal_memory_read = false WHERE execution_id = $1`, m.fence.ExecutionID)
	if refused("bearer-after-remint") {
		t.Fatal("precondition: the flag was cleared")
	}
	m.exec(t, `UPDATE elitea_runtime.vector_claim_tokens SET personal_memory_read = true WHERE execution_id = $1`, m.fence.ExecutionID)
	if !refused("bearer-after-remint") {
		t.Fatal("the flag of the execution's row was not read")
	}

	// A separate run is unaffected.
	other := newMemoryFixture(t, "vector-claim-cross-scope-other")
	other.turn(t, other.conversationUUID(1), "manual", 1)
	other.mintMemory(t, "bearer-other")
	if other.facts(t, "bearer-other", MemoryIntentProjectWrite).ProjectMemoryWriteRefused {
		t.Fatal("another run's project write was refused")
	}
}

// A personal read and a project write of ONE run asked at the same time are
// decided one after the other (both serialise on the execution's advisory
// lock): whichever order the database picks, a write decided after the read
// was recorded is refused, and no write is ever admitted after the read was.
func TestPostgresVectorClaimTokenParallelReadAndWriteAreSerialised(t *testing.T) {
	m := newMemoryFixture(t, "vector-claim-cross-scope-parallel")
	m.turn(t, m.conversationUUID(1), "manual", 1)
	m.mintMemory(t, "bearer")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Hold the execution's lock so both requests queue on it.
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, memoryRunLock, m.fence.ExecutionID); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		facts VectorClaimTokenFacts
		err   error
		at    time.Time
	}
	read := make(chan answer, 1)
	write := make(chan answer, 1)
	hash := sha256.Sum256([]byte("bearer"))
	go func() {
		got, err := m.store.Facts(ctx, hash, MemoryIntentPersonalRead)
		read <- answer{got, err, time.Now()}
	}()
	time.Sleep(200 * time.Millisecond)
	go func() {
		got, err := m.store.Facts(ctx, hash, MemoryIntentProjectWrite)
		write <- answer{got, err, time.Now()}
	}()
	time.Sleep(200 * time.Millisecond)
	select {
	case <-read:
		t.Fatal("the read was decided while the execution was locked")
	case <-write:
		t.Fatal("the write was decided while the execution was locked")
	default:
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r, w := <-read, <-write
	if r.err != nil || w.err != nil {
		t.Fatalf("read %v, write %v", r.err, w.err)
	}
	if !r.facts.PersonalMemoryAllowed {
		t.Fatal("precondition: the read is allowed")
	}
	// The read queued first, so it was recorded first: the write is refused.
	if !w.facts.ProjectMemoryWriteRefused {
		t.Fatal("a project write decided after a personal read was allowed")
	}
}

// Main adds the `memory` source to a claim token only when the claim's frozen
// toolkits include a memory toolkit, so an older elitea-vector is never shown
// a source it cannot name and a run without a memory toolkit has no memory
// reach.
func TestPostgresVectorClaimTokenMemorySourceOnlyForAMemoryToolkit(t *testing.T) {
	f := newVectorClaimFixture(t, "vector-claim-memory-source")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mint := func(bearer string) []string {
		t.Helper()
		minted, err := f.store.Mint(ctx, VectorClaimTokenMint{
			Fence:        f.fence,
			TokenSHA256:  sha256.Sum256([]byte(bearer)),
			Capabilities: vectorClaimCapabilities,
			Sources:      []string{"toolkit_index"},
			MemorySource: "memory",
			MaxLifetime:  6 * time.Hour,
		})
		if err != nil || !minted.Minted {
			t.Fatalf("mint: %+v %v", minted, err)
		}
		return minted.Sources
	}
	if got := mint("bearer-none"); !slices.Equal(got, []string{"toolkit_index"}) {
		t.Fatalf("no memory toolkit: sources %v", got)
	}
	// Freeze a memory toolkit into the execution's input bundle.
	content := []byte(`{"version_details": {"tools": [{"id": 9, "type": "memory", "toolkit_name": "notes", "settings": {"memory_scope": "personal"}}]}}`)
	digest := sha256.Sum256(content)
	f.exec(t, `
INSERT INTO elitea_runtime.input_bundle_entries (
    input_bundle_id, entry_id, entry_version, semantic_role, media_type,
    content_digest, content_size, content_reference, classification,
    required_grant_audience, content_bytes)
SELECT input_bundle_id, 'agent-request', 'v1', 'agent.request', 'application/json',
       $2, $3, 'content://memory-test', 'internal', 'elitea-main', $4
FROM elitea_runtime.execution_jobs WHERE execution_id = $1`,
		f.fence.ExecutionID, digest[:], int64(len(content)), content)
	if got := mint("bearer-memory"); !slices.Equal(got, []string{"toolkit_index", "memory"}) {
		t.Fatalf("a memory toolkit: sources %v", got)
	}
	// The token carries the claim's memory toolkits (all of its project), and
	// elitea-vector refuses any other toolkit id.
	facts, err := f.store.Facts(ctx, sha256.Sum256([]byte("bearer-memory")), MemoryIntentNone)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(facts.MemoryToolkits, []int64{9}) {
		t.Fatalf("memory toolkits = %v, want [9]", facts.MemoryToolkits)
	}
	// A "memory" word that is not a tool entry does not count, and neither
	// does a memory entry without a positive integer id.
	found := map[int64]bool{}
	collectMemoryToolkits(map[string]any{
		"meta":  map[string]any{"type": "memory"},
		"tools": []any{map[string]any{"type": "memory", "settings": map[string]any{}, "id": 1.5}, map[string]any{"type": "memory", "settings": map[string]any{}, "id": -2.0}, map[string]any{"type": "memory", "id": 4.0}},
	}, found)
	if len(found) != 0 {
		t.Fatalf("non-entries counted: %v", found)
	}
}
