package repos

// End to end for the memory toolkits of a claim token (security review 4,
// item 1): a REAL agent admission (the protobuf input bundle the chat start
// builds, through AdmissionService) with a memory toolkit in its frozen
// toolkit set, claimed through the production claim path, minted through the
// issuer's store: the token carries the memory source and exactly that
// toolkit id. Requires ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// admitAgentRun admits one agent execution through the application admission
// service (input bundle factory, request digest, memory toolkit computation)
// and the Postgres store, then claims it through the production claim path.
func admitAgentRun(t *testing.T, pool *pgxpool.Pool, suffix, capability string, input *runtimev1.AgentExecutionInputV1) (string, runtimedomain.Fence) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	policy := testAgentDispatchPolicy()
	policy.MaxOutstanding = 16
	store, err := NewAgentExecutionJobsRepository(pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}
	counter := 0
	newID := func() (string, error) {
		counter++
		return fmt.Sprintf("m4-%s-%02d", suffix, counter), nil
	}
	factory, err := agentexecutionapp.NewInputBundleFactory(agentexecutionapp.InputProfile{
		Classification: "tenant-confidential", RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, newID)
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentexecutionapp.NewAdmissionService(store, factory, time.Now, newID)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := service.Submit(ctx, agentexecutionapp.SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: "1", ResourceProjectID: "1", ProjectionProjectID: "1", ActorID: "42",
		},
		IdempotencyKey:  "m4-" + suffix,
		CapabilityID:    capability,
		ClientStreamID:  "11111111-1111-4111-8111-111111111111",
		ClientMessageID: "m4-message-" + suffix,
		SIOEvent:        "chat_predict",
		Input:           input,
	})
	if err != nil || !outcome.Created {
		t.Fatalf("admit: %+v %v", outcome, err)
	}

	// Publish and dispatch, as the outbox publisher does, then claim.
	envelope := []byte("signed-agent-command:" + outcome.CommandID)
	digest := runtimedomain.SHA256(envelope)
	var outboxID string
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
UPDATE elitea_runtime.command_outbox
SET prepared_signed_envelope_bytes = $2, prepared_signed_envelope_digest = $3,
    prepared_signature_profile = 1, prepared_key_id = 'm4-key', prepared_at = clock_timestamp(),
    published_at = clock_timestamp(), last_visibility_at = clock_timestamp(),
    published_envelope_digest = $3, publish_attempts = 1
WHERE execution_id = $1 RETURNING outbox_id`, outcome.ExecutionID, envelope, digest[:]).Scan(&outboxID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET state = 'DISPATCHED' WHERE execution_id = $1`, outcome.ExecutionID)
		return err
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.workload_sessions (workload_session_id, workload_identity, producer_id, issued_at, expires_at)
VALUES ($1, 'spiffe://elitea.test/agent-worker/1', 'agent-worker-1', clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 day')
ON CONFLICT DO NOTHING`, "agent-worker-session-"+suffix); err != nil {
		t.Fatal(err)
	}
	claims, err := NewClaimsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	claimService, err := executionapp.NewClaimService(claims, time.Now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := claimService.Claim(ctx, executionapp.ClaimRequest{
		CommandID: outcome.CommandID, OutboxID: outboxID, ExecutionID: outcome.ExecutionID,
		Generation: 1, CapabilityID: capability, SignedEnvelopeDigest: digest,
		WorkloadIdentity:  "spiffe://elitea.test/agent-worker/1",
		WorkloadSessionID: "agent-worker-session-" + suffix, ProducerID: "agent-worker-1",
	})
	if err != nil || decision.Disposition != executionapp.ClaimAccepted {
		t.Fatalf("claim: %+v %v", decision, err)
	}
	return outcome.ExecutionID, decision.Lease.Fence
}

func memoryAgentInput(application, tools string) *runtimev1.AgentExecutionInputV1 {
	input := agentCapacityInput("gen-m4")
	input.Application = []byte(application)
	input.Tools = []byte(tools)
	// Caller-supplied content that names a memory toolkit: never read.
	input.UserInput = []byte(`"{\"type\":\"memory\",\"id\":77,\"settings\":{}}"`)
	input.ChatHistory = []byte(`[{"role":"user","content":{"type":"memory","id":78,"settings":{}}}]`)
	return input
}

func TestPostgresAgentAdmissionGivesTheClaimTokenItsFrozenMemoryToolkits(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := NewVectorClaimTokens(pool)
	mint := func(fence runtimedomain.Fence, bearer string) VectorClaimTokenMinted {
		t.Helper()
		minted, err := store.Mint(ctx, VectorClaimTokenMint{
			Fence: fence, TokenSHA256: sha256.Sum256([]byte(bearer)),
			Capabilities: vectorClaimCapabilities, Sources: []string{"toolkit_index"},
			MemorySource: "memory", MaxLifetime: 6 * time.Hour,
		})
		if err != nil || !minted.Minted {
			t.Fatalf("mint: %+v %v", minted, err)
		}
		return minted
	}
	toolkits := func(bearer string) []int64 {
		t.Helper()
		facts, err := store.Facts(ctx, sha256.Sum256([]byte(bearer)), MemoryIntentNone)
		if err != nil {
			t.Fatal(err)
		}
		return facts.MemoryToolkits
	}

	// An application chat turn whose frozen version attaches memory toolkit 9
	// (and a github toolkit); a nested settings object that says "memory"
	// does not count.
	execution, fence := admitAgentRun(t, pool, "app", executiondomain.AgentApplicationCapability, memoryAgentInput(
		`{"id":7,"version_id":9,"version_details":{"tools":[
		    {"id":9,"type":"memory","toolkit_name":"notes","settings":{"memory_scope":"personal"}},
		    {"id":12,"type":"github","toolkit_name":"gh","settings":{"nested":{"type":"memory","id":13,"settings":{}}}}
		]}}`, `[]`))
	var stored []int64
	if err := pool.QueryRow(ctx, `SELECT memory_toolkit_ids FROM elitea_runtime.execution_jobs WHERE execution_id = $1`, execution).Scan(&stored); err != nil ||
		!slices.Equal(stored, []int64{9}) {
		t.Fatalf("admitted memory toolkits = %v, %v; want [9]", stored, err)
	}
	minted := mint(fence, "bearer-app")
	if !slices.Equal(minted.Sources, []string{"toolkit_index", "memory"}) || !slices.Equal(toolkits("bearer-app"), []int64{9}) {
		t.Fatalf("application turn: sources %v, toolkits %v", minted.Sources, toolkits("bearer-app"))
	}
	// A re-mint copies the same ids (no bundle decoding).
	if again := mint(fence, "bearer-app-2"); !slices.Equal(again.Sources, minted.Sources) || !slices.Equal(toolkits("bearer-app-2"), []int64{9}) {
		t.Fatalf("re-mint: %v %v", again.Sources, toolkits("bearer-app-2"))
	}

	// An ad-hoc turn: the input's own top-level tools.
	_, adhoc := admitAgentRun(t, pool, "adhoc", executiondomain.AgentAdhocCapability, memoryAgentInput(
		`{"id":7,"version_id":9}`,
		`[{"id":31,"type":"memory","toolkit_name":"team","settings":{"memory_scope":"project"}},{"id":31,"type":"memory","toolkit_name":"team","settings":{}}]`))
	if got := mint(adhoc, "bearer-adhoc"); !slices.Equal(got.Sources, []string{"toolkit_index", "memory"}) || !slices.Equal(toolkits("bearer-adhoc"), []int64{31}) {
		t.Fatalf("ad-hoc turn: sources %v toolkits %v", got.Sources, toolkits("bearer-adhoc"))
	}

	// No memory toolkit attached (only caller text naming one): no memory.
	_, plain := admitAgentRun(t, pool, "plain", executiondomain.AgentApplicationCapability, memoryAgentInput(
		`{"id":7,"version_id":9,"version_details":{"tools":[]}}`, `[]`))
	if got := mint(plain, "bearer-plain"); !slices.Equal(got.Sources, []string{"toolkit_index"}) || len(toolkits("bearer-plain")) != 0 {
		t.Fatalf("no memory toolkit: sources %v toolkits %v", got.Sources, toolkits("bearer-plain"))
	}
}

// A direct toolkit call is given its RESOLVED toolkit (type and id from the
// project's toolkit row) when that is a memory toolkit; a call to another
// toolkit whose ARGUMENTS name a memory toolkit adds nothing.
func TestPostgresCallToolAdmissionGivesOnlyTheResolvedMemoryToolkit(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repository, err := NewToolkitCallToolJobsRepository(pool, toolRunDispatchPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admittedToolkits := func(executionID string) []int64 {
		t.Helper()
		var ids []int64
		if err := pool.QueryRow(ctx, `SELECT memory_toolkit_ids FROM elitea_runtime.execution_jobs WHERE execution_id = $1`, executionID).Scan(&ids); err != nil {
			t.Fatal(err)
		}
		return ids
	}

	crafted := newToolRunAdmissionService(t, repository, "crafted")
	github, err := crafted.Submit(ctx, toolRunSubmitRequest("m4-crafted", `{"type":"memory","id":42,"settings":{}}`))
	if err != nil || !github.Outcome.Created {
		t.Fatalf("admit: %+v %v", github.Outcome, err)
	}
	if got := admittedToolkits(github.Outcome.ExecutionID); got == nil || len(got) != 0 {
		t.Fatalf("arguments naming a memory toolkit gave %v", got)
	}

	request := toolRunSubmitRequest("m4-memory", `{"key":"k"}`)
	request.Inputs.ToolkitType = "memory"
	request.Inputs.ToolkitID = 23
	request.Inputs.ToolName = "get_memory"
	request.Inputs.Settings = json.RawMessage(`{"id":23,"type":"memory","toolkit_name":"notes","settings":{}}`)
	memory, err := newToolRunAdmissionService(t, repository, "memory").Submit(ctx, request)
	if err != nil || !memory.Outcome.Created {
		t.Fatalf("admit memory: %+v %v", memory.Outcome, err)
	}
	if got := admittedToolkits(memory.Outcome.ExecutionID); !slices.Equal(got, []int64{23}) {
		t.Fatalf("a memory toolkit call gave %v, want [23]", got)
	}
}
