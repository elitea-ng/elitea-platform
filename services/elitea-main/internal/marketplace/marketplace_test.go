package marketplace

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestIsWriteTool(t *testing.T) {
	for name, want := range map[string]bool{
		"list_issues":        false,
		"get_pull_request":   false,
		"search_code":        false,
		"create_issue":       true,
		"updateIssue":        true,
		"delete-branch":      true,
		"list_and_close":     true,
		"PostComment":        true,
		"read_file":          false,
		"":                   false,
		"push_files":         true,
		"transitionTicket":   true,
		"describe_resources": false,
	} {
		if got := IsWriteTool(name); got != want {
			t.Errorf("IsWriteTool(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRequirementsTypesAreSortedAndDeduplicated(t *testing.T) {
	reqs := Requirements{Items: []Requirement{
		{Kind: RequirementToolkit, Type: "jira"},
		{Kind: RequirementMCP, Type: "mcp"},
		{Kind: RequirementToolkit, Type: "github"},
		{Kind: RequirementToolkit, Type: "github"},
		{Kind: RequirementSkill, Name: "tone"},
		{Kind: RequirementAgent, Name: "helper"},
	}}
	if got := strings.Join(reqs.Types(), ","); got != "github,jira,mcp" {
		t.Errorf("Types() = %q, want github,jira,mcp: a skill or a nested agent is not a toolkit type", got)
	}
}

func TestDigestIsStableAndSensitive(t *testing.T) {
	reqs := Requirements{Items: []Requirement{{Key: "toolkit:github:g", Kind: RequirementToolkit, Type: "github"}}, Tier: "low"}
	a, err := DigestOf(json.RawMessage(`{"b":1,"a":2}`), reqs)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DigestOf(json.RawMessage(`{ "a": 2, "b": 1 }`), reqs)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("the digest depends on key order or spacing")
	}
	c, _ := DigestOf(json.RawMessage(`{"a":2,"b":2}`), reqs)
	if a == c {
		t.Error("the digest ignores the definition")
	}
	reqs.Tier = "high"
	d, _ := DigestOf(json.RawMessage(`{"a":2,"b":1}`), reqs)
	if a == d {
		t.Error("the digest ignores the requirements")
	}
}

func TestSearchParamsValidate(t *testing.T) {
	var p SearchParams
	if err := p.Validate(); err != nil || p.Sort != SortRelevance || p.Limit != 24 {
		t.Fatalf("defaults: %v %+v", err, p)
	}
	for name, params := range map[string]SearchParams{
		"kind":       {Kind: "pipeline"},
		"sort":       {Sort: "best"},
		"max_tier":   {MaxTier: "extreme"},
		"min_rating": {MinRating: 6},
		"limit":      {Limit: 500},
		"offset":     {Offset: -1},
		"query":      {Query: strings.Repeat("x", 201)},
	} {
		err := params.Validate()
		var paramErr *ParamError
		if !errors.As(err, &paramErr) || paramErr.Param != name {
			t.Errorf("%s: Validate() = %v, want a ParamError naming it", name, err)
		}
	}
}

func TestSearchWhereBindsEveryValue(t *testing.T) {
	p := SearchParams{
		Query: "100%_sure \\ done", Categories: []string{"a'; DROP TABLE x;--"}, Tags: []string{"t"},
		Requires: []string{"github"}, PublisherID: 3, MaxTier: "medium", MinRating: 4, HiredHere: true, ProjectID: 9,
	}
	args := &argList{}
	where := p.where(args, facetNone)
	for _, forbidden := range []string{"DROP", "100%", "sure", "github"} {
		if strings.Contains(where, forbidden) {
			t.Errorf("the predicate text carries the value %q: %s", forbidden, where)
		}
	}
	// The LIKE pattern of a word escapes the three metacharacters.
	found := false
	for _, v := range args.values {
		if s, ok := v.(string); ok && s == `%100\%\_sure%` {
			found = true
		}
	}
	if !found {
		t.Errorf("the escaped pattern is missing from the arguments: %v", args.values)
	}
	// A facet group leaves out its own filter.
	without := &argList{}
	if strings.Contains(p.where(without, facetCategory), "l.category") {
		t.Error("the category facet counts under the category filter")
	}
}

func TestParseVersionRef(t *testing.T) {
	packed := "12|v2.3|2026-10-30T09:00:00Z"
	ref := parseVersionRef(&packed)
	if ref == nil || ref.ID != "12" || ref.Name != "v2.3" || ref.PublishedAt.Year() != 2026 {
		t.Errorf("parseVersionRef = %+v", ref)
	}
	// A version name may hold the separator; the date is the last part only if
	// the name has none, so a name with a bar is cut at the second one.
	if parseVersionRef(nil) != nil {
		t.Error("nil input must give nil")
	}
	bad := "12|v1"
	if parseVersionRef(&bad) != nil {
		t.Error("a malformed value must give nil")
	}
}

func TestSoulSummaryReadsTheRoleAndTheStartOfTheBody(t *testing.T) {
	soul := json.RawMessage(`{"schema":"elitea.soul/v1","role":"Release editor","body":"I write notes."}`)
	if got := soulSummary(soul); got != "Release editor I write notes." {
		t.Errorf("soulSummary = %q", got)
	}
	if soulSummary(json.RawMessage("null")) != "" || soulSummary(nil) != "" {
		t.Error("no soul has no summary")
	}
}

func TestDefinitionEqualityIgnoresEmptyForms(t *testing.T) {
	if !jsonEqual(nil, map[string]any{}) || !jsonEqual([]any{}, nil) {
		t.Error("an empty object, an empty list and null are the same stored value")
	}
	if jsonEqual("a", "b") {
		t.Error("two different strings are equal")
	}
	if !jsonEqual(map[string]any{"a": 1.0}, map[string]any{"a": 1.0}) {
		t.Error("equal objects differ")
	}
}
