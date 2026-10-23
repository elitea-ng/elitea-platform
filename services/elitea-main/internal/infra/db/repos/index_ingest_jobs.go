package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

type IndexIngestJobsRepository struct {
	pool   *pgxpool.Pool
	policy IndexIngestDispatchPolicy
}

var (
	indexMetaInitializationFailureEventBytes = []byte(
		`{"code":"INTERNAL","safe_message":"The runtime operation failed.","retryable":false}`,
	)
	indexMetaInitializationFailureEventDigest = runtimedomain.SHA256(
		indexMetaInitializationFailureEventBytes,
	)
)

func NewIndexIngestJobsRepository(pool *pgxpool.Pool, policy IndexIngestDispatchPolicy) (*IndexIngestJobsRepository, error) {
	if pool == nil {
		return nil, errors.New("index admission database is required")
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	return &IndexIngestJobsRepository{pool: pool, policy: policy}, nil
}

func (r *IndexIngestJobsRepository) AdmitIndexIngest(ctx context.Context, admission indexingapp.Admission) (indexingapp.AdmissionOutcome, error) {
	if err := admission.Record.Validate(); err != nil {
		return indexingapp.AdmissionOutcome{}, err
	}
	if admission.Record.Job.CapabilityID != executiondomain.IndexIngestCapability {
		return indexingapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
	}
	if err := admission.Binding.Validate(admission.Record.InputBundle); err != nil {
		return indexingapp.AdmissionOutcome{}, err
	}
	if err := validateInputManifest(admission.Record.InputBundle); err != nil {
		return indexingapp.AdmissionOutcome{}, err
	}
	if len(admission.Record.InputBundle.Manifest) > maxStoredInputManifestBytes || len(admission.Record.InputBundle.Entries) > executiondomain.MaxInputBundleEntries {
		return indexingapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
	}
	for _, entry := range admission.Record.InputBundle.Entries {
		if len(entry.Content) > executiondomain.MaxInputEntryContentBytes {
			return indexingapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
		}
	}
	if !boundedIndexAdmissionStrings(admission) || admission.Record.Job.Generation > math.MaxInt64 || admission.Record.Outbox.Generation > math.MaxInt64 {
		return indexingapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
	}
	resourceProject, err := parseProjectID(admission.Record.Job.ResourceProjectID)
	if err != nil || resourceProject > math.MaxInt32 {
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("resource project: %w", errors.New("project ID must fit the current integer schema"))
	}
	projectionProject, err := parseProjectID(admission.Record.Job.ProjectionProjectID)
	if err != nil || projectionProject > math.MaxInt32 {
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("projection project: %w", errors.New("project ID must fit the current integer schema"))
	}

	queries := sqlcgen.New(r.pool)
	existing, digest, err := loadIndexAdmission(ctx, queries, admission.Record.IdempotencyScope, admission.Record.IdempotencyKey)
	switch {
	case err == nil:
		if digest != admission.Record.RequestDigest {
			return indexingapp.AdmissionOutcome{}, executionapp.ErrIdempotencyConflict
		}
		return existing, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("load index idempotency binding: %w", err)
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("begin index admission transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	txQueries := sqlcgen.New(tx)
	if err := txQueries.EnsureRuntimeAdmissionPolicy(ctx, sqlcgen.EnsureRuntimeAdmissionPolicyParams{
		CapabilityID:   executiondomain.IndexIngestCapability,
		MaxOutstanding: r.policy.MaxOutstanding,
	}); err != nil {
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("ensure index admission policy: %w", err)
	}
	persistedMax, err := txQueries.LockRuntimeAdmissionPolicy(ctx, executiondomain.IndexIngestCapability)
	if err != nil {
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("lock index admission policy: %w", err)
	}
	if persistedMax != r.policy.MaxOutstanding {
		return indexingapp.AdmissionOutcome{}, admissionPolicyMismatchError(executiondomain.IndexIngestCapability, r.policy.MaxOutstanding, persistedMax)
	}

	existing, digest, err = loadIndexAdmission(ctx, txQueries, admission.Record.IdempotencyScope, admission.Record.IdempotencyKey)
	switch {
	case err == nil:
		if digest != admission.Record.RequestDigest {
			return indexingapp.AdmissionOutcome{}, executionapp.ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return indexingapp.AdmissionOutcome{}, fmt.Errorf("commit index admission replay: %w", err)
		}
		committed = true
		return existing, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("reload index idempotency binding: %w", err)
	}

	// Allocation locks the target-scoped counter until commit. Competing starts
	// therefore cannot both observe an empty active set under READ COMMITTED.
	// A same-target rejection rolls back the allocation with the transaction.
	indexGeneration, err := txQueries.AllocateIndexGeneration(ctx, sqlcgen.AllocateIndexGenerationParams{
		ResourceProjectID: int32(resourceProject),
		ToolkitID:         admission.Binding.ToolkitID,
		IndexName:         admission.Binding.IndexName,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return indexingapp.AdmissionOutcome{}, indexingapp.ErrIndexGenerationExhausted
	}
	if err != nil {
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("allocate index generation: %w", err)
	}
	if indexGeneration <= 0 {
		return indexingapp.AdmissionOutcome{}, errors.New("database returned invalid index generation")
	}

	activeTargets, err := txQueries.ListActiveIndexIngestTarget(
		ctx,
		sqlcgen.ListActiveIndexIngestTargetParams{
			TenantID:            admission.Record.Job.TenantID,
			ResourceProjectID:   int32(resourceProject),
			ProjectionProjectID: int32(projectionProject),
			ToolkitID:           admission.Binding.ToolkitID,
			IndexName:           admission.Binding.IndexName,
		},
	)
	if err != nil {
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("load active index target: %w", err)
	}
	switch len(activeTargets) {
	case 0:
	case 1:
		return indexingapp.AdmissionOutcome{},
			indexingapp.NewActiveIndexConflictError(activeTargets[0])
	default:
		return indexingapp.AdmissionOutcome{},
			indexingapp.ErrIndexMetaInitializationAmbiguous
	}

	active, err := txQueries.CountActiveRuntimeExecutionsUpTo(ctx, sqlcgen.CountActiveRuntimeExecutionsUpToParams{
		CapabilityID:   executiondomain.IndexIngestCapability,
		MaxOutstanding: r.policy.MaxOutstanding,
	})
	if err != nil {
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("count active index executions: %w", err)
	}
	if active >= r.policy.MaxOutstanding {
		if err := tx.Commit(ctx); err != nil {
			return indexingapp.AdmissionOutcome{}, fmt.Errorf("commit index admission capacity observation: %w", err)
		}
		committed = true
		return indexingapp.AdmissionOutcome{}, &executionapp.AdmissionCapacityError{
			CapabilityID:   executiondomain.IndexIngestCapability,
			MaxOutstanding: r.policy.MaxOutstanding,
		}
	}

	timingRow, err := txQueries.LoadRuntimeAdmissionTiming(ctx, r.policy.DeadlineTTL.Milliseconds())
	if err != nil {
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("load index admission timing: %w", err)
	}
	timing, err := decodeAdmissionTiming(timingRow, r.policy.DeadlineTTL)
	if err != nil {
		return indexingapp.AdmissionOutcome{}, err
	}
	if err := insertRuntimeInputBundle(ctx, txQueries, int32(resourceProject), admission.Record, timing.AdmittedAt); err != nil {
		return indexingapp.AdmissionOutcome{}, err
	}
	createdID, err := txQueries.InsertIndexIngestExecutionJob(ctx, sqlcgen.InsertIndexIngestExecutionJobParams{
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
		existing, digest, loadErr := loadIndexAdmission(ctx, txQueries, admission.Record.IdempotencyScope, admission.Record.IdempotencyKey)
		if loadErr != nil {
			return indexingapp.AdmissionOutcome{}, fmt.Errorf("load concurrent index admission: %w", loadErr)
		}
		if digest != admission.Record.RequestDigest {
			return indexingapp.AdmissionOutcome{}, executionapp.ErrIdempotencyConflict
		}
		if rollbackErr := tx.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			return indexingapp.AdmissionOutcome{}, fmt.Errorf("rollback concurrent index admission: %w", rollbackErr)
		}
		committed = true
		return existing, nil
	}
	if err != nil {
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("insert index execution job: %w", err)
	}
	if createdID != admission.Record.Job.ID {
		return indexingapp.AdmissionOutcome{}, errors.New("index execution job insert changed identity")
	}
	if err := txQueries.InsertIndexIngestJob(ctx, sqlcgen.InsertIndexIngestJobParams{
		ExecutionID:                          admission.Record.Job.ID,
		Generation:                           int64(admission.Record.Job.Generation),
		IndexGeneration:                      indexGeneration,
		InputBundleID:                        admission.Record.InputBundle.ID,
		ToolkitConfigurationEntryID:          admission.Binding.ToolkitConfigurationEntryID,
		ToolParametersEntryID:                admission.Binding.ToolParametersEntryID,
		LlmModelEntryID:                      optionalString(admission.Binding.LLMModelEntryID),
		LlmConfigurationEntryID:              optionalString(admission.Binding.LLMConfigurationEntryID),
		McpTokensEntryID:                     optionalString(admission.Binding.MCPTokensEntryID),
		EmbeddingBindingEntryID:              optionalString(admission.Binding.EmbeddingBindingEntryID),
		ClientStreamID:                       optionalString(admission.Binding.ClientStreamID),
		ClientMessageID:                      optionalString(admission.Binding.ClientMessageID),
		SioEvent:                             optionalString(admission.Binding.SIOEvent),
		IndexMetaID:                          admission.Binding.IndexMetaID,
		IndexMetaCorrelationID:               admission.Binding.IndexMetaCorrelationID,
		IndexMetaInitializationNextAttemptAt: timestamp(timing.AdmittedAt),
		ToolkitID:                            admission.Binding.ToolkitID,
		IndexName:                            admission.Binding.IndexName,
		Initiator:                            string(admission.Binding.Initiator),
	}); err != nil {
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("insert index capability job: %w", err)
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
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("insert index command outbox: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return indexingapp.AdmissionOutcome{}, fmt.Errorf("commit index admission: %w", err)
	}
	committed = true
	return indexingapp.AdmissionOutcome{
		AdmissionOutcome: executionapp.AdmissionOutcome{
			ExecutionID: admission.Record.Job.ID,
			CommandID:   admission.Record.Job.CommandID,
			Created:     true,
			AdmittedAt:  timing.AdmittedAt,
			Deadline:    timing.Deadline,
		},
		Generation:             admission.Record.Job.Generation,
		IndexGeneration:        uint64(indexGeneration),
		IndexMetaID:            admission.Binding.IndexMetaID,
		IndexMetaCorrelationID: admission.Binding.IndexMetaCorrelationID,
	}, nil
}

// MarkIndexMetaInitialized opens the dispatch gate only for the exact durable
// execution/generation/meta identity. Repeating the same transition returns the
// original database timestamp; a mismatched identity cannot make work visible.
func (r *IndexIngestJobsRepository) MarkIndexMetaInitialized(
	ctx context.Context,
	initialization indexingapp.IndexMetaInitialization,
) (time.Time, error) {
	if err := initialization.Validate(); err != nil {
		return time.Time{}, err
	}
	if initialization.Generation > math.MaxInt64 || initialization.IndexGeneration > math.MaxInt64 {
		return time.Time{}, indexingapp.ErrIndexMetaInitializationMismatch
	}
	initializedAt, err := sqlcgen.New(r.pool).MarkIndexMetaInitialized(ctx, sqlcgen.MarkIndexMetaInitializedParams{
		ExecutionID:            initialization.ExecutionID,
		Generation:             int64(initialization.Generation),
		IndexGeneration:        int64(initialization.IndexGeneration),
		IndexMetaID:            initialization.MetaID,
		IndexMetaCorrelationID: initialization.CorrelationID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, indexingapp.ErrIndexMetaInitializationMismatch
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("mark index metadata initialized: %w", err)
	}
	if !initializedAt.Valid || initializedAt.Time.IsZero() {
		return time.Time{}, errors.New("database returned invalid index metadata initialization time")
	}
	return initializedAt.Time.UTC(), nil
}

func (r *IndexIngestJobsRepository) ClaimExactIndexMetaInitialization(
	ctx context.Context,
	initialization indexingapp.IndexMetaInitialization,
	claimToken string,
	lease time.Duration,
) (indexingapp.IndexMetaInitializationClaim, error) {
	if ctx == nil || initialization.Validate() != nil ||
		initialization.Generation > math.MaxInt64 ||
		initialization.IndexGeneration > math.MaxInt64 ||
		!validIndexMetaInitializationClaimToken(claimToken) ||
		lease <= 0 || lease > 10*time.Minute {
		return indexingapp.IndexMetaInitializationClaim{},
			indexingapp.ErrIndexMetaInitializationMismatch
	}
	row, err := sqlcgen.New(r.pool).ClaimExactIndexMetaInitialization(
		ctx,
		sqlcgen.ClaimExactIndexMetaInitializationParams{
			ClaimToken:             claimToken,
			ClaimLeaseMicroseconds: lease.Microseconds(),
			ExecutionID:            initialization.ExecutionID,
			Generation:             int64(initialization.Generation),
			IndexGeneration:        int64(initialization.IndexGeneration),
			IndexMetaID:            initialization.MetaID,
			IndexMetaCorrelationID: initialization.CorrelationID,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return indexingapp.IndexMetaInitializationClaim{},
			indexingapp.ErrIndexMetaInitializationClaimUnavailable
	}
	if err != nil {
		return indexingapp.IndexMetaInitializationClaim{},
			fmt.Errorf("claim exact index metadata initialization: %w", err)
	}
	return indexMetaInitializationClaim(
		row.ExecutionID,
		row.Generation,
		row.IndexMetaInitializationAttemptCount,
		claimToken,
		row.IndexMetaInitializationClaimExpiresAt,
	)
}

func (r *IndexIngestJobsRepository) QuarantineExpiredTerminalIndexMetaInitializations(
	ctx context.Context,
	limit int,
) (int, error) {
	if ctx == nil || limit <= 0 ||
		limit > executionapp.MaxOutboxPublisherBatchSize {
		return 0, indexingapp.ErrIndexMetaInitializationMismatch
	}
	count, err := sqlcgen.New(r.pool).
		QuarantineExpiredTerminalIndexMetaInitializations(
			ctx,
			int32(limit),
		)
	if err != nil {
		return 0, fmt.Errorf(
			"quarantine expired terminal index metadata initializations: %w",
			err,
		)
	}
	if count < 0 || count > int64(limit) {
		return 0, errors.New(
			"database returned invalid expired terminal initialization count",
		)
	}
	return int(count), nil
}

func (r *IndexIngestJobsRepository) ClaimPendingIndexMetaInitializations(
	ctx context.Context,
	claimToken string,
	limit int,
	lease time.Duration,
) ([]indexingapp.IndexMetaInitializationClaim, error) {
	if ctx == nil || !validIndexMetaInitializationClaimToken(claimToken) ||
		limit <= 0 || limit > executionapp.MaxOutboxPublisherBatchSize ||
		lease <= 0 || lease > 10*time.Minute {
		return nil, indexingapp.ErrIndexMetaInitializationMismatch
	}
	rows, err := sqlcgen.New(r.pool).ClaimPendingIndexMetaInitializations(
		ctx,
		sqlcgen.ClaimPendingIndexMetaInitializationsParams{
			ClaimLimit:             int32(limit),
			ClaimToken:             claimToken,
			ClaimLeaseMicroseconds: lease.Microseconds(),
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"claim pending index metadata initializations: %w",
			err,
		)
	}
	claims := make([]indexingapp.IndexMetaInitializationClaim, 0, len(rows))
	for _, row := range rows {
		claim, err := indexMetaInitializationClaim(
			row.ExecutionID,
			row.Generation,
			row.IndexMetaInitializationAttemptCount,
			claimToken,
			row.IndexMetaInitializationClaimExpiresAt,
		)
		if err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, nil
}

func (r *IndexIngestJobsRepository) LoadIndexMetaInitializationWork(
	ctx context.Context,
	claim indexingapp.IndexMetaInitializationClaim,
) (indexingapp.IndexMetaInitializationWork, error) {
	if ctx == nil || claim.Validate() != nil || claim.Generation > math.MaxInt64 {
		return indexingapp.IndexMetaInitializationWork{},
			indexingapp.ErrIndexMetaInitializationMismatch
	}
	row, err := sqlcgen.New(r.pool).LoadIndexMetaInitializationWork(
		ctx,
		sqlcgen.LoadIndexMetaInitializationWorkParams{
			ExecutionID: claim.ExecutionID,
			Generation:  int64(claim.Generation),
			ClaimToken:  claim.ClaimToken,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return indexingapp.IndexMetaInitializationWork{},
			indexingapp.ErrIndexMetaInitializationClaimUnavailable
	}
	if err != nil {
		return indexingapp.IndexMetaInitializationWork{},
			fmt.Errorf("load index metadata initialization work: %w", err)
	}
	if row.IndexGeneration <= 0 || row.IndexMetaID == nil ||
		row.IndexMetaCorrelationID == nil || !row.AdmittedAt.Valid ||
		!row.Deadline.Valid || row.AdmittedAt.Time.IsZero() ||
		!row.Deadline.Time.After(row.AdmittedAt.Time) {
		return indexingapp.IndexMetaInitializationWork{},
			indexingapp.ErrIndexMetaInitializationMismatch
	}
	resourceProject := strconv.FormatInt(int64(row.ResourceProjectID), 10)
	projectionProject := strconv.FormatInt(int64(row.ProjectionProjectID), 10)
	work := indexingapp.IndexMetaInitializationWork{
		Claim: claim,
		Request: indexingapp.SubmitRequest{
			Identity: executionapp.AdmissionIdentity{
				TenantID:            row.TenantID,
				ResourceProjectID:   resourceProject,
				ProjectionProjectID: projectionProject,
				ActorID:             row.ActorID,
			},
			CorrelationID:   *row.IndexMetaCorrelationID,
			ClientStreamID:  valueOrEmpty(row.ClientStreamID),
			ClientMessageID: valueOrEmpty(row.ClientMessageID),
			SIOEvent:        valueOrEmpty(row.SioEvent),
			ToolkitID:       row.ToolkitID,
			Initiator:       executiondomain.IndexIngestInitiator(row.Initiator),
			Inputs: indexingapp.AuthoritativeInputs{
				ToolkitConfiguration: append(
					json.RawMessage(nil),
					row.ToolkitConfiguration...,
				),
				ToolParameters: append(
					json.RawMessage(nil),
					row.ToolParameters...,
				),
			},
		},
		Outcome: indexingapp.AdmissionOutcome{
			AdmissionOutcome: executionapp.AdmissionOutcome{
				ExecutionID: claim.ExecutionID,
				CommandID:   row.CommandID,
				Created:     false,
				AdmittedAt:  row.AdmittedAt.Time.UTC(),
				Deadline:    row.Deadline.Time.UTC(),
			},
			Generation:             claim.Generation,
			IndexGeneration:        uint64(row.IndexGeneration),
			IndexMetaID:            *row.IndexMetaID,
			IndexMetaCorrelationID: *row.IndexMetaCorrelationID,
		},
	}
	if err := work.Validate(); err != nil {
		return indexingapp.IndexMetaInitializationWork{}, err
	}
	return work, nil
}

func (r *IndexIngestJobsRepository) ResolveIndexMetaInitialization(
	ctx context.Context,
	claim indexingapp.IndexMetaInitializationClaim,
) (time.Time, error) {
	if ctx == nil || claim.Validate() != nil || claim.Generation > math.MaxInt64 {
		return time.Time{}, indexingapp.ErrIndexMetaInitializationMismatch
	}
	initializedAt, err := sqlcgen.New(r.pool).ResolveIndexMetaInitialization(
		ctx,
		sqlcgen.ResolveIndexMetaInitializationParams{
			ExecutionID: claim.ExecutionID,
			Generation:  int64(claim.Generation),
			ClaimToken:  claim.ClaimToken,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, indexingapp.ErrIndexMetaInitializationMismatch
	}
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"resolve index metadata initialization: %w",
			err,
		)
	}
	if !initializedAt.Valid || initializedAt.Time.IsZero() {
		return time.Time{}, errors.New(
			"database returned invalid index metadata initialization time",
		)
	}
	return initializedAt.Time.UTC(), nil
}

func (r *IndexIngestJobsRepository) ReleaseIndexMetaInitialization(
	ctx context.Context,
	claim indexingapp.IndexMetaInitializationClaim,
	errorCode string,
) error {
	if ctx == nil || claim.Validate() != nil ||
		claim.Generation > math.MaxInt64 ||
		!validIndexMetaInitializationErrorCode(errorCode) {
		return indexingapp.ErrIndexMetaInitializationMismatch
	}
	affected, err := sqlcgen.New(r.pool).ReleaseIndexMetaInitialization(
		ctx,
		sqlcgen.ReleaseIndexMetaInitializationParams{
			LastErrorCode: errorCode,
			ExecutionID:   claim.ExecutionID,
			Generation:    int64(claim.Generation),
			ClaimToken:    claim.ClaimToken,
		},
	)
	if err != nil {
		return fmt.Errorf("release index metadata initialization: %w", err)
	}
	if affected != 1 {
		return indexingapp.ErrIndexMetaInitializationMismatch
	}
	return nil
}

func (r *IndexIngestJobsRepository) QuarantineIndexMetaInitialization(
	ctx context.Context,
	claim indexingapp.IndexMetaInitializationClaim,
	errorCode string,
) error {
	if ctx == nil || claim.Validate() != nil ||
		claim.Generation > math.MaxInt64 ||
		!validIndexMetaInitializationErrorCode(errorCode) {
		return indexingapp.ErrIndexMetaInitializationMismatch
	}
	executionID, err := sqlcgen.New(r.pool).QuarantineIndexMetaInitialization(
		ctx,
		sqlcgen.QuarantineIndexMetaInitializationParams{
			LastErrorCode: errorCode,
			ExecutionID:   claim.ExecutionID,
			Generation:    int64(claim.Generation),
			ClaimToken:    claim.ClaimToken,
			FailureEventBytes: append(
				[]byte(nil),
				indexMetaInitializationFailureEventBytes...,
			),
			FailureEventDigest: append(
				[]byte(nil),
				indexMetaInitializationFailureEventDigest[:]...,
			),
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return indexingapp.ErrIndexMetaInitializationMismatch
	}
	if err != nil {
		return fmt.Errorf("quarantine index metadata initialization: %w", err)
	}
	if executionID != claim.ExecutionID {
		return indexingapp.ErrIndexMetaInitializationMismatch
	}
	return nil
}

func indexMetaInitializationClaim(
	executionID string,
	generation int64,
	attempt int32,
	token string,
	expiresAt pgtype.Timestamptz,
) (indexingapp.IndexMetaInitializationClaim, error) {
	if generation <= 0 || attempt <= 0 ||
		!expiresAt.Valid || expiresAt.Time.IsZero() {
		return indexingapp.IndexMetaInitializationClaim{},
			indexingapp.ErrIndexMetaInitializationMismatch
	}
	claim := indexingapp.IndexMetaInitializationClaim{
		ExecutionID: executionID,
		Generation:  uint64(generation),
		ClaimToken:  token,
		Attempt:     uint32(attempt),
		ExpiresAt:   expiresAt.Time.UTC(),
	}
	if err := claim.Validate(); err != nil {
		return indexingapp.IndexMetaInitializationClaim{}, err
	}
	return claim, nil
}

func validIndexMetaInitializationClaimToken(value string) bool {
	return value != "" && len(value) <= 256 &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func validIndexMetaInitializationErrorCode(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') &&
			character != '_' {
			return false
		}
	}
	return true
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func loadIndexAdmission(ctx context.Context, queries *sqlcgen.Queries, scope, key string) (indexingapp.AdmissionOutcome, runtimedomain.Digest, error) {
	row, err := queries.GetRuntimeAdmissionByIdempotency(ctx, sqlcgen.GetRuntimeAdmissionByIdempotencyParams{
		IdempotencyScope: scope,
		IdempotencyKey:   key,
	})
	if err != nil {
		return indexingapp.AdmissionOutcome{}, runtimedomain.Digest{}, err
	}
	digest, err := storedDigest(row.RequestDigest)
	if err != nil {
		return indexingapp.AdmissionOutcome{}, runtimedomain.Digest{}, fmt.Errorf("invalid stored index request digest: %w", err)
	}
	if !row.AdmittedAt.Valid || !row.Deadline.Valid || row.AdmittedAt.Time.IsZero() || !row.Deadline.Time.After(row.AdmittedAt.Time) {
		return indexingapp.AdmissionOutcome{}, runtimedomain.Digest{}, errors.New("stored index admission timing is invalid")
	}
	if row.Generation <= 0 || row.IndexGeneration <= 0 ||
		row.IndexMetaID == nil || *row.IndexMetaID == "" ||
		row.IndexMetaCorrelationID == nil || *row.IndexMetaCorrelationID == "" {
		return indexingapp.AdmissionOutcome{}, runtimedomain.Digest{}, errors.New("stored index metadata identity is invalid")
	}
	var initializedAt *time.Time
	if row.IndexMetaInitializedAt.Valid {
		value := row.IndexMetaInitializedAt.Time.UTC()
		if value.IsZero() {
			return indexingapp.AdmissionOutcome{}, runtimedomain.Digest{}, errors.New("stored index metadata initialization time is invalid")
		}
		initializedAt = &value
	}
	return indexingapp.AdmissionOutcome{
		AdmissionOutcome: executionapp.AdmissionOutcome{
			ExecutionID: row.ExecutionID,
			CommandID:   row.CommandID,
			Created:     false,
			AdmittedAt:  row.AdmittedAt.Time.UTC(),
			Deadline:    row.Deadline.Time.UTC(),
		},
		Generation:             uint64(row.Generation),
		IndexGeneration:        uint64(row.IndexGeneration),
		IndexMetaID:            *row.IndexMetaID,
		IndexMetaCorrelationID: *row.IndexMetaCorrelationID,
		IndexMetaInitializedAt: initializedAt,
	}, digest, nil
}

func decodeAdmissionTiming(row sqlcgen.LoadRuntimeAdmissionTimingRow, deadlineTTL time.Duration) (admissionTiming, error) {
	if !row.AdmittedAt.Valid || !row.Deadline.Valid {
		return admissionTiming{}, errors.New("database returned invalid index admission timing")
	}
	admittedAt := row.AdmittedAt.Time.UTC()
	deadline := row.Deadline.Time.UTC()
	if admittedAt.IsZero() || !deadline.After(admittedAt) || deadline.Sub(admittedAt) != deadlineTTL {
		return admissionTiming{}, errors.New("database returned invalid index admission timing")
	}
	return admissionTiming{AdmittedAt: admittedAt, Deadline: deadline}, nil
}

func insertRuntimeInputBundle(ctx context.Context, queries *sqlcgen.Queries, resourceProjectID int32, record executiondomain.Admission, admittedAt time.Time) error {
	bundle := record.InputBundle
	if err := queries.InsertRuntimeInputBundle(ctx, sqlcgen.InsertRuntimeInputBundleParams{
		InputBundleID:     bundle.ID,
		ImmutableVersion:  bundle.Version,
		MediaType:         bundle.MediaType,
		ResourceProjectID: resourceProjectID,
		ManifestDigest:    append([]byte(nil), bundle.Digest[:]...),
		ManifestSize:      int64(len(bundle.Manifest)),
		ManifestBytes:     append([]byte(nil), bundle.Manifest...),
		CreatedBy:         record.Job.ActorID,
		CreatedAt:         timestamp(admittedAt),
	}); err != nil {
		return fmt.Errorf("insert runtime input bundle: %w", err)
	}
	for _, entry := range bundle.Entries {
		if err := queries.InsertRuntimeInputBundleEntry(ctx, sqlcgen.InsertRuntimeInputBundleEntryParams{
			InputBundleID:         bundle.ID,
			EntryID:               entry.ID,
			EntryVersion:          entry.Version,
			SemanticRole:          entry.SemanticRole,
			MediaType:             entry.MediaType,
			ContentDigest:         append([]byte(nil), entry.ContentDigest[:]...),
			ContentSize:           entry.ContentLength,
			ContentReference:      entry.ContentID,
			Classification:        entry.Classification,
			RequiredGrantAudience: entry.RequiredGrantAudience,
			ContentBytes:          append([]byte(nil), entry.Content...),
		}); err != nil {
			return fmt.Errorf("insert runtime input bundle entry: %w", err)
		}
	}
	return nil
}

func boundedIndexAdmissionStrings(admission indexingapp.Admission) bool {
	record := admission.Record
	binding := admission.Binding
	values := []string{
		record.IdempotencyScope, record.IdempotencyKey,
		record.InputBundle.ID, record.InputBundle.Version, record.InputBundle.MediaType,
		record.Job.ID, record.Job.CommandID, record.Job.TenantID,
		record.Job.ResourceProjectID, record.Job.ProjectionProjectID,
		record.Job.ActorID, record.Job.CapabilityID, record.Outbox.ID,
		binding.ToolkitConfigurationEntryID, binding.ToolParametersEntryID,
		binding.IndexMetaID,
		binding.IndexName, string(binding.Initiator),
	}
	for _, entry := range record.InputBundle.Entries {
		values = append(values, entry.ID, entry.Version, entry.SemanticRole, entry.ContentID, entry.MediaType, entry.Classification, entry.RequiredGrantAudience)
	}
	for _, value := range values {
		if value == "" || len(value) > 256 || strings.ContainsRune(value, '\x00') {
			return false
		}
	}
	for _, optional := range []string{binding.LLMModelEntryID, binding.LLMConfigurationEntryID, binding.MCPTokensEntryID, binding.SIOEvent} {
		if len(optional) > 256 || strings.ContainsRune(optional, '\x00') {
			return false
		}
	}
	return len(binding.ClientStreamID) <= executiondomain.MaxIndexMetaCorrelationBytes &&
		len(binding.ClientMessageID) <= executiondomain.MaxIndexMetaCorrelationBytes &&
		!strings.ContainsAny(binding.ClientStreamID, "\x00\r\n") &&
		!strings.ContainsAny(binding.ClientMessageID, "\x00\r\n") &&
		len(binding.IndexMetaCorrelationID) <= executiondomain.MaxIndexMetaCorrelationBytes &&
		!strings.ContainsRune(binding.IndexMetaCorrelationID, '\x00')
}

func validateInputManifest(bundle executiondomain.InputBundle) error {
	var manifest runtimev1.ExecutionInputBundleV1
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(bundle.Manifest, &manifest); err != nil {
		return fmt.Errorf("%w: input manifest is not decodable", executiondomain.ErrInvalidInputBundle)
	}
	if manifest.GetInputBundleId() != bundle.ID || manifest.GetImmutableVersion() != bundle.Version || len(manifest.GetEntries()) != len(bundle.Entries) {
		return fmt.Errorf("%w: input manifest identity mismatch", executiondomain.ErrInvalidInputBundle)
	}
	for index, wireEntry := range manifest.GetEntries() {
		entry := bundle.Entries[index]
		content := wireEntry.GetContent()
		if content == nil || wireEntry.GetEntryId() != entry.ID || wireEntry.GetImmutableVersion() != entry.Version || wireEntry.GetSemanticRole() != entry.SemanticRole || content.GetContentId() != entry.ContentID || content.GetImmutableVersion() != entry.Version || content.GetMediaType() != entry.MediaType || content.GetByteLength() != uint64(entry.ContentLength) || content.GetClassification() != entry.Classification || content.GetRequiredGrantAudience() != entry.RequiredGrantAudience || content.GetDigest().GetAlgorithm() != runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 || len(content.GetDigest().GetValue()) != len(entry.ContentDigest) || string(content.GetDigest().GetValue()) != string(entry.ContentDigest[:]) {
			return fmt.Errorf("%w: input manifest entry mismatch", executiondomain.ErrInvalidInputBundle)
		}
	}
	return nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func timestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

var _ indexingapp.AtomicAdmissionStore = (*IndexIngestJobsRepository)(nil)
var _ indexingapp.IndexMetaInitializationStore = (*IndexIngestJobsRepository)(nil)
