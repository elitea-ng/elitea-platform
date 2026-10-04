package analytics_test

// Legacy issues 6879 and 6764, against the REAL gateway schema.
//
// 6879: the analytics pages count only MODEL CALLS. The request log also holds
// the model listing, the token counter, the connection check and unmatched
// paths. Those rows must not reach any figure, and an embedding call must,
// because the Usage page counts it.
//
// 6764: users with the same call count come back in a documented order —
// tokens, then display name, then id — and not in user-id order.

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// plantRoute writes one request-log row on a chosen gateway route.
func plantRoute(
	t *testing.T, pool *pgxpool.Pool,
	projectID, userID int, at time.Time, route, model string, promptTokens, completionTokens int64,
) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO gateway.llm_request_logs
    (project_id, user_id, occurred_at, route, method, status,
     duration_ms, provider, model, streaming, prompt_tokens, completion_tokens)
VALUES ($1, $2, $3, $4, 'POST', 200, 50, 'openai', $5, false, $6, $7)`,
		projectID, userID, at, route, model, promptTokens, completionTokens)
	if err != nil {
		t.Fatalf("plant %s row: %v", route, err)
	}
}

// plantNonInferenceTraffic writes one row on every route that is NOT a model
// call, each with a token count no assertion below could absorb unnoticed.
func plantNonInferenceTraffic(t *testing.T, pool *pgxpool.Pool, projectID int, at time.Time) {
	t.Helper()
	for _, route := range []string{
		"/llm/v1/models",
		"/llm/v1/models/*",
		"/llm/v1/messages/count_tokens",
		"/llm/v1/check_connection",
		"/llm/v1/list_provider_models",
		"(unmatched)",
	} {
		plantRoute(t, pool, projectID, 7, at, route, "gpt-4o", 10_000, 10_000)
	}
}

func TestUsageCountsOnlyInferenceRoutes(t *testing.T) {
	pool, router := newUsageEnvironment(t)
	at := usageNow.Add(-2 * time.Hour)

	plantRoute(t, pool, usageProjectID, 7, at, "/llm/v1/chat/completions", "gpt-4o", 100, 20)
	plantRoute(t, pool, usageProjectID, 8, at, "/llm/v1/embeddings", "text-embedding-3-small", 30, 0)
	plantNonInferenceTraffic(t, pool, usageProjectID, at)

	status, body := usageGet(t, router, fmt.Sprintf("/analytics/prompt_lib/%d", usageProjectID))
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, body)
	}
	kpis, _ := body["kpis"].(map[string]any)
	// The chat call and the embedding call, and nothing else.
	wantNumber(t, kpis, "llm_calls", "2")
	wantNumber(t, kpis, "total_tokens", "150")
	wantNumber(t, kpis, "ai_active_users", "2")

	models, _ := body["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models = %#v, want the chat model and the embedding model", body["models"])
	}
	// The Health tab counts every request on every route: the two model calls
	// and the six others. It is not a call figure.
	health := healthBlock(t, body)
	wantNumber(t, health, "requests", "8")
}

func TestUserListCountsOnlyInferenceRoutes(t *testing.T) {
	pool, router := newUsageEnvironment(t)
	at := usageNow.Add(-2 * time.Hour)

	plantRoute(t, pool, usageProjectID, 7, at, "/llm/v1/embeddings", "text-embedding-3-small", 5, 0)
	// User 9 made only non-model requests and is not an AI user here.
	for _, route := range []string{"/llm/v1/models", "/llm/v1/check_connection"} {
		plantRoute(t, pool, usageProjectID, 9, at, route, "gpt-4o", 1, 1)
	}

	_, body := usageGet(t, router, fmt.Sprintf("/analytics_users/prompt_lib/%d", usageProjectID))
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %#v, want only the user with an embedding call", body["items"])
	}
	if row, _ := items[0].(map[string]any); row["user_id"] != "7" {
		t.Fatalf("user = %v, want 7", items[0])
	}
}

func TestCostsEstimateCountsOnlyInferenceRoutes(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	from, to := estimateWindow()
	at := estimateNow.Add(-time.Hour)

	plantRoute(t, pool, costProjectID, 7, at, "/llm/v1/chat/completions", "gpt-4o", 100, 20)
	plantRoute(t, pool, costProjectID, 7, at, "/llm/v1/embeddings", "text-embedding-3-small", 30, 0)
	plantNonInferenceTraffic(t, pool, costProjectID, at)

	estimate := estimateBlock(t, decodeCosts(t, costsDo(t, router, costsTarget(from, to))))
	totals, _ := estimate["totals"].(map[string]any)
	wantCostNumber(t, totals, "calls", "2")
	wantCostNumber(t, totals, "total_tokens", "150")
}

// Three users with the same call count. The order must be tokens descending,
// then name ascending — not user id, which is on no column of the table.
func TestUserListBreaksTiesByTokensThenName(t *testing.T) {
	pool, router := newUsageEnvironment(t)
	plantMembership(t, pool, usageProjectID, map[int][]string{20: {"viewer"}, 30: {"viewer"}, 40: {"viewer"}})
	for id, name := range map[int]string{20: "Bob", 30: "Alice", 40: "Carol"} {
		if _, err := pool.Exec(context.Background(),
			`UPDATE public.auth_core__user SET name = $2 WHERE id = $1`, id, name); err != nil {
			t.Fatalf("name user %d: %v", id, err)
		}
	}
	at := usageNow.Add(-3 * time.Hour)
	// One call each. Carol (40) used more tokens; Alice (30) and Bob (20) tie.
	plantRoute(t, pool, usageProjectID, 20, at, "/llm/v1/chat/completions", "gpt-4o", 10, 5)
	plantRoute(t, pool, usageProjectID, 30, at, "/llm/v1/chat/completions", "gpt-4o", 10, 5)
	plantRoute(t, pool, usageProjectID, 40, at, "/llm/v1/chat/completions", "gpt-4o", 100, 5)

	_, body := usageGet(t, router, fmt.Sprintf("/analytics_users/prompt_lib/%d", usageProjectID))
	items, _ := body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %#v, want 3 users", body["items"])
	}
	want := []string{"40", "30", "20"} // Carol (tokens), Alice, Bob (name)
	for index, id := range want {
		row, _ := items[index].(map[string]any)
		if row["user_id"] != id {
			t.Fatalf("row %d = %v, want user %s; order must be calls, tokens, name, id", index, row, id)
		}
	}
}

// plantRouteStatus writes one request-log row on a chosen route with a chosen
// status. The row carries no tokens, as the gateway writes a refusal.
func plantRouteStatus(
	t *testing.T, pool *pgxpool.Pool,
	projectID, userID int, at time.Time, route string, status int, errorCode string,
) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO gateway.llm_request_logs
    (project_id, user_id, occurred_at, route, method, status,
     duration_ms, provider, model, streaming, error_code, prompt_tokens, completion_tokens)
VALUES ($1, $2, $3, $4, 'POST', $5, 10, 'openai', 'gpt-4o', false, $6, 0, 0)`,
		projectID, userID, at, route, status, errorCode)
	if err != nil {
		t.Fatalf("plant %s %d row: %v", route, status, err)
	}
}

// The call figures count COMPLETED calls only. The Usage page reads the billing
// ledger, and a refused or failed call never reaches it, so counting one here
// opens the mismatch of issue 6879 again. User 9 only hit the budget limit and
// is not an active user. The Health tab still counts every failure.
func TestUsageCountsCompletedCallsAndHealthCountsTheRefusals(t *testing.T) {
	pool, router := newUsageEnvironment(t)
	plantMembership(t, pool, usageProjectID, map[int][]string{7: {"viewer"}, 9: {"viewer"}})
	at := usageNow.Add(-2 * time.Hour)

	plantRoute(t, pool, usageProjectID, 7, at, "/llm/v1/chat/completions", "gpt-4o", 100, 20)
	plantRouteStatus(t, pool, usageProjectID, 9, at, "/llm/v1/chat/completions", 402, "budget_exceeded")
	plantRouteStatus(t, pool, usageProjectID, 9, at, "/llm/v1/chat/completions", 429, "rate_limited")
	plantRouteStatus(t, pool, usageProjectID, 9, at, "/llm/v1/embeddings", 502, "upstream_error")

	status, body := usageGet(t, router, fmt.Sprintf("/analytics/prompt_lib/%d", usageProjectID))
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, body)
	}
	kpis, _ := body["kpis"].(map[string]any)
	wantNumber(t, kpis, "llm_calls", "1")
	wantNumber(t, kpis, "ai_active_users", "1")
	wantNumber(t, kpis, "active_project_members", "1")

	models, _ := body["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models = %#v, want one row", body["models"])
	}
	model, _ := models[0].(map[string]any)
	wantNumber(t, model, "run_count", "1")

	daily, _ := body["daily_activity"].([]any)
	if len(daily) != 1 {
		t.Fatalf("daily_activity = %#v, want one day", body["daily_activity"])
	}
	day, _ := daily[0].(map[string]any)
	wantNumber(t, day, "llm_calls", "1")

	health := healthBlock(t, body)
	wantNumber(t, health, "requests", "4")
	wantNumber(t, health, "errors", "3")

	_, users := usageGet(t, router, fmt.Sprintf("/analytics_users/prompt_lib/%d", usageProjectID))
	items, _ := users["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("users = %#v, want only user 7, who completed a call", users["items"])
	}
}

// The Health tab is not a call figure. A failure on a non-inference route is a
// real fault, so the route filter of issue 6879 must not hide it there.
func TestHealthStillReportsFailuresOnNonInferenceRoutes(t *testing.T) {
	pool, router := newUsageEnvironment(t)
	at := usageNow.Add(-time.Hour)

	plantRoute(t, pool, usageProjectID, 7, at, "/llm/v1/chat/completions", "gpt-4o", 10, 5)
	plantRouteStatus(t, pool, usageProjectID, 7, at, "(unmatched)", 404, "not_found")
	plantRouteStatus(t, pool, usageProjectID, 7, at, "/llm/v1/models", 500, "upstream_error")
	plantRouteStatus(t, pool, usageProjectID, 7, at, "/llm/v1/messages/count_tokens", 502, "upstream_error")

	_, body := usageGet(t, router, fmt.Sprintf("/analytics/prompt_lib/%d", usageProjectID))
	kpis, _ := body["kpis"].(map[string]any)
	wantNumber(t, kpis, "llm_calls", "1")

	health := healthBlock(t, body)
	wantNumber(t, health, "requests", "4")
	wantNumber(t, health, "errors", "3")
	codes, _ := health["by_error_code"].([]any)
	if len(codes) != 2 {
		t.Fatalf("by_error_code = %#v, want not_found and upstream_error", health["by_error_code"])
	}
	daily, _ := health["daily"].([]any)
	if len(daily) != 1 {
		t.Fatalf("health.daily = %#v, want one day", health["daily"])
	}
	day, _ := daily[0].(map[string]any)
	wantNumber(t, day, "errors", "3")
}

// The cost estimate counts completed calls, like the Overview tab beside it.
func TestCostsEstimateCountsCompletedCallsOnly(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	from, to := estimateWindow()
	at := estimateNow.Add(-time.Hour)

	plantRoute(t, pool, costProjectID, 7, at, "/llm/v1/chat/completions", "gpt-4o", 100, 20)
	plantRouteStatus(t, pool, costProjectID, 7, at, "/llm/v1/chat/completions", 402, "budget_exceeded")
	plantRouteStatus(t, pool, costProjectID, 7, at, "/llm/v1/chat/completions", 500, "upstream_error")

	estimate := estimateBlock(t, decodeCosts(t, costsDo(t, router, costsTarget(from, to))))
	totals, _ := estimate["totals"].(map[string]any)
	wantCostNumber(t, totals, "calls", "1")
}
