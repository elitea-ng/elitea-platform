package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

// A message that names no agent, sent as a model chat (the body an older web
// composer sent for a fresh team conversation), in a conversation whose floor
// has a lead: the lead answers, as turntaking.Route says. It used to be
// refused with ErrAgentTargetRequired, because the ad-hoc start never read the
// floor, while "To: <lead> (lead)" sat above the composer (issue 3049).
func leadAdhocRequest() CurrentAdhocStartRequest {
	return CurrentAdhocStartRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		QuestionID:       "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
		UserInput:        "hello team!",
		InteractionUUID:  "31df012a-300d-4722-9be2-521d987c63a8",
		LLMSettings:      json.RawMessage(`{"model_name":"picked-for-the-plain-model"}`),
		MentionedUserIDs: []int64{5},
		TriggerOrigin:    executiondomain.TriggerOriginManual,
	}
}

func leadFloor() *floorStoreStub {
	store := twoAgentFloor()
	store.floor.Settings = turntaking.Settings{Policy: turntaking.PolicyLead, Lead: 22}
	return store
}

// adhocRefusal is the repository's answer to a no-target model chat among
// several agents (agent_start.go ResolveCurrentAdhoc), or err when set.
type adhocRefusal struct {
	*currentApplicationResolverStub
	err   error
	calls int
}

func (stub *adhocRefusal) ResolveCurrentAdhoc(context.Context, CurrentAdhocStartRequest) (CurrentAdhocTarget, error) {
	stub.calls++
	if stub.err != nil {
		return CurrentAdhocTarget{}, stub.err
	}
	return CurrentAdhocTarget{}, ErrAgentTargetRequired
}

func leadStartService(t *testing.T, floor FloorStore, adhoc *adhocRefusal) (*CurrentApplicationStartService, *currentApplicationAdmissionStub) {
	t.Helper()
	resolver := &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41,
		Variables:      json.RawMessage(`[]`),
		VersionDetails: json.RawMessage(`{"id":41,"application_id":31,"agent_type":"agent","instructions":"x","llm_settings":{"model_name":"lead-model"},"meta":{},"tools":[]}`),
		ChatHistory:    json.RawMessage(`[]`),
	}}
	adhoc.currentApplicationResolverStub = resolver
	admittedAt := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-1", CommandID: "command-1", Created: true,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
	service, err := NewCurrentApplicationStartService(resolver, adhoc, resolver, resolver,
		&currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	return service.WithFloor(floor), admissions
}

func TestAdhocStartWithoutTargetGoesToTheLead(t *testing.T) {
	service, admissions := leadStartService(t, leadFloor(), &adhocRefusal{})
	if _, err := service.StartCurrentAdhoc(context.Background(), leadAdhocRequest()); err != nil {
		t.Fatalf("an unaddressed message under a lead: %v", err)
	}
	if len(admissions.requests) != 1 {
		t.Fatalf("admissions = %d", len(admissions.requests))
	}
	submitted := admissions.requests[0]
	// The lead's own agent version answers; the model chat's llm_settings
	// (the plain model's selector) do not reach it.
	if submitted.CurrentTurn == nil || submitted.CurrentTurn.TargetParticipantID != 22 ||
		submitted.CurrentTurn.ApplicationID != 31 || submitted.CurrentTurn.ApplicationVersionID != 41 {
		t.Fatalf("turn = %+v, want the lead 22 as an agent turn", submitted.CurrentTurn)
	}
	if submitted.Floor == nil || submitted.Floor.Exchange == nil || submitted.Floor.Exchange.FirstParticipantID != 22 {
		t.Fatalf("floor = %+v", submitted.Floor)
	}
}

// A plain model chat (the repository admits it) never reads the floor, so it
// cannot fail on a floor read.
func TestAdhocStartAdmittedByTheRepositoryDoesNotReadTheFloor(t *testing.T) {
	store := leadFloor()
	store.err = errors.New("floor read failed")
	adhoc := &adhocRefusal{err: ErrUnsupportedCurrentAgentStart}
	service, _ := leadStartService(t, store, adhoc)
	_, _ = service.StartCurrentAdhoc(context.Background(), leadAdhocRequest())
	if store.calls != 0 || adhoc.calls != 1 {
		t.Fatalf("floor reads = %d, adhoc resolves = %d", store.calls, adhoc.calls)
	}
}

// A floor read that fails after the refusal names the read.
func TestAdhocStartFloorReadErrorIsReturned(t *testing.T) {
	store := leadFloor()
	store.err = errors.New("floor read failed")
	service, admissions := leadStartService(t, store, &adhocRefusal{})
	_, err := service.StartCurrentAdhoc(context.Background(), leadAdhocRequest())
	if !errors.Is(err, store.err) || len(admissions.requests) != 0 {
		t.Fatalf("err=%v admissions=%d", err, len(admissions.requests))
	}
}

// Under `addressed` the floor names nobody: the refusal stands.
func TestAdhocStartWithoutTargetAndNoLeadIsRefused(t *testing.T) {
	service, admissions := leadStartService(t, twoAgentFloor(), &adhocRefusal{})
	_, err := service.StartCurrentAdhoc(context.Background(), leadAdhocRequest())
	if !errors.Is(err, ErrAgentTargetRequired) || len(admissions.requests) != 0 {
		t.Fatalf("err=%v admissions=%d", err, len(admissions.requests))
	}
}

// A lead that left the conversation does not strand the message on it.
func TestAdhocStartWithADepartedLeadIsRefused(t *testing.T) {
	store := twoAgentFloor()
	store.floor.Settings = turntaking.Settings{Policy: turntaking.PolicyLead, Lead: 99}
	service, admissions := leadStartService(t, store, &adhocRefusal{})
	_, err := service.StartCurrentAdhoc(context.Background(), leadAdhocRequest())
	if !errors.Is(err, ErrAgentTargetRequired) || len(admissions.requests) != 0 {
		t.Fatalf("err=%v admissions=%d", err, len(admissions.requests))
	}
}

// Only a person's message is routed: an automated ad-hoc start keeps its own
// target rules.
func TestAdhocStartAutomatedOriginIsNotRoutedToTheLead(t *testing.T) {
	store := leadFloor()
	service, admissions := leadStartService(t, store, &adhocRefusal{})
	request := leadAdhocRequest()
	request.TriggerOrigin = executiondomain.TriggerOriginSchedule
	_, err := service.StartCurrentAdhoc(context.Background(), request)
	if store.calls != 0 || !errors.Is(err, ErrAgentTargetRequired) || len(admissions.requests) != 0 {
		t.Fatalf("floor reads=%d err=%v admissions=%d", store.calls, err, len(admissions.requests))
	}
}
