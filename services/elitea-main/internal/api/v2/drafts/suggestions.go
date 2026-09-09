package drafts

// Resource suggestions for a generated agent draft (issue #881).
//
// DESIGN CHOICE, RECORDED. Legacy's own description (see the package doc)
// is "read the project's toolkit instances, agents, pipelines and skills and
// OFFER THEM TO THE MODEL as candidates" — i.e. inject a candidate list into
// the LLM prompt and let the model pick. This port does the matching in Go
// instead (a simple lexical-overlap score between the generated draft's own
// text and each candidate's name+description), for two reasons:
//
//  1. NO ID-HALLUCINATION RISK. An LLM asked to name resource ids from a
//     candidate list still sometimes invents one, or echoes a plausible-
//     looking id from its training data rather than the list it was given.
//     A Go-side score can only ever select an id that is ACTUALLY in the
//     candidate list, because it never leaves Go.
//  2. NO EXTRA LLM ROUND TRIP OR PROMPT CHANGE. The existing
//     applicationDraftSystemPrompt explicitly tells the model not to name
//     tools/toolkits/agents ("attaching them is a separate, human step") —
//     changing that contract to inject and parse a second, larger
//     candidate-aware response is a bigger, riskier change than this
//     endpoint's blocking-single-completion shape was built for.
//
// The trade-off is real and disclosed: this is weaker than a semantic
// (embedding or LLM-judged) match — two projects with disjoint vocabulary
// but conceptually similar purpose will not match. It is deliberately simple
// enough to unit test exhaustively and to reason about from the response
// alone.
import (
	"context"
	"regexp"
	"sort"
	"strings"

	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
)

// maxSuggestionsPerCategory caps each of the five suggested_* lists. Legacy
// offered candidates to the model with no stated cap; this port caps because
// a long tail of near-zero-relevance matches is worse than an honestly short
// list — the UI's ResourceSuggestions renders whatever it is given.
const maxSuggestionsPerCategory = 5

// toolkitSuggestionScanLimit and applicationSuggestionScanLimit bound how many
// rows are read from the project before scoring. Both are generous relative to
// a real project's catalogue size; a project with more candidates than this
// gets a candidate list that is merely incomplete, not a failed request.
const (
	toolkitSuggestionScanLimit     = 200
	applicationSuggestionScanLimit = 200
	skillSuggestionScanLimit       = 200
)

// mcpToolkitType is the elitea_tools.type value a "connect one remote MCP"
// toolkit instance is stored with — see internal/api/v2/toolkits/
// mcp_projection.go's remoteMCPToolkitType. A row of this type is a
// suggested_mcp candidate, not a suggested_toolkits one.
const mcpToolkitType = "mcp"

// suggestionCandidate is the common shape every candidate source (toolkits,
// agents, pipelines, skills) is normalised into before scoring.
type suggestionCandidate struct {
	id          string
	name        string
	description string
	toolkitType string // toolkits only
	agentType   string // pipelines only ("pipeline")
}

func (c suggestionCandidate) toResource() SuggestedResource {
	r := SuggestedResource{ID: c.id, Name: c.name}
	if c.description != "" {
		r.Description = c.description
	}
	if c.toolkitType != "" {
		r.Type = c.toolkitType
	}
	if c.agentType != "" {
		r.AgentType = c.agentType
	}
	return r
}

// tokenPattern splits on runs of anything that is not a lowercase letter or
// digit — ASCII only, matching the rest of this package's slugify pattern
// (skillNamePattern), which is an existing, accepted simplification for the
// same class of input (short names/descriptions, not full documents).
var tokenPattern = regexp.MustCompile(`[a-z0-9]+`)

// stopwords are dropped so two candidates don't "match" purely on English
// filler. Deliberately short: this is a relevance signal, not a search
// engine, and a stopword list that is too aggressive risks dropping a
// genuinely distinguishing short word (e.g. a two-letter product name).
var stopwords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "be": {}, "for": {},
	"in": {}, "is": {}, "it": {}, "of": {}, "on": {}, "or": {}, "that": {},
	"the": {}, "this": {}, "to": {}, "with": {}, "you": {}, "your": {},
}

// tokenize lowercases and splits into a deduplicated token set, dropping
// stopwords and single-character tokens (too common to be a useful match
// signal on their own).
func tokenize(text string) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, tok := range tokenPattern.FindAllString(strings.ToLower(text), -1) {
		if len(tok) < 2 {
			continue
		}
		if _, isStop := stopwords[tok]; isStop {
			continue
		}
		tokens[tok] = struct{}{}
	}
	return tokens
}

// overlapScore counts how many tokens of `b` also appear in `a`.
func overlapScore(a, b map[string]struct{}) int {
	score := 0
	for tok := range b {
		if _, ok := a[tok]; ok {
			score++
		}
	}
	return score
}

// scoreCandidates ranks candidates by lexical overlap with `query` (the
// generated draft's own name+description+instructions), and returns the top
// `limit` candidates with a strictly positive score.
//
// A candidate's NAME counts twice — once as part of "name+description", once
// again alone — so a candidate whose name echoes the draft outranks one that
// merely shares a word buried in a longer description. Ties keep the
// caller's original ordering (a stable sort), which for every caller in this
// package is alphabetical-by-name, so a tie between equally-relevant
// candidates reads as an intentional, stable choice rather than query-order
// noise.
func scoreCandidates(query string, candidates []suggestionCandidate, limit int) []suggestionCandidate {
	queryTokens := tokenize(query)
	if len(queryTokens) == 0 || len(candidates) == 0 {
		return nil
	}

	type scored struct {
		candidate suggestionCandidate
		score     int
		index     int
	}
	ranked := make([]scored, 0, len(candidates))
	for i, c := range candidates {
		score := overlapScore(queryTokens, tokenize(c.name+" "+c.description))
		score += overlapScore(queryTokens, tokenize(c.name))
		if score > 0 {
			ranked = append(ranked, scored{candidate: c, score: score, index: i})
		}
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].index < ranked[j].index
	})

	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	result := make([]suggestionCandidate, len(ranked))
	for i, r := range ranked {
		result[i] = r.candidate
	}
	return result
}

// toSuggestedResources converts a scored candidate list, defaulting to an
// empty (never nil) slice so the wire response always carries `[]`, matching
// apps/elitea-web's AgentDraft, which already defaults every suggested_* field
// to `[]` and expects the same shape back.
func toSuggestedResources(candidates []suggestionCandidate) []SuggestedResource {
	resources := make([]SuggestedResource, 0, len(candidates))
	for _, c := range candidates {
		resources = append(resources, c.toResource())
	}
	return resources
}

// stringField reads a string value off a ListToolkits row (`map[string]any`)
// defensively: a missing or wrong-typed key degrades to "", not a panic.
func stringField(row map[string]any, key string) string {
	value, _ := row[key].(string)
	return value
}

// suggestToolkits reads the project's toolkit instances once and splits them
// into suggested_toolkits and suggested_mcp by elitea_tools.type — an MCP
// connection IS a toolkit row (mcp_projection.go's own doc comment: "the
// generic type a user picks to connect one remote MCP"), so one read serves
// both suggestion lists.
func (h *Handler) suggestToolkits(ctx context.Context, projectID, query string) (toolkits, mcp []SuggestedResource) {
	if h.toolkits == nil {
		return []SuggestedResource{}, []SuggestedResource{}
	}
	rows, _, err := h.toolkits.ListToolkits(ctx, projectID, 1, toolkitSuggestionScanLimit)
	if err != nil {
		return []SuggestedResource{}, []SuggestedResource{}
	}

	var toolkitCandidates, mcpCandidates []suggestionCandidate
	for _, row := range rows {
		c := suggestionCandidate{
			id:          stringField(row, "id"),
			name:        stringField(row, "name"),
			description: stringField(row, "description"),
			toolkitType: stringField(row, "type"),
		}
		if c.toolkitType == mcpToolkitType {
			mcpCandidates = append(mcpCandidates, c)
		} else {
			toolkitCandidates = append(toolkitCandidates, c)
		}
	}

	return toSuggestedResources(scoreCandidates(query, toolkitCandidates, maxSuggestionsPerCategory)),
		toSuggestedResources(scoreCandidates(query, mcpCandidates, maxSuggestionsPerCategory))
}

// suggestApplications reads the project's agents or pipelines — the same
// `applications` table, distinguished only by AgentsType ("" for agents,
// "pipeline" for pipelines, matching internal/api/v2/applications/
// handler.go's own ?agents_type= filter).
func (h *Handler) suggestApplications(ctx context.Context, projectID, query string, pipelines bool) []SuggestedResource {
	if h.apps == nil {
		return []SuggestedResource{}
	}
	agentsType := ""
	if pipelines {
		agentsType = "pipeline"
	}
	resp, err := h.apps.List(ctx, applications.ListRequest{
		ProjectID:  projectID,
		AgentsType: agentsType,
		PageSize:   applicationSuggestionScanLimit,
	})
	if err != nil {
		return []SuggestedResource{}
	}

	candidates := make([]suggestionCandidate, 0, len(resp.Rows))
	for _, app := range resp.Rows {
		c := suggestionCandidate{id: app.ID, name: app.Name, description: app.Description}
		if pipelines {
			c.agentType = "pipeline"
		}
		candidates = append(candidates, c)
	}
	return toSuggestedResources(scoreCandidates(query, candidates, maxSuggestionsPerCategory))
}

// suggestSkills reads the project's skills.
func (h *Handler) suggestSkills(ctx context.Context, projectID, query string) []SuggestedResource {
	if h.skills == nil {
		return []SuggestedResource{}
	}
	resp, err := h.skills.List(ctx, projectID, v2skills.ListParams{PageSize: skillSuggestionScanLimit})
	if err != nil {
		return []SuggestedResource{}
	}

	candidates := make([]suggestionCandidate, 0, len(resp.Items))
	for _, skill := range resp.Items {
		candidates = append(candidates, suggestionCandidate{id: skill.ID, name: skill.Name, description: skill.Description})
	}
	return toSuggestedResources(scoreCandidates(query, candidates, maxSuggestionsPerCategory))
}
