package execution

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

var (
	ErrInvalidSettlement      = errors.New("invalid execution settlement")
	ErrSettlementConflict     = errors.New("settlement idempotency conflict")
	ErrTerminalOutputNotReady = errors.New("terminal output is not durably ready for settlement")
)

type SettlementOutcome string

const (
	SettlementSucceeded      SettlementOutcome = "SUCCEEDED"
	SettlementFailed         SettlementOutcome = "FAILED"
	SettlementCancelled      SettlementOutcome = "CANCELLED"
	SettlementOutcomeUnknown SettlementOutcome = "OUTCOME_UNKNOWN"
)

func (o SettlementOutcome) valid() bool {
	return o == SettlementSucceeded || o == SettlementFailed || o == SettlementCancelled || o == SettlementOutcomeUnknown
}

type SettlementProposal struct {
	Fence                   runtimedomain.Fence
	ProposalID              string
	Outcome                 SettlementOutcome
	TerminalLogicalOutputID string
	TerminalEventID         string
	TerminalSequence        uint64
	TerminalPayloadDigest   runtimedomain.Digest
	ProposalDigest          runtimedomain.Digest
	IdempotencyKey          string
}

func (p SettlementProposal) Validate() error {
	if err := p.Fence.Validate(); err != nil {
		return err
	}
	if p.ProposalID == "" || !p.Outcome.valid() || p.TerminalLogicalOutputID == "" || p.TerminalEventID == "" || p.TerminalSequence == 0 || p.TerminalPayloadDigest.IsZero() || p.ProposalDigest.IsZero() || p.IdempotencyKey == "" {
		return ErrInvalidSettlement
	}
	return nil
}

type SettlementReceipt struct {
	ID      string
	Outcome SettlementOutcome
}

// SettlementRepository atomically verifies the live fence and matching
// durably ACKed terminal projection before insertion. Idempotent replay is
// authorized only by the exact persisted fence of the SETTLED claim; a reused
// key with a different digest returns ErrSettlementConflict.
type SettlementRepository interface {
	PrepareSettlement(ctx context.Context, proposal SettlementProposal) (SettlementReceipt, error)
}

// AfterSettleHook observes a settlement that has ALREADY committed —
// PrepareSettlement calls every registered hook only after
// s.repository.PrepareSettlement has returned a valid receipt, never before
// and never from inside whatever transaction the repository ran (this
// package has no idea what that repository does internally, which is exactly
// the point: a hook here cannot reach into it even by accident).
//
// A hook fires on EVERY successful call, including an idempotent REPLAY of an
// already-settled proposal (SettlementRepository.PrepareSettlement's own
// contract does not, and must not be made to, distinguish "just settled"
// from "already was settled, returned unchanged" — teaching it to would mean
// touching the exact fence-verification transaction this seam exists to stay
// out of). A hook that must fire EXACTLY ONCE per real settlement (the
// pipeline-run webhook events, in internal/application/pipelineruns) is
// responsible for its own idempotency — see that package's ClaimForEvent.
type AfterSettleHook func(ctx context.Context, proposal SettlementProposal, receipt SettlementReceipt)

// SettlementOption configures a SettlementService built by
// NewSettlementService.
type SettlementOption func(*SettlementService)

// WithAfterSettleHooks registers one or more AfterSettleHook values. Each
// fires on its OWN goroutine (mirroring internal/events.Publisher.Emit's own
// "a Sink's work must not block the producer" contract) and any panic inside
// one is recovered and logged, never propagated — see PrepareSettlement's
// call site below for why that is a correctness requirement, not a
// nicety: a worker's settlement RPC is the claim-fence protocol's own
// termination step, and a hook added AFTER that protocol was designed and
// proven must not be able to make it fail, hang, or return a wrong receipt.
func WithAfterSettleHooks(hooks ...AfterSettleHook) SettlementOption {
	return func(s *SettlementService) {
		s.hooks = append(s.hooks, hooks...)
	}
}

type SettlementService struct {
	repository SettlementRepository
	hooks      []AfterSettleHook
}

func NewSettlementService(repository SettlementRepository, opts ...SettlementOption) (*SettlementService, error) {
	if repository == nil {
		return nil, errors.New("settlement repository is required")
	}
	s := &SettlementService{repository: repository}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

func (s *SettlementService) PrepareSettlement(ctx context.Context, proposal SettlementProposal) (SettlementReceipt, error) {
	if err := proposal.Validate(); err != nil {
		return SettlementReceipt{}, err
	}
	receipt, err := s.repository.PrepareSettlement(ctx, proposal)
	if err != nil {
		return SettlementReceipt{}, fmt.Errorf("prepare execution settlement: %w", err)
	}
	if receipt.ID == "" || !receipt.Outcome.valid() || receipt.Outcome != proposal.Outcome {
		return SettlementReceipt{}, errors.New("settlement repository returned an invalid receipt")
	}
	// The repository call above already returned successfully — whatever
	// transaction it ran has committed. Everything from here on is OUTSIDE
	// that transaction and outside this method's own critical path: each
	// hook runs detached (context.WithoutCancel, the same reasoning
	// events.Publisher.Emit documents — a worker's RPC context is cancelled
	// the instant this method returns, and a hook's own work, e.g. an
	// outbound webhook delivery, must not be cut off by that) and on its own
	// goroutine, with a recovered panic, so nothing a hook does — slow,
	// wrong, or crashing — can change the receipt this method is about to
	// return or delay the caller from receiving it.
	for _, hook := range s.hooks {
		if hook == nil {
			continue
		}
		hookCtx := context.WithoutCancel(ctx)
		go runAfterSettleHook(hookCtx, hook, proposal, receipt)
	}
	return receipt, nil
}

// runAfterSettleHook isolates ONE hook invocation so a panic in it cannot
// crash the process (a bare `go hook(...)` would) and cannot reach any other
// hook or the caller of PrepareSettlement.
func runAfterSettleHook(ctx context.Context, hook AfterSettleHook, proposal SettlementProposal, receipt SettlementReceipt) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("execution: AfterSettle hook panicked",
				"recovered", r, "execution_id", proposal.Fence.ExecutionID, "generation", proposal.Fence.Generation)
		}
	}()
	hook(ctx, proposal, receipt)
}
