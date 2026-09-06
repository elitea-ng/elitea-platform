package repos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ToolkitExecuteReadJobsRepository owns only the durable execution lifecycle
// for externally exposed read tools. The current toolkit row is resolved and
// frozen by the application layer before admission.
type ToolkitExecuteReadJobsRepository struct {
	store  sharedStore
	policy ToolkitExecuteReadDispatchPolicy
}

func (r *ToolkitExecuteReadJobsRepository) ExpectedToolkitExecuteRead(
	ctx context.Context,
	executionID string,
	generation uint64,
) (outputapp.ExpectedToolkitExecuteRead, error) {
	if executionID == "" || generation == 0 || generation > math.MaxInt64 {
		return outputapp.ExpectedToolkitExecuteRead{}, outputapp.ErrInvalidToolkitExecuteReadOutput
	}
	queries, err := runtimeSQLCQueries(r.store)
	if err != nil {
		return outputapp.ExpectedToolkitExecuteRead{}, err
	}
	row, err := queries.GetExpectedToolkitExecuteReadHeader(
		ctx,
		sqlcgen.GetExpectedToolkitExecuteReadHeaderParams{
			ExecutionID: executionID, Generation: int64(generation),
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return outputapp.ExpectedToolkitExecuteRead{}, outputapp.ErrInvalidToolkitExecuteReadOutput
	}
	if err != nil {
		return outputapp.ExpectedToolkitExecuteRead{}, fmt.Errorf("load direct toolkit output binding: %w", err)
	}
	if row.LimitsRevision != r.policy.LimitsRevision || row.Generation <= 0 ||
		row.ResourceProjectID <= 0 || row.ProjectionProjectID <= 0 {
		return outputapp.ExpectedToolkitExecuteRead{}, outputapp.ErrInvalidToolkitExecuteReadOutput
	}
	bundleDigest, err := storedDigest(row.InputBundleDigest)
	if err != nil {
		return outputapp.ExpectedToolkitExecuteRead{}, fmt.Errorf("stored direct toolkit bundle digest: %w", err)
	}
	requestDigest, err := storedDigest(row.RequestContentDigest)
	if err != nil {
		return outputapp.ExpectedToolkitExecuteRead{}, fmt.Errorf("stored direct toolkit request digest: %w", err)
	}
	expected := outputapp.ExpectedToolkitExecuteRead{
		TenantID:            row.TenantID,
		ResourceProjectID:   strconv.FormatInt(int64(row.ResourceProjectID), 10),
		ProjectionProjectID: strconv.FormatInt(int64(row.ProjectionProjectID), 10),
		CapabilityID:        row.CapabilityID, CommandID: row.CommandID,
		ExecutionID: row.ExecutionID, Generation: uint64(row.Generation),
		LogicalOutputID: "toolkit-execute-read:" + row.ExecutionID,
		InputBundleID:   row.InputBundleID, InputBundleDigest: bundleDigest,
		RequestEntryID:          row.RequestEntryID,
		RequestImmutableVersion: row.RequestImmutableVersion,
		RequestContentDigest:    requestDigest,
	}
	if err := expected.Validate(); err != nil {
		return outputapp.ExpectedToolkitExecuteRead{}, fmt.Errorf("invalid stored direct toolkit output binding: %w", err)
	}
	return expected, nil
}

func (r *ToolkitExecuteReadJobsRepository) LoadToolkitExecuteReadCompletion(
	ctx context.Context,
	executionID string,
	generation uint64,
) (toolkitexecutionapp.Completion, error) {
	if executionID == "" || generation == 0 || generation > math.MaxInt64 {
		return toolkitexecutionapp.Completion{}, toolkitexecutionapp.ErrInvalidCurrentReadTool
	}
	queries, err := runtimeSQLCQueries(r.store)
	if err != nil {
		return toolkitexecutionapp.Completion{}, err
	}
	row, err := queries.GetToolkitExecuteReadCompletion(
		ctx,
		sqlcgen.GetToolkitExecuteReadCompletionParams{
			ExecutionID: executionID, Generation: int64(generation),
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return toolkitexecutionapp.Completion{}, toolkitexecutionapp.ErrInvalidCurrentReadTool
	}
	if err != nil {
		return toolkitexecutionapp.Completion{}, fmt.Errorf("load direct toolkit completion: %w", err)
	}
	completion := toolkitexecutionapp.Completion{
		State: executiondomain.JobState(row.State), TerminalErrorCode: row.TerminalErrorCode,
		ResultJSON: append([]byte(nil), row.ResultJson...),
	}
	if row.ToolkitType != nil {
		completion.ToolkitType = *row.ToolkitType
	}
	if row.ToolkitName != nil {
		completion.ToolkitName = *row.ToolkitName
	}
	if row.ToolName != nil {
		completion.ToolName = *row.ToolName
	}
	if err := completion.Validate(); err != nil {
		return toolkitexecutionapp.Completion{}, fmt.Errorf("invalid stored direct toolkit completion: %w", err)
	}
	return completion, nil
}

func NewToolkitExecuteReadJobsRepository(
	pool *pgxpool.Pool,
	policy ToolkitExecuteReadDispatchPolicy,
) (*ToolkitExecuteReadJobsRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return newToolkitExecuteReadJobsRepository(store, policy)
}

func newToolkitExecuteReadJobsRepository(
	store sharedStore,
	policy ToolkitExecuteReadDispatchPolicy,
) (*ToolkitExecuteReadJobsRepository, error) {
	if store == nil {
		return nil, errors.New("direct toolkit execution database is required")
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	return &ToolkitExecuteReadJobsRepository{store: store, policy: policy}, nil
}

func (r *ToolkitExecuteReadJobsRepository) AdmitToolkitExecuteRead(
	ctx context.Context,
	admission toolkitexecutionapp.Admission,
) (executionapp.AdmissionOutcome, error) {
	if err := admission.Record.Validate(); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	if admission.Record.Job.CapabilityID != executiondomain.ToolkitExecuteReadCapability ||
		admission.Record.Job.Generation > math.MaxInt64 ||
		admission.Record.Outbox.Generation > math.MaxInt64 ||
		len(admission.Record.InputBundle.Manifest) > maxStoredInputManifestBytes ||
		len(admission.Record.InputBundle.Entries) != 1 ||
		len(admission.Record.InputBundle.Entries[0].Content) > executiondomain.MaxToolkitExecuteReadInputBytes ||
		!boundedToolkitExecuteReadAdmissionStrings(admission) {
		return executionapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
	}
	if err := admission.Binding.Validate(admission.Record.InputBundle); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	if err := validateInputManifest(admission.Record.InputBundle); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	resourceProjectID, err := parseProjectID(admission.Record.Job.ResourceProjectID)
	if err != nil || resourceProjectID > math.MaxInt32 {
		return executionapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
	}
	projectionProjectID, err := parseProjectID(admission.Record.Job.ProjectionProjectID)
	if err != nil || projectionProjectID > math.MaxInt32 {
		return executionapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
	}

	var outcome executionapp.AdmissionOutcome
	var capacityError *executionapp.AdmissionCapacityError
	err = r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		queries, err := runtimeSQLCQueries(tx)
		if err != nil {
			return err
		}
		existing, digest, err := loadToolkitExecuteReadAdmission(
			ctx, queries, admission.Record.IdempotencyScope, admission.Record.IdempotencyKey,
		)
		switch {
		case err == nil:
			if digest != admission.Record.RequestDigest {
				return executionapp.ErrIdempotencyConflict
			}
			outcome = existing
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("load direct toolkit idempotency binding: %w", err)
		}

		if err := lockAdmissionPolicy(ctx, tx, executiondomain.ToolkitExecuteReadCapability, r.policy.MaxOutstanding); err != nil {
			return err
		}
		existing, digest, err = loadToolkitExecuteReadAdmission(
			ctx, queries, admission.Record.IdempotencyScope, admission.Record.IdempotencyKey,
		)
		switch {
		case err == nil:
			if digest != admission.Record.RequestDigest {
				return executionapp.ErrIdempotencyConflict
			}
			outcome = existing
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("reload direct toolkit idempotency binding: %w", err)
		}

		active, err := countActiveExecutionsUpTo(ctx, tx, executiondomain.ToolkitExecuteReadCapability, r.policy.MaxOutstanding)
		if err != nil {
			return err
		}
		if active >= r.policy.MaxOutstanding {
			capacityError = &executionapp.AdmissionCapacityError{
				CapabilityID:   executiondomain.ToolkitExecuteReadCapability,
				MaxOutstanding: r.policy.MaxOutstanding,
			}
			return nil
		}

		timing, err := loadAdmissionTiming(ctx, tx, r.policy.DeadlineTTL)
		if err != nil {
			return err
		}
		if err := insertInputBundle(
			ctx, tx, resourceProjectID, admission.Record.Job.ActorID,
			admission.Record.InputBundle, timing.AdmittedAt,
		); err != nil {
			return err
		}
		createdID, err := queries.InsertToolkitExecuteReadJob(ctx, sqlcgen.InsertToolkitExecuteReadJobParams{
			ExecutionID:         admission.Record.Job.ID,
			Generation:          int64(admission.Record.Job.Generation),
			CommandID:           admission.Record.Job.CommandID,
			TenantID:            admission.Record.Job.TenantID,
			ResourceProjectID:   int32(resourceProjectID),
			ProjectionProjectID: int32(projectionProjectID),
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
			existing, digest, loadErr := loadToolkitExecuteReadAdmission(
				ctx, queries, admission.Record.IdempotencyScope, admission.Record.IdempotencyKey,
			)
			if loadErr != nil {
				return fmt.Errorf("load concurrent direct toolkit admission: %w", loadErr)
			}
			if digest != admission.Record.RequestDigest {
				return executionapp.ErrIdempotencyConflict
			}
			return admissionReplay{outcome: existing}
		}
		if err != nil {
			return fmt.Errorf("insert direct toolkit execution job: %w", err)
		}
		if createdID != admission.Record.Job.ID {
			return errors.New("direct toolkit execution insert changed identity")
		}
		if err := queries.InsertToolkitExecuteReadBinding(ctx, sqlcgen.InsertToolkitExecuteReadBindingParams{
			ExecutionID:    admission.Record.Job.ID,
			Generation:     int64(admission.Record.Job.Generation),
			InputBundleID:  admission.Record.InputBundle.ID,
			RequestEntryID: admission.Binding.RequestEntryID,
		}); err != nil {
			return fmt.Errorf("insert direct toolkit execution binding: %w", err)
		}
		if err := insertCommandOutbox(ctx, tx, r.policy, admission.Record, timing); err != nil {
			return err
		}
		outcome = executionapp.AdmissionOutcome{
			ExecutionID: admission.Record.Job.ID, CommandID: admission.Record.Job.CommandID,
			Created: true, AdmittedAt: timing.AdmittedAt, Deadline: timing.Deadline,
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

func loadToolkitExecuteReadAdmission(
	ctx context.Context,
	queries *sqlcgen.Queries,
	scope string,
	key string,
) (executionapp.AdmissionOutcome, runtimedomain.Digest, error) {
	row, err := queries.GetToolkitExecuteReadAdmissionByIdempotency(
		ctx,
		sqlcgen.GetToolkitExecuteReadAdmissionByIdempotencyParams{
			IdempotencyScope: scope, IdempotencyKey: key,
		},
	)
	if err != nil {
		return executionapp.AdmissionOutcome{}, runtimedomain.Digest{}, err
	}
	digest, err := storedDigest(row.RequestDigest)
	if err != nil {
		return executionapp.AdmissionOutcome{}, runtimedomain.Digest{}, fmt.Errorf("invalid stored direct toolkit request digest: %w", err)
	}
	if row.ExecutionID == "" || row.CommandID == "" || row.Generation != 1 ||
		!row.AdmittedAt.Valid || !row.Deadline.Valid ||
		!row.Deadline.Time.After(row.AdmittedAt.Time) {
		return executionapp.AdmissionOutcome{}, runtimedomain.Digest{}, errors.New("stored direct toolkit admission is invalid")
	}
	return executionapp.AdmissionOutcome{
		ExecutionID: row.ExecutionID, CommandID: row.CommandID, Created: false,
		AdmittedAt: row.AdmittedAt.Time.UTC(), Deadline: row.Deadline.Time.UTC(),
	}, digest, nil
}

func boundedToolkitExecuteReadAdmissionStrings(admission toolkitexecutionapp.Admission) bool {
	values := []string{
		admission.Record.IdempotencyScope, admission.Record.IdempotencyKey,
		admission.Record.InputBundle.ID, admission.Record.InputBundle.Version,
		admission.Record.InputBundle.MediaType, admission.Record.Job.ID,
		admission.Record.Job.CommandID, admission.Record.Job.TenantID,
		admission.Record.Job.ResourceProjectID, admission.Record.Job.ProjectionProjectID,
		admission.Record.Job.ActorID, admission.Record.Job.CapabilityID,
		admission.Record.Outbox.ID, admission.Binding.RequestEntryID,
	}
	for _, entry := range admission.Record.InputBundle.Entries {
		values = append(values, entry.ID, entry.Version, entry.SemanticRole,
			entry.ContentID, entry.MediaType, entry.Classification, entry.RequiredGrantAudience)
	}
	for _, value := range values {
		if value == "" || len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
	}
	return true
}

var _ toolkitexecutionapp.AtomicAdmissionStore = (*ToolkitExecuteReadJobsRepository)(nil)
var _ toolkitexecutionapp.CompletionStore = (*ToolkitExecuteReadJobsRepository)(nil)
var _ outputapp.ToolkitExecuteReadBindingRepository = (*ToolkitExecuteReadJobsRepository)(nil)
