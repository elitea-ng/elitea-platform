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
	health := healthBlock(t, body)
	wantNumber(t, health, "requests", "2")
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
