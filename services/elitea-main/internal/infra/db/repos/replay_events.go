package repos

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	executionsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/executions"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxReplayBatch = 100

const replayEventReset = "execution.replay_reset"

type ReplayEventsRepository struct {
	store sqlExecutor
}

func NewReplayEventsRepository(pool *pgxpool.Pool) (*ReplayEventsRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return &ReplayEventsRepository{store: store}, nil
}

func newReplayEventsRepository(store sqlExecutor) *ReplayEventsRepository {
	return &ReplayEventsRepository{store: store}
}

func (r *ReplayEventsRepository) Replay(ctx context.Context, projectID, executionID string, afterCursor uint64, limit int) ([]executionsapi.DurableEvent, error) {
	projectionProjectID, err := parseProjectID(projectID)
	if err != nil || executionID == "" || afterCursor > math.MaxInt64 || limit <= 0 || limit > maxReplayBatch {
		return nil, executionsapi.ErrInvalidEventStream
	}
	var prunedThrough, highCursor int64
	var executionPresent, cursorRecognized bool
	err = r.store.QueryRow(ctx, `
WITH matching_execution AS MATERIALIZED (
    SELECT execution_id, generation
    FROM elitea_runtime.execution_jobs
    WHERE projection_project_id = $1
      AND execution_id = $2
), replay_bounds AS MATERIALIZED (
    SELECT COALESCE(max(s.pruned_through_cursor), 0) AS pruned_through,
           greatest(
               COALESCE(max(s.last_node_cursor), 0),
               COALESCE((
                   SELECT max(r.cursor)
                   FROM elitea_runtime.execution_replay_events AS r
                   WHERE r.projection_project_id = $1
                     AND r.execution_id = $2
               ), 0)
           ) AS high_cursor
    FROM elitea_runtime.execution_replay_state AS s
    JOIN matching_execution AS m
      ON m.execution_id = s.execution_id
     AND m.generation = s.generation
)
SELECT replay_bounds.pruned_through,
       replay_bounds.high_cursor,
       EXISTS (SELECT 1 FROM matching_execution),
       $3::bigint = 0
       OR $3::bigint = replay_bounds.pruned_through
       OR EXISTS (
           SELECT 1
           FROM elitea_runtime.execution_replay_events AS r
           WHERE r.projection_project_id = $1
             AND r.execution_id = $2
             AND r.cursor = $3
       )
FROM replay_bounds`,
		projectionProjectID,
		executionID,
		int64(afterCursor),
	).Scan(&prunedThrough, &highCursor, &executionPresent, &cursorRecognized)
	if err != nil {
		return nil, fmt.Errorf("query durable execution replay bounds: %w", err)
	}
	if !executionPresent || prunedThrough < 0 || highCursor < prunedThrough {
		return nil, executionsapi.ErrInvalidEventStream
	}

	effectiveCursor := int64(afterCursor)
	events := make([]executionsapi.DurableEvent, 0, limit)
	if effectiveCursor < prunedThrough {
		resetData, marshalErr := json.Marshal(struct {
			Reason string `json:"reason"`
		}{
			Reason: "progress_retention_window_elapsed",
		})
		if marshalErr != nil {
			return nil, fmt.Errorf("encode execution replay reset: %w", marshalErr)
		}
		events = append(events, executionsapi.DurableEvent{
			Cursor: uint64(prunedThrough),
			Type:   replayEventReset,
			Data:   resetData,
		})
		effectiveCursor = prunedThrough
	} else if effectiveCursor > highCursor || !cursorRecognized {
		return nil, executionsapi.ErrInvalidEventStream
	}
	if len(events) == limit {
		return events, nil
	}
	if effectiveCursor == highCursor {
		return events, nil
	}

	rows, err := r.store.Query(ctx, `
SELECT replay.cursor,
       replay.event_type,
       replay.event_bytes,
       replay.event_digest,
       CASE
           WHEN replay.event_type = 'execution.node_event'
            AND COALESCE(convert_from(replay.event_bytes, 'UTF8')::jsonb ->> 'type', '') IN (
                'full_message',
                'agent_hitl_interrupt',
                'mcp_authorization_required'
            )
           THEN EXISTS (
               SELECT 1
               FROM elitea_runtime.output_inbox AS terminal
               WHERE terminal.execution_id = replay.execution_id
                 AND terminal.generation = replay.generation
                 AND terminal.payload_type = 'AGENT_EXECUTION_RESULT'
                 AND terminal.projected_at IS NOT NULL
           )
           ELSE TRUE
       END AS visible
FROM elitea_runtime.execution_replay_events AS replay
WHERE replay.projection_project_id = $1
  AND replay.execution_id = $2
  AND replay.cursor > $3
ORDER BY replay.cursor
LIMIT $4`, projectionProjectID, executionID, effectiveCursor, limit-len(events))
	if err != nil {
		return nil, fmt.Errorf("query durable execution replay: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var cursor int64
		var event executionsapi.DurableEvent
		var digestBytes []byte
		var visible bool
		if err := rows.Scan(&cursor, &event.Type, &event.Data, &digestBytes, &visible); err != nil {
			return nil, fmt.Errorf("scan durable execution replay: %w", err)
		}
		if cursor <= 0 || len(event.Data) == 0 || len(event.Data) > 64*1024 || !json.Valid(event.Data) {
			return nil, executionsapi.ErrInvalidEventStream
		}
		digest, err := storedDigest(digestBytes)
		if err != nil || runtimedomain.SHA256(event.Data) != digest {
			return nil, executionsapi.ErrInvalidEventStream
		}
		if !visible {
			// A later cursor must not make this projection-hidden event unreachable.
			break
		}
		event.Cursor = uint64(cursor)
		event.Data = append(json.RawMessage(nil), event.Data...)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate durable execution replay: %w", err)
	}
	if events == nil {
		return []executionsapi.DurableEvent{}, nil
	}
	return events, nil
}

var _ executionsapi.EventRepository = (*ReplayEventsRepository)(nil)
