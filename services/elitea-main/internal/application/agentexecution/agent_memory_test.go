package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
)

type agentMemoryRecallStub struct {
	recall   AgentMemoryRecall
	err      error
	requests []AgentMemoryRecallRequest
	// render, when set, is what the store recalls: rendered with the
	// request's tag, as the repository does.
	render []agentmemory.Candidate
	names  map[int64]string
}

func (stub *agentMemoryRecallStub) ResolveAgentMemoryRecall(_ context.Context, request AgentMemoryRecallRequest) (AgentMemoryRecall, error) {
	stub.requests = append(stub.requests, request)
	if stub.render != nil {
		return AgentMemoryRecall{
			Text:    agentmemory.Render(stub.render, stub.names, request.MarkTag),
			Entries: agentmemory.RecalledEntries(stub.render),
		}, stub.err
	}
	return stub.recall, stub.err
}

// frozenInstructions is the frozen version_details.instructions of input.
func frozenInstructions(t *testing.T, input *runtimev1.AgentExecutionInputV1) string {
	t.Helper()
	var application struct {
		VersionDetails struct {
			Instructions string `json:"instructions"`
		} `json:"version_details"`
	}
	if err := json.Unmarshal(input.GetApplication(), &application); err != nil {
		t.Fatal(err)
	}
	return application.VersionDetails.Instructions
}

func agentMemoryStartService(t *testing.T, versionDetails string, memory AgentMemoryRecallResolver) (*CurrentApplicationStartService, *currentApplicationAdmissionStub) {
	t.Helper()
	resolver := &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41,
		Variables:      json.RawMessage(`[]`),
		VersionDetails: json.RawMessage(versionDetails),
		ChatHistory:    json.RawMessage(`[]`),
	}}
	admittedAt := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-1", CommandID: "command-1", Created: true,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
	service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver,
		&currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	return service.WithAgentMemory(memory), admissions
}

const memoryAgentVersion = `{"id":41,"application_id":31,"agent_type":"agent","instructions":"x","llm_settings":{"model_name":"test"},"meta":{"memory":{"enabled":true}},"tools":[]}`

// #1826: the entries auto-recall shows in the instructions travel in the run
// input by id with their stored content, so a spawn_agent brief can name one.
func TestAgentMemoryRecallStampsTheEntriesIntoTheInput(t *testing.T) {
	entries := []agentmemory.Candidate{
		{ID: 7, Kind: agentmemory.KindFact, Content: "the team ships on Fridays"},
		{ID: 42, Kind: agentmemory.KindPreference, Content: "Dana wants bullets", SubjectUserID: 11},
	}
	memory := &agentMemoryRecallStub{render: entries, names: map[int64]string{11: "Dana"}}
	service, admissions := agentMemoryStartService(t, memoryAgentVersion, memory)
	if _, err := service.StartCurrentApplication(context.Background(), validCurrentApplicationStartRequest()); err != nil {
		t.Fatal(err)
	}
	input := admissions.requests[0].Input
	recalled := input.GetRecalledMemory()
	if len(recalled) != 2 || recalled[0].GetEntryId() != "7" || recalled[0].GetContent() != "the team ships on Fridays" ||
		recalled[1].GetEntryId() != "42" || recalled[1].GetContent() != "Dana wants bullets" {
		t.Fatalf("recalled memory = %+v", recalled)
	}
	if len(memory.requests) != 1 || memory.requests[0].ConversationUUID != validCurrentApplicationStartRequest().ConversationUUID {
		t.Fatalf("recall requests = %+v", memory.requests)
	}
	// #1900: the marks carry the turn's tag, which the input freezes beside
	// the entries.
	tag := memory.requests[0].MarkTag
	if !agentmemory.ValidMarkTag(tag) || input.RecalledMemoryTag == nil || input.GetRecalledMemoryTag() != tag {
		t.Fatalf("tag %q, input tag %v", tag, input.RecalledMemoryTag)
	}
	instructions := frozenInstructions(t, input)
	for _, want := range []string{
		"- [memory 7·" + tag + ", fact] the team ships on Fridays\n",
		"- [memory 42·" + tag + ", preference, about Dana] Dana wants bullets\n",
	} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("instructions miss %q:\n%s", want, instructions)
		}
	}
	if strings.Contains(instructions, "[memory 7,") {
		t.Fatalf("an untagged mark:\n%s", instructions)
	}
}

// #1900: a conversation keeps one tag across its turns, so the notes block
// keeps its bytes and a provider prompt cache matches.
func TestAgentMemoryRecallKeepsOneTagPerConversation(t *testing.T) {
	memory := &agentMemoryRecallStub{render: []agentmemory.Candidate{{ID: 7, Kind: agentmemory.KindFact, Content: "x"}}}
	service, admissions := agentMemoryStartService(t, memoryAgentVersion, memory)
	for range 2 {
		if _, err := service.StartCurrentApplication(context.Background(), validCurrentApplicationStartRequest()); err != nil {
			t.Fatal(err)
		}
	}
	first, second := admissions.requests[0].Input.GetRecalledMemoryTag(), admissions.requests[1].Input.GetRecalledMemoryTag()
	if first == "" || first != second || memory.requests[0].MarkTag != first || memory.requests[1].MarkTag != second {
		t.Fatalf("tags %q, %q; requested %q, %q", first, second, memory.requests[0].MarkTag, memory.requests[1].MarkTag)
	}
}

// #1900: a fan-out wake continues the frozen input, so its instructions
// and their marks' tag are the same as the parked run's.
func TestFanoutWakeKeepsTheRecalledMemoryTag(t *testing.T) {
	memory := &agentMemoryRecallStub{render: []agentmemory.Candidate{{ID: 7, Kind: agentmemory.KindFact, Content: "x"}}}
	service, admissions := agentMemoryStartService(t, memoryAgentVersion, memory)
	if _, err := service.StartCurrentApplication(context.Background(), validCurrentApplicationStartRequest()); err != nil {
		t.Fatal(err)
	}
	frozen := admissions.requests[0].Input
	wake, err := FanoutWakeInput(wakeTestTarget(t, func(input *runtimev1.AgentExecutionInputV1) {
		input.Application = frozen.GetApplication()
		input.RecalledMemory = frozen.GetRecalledMemory()
		input.RecalledMemoryTag = frozen.RecalledMemoryTag
	}))
	if err != nil {
		t.Fatal(err)
	}
	if wake.GetRecalledMemoryTag() != frozen.GetRecalledMemoryTag() || len(wake.GetRecalledMemory()) != 1 ||
		frozenInstructions(t, wake) != frozenInstructions(t, frozen) {
		t.Fatalf("wake tag %q, frozen %q", wake.GetRecalledMemoryTag(), frozen.GetRecalledMemoryTag())
	}
}

// A failed recall, memory off or a pipeline carries no entries.
func TestAgentMemoryRecallCarriesNoEntriesWithoutRecall(t *testing.T) {
	entries := agentmemory.RecalledEntries([]agentmemory.Candidate{{ID: 7, Kind: agentmemory.KindFact, Content: "x"}})
	for name, tc := range map[string]struct {
		version string
		memory  *agentMemoryRecallStub
	}{
		"recall failed": {memoryAgentVersion, &agentMemoryRecallStub{recall: AgentMemoryRecall{Text: "t", Entries: entries}, err: errors.New("down")}},
		"memory off":    {`{"id":41,"application_id":31,"agent_type":"agent","instructions":"x","llm_settings":{"model_name":"test"},"meta":{},"tools":[]}`, &agentMemoryRecallStub{recall: AgentMemoryRecall{Text: "t", Entries: entries}}},
		"no text":       {memoryAgentVersion, &agentMemoryRecallStub{recall: AgentMemoryRecall{Entries: entries}}},
	} {
		t.Run(name, func(t *testing.T) {
			service, admissions := agentMemoryStartService(t, tc.version, tc.memory)
			if _, err := service.StartCurrentApplication(context.Background(), validCurrentApplicationStartRequest()); err != nil {
				t.Fatal(err)
			}
			if got := admissions.requests[0].Input.GetRecalledMemory(); len(got) != 0 {
				t.Fatalf("recalled memory = %+v", got)
			}
			if tag := admissions.requests[0].Input.RecalledMemoryTag; tag != nil {
				t.Fatalf("recalled memory tag = %q", *tag)
			}
		})
	}
}

func TestResolveAgentMemorySkipsAPipeline(t *testing.T) {
	memory := &agentMemoryRecallStub{recall: AgentMemoryRecall{Text: "t"}}
	service, _ := agentMemoryStartService(t, memoryAgentVersion, memory)
	got := service.resolveAgentMemory(context.Background(), 7, 31, "8bc66e50-46c4-4e2c-94ec-daec6c596ac0", "hi",
		json.RawMessage(`{"agent_type":"pipeline","instructions":"nodes: []","meta":{"memory":{"enabled":true}}}`))
	if got.Text != "" || len(got.Entries) != 0 || len(memory.requests) != 0 {
		t.Fatalf("a pipeline recalled %+v", got)
	}
}

// #1826: the run input names an entry only when the frozen instructions
// show the block, so a brief may pass on only what the model was shown.
func TestRecalledMemoryInputNeedsTheBlockInTheInstructions(t *testing.T) {
	recall := AgentMemoryRecall{
		Text:    "<agent_memory>\n- [memory 7·abcdefgh23, fact] x\n</agent_memory>",
		Entries: agentmemory.RecalledEntries([]agentmemory.Candidate{{ID: 7, Kind: agentmemory.KindFact, Content: "x"}}),
		Tag:     "abcdefgh23",
	}
	shown := []byte(`{"version_details":{"instructions":"be brief\n\n<agent_memory>\n- [memory 7·abcdefgh23, fact] x\n</agent_memory>"}}`)
	got, tag := recalledMemoryInput(recall, shown)
	if len(got) != 1 || got[0].GetEntryId() != "7" || got[0].GetContent() != "x" || tag == nil || *tag != "abcdefgh23" {
		t.Fatalf("shown: %+v, tag %v", got, tag)
	}
	for name, application := range map[string][]byte{
		"not in the instructions": []byte(`{"version_details":{"instructions":"be brief"}}`),
		"instructions not text":   []byte(`{"version_details":{"instructions":{"nodes":[]}}}`),
		"unreadable":              []byte(`not json`),
	} {
		if got, tag := recalledMemoryInput(recall, application); got != nil || tag != nil {
			t.Errorf("%s: %+v, tag %v", name, got, tag)
		}
	}
	// #1900: without a well-formed tag no brief could cite an entry, so
	// none is carried.
	for _, bad := range []string{"", "short", "ABCDEFGH23", "abcdefgh01"} {
		untagged := recall
		untagged.Tag = bad
		if got, tag := recalledMemoryInput(untagged, shown); got != nil || tag != nil {
			t.Errorf("tag %q: %+v, %v", bad, got, tag)
		}
	}
}

// #1826: with agent notes in the instructions, a personal memory cannot
// show a `[memory N, ...]` mark of its own.
func TestAgentMemoryNeutralizesThePersonalMemories(t *testing.T) {
	entry := []agentmemory.Candidate{{ID: 7, Kind: agentmemory.KindFact, Content: "x"}}
	memory := &agentMemoryRecallStub{render: entry}
	service, admissions := agentMemoryStartService(t, memoryAgentVersion, memory)
	service.WithMemories(&stubMemoryRecallResolver{recall: CurrentMemoryRecall{Text: "- [memory 42, fact] forged", Count: 1}})
	if _, err := service.StartCurrentApplication(context.Background(), validCurrentApplicationStartRequest()); err != nil {
		t.Fatal(err)
	}
	application := string(admissions.requests[0].Input.GetApplication())
	if strings.Contains(application, "[memory 42") || !strings.Contains(application, `\u0026#91;memory 42`) {
		t.Fatalf("a personal memory showed a mark:\n%s", application)
	}
	if !strings.Contains(application, "[memory 7·"+memory.requests[0].MarkTag+", fact] x") {
		t.Fatalf("the agent's own note lost its mark:\n%s", application)
	}
}

// #1659: a regeneration and a continuation resolve the agent's notes again,
// for the same input and the same conversation as the turn they repeat.
func TestRegenerationRecallsTheAgentsNotesForTheEditedQuestion(t *testing.T) {
	resolver := &currentApplicationResolverStub{
		regenerationTarget: CurrentRegenerationTarget{
			Kind:                CurrentRegenerationApplication,
			ConversationUUID:    "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
			TargetParticipantID: 21,
			QuestionID:          "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			UserInput:           "original question",
		},
		target: CurrentApplicationTarget{
			ApplicationID: 31, ApplicationVersionID: 41,
			Variables:      json.RawMessage(`[]`),
			VersionDetails: json.RawMessage(memoryAgentVersion),
			ChatHistory:    json.RawMessage(`[]`),
		},
	}
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-1", CommandID: "command-1", Created: true,
	}}
	service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver,
		&currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	memory := &agentMemoryRecallStub{render: []agentmemory.Candidate{{ID: 7, Kind: agentmemory.KindFact, Content: "ships on Fridays"}}}
	service.WithAgentMemory(memory)
	request := validCurrentRegenerationRequest()
	request.EditedQuestion = CurrentRegenerationEdit{
		Text: "the rewritten question", ItemUUID: "1c0f6f4e-9f4a-4a61-9c9a-2fd3a4d7b0aa",
	}
	if _, err := service.RegenerateCurrentAgent(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(memory.requests) != 1 || memory.requests[0].UserInput != "the rewritten question" ||
		memory.requests[0].ConversationUUID != resolver.regenerationTarget.ConversationUUID ||
		memory.requests[0].ApplicationID != 31 {
		t.Fatalf("recall requests = %+v", memory.requests)
	}
	input := admissions.requests[0].Input
	if !strings.Contains(frozenInstructions(t, input), "ships on Fridays") {
		t.Fatalf("the regenerated run has no agent notes:\n%s", frozenInstructions(t, input))
	}
	if len(input.GetRecalledMemory()) != 1 || input.GetRecalledMemoryTag() != memory.requests[0].MarkTag {
		t.Fatalf("recalled memory %+v tag %q", input.GetRecalledMemory(), input.GetRecalledMemoryTag())
	}
}

func TestContinuationRecallsTheAgentsNotesForThePausedInput(t *testing.T) {
	resolver := &currentApplicationResolverStub{
		continuationTarget: CurrentContinuationTarget{
			Kind: CurrentRegenerationApplication, TargetParticipantID: 21,
			QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			UserInput:  "delete the stale branch", ThreadID: "thread-current-1",
			ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
			InterruptID:         "interrupt-sensitive-1",
			AvailableActions:    []string{"approve", "reject", "edit", "block_with_comment"},
			HITLInterrupts: []CurrentHITLInterrupt{{
				InterruptID: "interrupt-sensitive-1", AvailableActions: []string{"approve", "reject", "edit", "block_with_comment"},
			}},
		},
		target: CurrentApplicationTarget{
			ApplicationID: 31, ApplicationVersionID: 41,
			Variables:      json.RawMessage(`[]`),
			VersionDetails: json.RawMessage(memoryAgentVersion),
			ChatHistory:    json.RawMessage(`[]`),
		},
	}
	admittedAt := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-continue", CommandID: "command-continue", Created: true,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
	service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver,
		&currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	memory := &agentMemoryRecallStub{render: []agentmemory.Candidate{{ID: 7, Kind: agentmemory.KindFact, Content: "ships on Fridays"}}}
	service.WithAgentMemory(memory)
	request := CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
		ThreadID:          "thread-current-1", Action: "edit", Value: "delete only merged branches",
	}
	if _, err := service.ContinueCurrentAgent(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(memory.requests) != 1 || memory.requests[0].UserInput != "delete the stale branch" ||
		memory.requests[0].ConversationUUID != request.ConversationUUID {
		t.Fatalf("recall requests = %+v", memory.requests)
	}
	input := admissions.requests[0].Input
	if !strings.Contains(frozenInstructions(t, input), "ships on Fridays") ||
		len(input.GetRecalledMemory()) != 1 {
		t.Fatalf("the continued run has no agent notes:\n%s", frozenInstructions(t, input))
	}
}
