package supportassistant

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

func withUser(r *http.Request, user auth.User) *http.Request {
	return r.WithContext(auth.ContextWithUser(r.Context(), user))
}

/* ── the config route ──────────────────────────────────────────────────── */

// A DEPLOYMENT WITH NO CONFIGURATION ANSWERS, and answers `false`.
//
// The widget mounts in the app shell and treats a non-answer as a failure, so a
// 404 or a 500 here is a rendering bug on every page of a deployment that simply
// has the feature off.
func TestConfigAnswersDisabledWithoutAPool(t *testing.T) {
	handler := NewHandler(nil)
	recorder := httptest.NewRecorder()
	handler.Config(recorder, withUser(
		httptest.NewRequest(http.MethodGet, "/config/", nil),
		auth.User{ID: "5", UserID: "5"}))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["enabled"] != false {
		t.Fatalf("enabled = %v, want false", body["enabled"])
	}
}

// An UNAUTHENTICATED caller gets 200/false, not 401.
//
// A 401 from a widget that polls on every page load is indistinguishable from a
// session expiry to the shell around it, which is the peripheral-401 logout loop
// this app has already been bitten by once.
func TestConfigDoesNotAnswer401(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewHandler(nil).Config(recorder, httptest.NewRequest(http.MethodGet, "/config/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

// THE DISABLED BODY DISCLOSES NOTHING. A feature that is off must not also be a
// channel that reports which project the platform reserved for support, what the
// deployment renamed the assistant to, or who is asking.
func TestDisabledConfigCarriesNothingButTheSwitch(t *testing.T) {
	encoded, err := json.Marshal(disabledConfig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var body ConfigResponse
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Enabled || body.SupportProjectID != 0 || body.Title != "" ||
		body.WelcomeMessage != "" || body.Placeholder != "" ||
		body.User.ID != 0 || body.User.Name != "" || body.User.Avatar != "" {
		t.Fatalf("disabled config carries data: %+v", body)
	}
	// The strings have no `omitempty`, so the keys are present and empty rather
	// than absent. That is the shape the client parses; what matters is that no
	// VALUE survives, which the decode above asserts.
	if !strings.Contains(string(encoded), `"enabled":false`) {
		t.Fatalf("disabled config does not report the switch: %s", encoded)
	}
}

/* ── predict validation ────────────────────────────────────────────────── */

// `question_id` IS REQUIRED, and required as a lowercase UUID.
//
// It is the turn's idempotency key: the execution pipeline derives the turn's
// message identifiers from it, so a server-minted one would turn every
// double-submit into a second billed agent run. This test is what stops a future
// "be lenient, generate one if it is missing" change from landing quietly.
func TestPredictRejectsAMissingOrNonCanonicalQuestionID(t *testing.T) {
	for name, questionID := range map[string]string{
		"absent":     "",
		"not a uuid": "42",
		"uppercase":  "3F2504E0-4F89-41D3-9A0C-0305E82C3301",
	} {
		t.Run(name, func(t *testing.T) {
			if validTurnUUID(questionID) {
				t.Fatalf("validTurnUUID(%q) = true", questionID)
			}
		})
	}
	if !validTurnUUID(sampleUUID) {
		t.Fatalf("validTurnUUID(%q) = false", sampleUUID)
	}
}

// The page context is APPENDED to the question rather than dropped, and a turn
// without one is unchanged — no empty fence, no trailing whitespace.
func TestPageContextIsFencedOntoTheQuestionAndOnlyWhenPresent(t *testing.T) {
	handler := NewHandler(nil)

	if got := handler.composeUserInput(PredictRequest{Content: "why?"}); got != "why?" {
		t.Fatalf("composeUserInput without context = %q, want %q", got, "why?")
	}

	got := handler.composeUserInput(PredictRequest{
		Content: "why?",
		Context: &AssistantContext{CurrentPage: "/agents", ProjectName: "Acme"},
	})
	if !strings.HasPrefix(got, "why?") {
		t.Fatalf("the question is not first in %q", got)
	}
	if !strings.Contains(got, "<support_assistant_context>") ||
		!strings.Contains(got, `"current_page":"/agents"`) {
		t.Fatalf("the context did not reach the agent: %q", got)
	}
}

/* ── the gate ──────────────────────────────────────────────────────────── */

// THE EXTRACTOR FAILS CLOSED.
//
// `projectIDFromContext` is what the permission gate resolves against. If the
// middleware order were ever inverted so the gate ran before `resolve`, this is
// the thing that has to refuse — returning ("", true) would resolve permissions
// against an empty project id and let the request through.
func TestProjectExtractorRefusesWithoutResolvedSettings(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/conversations/", nil)
	if _, ok := projectIDFromContext(request); ok {
		t.Fatal("extractor accepted a request with no resolved settings")
	}
	resolved := request.WithContext(withSettings(request.Context(),
		platformconfig.SupportAssistant{ProjectID: 0}))
	if _, ok := projectIDFromContext(resolved); ok {
		t.Fatal("extractor accepted a resolved project id of 0")
	}
	resolved = request.WithContext(withSettings(request.Context(),
		platformconfig.SupportAssistant{ProjectID: 42}))
	got, ok := projectIDFromContext(resolved)
	if !ok || got != "42" {
		t.Fatalf("extractor = (%q, %v), want (\"42\", true)", got, ok)
	}
}

// Every gated route refuses when the assistant is not configured, and refuses
// with 503 rather than 404 — the route exists and the caller may use it; the
// platform has not been set up to serve it.
func TestGatedRoutesRefuseWhenNotConfigured(t *testing.T) {
	routes := NewHandler(nil, WithPermissionResolver(stubResolver{})).Routes()
	for _, testCase := range []struct{ method, path string }{
		{http.MethodGet, "/conversations/"},
		{http.MethodPost, "/conversations/"},
		{http.MethodGet, "/conversation/" + sampleUUID},
		{http.MethodPost, "/predict/" + sampleUUID},
	} {
		t.Run(testCase.method+" "+testCase.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			routes.ServeHTTP(recorder, withUser(
				httptest.NewRequest(testCase.method, testCase.path, nil),
				auth.User{ID: "5", UserID: "5"}))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (body %s)", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// The config route stays reachable on the SAME router that refuses everything
// else, because it is the question "is this off?".
func TestConfigIsReachableWhileEverythingElseRefuses(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewHandler(nil, WithPermissionResolver(stubResolver{})).Routes().ServeHTTP(recorder,
		withUser(httptest.NewRequest(http.MethodGet, "/config/", nil),
			auth.User{ID: "5", UserID: "5"}))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

const sampleUUID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

type stubResolver struct{}

func (stubResolver) ResolvePermissions(
	context.Context, auth.User, string, string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{}, nil
}

/* ── input bounds ──────────────────────────────────────────────────────── */

// A MESSAGE THE RUN WOULD REFUSE IS REFUSED HERE, as the client's problem.
//
// The use case caps UserInput at 256KB and answers one undifferentiated error
// for nine distinct causes, which the route can only render as a 502 — "the
// support agent is broken" — for what is really "your message is too long".
// This is the check that keeps that decision on the client's side of the line.
func TestOversizedMessageIsTheClientsProblem(t *testing.T) {
	if maxAgentUserInput >= int(maxPredictBody) {
		t.Fatalf("maxAgentUserInput (%d) is not below maxPredictBody (%d); the gap this "+
			"check exists to cover has closed and the check is meaningless",
			maxAgentUserInput, maxPredictBody)
	}
}

// THE APPENDED CONTEXT COUNTS TOWARDS THE CAP.
//
// The page context is added to the message after the user stops typing, so a
// message that fits can stop fitting on bytes the user never saw. Measuring the
// COMPOSED string is what makes the refusal correspond to something they can act
// on, and this pins that the composition really does grow the input.
func TestPageContextCountsTowardsTheInputCap(t *testing.T) {
	handler := NewHandler(nil)
	bare := handler.composeUserInput(PredictRequest{Content: "why?"})
	withContext := handler.composeUserInput(PredictRequest{
		Content: "why?",
		Context: &AssistantContext{CurrentPage: "/agents", ProjectName: "Acme"},
	})
	if len(withContext) <= len(bare) {
		t.Fatalf("composed input did not grow: %d vs %d", len(withContext), len(bare))
	}
}

/* ── search ────────────────────────────────────────────────────────────── */

// `%` AND `_` ARE THE USER'S CHARACTERS, not LIKE's.
func TestSearchTermWildcardsAreEscaped(t *testing.T) {
	for term, want := range map[string]string{
		"50% CPU":    `50\% CPU`,
		"a_b":        `a\_b`,
		`back\slash`: `back\\slash`,
		"plain":      "plain",
		"":           "",
	} {
		if got := escapeLikePattern(term); got != want {
			t.Errorf("escapeLikePattern(%q) = %q, want %q", term, got, want)
		}
	}
}

/* ── bootstrap cooldown ────────────────────────────────────────────────── */

// A FAILED BOOTSTRAP IS NOT RETRIED ON THE NEXT REQUEST.
//
// Without the gate, a deployment whose provisioning fails re-runs the whole
// pipeline for every page load of every user, serialised on one advisory lock.
func TestFailedBootstrapIsNotRetriedImmediately(t *testing.T) {
	s := &store{logger: slog.Default()}
	if !s.claimBootstrapAttempt() {
		t.Fatal("the first attempt was refused")
	}
	if s.claimBootstrapAttempt() {
		t.Fatal("a second attempt was allowed immediately; a failing deployment would retry per request")
	}
	// A bootstrap that PRODUCED a project id clears the gate, so the next call
	// is not delayed by a cooldown started for an attempt that then succeeded.
	s.releaseBootstrapCooldown()
	if !s.claimBootstrapAttempt() {
		t.Fatal("the gate stayed closed after a success")
	}
}
