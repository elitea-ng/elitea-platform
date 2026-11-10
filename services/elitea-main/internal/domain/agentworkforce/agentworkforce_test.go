package agentworkforce

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/proactivity"
)

func TestCheckProposal(t *testing.T) {
	if _, err := CheckProposal(ProposalInput{Stage: "fire", Rationale: "x"}); err == nil {
		t.Fatal("an unknown stage passed")
	}
	if _, err := CheckProposal(ProposalInput{Stage: StageRetire, Rationale: "  "}); err == nil {
		t.Fatal("an empty rationale passed")
	}
	if _, err := CheckProposal(ProposalInput{Stage: StageRetire, Rationale: strings.Repeat("a", MaxRationaleBytes+1)}); err == nil {
		t.Fatal("a long rationale passed")
	}
	if _, err := CheckProposal(ProposalInput{Stage: StageDemote, Rationale: "x"}); err == nil {
		t.Fatal("a demotion that takes nothing passed")
	}
	got, err := CheckProposal(ProposalInput{Stage: StageRetire, Rationale: " ok ", Narrowing: Narrowing{RemoveLead: true}})
	if err != nil || got.Rationale != "ok" || !got.Narrowing.Empty() {
		t.Fatalf("retire = %+v %v", got, err)
	}
	demote, err := CheckProposal(ProposalInput{Stage: StageDemote, Rationale: "x", Narrowing: Narrowing{
		RemoveToolkitIDs: []int64{3, 1, 3}, RemoveCapabilities: []string{"can_review_agents", " can_review_agents"},
		ToolkitTypeCeiling: []string{"GitHub", "github"},
	}})
	if err != nil || len(demote.Narrowing.RemoveToolkitIDs) != 2 || len(demote.Narrowing.RemoveCapabilities) != 1 ||
		len(demote.Narrowing.ToolkitTypeCeiling) != 1 || demote.Narrowing.ToolkitTypeCeiling[0] != "github" {
		t.Fatalf("demote = %+v %v", demote, err)
	}
	if _, err := CheckNarrowing(Narrowing{RemoveCapabilities: []string{"can_fly"}}); err == nil {
		t.Fatal("an unknown capability passed")
	}
	if _, err := CheckNarrowing(Narrowing{RemoveToolkitIDs: []int64{-1}}); err == nil {
		t.Fatal("a negative toolkit id passed")
	}
}

func TestNarrowMetaOnlyTakesAway(t *testing.T) {
	meta := map[string]any{
		"capabilities": map[string]any{"can_propose_agents": true, "can_review_agents": true},
		"memory":       map[string]any{"enabled": true, "write": "auto"},
		"proactivity":  map[string]any{"mode": "heartbeat", "interval": "30m"},
		"other":        1,
	}
	out := NarrowMeta(meta, Narrowing{RemoveCapabilities: []string{ReviewCapability}, MemoryReview: true, ProactivityOff: true})
	capabilities := out["capabilities"].(map[string]any)
	if _, ok := capabilities[ReviewCapability]; ok || capabilities[ProposeCapability] != true {
		t.Fatalf("capabilities = %v", capabilities)
	}
	memory, err := agentmemory.Parse(out["memory"])
	if err != nil || !memory.Enabled || memory.Write != agentmemory.WriteReview {
		t.Fatalf("memory = %+v %v", memory, err)
	}
	// The heartbeat block keeps its other keys and parses off.
	if block, err := proactivity.Parse(out["proactivity"]); err != nil || block.Enabled() || block.Interval.Minutes() != 30 {
		t.Fatalf("narrowed heartbeat block = %+v %v", block, err)
	}
	if out["other"] != 1 {
		t.Fatalf("out = %v", out)
	}
	if meta["memory"].(map[string]any)["write"] != "auto" || meta["proactivity"].(map[string]any)["mode"] != "heartbeat" {
		t.Fatal("the input was modified")
	}
}

// A narrowing only takes away: an agent with memory off (no block, or
// enabled false) or no heartbeat must not gain one from a demotion.
func TestNarrowMetaNeverSwitchesMemoryOrTheHeartbeatOn(t *testing.T) {
	narrowing := Narrowing{MemoryReview: true, ProactivityOff: true}
	for _, meta := range []map[string]any{
		{},
		{"memory": map[string]any{"enabled": false}},
		{"memory": map[string]any{"write": "off"}},
	} {
		out := NarrowMeta(meta, narrowing)
		memory, err := agentmemory.Parse(out["memory"])
		if err != nil {
			t.Fatalf("%v: %v", meta, err)
		}
		before, _ := agentmemory.Parse(meta["memory"])
		if memory != before {
			t.Fatalf("%v: memory became %+v", meta, memory)
		}
		if _, ok := out["proactivity"]; ok {
			t.Fatalf("%v: a proactivity block appeared: %v", meta, out["proactivity"])
		}
	}
}

func TestCheckAgentProposalGuards(t *testing.T) {
	ok := GuardSubject{ProposerApplicationID: 1, ProposerTier: "high", ProposerIsHR: true,
		TargetApplicationID: 2, TargetTier: "medium", TargetState: StateActive}
	if err := CheckAgentProposal(ok, StageRetire); err != nil {
		t.Fatalf("a valid proposal was refused: %v", err)
	}
	cases := map[string]struct {
		edit  func(*GuardSubject)
		stage Stage
		code  string
	}{
		"not HR":      {func(s *GuardSubject) { s.ProposerIsHR = false }, StageWatch, OutcomeNotAllowed},
		"low HR":      {func(s *GuardSubject) { s.ProposerTier = "low" }, StageWatch, OutcomeNotAllowed},
		"self":        {func(s *GuardSubject) { s.TargetApplicationID = 1 }, StageWatch, OutcomeGuardRefused},
		"hirer":       {func(s *GuardSubject) { s.TargetHiredProposer = true }, StageWatch, OutcomeGuardRefused},
		"other HR":    {func(s *GuardSubject) { s.TargetIsHR = true }, StageWatch, OutcomeGuardRefused},
		"higher tier": {func(s *GuardSubject) { s.TargetTier = "critical"; s.ProposerTier = "high" }, StageWatch, OutcomeGuardRefused},
		"critical":    {func(s *GuardSubject) { s.TargetTier = "critical"; s.ProposerTier = "critical" }, StageWatch, OutcomeGuardRefused},
		"legal hold":  {func(s *GuardSubject) { s.TargetLegalHold = true }, StageWatch, OutcomeGuardRefused},
		"retired":     {func(s *GuardSubject) { s.TargetState = StateRetired }, StageWatch, OutcomeGuardRefused},
		"delete":      {func(*GuardSubject) {}, StageDelete, OutcomeGuardRefused},
	}
	for name, c := range cases {
		subject := ok
		c.edit(&subject)
		refusal, found := AsRefusal(CheckAgentProposal(subject, c.stage))
		if !found || refusal.Code != c.code {
			t.Errorf("%s: %v, want %s", name, refusal, c.code)
		}
	}
	if !ExemptFromAutomatic("critical", false) || !ExemptFromAutomatic("low", true) || ExemptFromAutomatic("high", false) {
		t.Fatal("ExemptFromAutomatic")
	}
}

func TestRetiredErrorRefusal(t *testing.T) {
	status, body, ok := HTTPRefusal(&RetiredError{ApplicationID: 5, Nested: true})
	if !ok || status != http.StatusConflict || body["code"] != NestedAgentRetiredCode || body["application_id"] != int64(5) {
		t.Fatalf("nested = %d %v %v", status, body, ok)
	}
	_, body, _ = HTTPRefusal(&RetiredError{ApplicationID: 5})
	if body["code"] != AgentRetiredCode {
		t.Fatalf("own = %v", body)
	}
	if _, _, ok := HTTPRefusal(Refuse(InvalidCode, "x")); ok {
		t.Fatal("another error mapped")
	}
	if Refuse(GuardCode, "x").Status() != http.StatusForbidden || Refuse("unknown", "x").Status() != http.StatusConflict {
		t.Fatal("status map")
	}
}

func TestScorecardFinishAndDigest(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	last := now.Add(-72 * time.Hour)
	card := Scorecard{Runs30d: 10, Failed30d: 4, CapReached30d: 2, LastUsedAt: &last}
	card.Finish(now)
	if card.FailureRate30d != 0.5 || card.UnusedDays == nil || *card.UnusedDays != 3 {
		t.Fatalf("card = %+v", card)
	}
	empty := Scorecard{}
	empty.Finish(now)
	if empty.FailureRate30d != 0 || empty.UnusedDays != nil {
		t.Fatalf("empty = %+v", empty)
	}
	a := EvidenceDigest(map[string]any{"b": 1, "a": 2})
	b := EvidenceDigest(map[string]any{"a": 2, "b": 1})
	if a != b || len(a) != 64 {
		t.Fatalf("digest %s %s", a, b)
	}
}

func TestHasCapability(t *testing.T) {
	if !IsHRAgent(map[string]any{"capabilities": map[string]any{"can_review_agents": true}}) {
		t.Fatal("HR")
	}
	if IsHRAgent(map[string]any{"capabilities": map[string]any{"can_review_agents": "true"}}) || IsHRAgent(nil) {
		t.Fatal("a non-boolean counted")
	}
}
