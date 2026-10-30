package policy

// guardrail.go — the `guardrail` row type (ADR-0043 decision 1, issue #1500).
//
// A guardrail row is a content control on the /llm path. Its scope selects
// requests like any other row: platform (no scope), project, model (provider
// and model) or agent (agent_ids of one project). Unlike the other types, the
// narrowest row does not win: EVERY row that selects a request runs. A
// platform row that masks secrets must still run for a project that adds its
// own classifier, and a narrower row must not lift a wider one.
//
// The rows run from the widest to the narrowest scope, then by name, so every
// replica runs them in the same order.

import (
	"sort"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/guardrail"
)

// TypeGuardrail is the guardrail row type (shared migration 0214 adds it to
// the type check of gateway.governance_config).
const TypeGuardrail = "guardrail"

// GuardrailDef is one compiled guardrail row.
type GuardrailDef struct {
	Name  string
	Scope Scope
	Row   *guardrail.Row
}

// Traffic kinds a guardrail selection distinguishes.
const (
	// TrafficGeneration is chat, Responses, Messages and text completions.
	TrafficGeneration = "generation"
	// TrafficEmbedding is embeddings, which is how indexing reaches the
	// gateway. Output rows never apply to it; input rows apply only with
	// apply_to_embeddings.
	TrafficEmbedding = "embedding"
)

// Guardrails returns the rows of stage that select sub, in run order.
func (s *Snapshot) Guardrails(sub Subject, stage, traffic string) []*guardrail.Row {
	if s == nil || len(s.guardrails) == 0 {
		return nil
	}
	var out []*guardrail.Row
	for _, d := range s.guardrails {
		if d.Row.Stage != stage || !d.Scope.selects(sub) {
			continue
		}
		if traffic == TrafficEmbedding && (stage != guardrail.StageInput || !d.Row.ApplyToEmbeddings) {
			continue
		}
		out = append(out, d.Row)
	}
	return out
}

// HasGuardrails reports whether any guardrail row is loaded, so a request
// skips the selection when none is.
func (s *Snapshot) HasGuardrails() bool { return s != nil && len(s.guardrails) > 0 }

// HasAgentGuardrails reports whether a guardrail row is scoped to agents. The
// gateway resolves a request's agent for guardrails only when one is.
func (s *Snapshot) HasAgentGuardrails() bool {
	if s == nil {
		return false
	}
	for _, d := range s.guardrails {
		if len(d.Scope.AgentIDs) > 0 {
			return true
		}
	}
	return false
}

func sortGuardrails(defs []GuardrailDef) {
	sort.SliceStable(defs, func(i, j int) bool {
		si, sj := defs[i].Scope.specificity(), defs[j].Scope.specificity()
		if si != sj {
			return si < sj
		}
		return defs[i].Name < defs[j].Name
	})
}
