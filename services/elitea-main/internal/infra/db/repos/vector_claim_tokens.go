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
	"strconv"
	"time"

	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrVectorClaimTokenFactsNotFound reports a hash that names no live worker
// claim token: unknown, expired, or its claim or execution is no longer live.
var ErrVectorClaimTokenFactsNotFound = errors.New("vector claim token facts not found")

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
        resource_project_id, actor_id, allowed_sources, expires_at
    )
    SELECT $5, qualifying.claim_id, qualifying.execution_id, qualifying.generation,
           qualifying.resource_project_id, qualifying.actor_id, $7::text[], qualifying.expires_at
    FROM qualifying
    ON CONFLICT (claim_id) DO UPDATE
    SET token_sha256 = EXCLUDED.token_sha256,
        allowed_sources = EXCLUDED.allowed_sources,
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

// Facts reads the token whose SHA-256 is tokenSHA256. Only a live token
// answers: unexpired; its claim unreleased with an unexpired lease and an
// unrevoked, unexpired workload session; its execution CLAIMED or RUNNING
// with desired state RUNNING; its project active. Anything else is
// ErrVectorClaimTokenFactsNotFound.
func (s *VectorClaimTokens) Facts(ctx context.Context, tokenSHA256 [32]byte) (VectorClaimTokenFacts, error) {
	if s == nil || s.pool == nil {
		return VectorClaimTokenFacts{}, fmt.Errorf("%w: no database", ErrVectorClaimTokenFactsNotFound)
	}
	var (
		project   int32
		actor     string
		sources   []string
		expiresAt time.Time
	)
	err := s.pool.QueryRow(ctx, `
SELECT t.resource_project_id, t.actor_id, t.allowed_sources, t.expires_at
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
		Scan(&project, &actor, &sources, &expiresAt)
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
	return VectorClaimTokenFacts{
		ProjectID: int64(project),
		ActorID:   actorID,
		Sources:   sources,
		ExpiresAt: expiresAt.UTC(),
	}, nil
}
