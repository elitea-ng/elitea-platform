package repos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	replayEventNodeEvent          = "execution.node_event"
	defaultMaxProgressEvents      = int64(2048)
	defaultMaxProgressBytes       = int64(8 * 1024 * 1024)
	defaultMaxProgressAge         = 5 * time.Minute
	defaultReplayJanitorBatchSize = 100
	maxTaskRestampSourceEventID   = 512
)

type replayRetentionPolicy struct {
	maxProgressEvents int64
	maxProgressBytes  int64
	maxProgressAge    time.Duration
	janitorBatchSize  int
}

func defaultReplayRetentionPolicy() replayRetentionPolicy {
	return replayRetentionPolicy{
		maxProgressEvents: defaultMaxProgressEvents,
		maxProgressBytes:  defaultMaxProgressBytes,
		maxProgressAge:    defaultMaxProgressAge,
		janitorBatchSize:  defaultReplayJanitorBatchSize,
	}
}

func (p replayRetentionPolicy) validate() error {
	if p.maxProgressEvents <= 0 || p.maxProgressBytes <= 0 || p.maxProgressAge <= 0 || p.janitorBatchSize <= 0 || p.janitorBatchSize > 1000 {
		return errors.New("invalid execution replay retention policy")
	}
	return nil
}

// NodeEventsRepository appends current-NodeEvent JSON directly to the durable
// execution replay log. Redis is deliberately absent from this data path.
type NodeEventsRepository struct {
	store        sharedStore
	retention    replayRetentionPolicy
	activity     currentIndexActivityProjector
	agentTrace   currentAgentTraceProjector
	agentText    currentAgentTextProjector
	agentContext currentAgentContextProjector
}

func NewNodeEventsRepository(pool *pgxpool.Pool) (*NodeEventsRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	repository, err := newNodeEventsRepository(store)
	if err != nil {
		return nil, err
	}
	repository.activity = &postgresCurrentIndexActivityProjector{}
	repository.agentTrace = &postgresCurrentAgentTraceProjector{}
	repository.agentText = postgresCurrentAgentTextProjector{}
	repository.agentContext = postgresCurrentAgentContextProjector{}
	return repository, nil
}

func newNodeEventsRepository(store sharedStore) (*NodeEventsRepository, error) {
	return newNodeEventsRepositoryWithPolicy(store, defaultReplayRetentionPolicy())
}

func newNodeEventsRepositoryWithPolicy(store sharedStore, retention replayRetentionPolicy) (*NodeEventsRepository, error) {
	if store == nil {
		return nil, errors.New("node event database is required")
	}
	if err := retention.validate(); err != nil {
		return nil, err
	}
	return &NodeEventsRepository{
		store:        store,
		retention:    retention,
		activity:     noopCurrentIndexActivityProjector{},
		agentTrace:   noopCurrentAgentTraceProjector{},
		agentText:    noopCurrentAgentTextProjector{},
		agentContext: noopCurrentAgentContextProjector{},
	}, nil
}

type durableNodeEvent struct {
	Cursor              int64
	ExecutionID         string
	Generation          int64
	ProjectionProjectID int64
	EventType           string
	EventBytes          []byte
	EventDigest         runtimedomain.Digest
}

type replayExecutionState struct {
	LastNodeSequence       int64
	LastNodeEventID        string
	LastNodeEventBytes     []byte
	LastNodeEventDigest    runtimedomain.Digest
	LastNodeCursor         int64
	PrunedThroughCursor    int64
	RetainedProgressEvents int64
	RetainedProgressBytes  int64
}

func (r *NodeEventsRepository) ProjectNodeEvent(ctx context.Context, frame outputapp.NodeEventFrame) (outputapp.ProjectionOutcome, error) {
	if err := frame.Validate(); err != nil || frame.Sequence > math.MaxInt64 || frame.Fence.Generation > math.MaxInt64 || frame.Fence.ClaimAttempt > math.MaxInt64 || frame.Fence.LeaseEpoch > math.MaxInt64 {
		return outputapp.ProjectionOutcome{}, outputapp.ErrInvalidNodeEventOutput
	}
	if int64(len(frame.BrowserData)) > r.retention.maxProgressBytes {
		return outputapp.ProjectionOutcome{}, outputapp.ErrInvalidNodeEventOutput
	}
	resourceProjectID, err := parseProjectID(frame.ResourceProjectID)
	if err != nil {
		return outputapp.ProjectionOutcome{}, outputapp.ErrInvalidNodeEventOutput
	}
	projectionProjectID, err := parseProjectID(frame.ProjectionProjectID)
	if err != nil {
		return outputapp.ProjectionOutcome{}, outputapp.ErrInvalidNodeEventOutput
	}
	eventDigest := runtimedomain.SHA256(frame.BrowserData)
	restampCreatedOn, persistTaskRestamp := frame.CurrentIndexMetaTaskRestampSource()
	if persistTaskRestamp && len(frame.EventID) > maxTaskRestampSourceEventID {
		return outputapp.ProjectionOutcome{}, outputapp.ErrInvalidNodeEventOutput
	}

	var outcome outputapp.ProjectionOutcome
	err = r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		existing, loadErr := loadDurableNodeEvent(ctx, tx, frame.EventID)
		if loadErr == nil {
			if !sameDurableNodeEvent(existing, frame, projectionProjectID, eventDigest) {
				return outputapp.ErrNodeEventOutputConflict
			}
			outcome = outputapp.ProjectionOutcome{Inserted: false, Cursor: uint64(existing.Cursor), CommittedSequence: frame.Sequence}
			return nil
		}
		if !errors.Is(loadErr, pgx.ErrNoRows) {
			return fmt.Errorf("load durable node event: %w", loadErr)
		}
		// Keep the authority lock in its own READ COMMITTED statement. A query
		// that waits for this lock retains its original statement snapshot; the
		// following statement must take a fresh snapshot of sequence/terminal rows.
		locked, lockErr := lockNodeEventAuthority(ctx, tx, frame, resourceProjectID, projectionProjectID)
		if lockErr != nil {
			return lockErr
		}
		if !locked {
			return runtimedomain.ErrStaleFence
		}
		if err := initializeReplayExecutionState(ctx, tx, frame.Fence.ExecutionID, int64(frame.Fence.Generation), projectionProjectID); err != nil {
			return err
		}
		state, stateErr := lockReplayExecutionState(ctx, tx, frame.Fence.ExecutionID, int64(frame.Fence.Generation), projectionProjectID)
		if stateErr != nil {
			return stateErr
		}
		switch {
		case int64(frame.Sequence) == state.LastNodeSequence:
			if state.LastNodeEventID != frame.EventID ||
				state.LastNodeEventDigest != eventDigest ||
				!bytes.Equal(state.LastNodeEventBytes, frame.BrowserData) {
				return outputapp.ErrNodeEventOutputConflict
			}
			outcome = outputapp.ProjectionOutcome{
				Inserted:          false,
				Cursor:            uint64(state.LastNodeCursor),
				CommittedSequence: frame.Sequence,
			}
			return nil
		case int64(frame.Sequence) != state.LastNodeSequence+1:
			return outputapp.ErrNodeEventOutputConflict
		}

		var cursor int64
		var authorityClaimID, desiredState, capabilityID string
		var deadlineExpired, terminalPresent bool
		err := tx.QueryRow(ctx, `
WITH authority AS MATERIALIZED (
    SELECT c.claim_id, j.desired_state, j.capability_id,
           o.deadline <= clock_timestamp() AS deadline_expired
    FROM elitea_runtime.execution_claims AS c
    JOIN elitea_runtime.execution_jobs AS j
      ON j.execution_id = c.execution_id AND j.generation = c.generation
    JOIN elitea_runtime.command_outbox AS o
      ON o.execution_id = j.execution_id AND o.generation = j.generation
    WHERE c.execution_id = $2
      AND c.generation = $3
	  AND j.capability_id IN (
	      'index.ingest.v1',
	      'agent.execute.application.v1',
	      'agent.execute.adhoc.v1'
	  )
      AND j.tenant_id = $8
      AND j.resource_project_id = $9
      AND j.projection_project_id = $4
      AND j.command_id = $10
      AND c.workload_identity = $11
      AND c.workload_session_id = $12
      AND c.producer_id = $13
      AND c.claim_attempt = $14
      AND c.lease_epoch = $15
      AND c.fence_token = $16
      AND c.initial_output_watermark = $18
      AND c.released_at IS NULL
      AND c.lease_expires_at > clock_timestamp()
      AND o.retired_at IS NULL
      AND o.authority_granted_at IS NOT NULL
), terminal_output AS MATERIALIZED (
    SELECT TRUE AS present
    FROM elitea_runtime.output_inbox
    WHERE execution_id = $2 AND generation = $3
    LIMIT 1
), ranked_progress AS MATERIALIZED (
    SELECT r.cursor,
           r.created_at,
           octet_length(r.event_bytes) AS event_size,
           row_number() OVER (ORDER BY r.cursor DESC) AS newest_rank,
           sum(octet_length(r.event_bytes)) OVER (
               ORDER BY r.cursor DESC
               ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
           ) AS newest_bytes
    FROM elitea_runtime.execution_replay_events AS r
    WHERE r.projection_project_id = $4
      AND r.execution_id = $2
      AND r.generation = $3
      AND r.event_type = $5
), deleted_progress AS (
    DELETE FROM elitea_runtime.execution_replay_events AS r
    USING ranked_progress, authority
    WHERE r.cursor = ranked_progress.cursor
      AND authority.desired_state = 'RUNNING'
      AND NOT authority.deadline_expired
      AND NOT EXISTS (SELECT 1 FROM terminal_output)
      AND (
          ranked_progress.created_at
              < clock_timestamp() - ($19::bigint * interval '1 millisecond')
          OR ranked_progress.newest_rank > ($20::bigint - 1)
          OR ranked_progress.newest_bytes > ($21::bigint - octet_length($6::bytea))
      )
    RETURNING r.cursor, octet_length(r.event_bytes) AS event_size
), deleted_summary AS MATERIALIZED (
    SELECT count(*) AS event_count,
           COALESCE(sum(event_size), 0) AS byte_count,
           COALESCE(max(cursor), 0) AS max_cursor
    FROM deleted_progress
), inserted AS (
    INSERT INTO elitea_runtime.execution_replay_events (
        event_id, execution_id, generation, projection_project_id,
        event_type, event_bytes, event_digest
    )
    SELECT $1, $2, $3, $4, $5, $6, $7
    FROM authority
    WHERE authority.desired_state = 'RUNNING'
      AND NOT authority.deadline_expired
      AND NOT EXISTS (SELECT 1 FROM terminal_output)
    ON CONFLICT DO NOTHING
    RETURNING cursor
), updated_state AS (
    UPDATE elitea_runtime.execution_replay_state AS s
    SET last_node_sequence = $17,
        last_node_event_id = $1,
        last_node_event_bytes = $6,
        last_node_event_digest = $7,
        last_node_cursor = inserted.cursor,
        pruned_through_cursor = greatest(
            s.pruned_through_cursor,
            deleted_summary.max_cursor
        ),
        retained_progress_events =
            s.retained_progress_events - deleted_summary.event_count + 1,
        retained_progress_bytes =
            s.retained_progress_bytes - deleted_summary.byte_count
            + octet_length($6::bytea),
        updated_at = clock_timestamp()
    FROM inserted, deleted_summary
    WHERE s.execution_id = $2
      AND s.generation = $3
    RETURNING inserted.cursor
)
SELECT COALESCE((SELECT cursor FROM updated_state LIMIT 1), 0),
       COALESCE((SELECT claim_id FROM authority LIMIT 1), ''),
       COALESCE((SELECT desired_state FROM authority LIMIT 1), ''),
       COALESCE((SELECT capability_id FROM authority LIMIT 1), ''),
       COALESCE((SELECT deadline_expired FROM authority LIMIT 1), FALSE),
       EXISTS (SELECT 1 FROM terminal_output)`,
			frame.EventID,
			frame.Fence.ExecutionID,
			int64(frame.Fence.Generation),
			projectionProjectID,
			replayEventNodeEvent,
			[]byte(frame.BrowserData),
			eventDigest[:],
			frame.TenantID,
			resourceProjectID,
			frame.Fence.CommandID,
			frame.Fence.WorkloadIdentity,
			frame.Fence.WorkloadSessionID,
			frame.Fence.ProducerID,
			int64(frame.Fence.ClaimAttempt),
			int64(frame.Fence.LeaseEpoch),
			frame.Fence.Token[:],
			int64(frame.Sequence),
			int64(frame.ClaimHandoffWatermark),
			r.retention.maxProgressAge.Milliseconds(),
			r.retention.maxProgressEvents,
			r.retention.maxProgressBytes,
		).Scan(&cursor, &authorityClaimID, &desiredState, &capabilityID, &deadlineExpired, &terminalPresent)
		if err != nil {
			return fmt.Errorf("append durable node event: %w", err)
		}
		if cursor > 0 {
			if capabilityID == executiondomain.IndexIngestCapability {
				if err := r.activity.projectNodeEvent(
					ctx,
					tx,
					projectionProjectID,
					frame,
				); err != nil {
					return err
				}
			}
			if err := r.agentTrace.projectAgentTraceDelta(
				ctx,
				tx,
				projectionProjectID,
				frame,
			); err != nil {
				return err
			}
			if capabilityID == executiondomain.AgentApplicationCapability ||
				capabilityID == executiondomain.AgentAdhocCapability {
				if err := r.agentContext.projectAgentContext(ctx, tx, projectionProjectID, frame); err != nil {
					return err
				}
				if err := r.agentText.projectAgentTextDelta(
					ctx,
					tx,
					projectionProjectID,
					frame,
				); err != nil {
					return err
				}
			}
			if capabilityID == executiondomain.IndexIngestCapability && persistTaskRestamp {
				if err := persistCurrentIndexMetaTaskRestampIntent(
					ctx,
					tx,
					frame,
					restampCreatedOn,
					resourceProjectID,
					projectionProjectID,
				); err != nil {
					return err
				}
			}
			outcome = outputapp.ProjectionOutcome{Inserted: true, Cursor: uint64(cursor), CommittedSequence: frame.Sequence}
			return nil
		}

		// ON CONFLICT may have waited for a concurrent identical append. Reload
		// after the statement snapshot before classifying the rejection.
		existing, loadErr = loadDurableNodeEvent(ctx, tx, frame.EventID)
		if loadErr == nil {
			if !sameDurableNodeEvent(existing, frame, projectionProjectID, eventDigest) {
				return outputapp.ErrNodeEventOutputConflict
			}
			outcome = outputapp.ProjectionOutcome{Inserted: false, Cursor: uint64(existing.Cursor), CommittedSequence: frame.Sequence}
			return nil
		}
		if !errors.Is(loadErr, pgx.ErrNoRows) {
			return fmt.Errorf("reload durable node event: %w", loadErr)
		}
		if terminalPresent {
			return outputapp.ErrNodeEventOutputConflict
		}
		if authorityClaimID == "" {
			return runtimedomain.ErrStaleFence
		}
		if desiredState == string(runtimedomain.DesiredCancelled) {
			return outputapp.ErrOutputCancelled
		}
		if deadlineExpired {
			return outputapp.ErrOutputDeadlineExceeded
		}
		return runtimedomain.ErrStaleFence
	})
	if err != nil {
		return outputapp.ProjectionOutcome{}, err
	}
	return outcome, nil
}

func persistCurrentIndexMetaTaskRestampIntent(
	ctx context.Context,
	tx sqlExecutor,
	frame outputapp.NodeEventFrame,
	createdOn float64,
	resourceProjectID int64,
	projectionProjectID int64,
) error {
	var targetExists, updated bool
	err := tx.QueryRow(ctx, `
WITH target AS MATERIALIZED (
    SELECT i.execution_id, i.generation
    FROM elitea_runtime.index_ingest_jobs AS i
    JOIN elitea_runtime.execution_jobs AS j
      ON j.execution_id = i.execution_id
     AND j.generation = i.generation
     AND j.capability_id = i.capability_id
    WHERE i.execution_id = $1
      AND i.generation = $2
      AND i.capability_id = 'index.ingest.v1'
      AND i.index_meta_initialized_at IS NOT NULL
      AND j.tenant_id = $3
      AND j.resource_project_id = $4
      AND j.projection_project_id = $5
),
persisted AS (
    UPDATE elitea_runtime.index_ingest_jobs AS i
    SET index_meta_task_restamp_source_event_id = $6,
        index_meta_task_restamp_occurred_at = $7,
        index_meta_task_restamp_created_on = $8,
        index_meta_task_restamp_status = 'PENDING',
        index_meta_task_restamp_attempt_count = 0,
        index_meta_task_restamp_next_attempt_at = clock_timestamp()
    FROM target
    WHERE i.execution_id = target.execution_id
      AND i.generation = target.generation
      AND i.index_meta_task_restamp_status IS NULL
    RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM target),
       EXISTS (SELECT 1 FROM persisted)`,
		frame.Fence.ExecutionID,
		int64(frame.Fence.Generation),
		frame.TenantID,
		resourceProjectID,
		projectionProjectID,
		frame.EventID,
		frame.OccurredAt.UTC(),
		createdOn,
	).Scan(&targetExists, &updated)
	if err != nil {
		return fmt.Errorf("persist current index metadata task restamp intent: %w", err)
	}
	if !targetExists {
		return runtimedomain.ErrStaleFence
	}
	// A prior authenticated in-progress event owns the immutable intent.
	// Later progress events in the same generation are intentional no-ops.
	_ = updated
	return nil
}

func initializeReplayExecutionState(ctx context.Context, tx sqlExecutor, executionID string, generation, projectionProjectID int64) error {
	tag, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.execution_replay_state (
    execution_id, generation, projection_project_id
)
SELECT execution_id, generation, projection_project_id
FROM elitea_runtime.execution_jobs
WHERE execution_id = $1
  AND generation = $2
  AND projection_project_id = $3
ON CONFLICT (execution_id, generation) DO NOTHING`,
		executionID,
		generation,
		projectionProjectID,
	)
	if err != nil {
		return fmt.Errorf("initialize execution replay state: %w", err)
	}
	if tag.RowsAffected() > 1 {
		return errors.New("execution replay state initialization affected multiple rows")
	}
	return nil
}

func lockReplayExecutionState(ctx context.Context, tx sqlExecutor, executionID string, generation, projectionProjectID int64) (replayExecutionState, error) {
	var state replayExecutionState
	var digest []byte
	err := tx.QueryRow(ctx, `
SELECT last_node_sequence,
       COALESCE(last_node_event_id, ''),
       COALESCE(last_node_event_bytes, ''::bytea),
       COALESCE(last_node_event_digest, ''::bytea),
       COALESCE(last_node_cursor, 0),
       pruned_through_cursor,
       retained_progress_events,
       retained_progress_bytes
FROM elitea_runtime.execution_replay_state
WHERE execution_id = $1
  AND generation = $2
  AND projection_project_id = $3
FOR UPDATE`,
		executionID,
		generation,
		projectionProjectID,
	).Scan(
		&state.LastNodeSequence,
		&state.LastNodeEventID,
		&state.LastNodeEventBytes,
		&digest,
		&state.LastNodeCursor,
		&state.PrunedThroughCursor,
		&state.RetainedProgressEvents,
		&state.RetainedProgressBytes,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return replayExecutionState{}, runtimedomain.ErrStaleFence
	}
	if err != nil {
		return replayExecutionState{}, fmt.Errorf("lock execution replay state: %w", err)
	}
	if state.LastNodeSequence == 0 {
		if state.LastNodeEventID != "" || len(state.LastNodeEventBytes) != 0 || len(digest) != 0 || state.LastNodeCursor != 0 {
			return replayExecutionState{}, errors.New("empty execution replay state contains a node receipt")
		}
	} else {
		if state.LastNodeEventID == "" || state.LastNodeCursor <= 0 {
			return replayExecutionState{}, errors.New("execution replay state is missing its latest node receipt")
		}
		stored, digestErr := storedDigest(digest)
		if digestErr != nil || runtimedomain.SHA256(state.LastNodeEventBytes) != stored {
			return replayExecutionState{}, errors.New("execution replay state contains an invalid latest node receipt")
		}
		state.LastNodeEventDigest = stored
	}
	if state.LastNodeSequence < 0 || state.PrunedThroughCursor < 0 ||
		state.RetainedProgressEvents < 0 || state.RetainedProgressBytes < 0 ||
		(state.LastNodeCursor > 0 && state.PrunedThroughCursor > state.LastNodeCursor) {
		return replayExecutionState{}, errors.New("execution replay state contains invalid retention counters")
	}
	return state, nil
}

func lockNodeEventAuthority(ctx context.Context, tx sqlExecutor, frame outputapp.NodeEventFrame, resourceProjectID, projectionProjectID int64) (bool, error) {
	var claimID string
	err := tx.QueryRow(ctx, `
SELECT c.claim_id
FROM elitea_runtime.execution_claims AS c
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = c.execution_id AND j.generation = c.generation
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE c.execution_id = $1
  AND c.generation = $2
	  AND j.capability_id IN (
	      'index.ingest.v1',
	      'agent.execute.application.v1',
	      'agent.execute.adhoc.v1'
	  )
  AND j.tenant_id = $3
  AND j.resource_project_id = $4
  AND j.projection_project_id = $5
  AND j.command_id = $6
  AND c.workload_identity = $7
  AND c.workload_session_id = $8
  AND c.producer_id = $9
  AND c.claim_attempt = $10
  AND c.lease_epoch = $11
  AND c.fence_token = $12
  AND c.initial_output_watermark = $13
  AND c.released_at IS NULL
  AND c.lease_expires_at > clock_timestamp()
  AND o.retired_at IS NULL
  AND o.authority_granted_at IS NOT NULL
FOR UPDATE OF j, o, c`,
		frame.Fence.ExecutionID,
		int64(frame.Fence.Generation),
		frame.TenantID,
		resourceProjectID,
		projectionProjectID,
		frame.Fence.CommandID,
		frame.Fence.WorkloadIdentity,
		frame.Fence.WorkloadSessionID,
		frame.Fence.ProducerID,
		int64(frame.Fence.ClaimAttempt),
		int64(frame.Fence.LeaseEpoch),
		frame.Fence.Token[:],
		int64(frame.ClaimHandoffWatermark),
	).Scan(&claimID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock node event authority: %w", err)
	}
	if claimID == "" {
		return false, errors.New("lock node event authority returned an empty claim")
	}
	return true, nil
}

func loadDurableNodeEvent(ctx context.Context, tx sqlExecutor, eventID string) (durableNodeEvent, error) {
	var event durableNodeEvent
	var digest []byte
	err := tx.QueryRow(ctx, `
SELECT cursor, execution_id, generation, projection_project_id,
       event_type, event_bytes, event_digest
FROM elitea_runtime.execution_replay_events
WHERE event_id = $1
UNION ALL
SELECT last_node_cursor, execution_id, generation, projection_project_id,
       $2, last_node_event_bytes, last_node_event_digest
FROM elitea_runtime.execution_replay_state
WHERE last_node_event_id = $1
  AND NOT EXISTS (
      SELECT 1
      FROM elitea_runtime.execution_replay_events
      WHERE event_id = $1
  )
LIMIT 1`, eventID, replayEventNodeEvent).Scan(
		&event.Cursor,
		&event.ExecutionID,
		&event.Generation,
		&event.ProjectionProjectID,
		&event.EventType,
		&event.EventBytes,
		&digest,
	)
	if err != nil {
		return durableNodeEvent{}, err
	}
	if event.Cursor <= 0 || event.Generation <= 0 {
		return durableNodeEvent{}, outputapp.ErrNodeEventOutputConflict
	}
	event.EventDigest, err = storedDigest(digest)
	if err != nil || runtimedomain.SHA256(event.EventBytes) != event.EventDigest {
		return durableNodeEvent{}, outputapp.ErrNodeEventOutputConflict
	}
	return event, nil
}

func sameDurableNodeEvent(existing durableNodeEvent, frame outputapp.NodeEventFrame, projectionProjectID int64, digest runtimedomain.Digest) bool {
	return existing.ExecutionID == frame.Fence.ExecutionID &&
		existing.Generation == int64(frame.Fence.Generation) &&
		existing.ProjectionProjectID == projectionProjectID &&
		existing.EventType == replayEventNodeEvent &&
		existing.EventDigest == digest &&
		bytes.Equal(existing.EventBytes, frame.BrowserData)
}

var _ outputapp.NodeEventProjector = (*NodeEventsRepository)(nil)
