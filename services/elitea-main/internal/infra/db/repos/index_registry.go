package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexregistryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexregistry"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IndexRegistryRepository is the Postgres implementation of the index registry
// (migration shared/0162). elitea-main is its only writer (ADR-0030 decision
// 2): every write here is a transition computed by the application layer
// (indexregistry.StartRun, ApplyTerminal, ApplyResult, ...) from a row this
// repository has locked, so the rules live in one place and the SQL only
// persists their outcome.
type IndexRegistryRepository struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// WithLogger sets the logger the repository reports no-ops with. The default is
// slog.Default().
func (r *IndexRegistryRepository) WithLogger(logger *slog.Logger) *IndexRegistryRepository {
	if r != nil && logger != nil {
		r.log = logger
	}
	return r
}

func (r *IndexRegistryRepository) logger() *slog.Logger {
	if r == nil || r.log == nil {
		return slog.Default()
	}
	return r.log
}

func NewIndexRegistryRepository(pool *pgxpool.Pool) (*IndexRegistryRepository, error) {
	if pool == nil {
		return nil, errors.New("index registry database pool is required")
	}
	return &IndexRegistryRepository{pool: pool}, nil
}

// indexRegistryRunJob reads the execution job the row names (execution_id at
// execution_generation): its state, desired state and terminal time, plus the
// database clock. They are the only liveness inputs (indexregistry.RunStatus);
// nothing compares updated_at.
const indexRegistryRunJob = `
    run_job.state, run_job.desired_state, run_job.settled_at, clock_timestamp()`

const indexRegistryRunJobJoin = `
LEFT JOIN LATERAL (
    SELECT job.state, job.desired_state, job.settled_at
    FROM elitea_runtime.execution_jobs AS job
    WHERE job.execution_id = index_registry.execution_id
      AND job.generation = index_registry.execution_generation
) AS run_job ON true`

const indexRegistryBaseColumns = `
    index_registry.index_id::text, index_registry.project_id, index_registry.toolkit_id,
    index_registry.name, index_registry.state, index_registry.task_id, index_registry.error,
    index_registry.conversation_id, index_registry.history, index_registry.index_configuration,
    index_registry.indexed_documents, index_registry.updated_documents,
    index_registry.indexed_chunks, index_registry.failed_chunks, index_registry.skipped,
    index_registry.embedding_model, index_registry.embedding_dimension, index_registry.collection,
    index_registry.execution_id, index_registry.execution_generation, index_registry.index_generation,
    index_registry.meta_id, index_registry.correlation_id,
    index_registry.created_at, index_registry.updated_at,`

// indexRegistrySelect selects one row with its run job for an update.
// FOR UPDATE OF locks the registry row only, never the job.
const indexRegistrySelect = `
SELECT ` + indexRegistryBaseColumns + indexRegistryRunJob + `,
    0::bigint, 0::bigint
FROM elitea_runtime.index_registry` + indexRegistryRunJobJoin

// indexRegistryReadSelect is the list and exact read: it adds the index's
// totals from index_registry_documents (indexregistry.Metadata).
const indexRegistryReadSelect = `
SELECT ` + indexRegistryBaseColumns + indexRegistryRunJob + `,
    documents.total_documents, documents.total_chunks
FROM elitea_runtime.index_registry` + indexRegistryRunJobJoin + `
CROSS JOIN LATERAL (
    SELECT count(*)::bigint AS total_documents,
           COALESCE(sum(document.chunk_count), 0)::bigint AS total_chunks
    FROM elitea_runtime.index_registry_documents AS document
    WHERE document.index_id = index_registry.index_id
) AS documents`

func scanIndexRegistryRow(row sqlRow) (indexregistryapp.Row, error) {
	var (
		out                                              indexregistryapp.Row
		state                                            string
		history, configuration, skipped                  []byte
		model, collection, executionID, metaID, correlID *string
		dimension                                        *int32
		executionGeneration                              *int64
		jobState, jobDesired                             *string
		jobSettledAt                                     *time.Time
	)
	if err := row.Scan(
		&out.IndexID, &out.ProjectID, &out.ToolkitID, &out.Name, &state, &out.TaskID, &out.Error, &out.ConvID,
		&history, &configuration,
		&out.Indexed, &out.Updated, &out.Chunks, &out.Failed, &skipped,
		&model, &dimension, &collection,
		&executionID, &executionGeneration, &out.IndexGeneration, &metaID, &correlID,
		&out.CreatedAt, &out.UpdatedAt,
		&jobState, &jobDesired, &jobSettledAt, &out.ObservedAt,
		&out.TotalDocuments, &out.TotalChunks,
	); err != nil {
		return indexregistryapp.Row{}, err
	}
	if jobState != nil {
		out.Job = indexregistryapp.RunJob{Found: true, State: executiondomain.JobState(*jobState)}
		if jobDesired != nil {
			out.Job.DesiredState = *jobDesired
		}
		if jobSettledAt != nil {
			out.Job.SettledAt = jobSettledAt.UTC()
		}
	}
	out.State = indexregistryapp.State(state)
	if !out.State.Valid() {
		return indexregistryapp.Row{}, fmt.Errorf("index registry row %s has state %q", out.IndexID, state)
	}
	if err := decodeRegistryJSON(history, &out.History); err != nil {
		return indexregistryapp.Row{}, fmt.Errorf("index registry row %s history: %w", out.IndexID, err)
	}
	if err := decodeRegistryJSON(configuration, &out.IndexConf); err != nil {
		return indexregistryapp.Row{}, fmt.Errorf("index registry row %s configuration: %w", out.IndexID, err)
	}
	if err := decodeRegistryJSON(skipped, &out.Skipped); err != nil {
		return indexregistryapp.Row{}, fmt.Errorf("index registry row %s skipped: %w", out.IndexID, err)
	}
	if out.History == nil {
		out.History = []map[string]any{}
	}
	if out.IndexConf == nil {
		out.IndexConf = map[string]any{}
	}
	if out.Skipped == nil {
		out.Skipped = map[string]uint64{}
	}
	if model != nil {
		out.Model = *model
	}
	if dimension != nil {
		out.Dimension = *dimension
	}
	if collection != nil {
		out.Collection = *collection
	}
	if executionID != nil {
		out.ExecutionID = *executionID
	}
	if executionGeneration != nil {
		out.ExecutionGeneration = *executionGeneration
	}
	if metaID != nil {
		out.MetaID = *metaID
	}
	if correlID != nil {
		out.CorrelationID = *correlID
	}
	return out, nil
}

// decodeRegistryJSON decodes jsonb keeping numbers exact: a history entry
// carries unix-second floats and generations that must round-trip unchanged.
func decodeRegistryJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(destination)
}

func nullIfEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// encodedIndexRegistryRow is a row as the SQL parameters both the insert and
// the update bind.
type encodedIndexRegistryRow struct {
	history, configuration, skipped []byte
	model, collection               *string
	dimension                       *int32
	executionID                     *string
	executionGeneration             *int64
	metaID, correlationID           *string
}

// encodeIndexRegistryRow is the one encoding of a row's stored columns.
func encodeIndexRegistryRow(row indexregistryapp.Row) (encodedIndexRegistryRow, error) {
	var (
		encoded encodedIndexRegistryRow
		err     error
	)
	if encoded.history, err = json.Marshal(row.History); err != nil {
		return encodedIndexRegistryRow{}, fmt.Errorf("encode index registry history: %w", err)
	}
	if encoded.configuration, err = json.Marshal(row.IndexConf); err != nil {
		return encodedIndexRegistryRow{}, fmt.Errorf("encode index registry configuration: %w", err)
	}
	if encoded.skipped, err = json.Marshal(row.Skipped); err != nil {
		return encodedIndexRegistryRow{}, fmt.Errorf("encode index registry skips: %w", err)
	}
	if row.Dimension != 0 {
		dimension := row.Dimension
		encoded.dimension = &dimension
	}
	if row.ExecutionID != "" {
		generation := row.ExecutionGeneration
		encoded.executionGeneration = &generation
	}
	encoded.model = nullIfEmpty(row.Model)
	encoded.collection = nullIfEmpty(row.Collection)
	encoded.executionID = nullIfEmpty(row.ExecutionID)
	encoded.metaID = nullIfEmpty(row.MetaID)
	encoded.correlationID = nullIfEmpty(row.CorrelationID)
	return encoded, nil
}

// updateIndexRegistryRow stores every mutable column of a row the caller holds
// locked. The identity (index id, project, toolkit, name) and created_at never
// change.
func updateIndexRegistryRow(ctx context.Context, q sqlExecutor, row indexregistryapp.Row) error {
	encoded, err := encodeIndexRegistryRow(row)
	if err != nil {
		return err
	}
	tag, err := q.Exec(ctx, `
UPDATE elitea_runtime.index_registry SET
    state = $2, task_id = $3, error = $4, conversation_id = $5,
    history = $6::jsonb, index_configuration = $7::jsonb,
    indexed_documents = $8, updated_documents = $9, indexed_chunks = $10, failed_chunks = $11,
    skipped = $12::jsonb,
    embedding_model = $13, embedding_dimension = $14, collection = $15,
    execution_id = $16, execution_generation = $17, index_generation = $18,
    meta_id = $19, correlation_id = $20, updated_at = $21
WHERE index_id = $1::uuid`,
		row.IndexID,
		string(row.State), row.TaskID, row.Error, row.ConvID,
		encoded.history, encoded.configuration,
		row.Indexed, row.Updated, row.Chunks, row.Failed,
		encoded.skipped,
		encoded.model, encoded.dimension, encoded.collection,
		encoded.executionID, encoded.executionGeneration, row.IndexGeneration,
		encoded.metaID, encoded.correlationID, row.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("update index registry row: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return indexingapp.ErrCurrentIndexMetaConflict
	}
	return nil
}

func insertIndexRegistryRow(ctx context.Context, q sqlExecutor, row indexregistryapp.Row) (string, error) {
	encoded, err := encodeIndexRegistryRow(row)
	if err != nil {
		return "", err
	}
	var indexID string
	err = q.QueryRow(ctx, `
INSERT INTO elitea_runtime.index_registry (
    project_id, toolkit_id, name, state, task_id, error, conversation_id,
    history, index_configuration,
    indexed_documents, updated_documents, indexed_chunks, failed_chunks, skipped,
    embedding_model, embedding_dimension, collection,
    execution_id, execution_generation, index_generation, meta_id, correlation_id,
    created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8::jsonb, $9::jsonb,
    $10, $11, $12, $13, $14::jsonb,
    $15, $16, $17,
    $18, $19, $20, $21, $22,
    $23, $23
) RETURNING index_id::text`,
		row.ProjectID, row.ToolkitID, row.Name, string(row.State), row.TaskID, row.Error, row.ConvID,
		encoded.history, encoded.configuration,
		row.Indexed, row.Updated, row.Chunks, row.Failed, encoded.skipped,
		encoded.model, encoded.dimension, encoded.collection,
		encoded.executionID, encoded.executionGeneration, row.IndexGeneration,
		encoded.metaID, encoded.correlationID,
		row.CreatedAt,
	).Scan(&indexID)
	if err != nil {
		return "", fmt.Errorf("insert index registry row: %w", err)
	}
	return indexID, nil
}

// lockLiveRegistryRowByName takes the row lock of a live (not deleted) index.
func lockLiveRegistryRowByName(
	ctx context.Context,
	q sqlExecutor,
	projectID, toolkitID int32,
	name string,
) (indexregistryapp.Row, bool, error) {
	row, err := scanIndexRegistryRow(q.QueryRow(ctx, indexRegistrySelect+`
WHERE index_registry.project_id = $1 AND index_registry.toolkit_id = $2
  AND index_registry.name = $3 AND index_registry.deleted_at IS NULL
FOR UPDATE OF index_registry`, projectID, toolkitID, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return indexregistryapp.Row{}, false, nil
	}
	if err != nil {
		return indexregistryapp.Row{}, false, err
	}
	return row, true, nil
}

// registryRunTombstoned reports whether a deleted row once belonged to the run
// of this fence. A transition for such a run has nothing to write and nothing
// to wait for: the index it was running was deleted.
func registryRunTombstoned(
	ctx context.Context,
	q sqlExecutor,
	projectID int32,
	executionID string,
	generation int64,
) (bool, error) {
	var found bool
	err := q.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM elitea_runtime.index_registry
    WHERE project_id = $1 AND execution_id = $2 AND execution_generation = $3
      AND deleted_at IS NOT NULL
)`, projectID, executionID, generation).Scan(&found)
	return found, err
}

func (r *IndexRegistryRepository) withinTx(ctx context.Context, fn func(sqlExecutor) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return fmt.Errorf("begin index registry transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rollbackCtx)
		}
	}()
	if err := fn(pgxExecutor{queryer: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit index registry transaction: %w", err)
	}
	committed = true
	return nil
}

// lockIndexName serializes the first insert of an index name. The partial
// unique index would reject the second inserter anyway; the lock turns that
// race into an ordinary wait followed by the right transition.
func lockIndexName(ctx context.Context, q sqlExecutor, projectID, toolkitID int32, name string) error {
	_, err := q.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		fmt.Sprintf("index_registry:%d:%d:%s", projectID, toolkitID, name),
	)
	return err
}

// InitializeRegistryRun implements indexing.RegistryRunWriter.
func (r *IndexRegistryRepository) InitializeRegistryRun(ctx context.Context, run indexingapp.RegistryInitialRun) error {
	if r == nil || r.pool == nil || ctx == nil {
		return indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	if err := run.Validate(); err != nil {
		return err
	}
	return r.withinTx(ctx, func(q sqlExecutor) error {
		if err := lockIndexName(ctx, q, run.ProjectID, run.ToolkitID, run.IndexName); err != nil {
			return fmt.Errorf("lock index registry name: %w", err)
		}
		existing, found, err := lockLiveRegistryRowByName(ctx, q, run.ProjectID, run.ToolkitID, run.IndexName)
		if err != nil {
			return fmt.Errorf("load index registry row: %w", err)
		}
		var current *indexregistryapp.Row
		if found {
			current = &existing
		}
		next, changed, err := indexregistryapp.StartRun(current, run)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		if !found {
			_, err = insertIndexRegistryRow(ctx, q, next)
			return err
		}
		next.IndexID = existing.IndexID
		return updateIndexRegistryRow(ctx, q, next)
	})
}

// ApplyRegistryTerminal implements indexing.RegistryTerminalWriter.
func (r *IndexRegistryRepository) ApplyRegistryTerminal(ctx context.Context, terminal indexingapp.RegistryTerminal) error {
	if r == nil || r.pool == nil || ctx == nil {
		return indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	if err := terminal.Validate(); err != nil {
		return err
	}
	return r.withinTx(ctx, func(q sqlExecutor) error {
		row, found, err := lockLiveRegistryRowByName(ctx, q, terminal.ProjectID, terminal.ToolkitID, terminal.IndexName)
		if err != nil {
			return fmt.Errorf("load index registry row: %w", err)
		}
		if !found {
			tombstoned, err := registryRunTombstoned(
				ctx, q, terminal.ProjectID, terminal.ExecutionID, int64(terminal.Generation),
			)
			if err != nil {
				return fmt.Errorf("look up deleted index registry row: %w", err)
			}
			if tombstoned {
				return indexingapp.ErrCurrentIndexMetaSuperseded
			}
			// No live and no deleted row names this run: it was admitted
			// under the python runtime, before the mode switched (or its row
			// never existed). There is nothing to write; acknowledge it, as
			// ApplyIndexResult does.
			r.logger().Info("index registry terminal effect has no registry row; acknowledged as a no-op",
				"project_id", terminal.ProjectID, "toolkit_id", terminal.ToolkitID,
				"index", terminal.IndexName, "execution_id", terminal.ExecutionID)
			return nil
		}
		next, changed, err := indexregistryapp.ApplyTerminal(row, terminal)
		if err != nil || !changed {
			return err
		}
		return updateIndexRegistryRow(ctx, q, next)
	})
}

// VerifyRegistryManualStop implements indexing.RegistryManualStopWriter. It
// returns an empty index id when no row names the run.
func (r *IndexRegistryRepository) VerifyRegistryManualStop(
	ctx context.Context,
	stop indexingapp.RegistryManualStop,
) (string, error) {
	if r == nil || r.pool == nil || ctx == nil {
		return "", indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	if err := stop.Validate(); err != nil {
		return "", err
	}
	var indexID string
	err := r.withinTx(ctx, func(q sqlExecutor) error {
		row, found, err := lockLiveRegistryRowByName(ctx, q, stop.ProjectID, stop.ToolkitID, stop.IndexName)
		if err != nil {
			return fmt.Errorf("load index registry row: %w", err)
		}
		if !found {
			tombstoned, err := registryRunTombstoned(ctx, q, stop.ProjectID, stop.ExecutionID, int64(stop.Generation))
			if err != nil {
				return fmt.Errorf("look up deleted index registry row: %w", err)
			}
			if tombstoned {
				return indexingapp.ErrCurrentIndexMetaSuperseded
			}
			// See ApplyRegistryTerminal: no row means nothing to verify.
			r.logger().Info("index registry manual Stop has no registry row; acknowledged as a no-op",
				"project_id", stop.ProjectID, "toolkit_id", stop.ToolkitID,
				"index", stop.IndexName, "execution_id", stop.ExecutionID)
			return nil
		}
		if err := indexregistryapp.VerifyManualStop(row, stop); err != nil {
			return err
		}
		indexID = row.IndexID
		return nil
	})
	return indexID, err
}

// RegistryResultOutcome reports what ApplyResult did.
type RegistryResultOutcome struct {
	// Applied is false when there was nothing to apply: no live row for the
	// run (a deleted index, or a run admitted before the deployment switched
	// runtimes), or a row already at rest.
	Applied bool
	// Recorded is the summary the registry recorded (indexregistry.
	// RecordedSummary), and Overridden reports that it is a failure the worker
	// did not report: no typed summary, or another embedding space.
	Recorded   outputapp.IndexIngestSummary
	Overridden bool
}

// LockIndexResultRow takes the row lock of the live registry row of one run,
// the ONE locked read of a result's transaction: the caller decides what to
// record from the returned row (indexregistry.RecordedSummary) and hands the
// same row to ApplyIndexResult. found is false when no live row names the run.
func LockIndexResultRow(
	ctx context.Context,
	q sqlExecutor,
	projectID int32,
	executionID string,
	generation uint64,
) (row indexregistryapp.Row, found bool, err error) {
	row, err = scanIndexRegistryRow(q.QueryRow(ctx, indexRegistrySelect+`
WHERE index_registry.project_id = $1 AND index_registry.execution_id = $2
  AND index_registry.execution_generation = $3 AND index_registry.deleted_at IS NULL
FOR UPDATE OF index_registry`, projectID, executionID, int64(generation)))
	if errors.Is(err, pgx.ErrNoRows) {
		return indexregistryapp.Row{}, false, nil
	}
	if err != nil {
		return indexregistryapp.Row{}, false, fmt.Errorf("load index registry row for result: %w", err)
	}
	return row, true, nil
}

// ApplyIndexResult applies a run's typed terminal result to the row the caller
// locked with LockIndexResultRow in the same transaction. The output
// projection calls it in the transaction that projects the run's terminal
// output, so the registry and the projection cannot disagree after a crash.
func ApplyIndexResult(
	ctx context.Context,
	q sqlExecutor,
	row indexregistryapp.Row,
	result indexregistryapp.Result,
) (applied bool, err error) {
	next, changed, err := indexregistryapp.ApplyResult(row, result)
	if err != nil || !changed {
		return false, err
	}
	if err := updateIndexRegistryRow(ctx, q, next); err != nil {
		return false, err
	}
	return true, nil
}

// ApplyResult is LockIndexResultRow and ApplyIndexResult in their own
// transaction. The output projection does not use it; tests and a future
// result replay do.
func (r *IndexRegistryRepository) ApplyResult(
	ctx context.Context,
	projectID int32,
	result indexregistryapp.Result,
) (RegistryResultOutcome, error) {
	var outcome RegistryResultOutcome
	err := r.withinTx(ctx, func(q sqlExecutor) error {
		row, found, err := LockIndexResultRow(ctx, q, projectID, result.ExecutionID, result.Generation)
		if err != nil || !found || row.State != indexregistryapp.StateInProgress {
			return err
		}
		outcome.Recorded, outcome.Overridden = indexregistryapp.RecordedSummary(row, result.Summary)
		outcome.Applied, err = ApplyIndexResult(ctx, q, row, result)
		return err
	})
	return outcome, err
}

// RecordScheduledFailure appends a failed-schedule entry to the live index of
// that name. A missing index is not an error: the Python path only warns.
func (r *IndexRegistryRepository) RecordScheduledFailure(
	ctx context.Context,
	projectID, toolkitID int32,
	name, effectID, safeReason string,
	occurredAt time.Time,
) error {
	if r == nil || r.pool == nil || ctx == nil || name == "" || effectID == "" || occurredAt.IsZero() {
		return indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	return r.withinTx(ctx, func(q sqlExecutor) error {
		row, found, err := lockLiveRegistryRowByName(ctx, q, projectID, toolkitID, name)
		if err != nil {
			return fmt.Errorf("load index registry row: %w", err)
		}
		if !found {
			return nil
		}
		next, changed := indexregistryapp.RecordScheduledFailure(row, effectID, safeReason, occurredAt)
		if !changed {
			return nil
		}
		return updateIndexRegistryRow(ctx, q, next)
	})
}

// List implements indexregistry.Store.
func (r *IndexRegistryRepository) List(ctx context.Context, projectID, toolkitID int32) ([]indexregistryapp.Row, error) {
	rows, err := r.pool.Query(ctx, indexRegistryReadSelect+`
WHERE index_registry.project_id = $1 AND index_registry.toolkit_id = $2
  AND index_registry.deleted_at IS NULL
ORDER BY index_registry.created_at, index_registry.index_id
LIMIT 10001`, projectID, toolkitID)
	if err != nil {
		return nil, fmt.Errorf("list index registry: %w", err)
	}
	defer rows.Close()
	result := make([]indexregistryapp.Row, 0)
	for rows.Next() {
		row, err := scanIndexRegistryRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan index registry row: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list index registry: %w", err)
	}
	return result, nil
}

// FindByName implements indexregistry.Store.
func (r *IndexRegistryRepository) FindByName(
	ctx context.Context,
	projectID, toolkitID int32,
	name string,
) (indexregistryapp.Row, bool, error) {
	row, err := scanIndexRegistryRow(r.pool.QueryRow(ctx, indexRegistryReadSelect+`
WHERE index_registry.project_id = $1 AND index_registry.toolkit_id = $2
  AND index_registry.name = $3 AND index_registry.deleted_at IS NULL`,
		projectID, toolkitID, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return indexregistryapp.Row{}, false, nil
	}
	if err != nil {
		return indexregistryapp.Row{}, false, fmt.Errorf("find index registry row: %w", err)
	}
	return row, true, nil
}

// SaveConfiguration implements indexregistry.Store. It replaces
// index_configuration and nothing else: the state, run fence and updated_at
// belong to the run path.
func (r *IndexRegistryRepository) SaveConfiguration(
	ctx context.Context,
	projectID, toolkitID int32,
	name string,
	configuration []byte,
) error {
	tag, err := r.pool.Exec(ctx, `
UPDATE elitea_runtime.index_registry
SET index_configuration = $4::jsonb
WHERE project_id = $1 AND toolkit_id = $2 AND name = $3 AND deleted_at IS NULL`,
		projectID, toolkitID, name, configuration)
	if err != nil {
		return fmt.Errorf("save index registry configuration: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return indexregistryapp.ErrNotFound
	}
	return nil
}

// MarkDeleted implements indexregistry.Store.
func (r *IndexRegistryRepository) MarkDeleted(
	ctx context.Context,
	projectID, toolkitID int32,
	indexID string,
) (indexregistryapp.Row, error) {
	var deleted indexregistryapp.Row
	err := r.withinTx(ctx, func(q sqlExecutor) error {
		row, err := scanIndexRegistryRow(q.QueryRow(ctx, indexRegistrySelect+`
WHERE index_registry.index_id = $1::uuid AND index_registry.project_id = $2
  AND index_registry.toolkit_id = $3 AND index_registry.deleted_at IS NULL
FOR UPDATE OF index_registry`, indexID, projectID, toolkitID))
		if errors.Is(err, pgx.ErrNoRows) {
			return indexregistryapp.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load index registry row: %w", err)
		}
		if row.Active() {
			return indexregistryapp.ErrActiveRun
		}
		if _, err := q.Exec(ctx, `
UPDATE elitea_runtime.index_registry
SET deleted_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE index_id = $1::uuid`, indexID); err != nil {
			return fmt.Errorf("tombstone index registry row: %w", err)
		}
		deleted = row
		return nil
	})
	return deleted, err
}

// PurgeDeleted implements indexregistry.Store. It removes only a tombstone, so
// a live index can never be purged by a stale id.
func (r *IndexRegistryRepository) PurgeDeleted(ctx context.Context, indexID string) error {
	if _, err := r.pool.Exec(ctx, `
DELETE FROM elitea_runtime.index_registry
WHERE index_id = $1::uuid AND deleted_at IS NOT NULL`, indexID); err != nil {
		return fmt.Errorf("purge index registry tombstone: %w", err)
	}
	return nil
}

// ListTombstones returns tombstones waiting for their vectors to be deleted,
// oldest first, whether or not they are due. It is the inspection list; the
// sweeper takes its work with ClaimTombstones.
func (r *IndexRegistryRepository) ListTombstones(ctx context.Context, limit int) ([]indexregistryapp.Tombstone, error) {
	if limit <= 0 || limit > 1000 {
		return nil, errors.New("tombstone limit is invalid")
	}
	rows, err := r.pool.Query(ctx, `
SELECT index_id::text, project_id, toolkit_id, name, deleted_at, attempts
FROM elitea_runtime.index_registry
WHERE deleted_at IS NOT NULL
ORDER BY deleted_at, index_id
LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list index registry tombstones: %w", err)
	}
	defer rows.Close()
	return scanTombstones(rows)
}

type tombstoneRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func scanTombstones(rows tombstoneRows) ([]indexregistryapp.Tombstone, error) {
	var out []indexregistryapp.Tombstone
	for rows.Next() {
		var tombstone indexregistryapp.Tombstone
		if err := rows.Scan(
			&tombstone.IndexID, &tombstone.ProjectID, &tombstone.ToolkitID, &tombstone.Name,
			&tombstone.DeletedAt, &tombstone.Attempts,
		); err != nil {
			return nil, err
		}
		out = append(out, tombstone)
	}
	return out, rows.Err()
}

// ClaimTombstones implements indexregistry.TombstoneSweeperStore. The claim is
// one short statement: FOR UPDATE SKIP LOCKED picks due rows no other replica
// is claiming, and the same statement pushes their next_attempt_at out by the
// lease, so nothing is locked while the deletions run.
func (r *IndexRegistryRepository) ClaimTombstones(
	ctx context.Context,
	limit int,
	lease time.Duration,
) ([]indexregistryapp.Tombstone, error) {
	if limit <= 0 || limit > 1000 || lease <= 0 {
		return nil, errors.New("tombstone claim is invalid")
	}
	rows, err := r.pool.Query(ctx, `
WITH due AS (
    SELECT index_id
    FROM elitea_runtime.index_registry
    WHERE deleted_at IS NOT NULL
      AND (next_attempt_at IS NULL OR next_attempt_at <= clock_timestamp())
    ORDER BY next_attempt_at NULLS FIRST, deleted_at, index_id
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
UPDATE elitea_runtime.index_registry AS registry
SET next_attempt_at = clock_timestamp() + make_interval(secs => $2)
FROM due
WHERE registry.index_id = due.index_id
RETURNING registry.index_id::text, registry.project_id, registry.toolkit_id,
          registry.name, registry.deleted_at, registry.attempts`, limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim index registry tombstones: %w", err)
	}
	defer rows.Close()
	return scanTombstones(rows)
}

// RescheduleTombstone implements indexregistry.TombstoneSweeperStore. It only
// touches a tombstone, so a live index is never rescheduled by a stale id.
func (r *IndexRegistryRepository) RescheduleTombstone(
	ctx context.Context,
	indexID string,
	nextAttemptAt time.Time,
	failed bool,
) error {
	increment := 0
	if failed {
		increment = 1
	}
	if _, err := r.pool.Exec(ctx, `
UPDATE elitea_runtime.index_registry
SET attempts = attempts + $3, next_attempt_at = $2
WHERE index_id = $1::uuid AND deleted_at IS NOT NULL`, indexID, nextAttemptAt, increment); err != nil {
		return fmt.Errorf("reschedule index registry tombstone: %w", err)
	}
	return nil
}

// UpsertDocuments records the (document_key, version) of documents a run has
// indexed (ADR-0030 decision 3). The caller is the future result path of the
// Rust worker; nothing in elitea-main calls it yet.
func (r *IndexRegistryRepository) UpsertDocuments(
	ctx context.Context,
	indexID string,
	documents []indexregistryapp.DocumentVersion,
) error {
	if len(documents) == 0 {
		return nil
	}
	return r.withinTx(ctx, func(q sqlExecutor) error {
		for _, document := range documents {
			if err := document.Validate(); err != nil {
				return err
			}
			if _, err := q.Exec(ctx, `
INSERT INTO elitea_runtime.index_registry_documents (index_id, document_key, version, chunk_count)
VALUES ($1::uuid, $2, $3, $4)
ON CONFLICT (index_id, document_key) DO UPDATE
SET version = EXCLUDED.version, chunk_count = EXCLUDED.chunk_count, updated_at = clock_timestamp()`,
				indexID, document.Key, document.Version, document.ChunkCount); err != nil {
				return fmt.Errorf("upsert index registry document: %w", err)
			}
		}
		return nil
	})
}

// DeleteDocuments forgets documents that disappeared from the source.
func (r *IndexRegistryRepository) DeleteDocuments(ctx context.Context, indexID string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if _, err := r.pool.Exec(ctx, `
DELETE FROM elitea_runtime.index_registry_documents
WHERE index_id = $1::uuid AND document_key = ANY($2::text[])`, indexID, keys); err != nil {
		return fmt.Errorf("delete index registry documents: %w", err)
	}
	return nil
}

// ListDocuments returns every recorded document of an index, by key.
func (r *IndexRegistryRepository) ListDocuments(ctx context.Context, indexID string) ([]indexregistryapp.DocumentVersion, error) {
	rows, err := r.pool.Query(ctx, `
SELECT document_key, version, chunk_count
FROM elitea_runtime.index_registry_documents
WHERE index_id = $1::uuid
ORDER BY document_key`, indexID)
	if err != nil {
		return nil, fmt.Errorf("list index registry documents: %w", err)
	}
	defer rows.Close()
	var out []indexregistryapp.DocumentVersion
	for rows.Next() {
		var document indexregistryapp.DocumentVersion
		if err := rows.Scan(&document.Key, &document.Version, &document.ChunkCount); err != nil {
			return nil, err
		}
		out = append(out, document)
	}
	return out, rows.Err()
}

var (
	_ indexingapp.RegistryRunWriter          = (*IndexRegistryRepository)(nil)
	_ indexingapp.RegistryTerminalWriter     = (*IndexRegistryRepository)(nil)
	_ indexingapp.RegistryManualStopWriter   = (*IndexRegistryRepository)(nil)
	_ indexregistryapp.Store                 = (*IndexRegistryRepository)(nil)
	_ indexregistryapp.TombstoneSweeperStore = (*IndexRegistryRepository)(nil)
)
