package repos

// The per-claim worker token for elitea-vector
// (elitea_runtime.vector_claim_tokens, shared/0160; ADR-0031 decision 1).
//
// Only the SHA-256 of a token is stored. Every binding — project, actor,
// execution — is read from the claim and its execution when the token is
// minted, never taken from the caller, and every read re-checks that the
// claim and the execution are still live. Revocation is that re-check: no
// settle, cancel or takeover path writes to this table.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrVectorClaimTokenFactsNotFound reports a hash that names no live worker
// claim token: unknown, expired, or its claim or execution is no longer live.
var ErrVectorClaimTokenFactsNotFound = errors.New("vector claim token facts not found")

// MemoryIntent says what a memory request is about to do; elitea-vector sends
// it with an introspection so the memory permissions are decided at that moment.
type MemoryIntent int

const (
	// MemoryIntentNone wants no memory decision (every non-memory request and
	// project-scope memory reads).
	MemoryIntentNone MemoryIntent = iota
	// MemoryIntentPersonalRead is a read in the personal scope; an allowed one
	// taints the run's conversation.
	MemoryIntentPersonalRead
	// MemoryIntentPersonalWrite is a write in the personal scope.
	MemoryIntentPersonalWrite
	// MemoryIntentProjectWrite is a write in the project scope; refused in a
	// tainted conversation.
	MemoryIntentProjectWrite
	// MemoryIntentSharedIndexWrite is a toolkit-index upsert or delete (an
	// index is shared by the project); refused in a tainted conversation.
	MemoryIntentSharedIndexWrite
)

// VectorClaimTokenMint asks for one token for the live claim Fence names.
type VectorClaimTokenMint struct {
	Fence runtimedomain.Fence
	// TokenSHA256 is the hash of the bearer the caller generated.
	TokenSHA256 [32]byte
	// Capabilities are the execution capabilities that get a token; another
	// capability gets none (Minted is false).
	Capabilities []string
	// Sources are the payload keywords the token is admitted for.
	Sources []string
	// MemorySource, when non-empty, is added to Sources only if the
	// execution was admitted with memory toolkits
	// (execution_jobs.memory_toolkit_ids, computed from the server-frozen
	// toolkit set). A claim without one gets no memory source, so an older
	// elitea-vector that cannot name it is never shown one, and a run with no
	// memory toolkit has no memory reach.
	MemorySource string
	// MaxLifetime caps the expiry; the command deadline is the other bound.
	MaxLifetime time.Duration
}

// VectorClaimTokenMinted is the result of a mint.
type VectorClaimTokenMinted struct {
	// Minted is false when the claim is live but its execution gets no token
	// (another capability, an actor that is not a user id, a passed deadline).
	Minted    bool
	ProjectID int64
	ActorID   int64
	Sources   []string
	ExpiresAt time.Time
}

// VectorClaimTokenFacts is what introspection answers for a live token.
type VectorClaimTokenFacts struct {
	ProjectID int64
	ActorID   int64
	Sources   []string
	ExpiresAt time.Time
	// PersonalMemoryAllowed is decided at THIS introspection, from the claim's
	// conversation as it is now (see personalMemoryTurn), and only for a
	// personal memory intent; elitea-vector refuses the personal memory scope
	// without it.
	PersonalMemoryAllowed bool
	// ProjectMemoryWriteRefused is true for a project-memory or shared-index
	// write intent from an execution whose conversation is tainted (personal
	// memory was read in it, by this or an earlier turn).
	ProjectMemoryWriteRefused bool
	// MemoryToolkits are the memory toolkits the claim was given (all of the
	// token's project); elitea-vector refuses a memory request for any other.
	MemoryToolkits []int64
	// ConversationBound is true for an agent execution (the only kind that
	// can be bound to a conversation); elitea-vector asks the shared-write
	// question only for such a token.
	ConversationBound bool
}

// VectorClaimTokens reads and writes the token hashes.
type VectorClaimTokens struct {
	pool *pgxpool.Pool
}

// NewVectorClaimTokens builds the store over pool.
func NewVectorClaimTokens(pool *pgxpool.Pool) *VectorClaimTokens {
	return &VectorClaimTokens{pool: pool}
}

// Mint records request.TokenSHA256 for the claim request.Fence names. The
// claim must be live — unreleased, unexpired, and matching the fence's
// execution, generation, token and workload identity — or Mint returns
// runtimedomain.ErrStaleFence and records nothing. A repeated mint for the
// same claim replaces the earlier hash; a repeat for which the execution no
// longer qualifies (Minted false) deletes the earlier hash instead, so no
// earlier token stays active for the claim.
func (s *VectorClaimTokens) Mint(ctx context.Context, request VectorClaimTokenMint) (VectorClaimTokenMinted, error) {
	if s == nil || s.pool == nil {
		return VectorClaimTokenMinted{}, errors.New("mint vector claim token: no database")
	}
	fence := request.Fence
	if fence.CommandID == "" || fence.ExecutionID == "" || fence.WorkloadIdentity == "" ||
		fence.Generation == 0 || fence.Generation > 1<<63-1 ||
		len(request.Capabilities) == 0 || len(request.Sources) == 0 ||
		request.MaxLifetime < time.Second {
		return VectorClaimTokenMinted{}, runtimedomain.ErrInvalidFence
	}
	// The memory toolkits are the execution's own, computed at admission
	// (execution_jobs.memory_toolkit_ids) and copied by the statement below:
	// nothing is decoded here, and a claim whose execution has none gets
	// neither memory toolkits nor the memory source.
	var (
		live      bool
		project   *int32
		actor     *string
		sources   []string
		expiresAt *time.Time
	)
	err := s.pool.QueryRow(ctx, `
WITH live AS (
    SELECT c.claim_id, c.execution_id, c.generation,
           j.resource_project_id, j.actor_id, j.capability_id,
           COALESCE(j.memory_toolkit_ids, '{}') AS memory_toolkit_ids,
           LEAST(o.deadline, clock_timestamp() + ($8::bigint * interval '1 millisecond')) AS expires_at
    FROM elitea_runtime.execution_claims AS c
    JOIN elitea_runtime.execution_jobs AS j
      ON j.execution_id = c.execution_id
     AND j.generation = c.generation
    JOIN elitea_runtime.command_outbox AS o
      ON o.execution_id = j.execution_id
     AND o.generation = j.generation
    WHERE c.execution_id = $1
      AND c.generation = $2
      AND c.workload_identity = $3
      AND c.fence_token = $4
      AND c.claim_attempt = $9
      AND c.lease_epoch = $10
      AND j.command_id = $11
      AND c.released_at IS NULL
      AND c.lease_expires_at > clock_timestamp()
), qualifying AS (
    SELECT live.*
    FROM live
    WHERE live.capability_id = ANY ($6::text[])
      AND live.resource_project_id > 0
      AND live.actor_id ~ '^[1-9][0-9]{0,18}$'
      AND live.expires_at > clock_timestamp() + interval '1 second'
), revoked AS (
    -- A live claim whose execution no longer qualifies must not keep an
    -- earlier token: a re-claim of the same claim after the execution
    -- changed (another capability, a passed deadline) takes the old one away
    -- in this same statement. The two writes touch disjoint cases: the
    -- delete runs only when nothing qualifies, the insert only when
    -- something does.
    DELETE FROM elitea_runtime.vector_claim_tokens AS stale
    USING live
    WHERE stale.claim_id = live.claim_id
      AND NOT EXISTS (SELECT 1 FROM qualifying)
), minted AS (
    INSERT INTO elitea_runtime.vector_claim_tokens (
        token_sha256, claim_id, execution_id, generation,
        resource_project_id, actor_id, allowed_sources, expires_at, memory_toolkit_ids
    )
    SELECT $5, qualifying.claim_id, qualifying.execution_id, qualifying.generation,
           qualifying.resource_project_id, qualifying.actor_id,
           CASE WHEN $12::text <> '' AND cardinality(qualifying.memory_toolkit_ids) > 0
                THEN $7::text[] || $12::text
                ELSE $7::text[] END,
           qualifying.expires_at,
           qualifying.memory_toolkit_ids
    FROM qualifying
    ON CONFLICT (claim_id) DO UPDATE
    SET token_sha256 = EXCLUDED.token_sha256,
        allowed_sources = EXCLUDED.allowed_sources,
        memory_toolkit_ids = EXCLUDED.memory_toolkit_ids,
        expires_at = EXCLUDED.expires_at,
        created_at = clock_timestamp()
    RETURNING resource_project_id, actor_id, allowed_sources, expires_at
)
SELECT EXISTS (SELECT 1 FROM live),
       minted.resource_project_id, minted.actor_id, minted.allowed_sources, minted.expires_at
FROM (SELECT 1) AS one
LEFT JOIN minted ON true`,
		fence.ExecutionID, int64(fence.Generation), fence.WorkloadIdentity, fence.Token[:],
		request.TokenSHA256[:], request.Capabilities, request.Sources,
		request.MaxLifetime.Milliseconds(),
		int64(fence.ClaimAttempt), int64(fence.LeaseEpoch), fence.CommandID,
		request.MemorySource,
	).Scan(&live, &project, &actor, &sources, &expiresAt)
	if err != nil {
		return VectorClaimTokenMinted{}, fmt.Errorf("mint vector claim token: %w", err)
	}
	if !live {
		return VectorClaimTokenMinted{}, runtimedomain.ErrStaleFence
	}
	if project == nil || actor == nil || expiresAt == nil {
		return VectorClaimTokenMinted{}, nil
	}
	actorID, err := strconv.ParseInt(*actor, 10, 64)
	if err != nil || actorID <= 0 {
		return VectorClaimTokenMinted{}, fmt.Errorf("mint vector claim token: actor %q is not a user id", *actor)
	}
	return VectorClaimTokenMinted{
		Minted:    true,
		ProjectID: int64(*project),
		ActorID:   actorID,
		Sources:   sources,
		ExpiresAt: expiresAt.UTC(),
	}, nil
}

// personalMemoryTurn says whether the claim's execution may use PERSONAL
// memory right now, and names its conversation when it may. It is decided at the introspection of a personal-scope
// request, never stored at mint, and it is true only when ALL of these hold:
//
//   - the execution is an agent CHAT turn a person started in the product: it
//     has an agent_execution_jobs row, and its trigger_origin is `manual` (the
//     browser composer). An MCP tools/call, an API/PAT start, a schedule, a
//     webhook, a direct toolkit call or an index run is refused, whatever
//     conversation it names;
//   - the turn's conversation is bound SERVER-SIDE, three ways that must
//     agree: the job's client_stream_id (written by Main's own admission, which
//     refuses a turn whose stream differs from the conversation it locked) is
//     compared as a uuid (`conversation.uuid = $::uuid`, so a value that is not
//     a uuid fails closed), and that conversation holds the response message
//     group whose task_id is this execution (the admission transaction wrote
//     it). A caller-supplied stream id that names another conversation finds
//     no response group there and is refused;
//   - that conversation is a chat conversation (source 'elitea', not an MCP,
//     trigger or support one) and PRIVATE (is_private);
//   - its user participants are exactly one, and it is the actor.
//
// Anything else, and any doubt (a failed read, a missing schema or row), is
// false and fails closed: the question is about privacy. Project memory is not
// affected. Because the answer is read each time, a participant added to the
// conversation mid-run withdraws the permission at the next call.
func (s *VectorClaimTokens) personalMemoryTurn(ctx context.Context, executionID string, generation int64, actor string) (conversationTurn, bool) {
	turn, err := s.executionConversation(ctx, executionID, generation)
	if err != nil {
		slog.WarnContext(ctx, "vector claim token: could not read the execution; personal memory is withheld", "err", err)
		return conversationTurn{}, false
	}
	// No conversation (no agent job, a stream that is not a uuid), or not a
	// person's own chat turn: no personal memory.
	if turn.conversation == "" || turn.origin != "manual" {
		return conversationTurn{}, false
	}
	schema, err := tenantschema.QuoteInt(turn.project)
	if err != nil {
		return conversationTurn{}, false
	}
	var (
		conversations int
		private       bool
		people        int
		isActor       bool
	)
	err = s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT count(DISTINCT conversation.id),
       COALESCE(bool_and(conversation.is_private IS TRUE AND conversation.source = 'elitea'), false),
       count(DISTINCT COALESCE(participant.entity_meta ->> 'id', '')),
       COALESCE(bool_and(COALESCE(participant.entity_meta ->> 'id' = $3, false)), false)
FROM %[1]s.chat_conversations AS conversation
JOIN %[1]s.chat_message_group AS response
  ON response.conversation_id = conversation.id AND response.task_id = $2
JOIN %[1]s.chat_participant_mapping AS mapping ON mapping.conversation_id = conversation.id
JOIN %[1]s.chat_participants AS participant
  ON participant.id = mapping.participant_id AND participant.entity_name = 'user'
WHERE conversation.uuid = $1::uuid`, schema), turn.conversation, executionID, actor).Scan(&conversations, &private, &people, &isActor)
	if err != nil {
		slog.WarnContext(ctx, "vector claim token: could not read the turn's conversation; personal memory is withheld", "err", err)
		return conversationTurn{}, false
	}
	if conversations != 1 || !private || people != 1 || !isActor {
		return conversationTurn{}, false
	}
	return turn, true
}

// Facts reads the token whose SHA-256 is tokenSHA256. Only a live token
// answers: unexpired; its claim unreleased with an unexpired lease and an
// unrevoked, unexpired workload session; its execution CLAIMED or RUNNING
// with desired state RUNNING; its project active. Anything else is
// ErrVectorClaimTokenFactsNotFound.
func (s *VectorClaimTokens) Facts(ctx context.Context, tokenSHA256 [32]byte, intent MemoryIntent) (VectorClaimTokenFacts, error) {
	if s == nil || s.pool == nil {
		return VectorClaimTokenFacts{}, fmt.Errorf("%w: no database", ErrVectorClaimTokenFactsNotFound)
	}
	var (
		project           int32
		actor             string
		sources           []string
		expiresAt         time.Time
		execution         string
		claimGen          int64
		memoryToolkits    []int64
		conversationBound bool
	)
	err := s.pool.QueryRow(ctx, `
SELECT t.resource_project_id, t.actor_id, t.allowed_sources, t.expires_at, t.execution_id, t.generation,
       t.memory_toolkit_ids, j.capability_id IN ('agent.execute.application.v1', 'agent.execute.adhoc.v1')
FROM elitea_runtime.vector_claim_tokens AS t
JOIN elitea_runtime.execution_claims AS c
  ON c.claim_id = t.claim_id
 AND c.execution_id = t.execution_id
 AND c.generation = t.generation
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = t.execution_id
 AND j.generation = t.generation
JOIN elitea_runtime.workload_sessions AS ws
  ON ws.workload_session_id = c.workload_session_id
 AND ws.workload_identity = c.workload_identity
 AND ws.producer_id = c.producer_id
JOIN centry.project AS p
  ON p.id = t.resource_project_id
WHERE t.token_sha256 = $1
  AND t.expires_at > clock_timestamp()
  AND c.released_at IS NULL
  AND c.lease_expires_at > clock_timestamp()
  AND ws.revoked_at IS NULL
  AND ws.expires_at > clock_timestamp()
  AND j.state IN ('CLAIMED', 'RUNNING')
  AND j.desired_state = 'RUNNING'
  AND j.resource_project_id = t.resource_project_id
  AND j.actor_id = t.actor_id
  AND p.suspended IS FALSE
  AND p.create_success IS TRUE`, tokenSHA256[:]).
		Scan(&project, &actor, &sources, &expiresAt, &execution, &claimGen, &memoryToolkits, &conversationBound)
	if errors.Is(err, pgx.ErrNoRows) {
		return VectorClaimTokenFacts{}, ErrVectorClaimTokenFactsNotFound
	}
	if err != nil {
		return VectorClaimTokenFacts{}, fmt.Errorf("read vector claim token facts: %w", err)
	}
	actorID, err := strconv.ParseInt(actor, 10, 64)
	if err != nil || actorID <= 0 || project <= 0 {
		return VectorClaimTokenFacts{}, ErrVectorClaimTokenFactsNotFound
	}
	facts := VectorClaimTokenFacts{
		ProjectID:         int64(project),
		ActorID:           actorID,
		Sources:           sources,
		ExpiresAt:         expiresAt.UTC(),
		ConversationBound: conversationBound,
	}
	// Only a token admitted for memory has memory decisions to make, and only
	// when the request says what it is about to do.
	if intent == MemoryIntentSharedIndexWrite && !slices.Contains(sources, "memory") {
		// A toolkit-index write is decided for every token: an index is
		// shared, whatever sources the token carries.
		refused, err := s.conversationTainted(ctx, execution, claimGen)
		if err != nil {
			slog.WarnContext(ctx, "vector claim token: could not read whether the conversation is tainted; the shared write is refused", "err", err)
			refused = true
		}
		facts.ProjectMemoryWriteRefused = refused
		return facts, nil
	}
	if !slices.Contains(sources, "memory") {
		return facts, nil
	}
	facts.MemoryToolkits = memoryToolkits
	switch intent {
	case MemoryIntentPersonalRead, MemoryIntentPersonalWrite:
		turn, allowed := s.personalMemoryTurn(ctx, execution, claimGen, actor)
		facts.PersonalMemoryAllowed = allowed
		if allowed && intent == MemoryIntentPersonalRead {
			// Recorded before the read runs: from here on, no execution bound
			// to this conversation writes shared memory or a shared index.
			if err := s.taintConversation(ctx, turn); err != nil {
				slog.WarnContext(ctx, "vector claim token: could not record the personal memory read; personal memory is withheld", "err", err)
				facts.PersonalMemoryAllowed = false
			}
		}
	case MemoryIntentProjectWrite, MemoryIntentSharedIndexWrite:
		refused, err := s.conversationTainted(ctx, execution, claimGen)
		if err != nil {
			// A doubt about whether shared memory may be written refuses it.
			slog.WarnContext(ctx, "vector claim token: could not read whether the conversation is tainted; the shared write is refused", "err", err)
			refused = true
		}
		facts.ProjectMemoryWriteRefused = refused
	}
	return facts, nil
}

// conversationTurn names the conversation an execution is bound to: the
// tenant (projection project) and the conversation uuid Main's admission
// wrote. A zero value names none.
type conversationTurn struct {
	project      int64
	conversation string
	// origin is the execution's trigger_origin (shared 0140).
	origin string
}

// conversationLock serialises the taint and its check for ONE conversation
// (every execution bound to it): both take this transaction-scoped advisory
// lock first, so a personal read and a shared write in one conversation,
// asked in parallel, are decided one after the other.
const conversationLock = `SELECT pg_advisory_xact_lock(hashtextextended('elitea_memory_conversation:' || ($1::bigint)::text || ':' || $2::text, 0))`

// taintConversation records that personal memory was read in turn's
// conversation. Never cleared; the row goes with the conversation.
func (s *VectorClaimTokens) taintConversation(ctx context.Context, turn conversationTurn) error {
	if turn.project <= 0 || turn.conversation == "" {
		return errors.New("no conversation to taint")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, conversationLock, turn.project, turn.conversation); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.memory_conversation_taints (project_id, conversation_uuid)
VALUES ($1, $2::uuid)
ON CONFLICT DO NOTHING`, turn.project, turn.conversation)
		return err
	})
}

// conversationTainted reports whether the execution's conversation is
// tainted, under the conversation's lock. An execution bound to no
// conversation (a direct tool call, an index run) is not: it cannot have read
// personal memory, which needs a private chat turn.
func (s *VectorClaimTokens) conversationTainted(ctx context.Context, executionID string, generation int64) (bool, error) {
	return s.ExecutionConversationTainted(ctx, executionID, generation)
}

// ExecutionConversationTainted reports whether the execution is bound to a
// conversation in which personal memory was read, under the conversation's
// lock: the check of every shared write (elitea-vector's shared writes
// through Facts, Main's own remove_index through the claim remover). An
// execution with no conversation is not.
func (s *VectorClaimTokens) ExecutionConversationTainted(ctx context.Context, executionID string, generation int64) (bool, error) {
	if s == nil || s.pool == nil {
		return false, errors.New("no database")
	}
	turn, err := s.executionConversation(ctx, executionID, generation)
	if err != nil {
		return false, err
	}
	if turn.conversation == "" {
		return false, nil
	}
	var tainted bool
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, conversationLock, turn.project, turn.conversation); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM elitea_runtime.memory_conversation_taints
    WHERE project_id = $1 AND conversation_uuid = $2::uuid)`, turn.project, turn.conversation).Scan(&tainted)
	})
	return tainted, err
}

// executionConversation is THE binding of an execution to a conversation, the
// one query both the personal-memory decision and the taint use: the
// conversation Main's admission bound it to (agent_execution_jobs.
// client_stream_id, a uuid), its projection project and its trigger origin.
// None for an execution without an agent job or with a stream that is not a
// uuid.
func (s *VectorClaimTokens) executionConversation(ctx context.Context, executionID string, generation int64) (conversationTurn, error) {
	var (
		projection int64
		stream     string
		origin     string
	)
	err := s.pool.QueryRow(ctx, `
SELECT j.projection_project_id, a.client_stream_id, j.trigger_origin
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
WHERE j.execution_id = $1 AND j.generation = $2`, executionID, generation).Scan(&projection, &stream, &origin)
	if errors.Is(err, pgx.ErrNoRows) {
		return conversationTurn{}, nil
	}
	if err != nil {
		return conversationTurn{}, err
	}
	parsed, err := uuid.Parse(stream)
	if err != nil {
		return conversationTurn{}, nil
	}
	return conversationTurn{project: projection, conversation: parsed.String(), origin: origin}, nil
}
