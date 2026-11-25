package agentexecution

import (
	"context"
	"encoding/json"
	"testing"

	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

// A message that names no agent, sent as a model chat (the body an older web
// composer sent for a fresh team conversation), in a conversation whose floor
// has a lead: the lead answers, as turntaking.Route says. It used to be
// refused with ErrAgentTargetRequired, because the ad-hoc start never read the
// floor, while "To: <lead> (lead)" sat above the composer.
func leadAdhocRequest() CurrentAdhocStartRequest {
	return CurrentAdhocStartRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		QuestionID:       "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
		UserInput:        "hello team!",
		InteractionUUID:  "31df012a-300d-4722-9be2-521d987c63a8",
		LLMSettings:      json.RawMessage(`{"model_name":"m"}`),
		MentionedUserIDs: []int64{5},
		TriggerOrigin:    executiondomain.TriggerOriginManual,
	}
}

func leadFloor() *floorStoreStub {
	store := twoAgentFloor()
	store.floor.Settings = turntaking.Settings{Policy: turntaking.PolicyLead, Lead: 22}
	return store
}

func TestAdhocStartWithoutTargetGoesToTheLead(t *testing.T) {
	service, admissions := floorStartService(t, leadFloor())
	if _, err := service.StartCurrentAdhoc(context.Background(), leadAdhocRequest()); err != nil {
		t.Fatalf("an unaddressed message under a lead: %v", err)
	}
	if len(admissions.requests) != 1 {
		t.Fatalf("admissions = %d", len(admissions.requests))
	}
	submitted := admissions.requests[0]
	if submitted.CurrentTurn == nil || submitted.CurrentTurn.TargetParticipantID != 22 || submitted.CurrentTurn.ApplicationID != 31 {
		t.Fatalf("turn = %+v, want the lead 22 as an agent turn", submitted.CurrentTurn)
	}
	if submitted.Floor == nil || submitted.Floor.Exchange == nil || submitted.Floor.Exchange.FirstParticipantID != 22 {
		t.Fatalf("floor = %+v", submitted.Floor)
	}
}

// An explicit target (the conversation's model, picked on purpose) is not
// rerouted: only a message that names nobody goes to the lead.
func TestAdhocStartWithATargetIsNotRoutedToTheLead(t *testing.T) {
	store := leadFloor()
	service, _ := floorStartService(t, store)
	request := leadAdhocRequest()
	request.TargetParticipantID = 23
	_, _ = service.StartCurrentAdhoc(context.Background(), request)
	if store.calls != 0 {
		t.Fatalf("an explicit model target read the floor %d times", store.calls)
	}
}

// Only a person's message is routed: an automated ad-hoc start keeps its own
// target rules.
func TestAdhocStartAutomatedOriginIsNotRoutedToTheLead(t *testing.T) {
	store := leadFloor()
	service, _ := floorStartService(t, store)
	request := leadAdhocRequest()
	request.TriggerOrigin = executiondomain.TriggerOriginSchedule
	_, _ = service.StartCurrentAdhoc(context.Background(), request)
	if store.calls != 0 {
		t.Fatalf("an automated start read the floor %d times", store.calls)
	}
}

// Under `addressed` the floor names nobody: the start stays a model-chat
// start, and the repository refuses it when several agents sit in the
// conversation (agent_target_required).
func TestAdhocStartWithoutTargetAndNoLeadStaysAdhoc(t *testing.T) {
	service, admissions := floorStartService(t, twoAgentFloor())
	_, _ = service.StartCurrentAdhoc(context.Background(), leadAdhocRequest())
	for _, submitted := range admissions.requests {
		if submitted.Floor != nil {
			t.Fatalf("an addressed-floor model chat was routed: %+v", submitted.Floor)
		}
	}
}
