package repos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxStoredInputManifestBytes = 64 * 1024
	maxStoredInputContentBytes  = 256 * 1024
	minValidationDeadlineTTL    = time.Millisecond
	maxValidationDeadlineTTL    = 24 * time.Hour
	// Phase one deliberately shares the Redis control-stream live-entry ceiling.
	// This keeps the serialized active-index scan and worst-case durable input
	// footprint bounded until production measurements justify a forward change.
	maxSupportedOutstandingJobs = 1_024
)

var ErrAdmissionPolicyMismatch = errors.New("execution admission policy does not match persisted policy")

type ExecutionDispatchPolicy struct {
	StreamName        string
	CapabilityVersion string
	ResourceClass     string
	IsolationClass    string
	Priority          uint32
	DeadlineTTL       time.Duration
	LimitsRevision    string
	MaxOutstanding    int64
}

// Capability-specific aliases keep composition explicit while sharing the
// same bounded durable dispatch policy shape.
type ValidationDispatchPolicy = ExecutionDispatchPolicy
type IndexIngestDispatchPolicy = ExecutionDispatchPolicy
type AgentExecutionDispatchPolicy = ExecutionDispatchPolicy
type ToolkitExecuteReadDispatchPolicy = ExecutionDispatchPolicy

func (p ExecutionDispatchPolicy) validate() error {
	if p.StreamName == "" || p.CapabilityVersion == "" || p.ResourceClass == "" || p.IsolationClass == "" || p.Priority == 0 || p.Priority > math.MaxInt32 || p.DeadlineTTL < minValidationDeadlineTTL || p.DeadlineTTL > maxValidationDeadlineTTL || p.DeadlineTTL%time.Millisecond != 0 || p.LimitsRevision == "" || p.MaxOutstanding <= 0 || p.MaxOutstanding > maxSupportedOutstandingJobs {
		return errors.New("execution dispatch policy is incomplete")
	}
	for _, value := range []string{p.StreamName, p.CapabilityVersion, p.ResourceClass, p.IsolationClass, p.LimitsRevision} {
		if len(value) > 256 || strings.ContainsRune(value, '\x00') {
			return errors.New("execution dispatch policy exceeds storage bounds")
		}
	}
	return nil
}

type ExecutionJobsRepository struct {
	store  sharedStore
	policy ValidationDispatchPolicy
}

func NewExecutionJobsRepository(pool *pgxpool.Pool, policy ValidationDispatchPolicy) (*ExecutionJobsRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return newExecutionJobsRepository(store, policy)
}

func newExecutionJobsRepository(store sharedStore, policy ValidationDispatchPolicy) (*ExecutionJobsRepository, error) {
	if store == nil {
		return nil, errors.New("execution database is required")
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	return &ExecutionJobsRepository{store: store, policy: policy}, nil
}

func (r *ExecutionJobsRepository) AdmitValidation(ctx context.Context, admission executionapp.ValidationAdmission) (executionapp.AdmissionOutcome, error) {
	if err := admission.Record.Validate(); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	if err := admission.Command.Validate(); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	if len(admission.Record.InputBundle.Entries) != 1 || admission.Record.InputBundle.Entries[0].ID != admission.Command.SettingsEntryID || len(admission.Record.InputBundle.Manifest) > maxStoredInputManifestBytes || len(admission.Record.InputBundle.Entries[0].Content) > maxStoredInputContentBytes {
		return executionapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
	}
	if !boundedAdmissionStrings(admission) {
		return executionapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
	}
	resourceProjectID, err := parseProjectID(admission.Record.Job.ResourceProjectID)
	if err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("resource project: %w", err)
	}
	projectionProjectID, err := parseProjectID(admission.Record.Job.ProjectionProjectID)
	if err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("projection project: %w", err)
	}

	var outcome executionapp.AdmissionOutcome
	var capacityError *executionapp.AdmissionCapacityError
	err = r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		existing, requestDigest, err := loadAdmissionByIdempotency(ctx, tx, admission.Record.IdempotencyScope, admission.Record.IdempotencyKey)
		switch {
		case err == nil:
			if requestDigest != admission.Record.RequestDigest {
				return executionapp.ErrIdempotencyConflict
			}
			outcome = existing
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("load validation idempotency binding: %w", err)
		}

		if err := lockAdmissionPolicy(ctx, tx, admission.Record.Job.CapabilityID, r.policy.MaxOutstanding); err != nil {
			return err
		}

		// A transaction using the same idempotency key may have committed while
		// this transaction waited for the capability policy row. Recheck under
		// that lock before evaluating capacity so exact replay remains available
		// even when the active-job high-water mark has been reached.
		existing, requestDigest, err = loadAdmissionByIdempotency(ctx, tx, admission.Record.IdempotencyScope, admission.Record.IdempotencyKey)
		switch {
		case err == nil:
			if requestDigest != admission.Record.RequestDigest {
				return executionapp.ErrIdempotencyConflict
			}
			outcome = existing
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("reload validation idempotency binding: %w", err)
		}

		active, err := countActiveExecutionsUpTo(ctx, tx, admission.Record.Job.CapabilityID, r.policy.MaxOutstanding)
		if err != nil {
			return err
		}
		if active >= r.policy.MaxOutstanding {
			// Commit a newly inserted policy row even when the first request finds
			// pre-existing work already at capacity. No input/job/outbox rows have
			// been written, and the caller receives the typed rejection after commit.
			capacityError = &executionapp.AdmissionCapacityError{
				CapabilityID:   admission.Record.Job.CapabilityID,
				MaxOutstanding: r.policy.MaxOutstanding,
			}
			return nil
		}

		timing, err := loadAdmissionTiming(ctx, tx, r.policy.DeadlineTTL)
		if err != nil {
			return err
		}
		if err := insertInputBundle(ctx, tx, resourceProjectID, admission.Record.Job.ActorID, admission.Record.InputBundle, timing.AdmittedAt); err != nil {
			return err
		}
		created, err := insertExecutionJob(ctx, tx, resourceProjectID, projectionProjectID, r.policy, admission, timing.AdmittedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			// A concurrent transaction won the unique idempotency key. Returning a
			// private replay error forces this transaction to roll back its newly
			// generated input bundle before returning the durable winner.
			existing, requestDigest, loadErr := loadAdmissionByIdempotency(ctx, tx, admission.Record.IdempotencyScope, admission.Record.IdempotencyKey)
			if loadErr != nil {
				return fmt.Errorf("load concurrent validation admission: %w", loadErr)
			}
			if requestDigest != admission.Record.RequestDigest {
				return executionapp.ErrIdempotencyConflict
			}
			return admissionReplay{outcome: existing}
		}
		if err != nil {
			return err
		}
		if !created {
			return errors.New("execution job insert did not report creation")
		}
		if err := insertCommandOutbox(ctx, tx, r.policy, admission.Record, timing); err != nil {
			return err
		}
		outcome = executionapp.AdmissionOutcome{
			ExecutionID: admission.Record.Job.ID,
			CommandID:   admission.Record.Job.CommandID,
			Created:     true,
			AdmittedAt:  timing.AdmittedAt,
			Deadline:    timing.Deadline,
		}
		return nil
	})
	var replay admissionReplay
	if errors.As(err, &replay) {
		return replay.outcome, nil
	}
	if err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	if capacityError != nil {
		return executionapp.AdmissionOutcome{}, capacityError
	}
	return outcome, nil
}

func lockAdmissionPolicy(ctx context.Context, tx sqlExecutor, capabilityID string, maxOutstanding int64) error {
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.execution_admission_policies (
    capability_id, max_outstanding
) VALUES ($1, $2)
ON CONFLICT (capability_id) DO NOTHING`, capabilityID, maxOutstanding); err != nil {
		return fmt.Errorf("ensure execution admission policy: %w", err)
	}

	var persistedMax int64
	if err := tx.QueryRow(ctx, `
SELECT max_outstanding
FROM elitea_runtime.execution_admission_policies
WHERE capability_id = $1
FOR UPDATE`, capabilityID).Scan(&persistedMax); err != nil {
		return fmt.Errorf("lock execution admission policy: %w", err)
	}
	if persistedMax != maxOutstanding {
		return fmt.Errorf("%w: capability %q configured=%d persisted=%d", ErrAdmissionPolicyMismatch, capabilityID, maxOutstanding, persistedMax)
	}
	return nil
}

func countActiveExecutionsUpTo(ctx context.Context, tx sqlExecutor, capabilityID string, limit int64) (int64, error) {
	var active int64
	err := tx.QueryRow(ctx, `
SELECT count(*)
FROM (
    SELECT 1
    FROM elitea_runtime.execution_jobs
    WHERE capability_id = $1
      AND state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')
    LIMIT $2
) AS bounded_active`, capabilityID, limit).Scan(&active)
	if err != nil {
		return 0, fmt.Errorf("count active executions for admission: %w", err)
	}
	return active, nil
}

func boundedAdmissionStrings(admission executionapp.ValidationAdmission) bool {
	record := admission.Record
	command := admission.Command
	values := []string{
		record.IdempotencyScope, record.IdempotencyKey,
		record.InputBundle.ID, record.InputBundle.Version, record.InputBundle.MediaType,
		record.Job.ID, record.Job.CommandID, record.Job.TenantID,
		record.Job.ResourceProjectID, record.Job.ProjectionProjectID,
		record.Job.ActorID, record.Job.CapabilityID, record.Outbox.ID,
		command.ConfigurationRevisionID, command.ConfigurationType,
		command.CatalogRevision, command.SchemaID, command.SchemaRevision,
		command.SettingsEntryID,
	}
	for _, entry := range record.InputBundle.Entries {
		values = append(values,
			entry.ID, entry.Version, entry.SemanticRole, entry.ContentID,
			entry.MediaType, entry.Classification, entry.RequiredGrantAudience,
		)
	}
	for _, value := range values {
		if value == "" || len(value) > 256 || strings.ContainsRune(value, '\x00') {
			return false
		}
	}
	return true
}

type admissionReplay struct {
	outcome executionapp.AdmissionOutcome
}

func (e admissionReplay) Error() string { return "concurrent admission replay" }

type admissionTiming struct {
	AdmittedAt time.Time
	Deadline   time.Time
}

func loadAdmissionTiming(ctx context.Context, db sqlExecutor, deadlineTTL time.Duration) (admissionTiming, error) {
	var timing admissionTiming
	err := db.QueryRow(ctx, `
WITH authority AS MATERIALIZED (
    SELECT date_trunc('milliseconds', clock_timestamp()) AS admitted_at
)
SELECT admitted_at,
       admitted_at + ($1::bigint * interval '1 millisecond')
FROM authority`, deadlineTTL.Milliseconds()).Scan(&timing.AdmittedAt, &timing.Deadline)
	if err != nil {
		return admissionTiming{}, fmt.Errorf("load database admission timing: %w", err)
	}
	if timing.AdmittedAt.IsZero() || !timing.Deadline.After(timing.AdmittedAt) || timing.Deadline.Sub(timing.AdmittedAt) != deadlineTTL {
		return admissionTiming{}, errors.New("database returned invalid admission timing")
	}
	return admissionTiming{
		AdmittedAt: timing.AdmittedAt.UTC(),
		Deadline:   timing.Deadline.UTC(),
	}, nil
}

func loadAdmissionByIdempotency(ctx context.Context, db sqlExecutor, scope, key string) (executionapp.AdmissionOutcome, runtimedomain.Digest, error) {
	var outcome executionapp.AdmissionOutcome
	var digestBytes []byte
	err := db.QueryRow(ctx, `
SELECT j.execution_id, j.command_id, j.request_digest,
       j.admitted_at, o.deadline
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE j.idempotency_scope = $1 AND j.idempotency_key = $2`, scope, key).Scan(
		&outcome.ExecutionID,
		&outcome.CommandID,
		&digestBytes,
		&outcome.AdmittedAt,
		&outcome.Deadline,
	)
	if err != nil {
		return executionapp.AdmissionOutcome{}, runtimedomain.Digest{}, err
	}
	digest, err := storedDigest(digestBytes)
	if err != nil {
		return executionapp.AdmissionOutcome{}, runtimedomain.Digest{}, fmt.Errorf("invalid stored request digest: %w", err)
	}
	outcome.Created = false
	outcome.AdmittedAt = outcome.AdmittedAt.UTC()
	outcome.Deadline = outcome.Deadline.UTC()
	if outcome.AdmittedAt.IsZero() || !outcome.Deadline.After(outcome.AdmittedAt) {
		return executionapp.AdmissionOutcome{}, runtimedomain.Digest{}, errors.New("stored admission timing is invalid")
	}
	return outcome, digest, nil
}

func insertExecutionJob(ctx context.Context, tx sqlExecutor, resourceProjectID, projectionProjectID int64, policy ExecutionDispatchPolicy, admission executionapp.ValidationAdmission, admittedAt time.Time) (bool, error) {
	record := admission.Record
	command := admission.Command
	var executionID string
	err := tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest,
    idempotency_scope, idempotency_key, configuration_revision_id,
    configuration_type, catalog_revision, catalog_digest, schema_id,
    schema_revision, schema_digest, settings_entry_id, state, desired_state,
    admitted_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $7, $8, $9, $10, $11, $12,
    $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, 'RUNNING', $23
)
ON CONFLICT (idempotency_scope, idempotency_key) DO NOTHING
RETURNING execution_id`,
		record.Job.ID,
		int64(record.Job.Generation),
		record.Job.CommandID,
		record.Job.TenantID,
		resourceProjectID,
		projectionProjectID,
		record.Job.ActorID,
		record.Job.CapabilityID,
		policy.CapabilityVersion,
		record.InputBundle.ID,
		record.RequestDigest[:],
		record.IdempotencyScope,
		record.IdempotencyKey,
		command.ConfigurationRevisionID,
		command.ConfigurationType,
		command.CatalogRevision,
		command.CatalogDigest[:],
		command.SchemaID,
		command.SchemaRevision,
		command.SchemaDigest[:],
		command.SettingsEntryID,
		string(record.Job.State),
		admittedAt,
	).Scan(&executionID)
	if err != nil {
		return false, fmt.Errorf("insert execution job: %w", err)
	}
	return executionID == record.Job.ID, nil
}

func storedDigest(value []byte) (runtimedomain.Digest, error) {
	var digest runtimedomain.Digest
	if len(value) != len(digest) {
		return digest, runtimedomain.ErrInvalidDigest
	}
	copy(digest[:], value)
	return digest, nil
}

var _ executionapp.AtomicAdmissionStore = (*ExecutionJobsRepository)(nil)
