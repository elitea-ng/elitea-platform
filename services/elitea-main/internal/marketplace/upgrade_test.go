package marketplace

import (
	"encoding/json"
	"testing"
)

func model(items []Requirement) string {
	for _, item := range items {
		if item.Kind == RequirementToolkit || item.Kind == RequirementMCP {
			return ModelToolCalling
		}
	}
	return ModelChat
}

func toolkit(typ string, tools ...string) Requirement {
	return Requirement{Key: "toolkit:" + typ + ":" + typ, Kind: RequirementToolkit, Type: typ, Name: typ, Tools: tools}
}

func build(tier string, write bool, items ...Requirement) Requirements {
	return Requirements{Items: items, ModelClass: model(items), Tier: tier, HasWriteTools: write}
}

// The widening rule of ADR-0051 decision 4: a change that adds a tool, a
// capability or a requirement, or raises the tier, is never taken by itself.
func TestWideningOfAndAutoUpgradeRule(t *testing.T) {
	base := build("low", false, toolkit("github", "list_issues", "get_issue"))
	cases := []struct {
		name    string
		next    Requirements
		kinds   []string
		autoOK  bool
		agentAt string
	}{
		{name: "identical", next: base, autoOK: true, agentAt: "low"},
		{name: "a tool removed", next: build("low", false, toolkit("github", "list_issues")), autoOK: true, agentAt: "low"},
		{name: "a requirement removed", next: build("low", false), autoOK: true, agentAt: "low"},
		{name: "tier lowered", next: build("low", false, toolkit("github", "list_issues", "get_issue")), autoOK: true, agentAt: "low"},
		{name: "a tool added", next: build("low", false, toolkit("github", "list_issues", "get_issue", "search_code")),
			kinds: []string{WidenTool}, agentAt: "low"},
		{name: "a requirement added", next: build("low", false, toolkit("github", "list_issues", "get_issue"), toolkit("jira", "get_issue")),
			kinds: []string{WidenRequirement}, agentAt: "low"},
		{name: "a skill requirement added", next: build("low", false, toolkit("github", "list_issues", "get_issue"),
			Requirement{Key: "skill::notes", Kind: RequirementSkill, Name: "notes"}), kinds: []string{WidenRequirement}, agentAt: "low"},
		{name: "a write capability added", next: build("low", true, toolkit("github", "list_issues", "get_issue")),
			kinds: []string{WidenCapability}, agentAt: "low"},
		{name: "the tier raised", next: build("medium", false, toolkit("github", "list_issues", "get_issue")),
			kinds: []string{WidenTier}, agentAt: "low"},
		{name: "identical but the agent is medium", next: base, autoOK: false, agentAt: "medium"},
		{name: "everything at once", next: build("high", true, toolkit("github", "list_issues", "get_issue", "create_issue"), toolkit("jira", "get_issue")),
			kinds: []string{WidenRequirement, WidenTool, WidenCapability, WidenTier}, agentAt: "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wider := WideningOf(base, tc.next)
			got := map[string]bool{}
			for _, w := range wider {
				got[w.Kind] = true
			}
			for _, want := range tc.kinds {
				if !got[want] {
					t.Errorf("widening %v is missing %s", wider, want)
				}
			}
			if len(tc.kinds) == 0 && len(wider) != 0 {
				t.Errorf("widening = %v, want none", wider)
			}
			allowed, blockedBy := AutoUpgradeAllowed(tc.agentAt, base, tc.next)
			if allowed != tc.autoOK {
				t.Errorf("AutoUpgradeAllowed = %v (%v), want %v", allowed, blockedBy, tc.autoOK)
			}
			if !allowed && len(blockedBy) == 0 {
				t.Error("a refused auto upgrade must say why")
			}
		})
	}
}

func TestModelClassRaisedIsACapability(t *testing.T) {
	chat := Requirements{ModelClass: ModelChat, Tier: "low"}
	calling := Requirements{ModelClass: ModelToolCalling, Tier: "low"}
	if got := WideningOf(chat, calling); len(got) != 1 || got[0].Kind != WidenCapability {
		t.Fatalf("widening = %v, want one capability", got)
	}
}

func TestAutoUpgradeNeverAboveLowTier(t *testing.T) {
	prev := build("medium", false, toolkit("github", "list_issues"))
	if allowed, _ := AutoUpgradeAllowed("medium", prev, prev); allowed {
		t.Fatal("a medium tier agent must not auto upgrade, even to an identical version")
	}
	low := build("low", false, toolkit("github", "list_issues"))
	if allowed, _ := AutoUpgradeAllowed("low", low, prev); allowed {
		t.Fatal("an upgrade that raises the tier must not be automatic")
	}
}

func TestNeedsApproval(t *testing.T) {
	if NeedsApproval("low", build("low", false)) {
		t.Error("a low tier upgrade with no write tool is one click")
	}
	if !NeedsApproval("low", build("low", true)) {
		t.Error("a write tool needs an approval")
	}
	if !NeedsApproval("low", build("medium", false)) {
		t.Error("a target above low needs an approval")
	}
	if !NeedsApproval("high", build("low", false)) {
		t.Error("an agent above low needs an approval for any target")
	}
}

func TestChangesShowsDefinitionAndRequirementDiff(t *testing.T) {
	definition := func(instructions string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"version": map[string]any{"instructions": instructions}})
		return raw
	}
	prev := StoredVersion{Definition: definition("be brief"), Requirements: build("low", false)}
	next := StoredVersion{Definition: definition("be thorough"), Requirements: build("low", false, toolkit("github", "list_issues"))}
	changes, err := Changes(prev, next)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{}
	for _, change := range changes {
		paths[change.Path] = change.Op
	}
	if paths["/definition/version/instructions"] != "replace" {
		t.Errorf("changes = %v, want the instructions replaced", changes)
	}
	if paths["/requirements/items"] == "" {
		t.Errorf("changes = %v, want the requirement items changed", changes)
	}
	same, err := Changes(prev, prev)
	if err != nil || len(same) != 0 {
		t.Errorf("identical versions: changes = %v, err = %v", same, err)
	}
}

func TestMaxTier(t *testing.T) {
	if MaxTier("low", "high") != "high" || MaxTier("high", "low") != "high" || MaxTier("", "") != "low" {
		t.Error("MaxTier must pick the higher tier and read an unknown tier as low")
	}
}
