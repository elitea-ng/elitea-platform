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
	"encoding/json"
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
	// is recorded on the claim.
	MemoryIntentPersonalRead
	// MemoryIntentPersonalWrite is a write in the personal scope.
	MemoryIntentPersonalWrite
	// MemoryIntentProjectWrite is a write in the project scope; refused once
	// the run has read personal memory.
	MemoryIntentProjectWrite
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
	// MemorySource, when non-empty, is added to Sources only if the claim's
	// frozen toolkits include a memory toolkit. A claim without one gets no
	// memory source, so an older elitea-vector that cannot name it is never
	// shown one, and a run with no memory toolkit has no memory reach.
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
	// conversation as it is now (see personalMemoryAllowed), and only for a
	// personal memory intent; elitea-vector refuses the personal memory scope
	// without it.
	PersonalMemoryAllowed bool
	// ProjectMemoryWriteRefused is true for a project-write intent from a run
	// that has already read personal memory.
	ProjectMemoryWriteRefused bool
	// MemoryToolkits are the memory toolkits the claim was given (all of the
	// token's project); elitea-vector refuses a memory request for any other.
	MemoryToolkits []int64
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
	mintSources := request.Sources
	memoryToolkits := []int64{}
	if request.MemorySource != "" {
		memoryToolkits = s.claimMemoryToolkits(ctx, fence.ExecutionID, int64(fence.Generation))
		if len(memoryToolkits) > 0 {
			mintSources = append(append([]string(nil), request.Sources...), request.MemorySource)
		}
	}
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
           qualifying.resource_project_id, qualifying.actor_id, $7::text[], qualifying.expires_at,
           $12::bigint[]
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
		request.TokenSHA256[:], request.Capabilities, mintSources,
		request.MaxLifetime.Milliseconds(),
		int64(fence.ClaimAttempt), int64(fence.LeaseEpoch), fence.CommandID,
		memoryToolkits,
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

// claimMemoryToolkits lists the ids of the memory toolkits the execution's
// frozen input names (the agent version snapshot in its input bundle: entries
// of type "memory" with a positive id). Main froze them from the run's own
// project, so every id is a toolkit of the token's project. Any doubt (no
// bundle, unreadable content, a failed read) answers none: the claim then gets
// no memory source and no memory toolkit.
func (s *VectorClaimTokens) claimMemoryToolkits(ctx context.Context, executionID string, generation int64) []int64 {
	rows, err := s.pool.Query(ctx, `
SELECT entry.content_bytes
FROM elitea_runtime.execution_jobs AS job
JOIN elitea_runtime.input_bundle_entries AS entry ON entry.input_bundle_id = job.input_bundle_id
WHERE job.execution_id = $1 AND job.generation = $2
  AND entry.media_type LIKE '%json%'`, executionID, generation)
	if err != nil {
		slog.WarnContext(ctx, "vector claim token: could not read the execution's frozen toolkits; no memory source", "err", err)
		return nil
	}
	defer rows.Close()
	found := map[int64]bool{}
	for rows.Next() {
		var content []byte
		if err := rows.Scan(&content); err != nil {
			return nil
		}
		var decoded any
		if json.Unmarshal(content, &decoded) == nil {
			collectMemoryToolkits(decoded, found)
		}
	}
	if rows.Err() != nil {
		return nil
	}
	ids := make([]int64, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// collectMemoryToolkits walks a decoded JSON document for tool entries of
// type "memory" (an object with that type, a settings object and a positive
// integer id) and records their ids.
func collectMemoryToolkits(node any, found map[int64]bool) {
	switch value := node.(type) {
	case map[string]any:
		if kind, _ := value["type"].(string); kind == "memory" {
			_, configured := value["settings"].(map[string]any)
			if id, ok := value["id"].(float64); ok && configured && id >= 1 && id <= 1<<53 && id == float64(int64(id)) {
				found[int64(id)] = true
			}
		}
		for _, child := range value {
			collectMemoryToolkits(child, found)
		}
	case []any:
		for _, child := range value {
			collectMemoryToolkits(child, found)
		}
	}
}

// personalMemoryAllowed says whether the claim's execution may use PERSONAL
// memory right now. It is decided at the introspection of a personal-scope
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
func (s *VectorClaimTokens) personalMemoryAllowed(ctx context.Context, executionID string, generation int64, actor string) bool {
	var (
		projection int64
		stream     string
	)
	err := s.pool.QueryRow(ctx, `
SELECT j.projection_project_id, a.client_stream_id
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
WHERE j.execution_id = $1 AND j.generation = $2
  AND j.trigger_origin = 'manual'`, executionID, generation).Scan(&projection, &stream)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		slog.WarnContext(ctx, "vector claim token: could not read the execution; personal memory is withheld", "err", err)
		return false
	}
	// Not a uuid: no conversation. Checked here as well as by the cast below,
	// so a malformed stream never reaches the tenant query.
	if _, err := uuid.Parse(stream); err != nil {
		return false
	}
	schema, err := tenantschema.QuoteInt(projection)
	if err != nil {
		return false
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
WHERE conversation.uuid = $1::uuid`, schema), stream, executionID, actor).Scan(&conversations, &private, &people, &isActor)
	if err != nil {
		slog.WarnContext(ctx, "vector claim token: could not read the turn's conversation; personal memory is withheld", "err", err)
		return false
	}
	return conversations == 1 && private && people == 1 && isActor
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
		project        int32
		actor          string
		sources        []string
		expiresAt      time.Time
		execution      string
		claimGen       int64
		memoryToolkits []int64
	)
	err := s.pool.QueryRow(ctx, `
SELECT t.resource_project_id, t.actor_id, t.allowed_sources, t.expires_at, t.execution_id, t.generation,
       t.memory_toolkit_ids
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
		Scan(&project, &actor, &sources, &expiresAt, &execution, &claimGen, &memoryToolkits)
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
		ProjectID: int64(project),
		ActorID:   actorID,
		Sources:   sources,
		ExpiresAt: expiresAt.UTC(),
	}
	// Only a token admitted for memory has memory decisions to make, and only
	// when the request says what it is about to do.
	if !slices.Contains(sources, "memory") {
		return facts, nil
	}
	facts.MemoryToolkits = memoryToolkits
	switch intent {
	case MemoryIntentPersonalRead, MemoryIntentPersonalWrite:
		facts.PersonalMemoryAllowed = s.personalMemoryAllowed(ctx, execution, claimGen, actor)
		if facts.PersonalMemoryAllowed && intent == MemoryIntentPersonalRead {
			// Recorded before the read runs: from here on, project memory is
			// not written by this run (any claim of this execution).
			if err := s.recordPersonalMemoryRead(ctx, execution, tokenSHA256); err != nil {
				slog.WarnContext(ctx, "vector claim token: could not record the personal memory read; personal memory is withheld", "err", err)
				facts.PersonalMemoryAllowed = false
			}
		}
	case MemoryIntentProjectWrite:
		refused, err := s.personalMemoryWasRead(ctx, execution)
		if err != nil {
			// A doubt about whether shared memory may be written refuses it.
			slog.WarnContext(ctx, "vector claim token: could not read whether the run read personal memory; the project write is refused", "err", err)
			refused = true
		}
		facts.ProjectMemoryWriteRefused = refused
	}
	return facts, nil
}

// memoryRunLock serialises the personal-read record and the project-write
// check of ONE execution (every generation and claim of it): both take this
// transaction-scoped advisory lock first. So a personal read and a project
// write of one run, asked in parallel, are decided one after the other: either
// the read is recorded first and the write is refused, or the write is
// admitted first, and then its content was fixed before the read was even
// admitted, so it cannot carry what the read returns.
const memoryRunLock = `SELECT pg_advisory_xact_lock(hashtextextended('elitea_vector_memory_run:' || $1::text, 0))`

// recordPersonalMemoryRead sets personal_memory_read on the token row, under
// the execution's memory lock.
func (s *VectorClaimTokens) recordPersonalMemoryRead(ctx context.Context, executionID string, tokenSHA256 [32]byte) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, memoryRunLock, executionID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`UPDATE elitea_runtime.vector_claim_tokens SET personal_memory_read = true WHERE token_sha256 = $1 AND execution_id = $2`,
			tokenSHA256[:], executionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("the token row is gone")
		}
		return nil
	})
}

// personalMemoryWasRead reports whether any token row of the execution (any
// generation, any claim) has recorded a personal read, under the execution's
// memory lock.
func (s *VectorClaimTokens) personalMemoryWasRead(ctx context.Context, executionID string) (bool, error) {
	var read bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, memoryRunLock, executionID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM elitea_runtime.vector_claim_tokens
    WHERE execution_id = $1 AND personal_memory_read)`, executionID).Scan(&read)
	})
	return read, err
}
