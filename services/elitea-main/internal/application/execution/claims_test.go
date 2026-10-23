package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type memoryClaimRepository struct {
	lease            runtimedomain.ActiveLease
	abortDisposition ClaimAbortDisposition
	changeRenewEpoch bool
	now              func() time.Time
}

func TestClaimServiceRequiresBoundedWholeMillisecondTTL(t *testing.T) {
	repository := &memoryClaimRepository{now: time.Now}
	for _, leaseTTL := range []time.Duration{
		-time.Millisecond,
		0,
		time.Nanosecond,
		30*time.Second + time.Millisecond,
	} {
		if _, err := NewClaimService(repository, time.Now, leaseTTL); err == nil {
			t.Fatalf("unsafe lease TTL was accepted: %s", leaseTTL)
		}
	}
	for _, leaseTTL := range []time.Duration{time.Millisecond, 30 * time.Second} {
		service, err := NewClaimService(repository, time.Now, leaseTTL)
		if err != nil || service.leaseTTL.Duration() != leaseTTL {
			t.Fatalf("bounded whole-millisecond TTL was rejected: ttl=%s service=%+v err=%v", leaseTTL, service, err)
		}
	}
}

func TestClaimServiceAcceptsOnlyImplementedCapabilities(t *testing.T) {
	for _, capabilityID := range []string{
		executiondomain.ConfigurationValidationCapability,
		executiondomain.IndexIngestCapability,
		executiondomain.AgentApplicationCapability,
		executiondomain.AgentAdhocCapability,
	} {
		repository := &memoryClaimRepository{now: time.Now}
		service, err := NewClaimService(repository, time.Now, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		request := ClaimRequest{
			CommandID:            "command-1",
			OutboxID:             "outbox-1",
			ExecutionID:          "execution-1",
			Generation:           1,
			CapabilityID:         capabilityID,
			SignedEnvelopeDigest: runtimedomain.SHA256([]byte("signed-envelope")),
			WorkloadIdentity:     "spiffe://elitea.test/workload/worker-1",
			WorkloadSessionID:    "session-1",
			ProducerID:           "producer-1",
		}
		if _, err := service.Claim(context.Background(), request); err != nil {
			t.Fatalf("implemented capability %q rejected: %v", capabilityID, err)
		}
	}

	repository := &memoryClaimRepository{now: time.Now}
	service, err := NewClaimService(repository, time.Now, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Claim(context.Background(), ClaimRequest{
		CommandID:            "command-1",
		OutboxID:             "outbox-1",
		ExecutionID:          "execution-1",
		Generation:           1,
		CapabilityID:         "unknown.capability.v1",
		SignedEnvelopeDigest: runtimedomain.SHA256([]byte("signed-envelope")),
		WorkloadIdentity:     "spiffe://elitea.test/workload/worker-1",
		WorkloadSessionID:    "session-1",
		ProducerID:           "producer-1",
	}); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("unknown capability error=%v", err)
	}
}

func TestNoLeaseObsoleteDecisionIsLimitedToDurableCancellation(t *testing.T) {
	request := ClaimRequest{
		CommandID:            "command-1",
		OutboxID:             "outbox-1",
		ExecutionID:          "execution-1",
		Generation:           1,
		CapabilityID:         executiondomain.ConfigurationValidationCapability,
		SignedEnvelopeDigest: runtimedomain.SHA256([]byte("signed-envelope")),
		WorkloadIdentity:     "spiffe://elitea.test/workload/worker-1",
		WorkloadSessionID:    "session-1",
		ProducerID:           "producer-1",
	}
	if err := (ClaimDecision{
		Disposition:  ClaimObsoleteACK,
		DesiredState: runtimedomain.DesiredCancelled,
	}).validate(request, MaxClaimLeaseTTLMillis); err != nil {
		t.Fatalf("durable no-lease cancellation was rejected: %v", err)
	}

	invalid := []ClaimDecision{
		{Disposition: ClaimObsoleteACK, DesiredState: runtimedomain.DesiredRunning},
		{Disposition: ClaimObsoleteACK, DesiredState: runtimedomain.DesiredDraining},
		{Disposition: ClaimObsoleteACK},
		{Disposition: ClaimObsoleteACK, DesiredState: runtimedomain.DesiredCancelled, ClaimHandoffWatermark: 1},
		{Disposition: ClaimObsoleteACK, DesiredState: runtimedomain.DesiredCancelled, SettlementRecovery: &SettlementRecovery{}},
	}
	for _, decision := range invalid {
		if err := decision.validate(request, MaxClaimLeaseTTLMillis); !errors.Is(err, ErrInvalidClaim) {
			t.Fatalf("unsafe no-lease obsolete decision was accepted: %+v err=%v", decision, err)
		}
	}
}

func TestNoLeaseRetiredDecisionRequiresDeadlineReason(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	request := ClaimRequest{
		CommandID:            "command-1",
		OutboxID:             "outbox-1",
		ExecutionID:          "execution-1",
		Generation:           1,
		CapabilityID:         executiondomain.ConfigurationValidationCapability,
		SignedEnvelopeDigest: runtimedomain.SHA256([]byte("signed-envelope")),
		WorkloadIdentity:     "spiffe://elitea.test/workload/worker-1",
		WorkloadSessionID:    "session-1",
		ProducerID:           "producer-1",
	}
	valid := ClaimDecision{
		Disposition:      ClaimRetiredACK,
		DesiredState:     runtimedomain.DesiredRunning,
		RetirementReason: RetirementDeadlineExceeded,
	}
	if err := valid.validate(request, MaxClaimLeaseTTLMillis); err != nil {
		t.Fatalf("durable deadline retirement was rejected: %v", err)
	}

	lease := runtimedomain.ActiveLease{
		ClaimID:      "claim-1",
		DesiredState: runtimedomain.DesiredRunning,
		ExpiresAt:    now.Add(time.Minute),
		Fence: runtimedomain.Fence{
			CommandID:         request.CommandID,
			ExecutionID:       request.ExecutionID,
			Generation:        request.Generation,
			WorkloadIdentity:  request.WorkloadIdentity,
			WorkloadSessionID: request.WorkloadSessionID,
			ProducerID:        request.ProducerID,
			ClaimAttempt:      1,
			LeaseEpoch:        1,
			Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("token"))),
		},
	}
	invalid := []ClaimDecision{
		{Disposition: ClaimRetiredACK, DesiredState: runtimedomain.DesiredRunning},
		{Disposition: ClaimRetiredACK, DesiredState: runtimedomain.DesiredRunning, RetirementReason: "UNKNOWN"},
		{Disposition: ClaimRetiredACK, RetirementReason: RetirementDeadlineExceeded},
		{Disposition: ClaimRetiredACK, DesiredState: runtimedomain.DesiredRunning, RetirementReason: RetirementDeadlineExceeded, ClaimHandoffWatermark: 1},
		{Disposition: ClaimRetiredACK, DesiredState: runtimedomain.DesiredRunning, RetirementReason: RetirementDeadlineExceeded, SettlementRecovery: &SettlementRecovery{}},
		{Lease: lease, Disposition: ClaimRetiredACK, RetirementReason: RetirementDeadlineExceeded},
		{Disposition: ClaimRetryLaterNoACK, DesiredState: runtimedomain.DesiredRunning, RetirementReason: RetirementDeadlineExceeded},
		{Disposition: ClaimObsoleteACK, DesiredState: runtimedomain.DesiredCancelled, RetirementReason: RetirementDeadlineExceeded},
	}
	for _, decision := range invalid {
		if err := decision.validate(request, MaxClaimLeaseTTLMillis); !errors.Is(err, ErrInvalidClaim) {
			t.Fatalf("unsafe retirement decision was accepted: %+v err=%v", decision, err)
		}
	}
}

func (r *memoryClaimRepository) ClaimValidation(_ context.Context, request ClaimRequest, leaseTTL ClaimLeaseTTLMillis) (ClaimDecision, error) {
	observedAt := r.now().UTC()
	if r.lease.Fence.ExecutionID == request.ExecutionID && !r.lease.ExpiresAt.IsZero() {
		return ClaimDecision{Lease: r.lease, LeaseObservedAt: observedAt, Disposition: ClaimAccepted}, nil
	}
	r.lease = runtimedomain.ActiveLease{
		ClaimID:      "claim-1",
		DesiredState: runtimedomain.DesiredRunning,
		Fence: runtimedomain.Fence{
			CommandID:         request.CommandID,
			ExecutionID:       request.ExecutionID,
			Generation:        request.Generation,
			WorkloadIdentity:  request.WorkloadIdentity,
			WorkloadSessionID: request.WorkloadSessionID,
			ProducerID:        request.ProducerID,
			ClaimAttempt:      1,
			LeaseEpoch:        1,
			Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("unpredictable-test-token"))),
		},
		ExpiresAt: observedAt.Add(leaseTTL.Duration()),
	}
	return ClaimDecision{Lease: r.lease, LeaseObservedAt: observedAt, Disposition: ClaimAccepted}, nil
}

func (r *memoryClaimRepository) CurrentLease(_ context.Context, executionID string, generation uint64) (runtimedomain.ActiveLease, time.Time, error) {
	if r.lease.Fence.ExecutionID != executionID || r.lease.Fence.Generation != generation {
		return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrStaleFence
	}
	observedAt := r.now().UTC()
	if !observedAt.Before(r.lease.ExpiresAt) {
		return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrLeaseExpired
	}
	return r.lease, observedAt, nil
}

func (r *memoryClaimRepository) RenewLease(_ context.Context, fence runtimedomain.Fence, leaseTTL ClaimLeaseTTLMillis) (runtimedomain.ActiveLease, time.Time, error) {
	if r.lease.Fence != fence {
		return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrStaleFence
	}
	observedAt := r.now().UTC()
	if !observedAt.Before(r.lease.ExpiresAt) {
		return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrLeaseExpired
	}
	r.lease.ExpiresAt = observedAt.Add(leaseTTL.Duration())
	if r.changeRenewEpoch {
		r.lease.Fence.LeaseEpoch++
	}
	return r.lease, observedAt, nil
}

func (r *memoryClaimRepository) ReleaseClaim(_ context.Context, fence runtimedomain.Fence) error {
	if r.lease.Fence != fence {
		return runtimedomain.ErrStaleFence
	}
	r.lease = runtimedomain.ActiveLease{}
	return nil
}

func (r *memoryClaimRepository) AbortClaim(_ context.Context, fence runtimedomain.Fence, disposition ClaimAbortDisposition) error {
	if r.lease.Fence != fence {
		return runtimedomain.ErrStaleFence
	}
	r.abortDisposition = disposition
	r.lease = runtimedomain.ActiveLease{}
	return nil
}

func (r *memoryClaimRepository) DesiredState(_ context.Context, executionID string, generation uint64) (runtimedomain.DesiredState, error) {
	if r.lease.Fence.ExecutionID != executionID || r.lease.Fence.Generation != generation {
		return "", runtimedomain.ErrStaleFence
	}
	return r.lease.DesiredState, nil
}

func TestClaimVerifierRejectsStaleFenceAndExpiredLease(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	repository := &memoryClaimRepository{now: func() time.Time { return now }}
	service, err := NewClaimService(repository, func() time.Time { return now }, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := service.Claim(context.Background(), ClaimRequest{
		CommandID:            "command-1",
		OutboxID:             "outbox-1",
		ExecutionID:          "execution-1",
		Generation:           1,
		CapabilityID:         executiondomain.ConfigurationValidationCapability,
		SignedEnvelopeDigest: runtimedomain.SHA256([]byte("signed-envelope")),
		WorkloadIdentity:     "spiffe://elitea.test/workload/worker-1",
		WorkloadSessionID:    "workload-1",
		ProducerID:           "worker-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := decision.Lease
	if err := service.VerifyActive(context.Background(), lease.Fence); err != nil {
		t.Fatalf("current fence rejected: %v", err)
	}

	stale := lease.Fence
	stale.LeaseEpoch++
	if err := service.VerifyActive(context.Background(), stale); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("expected stale fence, got %v", err)
	}
	wrongProducer := lease.Fence
	wrongProducer.ProducerID = "other-worker"
	if err := service.VerifyActive(context.Background(), wrongProducer); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("expected producer mismatch to be stale, got %v", err)
	}
	wrongToken := lease.Fence
	wrongToken.Token = runtimedomain.FenceToken(runtimedomain.SHA256([]byte("wrong-token")))
	if err := service.VerifyActive(context.Background(), wrongToken); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("expected token mismatch to be stale, got %v", err)
	}
	wrongGeneration := lease.Fence
	wrongGeneration.Generation++
	if err := service.VerifyActive(context.Background(), wrongGeneration); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("expected generation mismatch to be stale, got %v", err)
	}

	now = now.Add(30 * time.Second)
	if err := service.VerifyActive(context.Background(), lease.Fence); !errors.Is(err, runtimedomain.ErrLeaseExpired) {
		t.Fatalf("expected exact-expiry rejection, got %v", err)
	}
}

func TestClaimAbortRequiresExactFenceAndRecordsDisposition(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	repository := &memoryClaimRepository{now: func() time.Time { return now }}
	service, err := NewClaimService(repository, func() time.Time { return now }, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := service.Claim(context.Background(), ClaimRequest{
		CommandID:            "command-1",
		OutboxID:             "outbox-1",
		ExecutionID:          "execution-1",
		Generation:           1,
		CapabilityID:         executiondomain.ConfigurationValidationCapability,
		SignedEnvelopeDigest: runtimedomain.SHA256([]byte("signed-envelope")),
		WorkloadIdentity:     "spiffe://elitea.test/workload/worker-1",
		WorkloadSessionID:    "workload-1",
		ProducerID:           "worker-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := decision.Lease.Fence
	stale.Token[0] ^= 0xff
	if err := service.Abort(context.Background(), stale, ClaimAbortInputManifestInvalid); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("expected stale abort rejection, got %v", err)
	}
	if err := service.Abort(context.Background(), decision.Lease.Fence, ClaimAbortInputManifestInvalid); err != nil {
		t.Fatal(err)
	}
	if repository.abortDisposition != ClaimAbortInputManifestInvalid || !repository.lease.Fence.Token.IsZero() {
		t.Fatalf("abort was not recorded and released: disposition=%q lease=%+v", repository.abortDisposition, repository.lease)
	}
}

func TestClaimRenewRejectsRepositoryFenceRotation(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	repository := &memoryClaimRepository{now: func() time.Time { return now }}
	service, err := NewClaimService(repository, func() time.Time { return now }, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := service.Claim(context.Background(), ClaimRequest{
		CommandID:            "command-1",
		OutboxID:             "outbox-1",
		ExecutionID:          "execution-1",
		Generation:           1,
		CapabilityID:         executiondomain.ConfigurationValidationCapability,
		SignedEnvelopeDigest: runtimedomain.SHA256([]byte("signed-envelope")),
		WorkloadIdentity:     "spiffe://elitea.test/workload/worker-1",
		WorkloadSessionID:    "workload-1",
		ProducerID:           "worker-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	repository.changeRenewEpoch = true
	if _, err := service.Renew(context.Background(), decision.Lease.Fence); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("renewal accepted a replacement epoch that the wire response cannot carry: %v", err)
	}
}
