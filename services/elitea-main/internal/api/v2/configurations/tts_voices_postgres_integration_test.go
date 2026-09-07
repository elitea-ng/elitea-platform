package configurations_test

// The TTS voice listing against a real PostgreSQL (issue 323).
//
// # Why this cannot be a unit test
//
// The handler's job is the reference's three-step resolution
// (legacy/plugins/configurations/api/v2/tts_voices.py's `_resolve_voices`), and
// two of the three steps ARE the database:
//
//  1. `meta.voices` on the project's tts row is the cache, and the defect this
//     work closes is that NOTHING EVER WROTE IT. A handler that believes it
//     wrote the column, and a column that holds the value, are different facts
//     — so every case below re-reads `meta` with SQL.
//  2. A `tts_model` row names no provider dialect. It LINKS one, through
//     `data.ai_credentials`, and the linked row's `type` decides the voice set.
//     That join cannot be exercised without the rows.
//
// The gateway is stubbed. Its round trip is its own test
// (elitea-llm-gateway internal/llmproxy/listprovidervoices_test.go). What is
// asserted here is that this handler ASKS, that it asks with the RESOLVED
// dialect, and what it does with the answer.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
)

const (
	ttsModelTitle      = "project-tts"
	ttsCredentialTitle = "azure-key"
)

type ttsVoicesBody struct {
	Voices    []handler.ProviderVoice `json:"voices"`
	ModelName string                  `json:"model_name"`
}

// newTTSVoicesFixture seeds a `tts_model` row linked to an `azure_open_ai`
// credential — the shape a project really has — and returns the router and the
// pool that reads it back.
func newTTSVoicesFixture(t *testing.T, lister *fakeVoiceLister) (chi.Router, *pgxpool.Pool) {
	t.Helper()
	pool := newGlobalScopePool(t)
	ctx := context.Background()
	schema := fmt.Sprintf("p_%d", globalScopeProject)

	credentialData := `{"api_base":"https://example.invalid","api_key":"{{secret.k}}"}`
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.configuration
			(uuid, project_id, elitea_title, type, section, data, meta, shared, status_ok, source)
		VALUES (gen_random_uuid(), $1, $2, 'azure_open_ai', 'ai_credentials', $3::jsonb, '{}'::jsonb,
			false, true, 'user')`, schema),
		globalScopeProject, ttsCredentialTitle, credentialData); err != nil {
		t.Fatalf("seed credential row: %v", err)
	}
	// `type` is the GENERIC tts_model, exactly as the platform stores it. The
	// dialect is only reachable through the link below.
	modelData := fmt.Sprintf(`{"ai_credentials":{"elitea_title":%q}}`, ttsCredentialTitle)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.configuration
			(uuid, project_id, elitea_title, type, section, data, meta, shared, status_ok, source)
		VALUES (gen_random_uuid(), $1, $2, 'tts_model', 'tts', $3::jsonb, '{"note":"kept"}'::jsonb,
			false, true, 'user')`, schema),
		globalScopeProject, ttsModelTitle, modelData); err != nil {
		t.Fatalf("seed tts model row: %v", err)
	}

	options := []handler.Option{handler.WithPermissionResolver(entitledResolver())}
	if lister != nil {
		options = append(options, handler.WithConnectionChecker(lister))
	}
	h := handler.NewHandler(pool, options...)
	router := chi.NewRouter()
	router.Use(withTestUser)
	router.Mount("/api/v2", h.Routes())
	return router, pool
}

func getTTSVoices(t *testing.T, router chi.Router, query string) ttsVoicesBody {
	t.Helper()
	target := fmt.Sprintf("/api/v2/tts_voices/%d?%s", globalScopeProject, query)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body: %s", target, rec.Code, rec.Body.String())
	}
	var body ttsVoicesBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", target, err)
	}
	return body
}

// storedMeta reads the row's meta column with SQL. Reading it back through the
// handler would let a handler that caches in memory pass.
func storedMeta(t *testing.T, pool *pgxpool.Pool) map[string]any {
	t.Helper()
	var raw []byte
	query := fmt.Sprintf(
		`SELECT meta FROM p_%d.configuration WHERE elitea_title = $1 AND section = 'tts'`,
		globalScopeProject)
	if err := pool.QueryRow(context.Background(), query, ttsModelTitle).Scan(&raw); err != nil {
		t.Fatalf("read meta: %v", err)
	}
	meta := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("decode meta %q: %v", raw, err)
		}
	}
	return meta
}

func metaVoiceIDs(t *testing.T, meta map[string]any) []string {
	t.Helper()
	raw, ok := meta["voices"].([]any)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(raw))
	for _, entry := range raw {
		item, isObject := entry.(map[string]any)
		if !isObject {
			t.Fatalf("meta.voices holds a non-object entry: %v", entry)
		}
		id, _ := item["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// TestTTSVoicesFillsTheMetaCache is THE acceptance for issue 323's remaining
// half: "nothing fills meta.voices".
func TestTTSVoicesFillsTheMetaCache(t *testing.T) {
	lister := &fakeVoiceLister{voices: []handler.ProviderVoice{
		{ID: "alloy", Name: "Alloy"},
		{ID: "shimmer", Name: "Shimmer"},
	}}
	router, pool := newTTSVoicesFixture(t, lister)

	// Precondition: the cache is empty, so the answer below cannot come from it.
	if ids := metaVoiceIDs(t, storedMeta(t, pool)); len(ids) != 0 {
		t.Fatalf("the fixture already holds cached voices: %v", ids)
	}

	body := getTTSVoices(t, router, "model_name="+ttsModelTitle)
	if len(body.Voices) != 2 || body.Voices[0].ID != "alloy" {
		t.Fatalf("voices = %+v, want the provider's two in its own order", body.Voices)
	}
	if body.ModelName != ttsModelTitle {
		t.Errorf("model_name = %q, want %q", body.ModelName, ttsModelTitle)
	}

	// The DIALECT reached the gateway, not the generic `tts_model` type. A
	// handler that skipped the credential link would have asked for
	// "tts_model", and the gateway would have answered an empty list.
	if len(lister.calls) != 1 || lister.calls[0] != "azure_open_ai/"+ttsModelTitle {
		t.Fatalf("gateway calls = %v, want one for the linked dialect", lister.calls)
	}

	// THE assertion: the cache the reference reads is written, in SQL.
	meta := storedMeta(t, pool)
	if ids := metaVoiceIDs(t, meta); len(ids) != 2 || ids[0] != "alloy" {
		t.Fatalf("meta.voices = %v, want the two voices in provider order", ids)
	}
	// …and the write MERGED, so bookkeeping another path stored survives.
	if note, _ := meta["note"].(string); note != "kept" {
		t.Errorf("the cache write replaced the meta object instead of merging: %v", meta)
	}
}

// TestTTSVoicesServesTheCacheWithoutAskingAgain — step two of the reference's
// order. A handler that asked every time would work, and would make a provider
// round trip on every render of a picker.
func TestTTSVoicesServesTheCacheWithoutAskingAgain(t *testing.T) {
	lister := &fakeVoiceLister{voices: []handler.ProviderVoice{{ID: "alloy", Name: "Alloy"}}}
	router, _ := newTTSVoicesFixture(t, lister)

	getTTSVoices(t, router, "model_name="+ttsModelTitle)
	second := getTTSVoices(t, router, "model_name="+ttsModelTitle)

	if len(second.Voices) != 1 || second.Voices[0].ID != "alloy" {
		t.Fatalf("the second read returned %+v, want the cached voice", second.Voices)
	}
	if len(lister.calls) != 1 {
		t.Fatalf("the gateway was asked %d times, want once — the second read must come from the cache",
			len(lister.calls))
	}
}

// TestTTSVoicesRefreshAsksTheProviderAgain — step one. `refresh=true` is how an
// operator picks up voices a provider added after the cache was written.
func TestTTSVoicesRefreshAsksTheProviderAgain(t *testing.T) {
	lister := &fakeVoiceLister{voices: []handler.ProviderVoice{{ID: "alloy", Name: "Alloy"}}}
	router, pool := newTTSVoicesFixture(t, lister)

	getTTSVoices(t, router, "model_name="+ttsModelTitle)
	lister.voices = []handler.ProviderVoice{
		{ID: "alloy", Name: "Alloy"},
		{ID: "verse", Name: "Verse"},
	}

	refreshed := getTTSVoices(t, router, "model_name="+ttsModelTitle+"&refresh=true")
	if len(refreshed.Voices) != 2 {
		t.Fatalf("refresh returned %+v, want the provider's new pair", refreshed.Voices)
	}
	if len(lister.calls) != 2 {
		t.Fatalf("the gateway was asked %d times, want twice", len(lister.calls))
	}
	// The refreshed answer REPLACES the cache; a stale cache would go on
	// hiding the new voice from every later read.
	if ids := metaVoiceIDs(t, storedMeta(t, pool)); len(ids) != 2 {
		t.Fatalf("meta.voices = %v after a refresh, want the new pair", ids)
	}
}

// TestTTSVoicesEmptyProviderAnswerIsNotCached — a provider with no catalogue
// leaves the caller on its own default voice, and writes NOTHING. A stored
// empty list would read as a cache hit on every later request and the picker
// would never recover.
func TestTTSVoicesEmptyProviderAnswerIsNotCached(t *testing.T) {
	lister := &fakeVoiceLister{voices: nil}
	router, pool := newTTSVoicesFixture(t, lister)

	body := getTTSVoices(t, router, "model_name="+ttsModelTitle)
	if len(body.Voices) != 0 {
		t.Fatalf("voices = %+v, want none", body.Voices)
	}
	if _, cached := storedMeta(t, pool)["voices"]; cached {
		t.Error("an empty provider answer was cached, so no later read can ever reach the provider")
	}
}

// TestTTSVoicesGatewayFailureIsNotAnEmptyList — the direction that matters for
// an operator. "The gateway did not answer" and "this model has no voices" are
// different states, and reporting the first as the second sends somebody
// looking for a provider problem that is not there.
func TestTTSVoicesGatewayFailureIsNotAnEmptyList(t *testing.T) {
	lister := &fakeVoiceLister{err: fmt.Errorf("gateway unreachable")}
	router, _ := newTTSVoicesFixture(t, lister)

	target := fmt.Sprintf("/api/v2/tts_voices/%d?model_name=%s", globalScopeProject, ttsModelTitle)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("a failed gateway answered 200; body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", rec.Code, rec.Body.String())
	}
}

// TestTTSVoicesUnknownModelIsAnEmptyAnswer — the picker asks with whatever
// model is selected, and a stale selection is a normal state, not a 404.
func TestTTSVoicesUnknownModelIsAnEmptyAnswer(t *testing.T) {
	lister := &fakeVoiceLister{voices: []handler.ProviderVoice{{ID: "alloy", Name: "Alloy"}}}
	router, _ := newTTSVoicesFixture(t, lister)

	body := getTTSVoices(t, router, "model_name=no-such-model")
	if len(body.Voices) != 0 {
		t.Fatalf("an unknown model returned %+v", body.Voices)
	}
	if len(lister.calls) != 0 {
		t.Fatalf("the gateway was asked about a model this project does not have: %v", lister.calls)
	}
}
