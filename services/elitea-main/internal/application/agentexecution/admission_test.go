package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

type recordingAdmissionStore struct {
	admission Admission
	outcome   executionapp.AdmissionOutcome
	err       error
}

func (s *recordingAdmissionStore) AdmitAgentExecution(
	_ context.Context,
	admission Admission,
) (executionapp.AdmissionOutcome, error) {
	s.admission = admission
	return s.outcome, s.err
}

func TestAdmissionServiceCreatesAtomicReferenceOnlyAgentAdmission(t *testing.T) {
	admittedAt := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	store := &recordingAdmissionStore{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution", CommandID: "command", Created: true,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(15 * time.Minute),
	}}
	service := testAdmissionService(t, store)

	outcome, err := service.Submit(context.Background(), SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: "tenant", ResourceProjectID: "7",
			ProjectionProjectID: "7", ActorID: "19",
		},
		IdempotencyKey:  "message-1",
		CapabilityID:    executiondomain.AgentApplicationCapability,
		ClientStreamID:  "conversation-1",
		ClientMessageID: "message-1",
		SIOEvent:        "chat_predict",
		Input:           validAgentInput(),
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if outcome != store.outcome {
		t.Fatalf("Submit() outcome = %#v, want %#v", outcome, store.outcome)
	}
	if err := store.admission.Record.Validate(); err != nil {
		t.Fatalf("record.Validate() error = %v", err)
	}
	if err := store.admission.Binding.Validate(store.admission.Record.InputBundle); err != nil {
		t.Fatalf("binding.Validate() error = %v", err)
	}
	if store.admission.Record.Job.CapabilityID != executiondomain.AgentApplicationCapability {
		t.Fatalf("capability = %q", store.admission.Record.Job.CapabilityID)
	}
	if len(store.admission.Record.InputBundle.Entries) != 1 {
		t.Fatalf("input entries = %d, want 1", len(store.admission.Record.InputBundle.Entries))
	}
	if store.admission.Record.Outbox.ID != "outbox" {
		t.Fatalf("outbox ID = %q, want outbox", store.admission.Record.Outbox.ID)
	}
}

func TestAdmissionDigestIsStableAcrossGeneratedStorageIdentities(t *testing.T) {
	firstStore := successfulRecordingStore()
	secondStore := successfulRecordingStore()
	first := testAdmissionServiceWithIDs(
		t, firstStore, []string{"bundle-a", "content-a"},
		[]string{"execution-a", "command-a", "outbox-a"},
	)
	second := testAdmissionServiceWithIDs(
		t, secondStore, []string{"bundle-b", "content-b"},
		[]string{"execution-b", "command-b", "outbox-b"},
	)
	request := validSubmitRequest()
	if _, err := first.Submit(context.Background(), request); err != nil {
		t.Fatalf("first Submit() error = %v", err)
	}
	if _, err := second.Submit(context.Background(), request); err != nil {
		t.Fatalf("second Submit() error = %v", err)
	}
	if firstStore.admission.Record.RequestDigest != secondStore.admission.Record.RequestDigest {
		t.Fatal("same authoritative request produced different idempotency digest")
	}
}

func TestAdmissionServiceRejectsUnsupportedCapabilityBeforeStorage(t *testing.T) {
	store := successfulRecordingStore()
	service := testAdmissionService(t, store)
	request := validSubmitRequest()
	request.CapabilityID = "index.search.v1"

	_, err := service.Submit(context.Background(), request)
	if !errors.Is(err, ErrInvalidAgentAdmission) {
		t.Fatalf("Submit() error = %v, want %v", err, ErrInvalidAgentAdmission)
	}
	if store.admission.Record.Job.ID != "" {
		t.Fatal("invalid capability reached durable storage")
	}
}

func TestAdmissionServiceRejectsCurrentTurnGenerationDriftBeforeStorage(t *testing.T) {
	store := successfulRecordingStore()
	service := testAdmissionService(t, store)
	request := validSubmitRequest()
	request.CapabilityID = executiondomain.AgentApplicationCapability
	request.ClientStreamID = "8bc66e50-46c4-4e2c-94ec-daec6c596ac0"
	request.ClientMessageID = "061e2c58-2e09-5853-a006-532b082a0433"
	request.CurrentTurn = &CurrentApplicationTurn{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:     request.ClientStreamID,
		TargetParticipantID:  21,
		ApplicationID:        31,
		ApplicationVersionID: 41,
		QuestionID:           "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
		QuestionItemID:       "3d3f33b7-e629-4db7-b4ee-c2914067897a",
		ResponseMessageID:    request.ClientMessageID,
		QuestionMeta:         []byte(`{}`),
		UserInput:            "hello",
	}

	_, err := service.Submit(context.Background(), request)
	if !errors.Is(err, ErrInvalidAgentAdmission) {
		t.Fatalf("Submit() error = %v, want %v", err, ErrInvalidAgentAdmission)
	}
	if store.admission.Record.Job.ID != "" {
		t.Fatal("current-turn generation drift reached durable storage")
	}
}

func TestAdmissionServiceAcceptsBoundCurrentAdhocTurn(t *testing.T) {
	store := successfulRecordingStore()
	service := testAdmissionService(t, store)
	questionID := "ee92ccbd-3312-4c72-b20b-fddf224e7c0e"
	conversationID := "8bc66e50-46c4-4e2c-94ec-daec6c596ac0"
	responseID := "061e2c58-2e09-5853-a006-532b082a0433"
	input := validAgentInput()
	input.ExecutionGeneration = proto.String(questionID)
	request := validSubmitRequest()
	request.ClientStreamID = conversationID
	request.ClientMessageID = responseID
	request.Input = input
	request.CurrentAdhocTurn = &CurrentAdhocTurn{
		ProjectID: 7, ActorUserID: 11, ConversationUUID: conversationID,
		TargetParticipantID: 21, QuestionID: questionID,
		QuestionItemID:    "3d3f33b7-e629-4db7-b4ee-c2914067897a",
		ResponseMessageID: responseID, QuestionMeta: []byte(`{}`), UserInput: "hello",
	}

	if _, err := service.Submit(context.Background(), request); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if store.admission.CurrentTurn != nil || store.admission.CurrentAdhocTurn == nil ||
		store.admission.Record.Job.CapabilityID != executiondomain.AgentAdhocCapability {
		t.Fatalf("admission=%+v", store.admission)
	}
}

func TestAdmissionServiceAcceptsOnlyOneBoundCurrentContinuation(t *testing.T) {
	store := successfulRecordingStore()
	service := testAdmissionService(t, store)
	conversationID := "8bc66e50-46c4-4e2c-94ec-daec6c596ac0"
	questionID := "ee92ccbd-3312-4c72-b20b-fddf224e7c0e"
	responseID := "061e2c58-2e09-5853-a006-532b082a0433"
	input := validAgentInput()
	input.ThreadId = proto.String("thread-hitl-1")
	input.ExecutionGeneration = proto.String(questionID)
	input.ShouldContinue = true
	input.HitlResume = true
	input.HitlAction = proto.String("approve")
	input.HitlDecisions = []byte(`[{"interrupt_id":"interrupt-root-1","action":"approve","value":""}]`)
	request := validSubmitRequest()
	request.CapabilityID = executiondomain.AgentApplicationCapability
	request.ClientStreamID = conversationID
	request.ClientMessageID = responseID
	request.SIOEvent = "chat_continue_predict"
	request.Input = input
	request.CurrentContinueTurn = &CurrentContinueTurn{
		ProjectID: 7, ActorUserID: 11, ConversationUUID: conversationID,
		TargetParticipantID: 21, Kind: CurrentRegenerationApplication,
		ApplicationID: 31, ApplicationVersionID: 41, QuestionID: questionID,
		ResponseMessageID: responseID, ExecutionGeneration: questionID,
		ThreadID: "thread-hitl-1", InterruptID: "interrupt-root-1", Action: "approve",
		HITLDecisions: json.RawMessage(`[{"interrupt_id":"interrupt-root-1","action":"approve"}]`),
	}

	if _, err := service.Submit(context.Background(), request); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if store.admission.CurrentContinueTurn == nil || store.admission.CurrentTurn != nil ||
		store.admission.CurrentAdhocTurn != nil || store.admission.CurrentRegenerateTurn != nil {
		t.Fatalf("admission=%+v", store.admission)
	}

	request.CurrentContinueTurn.PipelineHITLReview = &CurrentPipelineHITLReview{
		InterruptID: "interrupt-root-1", NodeName: "review", Message: "Review the joke.",
	}
	service = testAdmissionService(t, store)
	if _, err := service.Submit(context.Background(), request); !errors.Is(err, ErrInvalidAgentAdmission) {
		t.Fatalf("direct review bound to paused response: %v", err)
	}
	request.ClientMessageID = request.CurrentContinueTurn.ProjectionResponseID()
	service = testAdmissionService(t, store)
	if _, err := service.Submit(context.Background(), request); err != nil {
		t.Fatalf("direct review bound to new response: %v", err)
	}
	if store.admission.Binding.ClientMessageID != request.ClientMessageID ||
		store.admission.CurrentContinueTurn.ResponseMessageID != responseID {
		t.Fatal("direct review lost its projection or paused-response identity")
	}

	request.CurrentRegenerateTurn = &CurrentRegenerateTurn{}
	invalidService := testAdmissionService(t, successfulRecordingStore())
	if _, err := invalidService.Submit(context.Background(), request); !errors.Is(err, ErrInvalidAgentAdmission) {
		t.Fatalf("overlapping continuation Submit() error = %v", err)
	}
}

func TestAdmissionServicePreservesCancellation(t *testing.T) {
	store := successfulRecordingStore()
	service := testAdmissionService(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := service.Submit(ctx, validSubmitRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Submit() error = %v, want %v", err, context.Canceled)
	}
}

func successfulRecordingStore() *recordingAdmissionStore {
	admittedAt := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	return &recordingAdmissionStore{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution", CommandID: "command", Created: true,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
}

func validSubmitRequest() SubmitRequest {
	return SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: "tenant", ResourceProjectID: "7",
			ProjectionProjectID: "7", ActorID: "19",
		},
		IdempotencyKey:  "message-1",
		CapabilityID:    executiondomain.AgentAdhocCapability,
		ClientStreamID:  "conversation-1",
		ClientMessageID: "message-1",
		SIOEvent:        "chat_predict",
		Input:           validAgentInput(),
	}
}

func testAdmissionService(
	t *testing.T,
	store *recordingAdmissionStore,
) *AdmissionService {
	t.Helper()
	return testAdmissionServiceWithIDs(
		t, store, []string{"bundle", "content"},
		[]string{"execution", "command", "outbox"},
	)
}

func testAdmissionServiceWithIDs(
	t *testing.T,
	store *recordingAdmissionStore,
	inputIDs,
	admissionIDs []string,
) *AdmissionService {
	t.Helper()
	inputIndex := 0
	factory, err := NewInputBundleFactory(InputProfile{
		Classification:        "tenant-confidential",
		RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, func() (string, error) {
		value := inputIDs[inputIndex]
		inputIndex++
		return value, nil
	})
	if err != nil {
		t.Fatalf("NewInputBundleFactory() error = %v", err)
	}
	admissionIndex := 0
	service, err := NewAdmissionService(
		store, factory,
		func() time.Time { return time.Date(2026, 8, 2, 11, 0, 0, 0, time.UTC) },
		func() (string, error) {
			value := admissionIDs[admissionIndex]
			admissionIndex++
			return value, nil
		},
	)
	if err != nil {
		t.Fatalf("NewAdmissionService() error = %v", err)
	}
	return service
}
