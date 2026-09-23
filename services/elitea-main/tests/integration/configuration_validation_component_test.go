package integration_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	configurationapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	executionapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/executions"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	configurationdomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/configurations"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/redisdispatch"
	controltransport "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/control"
	outputtransport "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/output"
	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// This is a deterministic in-process vertical integration test over the exact
// checked Python/Go corpus. It intentionally does not claim networked
// PostgreSQL, Redis, or gRPC-listener coverage; those adapters require a
// separately activated service-backed suite.
func TestConfigurationValidationVerticalComponentAgainstCheckedCorpus(t *testing.T) {
	for _, fixture := range []string{"valid", "invalid", "unsupported"} {
		t.Run(fixture, func(t *testing.T) {
			envelope, manifest, expectedOutput := readCorpus(t, fixture)
			command := decodeCommand(t, envelope.GetSignedCommand().GetWorkerCommandBytes())
			settings := mustReadCorpusFile(t, fixture, "settings.json")
			if !bytes.Equal(mustMarshal(t, manifest), mustReadCorpusFile(t, fixture, "input-bundle.pb")) {
				t.Fatal("corpus input manifest is not deterministic protobuf")
			}

			now := time.UnixMilli(1_700_000_000_000).UTC()
			publishedCommand := admitAndDispatchCorpus(t, command, manifest, settings, envelope.GetSignedCommand(), now)
			state := newMemoryRuntime(t, command, envelope.GetFence(), manifest, expectedOutput, now)
			claims, err := executionapp.NewClaimService(state, func() time.Time { return now }, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			settlements, err := executionapp.NewSettlementService(state)
			if err != nil {
				t.Fatal(err)
			}
			verifier := newCorpusVerifier(t)
			authorizer := corpusAuthorizer{
				workloadSessionID: envelope.GetFence().GetWorkloadSessionId(),
				producerID:        envelope.GetFence().GetProducerId(),
			}
			controlServer, err := controltransport.NewServer(
				controltransport.ServerConfig{
					MaxInputManifestBytes: 64 * 1024,
					MaxInputEntries:       16,
					MaxInputContentBytes:  256 * 1024,
					MaxStringBytes:        256,
				},
				authorizer,
				verifier,
				claims,
				corpusInputResolver{manifest: manifest},
				settlements,
			)
			if err != nil {
				t.Fatal(err)
			}

			claim, err := controlServer.ClaimCommand(context.Background(), &runtimev1.ClaimCommandRequestV1{
				WorkloadSessionId: envelope.GetFence().GetWorkloadSessionId(),
				ProducerId:        envelope.GetFence().GetProducerId(),
				SignedCommand:     publishedCommand,
			})
			if err != nil {
				t.Fatal(err)
			}
			if claim.GetRejection() != nil || claim.GetReceipt().GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_ACCEPTED || !proto.Equal(claim.GetReceipt().GetInputBundle(), manifest) {
				t.Fatalf("checked command was not claimed with its exact immutable manifest: %v", claim)
			}

			validationService, err := outputapp.NewConfigurationValidationService(state, claims, state)
			if err != nil {
				t.Fatal(err)
			}
			failureService, err := outputapp.NewRuntimeFailureService(state, claims, state)
			if err != nil {
				t.Fatal(err)
			}
			outputServer, err := outputtransport.NewServer(outputtransport.ServerConfig{
				OutputSchemaRevision: "elitea.runtime.execution-output.v1",
				MaxFrameBytes:        64 * 1024,
				CreditFrames:         8,
				CreditBytes:          64 * 1024,
			}, authorizer, validationService, failureService)
			if err != nil {
				t.Fatal(err)
			}
			stream := &corpusOutputStream{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{expectedOutput}}
			if err := outputServer.Publish(stream); err != nil {
				t.Fatal(err)
			}
			if len(stream.acks) != 2 || stream.acks[0].GetCreditFrames() != 8 || stream.acks[1].GetRejection() != nil || stream.acks[1].GetCommittedContiguousSequence() != expectedOutput.GetSequence() {
				t.Fatalf("dedicated output path did not durably acknowledge the corpus result: %v", stream.acks)
			}
			if fixture == "unsupported" {
				if state.validationProjections != 0 || state.failureProjections != 1 {
					t.Fatalf("unsupported capability was not preserved as runtime failure: validation=%d failure=%d", state.validationProjections, state.failureProjections)
				}
			} else if state.validationProjections != 1 || state.failureProjections != 0 {
				t.Fatalf("validation result was routed incorrectly: validation=%d failure=%d", state.validationProjections, state.failureProjections)
			}

			proposalBytes := mustMarshal(t, expectedOutput.GetSettlementProposal())
			settlement, err := controlServer.PrepareSettlement(context.Background(), &runtimev1.PrepareSettlementRequestV1{
				Identity:       expectedOutput.GetIdentity(),
				Fence:          expectedOutput.GetFence(),
				Proposal:       expectedOutput.GetSettlementProposal(),
				ProposalDigest: digestProto(runtimedomain.SHA256(proposalBytes)),
				IdempotencyKey: expectedOutput.GetSettlementProposal().GetPrepareIdempotencyKey(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if settlement.GetRejection() != nil || settlement.GetSettlementReceiptId() == "" || state.settlementCount != 1 {
				t.Fatalf("durable terminal result could not be settled: response=%v count=%d", settlement, state.settlementCount)
			}

			assertDurableSSEReplay(t, state, command.GetProjectionProjectId(), command.GetExecutionId(), settings)
		})
	}
}

func TestNeverStartedCancellationVerticalComponentReturnsFenceFreeObsoleteACK(t *testing.T) {
	envelope, _, _ := readCorpus(t, "valid")
	command := decodeCommand(t, envelope.GetSignedCommand().GetWorkerCommandBytes())
	now := time.UnixMilli(1_700_000_000_000).UTC()
	state := &neverStartedCancellationRuntime{
		commandID:            command.GetCommandId(),
		outboxID:             command.GetIdempotencyKey(),
		executionID:          command.GetExecutionId(),
		generation:           command.GetGeneration(),
		publishedDigest:      runtimedomain.SHA256(mustMarshal(t, envelope.GetSignedCommand())),
		workloadIdentity:     "spiffe://elitea.test/workload/python-reference",
		workloadSessionID:    envelope.GetFence().GetWorkloadSessionId(),
		producerID:           envelope.GetFence().GetProducerId(),
		state:                executiondomain.JobDispatched,
		desired:              runtimedomain.DesiredCancelled,
		finalizationClockNow: now,
	}
	claims, err := executionapp.NewClaimService(state, func() time.Time { return now }, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	inputs := &neverCalledInputResolver{}
	settlements := &neverCalledSettlementController{}
	server, err := controltransport.NewServer(
		controltransport.ServerConfig{
			MaxInputManifestBytes: 64 * 1024,
			MaxInputEntries:       16,
			MaxInputContentBytes:  256 * 1024,
			MaxStringBytes:        256,
		},
		corpusAuthorizer{
			workloadSessionID: envelope.GetFence().GetWorkloadSessionId(),
			producerID:        envelope.GetFence().GetProducerId(),
		},
		newCorpusVerifier(t),
		claims,
		inputs,
		settlements,
	)
	if err != nil {
		t.Fatal(err)
	}
	request := &runtimev1.ClaimCommandRequestV1{
		WorkloadSessionId: envelope.GetFence().GetWorkloadSessionId(),
		ProducerId:        envelope.GetFence().GetProducerId(),
		SignedCommand:     envelope.GetSignedCommand(),
	}

	for attempt := 1; attempt <= 2; attempt++ {
		response, claimErr := server.ClaimCommand(context.Background(), request)
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		receipt := response.GetReceipt()
		if response.GetRejection() != nil || receipt.GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_OBSOLETE_ACK || receipt.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_CANCELLED {
			t.Fatalf("attempt %d did not return an ACK-safe durable cancellation: %v", attempt, response)
		}
		if receipt.GetFence() != nil || receipt.GetClaimId() != "" || receipt.GetLeaseExpiresAtUnixMillis() != 0 || receipt.GetInputBundleRef() != nil || receipt.GetInputBundle() != nil || receipt.GetSettlementRecovery() != nil {
			t.Fatalf("attempt %d exposed worker authority/input for cancelled work: %v", attempt, receipt)
		}
		if receipt.GetIdentity().GetCommandId() != command.GetCommandId() || receipt.GetIdentity().GetExecutionId() != command.GetExecutionId() || receipt.GetIdentity().GetGeneration() != command.GetGeneration() {
			t.Fatalf("attempt %d lost the authenticated command identity: %v", attempt, receipt.GetIdentity())
		}
	}
	if state.state != executiondomain.JobCancelled || state.settledAt != now || state.finalizationCount != 1 {
		t.Fatalf("cancellation was not durably idempotent: state=%s settled_at=%s count=%d", state.state, state.settledAt, state.finalizationCount)
	}
	if inputs.calls != 0 || settlements.calls != 0 {
		t.Fatalf("obsolete cancellation reached input or settlement work: input=%d settlement=%d", inputs.calls, settlements.calls)
	}
}

type neverStartedCancellationRuntime struct {
	commandID            string
	outboxID             string
	executionID          string
	generation           uint64
	publishedDigest      runtimedomain.Digest
	workloadIdentity     string
	workloadSessionID    string
	producerID           string
	state                executiondomain.JobState
	desired              runtimedomain.DesiredState
	settledAt            time.Time
	finalizationClockNow time.Time
	finalizationCount    int
}

func (r *neverStartedCancellationRuntime) ClaimValidation(_ context.Context, request executionapp.ClaimRequest, _ executionapp.ClaimLeaseTTLMillis) (executionapp.ClaimDecision, error) {
	if request.CommandID != r.commandID || request.OutboxID != r.outboxID || request.ExecutionID != r.executionID || request.Generation != r.generation || request.SignedEnvelopeDigest != r.publishedDigest || request.WorkloadIdentity != r.workloadIdentity || request.WorkloadSessionID != r.workloadSessionID || request.ProducerID != r.producerID {
		return executionapp.ClaimDecision{}, executionapp.ErrInvalidClaim
	}
	if r.desired != runtimedomain.DesiredCancelled {
		return executionapp.ClaimDecision{Disposition: executionapp.ClaimRetryLaterNoACK, DesiredState: r.desired}, nil
	}
	switch r.state {
	case executiondomain.JobPending, executiondomain.JobDispatched:
		r.state = executiondomain.JobCancelled
		r.settledAt = r.finalizationClockNow
		r.finalizationCount++
	case executiondomain.JobCancelled:
	default:
		return executionapp.ClaimDecision{Disposition: executionapp.ClaimRetryLaterNoACK, DesiredState: r.desired}, nil
	}
	return executionapp.ClaimDecision{Disposition: executionapp.ClaimObsoleteACK, DesiredState: runtimedomain.DesiredCancelled}, nil
}

func (r *neverStartedCancellationRuntime) CurrentLease(context.Context, string, uint64) (runtimedomain.ActiveLease, time.Time, error) {
	return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrStaleFence
}

func (r *neverStartedCancellationRuntime) RenewLease(context.Context, runtimedomain.Fence, executionapp.ClaimLeaseTTLMillis) (runtimedomain.ActiveLease, time.Time, error) {
	return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrStaleFence
}

func (r *neverStartedCancellationRuntime) ReleaseClaim(context.Context, runtimedomain.Fence) error {
	return runtimedomain.ErrStaleFence
}

func (r *neverStartedCancellationRuntime) AbortClaim(context.Context, runtimedomain.Fence, executionapp.ClaimAbortDisposition) error {
	return runtimedomain.ErrStaleFence
}

func (r *neverStartedCancellationRuntime) DesiredState(_ context.Context, executionID string, generation uint64) (runtimedomain.DesiredState, error) {
	if executionID != r.executionID || generation != r.generation {
		return "", runtimedomain.ErrStaleFence
	}
	return r.desired, nil
}

type neverCalledInputResolver struct{ calls int }

func (r *neverCalledInputResolver) ResolveClaimInput(context.Context, runtimedomain.Fence, *runtimev1.ExecutionInputBundleReferenceV1) (*runtimev1.ExecutionInputBundleV1, error) {
	r.calls++
	return nil, errors.New("obsolete cancellation must not resolve input")
}

type neverCalledSettlementController struct{ calls int }

func (r *neverCalledSettlementController) PrepareSettlement(context.Context, executionapp.SettlementProposal) (executionapp.SettlementReceipt, error) {
	r.calls++
	return executionapp.SettlementReceipt{}, errors.New("obsolete cancellation must not prepare settlement")
}

type corpusTargetResolver struct {
	target   configurationapp.ValidationTarget
	identity executionapp.AdmissionIdentity
	revision string
}

type corpusAdmissionAuthorizer struct {
	identity   executionapp.AdmissionIdentity
	projectID  string
	revisionID string
	calls      int
}

func (a *corpusAdmissionAuthorizer) AuthorizeValidation(_ context.Context, projectID, revisionID string) (executionapp.AdmissionIdentity, error) {
	a.calls++
	if projectID != a.projectID || revisionID != a.revisionID {
		return executionapp.AdmissionIdentity{}, configurationapi.ErrValidationForbidden
	}
	return a.identity, nil
}

func (r corpusTargetResolver) ResolveValidationTarget(_ context.Context, identity executionapp.AdmissionIdentity, revision string) (configurationapp.ValidationTarget, error) {
	if identity != r.identity || revision != r.revision {
		return configurationapp.ValidationTarget{}, errors.New("validation target identity mismatch")
	}
	return r.target, nil
}

type verticalStore struct {
	template        executionapp.ValidationDispatch
	admission       *executionapp.ValidationAdmission
	dispatch        executionapp.ValidationDispatch
	prepared        *executionapp.StoredPreparedEnvelope
	publishedDigest runtimedomain.Digest
}

func (s *verticalStore) AdmitValidation(_ context.Context, admission executionapp.ValidationAdmission) (executionapp.AdmissionOutcome, error) {
	if s.admission != nil {
		if s.admission.Record.RequestDigest != admission.Record.RequestDigest {
			return executionapp.AdmissionOutcome{}, executionapp.ErrIdempotencyConflict
		}
		return executionapp.AdmissionOutcome{
			ExecutionID: s.admission.Record.Job.ID,
			CommandID:   s.admission.Record.Job.CommandID,
			AdmittedAt:  s.template.Deadline.Add(-time.Minute),
			Deadline:    s.template.Deadline,
		}, nil
	}
	copy := admission
	copy.Record.InputBundle = admission.Record.InputBundle.Clone()
	s.admission = &copy
	dispatch := s.template
	dispatch.OutboxID = admission.Record.Outbox.ID
	dispatch.CommandID = admission.Record.Job.CommandID
	dispatch.ExecutionID = admission.Record.Job.ID
	dispatch.Generation = admission.Record.Job.Generation
	dispatch.TenantID = admission.Record.Job.TenantID
	dispatch.ResourceProjectID = admission.Record.Job.ResourceProjectID
	dispatch.ProjectionProjectID = admission.Record.Job.ProjectionProjectID
	dispatch.PrincipalRef = admission.Record.Job.ActorID
	dispatch.InputBundleID = admission.Record.InputBundle.ID
	dispatch.InputBundleVersion = admission.Record.InputBundle.Version
	dispatch.InputBundleMediaType = admission.Record.InputBundle.MediaType
	dispatch.InputBundleByteLength = uint64(len(admission.Record.InputBundle.Manifest))
	dispatch.InputBundleDigest = admission.Record.InputBundle.Digest
	dispatch.Command = admission.Command
	s.dispatch = dispatch
	return executionapp.AdmissionOutcome{
		ExecutionID: admission.Record.Job.ID,
		CommandID:   admission.Record.Job.CommandID,
		Created:     true,
		AdmittedAt:  s.template.Deadline.Add(-time.Minute),
		Deadline:    s.template.Deadline,
	}, nil
}

func (s *verticalStore) LoadPendingValidation(_ context.Context, outboxID string) (executionapp.ValidationDispatch, error) {
	if s.admission == nil || outboxID != s.dispatch.OutboxID || !s.publishedDigest.IsZero() {
		return executionapp.ValidationDispatch{}, errors.New("pending validation outbox not found")
	}
	return s.dispatch, nil
}

func (s *verticalStore) LoadPreparedValidation(_ context.Context, outboxID string) (*executionapp.StoredPreparedEnvelope, error) {
	if s.admission == nil || outboxID != s.dispatch.OutboxID {
		return nil, errors.New("validation outbox not found")
	}
	if s.prepared == nil {
		return nil, nil
	}
	copy := *s.prepared
	copy.Envelope = copy.Envelope.Clone()
	return &copy, nil
}

func (s *verticalStore) StorePreparedValidation(_ context.Context, outboxID string, candidate executionapp.PreparedCommandEnvelope) (executionapp.StoredPreparedEnvelope, error) {
	if s.admission == nil || outboxID != s.dispatch.OutboxID || !s.publishedDigest.IsZero() {
		return executionapp.StoredPreparedEnvelope{}, errors.New("pending validation outbox not found")
	}
	if s.prepared == nil {
		s.prepared = &executionapp.StoredPreparedEnvelope{Envelope: candidate.Clone()}
	}
	copy := *s.prepared
	copy.Envelope = copy.Envelope.Clone()
	return copy, nil
}

func (s *verticalStore) MarkValidationPublished(_ context.Context, outboxID string, digest runtimedomain.Digest) error {
	if outboxID != s.dispatch.OutboxID || s.prepared == nil || digest != s.prepared.Envelope.Digest {
		return errors.New("published outbox identity mismatch")
	}
	s.publishedDigest = digest
	s.prepared.Published = true
	return nil
}

type corpusSigner struct{}

func (corpusSigner) SignWorkerCommand(_ context.Context, exact []byte) (redisdispatch.Signature, error) {
	mac := hmac.New(sha256.New, []byte("ELITEA_RUNTIME_V1_TEST_ONLY_NOT_A_SECRET"))
	_, _ = mac.Write(exact)
	return redisdispatch.Signature{
		Profile: runtimev1.SignatureProfileV1_SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
		KeyID:   "elitea-runtime-v1-conformance-hmac",
		Value:   mac.Sum(nil),
	}, nil
}

type captureAppender struct {
	stream     string
	field      string
	deliveryID string
	value      []byte
	count      int
}

func (a *captureAppender) Append(_ context.Context, stream, field, deliveryID string, value []byte) (string, error) {
	a.count++
	a.stream = stream
	a.field = field
	a.deliveryID = deliveryID
	a.value = append([]byte(nil), value...)
	return "1-0", nil
}

func admitAndDispatchCorpus(t *testing.T, command *runtimev1.WorkerCommandV1, manifest *runtimev1.ExecutionInputBundleV1, settings []byte, expectedSigned *runtimev1.SignedWorkerCommandEnvelopeV1, now time.Time) *runtimev1.SignedWorkerCommandEnvelopeV1 {
	t.Helper()
	validation := command.GetConfigurationValidation()
	entry := findSettingsEntry(t, manifest, validation.GetSettingsEntryId())
	inputIDs := []string{manifest.GetInputBundleId(), entry.GetContent().GetContentId()}
	inputFactory := executionapp.NewConformanceValidationInputBundleFactory(func() (string, error) {
		if len(inputIDs) == 0 {
			return "", errors.New("unexpected input ID allocation")
		}
		id := inputIDs[0]
		inputIDs = inputIDs[1:]
		return id, nil
	})
	jobIDs := []string{command.GetExecutionId(), command.GetCommandId(), command.GetIdempotencyKey()}
	store := &verticalStore{template: executionapp.ValidationDispatch{
		DispatchOrdinal:   command.GetDispatchOrdinal(),
		CapabilityVersion: command.GetCapabilityVersion(),
		ResourceClass:     command.GetResourceClass(),
		IsolationClass:    command.GetIsolationClass(),
		Priority:          command.GetPriority(),
		Deadline:          time.UnixMilli(command.GetDeadlineUnixMillis()).UTC(),
		LimitsRevision:    command.GetLimitsRevision(),
		Traceparent:       command.GetTraceparent(),
		Tracestate:        command.GetTracestate(),
	}}
	jobs, err := executionapp.NewSubmitJobService(store, func() time.Time { return now }, func() (string, error) {
		if len(jobIDs) == 0 {
			return "", errors.New("unexpected job ID allocation")
		}
		id := jobIDs[0]
		jobIDs = jobIDs[1:]
		return id, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := executionapp.AdmissionIdentity{
		TenantID:            command.GetTenantId(),
		ResourceProjectID:   command.GetResourceProjectId(),
		ProjectionProjectID: command.GetProjectionProjectId(),
		ActorID:             command.GetPrincipalRef(),
	}
	targets := corpusTargetResolver{
		identity: identity,
		revision: validation.GetConfigurationRevisionId(),
		target: configurationapp.ValidationTarget{
			ConfigurationType:      validation.GetConfigurationType(),
			CatalogRevision:        validation.GetCatalogRevision(),
			CatalogDigest:          mustDomainDigest(t, validation.GetCatalogDigest()),
			SchemaID:               validation.GetSchemaId(),
			SchemaRevision:         validation.GetSchemaRevision(),
			SchemaDigest:           mustDomainDigest(t, validation.GetSchemaDigest()),
			SettingsEntryID:        validation.GetSettingsEntryId(),
			SettingsVersion:        entry.GetImmutableVersion(),
			ExpectedSettingsDigest: runtimedomain.SHA256(settings),
		},
	}
	admission, err := configurationapp.NewSubmitValidationService(targets, inputFactory, jobs)
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &corpusAdmissionAuthorizer{
		identity:   identity,
		projectID:  command.GetResourceProjectId(),
		revisionID: validation.GetConfigurationRevisionId(),
	}
	handler, err := configurationapi.NewValidationHandler(authorizer, admission)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 0, len(settings)+len(`{"settings":}`))
	body = append(body, `{"settings":`...)
	body = append(body, settings...)
	body = append(body, '}')
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", command.GetIdempotencyKey())
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", command.GetResourceProjectId())
	routeContext.URLParams.Add("configurationRevisionID", validation.GetConfigurationRevisionId())
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	response := httptest.NewRecorder()
	handler.Submit(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("validation HTTP admission returned %d: %s", response.Code, response.Body.String())
	}
	var admitted struct {
		ExecutionID string `json:"execution_id"`
		CommandID   string `json:"command_id"`
		Created     bool   `json:"created"`
	}
	if err := json.NewDecoder(response.Body).Decode(&admitted); err != nil {
		t.Fatal(err)
	}
	if authorizer.calls != 1 {
		t.Fatalf("trusted validation authorizer called %d times", authorizer.calls)
	}
	if !admitted.Created || admitted.ExecutionID != command.GetExecutionId() || admitted.CommandID != command.GetCommandId() || store.admission == nil {
		t.Fatalf("Go HTTP admission did not durably create the corpus job/outbox: outcome=%+v admission=%+v", admitted, store.admission)
	}
	job := store.admission.Record.Job
	if job.TenantID != identity.TenantID || job.ResourceProjectID != identity.ResourceProjectID || job.ProjectionProjectID != identity.ProjectionProjectID || job.ActorID != identity.ActorID || store.admission.Record.Outbox.ID != command.GetIdempotencyKey() || job.State != executiondomain.JobPending || !bytes.Equal(store.admission.Record.InputBundle.Manifest, mustMarshal(t, manifest)) || len(store.admission.Record.InputBundle.Entries) != 1 || !bytes.Equal(store.admission.Record.InputBundle.Entries[0].Content, settings) {
		t.Fatal("durable admission changed the trusted identity or immutable corpus input/job/outbox")
	}

	appender := &captureAppender{}
	producer, err := redisdispatch.NewProducer(redisdispatch.ProducerConfig{
		Stream:                 "commands.v1.configuration.validate.short-validation.conformance.1.0",
		ProtocolRevision:       "elitea.runtime.v1",
		EnvelopeSchemaRevision: "elitea.runtime.signed-worker-command.v1",
		AllowTestOnlyHMAC:      true,
		Limits: redisdispatch.Limits{
			Revision:               "elitea.runtime.limits.conformance.v2",
			MaxWorkerCommandBytes:  32 * 1024,
			MaxSignedEnvelopeBytes: 48 * 1024,
			MaxRedisFieldBytes:     48 * 1024,
			MaxRedisEntryBytes:     64 * 1024,
			MaxSignatureBytes:      256,
			MaxStringBytes:         256,
		},
	}, corpusSigner{}, appender)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := executionapp.NewValidationDispatcher(store, producer)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Dispatch(context.Background(), command.GetIdempotencyKey()); err != nil {
		t.Fatal(err)
	}
	if appender.count != 1 || appender.field != "signed_envelope" || appender.deliveryID != command.GetIdempotencyKey() || store.publishedDigest != runtimedomain.SHA256(appender.value) {
		t.Fatalf("durable outbox did not publish one bounded Redis reference: count=%d field=%q", appender.count, appender.field)
	}
	if !bytes.Equal(appender.value, mustMarshal(t, expectedSigned)) {
		t.Fatal("Go admission/dispatcher produced bytes different from the checked cross-language signed command")
	}
	published := &runtimev1.SignedWorkerCommandEnvelopeV1{}
	if err := proto.Unmarshal(appender.value, published); err != nil {
		t.Fatal(err)
	}
	return published
}

type corpusAuthorizer struct {
	workloadSessionID string
	producerID        string
}

func (a corpusAuthorizer) authorize(workloadSessionID, producerID string) error {
	if workloadSessionID != a.workloadSessionID || producerID != a.producerID {
		return errors.New("authenticated workload identity mismatch")
	}
	return nil
}

func (a corpusAuthorizer) AuthorizeWorkload(_ context.Context, workloadSessionID, producerID string) (string, error) {
	if err := a.authorize(workloadSessionID, producerID); err != nil {
		return "", err
	}
	return "spiffe://elitea.test/workload/python-reference", nil
}

func (a corpusAuthorizer) AuthorizeOutput(_ context.Context, workloadSessionID, producerID string) (string, error) {
	if err := a.authorize(workloadSessionID, producerID); err != nil {
		return "", err
	}
	return "spiffe://elitea.test/workload/python-reference", nil
}

type corpusInputResolver struct {
	manifest *runtimev1.ExecutionInputBundleV1
}

func (r corpusInputResolver) ResolveClaimInput(_ context.Context, _ runtimedomain.Fence, reference *runtimev1.ExecutionInputBundleReferenceV1) (*runtimev1.ExecutionInputBundleV1, error) {
	encoded := mustMarshalWithoutTest(r.manifest)
	if reference.GetInputBundleId() != r.manifest.GetInputBundleId() || reference.GetImmutableVersion() != r.manifest.GetImmutableVersion() || reference.GetByteLength() != uint64(len(encoded)) || !digestMatches(reference.GetDigest(), encoded) {
		return nil, errors.New("immutable manifest reference mismatch")
	}
	return proto.Clone(r.manifest).(*runtimev1.ExecutionInputBundleV1), nil
}

type projectedTerminal struct {
	settlement        executionapp.SettlementProposal
	encodedSettlement []byte
}

type memoryRuntime struct {
	lease                 runtimedomain.ActiveLease
	now                   time.Time
	desired               runtimedomain.DesiredState
	outboxID              string
	expected              outputapp.ExpectedValidation
	terminal              *projectedTerminal
	settlementByKey       map[string]executionapp.SettlementReceipt
	settlementDigestByKey map[string]runtimedomain.Digest
	validationProjections int
	failureProjections    int
	settlementCount       int
	eventCursor           uint64
	events                []executionapi.DurableEvent
}

func newMemoryRuntime(t *testing.T, command *runtimev1.WorkerCommandV1, wireFence *runtimev1.ExecutionFenceV1, manifest *runtimev1.ExecutionInputBundleV1, output *runtimev1.ExecutionOutputFrameV1, now time.Time) *memoryRuntime {
	t.Helper()
	var token runtimedomain.FenceToken
	copy(token[:], wireFence.GetFenceToken())
	fence := runtimedomain.Fence{
		CommandID:         command.GetCommandId(),
		ExecutionID:       command.GetExecutionId(),
		Generation:        command.GetGeneration(),
		WorkloadIdentity:  "spiffe://elitea.test/workload/python-reference",
		WorkloadSessionID: wireFence.GetWorkloadSessionId(),
		ProducerID:        wireFence.GetProducerId(),
		ClaimAttempt:      wireFence.GetClaimAttempt(),
		LeaseEpoch:        wireFence.GetLeaseEpoch(),
		Token:             token,
	}
	validation := command.GetConfigurationValidation()
	settingsEntry := findSettingsEntry(t, manifest, validation.GetSettingsEntryId())
	expected := outputapp.ExpectedValidation{
		TenantID:            command.GetTenantId(),
		ResourceProjectID:   command.GetResourceProjectId(),
		ProjectionProjectID: command.GetProjectionProjectId(),
		CommandID:           command.GetCommandId(),
		ExecutionID:         command.GetExecutionId(),
		Generation:          command.GetGeneration(),
		Binding: configurationdomain.ValidationBinding{
			Command: configurationdomain.ValidationCommand{
				ConfigurationRevisionID: validation.GetConfigurationRevisionId(),
				ConfigurationType:       validation.GetConfigurationType(),
				CatalogRevision:         validation.GetCatalogRevision(),
				CatalogDigest:           mustDomainDigest(t, validation.GetCatalogDigest()),
				SchemaID:                validation.GetSchemaId(),
				SchemaRevision:          validation.GetSchemaRevision(),
				SchemaDigest:            mustDomainDigest(t, validation.GetSchemaDigest()),
				SettingsEntryID:         validation.GetSettingsEntryId(),
			},
			InputBundleID:         command.GetInputBundleRef().GetInputBundleId(),
			InputBundleDigest:     mustDomainDigest(t, command.GetInputBundleRef().GetDigest()),
			SettingsEntryVersion:  settingsEntry.GetImmutableVersion(),
			SettingsContentDigest: mustDomainDigest(t, settingsEntry.GetContent().GetDigest()),
		},
	}
	if output.GetIdentity().GetExecutionId() != command.GetExecutionId() || output.GetIdentity().GetGeneration() != command.GetGeneration() {
		t.Fatal("corpus output identity does not bind the signed command")
	}
	return &memoryRuntime{
		now: now,
		lease: runtimedomain.ActiveLease{
			ClaimID:      "claim-corpus-1",
			Fence:        fence,
			ExpiresAt:    now.Add(time.Minute),
			DesiredState: runtimedomain.DesiredRunning,
		},
		desired:               runtimedomain.DesiredRunning,
		outboxID:              command.GetIdempotencyKey(),
		expected:              expected,
		settlementByKey:       make(map[string]executionapp.SettlementReceipt),
		settlementDigestByKey: make(map[string]runtimedomain.Digest),
	}
}

func (m *memoryRuntime) ClaimValidation(_ context.Context, request executionapp.ClaimRequest, leaseTTL executionapp.ClaimLeaseTTLMillis) (executionapp.ClaimDecision, error) {
	if request.OutboxID != m.outboxID || request.SignedEnvelopeDigest.IsZero() || request.CommandID != m.lease.Fence.CommandID || request.ExecutionID != m.lease.Fence.ExecutionID || request.Generation != m.lease.Fence.Generation || request.WorkloadIdentity != m.lease.Fence.WorkloadIdentity || request.WorkloadSessionID != m.lease.Fence.WorkloadSessionID || request.ProducerID != m.lease.Fence.ProducerID {
		return executionapp.ClaimDecision{}, executionapp.ErrInvalidClaim
	}
	m.lease.ExpiresAt = m.now.Add(leaseTTL.Duration())
	return executionapp.ClaimDecision{Lease: m.lease, LeaseObservedAt: m.now, Disposition: executionapp.ClaimAccepted}, nil
}

func (m *memoryRuntime) AbortClaim(_ context.Context, fence runtimedomain.Fence, _ executionapp.ClaimAbortDisposition) error {
	if fence != m.lease.Fence {
		return runtimedomain.ErrStaleFence
	}
	return nil
}

func (m *memoryRuntime) CurrentLease(_ context.Context, executionID string, generation uint64) (runtimedomain.ActiveLease, time.Time, error) {
	if executionID != m.lease.Fence.ExecutionID || generation != m.lease.Fence.Generation {
		return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrStaleFence
	}
	if !m.now.Before(m.lease.ExpiresAt) {
		return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrLeaseExpired
	}
	return m.lease, m.now, nil
}

func (m *memoryRuntime) RenewLease(_ context.Context, fence runtimedomain.Fence, leaseTTL executionapp.ClaimLeaseTTLMillis) (runtimedomain.ActiveLease, time.Time, error) {
	if fence != m.lease.Fence {
		return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrStaleFence
	}
	if !m.now.Before(m.lease.ExpiresAt) {
		return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrLeaseExpired
	}
	m.lease.ExpiresAt = m.now.Add(leaseTTL.Duration())
	return m.lease, m.now, nil
}

func (m *memoryRuntime) ReleaseClaim(_ context.Context, fence runtimedomain.Fence) error {
	if fence != m.lease.Fence {
		return runtimedomain.ErrStaleFence
	}
	return nil
}

func (m *memoryRuntime) DesiredState(_ context.Context, executionID string, generation uint64) (runtimedomain.DesiredState, error) {
	if executionID != m.lease.Fence.ExecutionID || generation != m.lease.Fence.Generation {
		return "", runtimedomain.ErrStaleFence
	}
	return m.desired, nil
}

func (m *memoryRuntime) ExpectedValidation(_ context.Context, executionID string, generation uint64) (outputapp.ExpectedValidation, error) {
	if executionID != m.expected.ExecutionID || generation != m.expected.Generation {
		return outputapp.ExpectedValidation{}, runtimedomain.ErrStaleFence
	}
	return m.expected, nil
}

func (m *memoryRuntime) ExpectedRuntimeFailure(_ context.Context, executionID string, generation uint64) (outputapp.ExpectedRuntimeFailure, error) {
	if executionID != m.expected.ExecutionID || generation != m.expected.Generation {
		return outputapp.ExpectedRuntimeFailure{}, runtimedomain.ErrStaleFence
	}
	return outputapp.ExpectedRuntimeFailure{
		TenantID:            m.expected.TenantID,
		ResourceProjectID:   m.expected.ResourceProjectID,
		ProjectionProjectID: m.expected.ProjectionProjectID,
		CapabilityID:        executiondomain.ConfigurationValidationCapability,
		CommandID:           m.expected.CommandID,
		ExecutionID:         m.expected.ExecutionID,
		Generation:          m.expected.Generation,
		LogicalOutputID:     "configuration-validation:" + m.expected.Binding.Command.ConfigurationRevisionID,
	}, nil
}

func (m *memoryRuntime) ProjectConfigurationValidation(_ context.Context, projection outputapp.ValidationProjection) (outputapp.ProjectionOutcome, error) {
	m.validationProjections++
	m.eventCursor++
	m.events = append(m.events, executionapi.DurableEvent{Cursor: m.eventCursor, Type: "configuration.validation.completed", Data: append([]byte(nil), projection.BrowserData...)})
	m.terminal = &projectedTerminal{
		settlement:        projection.Frame.Settlement,
		encodedSettlement: append([]byte(nil), projection.Frame.EncodedSettlement...),
	}
	return outputapp.ProjectionOutcome{Inserted: true, Cursor: m.eventCursor, CommittedSequence: projection.Frame.Sequence}, nil
}

func (m *memoryRuntime) ProjectRuntimeFailure(_ context.Context, projection outputapp.RuntimeFailureProjection) (outputapp.ProjectionOutcome, error) {
	m.failureProjections++
	m.eventCursor++
	m.events = append(m.events, executionapi.DurableEvent{Cursor: m.eventCursor, Type: "execution.failed", Data: append([]byte(nil), projection.BrowserData...)})
	m.terminal = &projectedTerminal{
		settlement:        projection.Frame.Settlement,
		encodedSettlement: append([]byte(nil), projection.Frame.EncodedSettlement...),
	}
	return outputapp.ProjectionOutcome{Inserted: true, Cursor: m.eventCursor, CommittedSequence: projection.Frame.Sequence}, nil
}

func (m *memoryRuntime) Replay(_ context.Context, projectID, executionID string, afterCursor uint64, limit int) ([]executionapi.DurableEvent, error) {
	if projectID != m.expected.ProjectionProjectID || executionID != m.expected.ExecutionID || limit <= 0 {
		return nil, executionapi.ErrExecutionEventsForbidden
	}
	events := make([]executionapi.DurableEvent, 0, len(m.events))
	for _, event := range m.events {
		if event.Cursor > afterCursor {
			event.Data = append([]byte(nil), event.Data...)
			events = append(events, event)
			if len(events) == limit {
				break
			}
		}
	}
	return events, nil
}

func (m *memoryRuntime) PrepareSettlement(_ context.Context, proposal executionapp.SettlementProposal) (executionapp.SettlementReceipt, error) {
	if m.terminal == nil || proposal != m.terminal.settlement || proposal.Fence != m.lease.Fence || runtimedomain.SHA256(m.terminal.encodedSettlement) != proposal.ProposalDigest {
		return executionapp.SettlementReceipt{}, executionapp.ErrTerminalOutputNotReady
	}
	key := proposal.Fence.ExecutionID + ":" + proposal.IdempotencyKey
	if digest, exists := m.settlementDigestByKey[key]; exists {
		if digest != proposal.ProposalDigest {
			return executionapp.SettlementReceipt{}, executionapp.ErrSettlementConflict
		}
		return m.settlementByKey[key], nil
	}
	receipt := executionapp.SettlementReceipt{ID: "receipt-corpus-1", Outcome: proposal.Outcome}
	m.settlementDigestByKey[key] = proposal.ProposalDigest
	m.settlementByKey[key] = receipt
	m.settlementCount++
	return receipt, nil
}

type corpusOutputStream struct {
	context context.Context
	frames  []*runtimev1.ExecutionOutputFrameV1
	acks    []*runtimev1.ExecutionOutputAckV1
	index   int
}

func (s *corpusOutputStream) Recv() (*runtimev1.ExecutionOutputFrameV1, error) {
	if s.index >= len(s.frames) {
		return nil, io.EOF
	}
	frame := s.frames[s.index]
	s.index++
	return frame, nil
}

func (s *corpusOutputStream) Send(ack *runtimev1.ExecutionOutputAckV1) error {
	s.acks = append(s.acks, ack)
	return nil
}

func (s *corpusOutputStream) SetHeader(metadata.MD) error  { return nil }
func (s *corpusOutputStream) SendHeader(metadata.MD) error { return nil }
func (s *corpusOutputStream) SetTrailer(metadata.MD)       {}
func (s *corpusOutputStream) Context() context.Context     { return s.context }
func (s *corpusOutputStream) SendMsg(any) error            { return nil }
func (s *corpusOutputStream) RecvMsg(any) error            { return nil }

type sseAuthorizer struct {
	projectID   string
	executionID string
}

func (a sseAuthorizer) AuthorizeExecutionEvents(_ context.Context, projectID, executionID string) error {
	if projectID != a.projectID || executionID != a.executionID {
		return executionapi.ErrExecutionEventsForbidden
	}
	return nil
}

type stoppingReplayWaiter struct{}

func (stoppingReplayWaiter) Wait(context.Context, string, string, uint64) (bool, error) {
	return false, context.Canceled
}

type deadlineAwareRecorder struct {
	*httptest.ResponseRecorder
}

func newDeadlineAwareRecorder() *deadlineAwareRecorder {
	return &deadlineAwareRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (*deadlineAwareRecorder) SetWriteDeadline(time.Time) error { return nil }

func (r *deadlineAwareRecorder) Flush() { r.ResponseRecorder.Flush() }

func assertDurableSSEReplay(t *testing.T, state *memoryRuntime, projectID, executionID string, settings []byte) {
	t.Helper()
	handler, err := executionapi.NewEventHandler(
		sseAuthorizer{projectID: projectID, executionID: executionID},
		state,
		stoppingReplayWaiter{},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", projectID)
	routeContext.URLParams.Add("executionID", executionID)
	requestContext := context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)
	request = request.WithContext(auth.ContextWithAuthenticatedUser(
		requestContext,
		auth.User{ID: "7", UserID: "7", AuthType: "user"},
		auth.AuthenticationSourceSession,
	))
	request.Header.Set("Last-Event-ID", "0")
	response := newDeadlineAwareRecorder()
	handler.Stream(response, request)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("id: 1\n")) || !bytes.Contains(response.Body.Bytes(), []byte("data: {")) {
		t.Fatalf("durable projection was not replayed through SSE: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, forbidden := range [][]byte{bytes.TrimSpace(settings), []byte("settings_content_digest"), []byte("client_secret"), []byte("fence_token")} {
		if bytes.Contains(response.Body.Bytes(), forbidden) {
			t.Fatalf("SSE projection exposed protected execution material %q: %s", forbidden, response.Body.String())
		}
	}

	resume := httptest.NewRequest(http.MethodGet, "/", nil)
	resumeContext := context.WithValue(resume.Context(), chi.RouteCtxKey, routeContext)
	resume = resume.WithContext(auth.ContextWithAuthenticatedUser(
		resumeContext,
		auth.User{ID: "7", UserID: "7", AuthType: "user"},
		auth.AuthenticationSourceSession,
	))
	resume.Header.Set("Last-Event-ID", "1")
	resumed := newDeadlineAwareRecorder()
	handler.Stream(resumed, resume)
	if resumed.Code != http.StatusOK || bytes.Contains(resumed.Body.Bytes(), []byte("id: 1\n")) {
		t.Fatalf("Last-Event-ID replay duplicated a durable event: status=%d body=%s", resumed.Code, resumed.Body.String())
	}
}

func newCorpusVerifier(t *testing.T) *controltransport.ConformanceCommandVerifier {
	t.Helper()
	verifier, err := controltransport.NewConformanceCommandVerifier(controltransport.ConformanceVerifierConfig{
		EnvelopeSchemaRevision: "elitea.runtime.signed-worker-command.v1",
		ProtocolRevision:       "elitea.runtime.v1",
		CapabilityVersion:      "1",
		LimitsRevision:         "elitea.runtime.limits.conformance.v2",
		KeyID:                  "elitea-runtime-v1-conformance-hmac",
		HMACKey:                []byte("ELITEA_RUNTIME_V1_TEST_ONLY_NOT_A_SECRET"),
		MaxWorkerCommandBytes:  32 * 1024,
		MaxInputManifestBytes:  64 * 1024,
		MaxStringBytes:         256,
	})
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func readCorpus(t *testing.T, fixture string) (*runtimev1.WorkerExecutionEnvelopeV1, *runtimev1.ExecutionInputBundleV1, *runtimev1.ExecutionOutputFrameV1) {
	t.Helper()
	envelope := &runtimev1.WorkerExecutionEnvelopeV1{}
	manifest := &runtimev1.ExecutionInputBundleV1{}
	output := &runtimev1.ExecutionOutputFrameV1{}
	if err := proto.Unmarshal(mustReadCorpusFile(t, fixture, "envelope.pb"), envelope); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(mustReadCorpusFile(t, fixture, "input-bundle.pb"), manifest); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(mustReadCorpusFile(t, fixture, "expected-output.pb"), output); err != nil {
		t.Fatal(err)
	}
	return envelope, manifest, output
}

func mustReadCorpusFile(t *testing.T, fixture, name string) []byte {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate integration test source")
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "../../../../testdata/proto/runtime/v1/configuration-validation", fixture, name))
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func decodeCommand(t *testing.T, raw []byte) *runtimev1.WorkerCommandV1 {
	t.Helper()
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(raw, command); err != nil {
		t.Fatal(err)
	}
	return command
}

func findSettingsEntry(t *testing.T, manifest *runtimev1.ExecutionInputBundleV1, entryID string) *runtimev1.ExecutionInputEntryV1 {
	t.Helper()
	for _, entry := range manifest.GetEntries() {
		if entry.GetEntryId() == entryID {
			return entry
		}
	}
	t.Fatalf("settings entry %q is absent", entryID)
	return nil
}

func mustDomainDigest(t *testing.T, digest *runtimev1.DigestV1) runtimedomain.Digest {
	t.Helper()
	if digest.GetAlgorithm() != runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 || len(digest.GetValue()) != 32 {
		t.Fatal("invalid corpus SHA-256 digest")
	}
	var mapped runtimedomain.Digest
	copy(mapped[:], digest.GetValue())
	return mapped
}

func digestProto(digest runtimedomain.Digest) *runtimev1.DigestV1 {
	return &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: append([]byte(nil), digest[:]...)}
}

func digestMatches(digest *runtimev1.DigestV1, content []byte) bool {
	actual := runtimedomain.SHA256(content)
	return digest.GetAlgorithm() == runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 && bytes.Equal(digest.GetValue(), actual[:])
}

func mustMarshal(t *testing.T, message proto.Message) []byte {
	t.Helper()
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func mustMarshalWithoutTest(message proto.Message) []byte {
	encoded, _ := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	return encoded
}
