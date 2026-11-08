package agentexecution

import (
	"context"
	"errors"
	"log/slog"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/idlecompaction"
)

// Idle compaction admission bounds (ADR-0050 decision 9b2).
const (
	// IdleCompactionSweepBatch bounds one pass.
	IdleCompactionSweepBatch = 16
	// IdleCompactionSweepInterval is the periodic sweep: a check is due one
	// minute inside its cache, so 15 seconds keeps it well inside.
	IdleCompactionSweepInterval = 15 * time.Second
	// IdleCompactionAdmissionLease is how long a claimed row is skipped by
	// other admitters (database clock).
	IdleCompactionAdmissionLease = 30 * time.Second
	// IdleCompactionMaxAttempts ends a row whose admission keeps failing for
	// a reason it cannot classify (admission_exhausted). Its cache is gone
	// long before, so a small bound is enough.
	IdleCompactionMaxAttempts = 8
)

// IdleCompactionStore claims due QUEUED rows and ends the ones that do not
// run.
//
// ClaimIdleCompactions claims at most limit QUEUED rows with due_at <= now
// that no other admitter holds (FOR UPDATE SKIP LOCKED, a lease on the
// database clock), ends exhausted rows and rows whose source can never be
// admitted, and returns the rest with the decision inputs (the participant's
// latest execution, whether a turn of it runs) and the source's frozen
// request.
type IdleCompactionStore interface {
	ClaimIdleCompactions(ctx context.Context, limit int, lease time.Duration, maxAttempts int) ([]IdleCompactionTarget, error)
	// SkipIdleCompaction ends a QUEUED row SKIPPED with code.
	SkipIdleCompaction(ctx context.Context, turn CurrentIdleCompactionTurn, code string) error
	// DeferIdleCompaction keeps a transiently refused row QUEUED and gives
	// its attempt back.
	DeferIdleCompaction(ctx context.Context, turn CurrentIdleCompactionTurn) error
}

// IdleCompactionAdmitter turns a due QUEUED agent_idle_compactions row into
// at most one idle_compaction job. Kick runs a pass right after a settled
// turn armed a check; Run also sweeps periodically (the row is due minutes
// later, so the sweep is what admits it). Exactly one job per row holds
// across processes because the admission transaction moves the row QUEUED →
// ADMITTED with the job, under the row lock, behind the idempotency key
// idle-compaction/{source}.
type IdleCompactionAdmitter struct {
	store    IdleCompactionStore
	submit   FanoutWakeSubmitter
	logger   *slog.Logger
	interval time.Duration
	kick     chan struct{}
}

func NewIdleCompactionAdmitter(store IdleCompactionStore, submit FanoutWakeSubmitter, logger *slog.Logger) (*IdleCompactionAdmitter, error) {
	if store == nil || submit == nil || logger == nil {
		return nil, errors.New("idle compaction admitter dependencies are required")
	}
	return &IdleCompactionAdmitter{store: store, submit: submit, logger: logger,
		interval: IdleCompactionSweepInterval, kick: make(chan struct{}, 1)}, nil
}

// Kick asks for a pass now. It never blocks; kicks coalesce.
func (a *IdleCompactionAdmitter) Kick() {
	if a == nil {
		return
	}
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// Run serves kicks and the periodic sweep until ctx ends.
func (a *IdleCompactionAdmitter) Run(ctx context.Context) error {
	if a == nil || ctx == nil {
		return errors.New("idle compaction admitter is not configured")
	}
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-a.kick:
		case <-ticker.C:
		}
		if _, err := a.Sweep(ctx); err != nil && ctx.Err() == nil {
			a.logger.Error("idle compaction admission pass failed", "reason", "unavailable")
		}
	}
}

// Sweep runs one bounded pass and reports how many jobs it created.
func (a *IdleCompactionAdmitter) Sweep(ctx context.Context) (int, error) {
	targets, err := a.store.ClaimIdleCompactions(ctx, IdleCompactionSweepBatch, IdleCompactionAdmissionLease, IdleCompactionMaxAttempts)
	if err != nil {
		return 0, err
	}
	created := 0
	for _, target := range targets {
		if ctx.Err() != nil {
			return created, ctx.Err()
		}
		if code := idlecompaction.Decide(target.Check(), target.Now); code != idlecompaction.SkipNone {
			a.skip(ctx, target.Turn, string(code))
			continue
		}
		outcome, err := a.admit(ctx, target)
		switch {
		case err == nil:
			if outcome.Created {
				created++
			}
		case errors.Is(err, ErrIdleCompactionNotQueued):
			// Admitted elsewhere, or cancelled by the agent's next turn.
		case errors.Is(err, ErrIdleCompactionSuperseded):
			a.skip(ctx, target.Turn, string(idlecompaction.SkipSuperseded))
		case errors.Is(err, ErrIdleCompactionAgentBusy):
			a.skip(ctx, target.Turn, string(idlecompaction.SkipAgentBusy))
		case errors.Is(err, ErrIdleCompactionActorNotAllowed):
			a.skip(ctx, target.Turn, IdleCompactionSkipActorNotAllowed)
		case errors.Is(err, errInvalidIdleCompaction):
			a.skip(ctx, target.Turn, IdleCompactionSkipSourceInvalid)
		case errors.Is(err, ErrInvalidAgentAdmission), errors.Is(err, executionapp.ErrInvalidAdmission),
			errors.Is(err, executionapp.ErrIdempotencyConflict), errors.Is(err, ErrProjectSkillUnknown),
			governedRefusal(err), admissionRefusal(err), errors.Is(err, ErrUnsupportedCurrentAgentStart):
			a.skip(ctx, target.Turn, IdleCompactionSkipAdmission)
		case errors.Is(err, executionapp.ErrAdmissionCapacityExhausted):
			// Stays QUEUED and gives its attempt back; a check whose cache
			// expires meanwhile is skipped by the next pass's Decide.
			if deferErr := a.store.DeferIdleCompaction(ctx, target.Turn); deferErr != nil && ctx.Err() == nil {
				a.logger.Warn("idle compaction deferral was not recorded", "reason", "unavailable")
			}
		default:
			if ctx.Err() != nil {
				return created, ctx.Err()
			}
			// Keeps its attempt: an error that never clears ends the row
			// typed after IdleCompactionMaxAttempts.
			a.logger.Warn("idle compaction admission deferred", "reason", "admission_unavailable")
		}
	}
	return created, nil
}

func (a *IdleCompactionAdmitter) admit(ctx context.Context, target IdleCompactionTarget) (executionapp.AdmissionOutcome, error) {
	input, err := IdleCompactionInput(target)
	if err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	turn := target.Turn
	return a.submit.Submit(ctx, SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: target.TenantID, ResourceProjectID: target.ResourceProjectID,
			ProjectionProjectID: target.ProjectionProjectID, ActorID: target.ActorID,
		},
		IdempotencyKey:            IdleCompactionIdempotencyKey(turn.SourceExecutionID),
		CapabilityID:              target.CapabilityID,
		ClientStreamID:            turn.ConversationUUID,
		ClientMessageID:           turn.ResponseMessageID,
		SIOEvent:                  "chat_continue_predict",
		Input:                     input,
		CurrentIdleCompactionTurn: turn.Clone(),
		TriggerOrigin:             target.TriggerOrigin,
	})
}

func (a *IdleCompactionAdmitter) skip(ctx context.Context, turn CurrentIdleCompactionTurn, code string) {
	if err := a.store.SkipIdleCompaction(ctx, turn, code); err != nil && ctx.Err() == nil {
		a.logger.Warn("idle compaction skip was not recorded", "reason", "unavailable", "skip_code", code)
		return
	}
	a.logger.Debug("idle compaction skipped", "skip_code", code)
}

// IdleCompactionArmStore arms a settled turn's idle check and ends an idle
// run's row when it settles.
type IdleCompactionArmStore interface {
	// ArmIdleCompaction reads the settled execution, asks arm whether and
	// when its check is due and queues it; it reports whether a row was
	// written. It writes nothing for an execution that is not an agent's
	// latest answered chat turn.
	ArmIdleCompaction(ctx context.Context, executionID string, generation uint64,
		arm func(idlecompaction.Turn) (time.Time, idlecompaction.SkipCode)) (bool, error)
	// SettleIdleCompactionRun ends an ADMITTED row of an idle run.
	SettleIdleCompactionRun(ctx context.Context, executionID string, outcome executionapp.SettlementOutcome) error
}

// IdleCompactionSettleHook is the after-settle hook of idle compaction: a
// succeeded agent execution arms its check (and kicks the admitter when one
// was queued); every settled agent execution ends its row when it is an idle
// run. A replayed settlement is harmless: both statements are idempotent.
type IdleCompactionSettleHook struct {
	store    IdleCompactionArmStore
	admitter *IdleCompactionAdmitter
	logger   *slog.Logger
}

func NewIdleCompactionSettleHook(store IdleCompactionArmStore, admitter *IdleCompactionAdmitter, logger *slog.Logger) (*IdleCompactionSettleHook, error) {
	if store == nil || logger == nil {
		return nil, errors.New("idle compaction settle hook dependencies are required")
	}
	return &IdleCompactionSettleHook{store: store, admitter: admitter, logger: logger}, nil
}

// AfterSettle is an executionapp.AfterSettleHook.
func (h *IdleCompactionSettleHook) AfterSettle(ctx context.Context, proposal executionapp.SettlementProposal, receipt executionapp.SettlementReceipt) {
	if h == nil {
		return
	}
	executionID := proposal.Fence.ExecutionID
	if proposal.TerminalLogicalOutputID != "agent-execution:"+executionID {
		return
	}
	if err := h.store.SettleIdleCompactionRun(ctx, executionID, receipt.Outcome); err != nil && ctx.Err() == nil {
		h.logger.Warn("idle compaction run end was not recorded", "reason", "unavailable")
	}
	if receipt.Outcome != executionapp.SettlementSucceeded {
		return
	}
	armed, err := h.store.ArmIdleCompaction(ctx, executionID, proposal.Fence.Generation, idlecompaction.Arm)
	if err != nil {
		if ctx.Err() == nil {
			h.logger.Warn("idle compaction was not armed", "reason", "unavailable")
		}
		return
	}
	if armed {
		h.admitter.Kick()
	}
}
