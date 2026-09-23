package repos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AgentExecutionJobsRepository persists only execution durability and current
// browser correlation. Conversation content and trace steps remain owned by
// the existing per-project chat schema.
type AgentExecutionJobsRepository struct {
	pool   *pgxpool.Pool
	policy AgentExecutionDispatchPolicy
	// catalogueProjectID is the ONE public project, the same id
	// CurrentAgentStartRepository admits a foreign participant from. The turn
	// INSERT repeats that admission rather than trusting the resolve that
	// preceded it: the two run in separate transactions, and this is the
	// statement that writes the row.
	catalogueProjectID int32
}

func (r *AgentExecutionJobsRepository) ExpectedAgentExecution(
	ctx context.Context,
	executionID string,
	generation uint64,
) (outputapp.ExpectedAgentExecution, error) {
	if executionID == "" || generation == 0 || generation > math.MaxInt64 {
		return outputapp.ExpectedAgentExecution{}, outputapp.ErrInvalidAgentExecutionOutput
	}
	row, err := sqlcgen.New(r.pool).GetExpectedAgentExecutionHeader(
		ctx,
		sqlcgen.GetExpectedAgentExecutionHeaderParams{
			ExecutionID: executionID,
			Generation:  int64(generation),
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return outputapp.ExpectedAgentExecution{}, outputapp.ErrInvalidAgentExecutionOutput
	}
	if err != nil {
		return outputapp.ExpectedAgentExecution{}, fmt.Errorf("load agent output binding: %w", err)
	}
	if row.LimitsRevision != r.policy.LimitsRevision || row.Generation <= 0 ||
		row.ResourceProjectID <= 0 || row.ProjectionProjectID <= 0 {
		return outputapp.ExpectedAgentExecution{}, outputapp.ErrInvalidAgentExecutionOutput
	}
	bundleDigest, err := storedDigest(row.InputBundleDigest)
	if err != nil {
		return outputapp.ExpectedAgentExecution{}, fmt.Errorf("stored agent input bundle digest: %w", err)
	}
	requestDigest, err := storedDigest(row.RequestContentDigest)
	if err != nil {
		return outputapp.ExpectedAgentExecution{}, fmt.Errorf("stored agent request digest: %w", err)
	}
	expected := outputapp.ExpectedAgentExecution{
		TenantID:                  row.TenantID,
		ResourceProjectID:         strconv.FormatInt(int64(row.ResourceProjectID), 10),
		ProjectionProjectID:       strconv.FormatInt(int64(row.ProjectionProjectID), 10),
		CapabilityID:              row.CapabilityID,
		CommandID:                 row.CommandID,
		ExecutionID:               row.ExecutionID,
		Generation:                uint64(row.Generation),
		LogicalOutputID:           "agent-execution:" + row.ExecutionID,
		InputBundleID:             row.InputBundleID,
		InputBundleDigest:         bundleDigest,
		RequestEntryID:            row.RequestEntryID,
		RequestImmutableVersion:   row.RequestImmutableVersion,
		RequestContentDigest:      requestDigest,
		ClientStreamID:            row.ClientStreamID,
		ClientMessageID:           row.ClientMessageID,
		ClientExecutionGeneration: row.ClientExecutionGeneration,
		SIOEvent:                  row.SioEvent,
	}
	if err := expected.Validate(); err != nil {
		return outputapp.ExpectedAgentExecution{}, fmt.Errorf("invalid stored agent output binding: %w", err)
	}
	return expected, nil
}

func NewAgentExecutionJobsRepository(
	pool *pgxpool.Pool,
	policy AgentExecutionDispatchPolicy,
	catalogueProjectID int32,
) (*AgentExecutionJobsRepository, error) {
	if pool == nil {
		return nil, errors.New("agent admission database is required")
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	if catalogueProjectID <= 0 {
		return nil, errors.New("agent admission catalogue project id is required")
	}
	return &AgentExecutionJobsRepository{
		pool:               pool,
		policy:             policy,
		catalogueProjectID: catalogueProjectID,
	}, nil
}

func (r *AgentExecutionJobsRepository) AdmitAgentExecution(
	ctx context.Context,
	admission agentexecutionapp.Admission,
) (executionapp.AdmissionOutcome, error) {
	if err := admission.Record.Validate(); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	if !agentExecutionCapability(admission.Record.Job.CapabilityID) {
		return executionapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
	}
	if err := admission.Binding.Validate(admission.Record.InputBundle); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	if err := validateInputManifest(admission.Record.InputBundle); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	if len(admission.Record.InputBundle.Manifest) > maxStoredInputManifestBytes ||
		len(admission.Record.InputBundle.Entries) != 1 ||
		len(admission.Record.InputBundle.Entries[0].Content) > executiondomain.MaxAgentExecutionInputBytes ||
		!boundedAgentAdmissionStrings(admission) ||
		admission.Record.Job.Generation > math.MaxInt64 ||
		admission.Record.Outbox.Generation > math.MaxInt64 {
		return executionapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
	}
	if admission.CurrentTurn != nil {
		turn := admission.CurrentTurn
		if admission.CurrentAdhocTurn != nil ||
			admission.CurrentRegenerateTurn != nil ||
			admission.CurrentContinueTurn != nil ||
			admission.Record.Job.CapabilityID != executiondomain.AgentApplicationCapability ||
			turn.Validate() != nil ||
			turn.ProjectID > math.MaxInt32 ||
			turn.TargetParticipantID > math.MaxInt32 ||
			turn.ApplicationID > math.MaxInt32 || turn.ApplicationVersionID > math.MaxInt32 ||
			turn.ProjectID != resourceProjectIDUnchecked(admission.Record.Job.ResourceProjectID) ||
			turn.ResponseMessageID != admission.Binding.ClientMessageID ||
			turn.ConversationUUID != admission.Binding.ClientStreamID {
			return executionapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
		}
	}
	if admission.CurrentAdhocTurn != nil {
		turn := admission.CurrentAdhocTurn
		if admission.CurrentRegenerateTurn != nil || admission.CurrentContinueTurn != nil ||
			admission.Record.Job.CapabilityID != executiondomain.AgentAdhocCapability ||
			turn.Validate() != nil || turn.ProjectID > math.MaxInt32 ||
			turn.TargetParticipantID > math.MaxInt32 ||
			turn.ProjectID != resourceProjectIDUnchecked(admission.Record.Job.ResourceProjectID) ||
			turn.ResponseMessageID != admission.Binding.ClientMessageID ||
			turn.ConversationUUID != admission.Binding.ClientStreamID {
			return executionapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
		}
	}
	if admission.CurrentRegenerateTurn != nil {
		turn := admission.CurrentRegenerateTurn
		if admission.CurrentContinueTurn != nil || turn.Validate() != nil || turn.ProjectID > math.MaxInt32 ||
			turn.TargetParticipantID > math.MaxInt32 ||
			turn.ApplicationID > math.MaxInt32 || turn.ApplicationVersionID > math.MaxInt32 ||
			turn.ProjectID != resourceProjectIDUnchecked(admission.Record.Job.ResourceProjectID) ||
			turn.ResponseMessageID != admission.Binding.ClientMessageID ||
			turn.ConversationUUID != admission.Binding.ClientStreamID ||
			turn.ExecutionGeneration != admission.Binding.ClientExecutionGeneration ||
			(turn.Kind == agentexecutionapp.CurrentRegenerationApplication && admission.Record.Job.CapabilityID != executiondomain.AgentApplicationCapability) ||
			(turn.Kind == agentexecutionapp.CurrentRegenerationAdhoc && admission.Record.Job.CapabilityID != executiondomain.AgentAdhocCapability) {
			return executionapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
		}
	}
	if admission.CurrentContinueTurn != nil {
		turn := admission.CurrentContinueTurn
		if turn.Validate() != nil || turn.ProjectID > math.MaxInt32 ||
			turn.TargetParticipantID > math.MaxInt32 ||
			turn.ApplicationID > math.MaxInt32 || turn.ApplicationVersionID > math.MaxInt32 ||
			turn.ProjectID != resourceProjectIDUnchecked(admission.Record.Job.ResourceProjectID) ||
			turn.ResponseMessageID != admission.Binding.ClientMessageID ||
			turn.ConversationUUID != admission.Binding.ClientStreamID ||
			turn.ExecutionGeneration != admission.Binding.ClientExecutionGeneration ||
			admission.Binding.SIOEvent != "chat_continue_predict" ||
			(turn.Kind == agentexecutionapp.CurrentRegenerationApplication && admission.Record.Job.CapabilityID != executiondomain.AgentApplicationCapability) ||
			(turn.Kind == agentexecutionapp.CurrentRegenerationAdhoc && admission.Record.Job.CapabilityID != executiondomain.AgentAdhocCapability) {
			return executionapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
		}
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
	existing, digest, err := loadAgentAdmission(
		ctx,
		queries,
		admission.Record.IdempotencyScope,
		admission.Record.IdempotencyKey,
	)
	switch {
	case err == nil:
		if digest != admission.Record.RequestDigest {
			return executionapp.AdmissionOutcome{}, executionapp.ErrIdempotencyConflict
		}
		return existing, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return executionapp.AdmissionOutcome{}, fmt.Errorf("load agent idempotency binding: %w", err)
	}

	// Phase 1 reserves the durable slot behind the capability policy row lock
	// in a short, synchronous-commit-off transaction. Phase 2 then writes the
	// heavy durable rows without holding that lock (issue 965).
	if err := r.reserveAgentAdmission(ctx, admission); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	return r.materializeAgentAdmission(ctx, admission, resourceProject, projectionProject)
}

// reserveAgentAdmission claims the admission's durable slot. It runs in its own
// short transaction that disables synchronous_commit for the commit, and the
// agent_admission_reservations BEFORE INSERT trigger serializes concurrent
// starts on the capability policy row while re-computing the live cap. A replay
// (an identical reservation already present) is not an error: the insert's
// ON CONFLICT DO NOTHING drops the row and materialize reconciles against the
// durable job.
func (r *AgentExecutionJobsRepository) reserveAgentAdmission(
	ctx context.Context,
	admission agentexecutionapp.Admission,
) error {
	capabilityID := admission.Record.Job.CapabilityID
	err := r.reserveAgentAdmissionOnce(ctx, admission)
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "E9650" {
		return r.reserveAgentAdmissionError(capabilityID, err)
	}
	// E9650 means the capability policy row is missing. The guard trigger raised
	// it inside the reserve transaction, which is now aborted, so the row cannot
	// be created there. It is created out-of-band as an idempotent upsert, then
	// the reserve is retried in a fresh transaction where the mismatch (E9651)
	// and cap (E9652) checks apply as usual. This runs only before the first
	// start for a capability, not on the steady-state path (issue 965).
	if ensureErr := sqlcgen.New(r.pool).EnsureRuntimeAdmissionPolicy(
		ctx,
		sqlcgen.EnsureRuntimeAdmissionPolicyParams{
			CapabilityID:   capabilityID,
			MaxOutstanding: r.policy.MaxOutstanding,
		},
	); ensureErr != nil {
		return fmt.Errorf("ensure agent admission policy: %w", ensureErr)
	}
	if err = r.reserveAgentAdmissionOnce(ctx, admission); err != nil {
		return r.reserveAgentAdmissionError(capabilityID, err)
	}
	return nil
}

// reserveAgentAdmissionOnce runs the reservation in a single short transaction
// with synchronous_commit off and returns the raw reserve error so the caller
// can distinguish the policy-missing (E9650) case. A successful insert or a
// replay (an identical reservation already present) returns nil.
func (r *AgentExecutionJobsRepository) reserveAgentAdmissionOnce(
	ctx context.Context,
	admission agentexecutionapp.Admission,
) error {
	tx, err := r.pool.BeginTx(
		ctx,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
	)
	if err != nil {
		return fmt.Errorf("begin agent admission reserve: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	// The slot marker is durable only to the extent the reaper and the live cap
	// re-computation need it; skipping the commit fsync is what keeps this
	// transaction off the start-path critical section (issue 965).
	if _, err := tx.Exec(ctx, "SET LOCAL synchronous_commit = off"); err != nil {
		return fmt.Errorf("set agent reserve synchronous commit: %w", err)
	}
	_, err = sqlcgen.New(tx).ReserveAgentAdmission(
		ctx,
		sqlcgen.ReserveAgentAdmissionParams{
			CapabilityID:     admission.Record.Job.CapabilityID,
			IdempotencyScope: admission.Record.IdempotencyScope,
			IdempotencyKey:   admission.Record.IdempotencyKey,
			ExecutionID:      admission.Record.Job.ID,
			ConfiguredMax:    r.policy.MaxOutstanding,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit agent admission reserve replay: %w", err)
		}
		committed = true
		return nil
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit agent admission reserve: %w", err)
	}
	committed = true
	return nil
}

// reserveAgentAdmissionError maps the reservation trigger's custom SQLSTATEs to
// the typed admission errors the API surface already reports. E9651 fails
// closed on a policy mismatch; E9652 is the transient capacity exhaustion;
// E9650 is an internal invariant (the policy row is missing).
func (r *AgentExecutionJobsRepository) reserveAgentAdmissionError(
	capabilityID string,
	err error,
) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "E9651":
			return fmt.Errorf(
				"%w: capability %q configured=%d",
				ErrAdmissionPolicyMismatch,
				capabilityID,
				r.policy.MaxOutstanding,
			)
		case "E9652":
			return &executionapp.AdmissionCapacityError{
				CapabilityID:   capabilityID,
				MaxOutstanding: r.policy.MaxOutstanding,
			}
		case "E9650":
			return fmt.Errorf("agent admission policy missing for capability %q: %w", capabilityID, err)
		}
	}
	return fmt.Errorf("reserve agent admission: %w", err)
}

// ReapAgentAdmissionReservations removes two kinds of row: unmaterialized
// reservations older than staleSeconds, the slots a start leaked by dying
// between its reserve commit and its materialize commit, and materialized
// reservations older than gcSeconds, which are replay shortcuts only —
// execution_jobs owns idempotency. The DELETE is idempotent, so every replica
// may run its own reaper without fencing.
func (r *AgentExecutionJobsRepository) ReapAgentAdmissionReservations(
	ctx context.Context,
	staleSeconds, gcSeconds int64,
) (int64, error) {
	rows, err := sqlcgen.New(r.pool).ReapAgentAdmissionReservations(
		ctx,
		sqlcgen.ReapAgentAdmissionReservationsParams{
			StaleSeconds: staleSeconds,
			GcSeconds:    gcSeconds,
		},
	)
	if err != nil {
		return 0, fmt.Errorf("reap agent admission reservations: %w", err)
	}
	return rows, nil
}

// materializeAgentAdmission writes the input bundle, execution job, binding,
// current turn and command outbox, then marks the reserved slot materialized in
// the same commit. It runs without the capability policy row lock: the slot was
// already reserved in phase 1. It re-checks the idempotency anchor under this
// transaction's own view so a concurrent start that committed between the pool
// pre-check and here is reported as a replay, never a second job.
func (r *AgentExecutionJobsRepository) materializeAgentAdmission(
	ctx context.Context,
	admission agentexecutionapp.Admission,
	resourceProject,
	projectionProject int64,
) (executionapp.AdmissionOutcome, error) {
	capabilityID := admission.Record.Job.CapabilityID
	tx, err := r.pool.BeginTx(
		ctx,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
	)
	if err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("begin agent admission materialize: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	txQueries := sqlcgen.New(tx)
	currentProjectID := int64(0)
	if admission.CurrentTurn != nil {
		currentProjectID = admission.CurrentTurn.ProjectID
	} else if admission.CurrentAdhocTurn != nil {
		currentProjectID = admission.CurrentAdhocTurn.ProjectID
	} else if admission.CurrentRegenerateTurn != nil {
		currentProjectID = admission.CurrentRegenerateTurn.ProjectID
	} else if admission.CurrentContinueTurn != nil {
		currentProjectID = admission.CurrentContinueTurn.ProjectID
	}
	if currentProjectID > 0 {
		if err := tenant.BindProject(
			ctx,
			tx,
			tenant.Project{ID: currentProjectID},
		); err != nil {
			return executionapp.AdmissionOutcome{}, fmt.Errorf("bind current agent project: %w", err)
		}
		conversationID := ""
		if admission.CurrentTurn != nil {
			conversationID = admission.CurrentTurn.ConversationUUID
		} else if admission.CurrentAdhocTurn != nil {
			conversationID = admission.CurrentAdhocTurn.ConversationUUID
		} else if admission.CurrentRegenerateTurn != nil {
			conversationID = admission.CurrentRegenerateTurn.ConversationUUID
		} else {
			conversationID = admission.CurrentContinueTurn.ConversationUUID
		}
		conversationUUID, err := currentPGUUID(conversationID)
		if err != nil {
			return executionapp.AdmissionOutcome{}, executionapp.ErrInvalidAdmission
		}
		if _, err := txQueries.LockCurrentAgentConversation(ctx, conversationUUID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return executionapp.AdmissionOutcome{}, agentexecutionapp.ErrUnsupportedCurrentAgentStart
			}
			return executionapp.AdmissionOutcome{}, fmt.Errorf("lock current agent conversation: %w", err)
		}
	}

	existing, digest, err := loadAgentAdmission(
		ctx,
		txQueries,
		admission.Record.IdempotencyScope,
		admission.Record.IdempotencyKey,
	)
	switch {
	case err == nil:
		if digest != admission.Record.RequestDigest {
			return executionapp.AdmissionOutcome{}, executionapp.ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return executionapp.AdmissionOutcome{}, fmt.Errorf("commit agent admission replay: %w", err)
		}
		committed = true
		return existing, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return executionapp.AdmissionOutcome{}, fmt.Errorf("reload agent idempotency binding: %w", err)
	}

	// The admission timing is a pure clock read (admitted_at = now, deadline
	// = admitted_at + TTL). Computing it here instead of issuing a dedicated
	// SELECT removes one round trip from the durable materialize transaction
	// (issue 965). The invariant decodeAdmissionTiming enforces
	// (deadline - admitted_at == TTL) holds by construction.
	admittedAt := time.Now().UTC().Truncate(time.Millisecond)
	timing := admissionTiming{
		AdmittedAt: admittedAt,
		Deadline:   admittedAt.Add(r.policy.DeadlineTTL),
	}
	if err := insertRuntimeInputBundle(
		ctx,
		txQueries,
		int32(resourceProject),
		admission.Record,
		timing.AdmittedAt,
	); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	createdID, err := txQueries.InsertAgentExecutionJob(
		ctx,
		sqlcgen.InsertAgentExecutionJobParams{
			ExecutionID:         admission.Record.Job.ID,
			Generation:          int64(admission.Record.Job.Generation),
			CommandID:           admission.Record.Job.CommandID,
			TenantID:            admission.Record.Job.TenantID,
			ResourceProjectID:   int32(resourceProject),
			ProjectionProjectID: int32(projectionProject),
			ActorID:             admission.Record.Job.ActorID,
			PrincipalRef:        admission.Record.Job.ActorID,
			CapabilityID:        capabilityID,
			CapabilityVersion:   r.policy.CapabilityVersion,
			InputBundleID:       admission.Record.InputBundle.ID,
			RequestDigest:       append([]byte(nil), admission.Record.RequestDigest[:]...),
			IdempotencyScope:    admission.Record.IdempotencyScope,
			IdempotencyKey:      admission.Record.IdempotencyKey,
			State:               string(admission.Record.Job.State),
			AdmittedAt:          timestamp(timing.AdmittedAt),
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, digest, loadErr := loadAgentAdmission(
			ctx,
			txQueries,
			admission.Record.IdempotencyScope,
			admission.Record.IdempotencyKey,
		)
		if loadErr != nil {
			return executionapp.AdmissionOutcome{}, fmt.Errorf("load concurrent agent admission: %w", loadErr)
		}
		if digest != admission.Record.RequestDigest {
			return executionapp.AdmissionOutcome{}, executionapp.ErrIdempotencyConflict
		}
		if rollbackErr := tx.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil &&
			!errors.Is(rollbackErr, pgx.ErrTxClosed) {
			return executionapp.AdmissionOutcome{}, fmt.Errorf("rollback concurrent agent admission: %w", rollbackErr)
		}
		committed = true
		return existing, nil
	}
	if err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("insert agent execution job: %w", err)
	}
	if createdID != admission.Record.Job.ID {
		return executionapp.AdmissionOutcome{}, errors.New("agent execution job insert changed identity")
	}
	if err := txQueries.InsertAgentExecutionBinding(
		ctx,
		sqlcgen.InsertAgentExecutionBindingParams{
			ExecutionID:               admission.Record.Job.ID,
			Generation:                int64(admission.Record.Job.Generation),
			CapabilityID:              capabilityID,
			InputBundleID:             admission.Record.InputBundle.ID,
			RequestEntryID:            admission.Binding.RequestEntryID,
			ClientStreamID:            admission.Binding.ClientStreamID,
			ClientMessageID:           admission.Binding.ClientMessageID,
			ClientExecutionGeneration: admission.Binding.ClientExecutionGeneration,
			SioEvent:                  admission.Binding.SIOEvent,
		},
	); err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("insert agent execution binding: %w", err)
	}
	if admission.CurrentTurn != nil {
		if err := insertCurrentApplicationTurn(
			ctx,
			txQueries,
			admission.Record.Job.ID,
			*admission.CurrentTurn,
			r.catalogueProjectID,
		); err != nil {
			return executionapp.AdmissionOutcome{}, err
		}
	} else if admission.CurrentAdhocTurn != nil {
		if err := insertCurrentAdhocTurn(
			ctx,
			txQueries,
			admission.Record.Job.ID,
			*admission.CurrentAdhocTurn,
		); err != nil {
			return executionapp.AdmissionOutcome{}, err
		}
	} else if admission.CurrentRegenerateTurn != nil {
		if err := resetCurrentAgentResponse(
			ctx,
			txQueries,
			admission.Record.Job.ID,
			*admission.CurrentRegenerateTurn,
		); err != nil {
			return executionapp.AdmissionOutcome{}, err
		}
	} else if admission.CurrentContinueTurn != nil {
		turn := *admission.CurrentContinueTurn
		var resumeErr error
		switch turn.ContinuationKind {
		case agentexecutionapp.CurrentContinuationOutputLimit:
			resumeErr = resumeCurrentAgentOutputLimit(ctx, txQueries, admission.Record.Job.ID, turn)
		case agentexecutionapp.CurrentContinuationAuthorization:
			resumeErr = resumeCurrentAgentAuthorization(ctx, txQueries, admission.Record.Job.ID, turn)
		default:
			resumeErr = resumeCurrentAgentHITL(ctx, txQueries, admission.Record.Job.ID, turn)
		}
		if resumeErr != nil {
			return executionapp.AdmissionOutcome{}, resumeErr
		}
	}
	if err := txQueries.InsertRuntimeCommandOutbox(
		ctx,
		sqlcgen.InsertRuntimeCommandOutboxParams{
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
		},
	); err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("insert agent command outbox: %w", err)
	}
	// Mark the reserved slot materialized in the same commit as the job. Zero
	// rows means the reaper reclaimed it as stale while this materialized; the
	// job now holds the slot, so this is not fatal (issue 965).
	if _, err := txQueries.MarkAgentAdmissionMaterialized(
		ctx,
		sqlcgen.MarkAgentAdmissionMaterializedParams{
			CapabilityID:     capabilityID,
			IdempotencyScope: admission.Record.IdempotencyScope,
			IdempotencyKey:   admission.Record.IdempotencyKey,
		},
	); err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("mark agent admission materialized: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("commit agent admission: %w", err)
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

func resetCurrentAgentResponse(
	ctx context.Context,
	queries *sqlcgen.Queries,
	executionID string,
	turn agentexecutionapp.CurrentRegenerateTurn,
) error {
	conversationUUID, err := currentPGUUID(turn.ConversationUUID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	questionID, err := currentPGUUID(turn.QuestionID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	responseMessageID, err := currentPGUUID(turn.ResponseMessageID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	// Narrow every id explicitly. int64->int32 truncates silently, and these
	// address rows — see narrowRowID.
	targetParticipantID, ok := narrowRowID(turn.TargetParticipantID)
	if !ok {
		return executionapp.ErrInvalidAdmission
	}
	applicationID, ok := narrowRowID(turn.ApplicationID)
	if !ok {
		return executionapp.ErrInvalidAdmission
	}
	applicationVersionID, ok := narrowRowID(turn.ApplicationVersionID)
	if !ok {
		return executionapp.ErrInvalidAdmission
	}
	projectID, ok := narrowRowID(turn.ProjectID)
	if !ok {
		return executionapp.ErrInvalidAdmission
	}
	row, err := queries.ResetCurrentAgentResponse(
		ctx,
		sqlcgen.ResetCurrentAgentResponseParams{
			ActorUserID: turn.ActorUserID, TargetParticipantID: targetParticipantID,
			ConversationUuid: conversationUUID, QuestionID: questionID,
			ResponseMessageID: responseMessageID, RegenerationKind: string(turn.Kind),
			ApplicationID:        applicationID,
			ApplicationVersionID: applicationVersionID,
			ExecutionGeneration:  turn.ExecutionGeneration, ExecutionID: executionID,
			ProjectID: projectID,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentexecutionapp.ErrUnsupportedCurrentAgentStart
	}
	if err != nil {
		return fmt.Errorf("reset current agent response: %w", err)
	}
	if row.ResponseMessageGroupID <= 0 || row.ResponseMessageID != responseMessageID {
		return errors.New("current agent regeneration returned an invalid response binding")
	}
	return rewriteCurrentAgentQuestion(ctx, queries, turn, conversationUUID, questionID)
}

// rewriteCurrentAgentQuestion writes an EDITED question's text onto its stored
// item, in the same transaction that has just reset the answer (issue 980).
//
// A retry carries no edit and this is a no-op — the ordinary regeneration does
// not touch the question at all.
//
// WHY THE ORDER IS RESET-THEN-REWRITE: the reset holds `FOR UPDATE` on the
// response row and is the statement that decides whether this caller may
// regenerate this turn at all. Rewriting first would let a request that the
// reset then refuses leave an edited question behind with its old answer
// underneath it.
//
// NO ROW MEANS REFUSED, NOT IGNORED. The statement matches nothing when the
// item named does not belong to this question — the only way that happens is a
// client naming another message's item — so the admission fails with the
// invalid-request error the route renders as 400. The alternative (treat it as
// "nothing to rewrite" and run anyway) would answer a question the user never
// asked while telling them it had been edited.
func rewriteCurrentAgentQuestion(
	ctx context.Context,
	queries *sqlcgen.Queries,
	turn agentexecutionapp.CurrentRegenerateTurn,
	conversationUUID pgtype.UUID,
	questionID pgtype.UUID,
) error {
	if !turn.EditedQuestion.Requested() {
		return nil
	}
	// The zero uuid is this statement's "the caller named no item" — see the
	// query's own comment. It is not a value any row carries.
	itemUUID := pgtype.UUID{Bytes: [16]byte{}, Valid: true}
	if turn.EditedQuestion.ItemUUID != "" {
		parsed, err := currentPGUUID(turn.EditedQuestion.ItemUUID)
		if err != nil {
			return agentexecutionapp.ErrInvalidCurrentAgentStart
		}
		itemUUID = parsed
	}
	if _, err := queries.RewriteCurrentAgentQuestionText(
		ctx,
		sqlcgen.RewriteCurrentAgentQuestionTextParams{
			ConversationUuid: conversationUUID, QuestionID: questionID,
			ActorUserID: turn.ActorUserID, ItemUuid: itemUUID,
			Content: turn.EditedQuestion.Text,
		},
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return agentexecutionapp.ErrInvalidCurrentAgentStart
		}
		return fmt.Errorf("rewrite current agent question: %w", err)
	}
	return nil
}

func resumeCurrentAgentHITL(
	ctx context.Context,
	queries *sqlcgen.Queries,
	executionID string,
	turn agentexecutionapp.CurrentContinueTurn,
) error {
	projectID, projectIDValid := currentAgentDatabaseID(turn.ProjectID)
	targetParticipantID, targetParticipantIDValid := currentAgentDatabaseID(turn.TargetParticipantID)
	if !projectIDValid || !targetParticipantIDValid {
		return executionapp.ErrInvalidAdmission
	}
	var applicationID, applicationVersionID int32
	if turn.Kind == agentexecutionapp.CurrentRegenerationApplication {
		var applicationIDValid, applicationVersionIDValid bool
		applicationID, applicationIDValid = currentAgentDatabaseID(turn.ApplicationID)
		applicationVersionID, applicationVersionIDValid = currentAgentDatabaseID(turn.ApplicationVersionID)
		if !applicationIDValid || !applicationVersionIDValid {
			return executionapp.ErrInvalidAdmission
		}
	}
	conversationUUID, err := currentPGUUID(turn.ConversationUUID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	questionID, err := currentPGUUID(turn.QuestionID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	responseMessageID, err := currentPGUUID(turn.ResponseMessageID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	row, err := queries.ResumeCurrentAgentHITL(
		ctx,
		sqlcgen.ResumeCurrentAgentHITLParams{
			ActorUserID: turn.ActorUserID, TargetParticipantID: targetParticipantID,
			ConversationUuid: conversationUUID, QuestionID: questionID,
			ResponseMessageID: responseMessageID, ContinuationKind: string(turn.Kind),
			ApplicationID: applicationID, ApplicationVersionID: applicationVersionID,
			ExecutionGeneration: turn.ExecutionGeneration, ThreadID: turn.ThreadID,
			HitlDecisions: []byte(turn.HITLDecisions),
			ExecutionID:   executionID, ProjectID: projectID,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentexecutionapp.ErrCurrentAgentHITLAlreadyResolved
	}
	if err != nil {
		return fmt.Errorf("resume current agent HITL: %w", err)
	}
	if row.ResponseMessageGroupID <= 0 || row.ResponseMessageID != responseMessageID {
		return errors.New("current agent continuation returned an invalid response binding")
	}
	return nil
}

func resumeCurrentAgentOutputLimit(
	ctx context.Context,
	queries *sqlcgen.Queries,
	executionID string,
	turn agentexecutionapp.CurrentContinueTurn,
) error {
	projectID, projectIDValid := currentAgentDatabaseID(turn.ProjectID)
	targetParticipantID, targetParticipantIDValid := currentAgentDatabaseID(turn.TargetParticipantID)
	if !projectIDValid || !targetParticipantIDValid {
		return executionapp.ErrInvalidAdmission
	}
	var applicationID, applicationVersionID int32
	if turn.Kind == agentexecutionapp.CurrentRegenerationApplication {
		var applicationIDValid, applicationVersionIDValid bool
		applicationID, applicationIDValid = currentAgentDatabaseID(turn.ApplicationID)
		applicationVersionID, applicationVersionIDValid = currentAgentDatabaseID(turn.ApplicationVersionID)
		if !applicationIDValid || !applicationVersionIDValid {
			return executionapp.ErrInvalidAdmission
		}
	}
	conversationUUID, err := currentPGUUID(turn.ConversationUUID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	questionID, err := currentPGUUID(turn.QuestionID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	responseMessageID, err := currentPGUUID(turn.ResponseMessageID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	row, err := queries.ResumeCurrentAgentOutputLimit(
		ctx,
		sqlcgen.ResumeCurrentAgentOutputLimitParams{
			ActorUserID: turn.ActorUserID, TargetParticipantID: targetParticipantID,
			ConversationUuid: conversationUUID, QuestionID: questionID,
			ResponseMessageID: responseMessageID, ContinuationKind: string(turn.Kind),
			ApplicationID: applicationID, ApplicationVersionID: applicationVersionID,
			ExecutionGeneration: turn.ExecutionGeneration, ThreadID: turn.ThreadID,
			OutputLimitSequence: turn.OutputLimitSequence,
			ExecutionID:         executionID, ProjectID: projectID,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentexecutionapp.ErrCurrentAgentOutputLimitAlreadyResolved
	}
	if err != nil {
		return fmt.Errorf("resume current agent output-limit continuation: %w", err)
	}
	if row.ResponseMessageGroupID <= 0 || row.ResponseMessageID != responseMessageID {
		return errors.New("current agent output-limit continuation returned an invalid response binding")
	}
	return nil
}

func resumeCurrentAgentAuthorization(
	ctx context.Context,
	queries *sqlcgen.Queries,
	executionID string,
	turn agentexecutionapp.CurrentContinueTurn,
) error {
	projectID, projectIDValid := currentAgentDatabaseID(turn.ProjectID)
	targetParticipantID, targetParticipantIDValid := currentAgentDatabaseID(turn.TargetParticipantID)
	if !projectIDValid || !targetParticipantIDValid {
		return executionapp.ErrInvalidAdmission
	}
	var applicationID, applicationVersionID int32
	if turn.Kind == agentexecutionapp.CurrentRegenerationApplication {
		var applicationIDValid, applicationVersionIDValid bool
		applicationID, applicationIDValid = currentAgentDatabaseID(turn.ApplicationID)
		applicationVersionID, applicationVersionIDValid = currentAgentDatabaseID(turn.ApplicationVersionID)
		if !applicationIDValid || !applicationVersionIDValid {
			return executionapp.ErrInvalidAdmission
		}
	}
	conversationUUID, err := currentPGUUID(turn.ConversationUUID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	questionID, err := currentPGUUID(turn.QuestionID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	responseMessageID, err := currentPGUUID(turn.ResponseMessageID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	row, err := queries.ResumeCurrentAgentAuthorization(
		ctx,
		sqlcgen.ResumeCurrentAgentAuthorizationParams{
			ActorUserID: turn.ActorUserID, TargetParticipantID: targetParticipantID,
			ConversationUuid: conversationUUID, QuestionID: questionID,
			ResponseMessageID: responseMessageID, ContinuationKind: string(turn.Kind),
			ApplicationID: applicationID, ApplicationVersionID: applicationVersionID,
			ExecutionGeneration: turn.ExecutionGeneration, ThreadID: turn.ThreadID,
			HitlDecisions: []byte(turn.HITLDecisions),
			ExecutionID:   executionID, ProjectID: projectID,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentexecutionapp.ErrCurrentAgentAuthorizationAlreadyResolved
	}
	if err != nil {
		return fmt.Errorf("resume current agent authorization: %w", err)
	}
	if row.ResponseMessageGroupID <= 0 || row.ResponseMessageID != responseMessageID {
		return errors.New("current agent authorization continuation returned an invalid response binding")
	}
	return nil
}

func insertCurrentApplicationTurn(
	ctx context.Context,
	queries *sqlcgen.Queries,
	executionID string,
	turn agentexecutionapp.CurrentApplicationTurn,
	catalogueProjectID int32,
) error {
	if catalogueProjectID <= 0 {
		return executionapp.ErrInvalidAdmission
	}
	conversationUUID, err := currentPGUUID(turn.ConversationUUID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	questionID, err := currentPGUUID(turn.QuestionID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	questionItemID, err := currentPGUUID(turn.QuestionItemID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	responseMessageID, err := currentPGUUID(turn.ResponseMessageID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	// Same narrowing discipline as the reset path: these ids address rows and
	// int64->int32 truncates silently. CodeQL flagged only the reset call site,
	// but the exposure is identical here.
	targetParticipantID, ok := narrowRowID(turn.TargetParticipantID)
	if !ok {
		return executionapp.ErrInvalidAdmission
	}
	applicationVersionID, ok := narrowRowID(turn.ApplicationVersionID)
	if !ok {
		return executionapp.ErrInvalidAdmission
	}
	applicationID, ok := narrowRowID(turn.ApplicationID)
	if !ok {
		return executionapp.ErrInvalidAdmission
	}
	projectID, ok := narrowRowID(turn.ProjectID)
	if !ok {
		return executionapp.ErrInvalidAdmission
	}
	row, err := queries.InsertCurrentApplicationTurn(
		ctx,
		sqlcgen.InsertCurrentApplicationTurnParams{
			ActorUserID:          turn.ActorUserID,
			TargetParticipantID:  targetParticipantID,
			ApplicationVersionID: applicationVersionID,
			ApplicationID:        applicationID,
			ConversationUuid:     conversationUUID,
			ProjectID:            projectID,
			QuestionID:           questionID,
			QuestionMeta:         append([]byte(nil), turn.QuestionMeta...),
			QuestionItemID:       questionItemID,
			UserInput:            turn.UserInput,
			ResponseMessageID:    responseMessageID,
			ExecutionID:          executionID,
			ExecutionGeneration:  turn.QuestionID,
			CatalogueProjectID:   catalogueProjectID,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentexecutionapp.ErrUnsupportedCurrentAgentStart
	}
	if err != nil {
		return fmt.Errorf("insert current agent chat turn: %w", err)
	}
	if row.ResponseMessageGroupID <= 0 || row.ResponseMessageID != responseMessageID {
		return errors.New("current agent chat turn returned an invalid response binding")
	}
	return insertCurrentTurnAttachments(ctx, queries, row.QuestionMessageGroupID, turn.Attachments)
}

func insertCurrentAdhocTurn(
	ctx context.Context,
	queries *sqlcgen.Queries,
	executionID string,
	turn agentexecutionapp.CurrentAdhocTurn,
) error {
	conversationUUID, err := currentPGUUID(turn.ConversationUUID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	questionID, err := currentPGUUID(turn.QuestionID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	questionItemID, err := currentPGUUID(turn.QuestionItemID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	responseMessageID, err := currentPGUUID(turn.ResponseMessageID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	adhocTargetParticipantID, ok := narrowRowID(turn.TargetParticipantID)
	if !ok {
		return executionapp.ErrInvalidAdmission
	}
	adhocProjectID, ok := narrowRowID(turn.ProjectID)
	if !ok {
		return executionapp.ErrInvalidAdmission
	}
	row, err := queries.InsertCurrentAdhocTurn(
		ctx,
		sqlcgen.InsertCurrentAdhocTurnParams{
			ActorUserID: turn.ActorUserID, TargetParticipantID: adhocTargetParticipantID,
			ConversationUuid: conversationUUID, ProjectID: adhocProjectID,
			QuestionID: questionID, QuestionMeta: append([]byte(nil), turn.QuestionMeta...),
			QuestionItemID: questionItemID, UserInput: turn.UserInput,
			ResponseMessageID: responseMessageID, ExecutionGeneration: turn.QuestionID,
			ExecutionID: executionID,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentexecutionapp.ErrUnsupportedCurrentAgentStart
	}
	if err != nil {
		return fmt.Errorf("insert current ad-hoc agent chat turn: %w", err)
	}
	if row.ResponseMessageGroupID <= 0 || row.ResponseMessageID != responseMessageID {
		return errors.New("current ad-hoc agent chat turn returned an invalid response binding")
	}
	return insertCurrentTurnAttachments(ctx, queries, row.QuestionMessageGroupID, turn.Attachments)
}

// insertCurrentTurnAttachments writes the turn's uploaded files as
// `attachment_message` items on the QUESTION group, in the SAME transaction
// that just created that group (#606 part 2).
//
// WHY THE QUESTION GROUP, AND WHY HERE. Pylon attaches them to the user's
// message (rpc/chat_all.py:285-320), which is what makes the transcript render
// the file under the message the user sent it with; hanging them off the
// streaming response group instead would attribute the user's upload to the
// agent. And the admission transaction is the only place in this service where
// a message group is created at all — the worker writes no chat rows — so
// doing it anywhere else would mean a second transaction that can fail after
// the turn is already admitted, leaving an admitted turn whose attachments
// silently vanished.
//
// questionMessageGroupID comes back from the turn's own INSERT (its
// response_group's reply_to_id, which IS the question group) rather than from a
// follow-up SELECT on the question uuid: re-reading would be a second round
// trip for an id the statement already produced, and would read through the
// same uncommitted state anyway.
//
// order_index starts at 1: the question's text item holds 0
// (InsertCurrentApplicationTurn/InsertCurrentAdhocTurn write it there), and
// pylon enumerates its attachments from 1 for exactly that reason
// (rpc/chat_all.py:303).
func insertCurrentTurnAttachments(
	ctx context.Context,
	queries *sqlcgen.Queries,
	questionMessageGroupID *int32,
	attachments []agentexecutionapp.CurrentTurnAttachment,
) error {
	if len(attachments) == 0 {
		return nil
	}
	// A turn with attachments whose insert did not report a question group is
	// a contradiction — the response group is created with reply_to_id set
	// from the question group in the same statement — and must not degrade to
	// "write no attachments", which is indistinguishable from a turn that had
	// none. Fail the admission instead.
	if questionMessageGroupID == nil || *questionMessageGroupID <= 0 {
		return errors.New("current agent chat turn returned no question message group for its attachments")
	}
	for index, attachment := range attachments {
		itemID, err := currentPGUUID(attachment.ItemID)
		if err != nil {
			return executionapp.ErrInvalidAdmission
		}
		orderIndex, ok := narrowRowID(int64(index) + 1)
		if !ok {
			return executionapp.ErrInvalidAdmission
		}
		if err := queries.InsertCurrentAgentAttachmentItem(
			ctx,
			sqlcgen.InsertCurrentAgentAttachmentItemParams{
				ItemID:         itemID,
				OrderIndex:     orderIndex,
				MessageGroupID: *questionMessageGroupID,
				Name:           attachment.Name,
				Bucket:         attachment.Bucket,
				AttachmentType: attachment.AttachmentType,
				Content:        append([]byte(nil), attachment.Content...),
			},
		); err != nil {
			return fmt.Errorf("insert current agent chat attachment item: %w", err)
		}
	}
	return nil
}

func resourceProjectIDUnchecked(value string) int64 {
	projectID, _ := strconv.ParseInt(value, 10, 64)
	return projectID
}

func loadAgentAdmission(
	ctx context.Context,
	queries *sqlcgen.Queries,
	scope,
	key string,
) (executionapp.AdmissionOutcome, runtimedomain.Digest, error) {
	row, err := queries.GetAgentExecutionAdmissionByIdempotency(
		ctx,
		sqlcgen.GetAgentExecutionAdmissionByIdempotencyParams{
			IdempotencyScope: scope,
			IdempotencyKey:   key,
		},
	)
	if err != nil {
		return executionapp.AdmissionOutcome{}, runtimedomain.Digest{}, err
	}
	digest, err := storedDigest(row.RequestDigest)
	if err != nil {
		return executionapp.AdmissionOutcome{}, runtimedomain.Digest{}, fmt.Errorf(
			"invalid stored agent request digest: %w",
			err,
		)
	}
	if row.Generation <= 0 || !row.AdmittedAt.Valid || !row.Deadline.Valid ||
		row.AdmittedAt.Time.IsZero() || !row.Deadline.Time.After(row.AdmittedAt.Time) {
		return executionapp.AdmissionOutcome{}, runtimedomain.Digest{},
			errors.New("stored agent admission is invalid")
	}
	return executionapp.AdmissionOutcome{
		ExecutionID: row.ExecutionID,
		CommandID:   row.CommandID,
		Created:     false,
		AdmittedAt:  row.AdmittedAt.Time.UTC(),
		Deadline:    row.Deadline.Time.UTC(),
	}, digest, nil
}

func boundedAgentAdmissionStrings(admission agentexecutionapp.Admission) bool {
	record := admission.Record
	binding := admission.Binding
	values := []string{
		record.IdempotencyScope,
		record.IdempotencyKey,
		record.InputBundle.ID,
		record.InputBundle.Version,
		record.InputBundle.MediaType,
		record.Job.ID,
		record.Job.CommandID,
		record.Job.TenantID,
		record.Job.ResourceProjectID,
		record.Job.ProjectionProjectID,
		record.Job.ActorID,
		record.Job.CapabilityID,
		record.Outbox.ID,
		binding.RequestEntryID,
		binding.ClientStreamID,
		binding.ClientMessageID,
		binding.ClientExecutionGeneration,
		binding.SIOEvent,
	}
	for _, entry := range record.InputBundle.Entries {
		values = append(
			values,
			entry.ID,
			entry.Version,
			entry.SemanticRole,
			entry.ContentID,
			entry.MediaType,
			entry.Classification,
			entry.RequiredGrantAudience,
		)
	}
	for _, value := range values {
		if value == "" || len(value) > executiondomain.MaxIndexMetaCorrelationBytes ||
			strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
	}
	return true
}

func agentExecutionCapability(capabilityID string) bool {
	return capabilityID == executiondomain.AgentApplicationCapability ||
		capabilityID == executiondomain.AgentAdhocCapability
}

var _ agentexecutionapp.AtomicAdmissionStore = (*AgentExecutionJobsRepository)(nil)

// narrowRowID converts a domain id (int64) to the int32 the sqlc params use,
// refusing anything the target type cannot hold.
//
// Go's int64->int32 conversion is a SILENT TRUNCATION, and these ids address
// rows: 4294967300 becomes 4, so an out-of-range id would not fail — it would
// read or WRITE a different, entirely valid row. The columns are Postgres
// `integer`, so a value above MaxInt32 cannot correspond to any row and
// refusing is the only correct answer.
//
// The API boundary (positiveCanonicalID) already bounds ids on the way in.
// This is deliberate defence in depth at the point of narrowing: it also covers
// callers that do not come through that route, and unlike the boundary check it
// is local enough for CodeQL's dataflow to see (go/incorrect-integer-conversion).
func narrowRowID(value int64) (int32, bool) {
	if value < 0 || value > math.MaxInt32 {
		return 0, false
	}
	return int32(value), true
}
