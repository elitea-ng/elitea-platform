package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type projectSkillResolverStub struct {
	skills map[int64]CurrentProjectSkill
	calls  int
	// project is the one project the stub's skills live in, as a tenant
	// schema scopes them; any other project finds none.
	project int64
}

func (stub *projectSkillResolverStub) ResolveCurrentProjectSkills(_ context.Context, projectID int64, ids []int64) ([]CurrentProjectSkill, error) {
	stub.calls++
	result := make([]CurrentProjectSkill, 0, len(ids))
	for _, id := range ids {
		skill, ok := stub.skills[id]
		if !ok || projectID != stub.project {
			return nil, ErrProjectSkillUnknown
		}
		result = append(result, skill)
	}
	return result, nil
}

func projectSkill(id int64, name, instructions string) CurrentProjectSkill {
	return CurrentProjectSkill{SkillID: id, BaseVersionID: id + 1000, Name: name, Description: name + " d", Instructions: instructions, IconMeta: json.RawMessage(`null`)}
}

func startRequestWithPicks(project int64, ids ...int64) CurrentApplicationStartRequest {
	request := validCurrentApplicationStartRequest()
	request.ProjectID = project
	request.ProjectSkillIDs = ids
	return request
}

func TestResolveProjectSkillsFreezesWhatTheServerReadAndNothingElse(t *testing.T) {
	stub := &projectSkillResolverStub{project: 7, skills: map[int64]CurrentProjectSkill{4: projectSkill(4, "Triage", "Triage steps")}}
	service := (&CurrentApplicationStartService{}).WithProjectSkills(stub)

	snapshots, err := service.resolveProjectSkills(context.Background(), startRequestWithPicks(7, 4))
	if err != nil {
		t.Fatalf("resolveProjectSkills() error = %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %#v", snapshots)
	}
	got := snapshots[0]
	if got["id"] != "skill:4:version:1004" || got["scope"] != "project:7" ||
		got["revision"] != currentInstructionRevision("Triage steps") || got["instructions"] != "Triage steps" {
		t.Fatalf("snapshot = %#v", got)
	}
}

// The permission check: a skill the caller cannot read (another project's, or
// one folder access hides) is refused as unknown, and the turn does not start.
func TestResolveProjectSkillsRefusesASkillTheCallerCannotRead(t *testing.T) {
	stub := &projectSkillResolverStub{project: 8, skills: map[int64]CurrentProjectSkill{4: projectSkill(4, "Triage", "Triage steps")}}
	service := (&CurrentApplicationStartService{}).WithProjectSkills(stub)

	if _, err := service.resolveProjectSkills(context.Background(), startRequestWithPicks(7, 4)); !errors.Is(err, ErrProjectSkillUnknown) {
		t.Fatalf("another project's skill: error = %v", err)
	}
	if _, err := service.resolveProjectSkills(context.Background(), startRequestWithPicks(8, 4, 5)); !errors.Is(err, ErrProjectSkillUnknown) {
		t.Fatalf("one unknown id refuses the whole pick: error = %v", err)
	}
	// No resolver attached: a pick is refused, never ignored.
	if _, err := (&CurrentApplicationStartService{}).resolveProjectSkills(context.Background(), startRequestWithPicks(8, 4)); !errors.Is(err, ErrProjectSkillUnknown) {
		t.Fatalf("no resolver: error = %v", err)
	}
	// A skill with no instructions is as good as absent.
	blank := &projectSkillResolverStub{project: 8, skills: map[int64]CurrentProjectSkill{4: projectSkill(4, "Triage", "  ")}}
	if _, err := (&CurrentApplicationStartService{}).WithProjectSkills(blank).resolveProjectSkills(context.Background(), startRequestWithPicks(8, 4)); !errors.Is(err, ErrProjectSkillUnknown) {
		t.Fatalf("blank skill: error = %v", err)
	}
}

func TestResolveProjectSkillsReadsNothingWhenNothingWasPicked(t *testing.T) {
	stub := &projectSkillResolverStub{project: 7}
	service := (&CurrentApplicationStartService{}).WithProjectSkills(stub)
	snapshots, err := service.resolveProjectSkills(context.Background(), startRequestWithPicks(7))
	if err != nil || snapshots != nil || stub.calls != 0 {
		t.Fatalf("snapshots=%v err=%v calls=%d", snapshots, err, stub.calls)
	}
}

func TestResolveProjectSkillsRefusesSixPicks(t *testing.T) {
	stub := &projectSkillResolverStub{project: 7, skills: map[int64]CurrentProjectSkill{}}
	service := (&CurrentApplicationStartService{}).WithProjectSkills(stub)
	if _, err := service.resolveProjectSkills(context.Background(), startRequestWithPicks(7, 1, 2, 3, 4, 5, 6)); !errors.Is(err, ErrProjectSkillsTooMany) {
		t.Fatalf("error = %v", err)
	}
	if stub.calls != 0 {
		t.Fatal("six picks must be refused before anything is read")
	}
}

func TestRequestValidateRefusesMalformedProjectSkillIDs(t *testing.T) {
	for name, ids := range map[string][]int64{"zero": {0}, "negative": {-1}, "past int32": {1 << 31}, "repeat": {3, 3}} {
		request := validCurrentApplicationStartRequest()
		request.ProjectSkillIDs = ids
		if err := request.Validate(); !errors.Is(err, ErrInvalidCurrentAgentStart) {
			t.Fatalf("%s: Validate() = %v", name, err)
		}
	}
}

func frozenSnapshot(t *testing.T, ctx context.Context, skills ...CurrentProjectSkill) []map[string]any {
	t.Helper()
	stub := &projectSkillResolverStub{project: 7, skills: map[int64]CurrentProjectSkill{}}
	ids := make([]int64, 0, len(skills))
	for _, skill := range skills {
		stub.skills[skill.SkillID] = skill
		ids = append(ids, skill.SkillID)
	}
	snapshots, err := (&CurrentApplicationStartService{}).WithProjectSkills(stub).resolveProjectSkills(ctx, startRequestWithPicks(7, ids...))
	if err != nil {
		t.Fatalf("resolveProjectSkills() error = %v", err)
	}
	return snapshots
}

const versionWithOneSkill = `{
	"agent_type":"agent",
	"instructions":"Be brief.",
	"skills":[{"skill_id":1,"skill_version_id":11,"id":"skill:1:version:11","revision":"r","scope":"project:7","name":"Release","version_name":"base","description":"d","icon_meta":null,"instructions":"Release steps"}]
}`

func TestProjectedProjectSkillIsInvokedButNotInTheLoadSkillCatalogue(t *testing.T) {
	picks := frozenSnapshot(t, context.Background(), projectSkill(4, "Triage", "Triage steps"))

	projected, err := projectCurrentApplicationSkills("please ~Triage this", json.RawMessage(versionWithOneSkill), picks)
	if err != nil {
		t.Fatalf("projectCurrentApplicationSkills() error = %v", err)
	}
	if projected.userInput != "please Triage this" {
		t.Fatalf("user input = %q", projected.userInput)
	}
	invoked := decodeCurrentSkillList(t, projected.invoked)
	if len(invoked) != 1 || invoked[0]["name"] != "Triage" || invoked[0]["instructions"] != "Triage steps" ||
		invoked[0]["id"] != "skill:4:version:1004" || invoked[0]["scope"] != "project:7" {
		t.Fatalf("invoked = %#v", invoked)
	}
	if applied := decodeCurrentSkillList(t, projected.applied); len(applied) != 1 || applied[0]["name"] != "Triage" {
		t.Fatalf("applied = %#v", applied)
	}
	for _, skill := range decodeCurrentSkillList(t, projected.attached) {
		if skill["name"] == "Triage" {
			t.Fatalf("a project skill reached the load_skill catalogue: %#v", skill)
		}
	}
}

// A pick is a choice, not a mention: it is invoked even when the text no
// longer names it. An unpicked `~Name` is left alone.
func TestProjectSkillPickDoesNotDependOnTheText(t *testing.T) {
	picks := frozenSnapshot(t, context.Background(), projectSkill(4, "Triage", "Triage steps"))
	projected, err := projectCurrentApplicationSkills("hello ~Other", json.RawMessage(versionWithOneSkill), picks)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if projected.userInput != "hello ~Other" || len(decodeCurrentSkillList(t, projected.invoked)) != 1 {
		t.Fatalf("input=%q invoked=%s", projected.userInput, projected.invoked)
	}
	none, err := projectCurrentApplicationSkills("hello ~Triage", json.RawMessage(versionWithOneSkill), nil)
	if err != nil || none.userInput != "hello ~Triage" || len(decodeCurrentSkillList(t, none.invoked)) != 0 {
		t.Fatalf("a project skill that was not picked must not be invoked: input=%q invoked=%s err=%v", none.userInput, none.invoked, err)
	}
}

func TestProjectSkillsCountWithTheVersionsSkillsTowardFive(t *testing.T) {
	var picks []CurrentProjectSkill
	for id := int64(10); id < 14; id++ {
		picks = append(picks, projectSkill(id, "P"+string(rune('a'+id-10)), "steps"))
	}
	snapshots := frozenSnapshot(t, context.Background(), picks...)
	// 4 project + the version's 1 via ~Release = 5: fine.
	if _, err := projectCurrentApplicationSkills("~Release", json.RawMessage(versionWithOneSkill), snapshots); err != nil {
		t.Fatalf("five together: error = %v", err)
	}
	// 5 project + 1 version = 6: refused.
	six := frozenSnapshot(t, context.Background(), append(picks, projectSkill(20, "Pe", "steps"))...)
	if _, err := projectCurrentApplicationSkills("~Release", json.RawMessage(versionWithOneSkill), six); !errors.Is(err, ErrProjectSkillsTooMany) {
		t.Fatalf("six together: error = %v", err)
	}
}

func TestProjectSkillsOverSixtyFourKiBAreRefusedNotCut(t *testing.T) {
	big := strings.Repeat("a", currentMaxProjectSkillBytes/2+1)
	picks := frozenSnapshot(t, context.Background(), projectSkill(4, "One", big), projectSkill(5, "Two", big))
	if _, err := projectCurrentApplicationSkills("hi", json.RawMessage(versionWithOneSkill), picks); !errors.Is(err, ErrProjectSkillsTooLarge) {
		t.Fatalf("error = %v", err)
	}
	fits := frozenSnapshot(t, context.Background(), projectSkill(4, "One", strings.Repeat("a", currentMaxProjectSkillBytes)))
	if _, err := projectCurrentApplicationSkills("hi", json.RawMessage(versionWithOneSkill), fits); err != nil {
		t.Fatalf("exactly 64 KiB must fit: error = %v", err)
	}
}

func TestProjectSkillNamedLikeAnotherVersionSkillIsRefused(t *testing.T) {
	picks := frozenSnapshot(t, context.Background(), projectSkill(4, "release", "other steps"))
	if _, err := projectCurrentApplicationSkills("hi", json.RawMessage(versionWithOneSkill), picks); !errors.Is(err, ErrProjectSkillNameConflict) {
		t.Fatalf("error = %v", err)
	}
	// The version's own skill picked again is that skill, not a conflict.
	same := []map[string]any{{
		"skill_id": 1, "skill_version_id": 11, "id": "skill:1:version:11", "name": "Release",
		"instructions": "Release steps", "revision": "r", "scope": "project:7",
	}}
	projected, err := projectCurrentApplicationSkills("hi", json.RawMessage(versionWithOneSkill), same)
	if err != nil || len(decodeCurrentSkillList(t, projected.invoked)) != 1 {
		t.Fatalf("invoked=%s err=%v", projected.invoked, err)
	}
}
