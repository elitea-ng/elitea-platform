package control

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

type WorkloadAuthorizer interface {
	// AuthorizeWorkload derives the authenticated peer identity from ctx
	// (mTLS/SPIFFE or an explicitly staged transport credential) and compares
	// it with the persisted workload session/producer. The two string arguments
	// are caller-supplied references, not authentication by themselves.
	AuthorizeWorkload(ctx context.Context, workloadSessionID, producerID string) (workloadIdentity string, err error)
}

type ClaimController interface {
	Claim(ctx context.Context, request executionapp.ClaimRequest) (executionapp.ClaimDecision, error)
	BeginExecution(ctx context.Context, fence runtimedomain.Fence) (executionapp.BeginExecutionDisposition, error)
	AuthorizeInvocation(ctx context.Context, fence runtimedomain.Fence) (executionapp.AuthorizeInvocationDisposition, error)
	Abort(ctx context.Context, fence runtimedomain.Fence, disposition executionapp.ClaimAbortDisposition) error
	Renew(ctx context.Context, fence runtimedomain.Fence) (runtimedomain.ActiveLease, error)
	VerifyActive(ctx context.Context, fence runtimedomain.Fence) error
	ObserveDesiredState(ctx context.Context, fence runtimedomain.Fence) (runtimedomain.DesiredState, error)
}

const (
	maxInputResolutionClaimAttempts = 3
	claimAbortCleanupTimeout        = 5 * time.Second
)

type SettlementController interface {
	PrepareSettlement(ctx context.Context, proposal executionapp.SettlementProposal) (executionapp.SettlementReceipt, error)
}

// ClaimInputResolver returns only the immutable, bounded manifest after an
// accepted claim. It never returns settings/content bytes or a bearer grant.
type ClaimInputResolver interface {
	ResolveClaimInput(ctx context.Context, fence runtimedomain.Fence, reference *runtimev1.ExecutionInputBundleReferenceV1) (*runtimev1.ExecutionInputBundleV1, error)
}

type ServerConfig struct {
	MaxInputManifestBytes int
	MaxInputEntries       int
	MaxInputContentBytes  uint64
	MaxStringBytes        int
}

type Server struct {
	runtimev1.UnimplementedRuntimeControlServiceServer

	config      ServerConfig
	authorizer  WorkloadAuthorizer
	verifier    CommandVerifier
	claims      ClaimController
	inputs      ClaimInputResolver
	settlements SettlementController
}

func NewServer(config ServerConfig, authorizer WorkloadAuthorizer, verifier CommandVerifier, claims ClaimController, inputs ClaimInputResolver, settlements SettlementController) (*Server, error) {
	if authorizer == nil || verifier == nil || claims == nil || inputs == nil || settlements == nil {
		return nil, errors.New("control authorizer, verifier, claims, input resolver and settlement controller are required")
	}
	if config.MaxInputManifestBytes <= 0 || config.MaxInputEntries <= 0 || config.MaxInputContentBytes == 0 || config.MaxStringBytes <= 0 {
		return nil, errors.New("control input limits must be positive")
	}
	return &Server{config: config, authorizer: authorizer, verifier: verifier, claims: claims, inputs: inputs, settlements: settlements}, nil
}

func (s *Server) ClaimCommand(ctx context.Context, request *runtimev1.ClaimCommandRequestV1) (*runtimev1.ClaimCommandResponseV1, error) {
	if request == nil || request.GetWorkloadSessionId() == "" || request.GetProducerId() == "" || hasUnknownFields(request.ProtoReflect()) {
		return claimRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The claim request is malformed.", false), nil
	}
	workloadIdentity, err := s.authorizer.AuthorizeWorkload(ctx, request.GetWorkloadSessionId(), request.GetProducerId())
	if err != nil || workloadIdentity == "" {
		return claimRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false), nil
	}
	command, err := s.verifier.Verify(ctx, request.GetSignedCommand())
	if err != nil {
		return claimRejectionFor(err), nil
	}
	canonicalEnvelope, err := proto.MarshalOptions{Deterministic: true}.Marshal(request.GetSignedCommand())
	if err != nil {
		return claimRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The signed worker command is malformed.", false), nil
	}

	decision, err := s.claims.Claim(ctx, executionapp.ClaimRequest{
		CommandID:            command.GetCommandId(),
		OutboxID:             command.GetIdempotencyKey(),
		ExecutionID:          command.GetExecutionId(),
		Generation:           command.GetGeneration(),
		CapabilityID:         command.GetCapabilityId(),
		SignedEnvelopeDigest: runtimedomain.SHA256(canonicalEnvelope),
		WorkloadIdentity:     workloadIdentity,
		WorkloadSessionID:    request.GetWorkloadSessionId(),
		ProducerID:           request.GetProducerId(),
	})
	if err != nil {
		return claimRejectionFor(err), nil
	}
	lease := decision.Lease
	desired := lease.DesiredState
	if lease == (runtimedomain.ActiveLease{}) {
		desired = decision.DesiredState
	}
	desiredState, err := desiredStateProto(desired)
	if err != nil {
		return claimRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The execution desired state is unavailable.", false), nil
	}
	identity := identityProto(command)
	disposition := decision.Disposition
	if disposition == executionapp.ClaimAccepted && lease.DesiredState != runtimedomain.DesiredRunning {
		// OBSOLETE_ACK is reserved for an explicit durable cancellation
		// finalizer. A non-running lease alone never makes Redis ACK safe.
		disposition = executionapp.ClaimActiveLeaseNoACK
	}
	wireDisposition, err := claimDispositionProto(disposition)
	if err != nil {
		return claimRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The claim disposition is unavailable.", false), nil
	}
	recovery, err := settlementRecoveryProto(decision.SettlementRecovery)
	if err != nil {
		return claimRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The settlement recovery receipt is unavailable.", false), nil
	}
	retirement, err := retirementProto(decision.RetirementReason)
	if err != nil || (disposition == executionapp.ClaimRetiredACK) != (retirement != nil) {
		return claimRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The retirement receipt is unavailable.", false), nil
	}
	if disposition == executionapp.ClaimRetiredACK && (lease != (runtimedomain.ActiveLease{}) || decision.ClaimHandoffWatermark != 0 || recovery != nil) {
		return claimRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The retirement receipt is unavailable.", false), nil
	}
	receipt := &runtimev1.ClaimReceiptV1{
		Disposition:           wireDisposition,
		Identity:              identity,
		DesiredState:          desiredState,
		ClaimHandoffWatermark: decision.ClaimHandoffWatermark,
		SettlementRecovery:    recovery,
		Retirement:            retirement,
	}
	if lease != (runtimedomain.ActiveLease{}) {
		receipt.Fence = fenceProto(lease.Fence)
		receipt.LeaseExpiresAtUnixMillis = lease.ExpiresAt.UTC().UnixMilli()
		receipt.ClaimId = lease.ClaimID
	}
	if disposition != executionapp.ClaimAccepted {
		return &runtimev1.ClaimCommandResponseV1{Receipt: receipt}, nil
	}
	claimStartedAtUnixMicros := decision.LeaseObservedAt.UTC().UnixMicro()
	if decision.LeaseObservedAt.IsZero() || claimStartedAtUnixMicros <= 0 || claimStartedAtUnixMicros >= lease.ExpiresAt.UTC().UnixMicro() {
		return claimRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The claim creation time is unavailable.", false), nil
	}
	receipt.ClaimStartedAtUnixMicros = claimStartedAtUnixMicros

	manifest, err := s.inputs.ResolveClaimInput(ctx, lease.Fence, command.GetInputBundleRef())
	if err != nil {
		return s.abortInputResolution(ctx, lease.Fence, err), nil
	}
	if err := s.verifyResolvedManifest(command.GetInputBundleRef(), manifest, command); err != nil {
		if abortErr := s.abortClaim(ctx, lease.Fence, executionapp.ClaimAbortInputManifestInvalid); abortErr != nil {
			return claimRejectionFor(abortErr), nil
		}
		return claimRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INCOMPATIBLE_VERSION, "The immutable input manifest does not match the admitted command.", false), nil
	}

	receipt.InputBundleRef = proto.Clone(command.GetInputBundleRef()).(*runtimev1.ExecutionInputBundleReferenceV1)
	receipt.InputBundle = proto.Clone(manifest).(*runtimev1.ExecutionInputBundleV1)
	return &runtimev1.ClaimCommandResponseV1{Receipt: receipt}, nil
}

func (s *Server) abortInputResolution(ctx context.Context, fence runtimedomain.Fence, cause error) *runtimev1.ClaimCommandResponseV1 {
	if errors.Is(cause, runtimedomain.ErrStaleFence) || errors.Is(cause, runtimedomain.ErrLeaseExpired) {
		return claimRejectionFor(cause)
	}

	disposition := executionapp.ClaimAbortInputResolutionRetry
	code := runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE
	message := "The immutable input manifest is temporarily unavailable."
	retryable := true
	switch {
	case errors.Is(cause, executiondomain.ErrInvalidInputBundle):
		disposition = executionapp.ClaimAbortInputManifestInvalid
		code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INCOMPATIBLE_VERSION
		message = "The immutable input manifest does not match the admitted command."
		retryable = false
	case fence.ClaimAttempt >= maxInputResolutionClaimAttempts:
		disposition = executionapp.ClaimAbortInputResolutionExhausted
		message = "The immutable input manifest remains unavailable after bounded retries."
		retryable = false
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		code, message, retryable = safeRuntimeError(cause)
	}
	if err := s.abortClaim(ctx, fence, disposition); err != nil {
		return claimRejectionFor(err)
	}
	return claimRejection(code, message, retryable)
}

func (s *Server) abortClaim(ctx context.Context, fence runtimedomain.Fence, disposition executionapp.ClaimAbortDisposition) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimAbortCleanupTimeout)
	defer cancel()
	return s.claims.Abort(cleanupCtx, fence, disposition)
}

func (s *Server) BeginExecution(ctx context.Context, request *runtimev1.BeginExecutionRequestV1) (*runtimev1.BeginExecutionResponseV1, error) {
	if request == nil || request.GetIdentity() == nil || request.GetFence() == nil || hasUnknownFields(request.ProtoReflect()) {
		return beginExecutionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The begin execution request is malformed.", false), nil
	}
	workloadIdentity, err := s.authorizer.AuthorizeWorkload(ctx, request.GetFence().GetWorkloadSessionId(), request.GetFence().GetProducerId())
	if err != nil || workloadIdentity == "" {
		return beginExecutionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false), nil
	}
	fence, err := fenceDomain(request.GetIdentity(), request.GetFence(), workloadIdentity)
	if err != nil {
		return beginExecutionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false), nil
	}
	disposition, err := s.claims.BeginExecution(ctx, fence)
	if err != nil {
		code, message, retryable := safeRuntimeError(err)
		return beginExecutionRejection(code, message, retryable), nil
	}
	wireDisposition, err := beginExecutionDispositionProto(disposition)
	if err != nil {
		return beginExecutionRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The begin execution disposition is unavailable.", false), nil
	}
	return &runtimev1.BeginExecutionResponseV1{Disposition: wireDisposition}, nil
}

func (s *Server) AuthorizeInvocation(ctx context.Context, request *runtimev1.AuthorizeInvocationRequestV1) (*runtimev1.AuthorizeInvocationResponseV1, error) {
	if request == nil || request.GetIdentity() == nil || request.GetFence() == nil || hasUnknownFields(request.ProtoReflect()) {
		return authorizeInvocationRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The invocation authorization request is malformed.", false), nil
	}
	workloadIdentity, err := s.authorizer.AuthorizeWorkload(ctx, request.GetFence().GetWorkloadSessionId(), request.GetFence().GetProducerId())
	if err != nil || workloadIdentity == "" {
		return authorizeInvocationRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false), nil
	}
	fence, err := fenceDomain(request.GetIdentity(), request.GetFence(), workloadIdentity)
	if err != nil {
		return authorizeInvocationRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false), nil
	}
	disposition, err := s.claims.AuthorizeInvocation(ctx, fence)
	if err != nil {
		code, message, retryable := safeRuntimeError(err)
		return authorizeInvocationRejection(code, message, retryable), nil
	}
	wireDisposition, err := authorizeInvocationDispositionProto(disposition)
	if err != nil {
		return authorizeInvocationRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The invocation authorization disposition is unavailable.", false), nil
	}
	return &runtimev1.AuthorizeInvocationResponseV1{Disposition: wireDisposition}, nil
}

func (s *Server) RenewLease(ctx context.Context, request *runtimev1.RenewLeaseRequestV1) (*runtimev1.RenewLeaseResponseV1, error) {
	if request == nil || request.GetIdempotencyKey() == "" || hasUnknownFields(request.ProtoReflect()) {
		return renewRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The lease renewal request is malformed.", false), nil
	}
	if request.GetFence() == nil {
		return renewRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false), nil
	}
	workloadIdentity, err := s.authorizer.AuthorizeWorkload(ctx, request.GetFence().GetWorkloadSessionId(), request.GetFence().GetProducerId())
	if err != nil || workloadIdentity == "" {
		return renewRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false), nil
	}
	fence, err := fenceDomain(request.GetIdentity(), request.GetFence(), workloadIdentity)
	if err != nil {
		return renewRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false), nil
	}
	lease, err := s.claims.Renew(ctx, fence)
	if err != nil {
		code, message, retryable := safeRuntimeError(err)
		return renewRejection(code, message, retryable), nil
	}
	desiredState, err := desiredStateProto(lease.DesiredState)
	if err != nil {
		return renewRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The execution desired state is unavailable.", false), nil
	}
	return &runtimev1.RenewLeaseResponseV1{
		LeaseExpiresAtUnixMillis: lease.ExpiresAt.UTC().UnixMilli(),
		DesiredState:             desiredState,
	}, nil
}

func (s *Server) ObserveDesiredState(ctx context.Context, request *runtimev1.ObserveDesiredStateRequestV1) (*runtimev1.ObserveDesiredStateResponseV1, error) {
	if request == nil || hasUnknownFields(request.ProtoReflect()) {
		return observeRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The desired-state request is malformed.", false), nil
	}
	if request.GetFence() == nil {
		return observeRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false), nil
	}
	workloadIdentity, err := s.authorizer.AuthorizeWorkload(ctx, request.GetFence().GetWorkloadSessionId(), request.GetFence().GetProducerId())
	if err != nil || workloadIdentity == "" {
		return observeRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false), nil
	}
	fence, err := fenceDomain(request.GetIdentity(), request.GetFence(), workloadIdentity)
	if err != nil {
		return observeRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false), nil
	}
	state, err := s.claims.ObserveDesiredState(ctx, fence)
	if err != nil {
		code, message, retryable := safeRuntimeError(err)
		return observeRejection(code, message, retryable), nil
	}
	desiredState, err := desiredStateProto(state)
	if err != nil {
		return observeRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The execution desired state is unavailable.", false), nil
	}
	return &runtimev1.ObserveDesiredStateResponseV1{
		DesiredState: desiredState,
	}, nil
}

func (s *Server) PrepareSettlement(ctx context.Context, request *runtimev1.PrepareSettlementRequestV1) (*runtimev1.PrepareSettlementResponseV1, error) {
	if request == nil || request.GetProposal() == nil || request.GetIdempotencyKey() == "" || hasUnknownFields(request.ProtoReflect()) {
		return settlementRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The settlement request is malformed.", false), nil
	}
	if request.GetProposal().GetPrepareIdempotencyKey() == "" || request.GetProposal().GetPrepareIdempotencyKey() != request.GetIdempotencyKey() {
		return settlementRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The settlement idempotency binding is malformed.", false), nil
	}
	if request.GetFence() == nil {
		return settlementRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The settlement fence is malformed.", false), nil
	}
	workloadIdentity, err := s.authorizer.AuthorizeWorkload(ctx, request.GetFence().GetWorkloadSessionId(), request.GetFence().GetProducerId())
	if err != nil || workloadIdentity == "" {
		return settlementRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false), nil
	}
	fence, err := fenceDomain(request.GetIdentity(), request.GetFence(), workloadIdentity)
	if err != nil {
		return settlementRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The settlement fence is malformed.", false), nil
	}
	proposalBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(request.GetProposal())
	if err != nil || !validDigestProto(request.GetProposalDigest(), proposalBytes) {
		return settlementRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The settlement proposal digest is invalid.", false), nil
	}
	payloadDigest, err := digestDomain(request.GetProposal().GetTerminalPayloadDigest())
	if err != nil {
		return settlementRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The terminal output digest is malformed.", false), nil
	}
	proposalDigest, _ := digestDomain(request.GetProposalDigest())
	outcome, err := settlementOutcomeDomain(request.GetProposal().GetRequestedOutcome())
	if err != nil {
		return settlementRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The settlement outcome is malformed.", false), nil
	}
	proposal := executionapp.SettlementProposal{
		Fence:                   fence,
		ProposalID:              request.GetProposal().GetProposalId(),
		Outcome:                 outcome,
		TerminalLogicalOutputID: request.GetProposal().GetTerminalLogicalOutputId(),
		TerminalEventID:         request.GetProposal().GetTerminalEventId(),
		TerminalSequence:        request.GetProposal().GetTerminalSequence(),
		TerminalPayloadDigest:   payloadDigest,
		ProposalDigest:          proposalDigest,
		IdempotencyKey:          request.GetIdempotencyKey(),
	}
	if err := proposal.Validate(); err != nil {
		return settlementRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The settlement proposal is malformed.", false), nil
	}
	receipt, err := s.settlements.PrepareSettlement(ctx, proposal)
	if err != nil {
		code, message, retryable := safeRuntimeError(err)
		return settlementRejection(code, message, retryable), nil
	}
	return &runtimev1.PrepareSettlementResponseV1{
		SettlementReceiptId: receipt.ID,
		Outcome:             settlementOutcomeProto(receipt.Outcome),
	}, nil
}

func (s *Server) verifyResolvedManifest(reference *runtimev1.ExecutionInputBundleReferenceV1, manifest *runtimev1.ExecutionInputBundleV1, command *runtimev1.WorkerCommandV1) error {
	if reference == nil || manifest == nil || hasUnknownFields(manifest.ProtoReflect()) || len(manifest.GetEntries()) == 0 || len(manifest.GetEntries()) > s.config.MaxInputEntries {
		return errors.New("invalid input manifest")
	}
	if manifest.GetInputBundleId() != reference.GetInputBundleId() || manifest.GetImmutableVersion() != reference.GetImmutableVersion() {
		return errors.New("input manifest identity mismatch")
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(encoded) > s.config.MaxInputManifestBytes || uint64(len(encoded)) != reference.GetByteLength() || !validDigestProto(reference.GetDigest(), encoded) {
		return errors.New("input manifest digest or length mismatch")
	}
	if reference.GetMediaType() != executiondomain.InputBundleManifestMediaType {
		return errors.New("unsupported input manifest media type or selection")
	}
	expectedRoles, err := expectedInputRoles(command)
	if err != nil || len(expectedRoles) != len(manifest.GetEntries()) {
		return errors.New("input manifest does not match the command bindings")
	}
	seen := make(map[string]struct{}, len(manifest.GetEntries()))
	for _, entry := range manifest.GetEntries() {
		if entry == nil || entry.GetEntryId() == "" || len(entry.GetEntryId()) > s.config.MaxStringBytes || entry.GetImmutableVersion() == "" || len(entry.GetImmutableVersion()) > s.config.MaxStringBytes || entry.GetSemanticRole() == "" || len(entry.GetSemanticRole()) > s.config.MaxStringBytes || entry.GetContent() == nil {
			return errors.New("input manifest entry is malformed")
		}
		if _, duplicate := seen[entry.GetEntryId()]; duplicate {
			return errors.New("input manifest entry ID is duplicated")
		}
		seen[entry.GetEntryId()] = struct{}{}
		expectedRole, expected := expectedRoles[entry.GetEntryId()]
		if !expected || entry.GetSemanticRole() != expectedRole {
			return errors.New("input manifest entry has the wrong command binding")
		}
		content := entry.GetContent()
		expectedMediaType, maxContentBytes, err := inputContentContract(expectedRole, s.config.MaxInputContentBytes)
		if err != nil {
			return err
		}
		if content.GetContentId() == "" || len(content.GetContentId()) > s.config.MaxStringBytes || content.GetImmutableVersion() == "" || len(content.GetImmutableVersion()) > s.config.MaxStringBytes || content.GetImmutableVersion() != entry.GetImmutableVersion() || content.GetMediaType() != expectedMediaType || content.GetClassification() == "" || len(content.GetClassification()) > s.config.MaxStringBytes || content.GetRequiredGrantAudience() == "" || len(content.GetRequiredGrantAudience()) > s.config.MaxStringBytes || content.GetByteLength() == 0 || content.GetByteLength() > maxContentBytes || !validDigestMessage(content.GetDigest()) {
			return errors.New("input manifest content reference is malformed")
		}
	}
	return nil
}

func expectedInputRoles(command *runtimev1.WorkerCommandV1) (map[string]string, error) {
	if command == nil {
		return nil, errors.New("command is required")
	}
	switch command.GetCapabilityId() {
	case executiondomain.ConfigurationValidationCapability:
		validation := command.GetConfigurationValidation()
		if validation == nil || validation.GetSettingsEntryId() == "" {
			return nil, errors.New("configuration settings binding is required")
		}
		return map[string]string{validation.GetSettingsEntryId(): "configuration.settings"}, nil
	case executiondomain.IndexIngestCapability:
		indexing := command.GetIndexIngest()
		if indexing == nil || indexing.GetToolkitConfigurationEntryId() == "" || indexing.GetToolParametersEntryId() == "" {
			return nil, errors.New("index input bindings are required")
		}
		bindings := []struct {
			entryID string
			role    string
		}{
			{indexing.GetToolkitConfigurationEntryId(), executiondomain.IndexToolkitConfigurationRole},
			{indexing.GetToolParametersEntryId(), executiondomain.IndexToolParametersRole},
			{indexing.GetLlmModelEntryId(), executiondomain.IndexLLMModelRole},
			{indexing.GetLlmConfigurationEntryId(), executiondomain.IndexLLMConfigurationRole},
			{indexing.GetMcpTokensEntryId(), executiondomain.IndexMCPTokensRole},
		}
		if embedding := indexing.GetEmbeddingBinding(); embedding != nil {
			if embedding.GetEntryId() == "" {
				return nil, errors.New("index embedding input binding is required")
			}
			bindings = append(bindings, struct {
				entryID string
				role    string
			}{embedding.GetEntryId(), executiondomain.IndexEmbeddingBindingRole})
		}
		expected := make(map[string]string, len(bindings))
		for _, binding := range bindings {
			if binding.entryID == "" {
				continue
			}
			if _, duplicate := expected[binding.entryID]; duplicate {
				return nil, errors.New("index input binding is duplicated")
			}
			expected[binding.entryID] = binding.role
		}
		return expected, nil
	case executiondomain.AgentApplicationCapability, executiondomain.AgentAdhocCapability:
		agent := command.GetAgentExecution()
		if agent == nil || agent.GetRequestEntryId() == "" {
			return nil, errors.New("agent input binding is required")
		}
		return map[string]string{
			agent.GetRequestEntryId(): executiondomain.AgentExecutionRequestRole,
		}, nil
	case executiondomain.ToolkitExecuteReadCapability:
		toolkit := command.GetToolkitExecuteRead()
		if toolkit == nil || toolkit.GetRequestEntryId() == "" {
			return nil, errors.New("direct toolkit input binding is required")
		}
		return map[string]string{
			toolkit.GetRequestEntryId(): executiondomain.ToolkitExecuteReadRequestRole,
		}, nil
	case executiondomain.ToolkitAvailableToolsCapability:
		discovery := command.GetToolkitAvailableTools()
		if discovery == nil || discovery.GetSettingsEntryId() == "" || discovery.GetSettingsEntryId() == "toolkit-runtime-context" {
			return nil, errors.New("tool discovery input binding is required")
		}
		return map[string]string{discovery.GetSettingsEntryId(): executiondomain.ToolkitAvailableToolsSettingsRole, "toolkit-runtime-context": "toolkit.available_tools.runtime_context"}, nil
	case executiondomain.ToolkitCallToolCapability:
		call := command.GetToolkitCallTool()
		if call == nil || call.GetSettingsEntryId() == "" || call.GetArgumentsEntryId() == "" {
			return nil, errors.New("tool-run input bindings are required")
		}
		if call.GetSettingsEntryId() == call.GetArgumentsEntryId() || call.GetSettingsEntryId() == "toolkit-runtime-context" || call.GetArgumentsEntryId() == "toolkit-runtime-context" {
			return nil, errors.New("tool-run input binding is duplicated")
		}
		return map[string]string{
			call.GetSettingsEntryId():  executiondomain.ToolkitCallToolSettingsRole,
			call.GetArgumentsEntryId(): executiondomain.ToolkitCallToolArgumentsRole,
			"toolkit-runtime-context":  executiondomain.ToolkitCallToolRuntimeContextRole,
		}, nil
	default:
		return nil, errors.New("unsupported command capability")
	}
}

func inputContentContract(role string, configuredMax uint64) (string, uint64, error) {
	var mediaType string
	var roleMax uint64
	switch role {
	case executiondomain.AgentExecutionRequestRole:
		mediaType = executiondomain.AgentExecutionInputMediaType
		roleMax = executiondomain.MaxAgentExecutionInputBytes
	case executiondomain.ToolkitExecuteReadRequestRole:
		mediaType = executiondomain.ToolkitExecuteReadInputMediaType
		roleMax = executiondomain.MaxToolkitExecuteReadInputBytes
	default:
		mediaType = executiondomain.SettingsJSONMediaType
		roleMax = executiondomain.MaxInputEntryContentBytes
	}
	if configuredMax == 0 || roleMax == 0 {
		return "", 0, errors.New("input manifest content limit is unavailable")
	}
	if configuredMax < roleMax {
		roleMax = configuredMax
	}
	return mediaType, roleMax, nil
}

func identityProto(command *runtimev1.WorkerCommandV1) *runtimev1.ExecutionIdentityV1 {
	return &runtimev1.ExecutionIdentityV1{
		TenantId:            command.GetTenantId(),
		ResourceProjectId:   command.GetResourceProjectId(),
		ProjectionProjectId: command.GetProjectionProjectId(),
		CommandId:           command.GetCommandId(),
		ExecutionId:         command.GetExecutionId(),
		Generation:          command.GetGeneration(),
	}
}

func fenceDomain(identity *runtimev1.ExecutionIdentityV1, fence *runtimev1.ExecutionFenceV1, workloadIdentity string) (runtimedomain.Fence, error) {
	var result runtimedomain.Fence
	if identity == nil || fence == nil || workloadIdentity == "" || len(fence.GetFenceToken()) != sha256.Size {
		return result, runtimedomain.ErrInvalidFence
	}
	result = runtimedomain.Fence{
		CommandID:         identity.GetCommandId(),
		ExecutionID:       identity.GetExecutionId(),
		Generation:        identity.GetGeneration(),
		WorkloadIdentity:  workloadIdentity,
		WorkloadSessionID: fence.GetWorkloadSessionId(),
		ProducerID:        fence.GetProducerId(),
		ClaimAttempt:      fence.GetClaimAttempt(),
		LeaseEpoch:        fence.GetLeaseEpoch(),
	}
	copy(result.Token[:], fence.GetFenceToken())
	if err := result.Validate(); err != nil {
		return runtimedomain.Fence{}, err
	}
	return result, nil
}

func fenceProto(fence runtimedomain.Fence) *runtimev1.ExecutionFenceV1 {
	return &runtimev1.ExecutionFenceV1{
		WorkloadSessionId: fence.WorkloadSessionID,
		ClaimAttempt:      fence.ClaimAttempt,
		LeaseEpoch:        fence.LeaseEpoch,
		ProducerId:        fence.ProducerID,
		FenceToken:        append([]byte(nil), fence.Token[:]...),
	}
}

func claimRejectionFor(err error) *runtimev1.ClaimCommandResponseV1 {
	code, message, retryable := safeRuntimeError(err)
	return claimRejection(code, message, retryable)
}

func safeRuntimeError(err error) (runtimev1.RuntimeErrorCodeV1, string, bool) {
	switch {
	case errors.Is(err, runtimedomain.ErrStaleFence), errors.Is(err, runtimedomain.ErrLeaseExpired):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE, "The execution fence is no longer current.", false
	case errors.Is(err, ErrCommandAuthentication):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The worker command is not authenticated.", false
	case errors.Is(err, ErrCommandIncompatible):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INCOMPATIBLE_VERSION, "The worker command protocol is not compatible.", false
	case errors.Is(err, ErrMalformedWorkerCommand), errors.Is(err, executionapp.ErrInvalidClaim), errors.Is(err, runtimedomain.ErrInvalidFence):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The runtime control message is malformed.", false
	case errors.Is(err, executionapp.ErrSettlementConflict):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The settlement idempotency key conflicts with a durable proposal.", false
	case errors.Is(err, executionapp.ErrTerminalOutputNotReady):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The terminal output is not durably ready for settlement.", true
	case errors.Is(err, executionapp.ErrClaimDependencyUnavailable):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The durable claim state is temporarily unavailable.", true
	case errors.Is(err, executionapp.ErrInvalidSettlement):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The settlement proposal is malformed.", false
	case errors.Is(err, context.Canceled):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_CANCELLED, "The runtime operation was cancelled.", false
	case errors.Is(err, context.DeadlineExceeded):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The runtime control deadline was exceeded.", true
	default:
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The runtime control operation failed.", false
	}
}

func claimRejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.ClaimCommandResponseV1 {
	return &runtimev1.ClaimCommandResponseV1{Rejection: runtimeError(code, message, retryable)}
}

func renewRejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.RenewLeaseResponseV1 {
	return &runtimev1.RenewLeaseResponseV1{Rejection: runtimeError(code, message, retryable)}
}

func observeRejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.ObserveDesiredStateResponseV1 {
	return &runtimev1.ObserveDesiredStateResponseV1{Rejection: runtimeError(code, message, retryable)}
}

func settlementRejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.PrepareSettlementResponseV1 {
	return &runtimev1.PrepareSettlementResponseV1{Rejection: runtimeError(code, message, retryable)}
}

func beginExecutionRejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.BeginExecutionResponseV1 {
	return &runtimev1.BeginExecutionResponseV1{Rejection: runtimeError(code, message, retryable)}
}

func authorizeInvocationRejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.AuthorizeInvocationResponseV1 {
	return &runtimev1.AuthorizeInvocationResponseV1{Rejection: runtimeError(code, message, retryable)}
}

func runtimeError(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.RuntimeErrorV1 {
	return &runtimev1.RuntimeErrorV1{Code: code, SafeMessage: message, Retryable: retryable}
}

func desiredStateProto(state runtimedomain.DesiredState) (runtimev1.DesiredExecutionStateV1, error) {
	switch state {
	case runtimedomain.DesiredRunning:
		return runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_RUNNING, nil
	case runtimedomain.DesiredCancelled:
		return runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_CANCELLED, nil
	case runtimedomain.DesiredDraining:
		return runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_DRAINING, nil
	default:
		return runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_UNSPECIFIED, errors.New("invalid desired state")
	}
}

func claimDispositionProto(disposition executionapp.ClaimDisposition) (runtimev1.ClaimDispositionV1, error) {
	switch disposition {
	case executionapp.ClaimAccepted:
		return runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_ACCEPTED, nil
	case executionapp.ClaimRecoverTerminalACK:
		return runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RECOVER_TERMINAL_ACK, nil
	case executionapp.ClaimRecoverSettlement:
		return runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RECOVER_SETTLEMENT, nil
	case executionapp.ClaimSettledACK:
		return runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_SETTLED_ACK, nil
	case executionapp.ClaimObsoleteACK:
		return runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_OBSOLETE_ACK, nil
	case executionapp.ClaimActiveLeaseNoACK:
		return runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_ACTIVE_LEASE_NOACK, nil
	case executionapp.ClaimRetryLaterNoACK:
		return runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RETRY_LATER_NOACK, nil
	case executionapp.ClaimRetiredACK:
		return runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RETIRED_ACK, nil
	case executionapp.ClaimRecoverRunningNoACK:
		return runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK, nil
	case executionapp.ClaimRecoverAmbiguousInvocationNoACK:
		return runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RECOVER_AMBIGUOUS_INVOCATION_NOACK, nil
	default:
		return runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_UNSPECIFIED, executionapp.ErrInvalidClaim
	}
}

func beginExecutionDispositionProto(disposition executionapp.BeginExecutionDisposition) (runtimev1.BeginExecutionDispositionV1, error) {
	switch disposition {
	case executionapp.BeginExecutionStartedNow:
		return runtimev1.BeginExecutionDispositionV1_BEGIN_EXECUTION_DISPOSITION_V1_STARTED_NOW, nil
	case executionapp.BeginExecutionAlreadyStarted:
		return runtimev1.BeginExecutionDispositionV1_BEGIN_EXECUTION_DISPOSITION_V1_ALREADY_STARTED, nil
	default:
		return runtimev1.BeginExecutionDispositionV1_BEGIN_EXECUTION_DISPOSITION_V1_UNSPECIFIED, executionapp.ErrInvalidClaim
	}
}

func authorizeInvocationDispositionProto(disposition executionapp.AuthorizeInvocationDisposition) (runtimev1.AuthorizeInvocationDispositionV1, error) {
	switch disposition {
	case executionapp.AuthorizeInvocationNow:
		return runtimev1.AuthorizeInvocationDispositionV1_AUTHORIZE_INVOCATION_DISPOSITION_V1_AUTHORIZED_NOW, nil
	case executionapp.AuthorizeInvocationAlready:
		return runtimev1.AuthorizeInvocationDispositionV1_AUTHORIZE_INVOCATION_DISPOSITION_V1_ALREADY_AUTHORIZED, nil
	default:
		return runtimev1.AuthorizeInvocationDispositionV1_AUTHORIZE_INVOCATION_DISPOSITION_V1_UNSPECIFIED, executionapp.ErrInvalidClaim
	}
}

func retirementProto(reason executionapp.RetirementReason) (*runtimev1.RuntimeErrorV1, error) {
	switch reason {
	case "":
		return nil, nil
	case executionapp.RetirementDeadlineExceeded:
		return runtimeError(
			runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED,
			executionapp.DeadlineExceededSafeMessage,
			true,
		), nil
	default:
		return nil, executionapp.ErrInvalidClaim
	}
}

func settlementRecoveryProto(recovery *executionapp.SettlementRecovery) (*runtimev1.SettlementRecoveryV1, error) {
	if recovery == nil {
		return nil, nil
	}
	wire := &runtimev1.SettlementRecoveryV1{}
	if recovery.Proposal != nil {
		proposal := recovery.Proposal
		wire.Proposal = &runtimev1.SettlementProposalV1{
			ProposalId:              proposal.ProposalID,
			RequestedOutcome:        settlementOutcomeProto(proposal.Outcome),
			TerminalLogicalOutputId: proposal.TerminalLogicalOutputID,
			TerminalEventId:         proposal.TerminalEventID,
			TerminalSequence:        proposal.TerminalSequence,
			TerminalPayloadDigest:   digestProto(proposal.TerminalPayloadDigest),
			PrepareIdempotencyKey:   proposal.IdempotencyKey,
		}
		encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(wire.Proposal)
		if err != nil || runtimedomain.SHA256(encoded) != proposal.ProposalDigest {
			return nil, executionapp.ErrInvalidClaim
		}
		wire.ProposalDigest = digestProto(proposal.ProposalDigest)
		wire.IdempotencyKey = proposal.IdempotencyKey
	}
	if recovery.Receipt != nil {
		wire.SettlementReceiptId = recovery.Receipt.ID
		wire.Outcome = settlementOutcomeProto(recovery.Receipt.Outcome)
	}
	if wire.GetProposal() == nil && wire.GetSettlementReceiptId() == "" {
		return nil, executionapp.ErrInvalidClaim
	}
	return wire, nil
}

func digestProto(digest runtimedomain.Digest) *runtimev1.DigestV1 {
	return &runtimev1.DigestV1{
		Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
		Value:     append([]byte(nil), digest[:]...),
	}
}

func digestDomain(digest *runtimev1.DigestV1) (runtimedomain.Digest, error) {
	var mapped runtimedomain.Digest
	if !validDigestMessage(digest) {
		return mapped, runtimedomain.ErrInvalidDigest
	}
	copy(mapped[:], digest.GetValue())
	return mapped, nil
}

func settlementOutcomeDomain(outcome runtimev1.ExecutionOutcomeV1) (executionapp.SettlementOutcome, error) {
	switch outcome {
	case runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED:
		return executionapp.SettlementSucceeded, nil
	case runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_FAILED:
		return executionapp.SettlementFailed, nil
	case runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_CANCELLED:
		return executionapp.SettlementCancelled, nil
	case runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_OUTCOME_UNKNOWN:
		return executionapp.SettlementOutcomeUnknown, nil
	default:
		return "", executionapp.ErrInvalidSettlement
	}
}

func settlementOutcomeProto(outcome executionapp.SettlementOutcome) runtimev1.ExecutionOutcomeV1 {
	switch outcome {
	case executionapp.SettlementSucceeded:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED
	case executionapp.SettlementFailed:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_FAILED
	case executionapp.SettlementCancelled:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_CANCELLED
	case executionapp.SettlementOutcomeUnknown:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_OUTCOME_UNKNOWN
	default:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_UNSPECIFIED
	}
}

var _ runtimev1.RuntimeControlServiceServer = (*Server)(nil)
