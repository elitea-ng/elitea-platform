package agentproposal

import (
	"errors"
	"strings"
	"testing"
)

func valid() Request {
	return Request{
		EphemeralAgentID: " a_0000002a ",
		Rationale:        " It found the bug fast. ",
		Instructions:     "Review pull requests.",
		Profile: Profile{
			Label:       "Researcher",
			DisplayName: "Ada",
			Soul:        "Precise and brief.",
			Tools: []Tool{
				{Key: "GitHub/Read_File", ToolkitType: "github"},
				{Key: "search/*"},
				{Key: "github/read_file"},
			},
		},
		MemorySeeds: []Seed{{Content: "The repo uses Go 1.26."}, {Kind: "preference", Content: "Short answers."}},
	}
}

func TestCheckTrimsNormalisesAndKeepsSeeds(t *testing.T) {
	checked, err := Check(valid())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if checked.EphemeralAgentID != "a_0000002a" || checked.Rationale != "It found the bug fast." {
		t.Fatalf("not trimmed: %+v", checked)
	}
	if len(checked.Profile.Tools) != 2 || checked.Profile.Tools[0].Key != "github/read_file" ||
		checked.Profile.Tools[0].ToolkitType != "github" || checked.Profile.Tools[1].Key != "search/*" {
		t.Fatalf("tools not canonical and unique: %+v", checked.Profile.Tools)
	}
	if len(checked.MemorySeeds) != 2 || checked.MemorySeeds[0].Kind != "fact" || checked.MemorySeeds[1].Kind != "preference" {
		t.Fatalf("seeds: %+v", checked.MemorySeeds)
	}
}

func TestCheckRefusesWithTheInvalidOutcome(t *testing.T) {
	cases := map[string]func(*Request){
		"agent id":        func(r *Request) { r.EphemeralAgentID = "a_0000002A" },
		"no rationale":    func(r *Request) { r.Rationale = "  " },
		"long rationale":  func(r *Request) { r.Rationale = strings.Repeat("x", MaxRationaleBytes+1) },
		"long soul":       func(r *Request) { r.Profile.Soul = strings.Repeat("x", MaxSoulBytes+1) },
		"bad tool":        func(r *Request) { r.Profile.Tools = []Tool{{Key: "nokey"}} },
		"partial wild":    func(r *Request) { r.Profile.Tools = []Tool{{Key: "github/read*"}} },
		"seed kind":       func(r *Request) { r.MemorySeeds = []Seed{{Kind: "cursor", Content: "x"}} },
		"secret seed":     func(r *Request) { r.MemorySeeds = []Seed{{Content: "token ghp_" + strings.Repeat("a", 36)}} },
		"too many seeds":  func(r *Request) { r.MemorySeeds = make([]Seed, MaxSeeds+1) },
		"control in name": func(r *Request) { r.Profile.DisplayName = "A\nB" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := valid()
			mutate(&request)
			_, err := Check(request)
			var refused *Error
			if !errors.As(err, &refused) || refused.Code != OutcomeInvalid {
				t.Fatalf("want %s, got %v", OutcomeInvalid, err)
			}
		})
	}
}

// A spawn the runtime accepted is never refused for its form: a soul with
// CRLF line breaks or other control characters, model text with CRLF, and a
// tool key up to the runtime's 512-byte bound all pass.
func TestCheckAcceptsWhatTheRuntimeAccepted(t *testing.T) {
	request := valid()
	request.Profile.Soul = "Be careful.\r\n\x07Cite sources."
	request.Rationale = "It found\r\nthe bug fast."
	request.Instructions = "Step one.\r\nStep two."
	request.Profile.Tools = []Tool{{Key: "github/" + strings.Repeat("t", 300)}}
	checked, err := Check(request)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if checked.Profile.Soul != "Be careful.\r\n\x07Cite sources." {
		t.Fatalf("soul changed: %q", checked.Profile.Soul)
	}
	request = valid()
	request.Profile.Soul = "nul\x00"
	if _, err := Check(request); err == nil {
		t.Fatal("a NUL in the soul cannot be stored")
	}
	request = valid()
	request.Profile.Tools = []Tool{{Key: "github/" + strings.Repeat("t", MaxToolKeyBytes)}}
	if _, err := Check(request); err == nil {
		t.Fatal("a tool key past the runtime bound")
	}
}

// The rationale opens as the description, cut to the agent description
// bound so the editor can save the created agent.
func TestDraftCutsTheDescriptionToTheAgentBound(t *testing.T) {
	request := valid()
	request.Rationale = strings.Repeat("r", MaxRationaleBytes)
	checked, err := Check(request)
	if err != nil {
		t.Fatal(err)
	}
	description, _ := Draft(checked)["description"].(string)
	if len([]rune(description)) != MaxDescriptionChars {
		t.Fatalf("description %d characters", len([]rune(description)))
	}
}

func TestCheckKeepsAbsentToolsApartFromAnEmptyList(t *testing.T) {
	request := valid()
	request.Profile.Tools = nil
	absent, err := Check(request)
	if err != nil || absent.Profile.Tools != nil {
		t.Fatalf("absent tools: %v %+v", err, absent.Profile.Tools)
	}
	request.Profile.Tools = []Tool{}
	empty, err := Check(request)
	if err != nil || empty.Profile.Tools == nil {
		t.Fatalf("empty tools: %v %+v", err, empty.Profile.Tools)
	}
	if SpecDigest(absent.Profile) == SpecDigest(empty.Profile) {
		t.Fatal("absent and empty tools must digest differently, as in the runtime")
	}
}

// The vectors are Profile::digest of the same profiles in the runtime
// (libs/rust/agent-runtime subagents/profile_tests.rs
// a_profile_digest_matches_mains_vectors); both sides assert them.
func TestSpecDigestMatchesTheRuntime(t *testing.T) {
	if got := SpecDigest(Profile{}); got != emptyProfileDigest {
		t.Fatalf("empty profile digest %s", got)
	}
	checked, err := Check(valid())
	if err != nil {
		t.Fatal(err)
	}
	if got := SpecDigest(checked.Profile); got != sampleProfileDigest {
		t.Fatalf("sample profile digest %s", got)
	}
	// The label is not part of the digest.
	relabelled := checked.Profile
	relabelled.Label = "Other"
	if SpecDigest(relabelled) != sampleProfileDigest {
		t.Fatal("the label changed the digest")
	}
}

const (
	emptyProfileDigest  = "8e93047b83c690ec8ada61c3103afaece889147e8adf85467c1e173e51b72083"
	sampleProfileDigest = "e2b787c2bbc4e4526a87f569a06e3ab0cf846cae3febc48157c4ecd4b2fc09dc"
)

func TestDraftStartsLowAndCarriesCategories(t *testing.T) {
	checked, err := Check(valid())
	if err != nil {
		t.Fatal(err)
	}
	draft := Draft(checked)
	if draft["risk_tier"] != "low" || draft["name"] != "Ada" || draft["instructions"] != "Review pull requests." {
		t.Fatalf("draft %+v", draft)
	}
	soul, _ := draft["soul"].(map[string]any)
	if soul["body"] != "Precise and brief." || soul["display_name"] != "Ada" {
		t.Fatalf("soul %+v", soul)
	}
	categories := Categories(checked.Profile.Tools)
	if len(categories) != 2 || categories[1].Category != "write" {
		t.Fatalf("a wildcard is write: %+v", categories)
	}
	long := checked
	long.Profile.DisplayName = strings.Repeat("n", 60)
	if name, _ := Draft(long)["name"].(string); len([]rune(name)) != MaxNameChars {
		t.Fatalf("name not cut to the create bound: %q", name)
	}
}

func TestCapabilities(t *testing.T) {
	if CanPropose(nil) || CanPropose(map[string]any{"capabilities": map[string]any{"can_propose_agents": "true"}}) {
		t.Fatal("only a literal true grants it")
	}
	if !CanPropose(map[string]any{"capabilities": map[string]any{"can_propose_agents": true}}) {
		t.Fatal("true grants it")
	}
	for _, meta := range []map[string]any{nil, {"capabilities": nil}, {"capabilities": map[string]any{"can_propose_agents": false}}} {
		if err := CheckCapabilities(meta); err != nil {
			t.Fatalf("%v: %v", meta, err)
		}
	}
	for _, meta := range []map[string]any{{"capabilities": true}, {"capabilities": map[string]any{"can_propose_agents": "yes"}}} {
		if err := CheckCapabilities(meta); err == nil {
			t.Fatalf("%v accepted", meta)
		}
	}
}
