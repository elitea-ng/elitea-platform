package agentexecution

// Conversation skills (ADR-0050 amendment 1, A1.2; skills epic story S3): the
// skills pinned to a conversation join every turn of that conversation, on both
// start contracts, ahead of the message's own picks.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

type conversationSkillsStub struct {
	pinned map[string]ConversationSkills
	err    error
	calls  int
	asked  string
}

func (stub *conversationSkillsStub) ResolveConversationSkills(_ context.Context, _ int64, conversationUUID string) (ConversationSkills, error) {
	stub.calls++
	stub.asked = conversationUUID
	if stub.err != nil {
		return ConversationSkills{}, stub.err
	}
	return stub.pinned[conversationUUID], nil
}

func pinnedConversation(request CurrentAdhocStartRequest, pins ConversationSkills) *conversationSkillsStub {
	return &conversationSkillsStub{pinned: map[string]ConversationSkills{request.ConversationUUID: pins}}
}

func listingSkill(listing, version int64, name, instructions string) CurrentListingSkill {
	return CurrentListingSkill{ListingID: listing, VersionID: version, Name: name, Description: name + " d", Instructions: instructions}
}

func TestAConversationPinAppliesToTheTurnWithoutBeingPicked(t *testing.T) {
	stub := &projectSkillResolverStub{project: 1, skills: map[int64]CurrentProjectSkill{
		4: projectSkill(4, "Triage", "Triage steps"), 5: projectSkill(5, "Release", "Release steps"),
	}}
	service, admissions := adhocSkillsService(t, stub)
	request := adhocRequestWithPicks("what now?", 5)
	request.ProjectID = 1
	pins := pinnedConversation(request, ConversationSkills{ProjectSkillIDs: []int64{4}})
	service.WithConversationSkills(pins)

	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatalf("StartCurrentAdhoc() error = %v", err)
	}
	invoked := skillObjects(t, admissions.requests[0].Input.GetInvokedSkills())
	if len(invoked) != 2 || invoked[0]["name"] != "Triage" || invoked[1]["name"] != "Release" {
		t.Fatalf("the pin comes first, then the pick: %#v", invoked)
	}
	if pins.asked != request.ConversationUUID {
		t.Fatalf("the pins were read for %q", pins.asked)
	}
	if !bytes.Equal(admissions.requests[0].Input.GetUserInput(), []byte(`"what now?"`)) {
		t.Fatalf("worker input = %s", admissions.requests[0].Input.GetUserInput())
	}
}

func TestAPinThatIsAlsoPickedIsAppliedOnce(t *testing.T) {
	stub := &projectSkillResolverStub{project: 1, skills: map[int64]CurrentProjectSkill{4: projectSkill(4, "Triage", "Triage steps")}}
	service, admissions := adhocSkillsService(t, stub)
	request := adhocRequestWithPicks("use ~Triage", 4)
	request.ProjectID = 1
	service.WithConversationSkills(pinnedConversation(request, ConversationSkills{ProjectSkillIDs: []int64{4}}))

	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if invoked := skillObjects(t, admissions.requests[0].Input.GetInvokedSkills()); len(invoked) != 1 {
		t.Fatalf("invoked = %#v", invoked)
	}
}

// A pinned listing version is frozen from the listing, untrusted, with an id
// that names the listing and the version; nothing about it is a project skill.
func TestAPinnedListingVersionIsFrozenUntrusted(t *testing.T) {
	service, admissions := adhocSkillsService(t, &projectSkillResolverStub{project: 1})
	request := adhocRequestWithPicks("go")
	request.ProjectID = 1
	service.WithConversationSkills(pinnedConversation(request, ConversationSkills{
		Listing: []CurrentListingSkill{listingSkill(9, 12, "Reviewer", "Review steps")},
	}))

	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatalf("StartCurrentAdhoc() error = %v", err)
	}
	invoked := skillObjects(t, admissions.requests[0].Input.GetInvokedSkills())
	if len(invoked) != 1 {
		t.Fatalf("invoked = %#v", invoked)
	}
	got := invoked[0]
	if got["id"] != "listing:9:version:12" || got["scope"] != "marketplace:9:12" || got["trust"] != "untrusted" ||
		got["instructions"] != "Review steps" || got["revision"] != currentInstructionRevision("Review steps") ||
		got["skill_id"] != nil {
		t.Fatalf("listing snapshot = %#v", got)
	}
	if _, leaked := got["pinned"]; leaked {
		t.Fatalf("the internal pin marker must not reach the worker: %#v", got)
	}
}

func TestPinsAndPicksTogetherAreHeldToFiveSkills(t *testing.T) {
	skills := map[int64]CurrentProjectSkill{}
	for id := int64(1); id <= 5; id++ {
		skills[id] = projectSkill(id, "Skill"+string(rune('A'+id)), "steps")
	}
	service, admissions := adhocSkillsService(t, &projectSkillResolverStub{project: 1, skills: skills})
	request := adhocRequestWithPicks("go", 3, 4)
	request.ProjectID = 1
	service.WithConversationSkills(pinnedConversation(request, ConversationSkills{
		ProjectSkillIDs: []int64{1, 2},
		Listing:         []CurrentListingSkill{listingSkill(9, 12, "Reviewer", "Review steps")},
	}))

	// Three pins and two picks make five: allowed.
	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatalf("five skills: error = %v", err)
	}
	// A pick a pin already holds is not another skill: still five.
	request.ProjectSkillIDs = []int64{2, 3, 4}
	request.QuestionID = "00000000-0000-4000-8000-0000000000b2"
	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatalf("a pinned skill picked again is not a sixth: error = %v", err)
	}
	if len(admissions.requests) != 2 {
		t.Fatalf("admitted = %d, want 2", len(admissions.requests))
	}
	// Three distinct picks over three pins make six: refused, not cut.
	request.ProjectSkillIDs = []int64{3, 4, 5}
	request.QuestionID = "00000000-0000-4000-8000-0000000000b3"
	if _, err := service.StartCurrentAdhoc(context.Background(), request); !errors.Is(err, ErrProjectSkillsTooMany) {
		t.Fatalf("six skills: error = %v", err)
	}
	if len(admissions.requests) != 2 {
		t.Fatal("nothing more may be admitted over the limit")
	}
}

func TestAPinThatCannotBeReadRefusesTheTurn(t *testing.T) {
	service, admissions := adhocSkillsService(t, &projectSkillResolverStub{project: 1})
	request := adhocRequestWithPicks("go")
	request.ProjectID = 1
	service.WithConversationSkills(&conversationSkillsStub{err: ErrProjectSkillUnknown})

	if _, err := service.StartCurrentAdhoc(context.Background(), request); !errors.Is(err, ErrProjectSkillUnknown) {
		t.Fatalf("error = %v", err)
	}
	if len(admissions.requests) != 0 {
		t.Fatal("nothing may be admitted")
	}
}

func TestAConversationWithoutPinsReadsNothingExtra(t *testing.T) {
	service, admissions := adhocSkillsService(t, &projectSkillResolverStub{project: 1})
	request := adhocRequestWithPicks("follow up")
	request.ProjectID = 1
	stub := &conversationSkillsStub{pinned: map[string]ConversationSkills{}}
	service.WithConversationSkills(stub)

	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := admissions.requests[0].Input.GetInvokedSkills(); !bytes.Equal(got, []byte(`[]`)) {
		t.Fatalf("invoked = %s", got)
	}
	if stub.calls != 1 {
		t.Fatalf("the pins are read once per turn, calls = %d", stub.calls)
	}
}

// On an agent turn a pin never refuses the conversation: a name the agent's
// own skill already holds drops the PIN, while a message's PICK of such a name
// still refuses, as before.
func TestAPinWhoseNameTheAgentAlreadyHoldsIsDroppedNotRefused(t *testing.T) {
	other := projectSkill(77, "Release", "Project release steps")
	pin := map[string]any{
		"skill_id": other.SkillID, "skill_version_id": other.BaseVersionID, "name": other.Name, "version_name": "base",
		"description": other.Description, "icon_meta": nil, "instructions": other.Instructions,
		"id": "skill:77:version:1077", "revision": currentInstructionRevision(other.Instructions), "scope": "project:7",
		"pinned": true,
	}
	projection, err := projectCurrentApplicationSkills("hello", []byte(versionWithOneSkill), []map[string]any{pin})
	if err != nil {
		t.Fatalf("a conflicting pin must not refuse the turn: %v", err)
	}
	if strings.Contains(string(projection.invoked), "Project release steps") {
		t.Fatalf("the dropped pin was applied: %s", projection.invoked)
	}

	pick := map[string]any{}
	for key, value := range pin {
		pick[key] = value
	}
	delete(pick, "pinned")
	if _, err := projectCurrentApplicationSkills("hello", []byte(versionWithOneSkill), []map[string]any{pick}); !errors.Is(err, ErrProjectSkillNameConflict) {
		t.Fatalf("a conflicting pick still refuses: %v", err)
	}
}
