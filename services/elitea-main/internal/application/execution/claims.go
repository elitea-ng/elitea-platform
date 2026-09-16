package execution

import (
	"context"
	"errors"
	"fmt"
	"time"

	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

var (
	ErrInvalidClaim               = errors.New("invalid execution claim")
	ErrClaimDependencyUnavailable = errors.New("execution claim dependency is unavailable")
)

type ClaimRequest struct {
	AgentModelCheckpointRecovery bool
	CommandID                    string
	OutboxID                     string
	ExecutionID                  string
	Generation                   uint64
	CapabilityID                 string
	SignedEnvelopeDigest         runtimedomain.Digest
	WorkloadIdentity             string
	WorkloadSessionID            string
	ProducerID                   string
}

// ClaimAbortDisposition records why a live, fully fenced claim was released
// before any business work could start. Retry is bounded by the control plane;
// permanent and exhausted failures quarantine the durable job for explicit
// reconciliation instead of re-executing it indefinitely.
type ClaimAbortDisposition string

const (
	ClaimAbortInputResolutionRetry     ClaimAbortDisposition = "INPUT_RESOLUTION_RETRY"
	ClaimAbortInputResolutionExhausted ClaimAbortDisposition = "INPUT_RESOLUTION_EXHAUSTED"
	ClaimAbortInputManifestInvalid     ClaimAbortDisposition = "INPUT_MANIFEST_INVALID"
)

func (d ClaimAbortDisposition) valid() bool {
	switch d {
	case ClaimAbortInputResolutionRetry, ClaimAbortInputResolutionExhausted, ClaimAbortInputManifestInvalid:
		return true
	default:
		return false
	}
}

type ClaimDisposition string

const (
	ClaimRecoverAgentModelCheckpoint     ClaimDisposition = "RECOVER_AGENT_MODEL_CHECKPOINT"
	ClaimAccepted                        ClaimDisposition = "ACCEPTED"
	ClaimRecoverTerminalACK              ClaimDisposition = "RECOVER_TERMINAL_ACK"
	ClaimRecoverSettlement               ClaimDisposition = "RECOVER_SETTLEMENT"
	ClaimSettledACK                      ClaimDisposition = "SETTLED_ACK"
	ClaimObsoleteACK                     ClaimDisposition = "OBSOLETE_ACK"
	ClaimActiveLeaseNoACK                ClaimDisposition = "ACTIVE_LEASE_NOACK"
	ClaimRetryLaterNoACK                 ClaimDisposition = "RETRY_LATER_NOACK"
	ClaimRetiredACK                      ClaimDisposition = "RETIRED_ACK"
	ClaimRecoverRunningNoACK             ClaimDisposition = "RECOVER_RUNNING_NOACK"
	ClaimRecoverAmbiguousInvocationNoACK ClaimDisposition = "RECOVER_AMBIGUOUS_INVOCATION_NOACK"
)

func (d ClaimDisposition) valid() bool {
	switch d {
	case ClaimRecoverAgentModelCheckpoint, ClaimAccepted, ClaimRecoverTerminalACK, ClaimRecoverSettlement, ClaimSettledACK, ClaimObsoleteACK, ClaimActiveLeaseNoACK, ClaimRetryLaterNoACK, ClaimRetiredACK, ClaimRecoverRunningNoACK, ClaimRecoverAmbiguousInvocationNoACK:
		return true
	default:
		return false
	}
}

type RetirementReason string

const (
	RetirementDeadlineExceeded  RetirementReason = "DEADLINE_EXCEEDED"
	DeadlineExceededSafeMessage string           = "The execution deadline was exceeded before worker authority was granted."
)

func (r RetirementReason) valid() bool {
	return r == RetirementDeadlineExceeded
}

type SettlementRecovery struct {
	Proposal *SettlementProposal
	Receipt  *SettlementReceipt
}

type ClaimDecision struct {
	Lease runtimedomain.ActiveLease
	// LeaseObservedAt is the state-owner clock instant used to decide whether
	// Lease was live. PostgreSQL repositories must author it in the same
	// statement that creates, loads, or renews the lease; application-host time
	// is not authority.
	LeaseObservedAt time.Time
	// DesiredState is populated only for a no-lease disposition. A retry may
	// expose any valid desired state; OBSOLETE_ACK is limited to a durably
	// cancelled execution that never received worker authority. RETIRED_ACK is
	// distinct and requires an exact durable retirement reason.
	DesiredState          runtimedomain.DesiredState
	Disposition           ClaimDisposition
	RetirementReason      RetirementReason
	ClaimHandoffWatermark uint64
	SettlementRecovery    *SettlementRecovery
}

func (d ClaimDecision) validate(request ClaimRequest, leaseTTL ClaimLeaseTTLMillis) error {
	if !d.Disposition.valid() {
		return ErrInvalidClaim
	}
	if d.Lease == (runtimedomain.ActiveLease{}) {
		if !d.LeaseObservedAt.IsZero() || d.ClaimHandoffWatermark != 0 || d.SettlementRecovery != nil {
			return ErrInvalidClaim
		}
		switch d.Disposition {
		case ClaimRetryLaterNoACK:
			if d.DesiredState.Valid() && d.RetirementReason == "" {
				return nil
			}
		case ClaimObsoleteACK:
			if d.DesiredState == runtimedomain.DesiredCancelled && d.RetirementReason == "" {
				return nil
			}
		case ClaimRetiredACK:
			if d.DesiredState.Valid() && d.RetirementReason.valid() {
				return nil
			}
		}
		return ErrInvalidClaim
	}
	if d.DesiredState != "" || d.RetirementReason != "" {
		return ErrInvalidClaim
	}
	lease := d.Lease
	if d.LeaseObservedAt.IsZero() {
		return ErrInvalidClaim
	}
	if lease.Fence.CommandID != request.CommandID || lease.Fence.ExecutionID != request.ExecutionID || lease.Fence.Generation != request.Generation || lease.Fence.WorkloadIdentity != request.WorkloadIdentity || lease.Fence.WorkloadSessionID != request.WorkloadSessionID || lease.Fence.ProducerID != request.ProducerID {
		return fmt.Errorf("%w: repository returned mismatched lease", ErrInvalidClaim)
	}
	if err := lease.Verify(d.LeaseObservedAt.UTC(), lease.Fence); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidClaim, err)
	}
	if (d.Disposition == ClaimAccepted || d.Disposition == ClaimRecoverAgentModelCheckpoint) && lease.ExpiresAt.Sub(d.LeaseObservedAt) != leaseTTL.Duration() {
		return fmt.Errorf("%w: repository returned a lease outside the selected TTL", ErrInvalidClaim)
	}
	switch d.Disposition {
	case ClaimRecoverTerminalACK:
		if d.SettlementRecovery == nil || d.SettlementRecovery.Proposal == nil || d.SettlementRecovery.Receipt != nil {
			return ErrInvalidClaim
		}
		if err := d.SettlementRecovery.Proposal.Validate(); err != nil || d.SettlementRecovery.Proposal.Fence != lease.Fence {
			return ErrInvalidClaim
		}
	case ClaimRecoverSettlement:
		if d.SettlementRecovery == nil || d.SettlementRecovery.Proposal != nil || d.SettlementRecovery.Receipt == nil || d.SettlementRecovery.Receipt.ID == "" || !d.SettlementRecovery.Receipt.Outcome.valid() {
			return ErrInvalidClaim
		}
	default:
		if d.SettlementRecovery != nil {
			return ErrInvalidClaim
		}
	}
	return nil
}

type ClaimRepository interface {
	ClaimValidation(ctx context.Context, request ClaimRequest, leaseTTL ClaimLeaseTTLMillis) (ClaimDecision, error)
	CurrentLease(ctx context.Context, executionID string, generation uint64) (runtimedomain.ActiveLease, time.Time, error)
	RenewLease(ctx context.Context, fence runtimedomain.Fence, leaseTTL ClaimLeaseTTLMillis) (runtimedomain.ActiveLease, time.Time, error)
	ReleaseClaim(ctx context.Context, fence runtimedomain.Fence) error
	AbortClaim(ctx context.Context, fence runtimedomain.Fence, disposition ClaimAbortDisposition) error
	DesiredState(ctx context.Context, executionID string, generation uint64) (runtimedomain.DesiredState, error)
}

type BeginExecutionDisposition string

const (
	BeginExecutionStartedNow     BeginExecutionDisposition = "STARTED_NOW"
	BeginExecutionAlreadyStarted BeginExecutionDisposition = "ALREADY_STARTED"
)

func (d BeginExecutionDisposition) valid() bool {
	return d == BeginExecutionStartedNow || d == BeginExecutionAlreadyStarted
}

// BeginExecutionRepository owns the exact durable CLAIMED -> RUNNING
// transition. It is kept separate from ClaimRepository so read-only claim
// doubles do not accidentally acquire execution-start authority.
type BeginExecutionRepository interface {
	BeginExecution(ctx context.Context, fence runtimedomain.Fence) (BeginExecutionDisposition, error)
}

type AuthorizeInvocationDisposition string

const (
	AuthorizeInvocationNow     AuthorizeInvocationDisposition = "AUTHORIZED_NOW"
	AuthorizeInvocationAlready AuthorizeInvocationDisposition = "ALREADY_AUTHORIZED"
)

func (d AuthorizeInvocationDisposition) valid() bool {
	return d == AuthorizeInvocationNow || d == AuthorizeInvocationAlready
}

// InvocationAuthorizationRepository owns the final durable transition before
// an effecting synchronous SDK callable may be submitted. Only AUTHORIZED_NOW
// grants submission authority; ALREADY_AUTHORIZED is an unknown-outcome fence.
type InvocationAuthorizationRepository interface {
	AuthorizeInvocation(ctx context.Context, fence runtimedomain.Fence) (AuthorizeInvocationDisposition, error)
}

// ClaimLeaseTTLMillis is the bounded, whole-millisecond lease policy passed to
// the state owner. It deliberately cannot represent an application-authored
// absolute expiry.
type ClaimLeaseTTLMillis uint32

const MaxClaimLeaseTTLMillis ClaimLeaseTTLMillis = 30_000

func (ttl ClaimLeaseTTLMillis) Valid() bool {
	return ttl > 0 && ttl <= MaxClaimLeaseTTLMillis
}

func (ttl ClaimLeaseTTLMillis) Duration() time.Duration {
	return time.Duration(ttl) * time.Millisecond
}

type ClaimService struct {
	repository ClaimRepository
	leaseTTL   ClaimLeaseTTLMillis
}

func NewClaimService(repository ClaimRepository, applicationNow func() time.Time, leaseTTL time.Duration) (*ClaimService, error) {
	if repository == nil {
		return nil, errors.New("claim repository is required")
	}
	// Retain the clock parameter while existing callers migrate, but never use
	// it for lease authority. PostgreSQL is the authoritative clock.
	_ = applicationNow
	if leaseTTL <= 0 || leaseTTL%time.Millisecond != 0 {
		return nil, errors.New("lease TTL must be a positive whole number of milliseconds")
	}
	leaseMillis := ClaimLeaseTTLMillis(leaseTTL / time.Millisecond)
	if !leaseMillis.Valid() || leaseMillis.Duration() != leaseTTL {
		return nil, fmt.Errorf("lease TTL must not exceed %d milliseconds", MaxClaimLeaseTTLMillis)
	}
	return &ClaimService{repository: repository, leaseTTL: leaseMillis}, nil
}

func (s *ClaimService) Claim(ctx context.Context, request ClaimRequest) (ClaimDecision, error) {
	if request.CommandID == "" || request.OutboxID == "" || request.ExecutionID == "" || request.Generation == 0 || !claimCapabilityAllowed(request.CapabilityID) || request.SignedEnvelopeDigest.IsZero() || request.WorkloadIdentity == "" || request.WorkloadSessionID == "" || request.ProducerID == "" {
		return ClaimDecision{}, ErrInvalidClaim
	}
	decision, err := s.repository.ClaimValidation(ctx, request, s.leaseTTL)
	if err != nil {
		return ClaimDecision{}, fmt.Errorf("claim execution command: %w", err)
	}
	if err := decision.validate(request, s.leaseTTL); err != nil {
		return ClaimDecision{}, err
	}
	return decision, nil
}

func (s *ClaimService) BeginExecution(ctx context.Context, fence runtimedomain.Fence) (BeginExecutionDisposition, error) {
	if err := fence.Validate(); err != nil {
		return "", err
	}
	repository, ok := s.repository.(BeginExecutionRepository)
	if !ok {
		return "", ErrClaimDependencyUnavailable
	}
	disposition, err := repository.BeginExecution(ctx, fence)
	if err != nil {
		return "", fmt.Errorf("begin execution: %w", err)
	}
	if !disposition.valid() {
		return "", ErrInvalidClaim
	}
	return disposition, nil
}

func (s *ClaimService) AuthorizeInvocation(ctx context.Context, fence runtimedomain.Fence) (AuthorizeInvocationDisposition, error) {
	if err := fence.Validate(); err != nil {
		return "", err
	}
	repository, ok := s.repository.(InvocationAuthorizationRepository)
	if !ok {
		return "", ErrClaimDependencyUnavailable
	}
	disposition, err := repository.AuthorizeInvocation(ctx, fence)
	if err != nil {
		return "", fmt.Errorf("authorize execution invocation: %w", err)
	}
	if !disposition.valid() {
		return "", ErrInvalidClaim
	}
	return disposition, nil
}

func claimCapabilityAllowed(capabilityID string) bool {
	return executiondomain.SupportedCapability(capabilityID)
}

func (s *ClaimService) VerifyActive(ctx context.Context, fence runtimedomain.Fence) error {
	if err := fence.Validate(); err != nil {
		return err
	}
	lease, observedAt, err := s.repository.CurrentLease(ctx, fence.ExecutionID, fence.Generation)
	if err != nil {
		return fmt.Errorf("load current execution lease: %w", err)
	}
	if observedAt.IsZero() {
		return ErrInvalidClaim
	}
	if err := lease.Verify(observedAt.UTC(), fence); err != nil {
		return err
	}
	return nil
}

func (s *ClaimService) Renew(ctx context.Context, fence runtimedomain.Fence) (runtimedomain.ActiveLease, error) {
	if err := s.VerifyActive(ctx, fence); err != nil {
		return runtimedomain.ActiveLease{}, err
	}
	renewed, observedAt, err := s.repository.RenewLease(ctx, fence, s.leaseTTL)
	if err != nil {
		return runtimedomain.ActiveLease{}, fmt.Errorf("renew execution lease: %w", err)
	}
	if observedAt.IsZero() {
		return runtimedomain.ActiveLease{}, ErrInvalidClaim
	}
	if err := renewed.Verify(observedAt.UTC(), fence); err != nil {
		return runtimedomain.ActiveLease{}, fmt.Errorf("%w: repository changed renewed fence", ErrInvalidClaim)
	}
	if renewed.ExpiresAt.Sub(observedAt) != s.leaseTTL.Duration() {
		return runtimedomain.ActiveLease{}, fmt.Errorf("%w: repository returned a renewal outside the selected TTL", ErrInvalidClaim)
	}
	return renewed, nil
}

func (s *ClaimService) Release(ctx context.Context, fence runtimedomain.Fence) error {
	if err := s.VerifyActive(ctx, fence); err != nil {
		return err
	}
	if err := s.repository.ReleaseClaim(ctx, fence); err != nil {
		return fmt.Errorf("release execution claim: %w", err)
	}
	return nil
}

func (s *ClaimService) Abort(ctx context.Context, fence runtimedomain.Fence, disposition ClaimAbortDisposition) error {
	if err := fence.Validate(); err != nil {
		return err
	}
	if !disposition.valid() {
		return ErrInvalidClaim
	}
	if err := s.repository.AbortClaim(ctx, fence, disposition); err != nil {
		return fmt.Errorf("abort execution claim: %w", err)
	}
	return nil
}

func (s *ClaimService) ObserveDesiredState(ctx context.Context, fence runtimedomain.Fence) (runtimedomain.DesiredState, error) {
	if err := s.VerifyActive(ctx, fence); err != nil {
		return "", err
	}
	state, err := s.repository.DesiredState(ctx, fence.ExecutionID, fence.Generation)
	if err != nil {
		return "", fmt.Errorf("observe desired execution state: %w", err)
	}
	if !state.Valid() {
		return "", errors.New("repository returned invalid desired state")
	}
	return state, nil
}
