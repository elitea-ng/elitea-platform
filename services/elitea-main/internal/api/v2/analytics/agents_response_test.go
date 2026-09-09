package analytics

// The Agents response SHAPE.
//
// The flag decides the shape, not just a boolean in the body: with the
// dimension unavailable there is no `items` key at all. An empty list would
// render "0 agent runs" for a window that predates migration 0100 — a
// measurement nobody made, served with a 200, beside a live llm_calls tile. The
// client cannot tell that apart from a real zero, and neither could anyone
// reading the response.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/analytics"
)

type agentBreakdownRepo struct {
	stubRepo
	breakdown domain.AgentBreakdown
}

func (r agentBreakdownRepo) GetAgentAnalytics(_ context.Context, _ domain.QueryParams) (domain.AgentBreakdown, error) {
	return r.breakdown, nil
}

func agentsBody(t *testing.T, repo Repository, target string) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	NewHandler(repo).Agents(recorder, httptest.NewRequest(http.MethodGet, target, nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

// TestAgents_UnavailableOmitsItemsEntirely is the no-zero-fill contract at the
// wire.
func TestAgents_UnavailableOmitsItemsEntirely(t *testing.T) {
	body := agentsBody(t, agentBreakdownRepo{breakdown: domain.AgentBreakdown{
		Available: false, UnattributedCalls: 900,
	}}, "/analytics/agents?project_id=1")

	if available, _ := body["agent_dimension_available"].(bool); available {
		t.Fatal("agent_dimension_available must be false")
	}
	if _, present := body["items"]; present {
		t.Fatalf("items must be ABSENT when the dimension is unavailable; got %#v", body["items"])
	}
	// The window's real traffic is still reported: what is missing is the
	// agent dimension, not the requests.
	if got := body["unattributed_llm_calls"]; got != float64(900) {
		t.Fatalf("unattributed_llm_calls = %v, want 900", got)
	}
	if got := body["attributed_llm_calls"]; got != float64(0) {
		t.Fatalf("attributed_llm_calls = %v, want 0", got)
	}
}

// TestAgents_AvailableCarriesTheItems: once the dimension is live the list is
// present, including when it is genuinely empty — at that point "no row
// resolved to a named agent" is a measured fact and not an absence of data.
func TestAgents_AvailableCarriesTheItems(t *testing.T) {
	body := agentsBody(t, agentBreakdownRepo{breakdown: domain.AgentBreakdown{
		Available:         true,
		AttributedCalls:   6,
		UnattributedCalls: 5,
		Agents: []domain.AgentAnalytics{
			{ApplicationID: "4001", Name: "Research Agent", RunCount: 4, TotalTokens: 120, ErrorRate: 25},
		},
	}}, "/analytics/agents?project_id=1")

	if available, _ := body["agent_dimension_available"].(bool); !available {
		t.Fatal("agent_dimension_available must be true")
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want one row", body["items"])
	}
	first, _ := items[0].(map[string]any)
	if first["name"] != "Research Agent" || first["application_id"] != "4001" {
		t.Fatalf("row = %#v", first)
	}
	if body["attributed_llm_calls"] != float64(6) || body["unattributed_llm_calls"] != float64(5) {
		t.Fatalf("the attribution split is wrong: %#v", body)
	}
}

// TestAgents_PricedRowCarriesMoney_UnpricedRowOmitsIt is the money-shape
// contract at the wire (issue #875), the same "the flag decides the shape"
// rule token_dimension_available's neighbours already follow: a row the
// catalogue cannot price must OMIT its money keys rather than publish a
// fabricated zero next to a priced row's real figure.
func TestAgents_PricedRowCarriesMoney_UnpricedRowOmitsIt(t *testing.T) {
	priced := json.Number("0.001000000000")
	body := agentsBody(t, agentBreakdownRepo{breakdown: domain.AgentBreakdown{
		Available: true,
		Agents: []domain.AgentAnalytics{
			{
				ApplicationID: "4001", Name: "Research Agent", RunCount: 4,
				Priced: true, InputCost: &priced, OutputCost: &priced, TotalCost: &priced,
			},
			{ApplicationID: "4002", Name: "Unpriced Agent", RunCount: 2, Priced: false},
		},
	}}, "/analytics/agents?project_id=1")

	items, ok := body["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %#v, want two rows", body["items"])
	}

	// The generic map decode below reads JSON numbers as float64, which is
	// exactly the round trip json.Number exists to avoid — so this test checks
	// the SHAPE (present, and roughly the right figure), never an exact decimal
	// string. The exactness guarantee itself — the NUMERIC computation never
	// touching a Go float — is pinned at the SQL layer by
	// TestGetAgentAnalytics_PricesEachAgentsCallsAtTheCatalogueRate.
	pricedRow, _ := items[0].(map[string]any)
	if pricedRow["priced"] != true {
		t.Fatalf("priced row: priced = %v, want true", pricedRow["priced"])
	}
	if got, ok := pricedRow["total_cost"].(float64); !ok || got < 0.00099 || got > 0.00101 {
		t.Fatalf("priced row: total_cost = %#v, want ~0.001", pricedRow["total_cost"])
	}

	unpricedRow, _ := items[1].(map[string]any)
	if unpricedRow["priced"] != false {
		t.Fatalf("unpriced row: priced = %v, want false", unpricedRow["priced"])
	}
	for _, key := range []string{"input_cost", "output_cost", "total_cost"} {
		if _, present := unpricedRow[key]; present {
			t.Fatalf("unpriced row must OMIT %q, got %#v", key, unpricedRow[key])
		}
	}
}

// TestAgents_AvailableWithNoRowsStillCarriesAnEmptyList pins the third state
// explicitly, because it is the one that looks like the first.
func TestAgents_AvailableWithNoRowsStillCarriesAnEmptyList(t *testing.T) {
	body := agentsBody(t, agentBreakdownRepo{breakdown: domain.AgentBreakdown{
		Available: true, AttributedCalls: 3, Agents: []domain.AgentAnalytics{},
	}}, "/analytics/agents?project_id=1")

	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items must be present as an empty ARRAY, not null: %#v", body["items"])
	}
	if len(items) != 0 {
		t.Fatalf("items = %#v, want empty", items)
	}
}

// TestAgents_DetailSeparatesTheTwoDimensions.
//
// The detail view's shape is { entity_name, users, tools, daily_usage }, and
// `tools` has no producer in this platform at all. Answering with four empty
// lists would report an agent that did nothing; the two dimensions get separate
// flags so the screen can say which half it cannot show.
func TestAgents_DetailSeparatesTheTwoDimensions(t *testing.T) {
	body := agentsBody(t, agentBreakdownRepo{breakdown: domain.AgentBreakdown{
		Available: true, AttributedCalls: 4,
		Agents: []domain.AgentAnalytics{{ApplicationID: "4001", Name: "Research Agent", RunCount: 4}},
	}}, "/analytics/agents?project_id=1&application_id=4001")

	if available, _ := body["agent_dimension_available"].(bool); !available {
		t.Fatal("the agent half is answerable and must say so")
	}
	if tools, _ := body["tool_dimension_available"].(bool); tools {
		t.Fatal("the tool half has no producer and must not claim to")
	}
	if _, present := body["tools"]; present {
		t.Fatal("an empty tools list would report an agent that used no tools, which nothing measured")
	}
	if body["entity_name"] != "Research Agent" {
		t.Fatalf("entity_name = %v, want the agent's name", body["entity_name"])
	}
}
