package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerruncaps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runcaps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// RunCapsRepository records the effective run caps of an agent run, cancels
// a run past its spend cap, and records cap override grants (ADR-0041
// decision 3, shared 0212).
type RunCapsRepository struct {
	pool *pgxpool.Pool
}

// NewRunCapsRepository builds the repository.
func NewRunCapsRepository(pool *pgxpool.Pool) (*RunCapsRepository, error) {
	if pool == nil {
		return nil, errors.New("run caps database is required")
	}
	return &RunCapsRepository{pool: pool}, nil
}

// runCapsRecord is execution_jobs.run_caps.
type runCapsRecord struct {
	MaxToolCalls   uint32  `json:"max_tool_calls"`
	MaxModelTurns  uint32  `json:"max_model_turns"`
	MaxRecordsRead uint64  `json:"max_records_read"`
	MaxSpend       float64 `json:"max_spend"`
	Override       *string `json:"override"`
}

func (r runCapsRecord) caps() platformconfig.TierRunCaps {
	return platformconfig.TierRunCaps{
		MaxToolCalls: r.MaxToolCalls, MaxModelTurns: r.MaxModelTurns,
		MaxRecordsRead: r.MaxRecordsRead, MaxSpend: r.MaxSpend,
	}
}

// BindRunCaps records the run's caps and answers the caps the run gets.
//
// The execution's row is locked first, so two replicas preparing the same
// command agree: the first record of an execution wins and a later
// generation (a redispatch) copies it. With no record yet, an open, unexpired
// override grant of the run's conversation is consumed and raises its cap.
func (r *RunCapsRepository) BindRunCaps(ctx context.Context, binding workerruncaps.Binding) (platformconfig.TierRunCaps, error) {
	if binding.ExecutionID == "" || binding.Generation == 0 || binding.ProjectID <= 0 {
		return platformconfig.TierRunCaps{}, errors.New("run caps: execution identity is required")
	}
	var bound platformconfig.TierRunCaps
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var locked int
		if err := tx.QueryRow(ctx, `
SELECT count(*) FROM (
    SELECT 1 FROM elitea_runtime.execution_jobs
    WHERE execution_id = $1
    ORDER BY generation
    FOR UPDATE
) AS rows`, binding.ExecutionID).Scan(&locked); err != nil {
			return fmt.Errorf("lock the execution: %w", err)
		}
		if locked == 0 {
			bound = binding.Caps
			return nil
		}
		var stored []byte
		err := tx.QueryRow(ctx, `
SELECT run_caps FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND run_caps IS NOT NULL
ORDER BY generation LIMIT 1`, binding.ExecutionID).Scan(&stored)
		record := runCapsRecord{}
		switch {
		case err == nil:
			if err := json.Unmarshal(stored, &record); err != nil {
				return fmt.Errorf("read the recorded run caps: %w", err)
			}
		case errors.Is(err, pgx.ErrNoRows):
			record = runCapsRecord{
				MaxToolCalls: binding.Caps.MaxToolCalls, MaxModelTurns: binding.Caps.MaxModelTurns,
				MaxRecordsRead: binding.Caps.MaxRecordsRead, MaxSpend: binding.Caps.MaxSpend,
			}
			cap, claimErr := claimOverride(ctx, tx, binding)
			if claimErr != nil {
				return claimErr
			}
			if cap != "" {
				record.Override = &cap
				raised := record.caps().Raised(cap)
				record.MaxToolCalls, record.MaxModelTurns = raised.MaxToolCalls, raised.MaxModelTurns
				record.MaxRecordsRead, record.MaxSpend = raised.MaxRecordsRead, raised.MaxSpend
			}
		default:
			return fmt.Errorf("read the recorded run caps: %w", err)
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs SET run_caps = $3::jsonb
WHERE execution_id = $1 AND generation = $2 AND run_caps IS NULL`,
			binding.ExecutionID, int64(binding.Generation), encoded); err != nil {
			return fmt.Errorf("record the run caps: %w", err)
		}
		bound = record.caps()
		return nil
	})
	if undefinedColumnOrTable(err) {
		// A database before 0212: no record, no grant.
		return binding.Caps, nil
	}
	return bound, err
}

// claimOverride consumes the conversation's open grant for this run: ""
// when there is none. The grant is on the project that holds the
// conversation (the run's projection project, which for a borrowed agent is
// not its resource project). A run of the stopped response itself (a
// regenerate) does not consume it.
func claimOverride(ctx context.Context, tx pgx.Tx, binding workerruncaps.Binding) (string, error) {
	var cap string
	err := tx.QueryRow(ctx, `
UPDATE elitea_runtime.run_cap_overrides AS grant_row
SET consumed_execution_id = $1, consumed_at = clock_timestamp()
FROM elitea_runtime.agent_execution_jobs AS agent
JOIN elitea_runtime.execution_jobs AS job
  ON job.execution_id = agent.execution_id AND job.generation = agent.generation
WHERE agent.execution_id = $1
  AND agent.generation = $2
  AND grant_row.project_id = job.projection_project_id
  AND grant_row.conversation_uuid = agent.client_stream_id
  AND grant_row.source_message_id <> agent.client_message_id
  AND grant_row.consumed_execution_id IS NULL
  AND grant_row.expires_at > clock_timestamp()
RETURNING grant_row.cap`, binding.ExecutionID, int64(binding.Generation)).Scan(&cap)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("consume the run cap override: %w", err)
	}
	return cap, nil
}

// SpendCapCancel is one run the spend routine cancelled.
type SpendCapCancel struct {
	ExecutionID string
	Generation  uint64
	ProjectID   int64
	Spent       float64
	MaxSpend    float64
}

// enforceSpendCapsSQL cancels every running agent run whose recorded
// max_spend is positive and whose spend so far reached it. Spend is the
// run's gateway request log rows: model calls at the price catalogue rate
// and metered tool operations at their cost. A run another stop already
// cancelled keeps that stop.
const enforceSpendCapsSQL = `
WITH capped AS (
    SELECT j.execution_id, j.generation, j.resource_project_id, j.admitted_at,
           (j.run_caps->>'max_spend')::numeric AS max_spend
    FROM elitea_runtime.execution_jobs AS j
    WHERE j.desired_state = 'RUNNING'
      AND j.state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')
      AND j.run_caps IS NOT NULL
      AND j.capability_id IN ('agent.execute.application.v1', 'agent.execute.adhoc.v1')
      AND jsonb_typeof(j.run_caps->'max_spend') = 'number'
      AND (j.run_caps->>'max_spend')::numeric > 0
), spent AS (
    SELECT c.execution_id, c.generation, c.resource_project_id, c.max_spend,
           coalesce(sum(
               CASE WHEN l.operation = 'tool' THEN coalesce(l.cost_usd, 0)
                    ELSE l.prompt_tokens::numeric * coalesce(m.input_cost_per_1m_tokens, 0) / 1000000
                       + l.completion_tokens::numeric * coalesce(m.output_cost_per_1m_tokens, 0) / 1000000
               END), 0) AS spent
    FROM capped AS c
    JOIN gateway.llm_request_logs AS l
      ON l.project_id = c.resource_project_id
     AND l.execution_id = c.execution_id
     AND l.occurred_at >= c.admitted_at
    LEFT JOIN gateway.gateway_models AS m
      ON m.provider = l.provider AND m.model_name = l.model
    GROUP BY c.execution_id, c.generation, c.resource_project_id, c.max_spend
)
UPDATE elitea_runtime.execution_jobs AS j
SET desired_state = 'CANCELLED', cancel_reason = 'cap_reached', cancel_cap = 'max_spend'
FROM spent AS s
WHERE j.execution_id = s.execution_id
  AND j.generation = s.generation
  AND j.desired_state = 'RUNNING'
  AND s.spent >= s.max_spend
RETURNING j.execution_id, j.generation, s.resource_project_id, s.spent::float8, s.max_spend::float8`

// EnforceSpendCaps cancels the runs past their spend cap and answers them.
// The worker learns of the cancel at its next lease renewal, as for every
// other stop. A database without the request log, the price catalogue or
// 0212 has nothing to enforce.
func (r *RunCapsRepository) EnforceSpendCaps(ctx context.Context) ([]SpendCapCancel, error) {
	rows, err := r.pool.Query(ctx, enforceSpendCapsSQL)
	if err != nil {
		if undefinedColumnOrTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("enforce the run spend caps: %w", err)
	}
	defer rows.Close()
	var cancelled []SpendCapCancel
	for rows.Next() {
		var row SpendCapCancel
		var generation int64
		if err := rows.Scan(&row.ExecutionID, &generation, &row.ProjectID, &row.Spent, &row.MaxSpend); err != nil {
			return nil, fmt.Errorf("read a run past its spend cap: %w", err)
		}
		row.Generation = uint64(generation)
		cancelled = append(cancelled, row)
	}
	if err := rows.Err(); err != nil {
		if undefinedColumnOrTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("enforce the run spend caps: %w", err)
	}
	return cancelled, nil
}

var (
	runCapAuditOnce     sync.Once
	runCapAuditAppender securityaudit.Appender
	runCapAuditErr      error
)

func runCapOverrideAppender() (securityaudit.Appender, error) {
	runCapAuditOnce.Do(func() {
		runCapAuditAppender, runCapAuditErr = securityaudit.NewPostgresAppender("elitea-main")
	})
	return runCapAuditAppender, runCapAuditErr
}

// GrantRunCapOverride records a grant on a response that stopped at a cap,
// with its security audit event, in one transaction. The response must be
// in the conversation (when the request names one) and carry error_code
// CAP_REACHED and a known cap. The grant is on the response's conversation.
func (r *RunCapsRepository) GrantRunCapOverride(
	ctx context.Context, request workerruncaps.OverrideRequest,
) (workerruncaps.OverrideGrant, error) {
	schema, err := currentProjectSchema(request.ProjectID)
	if err != nil || request.ProjectID > math.MaxInt32 {
		return workerruncaps.OverrideGrant{}, workerruncaps.ErrOverrideInvalid
	}
	appender, err := runCapOverrideAppender()
	if err != nil {
		return workerruncaps.OverrideGrant{}, fmt.Errorf("run cap override: the audit appender: %w", err)
	}
	var grant workerruncaps.OverrideGrant
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var cap, sourceExecution, conversationUUID string
		err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT coalesce(message_group.meta->>'cap', ''), coalesce(message_group.task_id, ''), conversation.uuid::text
FROM %s AS message_group
JOIN %s AS conversation ON conversation.id = message_group.conversation_id
WHERE message_group.uuid::text = $1
  AND ($2 = '' OR conversation.uuid::text = $2)
  AND message_group.meta->>'error_code' = $3`,
			schema+".chat_message_group", schema+".chat_conversations"),
			request.MessageID, request.ConversationUUID, runcaps.Code).Scan(&cap, &sourceExecution, &conversationUUID)
		if errors.Is(err, pgx.ErrNoRows) {
			return workerruncaps.ErrOverrideNotCapped
		}
		if err != nil {
			return fmt.Errorf("read the stopped response: %w", err)
		}
		if cap == "" {
			// A wall cap response written before the cap name was stored.
			cap = runcaps.MaxWallSeconds
		}
		if !runcaps.Valid(cap) {
			return workerruncaps.ErrOverrideNotCapped
		}
		if workerruncaps.WorkerCap(cap) && !request.AllowWorkerCaps {
			return workerruncaps.ErrOverrideNeedsTierCaps
		}
		var source *string
		if sourceExecution != "" && len(sourceExecution) <= 128 {
			source = &sourceExecution
		}
		err = tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.run_cap_overrides (
    project_id, conversation_uuid, source_message_id, source_execution_id,
    cap, granted_by, expires_at
) VALUES ($1, $2, $3, $4, $5, $6, clock_timestamp() + ($7::bigint * interval '1 millisecond'))
RETURNING id, granted_at, expires_at`,
			request.ProjectID, conversationUUID, request.MessageID, source, cap,
			request.ActorUserID, request.ExpiresIn.Milliseconds(),
		).Scan(&grant.ID, &grant.GrantedAt, &grant.ExpiresAt)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				if pgErr.ConstraintName == "run_cap_overrides_open" {
					return workerruncaps.ErrOverrideOpenGrant
				}
				return workerruncaps.ErrOverrideAlreadyGranted
			}
			return fmt.Errorf("record the run cap override: %w", err)
		}
		grant.Cap, grant.SourceExecutionID = cap, sourceExecution
		eventID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		return appender.Append(ctx, tx, securityaudit.SecurityEvent{
			ID:              eventID.String(),
			Type:            securityaudit.TypeAgentCapOverride,
			TenantProjectID: request.ProjectID,
			Action:          "agent.cap.override.granted",
			Effect:          securityaudit.EffectAllow,
			Reasons:         []string{"cap:" + cap, "raise:double", "ttl_seconds:" + strconv.FormatInt(int64(request.ExpiresIn/time.Second), 10)},
			PrincipalRef:    auditUserRef(strconv.FormatInt(request.ActorUserID, 10)),
			ActorFidelity:   securityaudit.FidelityAttributed,
			ExecutionID:     auditText(sourceExecution),
			DecisionID:      strconv.FormatInt(grant.ID, 10),
			ResourceType:    "chat_message",
			ResourceID:      request.MessageID,
		})
	})
	if err != nil {
		if undefinedColumnOrTable(err) {
			return workerruncaps.OverrideGrant{}, fmt.Errorf("run cap override: shared migration 0212 has not run: %w", err)
		}
		return workerruncaps.OverrideGrant{}, err
	}
	return grant, nil
}

// undefinedColumnOrTable reports PostgreSQL's undefined_table (42P01),
// undefined_column (42703) or undefined_schema (3F000).
func undefinedColumnOrTable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "42P01", "42703", "3F000":
		return true
	default:
		return false
	}
}
