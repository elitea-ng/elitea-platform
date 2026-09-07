package api

// Where the two halves of the pipeline-trigger surface are MOUNTED.
//
// The package's own tests prove what each handler decides. This file proves the
// one thing only the router can be asked: the inbound call reaches its handler
// WITHOUT a session, and every settings route does not.
//
// It is worth its own file because the mistake it catches is invisible in both
// directions. An inbound route accidentally moved under the Auth group answers
// 401 to every webhook a customer configured, and the handler's own tests keep
// passing. A settings route accidentally moved ABOVE it serves a project's
// trigger credentials to anybody, and the handler's own tests keep passing
// then too.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v2pipelinetriggers "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// pipelineTriggerRouter builds the production router with the trigger handler
// mounted and a validator that accepts NOTHING.
//
// The handler carries no pool, so any route that reaches it answers its own
// "not available on this deployment" 503. That 503 is the discriminator: the
// Auth middleware answers 401, so the two are told apart by status alone and no
// database is needed to tell them apart.
func pipelineTriggerRouter() http.Handler {
	return NewRouter(RouterConfig{
		AuthValidator:      testTokenValidator{user: auth.User{ID: "1"}},
		PrincipalValidator: testPrincipalValidator{},
		PipelineTriggers:   v2pipelinetriggers.NewHandler(nil),
	})
}

// TestInboundPipelineTriggerIsReachableWithoutASession.
func TestInboundPipelineTriggerIsReachableWithoutASession(t *testing.T) {
	router := pipelineTriggerRouter()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v2/pipeline_trigger/7/abcdef", strings.NewReader(""))
	router.ServeHTTP(recorder, request)

	if recorder.Code == http.StatusUnauthorized {
		t.Fatal("the inbound trigger answered 401 to a request with no session. It is mounted under " +
			"the Auth group, which makes every configured webhook stop working: its only credential " +
			"is the per-pipeline secret, and the caller is an external system with no cookie.")
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 from the handler itself (no pool in this composition); body = %s",
			recorder.Code, recorder.Body.String())
	}
}

// TestPipelineTriggerSettingsRoutesRequireASession is the other direction.
func TestPipelineTriggerSettingsRoutesRequireASession(t *testing.T) {
	router := pipelineTriggerRouter()

	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v2/pipeline_triggers/prompt_lib/7/1"},
		{http.MethodGet, "/api/v2/pipeline_triggers/secret/prompt_lib/7/1"},
		{http.MethodPost, "/api/v2/pipeline_triggers/prompt_lib/7/1"},
		{http.MethodDelete, "/api/v2/pipeline_triggers/prompt_lib/7/1"},
		{http.MethodGet, "/api/v2/pipeline_schedules/prompt_lib/7/1"},
		{http.MethodPut, "/api/v2/pipeline_schedules/prompt_lib/7/1"},
		{http.MethodDelete, "/api/v2/pipeline_schedules/prompt_lib/7/1"},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, strings.NewReader("{}")))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 — this route reads or writes a project's trigger "+
					"credentials and must sit under the Auth group; body = %s",
					recorder.Code, recorder.Body.String())
			}
		})
	}
}

// TestPipelineTriggerRoutesAreAbsentWhenTheHandlerIsNot.
//
// An unregistered route answers the group's own 404. A route registered with no
// runtime behind it answering 200 is the "the endpoint exists, nothing is
// wired" defect this repository keeps rediscovering (issues 128, 134, 136, 149),
// so the absence is asserted rather than assumed.
func TestPipelineTriggerRoutesAreAbsentWhenTheHandlerIsNot(t *testing.T) {
	router := NewRouter(RouterConfig{
		AuthValidator:      testTokenValidator{user: auth.User{ID: "1"}},
		PrincipalValidator: testPrincipalValidator{},
	})

	// WITH a credential. The /api/v2 group's documented ordering is that an
	// unknown path answers 404 to a caller who has one and 401 to a caller who
	// does not, so an anonymous probe here would report the Auth middleware
	// rather than the absence this test is about.
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, testAuthHeader(
		httptest.NewRequest(http.MethodPost, "/api/v2/pipeline_trigger/7/abcdef", strings.NewReader(""))))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no handler is composed; body = %s",
			recorder.Code, recorder.Body.String())
	}
}
