package agentexecution

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProjectCurrentApplicationSkillsPreservesCurrentAgentContract(t *testing.T) {
	version := json.RawMessage(`{
		"agent_type":"agent",
		"instructions":"Use ~Release-Notes when publishing.",
		"skills":[
			{"skill_id":1,"skill_version_id":11,"name":"Release","version_name":"base","description":"Short release","icon_meta":null,"instructions":"Release instructions"},
			{"skill_id":2,"skill_version_id":22,"name":"Release-Notes","version_name":"v2","description":"Ship safely","icon_meta":{"icon":"notes"},"instructions":"Exact release-note steps"},
			{"skill_id":3,"skill_version_id":33,"name":"Blank","version_name":"base","description":"Ignored","icon_meta":null,"instructions":"   "}
		]
	}`)

	projected, err := projectCurrentApplicationSkills(
		"Please use ~release-notes and ~RELEASE-NOTES; leave ~unknown.",
		version,
		nil,
	)
	if err != nil {
		t.Fatalf("projectCurrentApplicationSkills() error = %v", err)
	}
	if projected.userInput != "Please use Release-Notes and Release-Notes; leave ~unknown." {
		t.Fatalf("user input = %q", projected.userInput)
	}

	invoked := decodeCurrentSkillList(t, projected.invoked)
	if len(invoked) != 1 || invoked[0]["name"] != "Release-Notes" ||
		invoked[0]["instructions"] != "Exact release-note steps" ||
		invoked[0]["skill_version_id"] != float64(22) {
		t.Fatalf("invoked skills = %#v", invoked)
	}
	applied := decodeCurrentSkillList(t, projected.applied)
	if len(applied) != 1 || applied[0]["name"] != "Release-Notes" ||
		applied[0]["skill_id"] != float64(2) || applied[0]["icon_meta"].(map[string]any)["icon"] != "notes" {
		t.Fatalf("applied skills = %#v", applied)
	}
	if _, leaked := applied[0]["instructions"]; leaked {
		t.Fatalf("applied skill leaked instructions: %#v", applied[0])
	}

	attached := decodeCurrentSkillList(t, projected.attached)
	if len(attached) != 2 {
		t.Fatalf("attached skills = %#v", attached)
	}
	if attached[0]["name"] != "Release" || attached[0]["description"] != "Short release" {
		t.Fatalf("first attached skill = %#v", attached[0])
	}
	description, _ := attached[1]["description"].(string)
	if attached[1]["name"] != "Release-Notes" ||
		!strings.HasSuffix(description, currentInstructionReferencedSkillHint) {
		t.Fatalf("referenced attached skill = %#v", attached[1])
	}

	var projectedVersion map[string]any
	if err := json.Unmarshal(projected.versionDetails, &projectedVersion); err != nil {
		t.Fatalf("decode version: %v", err)
	}
	if projectedVersion["instructions"] != "Use Release-Notes when publishing." {
		t.Fatalf("version instructions = %#v", projectedVersion["instructions"])
	}
}

func TestProjectCurrentApplicationSkillsKeepsPipelinesDeterministic(t *testing.T) {
	version := json.RawMessage(`{
		"agent_type":"pipeline",
		"instructions":"Run ~Release-Notes in this deterministic graph.",
		"skills":[{"skill_id":2,"skill_version_id":22,"name":"Release-Notes","version_name":"v2","description":"Ship safely","icon_meta":null,"instructions":"Exact steps"}]
	}`)

	projected, err := projectCurrentApplicationSkills("~release-notes", version, nil)
	if err != nil {
		t.Fatalf("projectCurrentApplicationSkills() error = %v", err)
	}
	if projected.userInput != "Release-Notes" || len(decodeCurrentSkillList(t, projected.invoked)) != 1 {
		t.Fatalf("pipeline message projection = %q invoked=%s", projected.userInput, projected.invoked)
	}
	if attached := decodeCurrentSkillList(t, projected.attached); len(attached) != 0 {
		t.Fatalf("pipeline attached skills = %#v", attached)
	}
	var projectedVersion map[string]any
	if err := json.Unmarshal(projected.versionDetails, &projectedVersion); err != nil {
		t.Fatalf("decode version: %v", err)
	}
	if projectedVersion["instructions"] != "Run ~Release-Notes in this deterministic graph." {
		t.Fatalf("pipeline instructions changed: %#v", projectedVersion["instructions"])
	}
}

func TestConsumeCurrentInvokedSkillsUsesStrictBoundariesAndCap(t *testing.T) {
	attached := make([]map[string]any, 0, currentMaxInvokedSkills+1)
	for index := 0; index < currentMaxInvokedSkills+1; index++ {
		attached = append(attached, map[string]any{
			"skill_id": index + 1, "skill_version_id": index + 101,
			"name": "Skill-" + string(rune('A'+index)), "version_name": "base",
			"icon_meta": nil, "instructions": "instructions",
		})
	}
	cleaned, invoked := consumeCurrentInvokedSkills(
		"x~Skill-A ~~Skill-B ~Skill-Ax ~skill-a ~skill-b ~skill-c ~skill-d ~skill-e ~skill-f",
		attached,
	)
	if cleaned != "x~Skill-A ~~Skill-B ~Skill-Ax Skill-A Skill-B Skill-C Skill-D Skill-E Skill-F" {
		t.Fatalf("cleaned = %q", cleaned)
	}
	if len(invoked) != currentMaxInvokedSkills || invoked[0]["name"] != "Skill-A" ||
		invoked[currentMaxInvokedSkills-1]["name"] != "Skill-E" {
		t.Fatalf("invoked = %#v", invoked)
	}
}

func TestProjectCurrentApplicationSkillsRejectsMalformedRegistry(t *testing.T) {
	_, err := projectCurrentApplicationSkills("hello", json.RawMessage(`{"skills":{}}`), nil)
	if err != ErrUnsupportedCurrentAgentStart {
		t.Fatalf("error = %v", err)
	}
}

func decodeCurrentSkillList(t *testing.T, source json.RawMessage) []map[string]any {
	t.Helper()
	var result []map[string]any
	if err := json.Unmarshal(source, &result); err != nil {
		t.Fatalf("decode skill list %s: %v", source, err)
	}
	return result
}
