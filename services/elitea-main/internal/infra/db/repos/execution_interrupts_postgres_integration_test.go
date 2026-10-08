package repos

// The per-interrupt HITL decision ledger against a real PostgreSQL
// (fanout-interrupt-decisions-v1 §5-§7, Track M2). Requires
// ELITEA_TEST_DATABASE_URL; every test skips without it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
)

const interruptContractFixtures = "../../../../../../libs/jsonschema/runtime/v1/fixtures"

// Principals. 7 owns the conversation; 8 asked the question; 9 is a project
// member outside the conversation; 10 is a member of project 2 only; 11 is a
// suspended participant.
const (
	interruptOwner     = int64(7)
	interruptAsker     = int64(8)
	interruptOutsider  = int64(9)
	interruptForeigner = int64(10)
	interruptSuspended = int64(11)
)

type interruptHarness struct {
	t              *testing.T
	pool           *pgxpool.Pool
	repo           *ExecutionInterruptRepository
	executions     map[string]string
	conversationID int64
	responses      []string
	sequence       int
}

func newInterruptHarness(t *testing.T) *interruptHarness {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	repo, err := NewExecutionInterruptRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	h := &interruptHarness{t: t, pool: pool, repo: repo, executions: map[string]string{}}
	h.seedPrincipals()
	h.conversationID = h.seedConversation()
	return h
}

func (h *interruptHarness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(h.t.Context(), query, args...); err != nil {
		h.t.Fatalf("seed %q: %v", strings.SplitN(strings.TrimSpace(query), "\n", 2)[0], err)
	}
}

func (h *interruptHarness) seedPrincipals() {
	// The auth tables are owned by 001_initial.sql, outside the ledgered
	// corpus; create them in that shape (auth_core_baseline.sql).
	seedAuthCoreTables(h.t, h.pool)
	h.exec(`
CREATE TABLE IF NOT EXISTS public.auth_core__role (
    id serial PRIMARY KEY, name varchar(64) NOT NULL, mode varchar(64) NOT NULL, UNIQUE (name, mode));
CREATE TABLE IF NOT EXISTS public.auth_core__role_permission (
    id serial PRIMARY KEY, role_id integer NOT NULL REFERENCES public.auth_core__role (id) ON DELETE CASCADE,
    permission varchar(64), UNIQUE (role_id, permission));
CREATE TABLE IF NOT EXISTS public.auth_core__project_role_permission (
    id serial PRIMARY KEY, project_id integer NOT NULL,
    role_id integer REFERENCES public.auth_core__project_role (id) ON UPDATE CASCADE ON DELETE CASCADE,
    permission text NOT NULL, UNIQUE (project_id, role_id, permission))`)
	h.exec(`INSERT INTO centry.project (id, create_success, suspended) VALUES (2, TRUE, FALSE)`)
	h.exec(`INSERT INTO public.auth_core__user (id, email, name, suspended) VALUES
 (7, 'owner@example.com', 'Owner', false), (8, 'asker@example.com', 'Asker', false),
 (9, 'outsider@example.com', 'Outsider', false), (10, 'foreigner@example.com', 'Foreigner', false),
 (11, 'suspended@example.com', 'Suspended', true)
 ON CONFLICT (id) DO UPDATE SET suspended = EXCLUDED.suspended`)
	h.exec(`INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES (9101, 1, 'm2_chat'), (9102, 2, 'm2_chat')`)
	h.exec(`INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
SELECT role.project_id, role.id, permission.name
FROM public.auth_core__project_role role
CROSS JOIN (VALUES ('models.chat.messages.create'), ('models.applications.task.get')) AS permission(name)
WHERE role.id IN (9101, 9102)`)
	h.exec(`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES
 (1, 7, 9101), (1, 8, 9101), (1, 9, 9101), (1, 11, 9101), (2, 10, 9102), (2, 7, 9102)`)
	// Project 2 has a tenant schema with no conversations, so a project-1
	// response cannot be reached through it.
	h.exec(`CREATE SCHEMA p_2`)
	for _, table := range []string{"chat_conversations", "chat_participants", "chat_participant_mapping", "chat_message_group"} {
		h.exec(fmt.Sprintf(`CREATE TABLE p_2.%[1]s (LIKE p_1.%[1]s INCLUDING ALL)`, table))
	}
}

func (h *interruptHarness) seedConversation() int64 {
	h.t.Helper()
	var conversationID int64
	if err := h.pool.QueryRow(h.t.Context(), `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source)
VALUES (gen_random_uuid(), 'interrupts', 7, 'agent') RETURNING id`).Scan(&conversationID); err != nil {
		h.t.Fatal(err)
	}
	for _, user := range []int64{interruptOwner, interruptAsker, interruptSuspended} {
		h.exec(`
WITH participant AS (
  INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
  VALUES (gen_random_uuid(), 'user', jsonb_build_object('id', $2::bigint)) RETURNING id)
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id) SELECT $1, id FROM participant`, conversationID, user)
	}
	h.exec(`
WITH participant AS (
  INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
  VALUES (gen_random_uuid(), 'application', '{"id": 1}'::jsonb) RETURNING id)
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id) SELECT $1, id FROM participant`, conversationID)
	return conversationID
}

// newResponse seeds a question by user 8, the agent's response to it, and the
// running execution bound to that response.
func (h *interruptHarness) newResponse() string {
	h.t.Helper()
	var responseID string
	if err := h.pool.QueryRow(h.t.Context(), `
WITH asker AS (
  SELECT participant.id FROM p_1.chat_participants participant
  JOIN p_1.chat_participant_mapping mapping ON mapping.participant_id = participant.id AND mapping.conversation_id = $1
  WHERE participant.entity_name = 'user' AND participant.entity_meta->>'id' = '8'
), agent AS (
  SELECT participant.id FROM p_1.chat_participants participant
  JOIN p_1.chat_participant_mapping mapping ON mapping.participant_id = participant.id AND mapping.conversation_id = $1
  WHERE participant.entity_name = 'application'
), question AS (
  INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id)
  SELECT gen_random_uuid(), asker.id, $1 FROM asker RETURNING id
)
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, reply_to_id, is_streaming)
SELECT gen_random_uuid(), agent.id, $1, question.id, true FROM agent, question
RETURNING uuid::text`, h.conversationID).Scan(&responseID); err != nil {
		h.t.Fatal(err)
	}
	h.responses = append(h.responses, responseID)
	sum := sha256.Sum256([]byte(h.t.Name() + responseID))
	executionID := hex.EncodeToString(sum[:16])
	h.seedExecution(executionID)
	h.exec(`
INSERT INTO elitea_runtime.agent_execution_jobs (
    execution_id, generation, capability_id, input_bundle_id, request_entry_id,
    client_stream_id, client_message_id, client_execution_generation, sio_event
) VALUES ($1, 1, 'agent.execute.application.v1', $2, 'request', 'stream-1', $3, '1', 'chat_predict')`,
		executionID, "bundle-"+executionID, responseID)
	h.executions[responseID] = executionID
	return responseID
}

func (h *interruptHarness) seedExecution(executionID string) {
	h.exec(`
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by, created_at
) VALUES ($1, '1', 'application/x-protobuf', 1, decode(repeat('ab', 32), 'hex'), 2, '{}'::bytea, 'tests', clock_timestamp())
ON CONFLICT (input_bundle_id) DO NOTHING`, "bundle-"+executionID)
	h.exec(`
INSERT INTO elitea_runtime.input_bundle_entries (
    input_bundle_id, entry_id, entry_version, semantic_role, media_type,
    content_digest, content_size, content_reference, classification,
    required_grant_audience, content_bytes
) VALUES ($1, 'request', '1', 'request', 'application/json', decode(repeat('cd', 32), 'hex'), 2,
    'inline://request', 'internal', 'worker', '{}'::bytea)
ON CONFLICT (input_bundle_id, entry_id) DO NOTHING`, "bundle-"+executionID)
	h.exec(`
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state, admitted_at
) VALUES ($1, 1, $2, '1', 1, 1, '8', 'user:8', 'agent.execute.application.v1',
    '1', $3, decode(repeat('ef', 32), 'hex'), 'execution-interrupts', $4, 'RUNNING', 'RUNNING', clock_timestamp())`,
		executionID, "command-"+executionID, "bundle-"+executionID, "idempotency-"+executionID)
}

// bindLiveClaim gives the response's execution a live, granted command, a
// current workload session and an ordinary claim.
func (h *interruptHarness) bindLiveClaim(responseID string) domain.ClaimFence {
	h.t.Helper()
	executionID := h.executions[responseID]
	fence := domain.ClaimFence{
		ClaimID: "claim-" + executionID, ExecutionID: executionID, Generation: 1,
		WorkloadIdentity: "spiffe://elitea.test/runtime/rust-worker", FenceToken: []byte(strings.Repeat("f", 32)),
	}
	envelope := []byte("envelope-" + executionID)
	digest := sha256.Sum256(envelope)
	h.exec(`
INSERT INTO elitea_runtime.command_outbox (
    outbox_id, execution_id, generation, stream_name, resource_class,
    isolation_class, priority, deadline, limits_revision,
    prepared_signed_envelope_bytes, prepared_signed_envelope_digest,
    prepared_signature_profile, prepared_key_id, prepared_at,
    published_at, last_visibility_at, published_envelope_digest, authority_granted_at, publish_attempts
) VALUES ($1, $2, 1, 'elitea:runtime:commands', 'agent', 'project', 1, clock_timestamp() + interval '1 hour', 'limits-v1',
          $3, $4, 1, 'test-key', clock_timestamp(), clock_timestamp(), clock_timestamp(), $4, clock_timestamp(), 1)`,
		"outbox-"+executionID, executionID, envelope, digest[:])
	h.exec(`
INSERT INTO elitea_runtime.workload_sessions (workload_session_id, workload_identity, producer_id, issued_at, expires_at)
VALUES ($1, $2, 'rust-worker-1', clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour')`,
		"session-"+executionID, fence.WorkloadIdentity)
	h.exec(`
INSERT INTO elitea_runtime.execution_claims (
    claim_id, execution_id, generation, workload_session_id, workload_identity, producer_id,
    claim_attempt, lease_epoch, fence_token, claimed_at, lease_expires_at
) VALUES ($1, $2, 1, $3, $4, 'rust-worker-1', 1, 1, $5, clock_timestamp(), clock_timestamp() + interval '10 minutes')`,
		fence.ClaimID, executionID, "session-"+executionID, fence.WorkloadIdentity, fence.FenceToken)
	return fence
}

// card returns a canonical card from the contract fixture with a fresh key
// and interrupt id.
func (h *interruptHarness) card(fixture string, mutate func(map[string]any)) (key string, raw []byte) {
	h.t.Helper()
	h.sequence++
	body, err := os.ReadFile(filepath.Join(interruptContractFixtures, fixture))
	if err != nil {
		h.t.Fatal(err)
	}
	var card map[string]any
	if err := json.Unmarshal(body, &card); err != nil {
		h.t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", h.t.Name(), h.sequence)))
	key = hex.EncodeToString(sum[:])
	card["interrupt_key"] = key
	card["interrupt_id"] = fmt.Sprintf("hitl:%d", h.sequence)
	if mutate != nil {
		mutate(card)
	}
	encoded, err := json.Marshal(card)
	if err != nil {
		h.t.Fatal(err)
	}
	raw, err = domain.Canonicalize(encoded, domain.MaxCardBytes)
	if err != nil {
		h.t.Fatal(err)
	}
	return key, raw
}

func (h *interruptHarness) raiseInput(responseID string, raw []byte) domain.RaiseInput {
	h.sequence++
	return domain.RaiseInput{
		ProjectID: 1, RootResponseID: responseID, ExecutionID: h.executions[responseID], Generation: 1,
		SourceEventID: fmt.Sprintf("event-%s-%d", h.t.Name(), h.sequence), SourceClaimID: "claim-raise",
		Frontier: domain.Frontier{ChildThread: "child-thread-1", FanoutNode: "research", Ordinal: 1},
		Card:     raw,
	}
}

func (h *interruptHarness) raise(responseID, fixture string) string {
	h.t.Helper()
	key, raw := h.card(fixture, nil)
	if _, err := h.repo.Raise(h.t.Context(), h.raiseInput(responseID, raw)); err != nil {
		h.t.Fatalf("raise: %v", err)
	}
	return key
}

func decisionBody(t *testing.T, requestSeed, action, value string, credentialRef string) (domain.Decision, []byte) {
	t.Helper()
	sum := sha256.Sum256([]byte(requestSeed))
	body := map[string]any{"request_id": hex.EncodeToString(sum[:]), "expected_revision": 1, "action": action, "value": value}
	if credentialRef != "" {
		body["credential_ref"] = credentialRef
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	decision, canonical, err := domain.ParseDecisionRequest(encoded)
	if err != nil {
		t.Fatalf("decision fixture: %v", err)
	}
	return decision, canonical
}

func (h *interruptHarness) decide(actor int64, responseID, key, requestSeed, action, value string) (domain.DecideResult, error) {
	_, canonical := decisionBody(h.t, requestSeed, action, value, "")
	return h.repo.Decide(h.t.Context(), domain.DecideInput{
		Selector:     domain.Selector{ProjectID: 1, ActorUserID: actor, ResponseMessageID: responseID},
		InterruptKey: key, Canonical: canonical,
	})
}

type interruptRow struct {
	state, requestID string
	revision         int64
}

func (h *interruptHarness) row(responseID, key string) interruptRow {
	h.t.Helper()
	var row interruptRow
	var requestID *string
	if err := h.pool.QueryRow(h.t.Context(), `SELECT state, revision, request_id FROM elitea_runtime.execution_interrupts
WHERE root_response_id = $1::uuid AND interrupt_key = $2`, responseID, key).Scan(&row.state, &row.revision, &requestID); err != nil {
		h.t.Fatal(err)
	}
	if requestID != nil {
		row.requestID = *requestID
	}
	return row
}

func (h *interruptHarness) audit(responseID, key string) []string {
	h.t.Helper()
	rows, err := h.pool.Query(h.t.Context(), `
SELECT transition || ':' || revision || ':' || COALESCE(actor_id::text, 'claim=' || claim_id)
FROM elitea_runtime.execution_interrupt_audit WHERE root_response_id = $1::uuid AND interrupt_key = $2
ORDER BY revision, created_at`, responseID, key)
	if err != nil {
		h.t.Fatal(err)
	}
	transitions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		h.t.Fatal(err)
	}
	return transitions
}

func (h *interruptHarness) decisionRevision(responseID string) int64 {
	h.t.Helper()
	var revision int64
	if err := h.pool.QueryRow(h.t.Context(), `SELECT decision_revision FROM elitea_runtime.execution_interrupt_responses
WHERE root_response_id = $1::uuid`, responseID).Scan(&revision); err != nil {
		h.t.Fatal(err)
	}
	return revision
}

func TestRaiseReplayIdenticalIsNoopAndDifferentBytesFault(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key, raw := h.card("fanout-interrupt-card-v1.json", nil)
	input := h.raiseInput(response, raw)
	first, err := h.repo.Raise(t.Context(), input)
	if err != nil || first.Replay || first.InterruptKey != key {
		t.Fatalf("first raise = %+v, %v", first, err)
	}
	// A replayed frame, and a re-raise from a worker restart under a new
	// event id, are both byte-identical no-ops.
	for _, eventID := range []string{input.SourceEventID, "event-after-restart"} {
		replay := input
		replay.SourceEventID = eventID
		got, err := h.repo.Raise(t.Context(), replay)
		if err != nil || !got.Replay {
			t.Fatalf("replay under %s = %+v, %v", eventID, got, err)
		}
	}
	_, changed := h.card("fanout-interrupt-card-v1.json", func(card map[string]any) {
		card["interrupt_key"] = key
		card["interrupt_id"] = "hitl:1"
		card["display"].(map[string]any)["message"] = "Different text under the same key"
	})
	different := input
	different.Card = changed
	if _, err := h.repo.Raise(t.Context(), different); !errors.Is(err, domain.ErrRaiseConflict) {
		t.Fatalf("different card bytes: %v, want ErrRaiseConflict", err)
	}
	moved := input
	moved.Frontier.ChildThread = "another-child"
	if _, err := h.repo.Raise(t.Context(), moved); !errors.Is(err, domain.ErrRaiseConflict) {
		t.Fatalf("different frontier: %v, want ErrRaiseConflict", err)
	}
	// A new key reusing an accepted source event id is refused, as is a new
	// key reusing an open interrupt id.
	_, other := h.card("fanout-interrupt-card-v1.json", nil)
	reused := h.raiseInput(response, other)
	reused.SourceEventID = input.SourceEventID
	if _, err := h.repo.Raise(t.Context(), reused); !errors.Is(err, domain.ErrRaiseConflict) {
		t.Fatalf("reused source event id: %v", err)
	}
	_, clash := h.card("fanout-interrupt-card-v1.json", func(card map[string]any) { card["interrupt_id"] = "hitl:1" })
	if _, err := h.repo.Raise(t.Context(), h.raiseInput(response, clash)); !errors.Is(err, domain.ErrRaiseConflict) {
		t.Fatalf("open interrupt id clash: %v", err)
	}
	if got := h.row(response, key); got.state != "PENDING" || got.revision != 1 {
		t.Fatalf("row after conflicts = %+v", got)
	}
	if got := h.audit(response, key); len(got) != 1 || got[0] != "RAISED:1:claim=claim-raise" {
		t.Fatalf("audit = %v", got)
	}
	var count int
	if err := h.pool.QueryRow(t.Context(), `SELECT count(*) FROM elitea_runtime.execution_interrupts WHERE root_response_id = $1::uuid`, response).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows = %d, %v", count, err)
	}
}

func TestRaiseRefusesInvalidInputBeforeAnyWrite(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	_, raw := h.card("fanout-interrupt-card-v1.json", nil)
	for name, mutate := range map[string]func(*domain.RaiseInput){
		"invalid card":       func(in *domain.RaiseInput) { in.Card = []byte(`{"schema":"x"}`) },
		"unknown response":   func(in *domain.RaiseInput) { in.RootResponseID = "10000000-0000-4000-8000-000000000099" },
		"uuid execution id":  func(in *domain.RaiseInput) { in.ExecutionID = response },
		"empty event id":     func(in *domain.RaiseInput) { in.SourceEventID = "" },
		"oversized event id": func(in *domain.RaiseInput) { in.SourceEventID = strings.Repeat("e", domain.MaxSourceEventIDBytes+1) },
		"bad frontier":       func(in *domain.RaiseInput) { in.Frontier.ChildThread = "" },
	} {
		input := h.raiseInput(response, raw)
		mutate(&input)
		if _, err := h.repo.Raise(t.Context(), input); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var count int
	if err := h.pool.QueryRow(t.Context(), `SELECT count(*) FROM elitea_runtime.execution_interrupts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rows after refusals = %d, %v", count, err)
	}
}

func TestRaiseCapAtSixteenAndSeventeen(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	keys := make([]string, 0, domain.MaxOpenInterrupts)
	for range domain.MaxOpenInterrupts {
		keys = append(keys, h.raise(response, "fanout-interrupt-card-v1.json"))
	}
	_, raw := h.card("fanout-interrupt-card-v1.json", nil)
	if _, err := h.repo.Raise(t.Context(), h.raiseInput(response, raw)); !errors.Is(err, domain.ErrCapReached) {
		t.Fatalf("17th open card: %v, want ErrCapReached", err)
	}
	// DECIDED still counts as open (user decision 2026-10-08).
	if _, err := h.decide(interruptOwner, response, keys[0], "cap-decide", "approve", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.Raise(t.Context(), h.raiseInput(response, raw)); !errors.Is(err, domain.ErrCapReached) {
		t.Fatalf("17th card with one DECIDED: %v", err)
	}
	// Closing frees the slots; another response is independent.
	if _, err := h.repo.Raise(t.Context(), h.raiseInput(h.newResponse(), raw)); err != nil {
		t.Fatalf("other response: %v", err)
	}
	closeOpen(t, h, response, domain.StateCancelled, domain.Closer{ActorUserID: interruptOwner})
	if _, err := h.repo.Raise(t.Context(), h.raiseInput(response, raw)); err != nil {
		t.Fatalf("after cancel: %v", err)
	}
}

// Twenty concurrent raises for one response admit exactly sixteen.
func TestRaiseCapHoldsUnderConcurrency(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	inputs := make([]domain.RaiseInput, 20)
	for i := range inputs {
		_, raw := h.card("fanout-interrupt-card-v1.json", nil)
		inputs[i] = h.raiseInput(response, raw)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(inputs))
	for i := range inputs {
		wg.Go(func() { _, errs[i] = h.repo.Raise(t.Context(), inputs[i]) })
	}
	wg.Wait()
	admitted, capped := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, domain.ErrCapReached):
			capped++
		default:
			t.Fatalf("unexpected raise error: %v", err)
		}
	}
	if admitted != domain.MaxOpenInterrupts || capped != 4 {
		t.Fatalf("admitted=%d capped=%d", admitted, capped)
	}
}

func TestDecideAppliesOnceAndReplays(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	got, err := h.decide(interruptOwner, response, key, "tab-1", "approve", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.StateDecided || got.Revision != 2 || got.Replay || got.DecisionRevision != 1 || got.InterruptID == "" || got.Action != domain.ActionApprove {
		t.Fatalf("first decide = %+v", got)
	}
	replay, err := h.decide(interruptOwner, response, key, "tab-1", "approve", "")
	if err != nil || !replay.Replay || replay.State != domain.StateDecided || replay.Revision != 2 || replay.DecisionRevision != 1 {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	for name, seed := range map[string][2]string{
		"other tab other action":      {"tab-2", "reject"},
		"other tab same action":       {"tab-2", "approve"},
		"same request different body": {"tab-1", "reject"},
	} {
		if _, err := h.decide(interruptOwner, response, key, seed[0], seed[1], ""); !errors.Is(err, domain.ErrAlreadyResolved) {
			t.Errorf("%s: %v, want ErrAlreadyResolved", name, err)
		}
	}
	if got := h.audit(response, key); strings.Join(got, ",") != "RAISED:1:claim=claim-raise,DECIDED:2:7" {
		t.Fatalf("audit = %v", got)
	}
	if h.decisionRevision(response) != 1 {
		t.Fatal("decision_revision moved on a replay or conflict")
	}
}

func TestDecideRefusesInvalidActionStaleRevisionAndUnknownKey(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key := h.raise(response, "fanout-interrupt-card-v1.json") // approve, reject, block_with_comment
	if _, err := h.decide(interruptOwner, response, key, "r", "edit", "x"); !errors.Is(err, domain.ErrInvalidDecision) {
		t.Fatalf("action not offered: %v", err)
	}
	staleSum := sha256.Sum256([]byte("stale"))
	_, canonical, err := domain.ParseDecisionRequest([]byte(`{"action":"approve","expected_revision":2,"request_id":"` + hex.EncodeToString(staleSum[:]) + `","value":""}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.Decide(t.Context(), domain.DecideInput{
		Selector: domain.Selector{ProjectID: 1, ActorUserID: interruptOwner, ResponseMessageID: response}, InterruptKey: key, Canonical: canonical,
	}); !errors.Is(err, domain.ErrAlreadyResolved) {
		t.Fatalf("stale expected revision: %v", err)
	}
	if _, err := h.decide(interruptOwner, response, strings.Repeat("cd", 32), "r", "approve", ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
	if got := h.row(response, key); got.state != "PENDING" {
		t.Fatalf("row = %+v", got)
	}
}

// TestDecideFiftyConcurrentTwoTabs: two tabs submit 25 copies each of their
// own decision at once. Exactly one decision is applied; the winning tab's
// other copies replay and every copy from the losing tab gets 409.
func TestDecideFiftyConcurrentTwoTabs(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	type outcome struct {
		tab    string
		result domain.DecideResult
		err    error
	}
	outcomes := make([]outcome, 50)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range outcomes {
		tab, action := "tab-a", "approve"
		if i%2 == 1 {
			tab, action = "tab-b", "reject"
		}
		wg.Go(func() {
			<-start
			result, err := h.decide(interruptOwner, response, key, tab, action, "")
			outcomes[i] = outcome{tab: tab, result: result, err: err}
		})
	}
	close(start)
	wg.Wait()
	applied, replays, conflicts := 0, 0, 0
	winner := ""
	for _, o := range outcomes {
		switch {
		case o.err == nil && !o.result.Replay:
			applied++
			winner = o.tab
		case o.err == nil && o.result.Replay:
			replays++
		case errors.Is(o.err, domain.ErrAlreadyResolved):
			conflicts++
		default:
			t.Fatalf("unexpected outcome: %+v %v", o.result, o.err)
		}
	}
	if applied != 1 || replays != 24 || conflicts != 25 {
		t.Fatalf("applied=%d replays=%d conflicts=%d", applied, replays, conflicts)
	}
	for _, o := range outcomes {
		if o.err == nil && o.tab != winner {
			t.Fatalf("losing tab %s got 200", o.tab)
		}
	}
	if got := h.row(response, key); got.state != "DECIDED" || got.revision != 2 {
		t.Fatalf("row = %+v", got)
	}
	if h.decisionRevision(response) != 1 || len(h.audit(response, key)) != 2 {
		t.Fatal("more than one decision was recorded")
	}
}

// Cards are independent: deciding in reverse raise order works, and TG-03,
// the same tool_call_id in two children, gives two separately decided cards.
func TestDecideReverseOrderAndSameToolCallInTwoChildren(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	keys := []string{}
	for ordinal := 1; ordinal <= 3; ordinal++ {
		key, raw := h.card("fanout-interrupt-card-v1.json", func(card map[string]any) {
			card["fanout_v1"].(map[string]any)["ordinal"] = ordinal
			card["parent_agent_path"].([]any)[0].(map[string]any)["sibling_ordinal"] = ordinal
			card["display"].(map[string]any)["tool_call_id"] = "call_jira_1" // TG-03
		})
		input := h.raiseInput(response, raw)
		input.Frontier = domain.Frontier{ChildThread: fmt.Sprintf("child-%d", ordinal), FanoutNode: "research", Ordinal: int64(ordinal)}
		if _, err := h.repo.Raise(t.Context(), input); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	for i := len(keys) - 1; i >= 0; i-- {
		action := "approve"
		if i == 1 {
			action = "reject"
		}
		got, err := h.decide(interruptAsker, response, keys[i], "reverse-"+keys[i], action, "")
		if err != nil || got.DecisionRevision != int64(len(keys)-i) {
			t.Fatalf("decide %d: %+v %v", i, got, err)
		}
		for j := range i {
			if row := h.row(response, keys[j]); row.state != "PENDING" {
				t.Fatalf("deciding card %d touched card %d: %+v", i, j, row)
			}
		}
	}
	list, err := h.repo.List(t.Context(), domain.Selector{ProjectID: 1, ActorUserID: interruptOwner, ResponseMessageID: response})
	if err != nil || len(list.Interrupts) != 3 || list.DecisionRevision != 3 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	for _, listed := range list.Interrupts {
		if listed.State != domain.StateDecided || listed.Revision != 2 {
			t.Fatalf("listed = %+v", listed)
		}
	}
}

func TestDecideAuthorizationInsideTheTransaction(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	refused := func(name string, selector domain.Selector) {
		t.Helper()
		_, canonical := decisionBody(t, name, "approve", "", "")
		_, err := h.repo.Decide(t.Context(), domain.DecideInput{Selector: selector, InterruptKey: key, Canonical: canonical})
		if !errors.Is(err, domain.ErrNotAllowed) {
			t.Errorf("%s: %v, want ErrNotAllowed", name, err)
		}
		if _, err := h.repo.List(t.Context(), selector); !errors.Is(err, domain.ErrNotAllowed) {
			t.Errorf("%s list: %v, want ErrNotAllowed", name, err)
		}
	}
	selector := func(project, actor int64) domain.Selector {
		return domain.Selector{ProjectID: project, ActorUserID: actor, ResponseMessageID: response}
	}
	refused("outsider (member, not a participant)", selector(1, interruptOutsider))
	refused("foreign actor (other project only)", selector(1, interruptForeigner))
	refused("foreign project (owner's own grant there)", selector(2, interruptOwner))
	refused("suspended participant", selector(1, interruptSuspended))
	refused("unknown response", domain.Selector{ProjectID: 1, ActorUserID: interruptOwner, ResponseMessageID: "10000000-0000-4000-8000-000000000098"})

	// The question's author loses their participant mapping: refused.
	h.exec(`DELETE FROM p_1.chat_participant_mapping mapping USING p_1.chat_participants participant
WHERE mapping.participant_id = participant.id AND participant.entity_meta->>'id' = '8'`)
	refused("removed member", selector(1, interruptAsker))
	// The owner loses the project grant: refused, though still the owner.
	h.exec(`DELETE FROM public.auth_core__project_user_role WHERE project_id = 1 AND user_id = 7`)
	refused("revoked role", selector(1, interruptOwner))
	if got := h.row(response, key); got.state != "PENDING" || got.revision != 1 {
		t.Fatalf("row changed by a refused caller: %+v", got)
	}
}

// interruptKey grants nothing: a key from another response is not found
// under an authorized response.
func TestDecideKeyFromOtherResponseIs404(t *testing.T) {
	h := newInterruptHarness(t)
	mine := h.newResponse()
	other := h.newResponse()
	h.raise(mine, "fanout-interrupt-card-v1.json")
	otherKey := h.raise(other, "fanout-interrupt-card-v1.json")
	if _, err := h.decide(interruptOwner, mine, otherKey, "cross", "approve", ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-response key: %v", err)
	}
	if got := h.row(other, otherKey); got.state != "PENDING" {
		t.Fatalf("other response card = %+v", got)
	}
}

func closeOpen(t *testing.T, h *interruptHarness, responseID string, state domain.State, by domain.Closer) []domain.Resolved {
	t.Helper()
	var closed []domain.Resolved
	err := h.repo.projects.WithinProjectTx(t.Context(), 1, interruptWriteTx, func(tx sqlExecutor) error {
		var err error
		if state == domain.StateCancelled {
			closed, err = h.repo.CancelAllForResponse(t.Context(), tx, responseID, by)
		} else {
			closed, err = h.repo.SupersedeForResponse(t.Context(), tx, responseID, by)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return closed
}

func TestCancelAndSupersedeCloseEveryOpenCard(t *testing.T) {
	h := newInterruptHarness(t)
	for _, tc := range []struct {
		state domain.State
		by    domain.Closer
		who   string
	}{
		{domain.StateCancelled, domain.Closer{ActorUserID: interruptOwner}, "7"},
		{domain.StateSuperseded, domain.Closer{ClaimID: "claim-drain"}, "claim=claim-drain"},
	} {
		response := h.newResponse()
		pending := h.raise(response, "fanout-interrupt-card-v1.json")
		decided := h.raise(response, "fanout-interrupt-card-v1.map-ask-user.json")
		if _, err := h.decide(interruptOwner, response, decided, "pre-close", "answer", "prod"); err != nil {
			t.Fatal(err)
		}
		closed := closeOpen(t, h, response, tc.state, tc.by)
		if len(closed) != 2 {
			t.Fatalf("%s closed %d cards", tc.state, len(closed))
		}
		if got := h.row(response, pending); got.state != string(tc.state) || got.revision != 2 {
			t.Fatalf("%s pending row = %+v", tc.state, got)
		}
		if got := h.row(response, decided); got.state != string(tc.state) || got.revision != 3 || got.requestID == "" {
			t.Fatalf("%s decided row = %+v", tc.state, got)
		}
		if got := h.audit(response, pending); got[len(got)-1] != fmt.Sprintf("%s:2:%s", tc.state, tc.who) {
			t.Fatalf("%s audit = %v", tc.state, got)
		}
		// A late decision and a repeated close change nothing.
		if _, err := h.decide(interruptOwner, response, pending, "late", "approve", ""); !errors.Is(err, domain.ErrAlreadyResolved) {
			t.Fatalf("decision after %s: %v", tc.state, err)
		}
		if _, err := h.decide(interruptOwner, response, decided, "pre-close", "answer", "prod"); !errors.Is(err, domain.ErrAlreadyResolved) {
			t.Fatalf("replay after %s: %v", tc.state, err)
		}
		if again := closeOpen(t, h, response, tc.state, tc.by); len(again) != 0 {
			t.Fatalf("second %s closed %d", tc.state, len(again))
		}
		list, err := h.repo.List(t.Context(), domain.Selector{ProjectID: 1, ActorUserID: interruptOwner, ResponseMessageID: response})
		if err != nil || len(list.Interrupts) != 0 {
			t.Fatalf("list after %s = %+v %v", tc.state, list, err)
		}
	}
	err := h.repo.projects.WithinProjectTx(t.Context(), 1, interruptWriteTx, func(tx sqlExecutor) error {
		_, err := h.repo.CancelAllForResponse(t.Context(), tx, h.responses[0], domain.Closer{ActorUserID: 7, ClaimID: "both"})
		return err
	})
	if !errors.Is(err, domain.ErrInvalidClose) {
		t.Fatalf("closer with both actor and claim: %v", err)
	}
}

func (h *interruptHarness) ack(fence domain.ClaimFence, ack domain.Ack) (domain.AckResult, error) {
	encoded, err := json.Marshal(ack)
	if err != nil {
		h.t.Fatal(err)
	}
	_, canonical, err := domain.ParseAck(encoded)
	if err != nil {
		h.t.Fatalf("ack fixture: %v", err)
	}
	return h.repo.Ack(h.t.Context(), fence, canonical)
}

func TestFetchAndAckUnderTheLiveClaim(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	fence := h.bindLiveClaim(response)
	empty, err := h.repo.FetchDecided(t.Context(), fence)
	if err != nil || len(empty.Decisions) != 0 || empty.DecisionRevision != 0 {
		t.Fatalf("fetch before any card = %+v, %v", empty, err)
	}
	approve := h.raise(response, "fanout-interrupt-card-v1.json")
	auth := h.raise(response, "fanout-interrupt-card-v1.delegated-auth.json")
	pending := h.raise(response, "fanout-interrupt-card-v1.map-ask-user.json")
	if _, err := h.decide(interruptOwner, response, approve, "fetch-approve", "approve", ""); err != nil {
		t.Fatal(err)
	}
	_, canonical := decisionBody(t, "fetch-auth", "authorize", "", "tsr_4f9a1c2b7d3e8f60")
	if _, err := h.repo.Decide(t.Context(), domain.DecideInput{
		Selector: domain.Selector{ProjectID: 1, ActorUserID: interruptAsker, ResponseMessageID: response}, InterruptKey: auth, Canonical: canonical,
	}); err != nil {
		t.Fatal(err)
	}
	fetch, err := h.repo.FetchDecided(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	if fetch.DecisionRevision != 2 || len(fetch.Decisions) != 2 || fetch.Decisions[0].InterruptKey != approve || fetch.Decisions[1].InterruptKey != auth {
		t.Fatalf("fetch = %+v", fetch)
	}
	// Reject/Skip is never permission: the stored action comes back verbatim.
	if fetch.Decisions[0].Action != domain.ActionApprove || fetch.Decisions[1].Action != domain.ActionAuthorize ||
		fetch.Decisions[1].CredentialRef == nil || *fetch.Decisions[1].CredentialRef != "tsr_4f9a1c2b7d3e8f60" {
		t.Fatalf("fetched actions = %+v", fetch.Decisions)
	}
	for _, entry := range fetch.Decisions {
		if entry.Revision != 2 || !domain.ValidDigest(entry.DecisionSHA256) || entry.InterruptKey == pending {
			t.Fatalf("entry = %+v", entry)
		}
	}
	encoded, err := fetch.CanonicalJSON()
	if err != nil || strings.Contains(string(encoded), "decided_by") || strings.Contains(string(encoded), "child-thread") {
		t.Fatalf("fetch body leaks private fields: %s %v", encoded, err)
	}

	checkpoint := "ckpt-0002"
	applied := domain.Ack{InterruptKey: approve, RequestID: fetch.Decisions[0].RequestID, Revision: 2,
		DecisionSHA256: fetch.Decisions[0].DecisionSHA256, Outcome: domain.AckApplied, ChildCheckpointID: &checkpoint}
	wrongDigest := applied
	wrongDigest.DecisionSHA256 = fetch.Decisions[1].DecisionSHA256
	if _, err := h.ack(fence, wrongDigest); !errors.Is(err, domain.ErrAckConflict) {
		t.Fatalf("ACK with another decision's digest: %v", err)
	}
	got, err := h.ack(fence, applied)
	if err != nil || got.State != domain.StateConsumed || got.Revision != 3 || got.Replay {
		t.Fatalf("applied ACK = %+v, %v", got, err)
	}
	replay, err := h.ack(fence, applied)
	if err != nil || !replay.Replay || replay.State != domain.StateConsumed || replay.Revision != 3 {
		t.Fatalf("ACK replay = %+v, %v", replay, err)
	}
	other := "ckpt-0003"
	conflicting := applied
	conflicting.ChildCheckpointID = &other
	if _, err := h.ack(fence, conflicting); !errors.Is(err, domain.ErrAckConflict) {
		t.Fatalf("conflicting ACK replay: %v", err)
	}
	stale := domain.Ack{InterruptKey: auth, RequestID: fetch.Decisions[1].RequestID, Revision: 2,
		DecisionSHA256: fetch.Decisions[1].DecisionSHA256, Outcome: domain.AckStale}
	if got, err := h.ack(fence, stale); err != nil || got.State != domain.StateSuperseded || got.Revision != 3 {
		t.Fatalf("stale ACK = %+v, %v", got, err)
	}
	if got := h.audit(response, approve); got[len(got)-1] != "CONSUMED:3:claim="+fence.ClaimID {
		t.Fatalf("approve audit = %v", got)
	}
	// A decided-then-consumed request still replays to the browser.
	if replay, err := h.decide(interruptOwner, response, approve, "fetch-approve", "approve", ""); err != nil || !replay.Replay || replay.State != domain.StateConsumed {
		t.Fatalf("browser replay after consume = %+v %v", replay, err)
	}
	after, err := h.repo.FetchDecided(t.Context(), fence)
	if err != nil || len(after.Decisions) != 0 {
		t.Fatalf("fetch after ACKs = %+v, %v", after, err)
	}
	// Pending cards cannot be ACKed.
	if _, err := h.ack(fence, domain.Ack{InterruptKey: pending, RequestID: fetch.Decisions[0].RequestID, Revision: 2,
		DecisionSHA256: fetch.Decisions[0].DecisionSHA256, Outcome: domain.AckStale}); !errors.Is(err, domain.ErrAckConflict) {
		t.Fatalf("ACK of a PENDING card: %v", err)
	}
}

func TestFetchAndAckRefuseStaleFences(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	fence := h.bindLiveClaim(response)
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	if _, err := h.decide(interruptOwner, response, key, "fence", "approve", ""); err != nil {
		t.Fatal(err)
	}
	fetch, err := h.repo.FetchDecided(t.Context(), fence)
	if err != nil || len(fetch.Decisions) != 1 {
		t.Fatalf("live fetch = %+v %v", fetch, err)
	}
	checkpoint := "ckpt-1"
	ack := domain.Ack{InterruptKey: key, RequestID: fetch.Decisions[0].RequestID, Revision: 2,
		DecisionSHA256: fetch.Decisions[0].DecisionSHA256, Outcome: domain.AckApplied, ChildCheckpointID: &checkpoint}
	for name, mutate := range map[string]func(*domain.ClaimFence){
		"wrong fence token": func(f *domain.ClaimFence) { f.FenceToken = []byte(strings.Repeat("g", 32)) },
		"wrong identity":    func(f *domain.ClaimFence) { f.WorkloadIdentity = "spiffe://elitea.test/other" },
		"wrong claim":       func(f *domain.ClaimFence) { f.ClaimID = "claim-other" },
		"short fence":       func(f *domain.ClaimFence) { f.FenceToken = []byte("short") },
	} {
		stale := fence
		mutate(&stale)
		if _, err := h.repo.FetchDecided(t.Context(), stale); !errors.Is(err, domain.ErrStaleFence) {
			t.Errorf("%s fetch: %v", name, err)
		}
		if _, err := h.ack(stale, ack); !errors.Is(err, domain.ErrStaleFence) {
			t.Errorf("%s ack: %v", name, err)
		}
	}
	for name, statement := range map[string]string{
		"node recovery claim":   `UPDATE elitea_runtime.execution_claims SET recovery_mode = 'NODE_RECOVERY' WHERE claim_id = $1`,
		"released claim":        `UPDATE elitea_runtime.execution_claims SET released_at = clock_timestamp() WHERE claim_id = $1`,
		"stopped execution":     `UPDATE elitea_runtime.execution_jobs SET desired_state = 'CANCELLED' WHERE execution_id = (SELECT execution_id FROM elitea_runtime.execution_claims WHERE claim_id = $1)`,
		"expired lease":         `UPDATE elitea_runtime.execution_claims SET claimed_at = clock_timestamp() - interval '2 seconds', lease_expires_at = clock_timestamp() - interval '1 second' WHERE claim_id = $1`,
		"expired session":       `UPDATE elitea_runtime.workload_sessions SET expires_at = clock_timestamp() - interval '1 second' WHERE workload_session_id = (SELECT workload_session_id FROM elitea_runtime.execution_claims WHERE claim_id = $1)`,
		"revoked session":       `UPDATE elitea_runtime.workload_sessions SET revoked_at = clock_timestamp() WHERE workload_session_id = (SELECT workload_session_id FROM elitea_runtime.execution_claims WHERE claim_id = $1)`,
		"past command deadline": `UPDATE elitea_runtime.command_outbox SET deadline = clock_timestamp() - interval '1 second' WHERE execution_id = (SELECT execution_id FROM elitea_runtime.execution_claims WHERE claim_id = $1)`,
		"terminal result": `INSERT INTO elitea_runtime.output_inbox (
    event_id, logical_output_id, execution_id, generation, claim_id, fence_token, workload_identity, workload_session_id,
    producer_id, claim_attempt, lease_epoch, stream_id, sequence, payload_type, payload_digest, payload_bytes,
    settlement_proposal_id, settlement_outcome, settlement_proposal_bytes, settlement_proposal_digest,
    settlement_idempotency_key, occurred_at)
SELECT 'terminal-' || c.claim_id, 'terminal-output', c.execution_id, 1, c.claim_id, c.fence_token, c.workload_identity,
    c.workload_session_id, c.producer_id, 1, 1, 'stream', 1, 'INDEX_INGEST_RESULT', decode(repeat('ab', 32), 'hex'), '{}'::bytea,
    'proposal-terminal', 'SUCCEEDED', '{}'::bytea, decode(repeat('ab', 32), 'hex'), 'idempotency-terminal', now()
FROM elitea_runtime.execution_claims c WHERE c.claim_id = $1`,
	} {
		h.exec(statement, fence.ClaimID)
		if _, err := h.repo.FetchDecided(t.Context(), fence); !errors.Is(err, domain.ErrStaleFence) {
			t.Errorf("%s fetch: %v", name, err)
		}
		if _, err := h.ack(fence, ack); !errors.Is(err, domain.ErrStaleFence) {
			t.Errorf("%s ack: %v", name, err)
		}
		h.exec(`UPDATE elitea_runtime.execution_claims SET recovery_mode = 'NONE', released_at = NULL,
 lease_expires_at = clock_timestamp() + interval '10 minutes' WHERE claim_id = $1`, fence.ClaimID)
		h.exec(`UPDATE elitea_runtime.execution_jobs SET desired_state = 'RUNNING' WHERE execution_id = $1`, fence.ExecutionID)
		h.exec(`UPDATE elitea_runtime.workload_sessions SET expires_at = clock_timestamp() + interval '1 hour', revoked_at = NULL
 WHERE workload_session_id = (SELECT workload_session_id FROM elitea_runtime.execution_claims WHERE claim_id = $1)`, fence.ClaimID)
		h.exec(`UPDATE elitea_runtime.command_outbox SET deadline = clock_timestamp() + interval '1 hour' WHERE execution_id = $1`, fence.ExecutionID)
		h.exec(`DELETE FROM elitea_runtime.output_inbox WHERE execution_id = $1`, fence.ExecutionID)
		if live, err := h.repo.FetchDecided(t.Context(), fence); err != nil || len(live.Decisions) != 1 {
			t.Fatalf("after restoring %s: %+v %v", name, live, err)
		}
	}
	// The original actor losing the decision permission stops delivery.
	h.exec(`DELETE FROM public.auth_core__project_user_role WHERE project_id = 1 AND user_id = 8`)
	if _, err := h.repo.FetchDecided(t.Context(), fence); !errors.Is(err, domain.ErrStaleFence) {
		t.Errorf("revoked actor fetch: %v", err)
	}
	if got := h.row(response, key); got.state != "DECIDED" {
		t.Fatalf("a refused ACK changed the row: %+v", got)
	}
}

// A stop closes the card; the late ACK of the old claim is refused.
func TestLateAckAfterCancelRefused(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	fence := h.bindLiveClaim(response)
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	if _, err := h.decide(interruptOwner, response, key, "late-ack", "approve", ""); err != nil {
		t.Fatal(err)
	}
	fetch, err := h.repo.FetchDecided(t.Context(), fence)
	if err != nil || len(fetch.Decisions) != 1 {
		t.Fatal(fetch, err)
	}
	closeOpen(t, h, response, domain.StateCancelled, domain.Closer{ActorUserID: interruptOwner})
	checkpoint := "ckpt-late"
	if _, err := h.ack(fence, domain.Ack{InterruptKey: key, RequestID: fetch.Decisions[0].RequestID, Revision: 2,
		DecisionSHA256: fetch.Decisions[0].DecisionSHA256, Outcome: domain.AckApplied, ChildCheckpointID: &checkpoint}); !errors.Is(err, domain.ErrAckConflict) {
		t.Fatalf("late ACK after cancel: %v", err)
	}
	if got := h.row(response, key); got.state != "CANCELLED" || got.revision != 3 {
		t.Fatalf("row = %+v", got)
	}
}

// The table CHECKs make an incoherent row impossible to write, whoever writes it.
func TestInterruptStateChecksRejectIncoherentRows(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	for name, statement := range map[string]string{
		"decided without decision":   `UPDATE elitea_runtime.execution_interrupts SET state = 'DECIDED', revision = 2 WHERE interrupt_key = $1`,
		"pending at revision 2":      `UPDATE elitea_runtime.execution_interrupts SET revision = 2 WHERE interrupt_key = $1`,
		"partial decision columns":   `UPDATE elitea_runtime.execution_interrupts SET state = 'DECIDED', revision = 2, request_id = repeat('a', 64) WHERE interrupt_key = $1`,
		"consumed without claim":     `UPDATE elitea_runtime.execution_interrupts SET state = 'CONSUMED', revision = 3 WHERE interrupt_key = $1`,
		"cancelled without closed":   `UPDATE elitea_runtime.execution_interrupts SET state = 'CANCELLED', revision = 2 WHERE interrupt_key = $1`,
		"unknown state":              `UPDATE elitea_runtime.execution_interrupts SET state = 'APPLIED' WHERE interrupt_key = $1`,
		"oversized decision":         `UPDATE elitea_runtime.execution_interrupts SET state = 'DECIDED', revision = 2, request_id = repeat('a', 64), decided_by = 7, decided_at = now(), decision_json = convert_to(repeat('x', 8193), 'UTF8') WHERE interrupt_key = $1`,
		"non-ascii interrupt id":     `UPDATE elitea_runtime.execution_interrupts SET interrupt_id = 'é' WHERE interrupt_key = $1`,
		"unknown action":             `UPDATE elitea_runtime.execution_interrupts SET available_actions = ARRAY['allow'] WHERE interrupt_key = $1`,
		"audit decided without user": `INSERT INTO elitea_runtime.execution_interrupt_audit (root_response_id, interrupt_key, transition, revision, claim_id) SELECT root_response_id, interrupt_key, 'DECIDED', 2, 'c' FROM elitea_runtime.execution_interrupts WHERE interrupt_key = $1`,
		"audit with actor and claim": `INSERT INTO elitea_runtime.execution_interrupt_audit (root_response_id, interrupt_key, transition, revision, actor_id, claim_id) SELECT root_response_id, interrupt_key, 'CANCELLED', 2, 7, 'c' FROM elitea_runtime.execution_interrupts WHERE interrupt_key = $1`,
	} {
		if _, err := h.pool.Exec(t.Context(), statement, key); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if got := h.row(response, key); got.state != "PENDING" || got.revision != 1 {
		t.Fatalf("row = %+v", got)
	}
}

// interruptStatementCounter counts statements sent by one pool.
type interruptStatementCounter struct {
	mu         sync.Mutex
	statements []string
}

func (c *interruptStatementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	c.statements = append(c.statements, strings.Fields(data.SQL)[0])
	c.mu.Unlock()
	return ctx
}

func (c *interruptStatementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (c *interruptStatementCounter) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.statements
	c.statements = nil
	return out
}

// TestDecideSingleTransaction pins the per-decision budget: one transaction
// and at most 11 statements: 3 tenant binding, 1 response row lock, 1
// ownership and membership check (re-read after the lock), 3 RBAC, 1
// per-response row lock, 1 card lock, 1 CAS+revision+audit. It writes 3 rows
// (card, response revision, audit). A replay is the same reads without the
// CAS.
func TestDecideSingleTransaction(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	counter := &interruptStatementCounter{}
	config := h.pool.Config()
	config.ConnConfig.Tracer = counter
	counted, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer counted.Close()
	repo, err := NewExecutionInterruptRepository(counted)
	if err != nil {
		t.Fatal(err)
	}
	_, canonical := decisionBody(t, "budget", "approve", "", "")
	input := domain.DecideInput{Selector: domain.Selector{ProjectID: 1, ActorUserID: interruptOwner, ResponseMessageID: response}, InterruptKey: key, Canonical: canonical}
	counter.take()
	if _, err := repo.Decide(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	statements := counter.take()
	begins, commits := 0, 0
	for _, statement := range statements {
		switch strings.ToLower(statement) {
		case "begin", "begin_transaction":
			begins++
		case "commit":
			commits++
		}
	}
	// pgx sends BEGIN and COMMIT as statements through the tracer.
	if commits != 1 || begins > 1 {
		t.Fatalf("decision used begins=%d commits=%d: %v", begins, commits, statements)
	}
	work := len(statements) - begins - commits
	if work > 11 {
		t.Fatalf("decision used %d statements, budget 11: %v", work, statements)
	}
	t.Logf("decision statements: %d work + BEGIN/COMMIT: %v", work, statements)
	var writes int
	if err := h.pool.QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM elitea_runtime.execution_interrupt_audit WHERE interrupt_key = $1 AND transition = 'DECIDED')
 + (SELECT count(*) FROM elitea_runtime.execution_interrupts WHERE interrupt_key = $1 AND state = 'DECIDED')
 + (SELECT count(*) FROM elitea_runtime.execution_interrupt_responses WHERE root_response_id = $2::uuid AND decision_revision = 1)`, key, response).Scan(&writes); err != nil || writes != 3 {
		t.Fatalf("row writes = %d, %v", writes, err)
	}
	if _, err := repo.Decide(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	if replay := counter.take(); len(replay) > 12 { // 10 reads + BEGIN/COMMIT
		t.Fatalf("replay used %d statements: %v", len(replay), replay)
	}
}

// TestDecideLatencyBudget: p95 of 200 sequential decisions at most 150 ms on
// the test database (contract §12 Performance). Wall-clock time also measures
// the host: the test runs three rounds and asserts the best round's p95, so a
// transient load spike does not fail it while a slow decision path does.
func TestDecideLatencyBudget(t *testing.T) {
	h := newInterruptHarness(t)
	const decisions = 200
	best := time.Duration(1<<63 - 1)
	for round := range 3 {
		keys := map[string][]string{}
		order := []string{}
		for len(order) < decisions {
			response := h.newResponse()
			for range domain.MaxOpenInterrupts {
				if len(order) == decisions {
					break
				}
				keys[response] = append(keys[response], h.raise(response, "fanout-interrupt-card-v1.json"))
				order = append(order, response)
			}
		}
		durations := make([]time.Duration, 0, decisions)
		next := map[string]int{}
		for index, response := range order {
			key := keys[response][next[response]]
			next[response]++
			started := time.Now()
			if _, err := h.decide(interruptOwner, response, key, fmt.Sprintf("latency-%d-%d", round, index), "approve", ""); err != nil {
				t.Fatal(err)
			}
			durations = append(durations, time.Since(started))
		}
		slices.Sort(durations)
		p50, p95, p99 := durations[decisions/2], durations[decisions*95/100], durations[decisions*99/100]
		t.Logf("round %d: decide latency over %d: p50=%s p95=%s p99=%s", round+1, decisions, p50, p95, p99)
		best = min(best, p95)
	}
	if best > 150*time.Millisecond {
		t.Fatalf("best-round p95 %s exceeds 150ms", best)
	}
}

// waitForLockWaiters blocks until at least n backends of this database wait
// on a lock, so an interleaving is forced rather than hoped for.
func (h *interruptHarness) waitForLockWaiters(n int) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := h.pool.QueryRow(h.t.Context(), `SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			h.t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%d backends waiting on a lock, want %d", waiting, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDecideWaitsForAnInFlightDecision forces the race the concurrent test
// can only sample: a decision is written but not yet committed when the same
// request and a different request arrive. Both must wait for it, then the
// same request replays (200) and the other gets 409. A decide that read the
// card before the winner committed would answer the same tab 409.
func TestDecideWaitsForAnInFlightDecision(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	decision, canonical := decisionBody(t, "in-flight", "approve", "", "")

	winner, err := h.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = winner.Rollback(context.Background()) }()
	for _, statement := range []string{
		`SELECT 1 FROM p_1.chat_message_group WHERE uuid = $1::uuid FOR UPDATE`,
		`UPDATE elitea_runtime.execution_interrupts SET state = 'DECIDED', revision = 2, request_id = $3,
 decision_json = $4, decided_by = 7, decided_at = clock_timestamp() WHERE root_response_id = $1::uuid AND interrupt_key = $2`,
		`UPDATE elitea_runtime.execution_interrupt_responses SET decision_revision = decision_revision + 1 WHERE root_response_id = $1::uuid`,
	} {
		args := []any{response}
		if strings.Contains(statement, "$2") {
			args = append(args, key, decision.RequestID, canonical)
		}
		if _, err := winner.Exec(t.Context(), statement, args...); err != nil {
			t.Fatal(err)
		}
	}

	type outcome struct {
		result domain.DecideResult
		err    error
	}
	same, other := make(chan outcome, 1), make(chan outcome, 1)
	go func() {
		result, err := h.repo.Decide(t.Context(), domain.DecideInput{
			Selector:     domain.Selector{ProjectID: 1, ActorUserID: interruptOwner, ResponseMessageID: response},
			InterruptKey: key, Canonical: canonical,
		})
		same <- outcome{result, err}
	}()
	go func() {
		result, err := h.decide(interruptOwner, response, key, "in-flight-other-tab", "reject", "")
		other <- outcome{result, err}
	}()
	h.waitForLockWaiters(2)
	if err := winner.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := <-same; got.err != nil || !got.result.Replay || got.result.State != domain.StateDecided || got.result.Revision != 2 {
		t.Fatalf("same request after the in-flight decision = %+v, %v; want replay", got.result, got.err)
	}
	if got := <-other; !errors.Is(got.err, domain.ErrAlreadyResolved) {
		t.Fatalf("other request after the in-flight decision = %+v, %v; want 409", got.result, got.err)
	}
}

// The decider loses conversation membership while waiting for another tab's
// lock on the response. The ownership rule is re-read after the lock, so the
// decision is refused (review finding: one-statement lock+check used the
// pre-wait snapshot).
func TestDecideRechecksMembershipAfterTheLockWait(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	holder, err := h.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(t.Context(), `SELECT 1 FROM p_1.chat_message_group WHERE uuid = $1::uuid FOR UPDATE`, response); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.decide(interruptAsker, response, key, "member-wait", "approve", "")
		done <- err
	}()
	h.waitForLockWaiters(1)
	h.exec(`DELETE FROM p_1.chat_participant_mapping mapping USING p_1.chat_participants participant
WHERE mapping.participant_id = participant.id AND participant.entity_meta->>'id' = '8'`)
	if err := holder.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, domain.ErrNotAllowed) {
		t.Fatalf("decision by a member removed during the wait: %v, want ErrNotAllowed", err)
	}
	if got := h.row(response, key); got.state != "PENDING" {
		t.Fatalf("row = %+v", got)
	}
}

// The claim's lease expires while FetchDecided waits for the job lock. The
// authority is looked up again after the lock with the database clock
// (contract §7), so nothing is delivered under the expired lease.
func TestFetchRechecksTheClaimAfterTheLockWait(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	fence := h.bindLiveClaim(response)
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	if _, err := h.decide(interruptOwner, response, key, "lease-wait", "approve", ""); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE elitea_runtime.execution_claims SET lease_expires_at = clock_timestamp() + interval '1500 milliseconds' WHERE claim_id = $1`, fence.ClaimID)
	holder, err := h.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(t.Context(), `SELECT 1 FROM elitea_runtime.execution_jobs WHERE execution_id = $1 FOR UPDATE`, fence.ExecutionID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.repo.FetchDecided(t.Context(), fence)
		done <- err
	}()
	h.waitForLockWaiters(1)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var expired bool
		if err := h.pool.QueryRow(t.Context(), `SELECT lease_expires_at <= clock_timestamp() FROM elitea_runtime.execution_claims WHERE claim_id = $1`, fence.ClaimID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lease did not expire")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := holder.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("fetch after the lease expired during the wait: %v, want ErrStaleFence", err)
	}
}

// A raise is in flight (its card is written, not committed) when a stop
// closes the response. Cancel takes the per-response lock itself, so it waits
// for the raise and closes the new card too instead of leaving it PENDING.
func TestCancelSeesACardRaisedWhileItWaited(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	first := h.raise(response, "fanout-interrupt-card-v1.json")
	key, raw := h.card("fanout-interrupt-card-v1.json", nil)
	raising, err := h.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raising.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), raising, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := raiseExecutionInterrupt(t.Context(), pgxExecutor{queryer: raising}, h.raiseInput(response, raw)); err != nil {
		t.Fatal(err)
	}
	done := make(chan []domain.Resolved, 1)
	go func() {
		done <- closeOpen(t, h, response, domain.StateCancelled, domain.Closer{ActorUserID: interruptOwner})
	}()
	h.waitForLockWaiters(1)
	if err := raising.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if closed := <-done; len(closed) != 2 {
		t.Fatalf("cancel closed %d cards, want 2", len(closed))
	}
	for _, k := range []string{first, key} {
		if got := h.row(response, k); got.state != "CANCELLED" {
			t.Fatalf("card %s = %+v", k[:8], got)
		}
	}
}

// Raise binds the raising execution to the root response and the project.
func TestRaiseRefusesAnExecutionNotBoundToTheResponse(t *testing.T) {
	h := newInterruptHarness(t)
	mine, other := h.newResponse(), h.newResponse()
	_, raw := h.card("fanout-interrupt-card-v1.json", nil)
	input := h.raiseInput(mine, raw)
	input.ExecutionID = h.executions[other]
	if _, err := h.repo.Raise(t.Context(), input); !errors.Is(err, domain.ErrInvalidRaise) {
		t.Fatalf("execution of another response: %v", err)
	}
	h.exec(`UPDATE elitea_runtime.execution_jobs SET resource_project_id = 2, projection_project_id = 2, tenant_id = '2' WHERE execution_id = $1`, h.executions[mine])
	if _, err := h.repo.Raise(t.Context(), h.raiseInput(mine, raw)); !errors.Is(err, domain.ErrInvalidRaise) {
		t.Fatalf("execution of another project: %v", err)
	}
}

// More than MaxOpenInterrupts open or DECIDED rows can only come from a
// broken invariant; List and FetchDecided refuse them instead of truncating.
func TestFetchAndListRefuseMoreThanTheCap(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	fence := h.bindLiveClaim(response)
	var last string
	for i := range domain.MaxOpenInterrupts {
		last = h.raise(response, "fanout-interrupt-card-v1.json")
		if _, err := h.decide(interruptOwner, response, last, fmt.Sprintf("cap-%d", i), "approve", ""); err != nil {
			t.Fatal(err)
		}
	}
	if fetch, err := h.repo.FetchDecided(t.Context(), fence); err != nil || len(fetch.Decisions) != domain.MaxOpenInterrupts {
		t.Fatalf("fetch at the cap = %d, %v", len(fetch.Decisions), err)
	}
	h.exec(`INSERT INTO elitea_runtime.execution_interrupts (
    root_response_id, interrupt_key, project_id, conversation_id, execution_id, generation, interrupt_id, kind,
    available_actions, frontier, card_json, payload_sha256, source_event_id, state, revision, request_id,
    decision_json, decided_by, decided_at)
SELECT root_response_id, repeat('e', 64), project_id, conversation_id, execution_id, generation, 'injected', kind,
    available_actions, frontier, card_json, payload_sha256, 'injected-event', state, revision, request_id,
    decision_json, decided_by, decided_at
FROM elitea_runtime.execution_interrupts WHERE interrupt_key = $1`, last)
	if _, err := h.repo.FetchDecided(t.Context(), fence); !errors.Is(err, domain.ErrLedgerFault) {
		t.Fatalf("fetch over the cap: %v", err)
	}
	if _, err := h.repo.List(t.Context(), domain.Selector{ProjectID: 1, ActorUserID: interruptOwner, ResponseMessageID: response}); !errors.Is(err, domain.ErrLedgerFault) {
		t.Fatalf("list over the cap: %v", err)
	}
}

// Every invalid ACK fixture is refused before any row is touched.
func TestAckRefusesInvalidBodies(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	fence := h.bindLiveClaim(response)
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	if _, err := h.decide(interruptOwner, response, key, "ack-invalid", "approve", ""); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(interruptContractFixtures, "fanout-interrupt-ack-request-v1.invalid.*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatal(paths, err)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.repo.Ack(t.Context(), fence, raw); !errors.Is(err, domain.ErrInvalidAck) {
			t.Errorf("%s: %v", filepath.Base(path), err)
		}
	}
	if got := h.row(response, key); got.state != "DECIDED" {
		t.Fatalf("row = %+v", got)
	}
}

// A caller who may not decide never takes the response row lock: while
// another transaction holds it, an outsider is refused at once instead of
// queueing behind the owner's writes.
func TestDecideByOutsiderNeverWaitsForTheResponseLock(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	holder, err := h.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(t.Context(), `SELECT 1 FROM p_1.chat_message_group WHERE uuid = $1::uuid FOR UPDATE`, response); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, canonical := decisionBody(t, "outsider-lock", "approve", "", "")
	_, err = h.repo.Decide(ctx, domain.DecideInput{
		Selector:     domain.Selector{ProjectID: 1, ActorUserID: interruptOutsider, ResponseMessageID: response},
		InterruptKey: key, Canonical: canonical,
	})
	if !errors.Is(err, domain.ErrNotAllowed) {
		t.Fatalf("outsider while the row is locked: %v, want an immediate ErrNotAllowed", err)
	}
}
