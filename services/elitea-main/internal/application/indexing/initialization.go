package indexing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
)

const (
	maxIndexMetaInitializationClaimTokenBytes = 256
)

var (
	ErrIndexMetaInitializationClaimUnavailable = fmt.Errorf(
		"%w: claim is unavailable",
		ErrIndexMetaInitializationMismatch,
	)
	ErrIndexMetaInitializationAmbiguous = errors.New("active index target is ambiguous")
)

// ActiveIndexConflictError is emitted only after an already-authorized start
// resolves one exact active execution for the same tenant/project/projection,
// toolkit, index, and capability. It is the only error allowed to expose a
// task ID through the current 409 response.
type ActiveIndexConflictError struct {
	TaskID string
}

func (e *ActiveIndexConflictError) Error() string {
	return ErrCurrentIndexMetaConflict.Error()
}

func (e *ActiveIndexConflictError) Unwrap() error {
	return ErrCurrentIndexMetaConflict
}

func NewActiveIndexConflictError(taskID string) error {
	if !validOptionalText(taskID, maxIndexAdmissionStringBytes) || taskID == "" {
		return ErrIndexMetaInitializationAmbiguous
	}
	return &ActiveIndexConflictError{TaskID: taskID}
}

// IndexMetaMaterializer owns the idempotent index registry write for the
// exact admission identity. It must reproduce the approved current initial
// metadata contract and return only after that row is durably visible. It does
// not open the command dispatch gate. Implementations must propagate and
// honor context cancellation so a call cannot outlive its initialization
// claim lease.
type IndexMetaMaterializer interface {
	MaterializeInitialIndexMeta(context.Context, SubmitRequest, AdmissionOutcome) error
}

type IndexMetaInitializationClaim struct {
	ExecutionID string
	Generation  uint64
	ClaimToken  string
	Attempt     uint32
	ExpiresAt   time.Time
}

func (c IndexMetaInitializationClaim) Validate() error {
	if !validOptionalText(c.ExecutionID, maxIndexAdmissionStringBytes) ||
		c.ExecutionID == "" || c.Generation == 0 ||
		!validOptionalText(
			c.ClaimToken,
			maxIndexMetaInitializationClaimTokenBytes,
		) ||
		c.ClaimToken == "" || c.Attempt == 0 || c.ExpiresAt.IsZero() {
		return ErrIndexMetaInitializationMismatch
	}
	return nil
}

// IndexMetaInitializationWork is reconstructed from the immutable admitted
// input bundle. It never contains a fresh configuration lookup or a redeemed
// secret. Redemption is scoped to the materializer call.
type IndexMetaInitializationWork struct {
	Claim   IndexMetaInitializationClaim
	Request SubmitRequest
	Outcome AdmissionOutcome
}

func (w IndexMetaInitializationWork) Validate() error {
	if err := w.Claim.Validate(); err != nil ||
		w.Request.ToolkitID <= 0 ||
		w.Request.Inputs.validate() != nil ||
		w.Outcome.ExecutionID != w.Claim.ExecutionID ||
		w.Outcome.Generation != w.Claim.Generation ||
		w.Outcome.IndexGeneration == 0 ||
		w.Outcome.IndexMetaID == "" ||
		w.Outcome.IndexMetaCorrelationID == "" ||
		w.Outcome.IndexMetaCorrelationID != w.Request.CorrelationID ||
		w.Outcome.IndexMetaInitializedAt != nil ||
		w.Outcome.AdmittedAt.IsZero() ||
		!w.Outcome.Deadline.After(w.Outcome.AdmittedAt) {
		return ErrIndexMetaInitializationMismatch
	}
	return nil
}

type IndexMetaInitializationRepository interface {
	QuarantineExpiredTerminalIndexMetaInitializations(
		context.Context,
		int,
	) (int, error)
	ClaimExactIndexMetaInitialization(
		context.Context,
		IndexMetaInitialization,
		string,
		time.Duration,
	) (IndexMetaInitializationClaim, error)
	ClaimPendingIndexMetaInitializations(
		context.Context,
		string,
		int,
		time.Duration,
	) ([]IndexMetaInitializationClaim, error)
	LoadIndexMetaInitializationWork(
		context.Context,
		IndexMetaInitializationClaim,
	) (IndexMetaInitializationWork, error)
	ResolveIndexMetaInitialization(
		context.Context,
		IndexMetaInitializationClaim,
	) (time.Time, error)
	ReleaseIndexMetaInitialization(
		context.Context,
		IndexMetaInitializationClaim,
		string,
	) error
	QuarantineIndexMetaInitialization(
		context.Context,
		IndexMetaInitializationClaim,
		string,
	) error
}

type IndexMetaInitializationReconcilerConfig struct {
	PollInterval  time.Duration
	ClaimLease    time.Duration
	BatchSize     int
	MaxConcurrent int
	ReportFailure func(error)
}

func (c IndexMetaInitializationReconcilerConfig) validate() error {
	if c.PollInterval <= 0 || c.PollInterval > time.Minute ||
		c.ClaimLease < time.Millisecond || c.ClaimLease > 10*time.Minute ||
		c.BatchSize <= 0 ||
		c.BatchSize > executionapp.MaxOutboxPublisherBatchSize ||
		c.MaxConcurrent <= 0 || c.MaxConcurrent > c.BatchSize ||
		c.ReportFailure == nil {
		return errors.New("invalid index metadata initialization reconciler config")
	}
	return nil
}

// DurableIndexMetaInitializer is the single immediate/recovery implementation.
// PostgreSQL leases allow multiple Main replicas to run it safely, while the
// bounded worker set prevents registry/configuration fan-out from exceeding the
// pool-derived composition limit.
type DurableIndexMetaInitializer struct {
	store        IndexMetaInitializationRepository
	materializer IndexMetaMaterializer
	newClaimID   executionapp.IDGenerator
	config       IndexMetaInitializationReconcilerConfig
	now          func() time.Time
}

func NewDurableIndexMetaInitializer(
	store IndexMetaInitializationRepository,
	materializer IndexMetaMaterializer,
	newClaimID executionapp.IDGenerator,
	config IndexMetaInitializationReconcilerConfig,
) (*DurableIndexMetaInitializer, error) {
	if store == nil || materializer == nil || newClaimID == nil {
		return nil, errors.New("durable index metadata initialization dependencies are required")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &DurableIndexMetaInitializer{
		store:        store,
		materializer: materializer,
		newClaimID:   newClaimID,
		config:       config,
		now:          time.Now,
	}, nil
}

func (i *DurableIndexMetaInitializer) Initialize(
	ctx context.Context,
	outcome AdmissionOutcome,
) (time.Time, error) {
	if ctx == nil || outcome.ExecutionID == "" || outcome.Generation == 0 ||
		outcome.IndexGeneration == 0 || outcome.IndexMetaID == "" ||
		outcome.IndexMetaCorrelationID == "" {
		return time.Time{}, ErrIndexMetaInitializationMismatch
	}
	token, err := i.claimID()
	if err != nil {
		return time.Time{}, err
	}
	claim, err := i.store.ClaimExactIndexMetaInitialization(
		ctx,
		IndexMetaInitialization{
			ExecutionID:     outcome.ExecutionID,
			Generation:      outcome.Generation,
			IndexGeneration: outcome.IndexGeneration,
			MetaID:          outcome.IndexMetaID,
			CorrelationID:   outcome.IndexMetaCorrelationID,
		},
		token,
		i.config.ClaimLease,
	)
	if err != nil {
		return time.Time{}, err
	}
	return i.apply(ctx, claim)
}

func (i *DurableIndexMetaInitializer) Reconcile(ctx context.Context) (int, error) {
	if ctx == nil {
		return 0, ErrIndexMetaInitializationMismatch
	}
	quarantined, err :=
		i.store.QuarantineExpiredTerminalIndexMetaInitializations(
			ctx,
			i.config.BatchSize,
		)
	if err != nil || quarantined == i.config.BatchSize {
		return quarantined, err
	}
	token, err := i.claimID()
	if err != nil {
		return quarantined, err
	}
	claims, err := i.store.ClaimPendingIndexMetaInitializations(
		ctx,
		token,
		i.config.BatchSize-quarantined,
		i.config.ClaimLease,
	)
	if err != nil || len(claims) == 0 {
		return quarantined, err
	}

	errorsByClaim := make([]error, len(claims))
	jobs := make(chan int)
	var workers sync.WaitGroup
	workerCount := min(i.config.MaxConcurrent, len(claims))
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				_, errorsByClaim[index] = i.apply(ctx, claims[index])
			}
		}()
	}
send:
	for index := range claims {
		select {
		case jobs <- index:
		case <-ctx.Done():
			break send
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var cycleErrors []error
	for index, itemErr := range errorsByClaim {
		if itemErr == nil {
			continue
		}
		wrapped := fmt.Errorf(
			"initialize index metadata %q generation %d: %w",
			claims[index].ExecutionID,
			claims[index].Generation,
			itemErr,
		)
		cycleErrors = append(cycleErrors, wrapped)
	}
	return quarantined + len(claims), errors.Join(cycleErrors...)
}

func (i *DurableIndexMetaInitializer) Run(ctx context.Context) error {
	if ctx == nil {
		return ErrIndexMetaInitializationMismatch
	}
	ticker := time.NewTicker(i.config.PollInterval)
	defer ticker.Stop()
	for {
		if _, err := i.Reconcile(ctx); err != nil && ctx.Err() == nil {
			i.config.ReportFailure(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (i *DurableIndexMetaInitializer) apply(
	ctx context.Context,
	claim IndexMetaInitializationClaim,
) (time.Time, error) {
	work, err := i.store.LoadIndexMetaInitializationWork(ctx, claim)
	if err != nil {
		return time.Time{}, i.finishFailure(ctx, claim, err)
	}
	if err := work.Validate(); err != nil {
		return time.Time{}, i.finishFailure(ctx, claim, err)
	}
	if !work.Outcome.Deadline.After(i.now().UTC()) {
		return time.Time{}, i.quarantine(
			ctx,
			claim,
			"INITIALIZATION_DEADLINE_EXCEEDED",
			context.DeadlineExceeded,
		)
	}

	request := work.Request
	request.Inputs = work.Request.Inputs.Clone()
	// Leave enough of the database-authoritative claim lease to release the
	// attempt or commit its marker before another replica can reclaim it.
	materializationDeadline, ok := i.materializationDeadline(
		claim,
		work.Outcome.Deadline,
	)
	if !ok {
		return time.Time{}, i.finishFailure(
			ctx,
			claim,
			ErrIndexMetaInitializationClaimUnavailable,
		)
	}
	materializationCtx, cancel := context.WithTimeout(
		ctx,
		materializationDeadline.Sub(i.now().UTC()),
	)
	defer cancel()
	if err := i.materializer.MaterializeInitialIndexMeta(
		materializationCtx,
		request,
		cloneAdmissionOutcome(work.Outcome),
	); err != nil {
		if errors.Is(err, context.DeadlineExceeded) &&
			!work.Outcome.Deadline.After(i.now().UTC()) {
			return time.Time{}, i.quarantine(
				ctx,
				claim,
				"INITIALIZATION_DEADLINE_EXCEEDED",
				context.DeadlineExceeded,
			)
		}
		return time.Time{}, i.finishFailure(ctx, claim, err)
	}

	// The external commit may already be durable. Complete the marker with a
	// bounded context that is not invalidated by an HTTP disconnect.
	finishCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		min(i.config.ClaimLease/2, 5*time.Second),
	)
	defer cancel()
	initializedAt, err := i.store.ResolveIndexMetaInitialization(
		finishCtx,
		claim,
	)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"resolve index metadata initialization after external commit: %w",
			err,
		)
	}
	if initializedAt.IsZero() {
		return time.Time{}, errors.New(
			"index metadata initialization transition returned an invalid timestamp",
		)
	}
	return initializedAt, nil
}

func (i *DurableIndexMetaInitializer) materializationDeadline(
	claim IndexMetaInitializationClaim,
	jobDeadline time.Time,
) (time.Time, bool) {
	reserve := min(i.config.ClaimLease/4, 5*time.Second)
	if reserve <= 0 {
		return time.Time{}, false
	}
	leaseDeadline := claim.ExpiresAt.UTC().Add(-reserve)
	deadline := jobDeadline.UTC()
	if leaseDeadline.Before(deadline) {
		deadline = leaseDeadline
	}
	return deadline, deadline.After(i.now().UTC())
}

func (i *DurableIndexMetaInitializer) finishFailure(
	ctx context.Context,
	claim IndexMetaInitializationClaim,
	cause error,
) error {
	code, permanent := indexMetaInitializationErrorCode(cause)
	if permanent {
		return i.quarantine(ctx, claim, code, cause)
	}
	finishCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		min(i.config.ClaimLease/2, 5*time.Second),
	)
	defer cancel()
	if err := i.store.ReleaseIndexMetaInitialization(
		finishCtx,
		claim,
		code,
	); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (i *DurableIndexMetaInitializer) quarantine(
	ctx context.Context,
	claim IndexMetaInitializationClaim,
	code string,
	cause error,
) error {
	finishCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		min(i.config.ClaimLease/2, 5*time.Second),
	)
	defer cancel()
	if err := i.store.QuarantineIndexMetaInitialization(
		finishCtx,
		claim,
		code,
	); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (i *DurableIndexMetaInitializer) claimID() (string, error) {
	value, err := i.newClaimID()
	if err != nil {
		return "", fmt.Errorf("generate index metadata initialization claim: %w", err)
	}
	if !validOptionalText(value, maxIndexMetaInitializationClaimTokenBytes) ||
		value == "" {
		return "", errors.New(
			"index metadata initialization claim generator returned an invalid ID",
		)
	}
	return value, nil
}

func indexMetaInitializationErrorCode(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrIndexMetaInitializationClaimUnavailable):
		return "INITIALIZATION_CLAIM_LOST", false
	case errors.Is(err, ErrIndexMetaInitializationMismatch),
		errors.Is(err, ErrCurrentIndexMetaInitializationInvalid):
		return "INITIALIZATION_INTENT_INVALID", true
	case errors.Is(err, ErrCurrentIndexMetaConflict):
		return "INITIALIZATION_EXTERNAL_CONFLICT", true
	case errors.Is(err, ErrCurrentIndexMetaSuperseded):
		return "INITIALIZATION_EXTERNAL_SUPERSEDED", true
	case errors.Is(err, ErrCurrentIndexMetaTargetUnavailable):
		return "INITIALIZATION_TARGET_INVALID", true
	case errors.Is(err, context.Canceled):
		return "INITIALIZATION_CANCELLED", false
	case errors.Is(err, context.DeadlineExceeded):
		return "INITIALIZATION_ATTEMPT_DEADLINE", false
	default:
		return "INITIALIZATION_DEPENDENCY_UNAVAILABLE", false
	}
}

// InitializingAdmissionSubmitter composes admission with the leased immediate
// attempt. If the request dies after admission, the background reconciler
// resumes the exact immutable intent.
type InitializingAdmissionSubmitter struct {
	admissions  IndexAdmissionSubmitter
	initializer *DurableIndexMetaInitializer
}

func NewInitializingAdmissionSubmitter(
	admissions IndexAdmissionSubmitter,
	initializer *DurableIndexMetaInitializer,
) (*InitializingAdmissionSubmitter, error) {
	if admissions == nil || initializer == nil {
		return nil, errors.New("index metadata initialization dependencies are required")
	}
	return &InitializingAdmissionSubmitter{
		admissions:  admissions,
		initializer: initializer,
	}, nil
}

func (s *InitializingAdmissionSubmitter) Submit(
	ctx context.Context,
	request SubmitRequest,
) (AdmissionOutcome, error) {
	outcome, err := s.admissions.Submit(ctx, request)
	if err != nil {
		return AdmissionOutcome{}, err
	}
	if outcome.IndexMetaInitializedAt != nil {
		if outcome.IndexMetaInitializedAt.IsZero() {
			return AdmissionOutcome{}, errors.New(
				"index metadata initialization timestamp is invalid",
			)
		}
		return outcome, nil
	}
	initializedAt, err := s.initializer.Initialize(ctx, outcome)
	if err != nil {
		return AdmissionOutcome{}, fmt.Errorf(
			"initialize admitted index metadata: %w",
			err,
		)
	}
	outcome.IndexMetaInitializedAt = &initializedAt
	return outcome, nil
}

func cloneAdmissionOutcome(outcome AdmissionOutcome) AdmissionOutcome {
	if outcome.IndexMetaInitializedAt != nil {
		value := *outcome.IndexMetaInitializedAt
		outcome.IndexMetaInitializedAt = &value
	}
	return outcome
}

var _ IndexAdmissionSubmitter = (*InitializingAdmissionSubmitter)(nil)
var _ interface{ Run(context.Context) error } = (*DurableIndexMetaInitializer)(nil)
