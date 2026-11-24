// modelmap_test.go — proves that GET /llm/v1/models and the inference path
// agree on model names (issue #317).
//
// Every assertion here reads what the PROVIDER received, never the HTTP status.
// A 200 proves nothing: before the fix every one of these calls returned 200
// and handed the provider the user-authored elitea_title, which the provider
// does not recognise.
package llmproxy

import (
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// ── the provider-side spy ─────────────────────────────────────────────────────

// dispatched is one (provider, model) pair as the router received it.
type dispatched struct {
	provider string
	model    string
}

// dispatchSpy is an LLMRouter that records the provider and model of every
// dispatched request. It embeds fakeRouter for the canned responses and
// overrides only the recording.
type dispatchSpy struct {
	fakeRouter

	mu    sync.Mutex
	calls []dispatched
}

func (s *dispatchSpy) record(provider schemas.ModelProvider, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, dispatched{provider: string(provider), model: model})
}

// last returns the most recent dispatch and whether any happened.
func (s *dispatchSpy) last() (dispatched, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return dispatched{}, false
	}
	return s.calls[len(s.calls)-1], true
}

func (s *dispatchSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *dispatchSpy) ChatCompletionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	s.record(req.Provider, req.Model)
	return s.fakeRouter.ChatCompletionRequest(ctx, req)
}

func (s *dispatchSpy) ChatCompletionStreamRequest(ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	s.record(req.Provider, req.Model)
	s.captureVK(ctx)
	// A fresh channel per call: one spy serves many subtests, and a channel can
	// only be drained once.
	return newChunkChan(), nil
}

func (s *dispatchSpy) TextCompletionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostTextCompletionRequest) (*schemas.BifrostTextCompletionResponse, *schemas.BifrostError) {
	s.record(req.Provider, req.Model)
	return s.fakeRouter.TextCompletionRequest(ctx, req)
}

func (s *dispatchSpy) EmbeddingRequest(ctx *schemas.BifrostContext, req *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	s.record(req.Provider, req.Model)
	return s.fakeRouter.EmbeddingRequest(ctx, req)
}

func (s *dispatchSpy) ResponsesRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	s.record(req.Provider, req.Model)
	return s.fakeRouter.ResponsesRequest(ctx, req)
}

func (s *dispatchSpy) ResponsesStreamRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	s.record(req.Provider, req.Model)
	s.captureVK(ctx)
	return newChunkChan(), nil
}

func (s *dispatchSpy) CountTokensRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (*schemas.BifrostCountTokensResponse, *schemas.BifrostError) {
	s.record(req.Provider, req.Model)
	return s.fakeRouter.CountTokensRequest(ctx, req)
}

func (s *dispatchSpy) ImageGenerationRequest(ctx *schemas.BifrostContext, req *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	s.record(req.Provider, req.Model)
	return s.fakeRouter.ImageGenerationRequest(ctx, req)
}

func (s *dispatchSpy) SpeechRequest(ctx *schemas.BifrostContext, req *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	s.record(req.Provider, req.Model)
	return s.fakeRouter.SpeechRequest(ctx, req)
}

func (s *dispatchSpy) TranscriptionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	s.record(req.Provider, req.Model)
	return s.fakeRouter.TranscriptionRequest(ctx, req)
}

var _ LLMRouter = (*dispatchSpy)(nil)

// newDispatchSpy returns a spy pre-loaded with a successful response for every
// dialect, so a request that reaches the provider always returns 200.
func newDispatchSpy() *dispatchSpy {
	return &dispatchSpy{fakeRouter: fakeRouter{
		chatResp:  &schemas.BifrostChatResponse{ID: "cmpl-1"},
		textResp:  &schemas.BifrostTextCompletionResponse{ID: "txt-1"},
		embResp:   &schemas.BifrostEmbeddingResponse{},
		respResp:  &schemas.BifrostResponsesResponse{ID: strPtr("resp-1")},
		countResp: &schemas.BifrostCountTokensResponse{},
		imgResp:   &schemas.BifrostImageGenerationResponse{ID: "img-1"},

		speechResp:        &schemas.BifrostSpeechResponse{Audio: []byte("audio-1")},
		transcriptionResp: &schemas.BifrostTranscriptionResponse{Text: "transcript-1"},
	}}
}

// ── the fixture ───────────────────────────────────────────────────────────────

// mapProjectID is the project every request in this file carries.
const mapProjectID = "42"

// modelMapRows are three configured llm_model rows whose advertised
// elitea_title differs from the provider's own data.name — the exact condition
// issue #317 describes. elitea_title is NOT NULL and UNIQUE on the real table,
// so this is the normal shape of a row, not an edge case.
func modelMapRows() []fakeModelRow {
	return []fakeModelRow{
		// A plain wire name behind a user-authored title.
		{title: "Prod GPT", data: []byte(`{"name":"gpt-5.1"}`)},
		// The wire name carries its own provider prefix. The prefix selects the
		// credential, so the mapping must recover it from data.name.
		{title: "Team Claude", data: []byte(`{"name":"anthropic/claude-sonnet-4-5"}`)},
		// The advertised id itself carries a provider prefix, as preflight's
		// StaticLegacyModels does.
		{title: "openai/gpt-4o", data: []byte(`{"name":"gpt-4o-2024-11-20"}`)},
	}
}

// wantDispatch maps each advertised id to the (provider, model) pair the
// provider must receive for it.
func wantDispatch() map[string]dispatched {
	return map[string]dispatched{
		"Prod GPT":      {provider: "", model: "gpt-5.1"},
		"Team Claude":   {provider: "anthropic", model: "claude-sonnet-4-5"},
		"openai/gpt-4o": {provider: "openai", model: "gpt-4o-2024-11-20"},
	}
}

// newMapHandler builds a handler over the given rows with the model resolver
// wired, and returns it with the spy that observes the provider.
func newMapHandler(t *testing.T, rows []fakeModelRow) (http.Handler, *dispatchSpy) {
	t.Helper()
	spy := newDispatchSpy()
	resolver := NewModelResolver(ModelResolverConfig{DB: &fakeModelDB{rows: rows}})
	h := newTestHandler(spy, nil, nil, WithModelResolver(resolver))
	return h.route(), spy
}

// dialect is one /llm surface and a builder for a minimal valid body on it.
type dialect struct {
	name string
	path string
	body func(model string) string
}

// mappedDialects are the /llm surfaces that carry a model and dispatch it.
func mappedDialects() []dialect {
	return []dialect{
		{"chat", "/llm/v1/chat/completions", func(m string) string {
			return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, m)
		}},
		{"chat-stream", "/llm/v1/chat/completions", func(m string) string {
			return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":true}`, m)
		}},
		{"completions", "/llm/v1/completions", func(m string) string {
			return fmt.Sprintf(`{"model":%q,"prompt":"hi"}`, m)
		}},
		{"embeddings", "/llm/v1/embeddings", func(m string) string {
			return fmt.Sprintf(`{"model":%q,"input":"hi"}`, m)
		}},
		{"responses", "/llm/v1/responses", func(m string) string {
			return fmt.Sprintf(`{"model":%q,"input":"hi"}`, m)
		}},
		{"messages", "/llm/v1/messages", func(m string) string {
			return fmt.Sprintf(`{"model":%q,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, m)
		}},
		{"count_tokens", "/llm/v1/messages/count_tokens", func(m string) string {
			return fmt.Sprintf(`{"model":%q,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, m)
		}},
		{"images", "/llm/v1/images/generations", func(m string) string {
			return fmt.Sprintf(`{"model":%q,"prompt":"a cat"}`, m)
		}},
	}
}

// postAs posts body to path carrying the project identity header.
func postAs(t *testing.T, h http.Handler, path, projectID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if projectID != "" {
		req.Header.Set(headerProjectID, projectID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// listedIDs performs GET /llm/v1/models and returns the advertised ids.
func listedIDs(t *testing.T, h http.Handler, projectID string) []string {
	t.Helper()
	rec := getModels(t, h, "/llm/v1/models", projectID)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /llm/v1/models: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var list modelsList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode model list: %v", err)
	}
	ids := make([]string, 0, len(list.Data))
	for _, mo := range list.Data {
		ids = append(ids, mo.ID)
	}
	return ids
}

// ── the acceptance test ───────────────────────────────────────────────────────

// TestModelMap_EveryListedIDDispatchesTheProviderWireName is issue #317's
// acceptance criterion. It lists the models for a project whose elitea_title
// differs from data.name, then calls EVERY listed id on EVERY dialect and
// asserts what the provider received.
//
// The list drives the loop: no id is hard-coded into the request path, so the
// test fails if /llm/v1/models ever advertises something the inference path
// cannot map.
func TestModelMap_EveryListedIDDispatchesTheProviderWireName(t *testing.T) {
	h, spy := newMapHandler(t, modelMapRows())
	want := wantDispatch()

	ids := listedIDs(t, h, mapProjectID)
	if len(ids) != len(want) {
		t.Fatalf("advertised ids = %v, want %d entries", ids, len(want))
	}

	for _, id := range ids {
		expect, ok := want[id]
		if !ok {
			t.Fatalf("advertised id %q has no expected provider model; the list and this test disagree", id)
		}
		for _, d := range mappedDialects() {
			t.Run(d.name+"/"+id, func(t *testing.T) {
				before := spy.count()
				rec := postAs(t, h, d.path, mapProjectID, d.body(id))
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
				}
				if spy.count() != before+1 {
					t.Fatalf("dispatch count = %d, want %d: the provider was not called", spy.count(), before+1)
				}
				got, _ := spy.last()
				if got != expect {
					t.Fatalf("provider received %+v, want %+v (advertised id %q)", got, expect, id)
				}
				// The whole point: the title must never reach the provider.
				if got.model == id && expect.model != id {
					t.Fatalf("provider received the advertised title %q unmapped", id)
				}
			})
		}
	}
}

// TestModelMap_ListDoesNotLeakTheProviderWireName guards the other direction:
// the mapping must stay inside the gateway. modelsOwnedBy states that the real
// provider is never leaked, so the wire name must not appear in the response.
func TestModelMap_ListDoesNotLeakTheProviderWireName(t *testing.T) {
	h, _ := newMapHandler(t, modelMapRows())
	rec := getModels(t, h, "/llm/v1/models", mapProjectID)
	body := rec.Body.String()
	for _, wire := range []string{"gpt-5.1", "claude-sonnet-4-5", "gpt-4o-2024-11-20"} {
		if strings.Contains(body, wire) {
			t.Fatalf("/llm/v1/models leaked the provider wire name %q: %s", wire, body)
		}
	}
}

// TestModelMap_ProviderWireNameIsAlsoAccepted guards the callers that exist
// today. elitea-main and elitea-web both send the row's data.name (the model
// catalog exposes data.name as the model `name`), so the wire name must keep
// working. It names the same row, so it resolves to the same dispatch.
func TestModelMap_ProviderWireNameIsAlsoAccepted(t *testing.T) {
	h, spy := newMapHandler(t, modelMapRows())

	rec := postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"gpt-5.1","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got, ok := spy.last()
	if !ok {
		t.Fatal("the provider was not called")
	}
	if want := (dispatched{provider: "", model: "gpt-5.1"}); got != want {
		t.Fatalf("provider received %+v, want %+v", got, want)
	}
}

// TestModelMap_UnknownModelIs404AndNeverReachesTheProvider covers the third
// acceptance bullet: an id that matches nothing fails at the gateway, with an
// OpenAI-shaped body, and no provider call happens.
func TestModelMap_UnknownModelIs404AndNeverReachesTheProvider(t *testing.T) {
	for _, d := range mappedDialects() {
		t.Run(d.name, func(t *testing.T) {
			h, spy := newMapHandler(t, modelMapRows())
			rec := postAs(t, h, d.path, mapProjectID, d.body("no-such-model"))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
			}
			if spy.count() != 0 {
				got, _ := spy.last()
				t.Fatalf("the provider was called with %+v; an unknown model must not reach it", got)
			}
			var body openAIError
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if body.Error.Type != "invalid_request_error" || body.Error.Code != "model_not_found" {
				t.Fatalf("error body = %+v, want type=invalid_request_error code=model_not_found", body.Error)
			}
			if !strings.Contains(body.Error.Message, "no-such-model") {
				t.Fatalf("error message %q does not name the rejected model", body.Error.Message)
			}
		})
	}
}

// TestModelMap_MissingModelIs404 pins the empty-model case. A body with no
// model names no configured row, so it is rejected at the gateway like any
// other unknown model instead of reaching the provider.
func TestModelMap_MissingModelIs404(t *testing.T) {
	h, spy := newMapHandler(t, modelMapRows())
	rec := postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if spy.count() != 0 {
		t.Fatal("a request with no model must not reach the provider")
	}
}

// TestModelMap_NoResolverForwardsUnchanged proves the mapping is inert when no
// resolver is wired. Such a gateway advertises no models either, so list and
// dispatch still agree — and every pre-existing embedder keeps working.
func TestModelMap_NoResolverForwardsUnchanged(t *testing.T) {
	spy := newDispatchSpy()
	h := newTestHandler(spy, nil, nil) // no WithModelResolver
	rec := postAs(t, h.route(), "/llm/v1/chat/completions", mapProjectID,
		`{"model":"openai/whatever-the-caller-wants","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got, ok := spy.last()
	if !ok {
		t.Fatal("the provider was not called")
	}
	if want := (dispatched{provider: "openai", model: "whatever-the-caller-wants"}); got != want {
		t.Fatalf("provider received %+v, want %+v", got, want)
	}
}

// ── issue #469: the three conditions in which the model set is unreadable ─────
//
// Each condition gets its OWN test and its OWN expected behaviour. Before issue
// #469 all three produced one outcome: HTTP 200, and the caller's model name
// sent to the provider with no map. Every test below fails on that behaviour.

// counterDelta returns the increase of v across fn.
//
// The counters are process-wide expvar variables, so an absolute value depends
// on which other tests ran first. Only the delta is stable.
func counterDelta(t *testing.T, v *expvar.Int, fn func()) int64 {
	t.Helper()
	before := v.Value()
	fn()
	return v.Value() - before
}

// assertRefused checks the status, the OpenAI error body, and that the provider
// was never called.
func assertRefused(t *testing.T, rec *httptest.ResponseRecorder, spy *dispatchSpy, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, wantStatus, rec.Body.String())
	}
	if spy.count() != 0 {
		got, _ := spy.last()
		t.Fatalf("the provider was called with %+v; an unmapped model must never reach it", got)
	}
	var body openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code != wantCode {
		t.Fatalf("error code = %q, want %q (body=%s)", body.Error.Code, wantCode, rec.Body.String())
	}
}

// TestModelMap_NoProjectIsRefused covers condition 1: the request carries no
// project identity.
//
// This is a condition of the REQUEST, not a fault. A caller with no project has
// no configured model and no credential, so the gateway answers the OpenAI 404
// that says exactly that. It must not send the caller's model name onward: with
// no project the budget gate also does not run, so an accepted request would be
// unmapped AND unmetered.
func TestModelMap_NoProjectIsRefused(t *testing.T) {
	h, spy := newMapHandler(t, modelMapRows())
	var rec *httptest.ResponseRecorder
	delta := counterDelta(t, modelMapRefusedNoProject, func() {
		rec = postAs(t, h, "/llm/v1/chat/completions", "", `{"model":"anything","messages":[]}`)
	})
	assertRefused(t, rec, spy, http.StatusNotFound, "model_not_found")
	if delta != 1 {
		t.Fatalf("%s rose by %d, want 1 — an operator cannot see this refusal",
			MetricModelMapRefusedNoProject, delta)
	}
}

// TestModelMap_NoDatabaseIsRefused covers condition 2: the resolver holds no
// database handle, so it can never read any project's model set.
//
// This is a WIRING fault, not a deployment posture. A gateway that runs with no
// database gets no resolver at all, and that posture is covered by
// TestModelMap_NoResolverForwardsUnchanged.
func TestModelMap_NoDatabaseIsRefused(t *testing.T) {
	spy := newDispatchSpy()
	resolver := NewModelResolver(ModelResolverConfig{DB: nil})
	h := newTestHandler(spy, nil, nil, WithModelResolver(resolver))

	var rec *httptest.ResponseRecorder
	delta := counterDelta(t, modelMapRefusedNoDatabase, func() {
		rec = postAs(t, h.route(), "/llm/v1/chat/completions", mapProjectID,
			`{"model":"gpt-5.1","messages":[]}`)
	})
	assertRefused(t, rec, spy, http.StatusBadGateway, "model_catalogue_unavailable")
	if delta != 1 {
		t.Fatalf("%s rose by %d, want 1 — an operator cannot see this refusal",
			MetricModelMapRefusedNoDatabase, delta)
	}
}

// TestModelMap_LookupFailureIsRefused covers condition 3: the query fails and
// no cached list exists.
//
// Reaching this outcome means the gateway has NEVER read this project's model
// set, so there is no last good list to bound a permissive path with. The
// deleted elitea-main handler answered 502 for the same condition.
func TestModelMap_LookupFailureIsRefused(t *testing.T) {
	spy := newDispatchSpy()
	resolver := NewModelResolver(ModelResolverConfig{DB: &fakeModelDB{err: errors.New("connection refused")}})
	h := newTestHandler(spy, nil, nil, WithModelResolver(resolver))

	var rec *httptest.ResponseRecorder
	delta := counterDelta(t, modelMapRefusedLookupFailed, func() {
		rec = postAs(t, h.route(), "/llm/v1/chat/completions", mapProjectID,
			`{"model":"gpt-5.1","messages":[]}`)
	})
	assertRefused(t, rec, spy, http.StatusBadGateway, "model_catalogue_unavailable")
	if delta != 1 {
		t.Fatalf("%s rose by %d, want 1 — an operator cannot see this refusal",
			MetricModelMapRefusedLookupFailed, delta)
	}
}

// TestModelMap_QueryFailureWithACachedListStillDispatches is the other half of
// the issue #469 decision, and the reason the three refusals above are safe.
//
// A database fault must not stop all inference. It does not: once the gateway
// has read a project's model set, a later query failure serves the last good
// list, and the request maps and dispatches as normal. That stale list is the
// bounded permissive path — every name in it came from a real configuration
// row. Delete this behaviour and a database blip becomes a total outage.
func TestModelMap_QueryFailureWithACachedListStillDispatches(t *testing.T) {
	clock := time.Now()
	db := &fakeModelDB{rows: modelMapRows()}
	spy := newDispatchSpy()
	resolver := NewModelResolver(ModelResolverConfig{
		DB:  db,
		Now: func() time.Time { return clock },
	})
	h := newTestHandler(spy, nil, nil, WithModelResolver(resolver)).route()

	// First call: the query succeeds and fills the cache.
	if rec := postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Prod GPT","messages":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("first call: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// The cache expires, and the database is now down.
	clock = clock.Add(2 * DefaultModelsCacheTTL)
	db.err = errors.New("connection refused")

	rec := postAs(t, h, "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Prod GPT","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; a cached list must survive a database fault; body=%s",
			rec.Code, rec.Body.String())
	}
	got, ok := spy.last()
	if !ok {
		t.Fatal("the provider was not called")
	}
	if want := (dispatched{provider: "", model: "gpt-5.1"}); got != want {
		t.Fatalf("provider received %+v, want %+v", got, want)
	}
}

// TestModelMap_ResolveClassifiesEachUnknownCondition holds resolve and List in
// step.
//
// resolve names the three conditions itself, in the order List applies them.
// This test proves the two agree: each case is one List reports as unknown, and
// resolve reports the matching outcome. If List ever changes its order or adds
// a fourth unknown condition, the pairing below breaks here rather than in
// production.
func TestModelMap_ResolveClassifiesEachUnknownCondition(t *testing.T) {
	cases := []struct {
		name      string
		resolver  *ModelResolver
		projectID string
		want      modelLookup
	}{
		{
			name:      "empty project id",
			resolver:  NewModelResolver(ModelResolverConfig{DB: &fakeModelDB{rows: modelMapRows()}}),
			projectID: "",
			want:      modelSetNoProject,
		},
		{
			name:      "nil database handle",
			resolver:  NewModelResolver(ModelResolverConfig{DB: nil}),
			projectID: mapProjectID,
			want:      modelSetNoDatabase,
		},
		{
			name:      "query failure with no cache",
			resolver:  NewModelResolver(ModelResolverConfig{DB: &fakeModelDB{err: errors.New("down")}}),
			projectID: mapProjectID,
			want:      modelSetLookupFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, known := tc.resolver.list(t.Context(), tc.projectID); known {
				t.Fatalf("List reports the model set as known; this case is not an unknown set at all")
			}
			_, got := tc.resolver.resolve(t.Context(), tc.projectID, []string{"gpt-5.1"})
			if got != tc.want {
				t.Fatalf("resolve outcome = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestModelMapMetricNames_AreAllPublished proves the counters this package
// declares are the counters it publishes. The composition root builds the
// /metrics allowlist from ModelMapMetricNames, so a name that no expvar
// variable carries would serve nothing and report nothing.
func TestModelMapMetricNames_AreAllPublished(t *testing.T) {
	names := ModelMapMetricNames()
	if len(names) != 3 {
		t.Fatalf("ModelMapMetricNames returned %d names, want 3", len(names))
	}
	for _, name := range names {
		if expvar.Get(name) == nil {
			t.Errorf("metric %q is named but not published; /metrics would serve nothing for it", name)
		}
	}
}

// ── resolver-level units ──────────────────────────────────────────────────────

// TestModelNames_IDAndProviderModel pins the two names one configuration row
// yields.
func TestModelNames_IDAndProviderModel(t *testing.T) {
	cases := []struct {
		name         string
		title        string
		data         string
		wantID       string
		wantProvider string
	}{
		{"title and wire name differ", "Prod GPT", `{"name":"gpt-5.1"}`, "Prod GPT", "gpt-5.1"},
		{"no title falls back to the wire name", "", `{"name":"gpt-5.1"}`, "gpt-5.1", "gpt-5.1"},
		{"no wire name dispatches the title", "Prod GPT", `{}`, "Prod GPT", "Prod GPT"},
		{"malformed data is treated as absent", "Prod GPT", `not json`, "Prod GPT", "Prod GPT"},
		{"nothing usable is skipped", "", `{}`, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, providerModel, _ := modelNames(tc.title, []byte(tc.data))
			if id != tc.wantID || providerModel != tc.wantProvider {
				t.Fatalf("modelNames = (%q, %q), want (%q, %q)", id, providerModel, tc.wantID, tc.wantProvider)
			}
		})
	}
}

// TestRequestModelCandidates pins the order the candidates are tried in. The
// rejoined form must come first, or an advertised id that carries a provider
// prefix would never match.
func TestRequestModelCandidates(t *testing.T) {
	if got := requestModelCandidates("openai", "gpt-4o"); !equalStrs(got, []string{"openai/gpt-4o", "gpt-4o"}) {
		t.Fatalf("candidates = %v", got)
	}
	if got := requestModelCandidates("", "gpt-4o"); !equalStrs(got, []string{"gpt-4o"}) {
		t.Fatalf("candidates = %v", got)
	}
	if got := requestModelCandidates("openai", ""); !equalStrs(got, []string{""}) {
		t.Fatalf("candidates = %v", got)
	}
}
