package agentexecution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	app "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/noderecovery"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/noderecovery"
)

type recoveryUseCase struct {
	calls      int
	selector   app.Selector
	submission app.Submission
	err        error
}

func (s *recoveryUseCase) Read(_ context.Context, sel app.Selector) (app.State, error) {
	s.calls++
	s.selector = sel
	return app.State{Schema: "elitea.pipeline.node-recovery-state.v1", ExecutionID: "0123456789abcdef0123456789abcdef", Generation: 1, Status: "suspended", Receipt: json.RawMessage(`{}`)}, s.err
}
func (s *recoveryUseCase) Submit(_ context.Context, sub app.Submission) (app.Outcome, error) {
	s.calls++
	s.submission = sub
	return app.Outcome{Schema: "elitea.pipeline.node-recovery-accepted.v1", ExecutionID: sub.Request.ExecutionID, Generation: sub.Request.Generation}, s.err
}

func TestCurrentNodeRecoveryRouteStrictPublicAdmission(t *testing.T) {
	r := domain.Request{RequestID: strings.Repeat("2", 64), ExecutionID: "0123456789abcdef0123456789abcdef", Generation: 1, ActivationID: strings.Repeat("1", 64), ExpectedRevision: 3, Action: "retry"}
	raw, _ := json.Marshal(r)
	for _, spec := range []struct {
		name, body    string
		permission    bool
		err           error
		status, calls int
	}{{"accepted", string(raw), true, nil, 202, 1}, {"child selector", strings.TrimSuffix(string(raw), "}") + `,"child_thread_id":"foreign"}`, true, nil, 400, 0}, {"duplicate revision", strings.Replace(string(raw), `"expected_revision":3`, `"expected_revision":3,"expected_revision":3`, 1), true, nil, 400, 0}, {"no permission", string(raw), false, nil, 403, 0}, {"stale", string(raw), true, app.ErrNotAllowed, 409, 1}} {
		t.Run(spec.name, func(t *testing.T) {
			useCase := &recoveryUseCase{err: spec.err}
			config := apimw.AuthConfig{PrincipalValidator: currentStartPrincipalValidatorFunc(func(_ context.Context, u auth.User) (auth.User, error) { return u, nil }), FormSessions: authtest.FormSessions{}}
			permissions := currentStartPermissionResolverFunc(func(_ context.Context, u auth.User, mode, project string) (auth.PermissionResolution, error) {
				if mode != auth.PermissionModeDefault || project != "7" {
					t.Fatal(mode, project)
				}
				p := []string{}
				if spec.permission {
					p = []string{"models.chat.messages.create"}
				}
				return auth.PermissionResolution{UserID: 11, Permissions: p}, nil
			})
			route, err := NewCurrentNodeRecoveryRoute(useCase, config, permissions)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v2/elitea_core/task/prompt_lib/7/10000000-0000-4000-8000-000000000043/node_recovery/actions", strings.NewReader(spec.body))
			authtest.SignIn(req, "11")
			req.RemoteAddr = "10.0.0.8:43120"
			w := httptest.NewRecorder()
			route.ServeHTTP(w, req)
			if w.Code != spec.status || useCase.calls != spec.calls {
				t.Fatal(w.Code, w.Body.String(), useCase.calls)
			}
			if spec.status == 202 && (useCase.submission.ActorUserID != 11 || useCase.submission.ProjectID != 7 || useCase.submission.Request != r) {
				t.Fatal(useCase.submission)
			}
		})
	}
}

func TestCurrentNodeRecoveryRoutePerThreadReadAndForeignActorRefusal(t *testing.T) {
	childThread := "p1:Z8y5h_dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzxW6Q"
	actionReq := domain.Request{RequestID: strings.Repeat("2", 64), ExecutionID: "0123456789abcdef0123456789abcdef", Generation: 1, ActivationID: strings.Repeat("1", 64), ExpectedRevision: 3, Action: "retry"}
	actionBody, _ := json.Marshal(actionReq)
	for _, spec := range []struct {
		name        string
		method      string
		query       string
		body        string
		actor       string
		permission  bool
		err         error
		status      int
		calls       int
		wantThread  string
	}{
		{name: "read fanout child thread", method: http.MethodGet, query: "graph_thread=" + childThread, actor: "11", permission: true, status: 200, calls: 1, wantThread: childThread},
		{name: "read default without query", method: http.MethodGet, actor: "11", permission: true, status: 200, calls: 1, wantThread: ""},
		{name: "foreign actor without permission on fanout child read", method: http.MethodGet, query: "graph_thread=" + childThread, actor: "99", permission: false, status: 403, calls: 0},
		{name: "foreign actor denied in tx on fanout child read", method: http.MethodGet, query: "graph_thread=" + childThread, actor: "99", permission: true, err: app.ErrNotAllowed, status: 404, calls: 1, wantThread: childThread},
		{name: "foreign actor denied in tx on fanout child submit", method: http.MethodPost, body: string(actionBody), actor: "99", permission: true, err: app.ErrNotAllowed, status: 409, calls: 1},
		{name: "unknown query param rejected", method: http.MethodGet, query: "child_thread_id=" + childThread, actor: "11", permission: true, status: 400, calls: 0},
		{name: "empty graph_thread rejected", method: http.MethodGet, query: "graph_thread=", actor: "11", permission: true, status: 400, calls: 0},
		{name: "duplicate graph_thread rejected", method: http.MethodGet, query: "graph_thread=" + childThread + "&graph_thread=other", actor: "11", permission: true, status: 400, calls: 0},
		{name: "control char graph_thread rejected", method: http.MethodGet, query: "graph_thread=p1:bad%0athread", actor: "11", permission: true, status: 400, calls: 0},
		{name: "post rejects query param", method: http.MethodPost, query: "graph_thread=" + childThread, body: string(actionBody), actor: "11", permission: true, status: 400, calls: 0},
	} {
		t.Run(spec.name, func(t *testing.T) {
			useCase := &recoveryUseCase{err: spec.err}
			config := apimw.AuthConfig{PrincipalValidator: currentStartPrincipalValidatorFunc(func(_ context.Context, u auth.User) (auth.User, error) { return u, nil }), FormSessions: authtest.FormSessions{}}
			permissions := currentStartPermissionResolverFunc(func(_ context.Context, _ auth.User, mode, project string) (auth.PermissionResolution, error) {
				if mode != auth.PermissionModeDefault || project != "7" {
					t.Fatal(mode, project)
				}
				p := []string{}
				if spec.permission {
					p = []string{"models.applications.task.get", "models.chat.messages.create"}
				}
				return auth.PermissionResolution{UserID: 11, Permissions: p}, nil
			})
			route, err := NewCurrentNodeRecoveryRoute(useCase, config, permissions)
			if err != nil {
				t.Fatal(err)
			}
			target := "/api/v2/elitea_core/task/prompt_lib/7/10000000-0000-4000-8000-000000000043/node_recovery"
			if spec.method == http.MethodPost {
				target += "/actions"
			}
			if spec.query != "" {
				target += "?" + spec.query
			}
			var req *http.Request
			if spec.method == http.MethodPost {
				req = httptest.NewRequest(spec.method, target, strings.NewReader(spec.body))
			} else {
				req = httptest.NewRequest(spec.method, target, nil)
			}
			authtest.SignIn(req, spec.actor)
			req.RemoteAddr = "10.0.0.8:43120"
			w := httptest.NewRecorder()
			route.ServeHTTP(w, req)
			if w.Code != spec.status || useCase.calls != spec.calls {
				t.Fatalf("code=%d want=%d calls=%d want=%d body=%s", w.Code, spec.status, useCase.calls, spec.calls, w.Body.String())
			}
			if spec.method == http.MethodGet && spec.calls == 1 && useCase.selector.GraphThread != spec.wantThread {
				t.Fatalf("graph_thread=%q want=%q", useCase.selector.GraphThread, spec.wantThread)
			}
		})
	}
}
