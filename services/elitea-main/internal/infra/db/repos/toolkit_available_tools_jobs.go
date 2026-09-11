package repos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkitdiscoveryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ToolkitAvailableToolsDispatchPolicy = ExecutionDispatchPolicy

var ErrPendingToolkitAvailableToolsDispatchNotFound = errors.New("pending toolkit discovery dispatch not found")

type ToolkitAvailableToolsJobsRepository struct {
	pool   *pgxpool.Pool
	policy ToolkitAvailableToolsDispatchPolicy
}

func NewToolkitAvailableToolsJobsRepository(
	pool *pgxpool.Pool,
	policy ToolkitAvailableToolsDispatchPolicy,
) (*ToolkitAvailableToolsJobsRepository, error) {
	if pool == nil {
		return nil, errors.New("toolkit discovery admission database is required")
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	return &ToolkitAvailableToolsJobsRepository{pool: pool, policy: policy}, nil
}

func (r *ToolkitAvailableToolsJobsRepository) AdmitToolkitAvailableTools(
	ctx context.Context,
	admission toolkitdiscoveryapp.Admission,
) (executionapp.AdmissionOutcome, error) {
	if err := admission.Record.Validate(); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	if admission.Record.Job.CapabilityID != executiondomain.ToolkitAvailableToolsCapability {
		return executionapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
	}
	if err := admission.Binding.Validate(admission.Record.InputBundle); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	if err := validateInputManifest(admission.Record.InputBundle); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	if len(admission.Record.InputBundle.Manifest) > maxStoredInputManifestBytes ||
		len(admission.Record.InputBundle.Entries) != 2 ||
		!boundedDiscoveryAdmissionStrings(admission) ||
		admission.Record.Job.Generation > math.MaxInt64 ||
		admission.Record.Outbox.Generation > math.MaxInt64 {
		return executionapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
	}
	resourceProject, err := parseProjectID(admission.Record.Job.ResourceProjectID)
	if err != nil || resourceProject > math.MaxInt32 {
		return executionapp.AdmissionOutcome{}, fmt.Errorf(
			"resource project: %w",
			errors.New("project ID must fit the current integer schema"),
		)
	}
	projectionProject, err := parseProjectID(admission.Record.Job.ProjectionProjectID)
	if err != nil || projectionProject > math.MaxInt32 {
		return executionapp.AdmissionOutcome{}, fmt.Errorf(
			"projection project: %w",
			errors.New("project ID must fit the current integer schema"),
		)
	}

	queries := sqlcgen.New(r.pool)
	existing, digest, err := loadDiscoveryAdmission(
		ctx, queries,
		admission.Record.IdempotencyScope, admission.Record.IdempotencyKey,
	)
	switch {
	case err == nil:
		if digest != admission.Record.RequestDigest {
			return executionapp.AdmissionOutcome{}, executionapp.ErrIdempotencyConflict
		}
		return existing, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return executionapp.AdmissionOutcome{}, fmt.Errorf("load toolkit discovery idempotency binding: %w", err)
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("begin toolkit discovery admission transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	txQueries := sqlcgen.New(tx)
	capabilityID := executiondomain.ToolkitAvailableToolsCapability
	if err := txQueries.EnsureRuntimeAdmissionPolicy(ctx, sqlcgen.EnsureRuntimeAdmissionPolicyParams{
		CapabilityID:   capabilityID,
		MaxOutstanding: r.policy.MaxOutstanding,
	}); err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("ensure toolkit discovery admission policy: %w", err)
	}
	persistedMax, err := txQueries.LockRuntimeAdmissionPolicy(ctx, capabilityID)
	if err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("lock toolkit discovery admission policy: %w", err)
	}
	if persistedMax != r.policy.MaxOutstanding {
		return executionapp.AdmissionOutcome{}, fmt.Errorf(
			"%w: capability %q configured=%d persisted=%d",
			ErrAdmissionPolicyMismatch, capabilityID, r.policy.MaxOutstanding, persistedMax,
		)
	}

	existing, digest, err = loadDiscoveryAdmission(
		ctx, txQueries,
		admission.Record.IdempotencyScope, admission.Record.IdempotencyKey,
	)
	switch {
	case err == nil:
		if digest != admission.Record.RequestDigest {
			return executionapp.AdmissionOutcome{}, executionapp.ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return executionapp.AdmissionOutcome{}, fmt.Errorf("commit toolkit discovery admission replay: %w", err)
		}
		committed = true
		return existing, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return executionapp.AdmissionOutcome{}, fmt.Errorf("reload toolkit discovery idempotency binding: %w", err)
	}

	active, err := txQueries.CountActiveRuntimeExecutionsUpTo(ctx, sqlcgen.CountActiveRuntimeExecutionsUpToParams{
		CapabilityID:   capabilityID,
		MaxOutstanding: r.policy.MaxOutstanding,
	})
	if err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("count active toolkit discoverys: %w", err)
	}
	if active >= r.policy.MaxOutstanding {
		if err := tx.Commit(ctx); err != nil {
			return executionapp.AdmissionOutcome{}, fmt.Errorf("commit toolkit discovery capacity observation: %w", err)
		}
		committed = true
		return executionapp.AdmissionOutcome{}, &executionapp.AdmissionCapacityError{
			CapabilityID:   capabilityID,
			MaxOutstanding: r.policy.MaxOutstanding,
		}
	}

	timingRow, err := txQueries.LoadRuntimeAdmissionTiming(ctx, r.policy.DeadlineTTL.Milliseconds())
	if err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("load toolkit discovery admission timing: %w", err)
	}
	timing, err := decodeAdmissionTiming(timingRow, r.policy.DeadlineTTL)
	if err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	if err := insertRuntimeInputBundle(
		ctx, txQueries, int32(resourceProject), admission.Record, timing.AdmittedAt,
	); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	createdID, err := txQueries.InsertToolkitAvailableToolsJob(ctx, sqlcgen.InsertToolkitAvailableToolsJobParams{
		ExecutionID:         admission.Record.Job.ID,
		Generation:          int64(admission.Record.Job.Generation),
		CommandID:           admission.Record.Job.CommandID,
		TenantID:            admission.Record.Job.TenantID,
		ResourceProjectID:   int32(resourceProject),
		ProjectionProjectID: int32(projectionProject),
		ActorID:             admission.Record.Job.ActorID,
		PrincipalRef:        admission.Record.Job.ActorID,
		CapabilityVersion:   r.policy.CapabilityVersion,
		InputBundleID:       admission.Record.InputBundle.ID,
		RequestDigest:       append([]byte(nil), admission.Record.RequestDigest[:]...),
		IdempotencyScope:    admission.Record.IdempotencyScope,
		IdempotencyKey:      admission.Record.IdempotencyKey,
		State:               string(admission.Record.Job.State),
		AdmittedAt:          timestamp(timing.AdmittedAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		existing, digest, loadErr := loadDiscoveryAdmission(
			ctx, txQueries,
			admission.Record.IdempotencyScope, admission.Record.IdempotencyKey,
		)
		if loadErr != nil {
			return executionapp.AdmissionOutcome{}, fmt.Errorf("load concurrent toolkit discovery admission: %w", loadErr)
		}
		if digest != admission.Record.RequestDigest {
			return executionapp.AdmissionOutcome{}, executionapp.ErrIdempotencyConflict
		}
		if rollbackErr := tx.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil &&
			!errors.Is(rollbackErr, pgx.ErrTxClosed) {
			return executionapp.AdmissionOutcome{}, fmt.Errorf("rollback concurrent toolkit discovery admission: %w", rollbackErr)
		}
		committed = true
		return existing, nil
	}
	if err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("insert toolkit discovery execution job: %w", err)
	}
	if createdID != admission.Record.Job.ID {
		return executionapp.AdmissionOutcome{}, errors.New("toolkit discovery execution job insert changed identity")
	}
	if err := txQueries.InsertRuntimeCommandOutbox(ctx, sqlcgen.InsertRuntimeCommandOutboxParams{
		OutboxID:       admission.Record.Outbox.ID,
		ExecutionID:    admission.Record.Outbox.ExecutionID,
		Generation:     int64(admission.Record.Outbox.Generation),
		StreamName:     r.policy.StreamName,
		ResourceClass:  r.policy.ResourceClass,
		IsolationClass: r.policy.IsolationClass,
		Priority:       int32(r.policy.Priority),
		Deadline:       timestamp(timing.Deadline),
		LimitsRevision: r.policy.LimitsRevision,
		CreatedAt:      timestamp(timing.AdmittedAt),
	}); err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("insert toolkit discovery command outbox: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("commit toolkit discovery admission: %w", err)
	}
	committed = true
	return executionapp.AdmissionOutcome{
		ExecutionID: admission.Record.Job.ID,
		CommandID:   admission.Record.Job.CommandID,
		Created:     true,
		AdmittedAt:  timing.AdmittedAt,
		Deadline:    timing.Deadline,
	}, nil
}

func loadDiscoveryAdmission(
	ctx context.Context,
	queries *sqlcgen.Queries,
	scope, key string,
) (executionapp.AdmissionOutcome, runtimedomain.Digest, error) {
	row, err := queries.GetToolkitAvailableToolsAdmissionByIdempotency(
		ctx,
		sqlcgen.GetToolkitAvailableToolsAdmissionByIdempotencyParams{
			IdempotencyScope: scope,
			IdempotencyKey:   key,
		},
	)
	if err != nil {
		return executionapp.AdmissionOutcome{}, runtimedomain.Digest{}, err
	}
	digest, err := storedDigest(row.RequestDigest)
	if err != nil {
		return executionapp.AdmissionOutcome{}, runtimedomain.Digest{},
			fmt.Errorf("invalid stored toolkit discovery request digest: %w", err)
	}
	if !row.AdmittedAt.Valid || !row.Deadline.Valid || row.AdmittedAt.Time.IsZero() ||
		!row.Deadline.Time.After(row.AdmittedAt.Time) || row.Generation <= 0 {
		return executionapp.AdmissionOutcome{}, runtimedomain.Digest{},
			errors.New("stored toolkit discovery admission timing is invalid")
	}
	return executionapp.AdmissionOutcome{
		ExecutionID: row.ExecutionID,
		CommandID:   row.CommandID,
		Created:     false,
		AdmittedAt:  row.AdmittedAt.Time.UTC(),
		Deadline:    row.Deadline.Time.UTC(),
	}, digest, nil
}

func boundedDiscoveryAdmissionStrings(admission toolkitdiscoveryapp.Admission) bool {
	record := admission.Record
	binding := admission.Binding
	values := []string{
		record.IdempotencyScope, record.IdempotencyKey,
		record.InputBundle.ID, record.InputBundle.Version, record.InputBundle.MediaType,
		record.Job.ID, record.Job.CommandID, record.Job.TenantID,
		record.Job.ResourceProjectID, record.Job.ProjectionProjectID,
		record.Job.ActorID, record.Job.CapabilityID, record.Outbox.ID,
		binding.ToolkitType,
		binding.SettingsEntryID,
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
	return len(binding.ToolkitVersion) <= 256 &&
		!strings.ContainsRune(binding.ToolkitVersion, '\x00')
}

var _ toolkitdiscoveryapp.AtomicAdmissionStore = (*ToolkitAvailableToolsJobsRepository)(nil)
