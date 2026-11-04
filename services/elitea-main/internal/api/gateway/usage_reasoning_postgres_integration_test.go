package gateway_test

// ADR-0038 decision 5 on the platform usage report: every section reads the
// ledger's reasoning_tokens (shared migration 0225) against the real schema,
// apart from completion_tokens.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/gateway"
	dbschema "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/schema"
)

func TestPlatformUsageReportsReasoningTokensApart(t *testing.T) {
	pool := newAlertPool(t)
	ctx := context.Background()
	// The project and member breakdowns LEFT JOIN these two tables.
	for _, projection := range []string{
		dbschema.CentryProjectsBaselineSQLCProjection,
		dbschema.AuthCoreBaselineSQLCProjection,
	} {
		if _, err := pool.Exec(ctx, projection); err != nil {
			t.Fatalf("apply schema projection: %v", err)
		}
	}
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		id        string
		reasoning int64
	}{
		{"55555555-5555-5555-5555-555555555551", 600},
		{"55555555-5555-5555-5555-555555555552", 0},
	} {
		if _, err := pool.Exec(ctx, `
INSERT INTO gateway.llm_usage_events
    (event_id, project_id, user_id, provider, model,
     prompt_tokens, completion_tokens, reasoning_tokens, cost_usd,
     period_start, period_end, occurred_at)
VALUES ($1, 42, 7, 'openai', 'gpt-5.1', 100, 800, $2, 0.5, $3, $4, $5)`,
			row.id, row.reasoning, start, start.AddDate(0, 1, 0), now.Add(-time.Minute)); err != nil {
			t.Fatalf("plant usage event: %v", err)
		}
	}

	recorder := httptest.NewRecorder()
	gateway.NewLLMProxyHandler(pool, nil).Usage(recorder,
		httptest.NewRequest(http.MethodGet, "/api/v2/admin/gateway/usage", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body)
	}
	type counts struct {
		CompletionTokens int64 `json:"completion_tokens"`
		ReasoningTokens  int64 `json:"reasoning_tokens"`
	}
	var body struct {
		Error    string   `json:"error"`
		Totals   counts   `json:"totals"`
		Daily    []counts `json:"daily"`
		Models   []counts `json:"models"`
		Projects []counts `json:"projects"`
		Members  []counts `json:"members"`
		DailyErr string   `json:"daily_error"`
		ModelErr string   `json:"models_error"`
		ProjErr  string   `json:"projects_error"`
		MembErr  string   `json:"members_error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, failure := range []string{body.Error, body.DailyErr, body.ModelErr, body.ProjErr, body.MembErr} {
		if failure != "" {
			t.Fatalf("a section failed: %s (body %s)", failure, recorder.Body)
		}
	}
	if body.Totals.ReasoningTokens != 600 || body.Totals.CompletionTokens != 1600 {
		t.Errorf("totals = %+v, want reasoning 600 apart from completion 1600", body.Totals)
	}
	for name, section := range map[string][]counts{
		"daily": body.Daily, "models": body.Models, "projects": body.Projects, "members": body.Members,
	} {
		if len(section) != 1 || section[0].ReasoningTokens != 600 {
			t.Errorf("%s = %+v, want one row with reasoning 600", name, section)
		}
	}
}
