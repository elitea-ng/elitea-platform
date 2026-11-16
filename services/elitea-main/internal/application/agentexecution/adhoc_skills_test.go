package agentexecution

// A model chat takes project skills (ADR-0050 amendment 1, skills epic story
// S2): the ad-hoc start reads the picked ids under the caller's visibility and
// hands the worker the same invoked and applied skill snapshots an agent turn
// does. There is no skill runner; the worker's skill loading does the work.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
)

func adhocSkillsService(t *testing.T, stub *projectSkillResolverStub) (*CurrentApplicationStartService, *currentApplicationAdmissionStub) {
	t.Helper()
	resolver := &currentApplicationResolverStub{adhocTarget: CurrentAdhocTarget{
		TargetParticipantID: 21,
		LLMSettings:         json.RawMessage(`{"model_name":"saved","model_project_id":7}`),
		Tools:               json.RawMessage(`[]`),
		ChatHistory:         json.RawMessage(`[]`),
		ConversationMeta:    json.RawMessage(`{}`),
	}}
	admittedAt := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-adhoc", CommandID: "command-adhoc", Created: true,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
	service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver,
		&currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	return service.WithProjectSkills(stub), admissions
}

func adhocRequestWithPicks(text string, ids ...int64) CurrentAdhocStartRequest {
	request := validCurrentAdhocStartRequest()
	request.UserInput = text
	request.ProjectSkillIDs = ids
	return request
}

func skillObjects(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var skills []map[string]any
	if err := json.Unmarshal(raw, &skills); err != nil {
		t.Fatalf("skills %s: %v", raw, err)
	}
	return skills
}

func TestCurrentAdhocStartAppliesAPickedProjectSkill(t *testing.T) {
	stub := &projectSkillResolverStub{project: 1, skills: map[int64]CurrentProjectSkill{4: projectSkill(4, "Triage", "Triage steps")}}
	service, admissions := adhocSkillsService(t, stub)
	request := adhocRequestWithPicks("please ~Triage this", 4)
	request.ProjectID = 1

	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatalf("StartCurrentAdhoc() error = %v", err)
	}
	if len(admissions.requests) != 1 {
		t.Fatalf("admissions = %d", len(admissions.requests))
	}
	submitted := admissions.requests[0]
	invoked := skillObjects(t, submitted.Input.GetInvokedSkills())
	applied := skillObjects(t, submitted.Input.GetAppliedSkills())
	if len(invoked) != 1 || invoked[0]["id"] != "skill:4:version:1004" ||
		invoked[0]["instructions"] != "Triage steps" ||
		invoked[0]["revision"] != currentInstructionRevision("Triage steps") ||
		invoked[0]["scope"] != "project:1" {
		t.Fatalf("invoked = %#v", invoked)
	}
	if _, marked := invoked[0]["trust"]; marked {
		t.Fatalf("a trusted skill's snapshot carries no trust key: %#v", invoked[0])
	}
	if len(applied) != 1 || applied[0]["name"] != "Triage" {
		t.Fatalf("applied = %#v", applied)
	}
	// The worker reads the text without the sigil; the stored question keeps
	// what the person typed.
	if !bytes.Equal(submitted.Input.GetUserInput(), []byte(`"please Triage this"`)) {
		t.Fatalf("worker input = %s", submitted.Input.GetUserInput())
	}
	if submitted.CurrentAdhocTurn.UserInput != "please ~Triage this" {
		t.Fatalf("stored question = %q", submitted.CurrentAdhocTurn.UserInput)
	}
	// A pick is applied whether or not the text names it, and a model chat has
	// no catalogue to load from.
	if !bytes.Equal(submitted.Input.GetAttachedSkills(), []byte(`[]`)) {
		t.Fatalf("attached = %s", submitted.Input.GetAttachedSkills())
	}
}

func TestCurrentAdhocStartAppliesAPickTheTextDoesNotName(t *testing.T) {
	stub := &projectSkillResolverStub{project: 1, skills: map[int64]CurrentProjectSkill{4: projectSkill(4, "Triage", "Triage steps")}}
	service, admissions := adhocSkillsService(t, stub)
	request := adhocRequestWithPicks("what now?", 4)
	request.ProjectID = 1

	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	input := admissions.requests[0].Input
	if got := skillObjects(t, input.GetInvokedSkills()); len(got) != 1 || got[0]["name"] != "Triage" {
		t.Fatalf("invoked = %#v", got)
	}
	if !bytes.Equal(input.GetUserInput(), []byte(`"what now?"`)) {
		t.Fatalf("worker input = %s", input.GetUserInput())
	}
}

func TestCurrentAdhocStartWithoutPicksKeepsEmptySkills(t *testing.T) {
	service, admissions := adhocSkillsService(t, &projectSkillResolverStub{project: 1})
	request := adhocRequestWithPicks("follow up")
	request.ProjectID = 1
	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	input := admissions.requests[0].Input
	for name, raw := range map[string][]byte{"invoked": input.GetInvokedSkills(), "applied": input.GetAppliedSkills(), "attached": input.GetAttachedSkills()} {
		if !bytes.Equal(raw, []byte(`[]`)) {
			t.Fatalf("%s = %s", name, raw)
		}
	}
}

// Text from another project that this project has not trusted is frozen as
// untrusted, so the worker quotes it as data (skills epic story S1).
func TestCurrentAdhocStartFreezesAnUntrustedPickAsUntrusted(t *testing.T) {
	fork := projectSkill(4, "Triage", "Triage steps")
	fork.Untrusted, fork.PublisherProjectID = true, 1
	stub := &projectSkillResolverStub{project: 7, skills: map[int64]CurrentProjectSkill{4: fork}}
	service, admissions := adhocSkillsService(t, stub)
	request := adhocRequestWithPicks("go", 4)
	request.ProjectID = 7

	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	input := admissions.requests[0].Input
	invoked := skillObjects(t, input.GetInvokedSkills())
	applied := skillObjects(t, input.GetAppliedSkills())
	if len(invoked) != 1 || invoked[0]["trust"] != "untrusted" || len(applied) != 1 || applied[0]["trust"] != "untrusted" {
		t.Fatalf("invoked=%#v applied=%#v", invoked, applied)
	}
}

// A pick the caller cannot read refuses the turn before anything is admitted,
// with the answer a missing skill gets.
func TestCurrentAdhocStartRefusesAnUnreadablePickBeforeAdmission(t *testing.T) {
	stub := &projectSkillResolverStub{project: 8, skills: map[int64]CurrentProjectSkill{4: projectSkill(4, "Triage", "Triage steps")}}
	service, admissions := adhocSkillsService(t, stub)
	request := adhocRequestWithPicks("go", 4)
	request.ProjectID = 1 // the skill lives in project 8

	if _, err := service.StartCurrentAdhoc(context.Background(), request); !errors.Is(err, ErrProjectSkillUnknown) {
		t.Fatalf("error = %v", err)
	}
	if len(admissions.requests) != 0 {
		t.Fatal("nothing may be admitted")
	}
	// No resolver attached: a pick is refused, never ignored.
	bare, bareAdmissions := adhocSkillsService(t, nil)
	bare.projectSkills = nil
	if _, err := bare.StartCurrentAdhoc(context.Background(), request); !errors.Is(err, ErrProjectSkillUnknown) || len(bareAdmissions.requests) != 0 {
		t.Fatalf("no resolver: error = %v", err)
	}
}

func TestCurrentAdhocStartRefusesTooManyAndTooLargeSkills(t *testing.T) {
	skills := map[int64]CurrentProjectSkill{}
	for id := int64(1); id <= 5; id++ {
		skills[id] = projectSkill(id, "Skill"+string(rune('A'+id)), strings.Repeat("x", 14*1024))
	}
	stub := &projectSkillResolverStub{project: 1, skills: skills}
	service, admissions := adhocSkillsService(t, stub)

	// Six picks are refused before anything is read.
	six := adhocRequestWithPicks("go", 1, 2, 3, 4, 5, 6)
	six.ProjectID = 1
	if _, err := service.StartCurrentAdhoc(context.Background(), six); !errors.Is(err, ErrProjectSkillsTooMany) {
		t.Fatalf("six picks: error = %v", err)
	}
	// Five skills over 64 KiB together are refused, not cut.
	large := adhocRequestWithPicks("go", 1, 2, 3, 4, 5)
	large.ProjectID = 1
	if _, err := service.StartCurrentAdhoc(context.Background(), large); !errors.Is(err, ErrProjectSkillsTooLarge) {
		t.Fatalf("over 64 KiB: error = %v", err)
	}
	if len(admissions.requests) != 0 {
		t.Fatal("nothing may be admitted")
	}
}

func TestAdhocRequestValidateRefusesMalformedProjectSkillIDs(t *testing.T) {
	for name, ids := range map[string][]int64{"zero": {0}, "negative": {-1}, "past int32": {1 << 31}, "repeat": {3, 3}} {
		request := validCurrentAdhocStartRequest()
		request.ProjectSkillIDs = ids
		if err := request.Validate(); !errors.Is(err, ErrInvalidCurrentAgentStart) {
			t.Fatalf("%s: Validate() = %v", name, err)
		}
	}
}
