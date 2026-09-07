package analytics

// The Tools tab's wire shape (issue 618).
//
// The availability flag decides the SHAPE, not just a boolean on the body: with
// tool_dimension_available false there is NO items key for a client to map
// over, so a window this deployment cannot speak for cannot be rendered as
// "no tool ran". The precedent is agents_response_test.go beside this file, and
// behind it internal/api/v2/budgets/usage_dimensions.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/analytics"
)

type toolBreakdownRepo struct {
	stubRepo
	breakdown domain.ToolBreakdown
	agents    domain.AgentBreakdown
	seen      *domain.QueryParams
}

func (r toolBreakdownRepo) GetToolAnalytics(_ context.Context, params domain.QueryParams) (domain.ToolBreakdown, error) {
	if r.seen != nil {
		*r.seen = params
	}
	return r.breakdown, nil
}

func (r toolBreakdownRepo) GetAgentAnalytics(_ context.Context, _ domain.QueryParams) (domain.AgentBreakdown, error) {
	return r.agents, nil
}

func toolsBody(t *testing.T, repo Repository, target string) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	NewHandler(repo).Tools(recorder, httptest.NewRequest(http.MethodGet, target, nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

// The no-zero-fill contract at the wire. A window that predates shared
// migration 0119 has no tool data, and `items: []` would report a month of
// constant tool use as zero.
func TestTools_UnavailableOmitsItemsEntirely(t *testing.T) {
	body := toolsBody(t, toolBreakdownRepo{breakdown: domain.ToolBreakdown{Available: false}},
		"/analytics/tools?period=month")

	if available, ok := body["tool_dimension_available"].(bool); !ok || available {
		t.Fatalf("tool_dimension_available = %v, want false", body["tool_dimension_available"])
	}
	if _, present := body["items"]; present {
		t.Fatalf("an unavailable dimension must publish NO items key, got %v", body["items"])
	}
	if _, present := body["truncated"]; present {
		t.Fatal("truncated describes a list that was not published")
	}
}

func TestTools_AvailableCarriesTheItems(t *testing.T) {
	body := toolsBody(t, toolBreakdownRepo{breakdown: domain.ToolBreakdown{
		Available: true,
		Tools: []domain.ToolAnalytics{{
			ToolkitID: "7", ToolkitName: "GitHub", ToolName: "list_issues",
			RunCount: 12, ErrorCount: 3, AvgDuration: 480.5, ErrorRate: 25,
		}},
	}}, "/analytics/tools?period=month")

	if available, _ := body["tool_dimension_available"].(bool); !available {
		t.Fatal("tool_dimension_available = false, want true")
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %v", body["items"])
	}
	item, _ := items[0].(map[string]any)
	if item["tool_name"] != "list_issues" || item["toolkit_id"] != "7" ||
		item["toolkit_name"] != "GitHub" || item["run_count"] != float64(12) ||
		item["error_count"] != float64(3) || item["avg_duration_ms"] != 480.5 ||
		item["error_rate"] != float64(25) {
		t.Fatalf("the aggregate the Tools tab renders is incomplete: %v", item)
	}
}

// The third state the issue requires to stay distinguishable: the deployment WAS
// recording and nothing ran. That IS a measurement, so the list is present and
// empty.
func TestTools_AvailableWithNoRowsStillCarriesAnEmptyList(t *testing.T) {
	body := toolsBody(t, toolBreakdownRepo{breakdown: domain.ToolBreakdown{
		Available: true, Tools: []domain.ToolAnalytics{},
	}}, "/analytics/tools?period=month")

	items, present := body["items"]
	if !present {
		t.Fatal("an available dimension with no rows must still publish items")
	}
	if list, ok := items.([]any); !ok || len(list) != 0 {
		t.Fatalf("items = %v, want []", items)
	}
}

// The handler must pass the caller's window through. Without this the tab would
// report a project's whole history under whatever range the user picked.
func TestTools_ForwardsTheWindow(t *testing.T) {
	var seen domain.QueryParams
	toolsBody(t, toolBreakdownRepo{
		breakdown: domain.ToolBreakdown{Available: true},
		seen:      &seen,
	}, "/analytics/tools?start_date=2026-03-01&end_date=2026-03-08&period=custom")

	if seen.Period != "custom" {
		t.Errorf("period = %q, want custom", seen.Period)
	}
	if seen.From.IsZero() || seen.To.IsZero() || !seen.To.After(seen.From) {
		t.Errorf("window = %s .. %s", seen.From, seen.To)
	}
	if seen.From.Format("2006-01-02") != "2026-03-01" {
		t.Errorf("window start = %s, want 2026-03-01", seen.From)
	}
}

// The detail branch is still a stub for its users/agents halves, but the flag
// it publishes is now READ rather than hardcoded, and it answers no kpis block.
func TestTools_DetailReportsTheDimensionAndNoZeros(t *testing.T) {
	body := toolsBody(t, toolBreakdownRepo{breakdown: domain.ToolBreakdown{
		Available: true,
		Tools:     []domain.ToolAnalytics{{ToolkitID: "7", ToolName: "list_issues", RunCount: 2}},
	}}, "/analytics/tools?tool_id=list_issues")

	if available, _ := body["tool_dimension_available"].(bool); !available {
		t.Fatal("the detail branch must report the dimension it can answer")
	}
	if body["entity_name"] != "list_issues" {
		t.Errorf("entity_name = %v, want the tool the caller asked for", body["entity_name"])
	}
	for _, forbidden := range []string{"kpis", "items", "total_cost"} {
		if _, present := body[forbidden]; present {
			t.Errorf("the detail stub published %q", forbidden)
		}
	}
}

// The Agents detail branch used to hardcode tool_dimension_available:false
// because no producer existed. It now reads the real flag, and still decides
// the shape: no tools key for a window it cannot speak for.
func TestAgentsDetail_ReadsTheToolDimensionRatherThanHardcodingIt(t *testing.T) {
	for name, breakdown := range map[string]domain.ToolBreakdown{
		"available":   {Available: true, Tools: []domain.ToolAnalytics{{ToolName: "search", RunCount: 1}}},
		"unavailable": {Available: false},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			NewHandler(toolBreakdownRepo{
				breakdown: breakdown,
				agents:    domain.AgentBreakdown{Available: true, Agents: []domain.AgentAnalytics{{ApplicationID: "5", Name: "Triager"}}},
			}).Agents(recorder, httptest.NewRequest(http.MethodGet, "/analytics/agents?application_id=5", nil))

			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if available, _ := body["tool_dimension_available"].(bool); available != breakdown.Available {
				t.Fatalf("tool_dimension_available = %v, want %v", body["tool_dimension_available"], breakdown.Available)
			}
			_, present := body["tools"]
			if present != breakdown.Available {
				t.Fatalf("tools key present = %v, want %v", present, breakdown.Available)
			}
			if body["entity_name"] != "Triager" {
				t.Errorf("entity_name = %v", body["entity_name"])
			}
		})
	}
}
