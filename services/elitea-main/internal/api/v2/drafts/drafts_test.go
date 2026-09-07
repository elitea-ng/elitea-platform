package drafts_test

// Behaviour tests for the three AI-draft routes (#254 P1), against a stubbed
// completer standing in for the LLM gateway.
//
// Every case here is a claim about what a CALLER sees, not about an internal
// helper: the status, the body shape, and — for the settings case — what the
// gateway is actually asked for. The route table at the top is what makes the
// three routes provably share the status contract instead of three copies
// drifting apart, which is what the one-package decision buys.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	v2drafts "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/drafts"
	v2predict "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/predict"
)

// stubCompleter stands in for the gateway hop. It records the request so a test
// can assert what was ASKED rather than only what came back — the difference
// between "the model settings are honoured" and "the response happens to look
// right".
type stubCompleter struct {
	content string
	err     error

	got    v2predict.CompletionRequest
	called int
}

func (s *stubCompleter) Complete(_ context.Context, req v2predict.CompletionRequest) (string, error) {
	s.called++
	s.got = req
	if s.err != nil {
		return "", s.err
	}
	return s.content, nil
}

// route is one of the three endpoints, addressed the way the router addresses
// it. handler picks the method off the Handler so the shared cases below can
// run against all three without naming them one at a time.
type route struct {
	name    string
	path    string
	handler func(*v2drafts.Handler) http.HandlerFunc
	// minimalBody is the smallest body that reaches the LLM hop.
	minimalBody string
	// modelAnswer is a valid draft for this route, used by the shared cases
	// that need a 200.
	modelAnswer string
}

var routes = []route{
	{
		name:        "generate_skill_draft",
		path:        "/generate_skill_draft/prompt_lib/{projectID}",
		handler:     func(h *v2drafts.Handler) http.HandlerFunc { return h.GenerateSkillDraft },
		minimalBody: `{"user_description":"a skill that reviews pull requests"}`,
		modelAnswer: `{"name":"PR Reviewer","description":"Reviews pull requests","instructions":"Be thorough","tags":["quality"]}`,
	},
	{
		name:        "generate_application_draft",
		path:        "/generate_application_draft/prompt_lib/{projectID}",
		handler:     func(h *v2drafts.Handler) http.HandlerFunc { return h.GenerateApplicationDraft },
		minimalBody: `{"user_description":"an agent that triages incidents"}`,
		modelAnswer: `{"name":"Triager","description":"Triages incidents","instructions":"Sort by severity"}`,
	},
	{
		name:        "generate_project_context_draft",
		path:        "/generate_project_context_draft/prompt_lib/{projectID}",
		handler:     func(h *v2drafts.Handler) http.HandlerFunc { return h.GenerateProjectContextDraft },
		minimalBody: `{"user_description":"a Go monorepo behind a React SPA"}`,
		modelAnswer: `{"project_background":"## Stack\nGo monorepo, React SPA."}`,
	},
}

func serve(t *testing.T, r route, completer v2predict.Completer, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := chi.NewRouter()
	mux.Post(r.path, r.handler(v2drafts.NewHandler(completer)))

	request := httptest.NewRequest(http.MethodPost, strings.Replace(r.path, "{projectID}", "7", 1),
		strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}

func decode[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.NewDecoder(recorder.Body).Decode(&value); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	return value
}

// ---------------------------------------------------------------------------
// The status contract, asserted for all three routes at once.
// ---------------------------------------------------------------------------

func TestEveryDraftRouteAnswers503WhenNoLLMPlaneIsComposed(t *testing.T) {
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			// nil completer is the "LLM_GATEWAY_URL is unset" deployment. The
			// route must still exist and must NAME the missing configuration:
			// a 404 here is what #126 shipped, and it is indistinguishable
			// from a typo'd path.
			recorder := serve(t, r, nil, r.minimalBody)

			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body=%s", recorder.Code, recorder.Body.String())
			}
			body := decode[map[string]any](t, recorder)
			if body["code"] != v2drafts.NotConfiguredCode {
				t.Errorf("code = %v, want %q — an operator matches on the code, not the prose",
					body["code"], v2drafts.NotConfiguredCode)
			}
			if !strings.Contains(body["error"].(string), "LLM_GATEWAY_URL") {
				t.Errorf("error = %q, want it to name LLM_GATEWAY_URL", body["error"])
			}
		})
	}
}

func TestEveryDraftRouteMapsAGatewayFailureTo502(t *testing.T) {
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			completer := &stubCompleter{err: errors.New("dial tcp: connection refused")}
			recorder := serve(t, r, completer, r.minimalBody)

			// 502, not 500: the hop is one identifiable component away, and
			// 503 stays reserved for "no gateway configured at all".
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%s", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "connection refused") {
				t.Error("the upstream error text reached the caller; it can carry provider detail and is log-only")
			}
		})
	}
}

func TestEveryDraftRouteRefusesAMalformedBody(t *testing.T) {
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			for name, body := range map[string]string{
				"not json":             `{"user_description":`,
				"empty":                ``,
				"blank description":    `{"user_description":"   "}`,
				"missing description":  `{"llm_settings":{"model_name":"gpt-4o-mini"}}`,
				"description not text": `{"user_description":42}`,
			} {
				t.Run(name, func(t *testing.T) {
					completer := &stubCompleter{content: r.modelAnswer}
					recorder := serve(t, r, completer, body)

					if recorder.Code != http.StatusBadRequest {
						t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
					}
					if completer.called != 0 {
						t.Error("the gateway was called for a body that never validated — that spends the project's budget on a request the caller cannot have meant")
					}
				})
			}
		})
	}
}

func TestEveryDraftRouteRefusesAnAnswerThatIsNotJSON(t *testing.T) {
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			recorder := serve(t, r, &stubCompleter{content: "Sure! Here is a draft for you."}, r.minimalBody)

			// 422 and not 500: the service is not broken, the generation is,
			// and the SPA distinguishes the two so it can offer "try again".
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestEveryDraftRouteToleratesAFencedAnswer(t *testing.T) {
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			content := "Here you go:\n```json\n" + r.modelAnswer + "\n```\nHope that helps!"
			recorder := serve(t, r, &stubCompleter{content: content}, r.minimalBody)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestEveryDraftRouteNamesTruncationWhenTheAnswerIsCutOff(t *testing.T) {
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			// An unbalanced object is the signature of a max_tokens cut-off.
			// "unparseable" would read as "retry the same request", which
			// would fail the same way every time.
			cut := strings.TrimSuffix(r.modelAnswer, "}") + `,"more":{"a":`
			recorder := serve(t, r, &stubCompleter{content: cut}, r.minimalBody)

			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "max_tokens") {
				t.Errorf("the 422 does not name max_tokens, so the caller cannot repair it; body=%s",
					recorder.Body.String())
			}
		})
	}
}

func TestEveryDraftRouteSignsTheProjectFromThePathAndForwardsTheModelSettings(t *testing.T) {
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			completer := &stubCompleter{content: r.modelAnswer}
			body := strings.TrimSuffix(r.minimalBody, "}") +
				`,"llm_settings":{"model_name":"gpt-4o-mini","temperature":0,"max_tokens":4096,"reasoning_effort":"low"}}`
			recorder := serve(t, r, completer, body)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
			}
			got := completer.got
			if got.ProjectID != "7" {
				t.Errorf("ProjectID = %q, want %q — the gateway bills the project in the path", got.ProjectID, "7")
			}
			if got.Model != "gpt-4o-mini" {
				t.Errorf("Model = %q, want gpt-4o-mini", got.Model)
			}
			// A pointer, so "explicitly 0" stays distinguishable from absent —
			// 0 temperature is a legitimate, meaningful value for a draft.
			if got.Temperature == nil || *got.Temperature != 0 {
				t.Errorf("Temperature = %v, want an explicit 0", got.Temperature)
			}
			if got.MaxTokens == nil || *got.MaxTokens != 4096 {
				t.Errorf("MaxTokens = %v, want 4096", got.MaxTokens)
			}
			if got.ReasoningEffort != "low" {
				t.Errorf("ReasoningEffort = %q, want low", got.ReasoningEffort)
			}
			if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[1].Role != "user" {
				t.Fatalf("Messages = %+v, want a system prompt then the user's description", got.Messages)
			}
			if !strings.Contains(got.Messages[0].Content, "JSON object") {
				t.Error("the system prompt does not ask for a JSON object, so the parse below it is a guess")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Per-route success shapes. These are the contracts the SPA casts to.
// ---------------------------------------------------------------------------

func TestSkillDraftReturnsTheShapeTheSkillFormApplies(t *testing.T) {
	completer := &stubCompleter{
		content: `{"name":"PR Reviewer","description":"Reviews pull requests","instructions":"Be thorough","tags":["quality","github"]}`,
	}
	recorder := serve(t, routes[0], completer, `{"user_description":"a skill that reviews pull requests"}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	draft := decode[v2drafts.SkillDraft](t, recorder)
	// Slugified, not rejected: a model that answers "PR Reviewer" should cost
	// the user a rename, not a whole generation.
	if draft.Name != "pr-reviewer" {
		t.Errorf("Name = %q, want pr-reviewer", draft.Name)
	}
	if draft.Description != "Reviews pull requests" || draft.Instructions != "Be thorough" {
		t.Errorf("unexpected draft: %+v", draft)
	}
	if len(draft.Tags) != 2 {
		t.Errorf("Tags = %v, want both — CreateSkill applies the draft wholesale with setValue(draft)", draft.Tags)
	}
}

func TestSkillDraftTagsAreNeverNull(t *testing.T) {
	// `tags: null` would reach the SPA as null and break `.map` on a field its
	// SkillDraft type declares as a list.
	completer := &stubCompleter{content: `{"name":"x","description":"d","instructions":"i"}`}
	recorder := serve(t, routes[0], completer, `{"user_description":"anything"}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"tags":[]`) {
		t.Errorf("body = %s, want an empty tags array", recorder.Body.String())
	}
}

func TestSkillDraftRefusesANameItCannotSalvage(t *testing.T) {
	for name, answer := range map[string]string{
		"nothing sluggable": `{"name":"###","description":"d","instructions":"i"}`,
		// Legacy's own reserved-word rule. A refusal and not a coercion:
		// silently renaming the skill the user asked for is the wrong kind of
		// helpful.
		"reserved word": `{"name":"claude-helper","description":"d","instructions":"i"}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := serve(t, routes[0], &stubCompleter{content: answer}, `{"user_description":"anything"}`)
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestApplicationDraftReturnsTheReviewFormsFields(t *testing.T) {
	completer := &stubCompleter{content: `{
		"name":"Incident Triager",
		"description":"Triages incidents",
		"instructions":"Sort by severity",
		"welcome_message":"What broke?",
		"conversation_starters":["Triage this page","  ","Summarise the last hour","Who is on call","What changed","Extra"]
	}`}
	recorder := serve(t, routes[1], completer, `{"user_description":"an agent that triages incidents"}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	draft := decode[v2drafts.ApplicationDraft](t, recorder)
	if draft.Name != "Incident Triager" || draft.WelcomeMessage != "What broke?" {
		t.Errorf("unexpected draft: %+v", draft)
	}
	// Blank entries dropped, then capped at four — legacy's own rule, and the
	// one the review form renders against.
	if len(draft.ConversationStarters) != 4 {
		t.Fatalf("ConversationStarters = %v, want 4 after the blank is dropped and the list is capped",
			draft.ConversationStarters)
	}
	if draft.ConversationStarters[1] != "Summarise the last hour" {
		t.Errorf("ConversationStarters = %v, want the blank entry dropped rather than kept as an empty chip",
			draft.ConversationStarters)
	}
}

func TestApplicationDraftRefusesAnAnswerMissingARequiredField(t *testing.T) {
	completer := &stubCompleter{content: `{"description":"Triages incidents","instructions":"Sort by severity"}`}
	recorder := serve(t, routes[1], completer, `{"user_description":"anything"}`)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestProjectContextDraftReturnsTheBackground(t *testing.T) {
	completer := &stubCompleter{content: `{"project_background":"## Stack\nGo monorepo, React SPA."}`}
	recorder := serve(t, routes[2], completer, `{"user_description":"a Go monorepo behind a React SPA"}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	draft := decode[v2drafts.ProjectContextDraft](t, recorder)
	if !strings.Contains(draft.ProjectBackground, "Go monorepo") {
		t.Errorf("ProjectBackground = %q", draft.ProjectBackground)
	}
}

func TestProjectContextDraftEditModeShowsTheModelTheCurrentBackground(t *testing.T) {
	// This is the one edit mode that IS served: it carries its own prior
	// content, so it needs no repository. If the current background never
	// reaches the prompt the route silently becomes a create — the model
	// rewrites from nothing and the user applies it over their real content.
	completer := &stubCompleter{content: `{"project_background":"refined"}`}
	recorder := serve(t, routes[2], completer,
		`{"user_description":"add the deploy process","current_project_background":"MARKER-EXISTING-CONTENT"}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(completer.got.Messages[0].Content, "MARKER-EXISTING-CONTENT") {
		t.Fatalf("the current background never reached the prompt, so edit mode is a create in disguise:\n%s",
			completer.got.Messages[0].Content)
	}
}

func TestProjectContextDraftIsCappedAtTheColumnLength(t *testing.T) {
	// project_context.content is capped at 2500 characters, so a longer draft
	// cannot be saved. Truncating here beats handing the review form content
	// whose save will fail.
	long := strings.Repeat("x", 4000)
	completer := &stubCompleter{content: `{"project_background":"` + long + `"}`}
	recorder := serve(t, routes[2], completer, `{"user_description":"anything"}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	draft := decode[v2drafts.ProjectContextDraft](t, recorder)
	if len(draft.ProjectBackground) != 2500 {
		t.Errorf("len = %d, want 2500", len(draft.ProjectBackground))
	}
}

// ---------------------------------------------------------------------------
// Edit-by-id is refused, not ignored.
// ---------------------------------------------------------------------------

func TestEditByIdIsRefusedRatherThanSilentlyDroppedToACreate(t *testing.T) {
	// A caller sending skill_id/application_id means "rewrite THIS one". A
	// handler that ignored the field would answer 200 with a from-scratch
	// draft the caller then applies over their existing entity — a wrong
	// answer nothing surfaces, which is the #128 pattern.
	for _, tc := range []struct {
		name  string
		route route
		body  string
	}{
		{"skill", routes[0], `{"user_description":"tighten it","skill_id":3,"version_id":1}`},
		{"application", routes[1], `{"user_description":"tighten it","application_id":3,"version_id":1}`},
		// Half a pair is still edit intent, and legacy refuses it too (its
		// model_validator demands both).
		{"skill id alone", routes[0], `{"user_description":"tighten it","skill_id":3}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			completer := &stubCompleter{content: tc.route.modelAnswer}
			recorder := serve(t, tc.route, completer, tc.body)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
			}
			if completer.called != 0 {
				t.Error("the model was called for a request that could not be honoured")
			}
		})
	}
}

func TestAnExplicitNullEditIdIsNotEditIntent(t *testing.T) {
	// The SPA's AI-edit modal sends `skill_id: formik.values.id ?? undefined`,
	// which JSON.stringify drops — but a client that sends an explicit null is
	// clearing the field, not asking to edit. Refusing that would break a
	// create the caller meant.
	completer := &stubCompleter{content: routes[0].modelAnswer}
	recorder := serve(t, routes[0], completer, `{"user_description":"anything","skill_id":null,"version_id":null}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
}
