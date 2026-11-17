package agentexecution

import (
	"context"
	"testing"
)

// One person's pin must not make the conversation unusable for another: a
// project skill the caller cannot read is not applied for them, and the turn
// still starts with the pins and picks they can read.
func TestAProjectPinTheCallerCannotReadIsSkippedNotRefused(t *testing.T) {
	// Skill 4 is readable; skill 9 is pinned but the resolver does not find it
	// for this caller (hidden by folder access).
	stub := &projectSkillResolverStub{project: 1, skills: map[int64]CurrentProjectSkill{4: projectSkill(4, "Triage", "Triage steps")}}
	service, admissions := adhocSkillsService(t, stub)
	request := adhocRequestWithPicks("go")
	request.ProjectID = 1
	service.WithConversationSkills(pinnedConversation(request, ConversationSkills{ProjectSkillIDs: []int64{9, 4}}))

	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatalf("a hidden pin must not refuse the turn: %v", err)
	}
	invoked := skillObjects(t, admissions.requests[0].Input.GetInvokedSkills())
	if len(invoked) != 1 || invoked[0]["name"] != "Triage" {
		t.Fatalf("invoked = %#v, want only the readable pin", invoked)
	}
}
