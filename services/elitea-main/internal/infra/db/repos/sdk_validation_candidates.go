package repos

import (
	"context"
	"errors"
	"fmt"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type CurrentSDKValidationCandidatesRepository struct {
	projects projectStore
	shared   sqlExecutor
}

func NewCurrentSDKValidationCandidatesRepository(pool *pgxpool.Pool) (*CurrentSDKValidationCandidatesRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	shared, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentSDKValidationCandidatesRepository(projects, shared)
}

func newCurrentSDKValidationCandidatesRepository(
	projects projectStore,
	shared sqlExecutor,
) (*CurrentSDKValidationCandidatesRepository, error) {
	if projects == nil || shared == nil {
		return nil, errors.New("current SDK validation tenant and execution databases are required")
	}
	return &CurrentSDKValidationCandidatesRepository{projects: projects, shared: shared}, nil
}

func (r *CurrentSDKValidationCandidatesRepository) StageCurrentSDKValidationCandidate(
	ctx context.Context,
	candidate configurationapp.CurrentSDKValidationCandidate,
) error {
	if err := candidate.Validate(); err != nil {
		return err
	}
	return r.projects.WithinProjectTx(
		ctx,
		int64(candidate.ProjectID),
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			var revisionID, inputBundleID string
			err := tx.QueryRow(ctx, `
INSERT INTO configuration_revisions (
    revision_id, configuration_id, configuration_type, settings_entry_id,
    settings_entry_version, settings_content_digest, input_bundle_id,
    catalog_revision, catalog_digest, schema_id, schema_revision,
    schema_digest, created_by
) VALUES (
    $1, NULL, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
)
RETURNING revision_id, input_bundle_id`,
				candidate.RevisionID,
				candidate.ConfigurationType,
				candidate.SettingsEntryID,
				candidate.SettingsEntryVersion,
				candidate.SettingsContentDigest[:],
				candidate.InputBundleID,
				candidate.CatalogRevision,
				candidate.CatalogDigest[:],
				candidate.SchemaID,
				candidate.SchemaRevision,
				candidate.SchemaDigest[:],
				candidate.CreatedBy,
			).Scan(&revisionID, &inputBundleID)
			if err != nil {
				return fmt.Errorf("insert current SDK validation candidate: %w", err)
			}
			if revisionID != candidate.RevisionID || inputBundleID != candidate.InputBundleID {
				return errors.New("current SDK validation candidate identity changed")
			}
			return nil
		},
	)
}

func (r *CurrentSDKValidationCandidatesRepository) ObserveCurrentSDKValidationCandidate(
	ctx context.Context,
	execution configurationapp.CurrentSDKValidationCandidateExecution,
) (configurationapp.CurrentSDKValidationCandidateStatus, error) {
	if err := execution.Validate(); err != nil {
		return "", err
	}
	candidate := execution.Candidate
	var (
		jobState            string
		jobSettled          bool
		projected           bool
		valid               bool
		settlementOutcome   string
		settlementCommitted bool
		exactTerminalOutput bool
	)
	err := r.projects.WithinProjectTx(
		ctx,
		int64(candidate.ProjectID),
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
		func(tx sqlExecutor) error {
			err := tx.QueryRow(ctx, `
SELECT j.state,
       j.settled_at IS NOT NULL,
       p.revision_id IS NOT NULL,
       COALESCE(p.valid, FALSE),
       COALESCE(s.disposition, ''),
       s.committed_at IS NOT NULL,
       COALESCE(
           p.execution_id = j.execution_id
           AND p.execution_generation = j.generation
           AND p.logical_output_id = s.final_logical_output_id,
           FALSE
       )
FROM configuration_revisions AS r
JOIN elitea_runtime.execution_jobs AS j
  ON j.configuration_revision_id = r.revision_id
 AND j.execution_id = $13
 AND j.generation = $14
 AND j.command_id = $15
 AND j.tenant_id = $16
 AND j.resource_project_id = $17
 AND j.projection_project_id = $17
 AND j.actor_id = r.created_by
 AND j.capability_id = 'configuration.validate.v1'
 AND j.input_bundle_id = r.input_bundle_id
 AND j.configuration_type = r.configuration_type
 AND j.catalog_revision = r.catalog_revision
 AND j.catalog_digest = r.catalog_digest
 AND j.schema_id = r.schema_id
 AND j.schema_revision = r.schema_revision
 AND j.schema_digest = r.schema_digest
 AND j.settings_entry_id = r.settings_entry_id
JOIN elitea_runtime.input_bundles AS b
  ON b.input_bundle_id = r.input_bundle_id
 AND b.immutable_version = r.revision_id
 AND b.resource_project_id = $17
 AND b.manifest_digest = $12
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id = b.input_bundle_id
 AND e.entry_id = r.settings_entry_id
 AND e.entry_version = r.settings_entry_version
 AND e.semantic_role = 'configuration.settings'
 AND e.media_type = 'application/json'
 AND e.content_digest = r.settings_content_digest
LEFT JOIN configuration_validation_projection AS p
  ON p.revision_id = r.revision_id
LEFT JOIN elitea_runtime.execution_settlements AS s
  ON s.execution_id = j.execution_id
 AND s.generation = j.generation
WHERE r.revision_id = $1
  AND r.configuration_id IS NULL
  AND r.configuration_type = $2
  AND r.settings_entry_id = $3
  AND r.settings_entry_version = $4
  AND r.settings_content_digest = $5
  AND r.input_bundle_id = $6
  AND r.catalog_revision = $7
  AND r.catalog_digest = $8
  AND r.schema_id = $9
  AND r.schema_revision = $10
  AND r.schema_digest = $11
  AND r.created_by = $18`,
				candidate.RevisionID,
				candidate.ConfigurationType,
				candidate.SettingsEntryID,
				candidate.SettingsEntryVersion,
				candidate.SettingsContentDigest[:],
				candidate.InputBundleID,
				candidate.CatalogRevision,
				candidate.CatalogDigest[:],
				candidate.SchemaID,
				candidate.SchemaRevision,
				candidate.SchemaDigest[:],
				candidate.InputBundleDigest[:],
				execution.ExecutionID,
				int64(execution.Generation),
				execution.CommandID,
				fmt.Sprintf("%d", candidate.ProjectID),
				int64(candidate.ProjectID),
				candidate.CreatedBy,
			).Scan(
				&jobState,
				&jobSettled,
				&projected,
				&valid,
				&settlementOutcome,
				&settlementCommitted,
				&exactTerminalOutput,
			)
			if errors.Is(err, pgx.ErrNoRows) {
				return configurationapp.ErrCurrentSDKValidationCandidateNotFound
			}
			if err != nil {
				return fmt.Errorf("load current SDK validation candidate: %w", err)
			}
			return nil
		},
	)
	if err != nil {
		return "", err
	}

	if !jobSettled || !settlementCommitted {
		return configurationapp.CurrentSDKValidationCandidatePending, nil
	}
	switch {
	case jobState == "SUCCEEDED" && settlementOutcome == string(executionapp.SettlementSucceeded) && projected && exactTerminalOutput:
		if valid {
			return configurationapp.CurrentSDKValidationCandidateValid, nil
		}
		return configurationapp.CurrentSDKValidationCandidateInvalid, nil
	case jobState == "FAILED" && settlementOutcome == string(executionapp.SettlementFailed):
		return configurationapp.CurrentSDKValidationCandidateFailed, nil
	case jobState == "CANCELLED" && settlementOutcome == string(executionapp.SettlementCancelled):
		return configurationapp.CurrentSDKValidationCandidateCancelled, nil
	default:
		return "", configurationapp.ErrInvalidCurrentSDKValidationExecution
	}
}

func (r *CurrentSDKValidationCandidatesRepository) RequestCurrentSDKValidationCancellation(
	ctx context.Context,
	execution configurationapp.CurrentSDKValidationCandidateExecution,
) error {
	if err := execution.Validate(); err != nil {
		return err
	}
	candidate := execution.Candidate
	_, err := r.shared.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET desired_state = 'CANCELLED'
WHERE execution_id = $1
  AND generation = $2
  AND command_id = $3
  AND configuration_revision_id = $4
  AND input_bundle_id = $5
  AND resource_project_id = $6
  AND projection_project_id = $6
  AND capability_id = 'configuration.validate.v1'
  AND desired_state = 'RUNNING'
  AND state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')`,
		execution.ExecutionID,
		int64(execution.Generation),
		execution.CommandID,
		candidate.RevisionID,
		candidate.InputBundleID,
		int64(candidate.ProjectID),
	)
	if err != nil {
		return fmt.Errorf("request current SDK validation cancellation: %w", err)
	}
	return nil
}

func (r *CurrentSDKValidationCandidatesRepository) CleanupCurrentSDKValidationCandidate(
	ctx context.Context,
	candidate configurationapp.CurrentSDKValidationCandidate,
) error {
	if err := candidate.Validate(); err != nil {
		return err
	}
	return r.projects.WithinProjectTx(
		ctx,
		int64(candidate.ProjectID),
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			_, err := tx.Exec(ctx, `
WITH removable AS MATERIALIZED (
    SELECT r.revision_id
    FROM configuration_revisions AS r
    WHERE r.revision_id = $1
      AND r.configuration_id IS NULL
      AND r.input_bundle_id = $2
      AND r.settings_content_digest = $3
      AND EXISTS (
          SELECT 1
          FROM elitea_runtime.execution_jobs AS admitted
          JOIN elitea_runtime.execution_settlements AS settlement
            ON settlement.execution_id = admitted.execution_id
           AND settlement.generation = admitted.generation
           AND settlement.committed_at IS NOT NULL
          LEFT JOIN configuration_validation_projection AS projection
            ON projection.revision_id = r.revision_id
           AND projection.execution_id = admitted.execution_id
           AND projection.execution_generation = admitted.generation
           AND projection.logical_output_id = settlement.final_logical_output_id
          WHERE admitted.configuration_revision_id = r.revision_id
            AND admitted.input_bundle_id = r.input_bundle_id
            AND admitted.capability_id = 'configuration.validate.v1'
            AND admitted.state IN ('SUCCEEDED', 'FAILED', 'CANCELLED')
            AND admitted.settled_at IS NOT NULL
            AND (
                (admitted.state = 'SUCCEEDED'
                    AND settlement.disposition = 'SUCCEEDED'
                    AND projection.revision_id IS NOT NULL)
                OR (admitted.state = 'FAILED' AND settlement.disposition = 'FAILED')
                OR (admitted.state = 'CANCELLED' AND settlement.disposition = 'CANCELLED')
            )
      )
      AND NOT EXISTS (
          SELECT 1
          FROM elitea_runtime.execution_jobs AS unsafe
          LEFT JOIN elitea_runtime.execution_settlements AS settlement
            ON settlement.execution_id = unsafe.execution_id
           AND settlement.generation = unsafe.generation
          LEFT JOIN configuration_validation_projection AS projection
            ON projection.revision_id = r.revision_id
           AND projection.execution_id = unsafe.execution_id
           AND projection.execution_generation = unsafe.generation
           AND projection.logical_output_id = settlement.final_logical_output_id
          WHERE unsafe.configuration_revision_id = r.revision_id
            AND (
                unsafe.capability_id IS DISTINCT FROM 'configuration.validate.v1'
                OR unsafe.input_bundle_id IS DISTINCT FROM r.input_bundle_id
                OR unsafe.state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED')
                OR unsafe.settled_at IS NULL
                OR settlement.committed_at IS NULL
                OR NOT (
                    (unsafe.state = 'SUCCEEDED'
                        AND settlement.disposition = 'SUCCEEDED'
                        AND projection.revision_id IS NOT NULL)
                    OR (unsafe.state = 'FAILED' AND settlement.disposition = 'FAILED')
                    OR (unsafe.state = 'CANCELLED' AND settlement.disposition = 'CANCELLED')
                )
            )
      )
    FOR UPDATE
), removed_projection AS (
    DELETE FROM configuration_validation_projection AS p
    USING removable
    WHERE p.revision_id = removable.revision_id
    RETURNING p.revision_id
)
DELETE FROM configuration_revisions AS r
USING removable
WHERE r.revision_id = removable.revision_id`,
				candidate.RevisionID,
				candidate.InputBundleID,
				candidate.SettingsContentDigest[:],
			)
			if err != nil {
				return fmt.Errorf("cleanup current SDK validation candidate: %w", err)
			}
			return nil
		},
	)
}

// CleanupStaleCurrentSDKValidationCandidates removes only detached candidates
// whose exactly bound validation executions and committed settlements are all
// terminal and older than the retention cutoff. Old candidates with no job are
// counted but deliberately retained: the current schema has no durable marker
// that distinguishes abandoned staging from an admission still being committed.
func (r *CurrentSDKValidationCandidatesRepository) CleanupStaleCurrentSDKValidationCandidates(
	ctx context.Context,
	request configurationapp.CurrentSDKValidationCleanupRequest,
) (configurationapp.CurrentSDKValidationCleanupResult, error) {
	if err := request.Validate(); err != nil {
		return configurationapp.CurrentSDKValidationCleanupResult{}, err
	}

	var result configurationapp.CurrentSDKValidationCleanupResult
	err := r.projects.WithinProjectTx(
		ctx,
		int64(request.ProjectID),
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			var terminalDeleted int64
			err := tx.QueryRow(ctx, `
WITH removable AS MATERIALIZED (
    SELECT r.revision_id
    FROM configuration_revisions AS r
    WHERE r.configuration_id IS NULL
      AND r.created_at <= $2
      AND EXISTS (
          SELECT 1
          FROM elitea_runtime.execution_jobs AS admitted
          JOIN elitea_runtime.input_bundles AS bundle
            ON bundle.input_bundle_id = admitted.input_bundle_id
           AND bundle.immutable_version = r.revision_id
           AND bundle.resource_project_id = $1
          JOIN elitea_runtime.input_bundle_entries AS entry
            ON entry.input_bundle_id = bundle.input_bundle_id
           AND entry.entry_id = r.settings_entry_id
           AND entry.entry_version = r.settings_entry_version
           AND entry.semantic_role = 'configuration.settings'
           AND entry.media_type = 'application/json'
           AND entry.content_digest = r.settings_content_digest
          JOIN elitea_runtime.execution_settlements AS settlement
            ON settlement.execution_id = admitted.execution_id
           AND settlement.generation = admitted.generation
           AND settlement.committed_at IS NOT NULL
           AND settlement.committed_at <= $2
          LEFT JOIN configuration_validation_projection AS projection
            ON projection.revision_id = r.revision_id
           AND projection.execution_id = admitted.execution_id
           AND projection.execution_generation = admitted.generation
           AND projection.logical_output_id = settlement.final_logical_output_id
          WHERE admitted.configuration_revision_id = r.revision_id
            AND admitted.capability_id = 'configuration.validate.v1'
            AND admitted.tenant_id = $1::text
            AND admitted.input_bundle_id = r.input_bundle_id
            AND admitted.resource_project_id = $1
            AND admitted.projection_project_id = $1
            AND admitted.actor_id = r.created_by
            AND admitted.configuration_type = r.configuration_type
            AND admitted.catalog_revision = r.catalog_revision
            AND admitted.catalog_digest = r.catalog_digest
            AND admitted.schema_id = r.schema_id
            AND admitted.schema_revision = r.schema_revision
            AND admitted.schema_digest = r.schema_digest
            AND admitted.settings_entry_id = r.settings_entry_id
            AND admitted.state IN ('SUCCEEDED', 'FAILED', 'CANCELLED')
            AND admitted.settled_at IS NOT NULL
            AND admitted.settled_at <= $2
            AND (
                (admitted.state = 'SUCCEEDED'
                    AND settlement.disposition = 'SUCCEEDED'
                    AND projection.revision_id IS NOT NULL)
                OR (admitted.state = 'FAILED' AND settlement.disposition = 'FAILED')
                OR (admitted.state = 'CANCELLED' AND settlement.disposition = 'CANCELLED')
            )
      )
      AND NOT EXISTS (
          SELECT 1
          FROM elitea_runtime.execution_jobs AS referenced
          LEFT JOIN elitea_runtime.input_bundles AS bundle
            ON bundle.input_bundle_id = referenced.input_bundle_id
          LEFT JOIN elitea_runtime.input_bundle_entries AS entry
            ON entry.input_bundle_id = bundle.input_bundle_id
           AND entry.entry_id = r.settings_entry_id
          LEFT JOIN elitea_runtime.execution_settlements AS settlement
            ON settlement.execution_id = referenced.execution_id
           AND settlement.generation = referenced.generation
          LEFT JOIN configuration_validation_projection AS projection
            ON projection.revision_id = r.revision_id
           AND projection.execution_id = referenced.execution_id
           AND projection.execution_generation = referenced.generation
           AND projection.logical_output_id = settlement.final_logical_output_id
          WHERE referenced.configuration_revision_id = r.revision_id
            AND (
                referenced.capability_id IS DISTINCT FROM 'configuration.validate.v1'
                OR referenced.tenant_id IS DISTINCT FROM $1::text
                OR referenced.input_bundle_id IS DISTINCT FROM r.input_bundle_id
                OR referenced.resource_project_id IS DISTINCT FROM $1
                OR referenced.projection_project_id IS DISTINCT FROM $1
                OR referenced.actor_id IS DISTINCT FROM r.created_by
                OR referenced.configuration_type IS DISTINCT FROM r.configuration_type
                OR referenced.catalog_revision IS DISTINCT FROM r.catalog_revision
                OR referenced.catalog_digest IS DISTINCT FROM r.catalog_digest
                OR referenced.schema_id IS DISTINCT FROM r.schema_id
                OR referenced.schema_revision IS DISTINCT FROM r.schema_revision
                OR referenced.schema_digest IS DISTINCT FROM r.schema_digest
                OR referenced.settings_entry_id IS DISTINCT FROM r.settings_entry_id
                OR bundle.immutable_version IS DISTINCT FROM r.revision_id
                OR bundle.resource_project_id IS DISTINCT FROM $1
                OR entry.entry_version IS DISTINCT FROM r.settings_entry_version
                OR entry.semantic_role IS DISTINCT FROM 'configuration.settings'
                OR entry.media_type IS DISTINCT FROM 'application/json'
                OR entry.content_digest IS DISTINCT FROM r.settings_content_digest
                OR referenced.state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED')
                OR referenced.settled_at IS NULL
                OR referenced.settled_at > $2
                OR settlement.committed_at IS NULL
                OR settlement.committed_at > $2
                OR NOT (
                    (referenced.state = 'SUCCEEDED'
                        AND settlement.disposition = 'SUCCEEDED'
                        AND projection.revision_id IS NOT NULL)
                    OR (referenced.state = 'FAILED' AND settlement.disposition = 'FAILED')
                    OR (referenced.state = 'CANCELLED' AND settlement.disposition = 'CANCELLED')
                )
            )
      )
    ORDER BY r.created_at, r.revision_id
    LIMIT $3
    FOR UPDATE OF r SKIP LOCKED
), removed_projection AS (
    DELETE FROM configuration_validation_projection AS projection
    USING removable
    WHERE projection.revision_id = removable.revision_id
    RETURNING projection.revision_id
), removed_revisions AS (
    DELETE FROM configuration_revisions AS revision
    USING removable
    WHERE revision.revision_id = removable.revision_id
    RETURNING revision.revision_id
)
SELECT count(*) FROM removed_revisions`,
				int64(request.ProjectID),
				request.OlderThan,
				request.Limit,
			).Scan(&terminalDeleted)
			if err != nil {
				return fmt.Errorf("cleanup stale terminal current SDK validation candidates: %w", err)
			}
			if terminalDeleted < 0 || terminalDeleted > int64(request.Limit) {
				return errors.New("stale current SDK validation cleanup exceeded batch limit")
			}

			var unreferencedObserved int64
			err = tx.QueryRow(ctx, `
SELECT count(*)
FROM (
    SELECT r.revision_id
    FROM configuration_revisions AS r
    WHERE r.configuration_id IS NULL
      AND r.created_at <= $1
      AND NOT EXISTS (
          SELECT 1
          FROM elitea_runtime.execution_jobs AS referenced
          WHERE referenced.configuration_revision_id = r.revision_id
      )
    ORDER BY r.created_at, r.revision_id
    LIMIT $2
) AS unreferenced`,
				request.OlderThan,
				request.Limit,
			).Scan(&unreferencedObserved)
			if err != nil {
				return fmt.Errorf("audit unreferenced current SDK validation candidates: %w", err)
			}
			if unreferencedObserved < 0 || unreferencedObserved > int64(request.Limit) {
				return errors.New("unreferenced current SDK validation audit exceeded batch limit")
			}

			result.TerminalDeleted = int32(terminalDeleted)
			result.UnreferencedObserved = int32(unreferencedObserved)
			return nil
		},
	)
	if err != nil {
		return configurationapp.CurrentSDKValidationCleanupResult{}, err
	}
	return result, nil
}

var _ configurationapp.CurrentSDKValidationCandidateStore = (*CurrentSDKValidationCandidatesRepository)(nil)
var _ configurationapp.CurrentSDKValidationCandidateJanitor = (*CurrentSDKValidationCandidatesRepository)(nil)
