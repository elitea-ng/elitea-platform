package control

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

type workloadAuthorizerStub struct {
	calls    *[]string
	identity string
	err      error
}

func (s workloadAuthorizerStub) AuthorizeWorkload(_ context.Context, _, _ string) (string, error) {
	*s.calls = append(*s.calls, "authorize-peer")
	if s.err != nil {
		return "", s.err
	}
	if s.identity != "" {
		return s.identity, nil
	}
	return "spiffe://elitea.test/workload/worker-1", nil
}

type verifierSpy struct {
	calls    *[]string
	verifier CommandVerifier
}

func (s verifierSpy) Verify(ctx context.Context, envelope *runtimev1.SignedWorkerCommandEnvelopeV1) (*runtimev1.WorkerCommandV1, error) {
	*s.calls = append(*s.calls, "verify-command")
	return s.verifier.Verify(ctx, envelope)
}

type claimControllerStub struct {
	calls             *[]string
	lease             runtimedomain.ActiveLease
	disposition       executionapp.ClaimDisposition
	retirementReason  executionapp.RetirementReason
	recovery          *executionapp.SettlementRecovery
	watermark         uint64
	desiredState      runtimedomain.DesiredState
	claimRequest      *executionapp.ClaimRequest
	claimErr          error
	leaseObservedAt   time.Time
	beginDisposition  executionapp.BeginExecutionDisposition
	beginSequence     *[]executionapp.BeginExecutionDisposition
	beginErr          error
	invokeDisposition executionapp.AuthorizeInvocationDisposition
	invokeSequence    *[]executionapp.AuthorizeInvocationDisposition
	invokeErr         error
	abortDisposition  *executionapp.ClaimAbortDisposition
	abortErr          error
}

func (s claimControllerStub) AuthorizeInvocation(_ context.Context, fence runtimedomain.Fence) (executionapp.AuthorizeInvocationDisposition, error) {
	*s.calls = append(*s.calls, "authorize-invocation")
	if fence != s.lease.Fence {
		return "", runtimedomain.ErrStaleFence
	}
	if s.invokeErr != nil {
		return "", s.invokeErr
	}
	if s.invokeSequence != nil && len(*s.invokeSequence) > 0 {
		disposition := (*s.invokeSequence)[0]
		*s.invokeSequence = (*s.invokeSequence)[1:]
		return disposition, nil
	}
	if s.invokeDisposition != "" {
		return s.invokeDisposition, nil
	}
	return executionapp.AuthorizeInvocationNow, nil
}

func (s claimControllerStub) BeginExecution(_ context.Context, fence runtimedomain.Fence) (executionapp.BeginExecutionDisposition, error) {
	*s.calls = append(*s.calls, "begin-execution")
	if fence != s.lease.Fence {
		return "", runtimedomain.ErrStaleFence
	}
	if s.beginErr != nil {
		return "", s.beginErr
	}
	if s.beginSequence != nil && len(*s.beginSequence) > 0 {
		disposition := (*s.beginSequence)[0]
		*s.beginSequence = (*s.beginSequence)[1:]
		return disposition, nil
	}
	if s.beginDisposition != "" {
		return s.beginDisposition, nil
	}
	return executionapp.BeginExecutionStartedNow, nil
}

func (s claimControllerStub) Claim(_ context.Context, request executionapp.ClaimRequest) (executionapp.ClaimDecision, error) {
	*s.calls = append(*s.calls, "claim")
	if s.claimRequest != nil {
		*s.claimRequest = request
	}
	if request.OutboxID != "outbox-1" || request.SignedEnvelopeDigest.IsZero() {
		return executionapp.ClaimDecision{}, executionapp.ErrInvalidClaim
	}
	if s.claimErr != nil {
		return executionapp.ClaimDecision{}, s.claimErr
	}
	if s.lease == (runtimedomain.ActiveLease{}) {
		return executionapp.ClaimDecision{Disposition: s.disposition, DesiredState: s.desiredState, RetirementReason: s.retirementReason, SettlementRecovery: s.recovery, ClaimHandoffWatermark: s.watermark}, nil
	}
	if request.CommandID != s.lease.Fence.CommandID || request.ExecutionID != s.lease.Fence.ExecutionID || request.Generation != s.lease.Fence.Generation || request.WorkloadIdentity != s.lease.Fence.WorkloadIdentity || request.WorkloadSessionID != s.lease.Fence.WorkloadSessionID || request.ProducerID != s.lease.Fence.ProducerID {
		return executionapp.ClaimDecision{}, executionapp.ErrInvalidClaim
	}
	disposition := s.disposition
	if disposition == "" {
		disposition = executionapp.ClaimAccepted
	}
	leaseObservedAt := s.leaseObservedAt
	if leaseObservedAt.IsZero() && disposition == executionapp.ClaimAccepted {
		leaseObservedAt = s.lease.ExpiresAt.Add(-time.Minute)
	}
	return executionapp.ClaimDecision{Lease: s.lease, LeaseObservedAt: leaseObservedAt, Disposition: disposition, RetirementReason: s.retirementReason, SettlementRecovery: s.recovery, ClaimHandoffWatermark: s.watermark}, nil
}

func (s claimControllerStub) Abort(_ context.Context, fence runtimedomain.Fence, disposition executionapp.ClaimAbortDisposition) error {
	*s.calls = append(*s.calls, "abort-claim")
	if fence != s.lease.Fence {
		return runtimedomain.ErrStaleFence
	}
	if s.abortDisposition != nil {
		*s.abortDisposition = disposition
	}
	return s.abortErr
}

func (s claimControllerStub) Renew(_ context.Context, fence runtimedomain.Fence) (runtimedomain.ActiveLease, error) {
	*s.calls = append(*s.calls, "renew")
	if fence != s.lease.Fence {
		return runtimedomain.ActiveLease{}, runtimedomain.ErrStaleFence
	}
	return s.lease, nil
}

func (s claimControllerStub) VerifyActive(_ context.Context, fence runtimedomain.Fence) error {
	*s.calls = append(*s.calls, "verify-fence")
	if fence != s.lease.Fence {
		return runtimedomain.ErrStaleFence
	}
	return nil
}

func (s claimControllerStub) ObserveDesiredState(_ context.Context, fence runtimedomain.Fence) (runtimedomain.DesiredState, error) {
	*s.calls = append(*s.calls, "observe-desired-state")
	if fence != s.lease.Fence {
		return "", runtimedomain.ErrStaleFence
	}
	return s.lease.DesiredState, nil
}

type settlementControllerStub struct {
	calls     *[]string
	proposals []executionapp.SettlementProposal
	receipt   executionapp.SettlementReceipt
	err       error
}

func (s *settlementControllerStub) PrepareSettlement(_ context.Context, proposal executionapp.SettlementProposal) (executionapp.SettlementReceipt, error) {
	*s.calls = append(*s.calls, "prepare-settlement")
	s.proposals = append(s.proposals, proposal)
	return s.receipt, s.err
}

type inputResolverStub struct {
	calls    *[]string
	manifest *runtimev1.ExecutionInputBundleV1
	err      error
}

func (s inputResolverStub) ResolveClaimInput(_ context.Context, _ runtimedomain.Fence, _ *runtimev1.ExecutionInputBundleReferenceV1) (*runtimev1.ExecutionInputBundleV1, error) {
	*s.calls = append(*s.calls, "resolve-reference-manifest")
	return s.manifest, s.err
}

func TestClaimCommandAuthenticatesThenVerifiesThenClaimsBeforeResolvingInput(t *testing.T) {
	manifest := validManifest()
	request := claimRequestForManifest(t, manifest)
	calls := []string{}
	var durableRequest executionapp.ClaimRequest
	lease := validLease()
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{calls: &calls, lease: lease, claimRequest: &durableRequest},
		inputResolverStub{calls: &calls, manifest: manifest},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.ClaimCommand(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRejection() != nil || response.GetReceipt().GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_ACCEPTED {
		t.Fatalf("unexpected claim response: %v", response)
	}
	if response.GetReceipt().GetClaimId() != lease.ClaimID {
		t.Fatalf("claim receipt lost durable claim ID: %v", response.GetReceipt())
	}
	wantClaimStartedAt := lease.ExpiresAt.Add(-time.Minute).UTC().UnixMicro()
	if response.GetReceipt().GetClaimStartedAtUnixMicros() != wantClaimStartedAt {
		t.Fatalf("claim receipt lost database-authored claim ordering: got %d want %d", response.GetReceipt().GetClaimStartedAtUnixMicros(), wantClaimStartedAt)
	}
	canonicalEnvelope, err := proto.MarshalOptions{Deterministic: true}.Marshal(request.GetSignedCommand())
	if err != nil {
		t.Fatal(err)
	}
	if durableRequest.OutboxID != "outbox-1" || durableRequest.SignedEnvelopeDigest != runtimedomain.SHA256(canonicalEnvelope) {
		t.Fatalf("claim lost exact published command binding: %+v", durableRequest)
	}
	wantOrder := []string{"authorize-peer", "verify-command", "claim", "resolve-reference-manifest"}
	if !reflect.DeepEqual(calls, wantOrder) {
		t.Fatalf("unsafe claim order: got %v want %v", calls, wantOrder)
	}
	if got := response.GetReceipt().GetFence().GetFenceToken(); string(got) != string(lease.Fence.Token[:]) {
		t.Fatal("claim response lost unpredictable fence token")
	}
}

func TestClaimCommandRejectsMalformedClaimOrderingBeforeResolvingInput(t *testing.T) {
	for _, test := range []struct {
		name            string
		leaseObservedAt func(runtimedomain.ActiveLease) time.Time
	}{
		{name: "non-positive", leaseObservedAt: func(runtimedomain.ActiveLease) time.Time { return time.Unix(0, 0).UTC() }},
		{name: "not-before-expiry", leaseObservedAt: func(lease runtimedomain.ActiveLease) time.Time { return lease.ExpiresAt }},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			lease := validLease()
			server, err := NewServer(
				testControlServerConfig(),
				workloadAuthorizerStub{calls: &calls},
				verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
				claimControllerStub{calls: &calls, lease: lease, leaseObservedAt: test.leaseObservedAt(lease)},
				inputResolverStub{calls: &calls, manifest: validManifest()},
				&settlementControllerStub{calls: &calls},
			)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
			if err != nil {
				t.Fatal(err)
			}
			if response.GetReceipt() != nil || response.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL {
				t.Fatalf("malformed claim ordering was accepted: %v", response)
			}
			if want := []string{"authorize-peer", "verify-command", "claim"}; !reflect.DeepEqual(calls, want) {
				t.Fatalf("malformed claim ordering reached business input: got %v want %v", calls, want)
			}
		})
	}
}

func TestBeginExecutionReturnsStartedThenLostResponseReplayWithoutBusinessMaterial(t *testing.T) {
	calls := []string{}
	lease := validLease()
	sequence := []executionapp.BeginExecutionDisposition{
		executionapp.BeginExecutionStartedNow,
		executionapp.BeginExecutionAlreadyStarted,
	}
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		newTestVerifier(t),
		claimControllerStub{calls: &calls, lease: lease, beginSequence: &sequence},
		inputResolverStub{calls: &calls, manifest: validManifest()},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := &runtimev1.BeginExecutionRequestV1{
		Identity: &runtimev1.ExecutionIdentityV1{
			CommandId:   lease.Fence.CommandID,
			ExecutionId: lease.Fence.ExecutionID,
			Generation:  lease.Fence.Generation,
		},
		Fence: fenceProto(lease.Fence),
	}

	first, err := server.BeginExecution(context.Background(), request)
	if err != nil || first.GetRejection() != nil || first.GetDisposition() != runtimev1.BeginExecutionDispositionV1_BEGIN_EXECUTION_DISPOSITION_V1_STARTED_NOW {
		t.Fatalf("first begin response=%v err=%v", first, err)
	}
	replay, err := server.BeginExecution(context.Background(), request)
	if err != nil || replay.GetRejection() != nil || replay.GetDisposition() != runtimev1.BeginExecutionDispositionV1_BEGIN_EXECUTION_DISPOSITION_V1_ALREADY_STARTED {
		t.Fatalf("lost-response replay=%v err=%v", replay, err)
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "begin-execution", "authorize-peer", "begin-execution"}) {
		t.Fatalf("begin execution crossed a business-input boundary: %v", calls)
	}
}

func TestAuthorizeInvocationReturnsOneSubmissionAuthorityWithoutBusinessMaterial(t *testing.T) {
	calls := []string{}
	lease := validLease()
	sequence := []executionapp.AuthorizeInvocationDisposition{
		executionapp.AuthorizeInvocationNow,
		executionapp.AuthorizeInvocationAlready,
	}
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		newTestVerifier(t),
		claimControllerStub{calls: &calls, lease: lease, invokeSequence: &sequence},
		inputResolverStub{calls: &calls, manifest: validManifest()},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := &runtimev1.AuthorizeInvocationRequestV1{
		Identity: &runtimev1.ExecutionIdentityV1{
			CommandId:   lease.Fence.CommandID,
			ExecutionId: lease.Fence.ExecutionID,
			Generation:  lease.Fence.Generation,
		},
		Fence: fenceProto(lease.Fence),
	}

	first, err := server.AuthorizeInvocation(context.Background(), request)
	if err != nil || first.GetRejection() != nil || first.GetDisposition() != runtimev1.AuthorizeInvocationDispositionV1_AUTHORIZE_INVOCATION_DISPOSITION_V1_AUTHORIZED_NOW {
		t.Fatalf("first invocation authorization=%v err=%v", first, err)
	}
	replay, err := server.AuthorizeInvocation(context.Background(), request)
	if err != nil || replay.GetRejection() != nil || replay.GetDisposition() != runtimev1.AuthorizeInvocationDispositionV1_AUTHORIZE_INVOCATION_DISPOSITION_V1_ALREADY_AUTHORIZED {
		t.Fatalf("lost-response invocation authorization=%v err=%v", replay, err)
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "authorize-invocation", "authorize-peer", "authorize-invocation"}) {
		t.Fatalf("invocation authorization crossed a business-input boundary: %v", calls)
	}
}

func TestRecoverRunningClaimNeverResolvesBusinessInputs(t *testing.T) {
	calls := []string{}
	lease := validLease()
	lease.DesiredState = runtimedomain.DesiredCancelled
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{
			calls:       &calls,
			lease:       lease,
			disposition: executionapp.ClaimRecoverRunningNoACK,
			watermark:   7,
		},
		inputResolverStub{calls: &calls, manifest: validIndexManifest()},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.ClaimCommand(context.Background(), claimRequestForIndexManifest(t, validIndexManifest()))
	if err != nil {
		t.Fatal(err)
	}
	receipt := response.GetReceipt()
	if response.GetRejection() != nil || receipt.GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK || receipt.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_CANCELLED {
		t.Fatalf("unexpected running recovery receipt: %v", response)
	}
	if receipt.GetInputBundleRef() != nil || receipt.GetInputBundle() != nil || receipt.GetSettlementRecovery() != nil || receipt.GetClaimStartedAtUnixMicros() != 0 {
		t.Fatalf("running recovery leaked business material: %v", receipt)
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-command", "claim"}) {
		t.Fatalf("running recovery reached business input: %v", calls)
	}
}

func TestRecoverAmbiguousInvocationClaimNeverResolvesBusinessInputs(t *testing.T) {
	calls := []string{}
	lease := validLease()
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{
			calls:       &calls,
			lease:       lease,
			disposition: executionapp.ClaimRecoverAmbiguousInvocationNoACK,
			watermark:   7,
		},
		inputResolverStub{calls: &calls, manifest: validIndexManifest()},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.ClaimCommand(context.Background(), claimRequestForIndexManifest(t, validIndexManifest()))
	if err != nil {
		t.Fatal(err)
	}
	receipt := response.GetReceipt()
	if response.GetRejection() != nil || receipt.GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RECOVER_AMBIGUOUS_INVOCATION_NOACK || receipt.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_RUNNING {
		t.Fatalf("unexpected ambiguous invocation receipt: %v", response)
	}
	if receipt.GetInputBundleRef() != nil || receipt.GetInputBundle() != nil || receipt.GetSettlementRecovery() != nil || receipt.GetClaimStartedAtUnixMicros() != 0 {
		t.Fatalf("ambiguous invocation recovery leaked business material: %v", receipt)
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-command", "claim"}) {
		t.Fatalf("ambiguous invocation recovery reached business input: %v", calls)
	}
}

func TestClaimCommandNeverVerifiesOrClaimsWhenPeerAuthorizationFails(t *testing.T) {
	calls := []string{}
	manifest := validManifest()
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls, err: errors.New("peer mismatch")},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{calls: &calls, lease: validLease()},
		inputResolverStub{calls: &calls, manifest: manifest},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, manifest))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED {
		t.Fatalf("unexpected rejection: %v", response)
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer"}) {
		t.Fatalf("unauthorized peer reached command/claim work: %v", calls)
	}
}

func TestClaimCommandReturnsRetryableDependencyWhenDurableBindingLookupFails(t *testing.T) {
	calls := []string{}
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{calls: &calls, lease: validLease(), claimErr: executionapp.ErrClaimDependencyUnavailable},
		inputResolverStub{calls: &calls, manifest: validManifest()},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetReceipt() != nil || response.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE || !response.GetRejection().GetRetryable() {
		t.Fatalf("durable lookup failure was not a retryable no-lease rejection: %v", response)
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-command", "claim"}) {
		t.Fatalf("durable lookup failure reached input/business work: %v", calls)
	}
}

func TestClaimCommandRejectsResolvedManifestDigestMismatchAfterClaim(t *testing.T) {
	admitted := validManifest()
	resolved := proto.Clone(admitted).(*runtimev1.ExecutionInputBundleV1)
	resolved.Entries[0].Content.ContentId = "tampered-content"
	calls := []string{}
	var abortDisposition executionapp.ClaimAbortDisposition
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{calls: &calls, lease: validLease(), abortDisposition: &abortDisposition},
		inputResolverStub{calls: &calls, manifest: resolved},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, admitted))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INCOMPATIBLE_VERSION {
		t.Fatalf("tampered manifest accepted: %v", response)
	}
	if abortDisposition != executionapp.ClaimAbortInputManifestInvalid {
		t.Fatalf("permanent manifest mismatch did not quarantine the fenced claim: %q", abortDisposition)
	}
	wantOrder := []string{"authorize-peer", "verify-command", "claim", "resolve-reference-manifest", "abort-claim"}
	if !reflect.DeepEqual(calls, wantOrder) {
		t.Fatalf("manifest was resolved before claim: %v", calls)
	}
}

func TestVerifyResolvedManifestBindsEveryIndexEntryExactly(t *testing.T) {
	server := &Server{config: testControlServerConfig()}
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(validRawIndexWorkerCommand(t), command); err != nil {
		t.Fatal(err)
	}
	manifest := validIndexManifest()
	if err := server.verifyResolvedManifest(inputReferenceForManifest(t, manifest), manifest, command); err != nil {
		t.Fatalf("valid index manifest rejected: %v", err)
	}

	wrongRole := proto.Clone(manifest).(*runtimev1.ExecutionInputBundleV1)
	wrongRole.Entries[1].SemanticRole = executiondomain.IndexToolkitConfigurationRole
	if err := server.verifyResolvedManifest(inputReferenceForManifest(t, wrongRole), wrongRole, command); err == nil {
		t.Fatal("index manifest with a mismatched semantic role was accepted")
	}

	extraEntry := proto.Clone(manifest).(*runtimev1.ExecutionInputBundleV1)
	extraEntry.Entries = append(extraEntry.Entries, &runtimev1.ExecutionInputEntryV1{
		EntryId:          "unbound",
		ImmutableVersion: "revision-1",
		SemanticRole:     "index.unbound",
		Content: &runtimev1.ScopedContentReferenceV1{
			ContentId:             "content-unbound",
			ImmutableVersion:      "revision-1",
			MediaType:             "application/json",
			ByteLength:            2,
			Digest:                testDigest([]byte(`{}`)),
			Classification:        "synthetic",
			RequiredGrantAudience: "elitea.runtime.input.read.v1",
		},
	})
	if err := server.verifyResolvedManifest(inputReferenceForManifest(t, extraEntry), extraEntry, command); err == nil {
		t.Fatal("index manifest with an unbound entry was accepted")
	}
}

func TestVerifyResolvedManifestBindsAgentProtobufEntryExactly(t *testing.T) {
	server := &Server{config: ServerConfig{
		MaxInputManifestBytes: 64 * 1024,
		MaxInputEntries:       16,
		MaxInputContentBytes:  executiondomain.MaxAgentExecutionInputBytes,
		MaxStringBytes:        256,
	}}
	command := &runtimev1.WorkerCommandV1{
		CapabilityId: executiondomain.AgentApplicationCapability,
		CapabilityCommand: &runtimev1.WorkerCommandV1_AgentExecution{
			AgentExecution: &runtimev1.AgentExecutionCommandV1{
				RequestEntryId: "agent-request",
			},
		},
	}
	manifest := &runtimev1.ExecutionInputBundleV1{
		InputBundleId:    "agent-bundle",
		ImmutableVersion: "agent-bundle-v1",
		Entries: []*runtimev1.ExecutionInputEntryV1{{
			EntryId:          "agent-request",
			ImmutableVersion: "agent-request-v1",
			SemanticRole:     executiondomain.AgentExecutionRequestRole,
			Content: &runtimev1.ScopedContentReferenceV1{
				ContentId:             "agent-content",
				ImmutableVersion:      "agent-request-v1",
				MediaType:             executiondomain.AgentExecutionInputMediaType,
				ByteLength:            344,
				Digest:                testDigest([]byte("agent request")),
				Classification:        "tenant-confidential",
				RequiredGrantAudience: "elitea.runtime.input.read.v1",
			},
		}},
	}
	if err := server.verifyResolvedManifest(inputReferenceForManifest(t, manifest), manifest, command); err != nil {
		t.Fatalf("valid agent manifest rejected: %v", err)
	}

	wrongMediaType := proto.Clone(manifest).(*runtimev1.ExecutionInputBundleV1)
	wrongMediaType.Entries[0].Content.MediaType = executiondomain.SettingsJSONMediaType
	if err := server.verifyResolvedManifest(inputReferenceForManifest(t, wrongMediaType), wrongMediaType, command); err == nil {
		t.Fatal("agent manifest with JSON content was accepted")
	}
}

func TestClaimCommandAcceptsBoundedIndexManifestAfterAuthorizedClaim(t *testing.T) {
	manifest := validIndexManifest()
	calls := []string{}
	var claimed executionapp.ClaimRequest
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{calls: &calls, lease: validLease(), claimRequest: &claimed},
		inputResolverStub{calls: &calls, manifest: manifest},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.ClaimCommand(context.Background(), claimRequestForIndexManifest(t, manifest))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRejection() != nil || response.GetReceipt().GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_ACCEPTED || len(response.GetReceipt().GetInputBundle().GetEntries()) != 3 {
		t.Fatalf("unexpected index claim response: %v", response)
	}
	wantOrder := []string{"authorize-peer", "verify-command", "claim", "resolve-reference-manifest"}
	if !reflect.DeepEqual(calls, wantOrder) {
		t.Fatalf("unsafe index claim order: got %v want %v", calls, wantOrder)
	}
	if claimed.CapabilityID != executiondomain.IndexIngestCapability {
		t.Fatalf("index command claimed as capability %q", claimed.CapabilityID)
	}
}

func TestClaimCommandAbortsInputResolutionFailureWithoutACKOrBusinessReceipt(t *testing.T) {
	tests := []struct {
		name          string
		claimAttempt  uint64
		resolveErr    error
		wantAbort     executionapp.ClaimAbortDisposition
		wantCode      runtimev1.RuntimeErrorCodeV1
		wantRetryable bool
	}{
		{
			name:          "transient release",
			claimAttempt:  1,
			resolveErr:    errors.New("temporary database failure"),
			wantAbort:     executionapp.ClaimAbortInputResolutionRetry,
			wantCode:      runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE,
			wantRetryable: true,
		},
		{
			name:          "transient retries exhausted",
			claimAttempt:  maxInputResolutionClaimAttempts,
			resolveErr:    errors.New("temporary database failure"),
			wantAbort:     executionapp.ClaimAbortInputResolutionExhausted,
			wantCode:      runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE,
			wantRetryable: false,
		},
		{
			name:          "durable input corruption",
			claimAttempt:  1,
			resolveErr:    executiondomain.ErrInvalidInputBundle,
			wantAbort:     executionapp.ClaimAbortInputManifestInvalid,
			wantCode:      runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INCOMPATIBLE_VERSION,
			wantRetryable: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			lease := validLease()
			lease.Fence.ClaimAttempt = test.claimAttempt
			var aborted executionapp.ClaimAbortDisposition
			server, err := NewServer(
				testControlServerConfig(),
				workloadAuthorizerStub{calls: &calls},
				verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
				claimControllerStub{calls: &calls, lease: lease, abortDisposition: &aborted},
				inputResolverStub{calls: &calls, err: test.resolveErr},
				&settlementControllerStub{calls: &calls},
			)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
			if err != nil {
				t.Fatal(err)
			}
			if response.GetReceipt() != nil || response.GetRejection().GetCode() != test.wantCode || response.GetRejection().GetRetryable() != test.wantRetryable {
				t.Fatalf("unsafe input-resolution response: %v", response)
			}
			if aborted != test.wantAbort {
				t.Fatalf("wrong fenced abort disposition: got %q want %q", aborted, test.wantAbort)
			}
			wantCalls := []string{"authorize-peer", "verify-command", "claim", "resolve-reference-manifest", "abort-claim"}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("input-resolution failure reached unexpected work: got %v want %v", calls, wantCalls)
			}
		})
	}
}

func TestClaimCommandReturnsRetryLaterWithoutLeaseForDurableQuarantine(t *testing.T) {
	calls := []string{}
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{
			calls:        &calls,
			disposition:  executionapp.ClaimRetryLaterNoACK,
			desiredState: runtimedomain.DesiredRunning,
		},
		inputResolverStub{calls: &calls, manifest: validManifest()},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
	if err != nil {
		t.Fatal(err)
	}
	receipt := response.GetReceipt()
	if response.GetRejection() != nil || receipt.GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RETRY_LATER_NOACK || receipt.GetFence() != nil || receipt.GetClaimId() != "" || receipt.GetInputBundle() != nil {
		t.Fatalf("quarantined command received a lease or business input: %v", response)
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-command", "claim"}) {
		t.Fatalf("quarantined command reached input/business work: %v", calls)
	}
}

func TestClaimCommandEmitsFenceFreeRetiredACKForDurableDeadline(t *testing.T) {
	calls := []string{}
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{
			calls:            &calls,
			disposition:      executionapp.ClaimRetiredACK,
			desiredState:     runtimedomain.DesiredRunning,
			retirementReason: executionapp.RetirementDeadlineExceeded,
		},
		inputResolverStub{calls: &calls, manifest: validManifest()},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
	if err != nil {
		t.Fatal(err)
	}
	receipt := response.GetReceipt()
	if response.GetRejection() != nil || receipt.GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RETIRED_ACK {
		t.Fatalf("unexpected retirement response: %v", response)
	}
	if receipt.GetRetirement().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED || receipt.GetRetirement().GetSafeMessage() != executionapp.DeadlineExceededSafeMessage || !receipt.GetRetirement().GetRetryable() {
		t.Fatalf("deadline retirement detail is not canonical: %v", receipt.GetRetirement())
	}
	if receipt.GetFence() != nil || receipt.GetLeaseExpiresAtUnixMillis() != 0 || receipt.GetClaimId() != "" || receipt.GetInputBundleRef() != nil || receipt.GetInputBundle() != nil || receipt.GetClaimHandoffWatermark() != 0 || receipt.GetSettlementRecovery() != nil {
		t.Fatalf("retired command received worker authority or business input: %v", receipt)
	}
	if want := []string{"authorize-peer", "verify-command", "claim"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("retired command reached a business plane: got %v want %v", calls, want)
	}
}

func TestClaimCommandRejectsMalformedRetirementDecision(t *testing.T) {
	tests := []struct {
		name   string
		claims claimControllerStub
	}{
		{
			name: "retired disposition missing reason",
			claims: claimControllerStub{
				disposition:  executionapp.ClaimRetiredACK,
				desiredState: runtimedomain.DesiredRunning,
			},
		},
		{
			name: "reason attached to retry",
			claims: claimControllerStub{
				disposition:      executionapp.ClaimRetryLaterNoACK,
				desiredState:     runtimedomain.DesiredRunning,
				retirementReason: executionapp.RetirementDeadlineExceeded,
			},
		},
		{
			name: "retirement carries watermark",
			claims: claimControllerStub{
				disposition:      executionapp.ClaimRetiredACK,
				desiredState:     runtimedomain.DesiredRunning,
				retirementReason: executionapp.RetirementDeadlineExceeded,
				watermark:        1,
			},
		},
		{
			name: "retirement carries lease",
			claims: claimControllerStub{
				lease:            validLease(),
				disposition:      executionapp.ClaimRetiredACK,
				retirementReason: executionapp.RetirementDeadlineExceeded,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			test.claims.calls = &calls
			server, err := NewServer(
				testControlServerConfig(),
				workloadAuthorizerStub{calls: &calls},
				verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
				test.claims,
				inputResolverStub{calls: &calls, manifest: validManifest()},
				&settlementControllerStub{calls: &calls},
			)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
			if err != nil {
				t.Fatal(err)
			}
			if response.GetReceipt() != nil || response.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL {
				t.Fatalf("malformed retirement decision escaped: %v", response)
			}
			if want := []string{"authorize-peer", "verify-command", "claim"}; !reflect.DeepEqual(calls, want) {
				t.Fatalf("malformed retirement reached input resolution: got %v want %v", calls, want)
			}
		})
	}
}

func TestClaimCommandEmitsFenceFreeObsoleteACKForDurableCancellation(t *testing.T) {
	calls := []string{}
	manifest := validManifest()
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{
			calls:        &calls,
			disposition:  executionapp.ClaimObsoleteACK,
			desiredState: runtimedomain.DesiredCancelled,
		},
		inputResolverStub{calls: &calls, manifest: manifest},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, manifest))
	if err != nil {
		t.Fatal(err)
	}
	receipt := response.GetReceipt()
	if response.GetRejection() != nil || receipt == nil {
		t.Fatalf("durable cancellation was rejected: %v", response)
	}
	if receipt.GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_OBSOLETE_ACK || receipt.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_CANCELLED {
		t.Fatalf("durable cancellation did not produce a Python-ACKable receipt: %v", receipt)
	}
	if receipt.GetFence() != nil || receipt.GetClaimId() != "" || receipt.GetLeaseExpiresAtUnixMillis() != 0 || receipt.GetInputBundleRef() != nil || receipt.GetInputBundle() != nil || receipt.GetSettlementRecovery() != nil || receipt.GetClaimHandoffWatermark() != 0 {
		t.Fatalf("no-lease obsolete receipt exposed authority, input, or recovery state: %v", receipt)
	}
	if receipt.GetIdentity().GetExecutionId() == "" || receipt.GetIdentity().GetGeneration() == 0 || receipt.GetIdentity().GetCommandId() == "" {
		t.Fatalf("obsolete receipt lost its command identity: %v", receipt)
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-command", "claim"}) {
		t.Fatalf("obsolete cancellation reached input or business work: %v", calls)
	}
}

func TestRenewLeaseBindsIdentityProducerAndExactFenceToken(t *testing.T) {
	calls := []string{}
	lease := validLease()
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{calls: &calls, lease: lease},
		inputResolverStub{calls: &calls, manifest: validManifest()},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := &runtimev1.RenewLeaseRequestV1{
		Identity:       identityForFence(lease.Fence),
		Fence:          fenceProto(lease.Fence),
		IdempotencyKey: "renew-1",
	}
	response, err := server.RenewLease(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRejection() != nil || response.GetLeaseExpiresAtUnixMillis() != lease.ExpiresAt.UnixMilli() {
		t.Fatalf("valid renewal rejected: %v", response)
	}

	wrong := proto.Clone(request).(*runtimev1.RenewLeaseRequestV1)
	wrong.Fence.FenceToken[0] ^= 0xff
	response, err = server.RenewLease(context.Background(), wrong)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE {
		t.Fatalf("wrong fence token accepted: %v", response)
	}
}

func TestClaimCommandNeverACKsCancellationWithoutDurableFinalization(t *testing.T) {
	calls := []string{}
	lease := validLease()
	lease.DesiredState = runtimedomain.DesiredCancelled
	manifest := validManifest()
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{calls: &calls, lease: lease},
		inputResolverStub{calls: &calls, manifest: manifest},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, manifest))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetReceipt().GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_ACTIVE_LEASE_NOACK || response.GetReceipt().GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_CANCELLED {
		t.Fatalf("cancelled command was unsafely acknowledged before durable finalization: %v", response)
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-command", "claim"}) {
		t.Fatalf("cancelled command resolved or exposed immutable input: %v", calls)
	}
}

func TestClaimCommandReturnsTypedSettlementRecoveryWithoutBusinessInput(t *testing.T) {
	lease := validLease()
	wireProposal := &runtimev1.SettlementProposalV1{
		ProposalId:              "proposal-recovery-1",
		RequestedOutcome:        runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED,
		TerminalLogicalOutputId: "validation:revision-1",
		TerminalEventId:         "event-1",
		TerminalSequence:        1,
		TerminalPayloadDigest:   testDigest([]byte("terminal-output")),
		PrepareIdempotencyKey:   "recover-terminal-1",
	}
	proposalBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(wireProposal)
	if err != nil {
		t.Fatal(err)
	}
	terminalDigest := runtimedomain.SHA256([]byte("terminal-output"))
	proposalDigest := runtimedomain.SHA256(proposalBytes)

	tests := []struct {
		name        string
		disposition executionapp.ClaimDisposition
		recovery    *executionapp.SettlementRecovery
		assert      func(*testing.T, *runtimev1.SettlementRecoveryV1)
	}{
		{
			name:        "terminal ACK recovery",
			disposition: executionapp.ClaimRecoverTerminalACK,
			recovery: &executionapp.SettlementRecovery{Proposal: &executionapp.SettlementProposal{
				Fence:                   lease.Fence,
				ProposalID:              wireProposal.GetProposalId(),
				Outcome:                 executionapp.SettlementSucceeded,
				TerminalLogicalOutputID: wireProposal.GetTerminalLogicalOutputId(),
				TerminalEventID:         wireProposal.GetTerminalEventId(),
				TerminalSequence:        wireProposal.GetTerminalSequence(),
				TerminalPayloadDigest:   terminalDigest,
				ProposalDigest:          proposalDigest,
				IdempotencyKey:          "recover-terminal-1",
			}},
			assert: func(t *testing.T, recovery *runtimev1.SettlementRecoveryV1) {
				t.Helper()
				if !proto.Equal(recovery.GetProposal(), wireProposal) || !validDigestProto(recovery.GetProposalDigest(), proposalBytes) || recovery.GetIdempotencyKey() != "recover-terminal-1" || recovery.GetSettlementReceiptId() != "" {
					t.Fatalf("persisted terminal proposal was not returned exactly: %v", recovery)
				}
			},
		},
		{
			name:        "prepared settlement recovery",
			disposition: executionapp.ClaimRecoverSettlement,
			recovery: &executionapp.SettlementRecovery{Receipt: &executionapp.SettlementReceipt{
				ID: "settlement-receipt-recovery-1", Outcome: executionapp.SettlementSucceeded,
			}},
			assert: func(t *testing.T, recovery *runtimev1.SettlementRecoveryV1) {
				t.Helper()
				if recovery.GetProposal() != nil || recovery.GetSettlementReceiptId() != "settlement-receipt-recovery-1" || recovery.GetOutcome() != runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED {
					t.Fatalf("prepared settlement receipt was not returned exactly: %v", recovery)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			manifest := validManifest()
			server, err := NewServer(
				testControlServerConfig(),
				workloadAuthorizerStub{calls: &calls},
				verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
				claimControllerStub{calls: &calls, lease: lease, disposition: test.disposition, recovery: test.recovery, watermark: 7},
				inputResolverStub{calls: &calls, manifest: manifest},
				&settlementControllerStub{calls: &calls},
			)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, manifest))
			if err != nil {
				t.Fatal(err)
			}
			wantDisposition, err := claimDispositionProto(test.disposition)
			if err != nil {
				t.Fatal(err)
			}
			if response.GetRejection() != nil || response.GetReceipt().GetDisposition() != wantDisposition || response.GetReceipt().GetClaimHandoffWatermark() != 7 {
				t.Fatalf("recovery claim was rejected: %v", response)
			}
			test.assert(t, response.GetReceipt().GetSettlementRecovery())
			if !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-command", "claim"}) {
				t.Fatalf("recovery disposition fetched input or reran business work: %v", calls)
			}
		})
	}
}

func TestObserveDesiredStateUsesPersistedGenerationAndFence(t *testing.T) {
	calls := []string{}
	lease := validLease()
	lease.DesiredState = runtimedomain.DesiredDraining
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{calls: &calls, lease: lease},
		inputResolverStub{calls: &calls, manifest: validManifest()},
		&settlementControllerStub{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.ObserveDesiredState(context.Background(), &runtimev1.ObserveDesiredStateRequestV1{
		Identity: identityForFence(lease.Fence),
		Fence:    fenceProto(lease.Fence),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRejection() != nil || response.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_DRAINING {
		t.Fatalf("persisted draining state was not observed: %v", response)
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "observe-desired-state"}) {
		t.Fatalf("unexpected desired-state path: %v", calls)
	}
}

func TestPrepareSettlementRequiresAuthenticatedFullFenceAndReturnsDurableReceipt(t *testing.T) {
	calls := []string{}
	lease := validLease()
	settlements := &settlementControllerStub{
		calls:   &calls,
		receipt: executionapp.SettlementReceipt{ID: "settlement-receipt-1", Outcome: executionapp.SettlementSucceeded},
	}
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{calls: &calls, lease: lease},
		inputResolverStub{calls: &calls, manifest: validManifest()},
		settlements,
	)
	if err != nil {
		t.Fatal(err)
	}
	proposal := &runtimev1.SettlementProposalV1{
		ProposalId:              "proposal-1",
		RequestedOutcome:        runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED,
		TerminalLogicalOutputId: "validation:revision-1",
		TerminalEventId:         "event-1",
		TerminalSequence:        1,
		TerminalPayloadDigest:   testDigest([]byte("terminal-output")),
		PrepareIdempotencyKey:   "settlement-key-1",
	}
	proposalBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.PrepareSettlement(context.Background(), &runtimev1.PrepareSettlementRequestV1{
		Identity:       identityForFence(lease.Fence),
		Fence:          fenceProto(lease.Fence),
		Proposal:       proposal,
		ProposalDigest: testDigest(proposalBytes),
		IdempotencyKey: "settlement-key-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRejection() != nil || response.GetSettlementReceiptId() != settlements.receipt.ID || response.GetOutcome() != runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED {
		t.Fatalf("valid settlement was not prepared: %v", response)
	}
	if len(settlements.proposals) != 1 || settlements.proposals[0].Fence != lease.Fence || settlements.proposals[0].TerminalSequence != 1 {
		t.Fatalf("settlement lost fence or terminal binding: %+v", settlements.proposals)
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "prepare-settlement"}) {
		t.Fatalf("unsafe settlement order: %v", calls)
	}
}

func TestPrepareSettlementDoesNotExposeReceiptAcrossAuthenticatedFences(t *testing.T) {
	calls := []string{}
	attackerFence := validLease().Fence
	attackerFence.WorkloadIdentity = "spiffe://elitea.test/workload/other"
	attackerFence.WorkloadSessionID = "other-session"
	attackerFence.ProducerID = "other-producer"
	attackerFence.ClaimAttempt++
	attackerFence.LeaseEpoch++
	attackerFence.Token = runtimedomain.FenceToken(runtimedomain.SHA256([]byte("other-token")))
	settlements := &settlementControllerStub{
		calls: &calls,
		err:   runtimedomain.ErrStaleFence,
	}
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: &calls, identity: attackerFence.WorkloadIdentity},
		verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
		claimControllerStub{calls: &calls, lease: validLease()},
		inputResolverStub{calls: &calls, manifest: validManifest()},
		settlements,
	)
	if err != nil {
		t.Fatal(err)
	}
	proposal := &runtimev1.SettlementProposalV1{
		ProposalId:              "proposal-1",
		RequestedOutcome:        runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED,
		TerminalLogicalOutputId: "validation:revision-1",
		TerminalEventId:         "event-1",
		TerminalSequence:        1,
		TerminalPayloadDigest:   testDigest([]byte("terminal-output")),
		PrepareIdempotencyKey:   "settlement-key-1",
	}
	proposalBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.PrepareSettlement(context.Background(), &runtimev1.PrepareSettlementRequestV1{
		Identity:       identityForFence(attackerFence),
		Fence:          fenceProto(attackerFence),
		Proposal:       proposal,
		ProposalDigest: testDigest(proposalBytes),
		IdempotencyKey: "settlement-key-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE || response.GetSettlementReceiptId() != "" || response.GetOutcome() != runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_UNSPECIFIED {
		t.Fatalf("different authenticated fence observed a settlement receipt: %v", response)
	}
	if len(settlements.proposals) != 1 || settlements.proposals[0].Fence != attackerFence {
		t.Fatalf("control did not preserve the authenticated requester fence: %+v", settlements.proposals)
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "prepare-settlement"}) {
		t.Fatalf("cross-fence replay reached unexpected control work: %v", calls)
	}
}

func TestClaimCommandRejectsStructurallyInvalidBoundManifest(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runtimev1.ExecutionInputBundleV1)
	}{
		{name: "duplicate entry ID", mutate: func(manifest *runtimev1.ExecutionInputBundleV1) {
			manifest.Entries = append(manifest.Entries, proto.Clone(manifest.Entries[0]).(*runtimev1.ExecutionInputEntryV1))
		}},
		{name: "entry-content version mismatch", mutate: func(manifest *runtimev1.ExecutionInputBundleV1) {
			manifest.Entries[0].Content.ImmutableVersion = "other-version"
		}},
		{name: "missing classification", mutate: func(manifest *runtimev1.ExecutionInputBundleV1) {
			manifest.Entries[0].Content.Classification = ""
		}},
		{name: "wrong selected semantic role", mutate: func(manifest *runtimev1.ExecutionInputBundleV1) {
			manifest.Entries[0].SemanticRole = "unrelated.input"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validManifest()
			test.mutate(manifest)
			calls := []string{}
			server, err := NewServer(
				testControlServerConfig(),
				workloadAuthorizerStub{calls: &calls},
				verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
				claimControllerStub{calls: &calls, lease: validLease()},
				inputResolverStub{calls: &calls, manifest: manifest},
				&settlementControllerStub{calls: &calls},
			)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, manifest))
			if err != nil {
				t.Fatal(err)
			}
			if response.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INCOMPATIBLE_VERSION {
				t.Fatalf("invalid manifest was accepted: %v", response)
			}
		})
	}
}

func validLease() runtimedomain.ActiveLease {
	token := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("unpredictable-test-token")))
	return runtimedomain.ActiveLease{
		ClaimID: "claim-1",
		Fence: runtimedomain.Fence{
			CommandID:         "command-1",
			ExecutionID:       "execution-1",
			Generation:        1,
			WorkloadIdentity:  "spiffe://elitea.test/workload/worker-1",
			WorkloadSessionID: "workload-1",
			ProducerID:        "worker-1",
			ClaimAttempt:      1,
			LeaseEpoch:        1,
			Token:             token,
		},
		ExpiresAt:    time.Date(2026, time.July, 16, 12, 1, 0, 0, time.UTC),
		DesiredState: runtimedomain.DesiredRunning,
	}
}

func testControlServerConfig() ServerConfig {
	return ServerConfig{
		MaxInputManifestBytes: 64 * 1024,
		MaxInputEntries:       16,
		MaxInputContentBytes:  256 * 1024,
		MaxStringBytes:        256,
	}
}

func validManifest() *runtimev1.ExecutionInputBundleV1 {
	return &runtimev1.ExecutionInputBundleV1{
		InputBundleId:    "bundle-1",
		ImmutableVersion: "bundle-v1",
		Entries: []*runtimev1.ExecutionInputEntryV1{{
			EntryId:          "settings",
			ImmutableVersion: "revision-1",
			SemanticRole:     "configuration.settings",
			Content: &runtimev1.ScopedContentReferenceV1{
				ContentId:             "content-1",
				ImmutableVersion:      "revision-1",
				MediaType:             "application/json",
				ByteLength:            2,
				Digest:                testDigest([]byte(`{}`)),
				Classification:        "synthetic",
				RequiredGrantAudience: "elitea.runtime.input.read.v1",
			},
		}},
	}
}

func validIndexManifest() *runtimev1.ExecutionInputBundleV1 {
	manifest := &runtimev1.ExecutionInputBundleV1{
		InputBundleId:    "bundle-1",
		ImmutableVersion: "bundle-v1",
	}
	for _, binding := range []struct {
		entryID   string
		role      string
		contentID string
	}{
		{entryID: "toolkit-configuration", role: executiondomain.IndexToolkitConfigurationRole, contentID: "content-toolkit"},
		{entryID: "tool-parameters", role: executiondomain.IndexToolParametersRole, contentID: "content-parameters"},
		{entryID: "embedding-binding", role: executiondomain.IndexEmbeddingBindingRole, contentID: "content-embedding"},
	} {
		manifest.Entries = append(manifest.Entries, &runtimev1.ExecutionInputEntryV1{
			EntryId:          binding.entryID,
			ImmutableVersion: "revision-1",
			SemanticRole:     binding.role,
			Content: &runtimev1.ScopedContentReferenceV1{
				ContentId:             binding.contentID,
				ImmutableVersion:      "revision-1",
				MediaType:             "application/json",
				ByteLength:            2,
				Digest:                testDigest([]byte(`{}`)),
				Classification:        "synthetic",
				RequiredGrantAudience: "elitea.runtime.input.read.v1",
			},
		})
	}
	return manifest
}

func inputReferenceForManifest(t *testing.T, manifest *runtimev1.ExecutionInputBundleV1) *runtimev1.ExecutionInputBundleReferenceV1 {
	t.Helper()
	manifestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return &runtimev1.ExecutionInputBundleReferenceV1{
		InputBundleId:    manifest.GetInputBundleId(),
		ImmutableVersion: manifest.GetImmutableVersion(),
		Digest:           testDigest(manifestBytes),
		ByteLength:       uint64(len(manifestBytes)),
		MediaType:        executiondomain.InputBundleManifestMediaType,
	}
}

func claimRequestForManifest(t *testing.T, manifest *runtimev1.ExecutionInputBundleV1) *runtimev1.ClaimCommandRequestV1 {
	t.Helper()
	manifestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(validRawWorkerCommand(t), command); err != nil {
		t.Fatal(err)
	}
	command.InputBundleRef.InputBundleId = manifest.GetInputBundleId()
	command.InputBundleRef.ImmutableVersion = manifest.GetImmutableVersion()
	command.InputBundleRef.ByteLength = uint64(len(manifestBytes))
	command.InputBundleRef.Digest = testDigest(manifestBytes)
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	return &runtimev1.ClaimCommandRequestV1{
		WorkloadSessionId: "workload-1",
		ProducerId:        "worker-1",
		SignedCommand:     signedEnvelope(raw),
	}
}

func claimRequestForIndexManifest(t *testing.T, manifest *runtimev1.ExecutionInputBundleV1) *runtimev1.ClaimCommandRequestV1 {
	t.Helper()
	manifestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(validRawIndexWorkerCommand(t), command); err != nil {
		t.Fatal(err)
	}
	command.InputBundleRef.InputBundleId = manifest.GetInputBundleId()
	command.InputBundleRef.ImmutableVersion = manifest.GetImmutableVersion()
	command.InputBundleRef.ByteLength = uint64(len(manifestBytes))
	command.InputBundleRef.Digest = testDigest(manifestBytes)
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	return &runtimev1.ClaimCommandRequestV1{
		WorkloadSessionId: "workload-1",
		ProducerId:        "worker-1",
		SignedCommand:     signedEnvelope(raw),
	}
}

func identityForFence(fence runtimedomain.Fence) *runtimev1.ExecutionIdentityV1 {
	return &runtimev1.ExecutionIdentityV1{
		TenantId:            "tenant-1",
		ResourceProjectId:   "project-1",
		ProjectionProjectId: "project-1",
		CommandId:           fence.CommandID,
		ExecutionId:         fence.ExecutionID,
		Generation:          fence.Generation,
	}
}

// ClaimCommand resolves the bundle manifest against this map, so a role the map
// does not name is a content the worker cannot fetch. Getting the two tool-run
// roles right here is what makes a claimed tool run able to read its own
// settings; getting them WRONG is a 403 with no explanation at the data plane.
func TestExpectedInputRolesNamesBothToolRunEntries(t *testing.T) {
	command := &runtimev1.WorkerCommandV1{
		CapabilityId: executiondomain.ToolkitCallToolCapability,
		CapabilityCommand: &runtimev1.WorkerCommandV1_ToolkitCallTool{
			ToolkitCallTool: &runtimev1.ToolkitCallToolCommandV1{
				SettingsEntryId:  "toolkit-settings",
				ArgumentsEntryId: "tool-arguments",
			},
		},
	}

	roles, err := expectedInputRoles(command)
	if err != nil {
		t.Fatalf("expectedInputRoles() error = %v", err)
	}
	if roles["toolkit-settings"] != executiondomain.ToolkitCallToolSettingsRole ||
		roles["tool-arguments"] != executiondomain.ToolkitCallToolArgumentsRole ||
		len(roles) != 2 {
		t.Fatalf("unexpected tool-run input roles: %v", roles)
	}
}

func TestExpectedInputRolesRefusesAToolRunWithOneEntryForBoth(t *testing.T) {
	command := &runtimev1.WorkerCommandV1{
		CapabilityId: executiondomain.ToolkitCallToolCapability,
		CapabilityCommand: &runtimev1.WorkerCommandV1_ToolkitCallTool{
			ToolkitCallTool: &runtimev1.ToolkitCallToolCommandV1{
				SettingsEntryId:  "one-entry",
				ArgumentsEntryId: "one-entry",
			},
		},
	}

	if _, err := expectedInputRoles(command); err == nil {
		t.Fatal("expectedInputRoles() accepted one entry for settings and arguments")
	}
}
