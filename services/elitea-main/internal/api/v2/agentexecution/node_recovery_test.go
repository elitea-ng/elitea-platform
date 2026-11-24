package agentexecution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestCurrentNodeRecoveryRoutePerThreadReadAndAuthorizationMatrix(t *testing.T) {
	childThread := "p1:Z8y5h_dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzxW6Q"
	threadAtLimit := "p1:" + strings.Repeat("a", 512-3)
	threadOverLimit := "p1:" + strings.Repeat("a", 513-3)
	actionReq := domain.Request{RequestID: strings.Repeat("2", 64), ExecutionID: "0123456789abcdef0123456789abcdef", Generation: 1, ActivationID: strings.Repeat("1", 64), ExpectedRevision: 3, Action: "retry"}
	actionBody, _ := json.Marshal(actionReq)

	type authMode int
	const (
		authSession authMode = iota
		authToken
		authNone
	)

	for _, spec := range []struct {
		name        string
		method      string
		project     string
		query       string
		body        string
		actor       string
		auth        authMode
		permission  bool
		err         error
		status      int
		calls       int
		wantThread  string
		wantActorID int64
	}{
		// §3a Authorization Matrix: owner, member, foreign-project, other user without permission, unauthenticated, token vs session
		{name: "owner session read fanout child thread", method: http.MethodGet, project: "7", query: "graph_thread=" + childThread, actor: "11", auth: authSession, permission: true, status: 200, calls: 1, wantThread: childThread, wantActorID: 11},
		{name: "owner token read fanout child thread", method: http.MethodGet, project: "7", query: "graph_thread=" + childThread, actor: "11", auth: authToken, permission: true, status: 200, calls: 1, wantThread: childThread, wantActorID: 11},
		{name: "owner token submit fanout action", method: http.MethodPost, project: "7", body: string(actionBody), actor: "11", auth: authToken, permission: true, status: 202, calls: 1, wantActorID: 11},
		{name: "owner read default without query", method: http.MethodGet, project: "7", actor: "11", auth: authSession, permission: true, status: 200, calls: 1, wantThread: "", wantActorID: 11},
		{name: "project member denied in tx on foreign execution read", method: http.MethodGet, project: "7", query: "graph_thread=" + childThread, actor: "22", auth: authSession, permission: true, err: app.ErrNotAllowed, status: 404, calls: 1, wantThread: childThread, wantActorID: 22},
		{name: "project member token denied in tx on foreign execution submit", method: http.MethodPost, project: "7", body: string(actionBody), actor: "22", auth: authToken, permission: true, err: app.ErrNotAllowed, status: 409, calls: 1, wantActorID: 22},
		{name: "foreign project actor denied permission", method: http.MethodGet, project: "88", query: "graph_thread=" + childThread, actor: "11", auth: authSession, permission: false, status: 403, calls: 0},
		{name: "foreign project actor denied in tx", method: http.MethodGet, project: "88", query: "graph_thread=" + childThread, actor: "11", auth: authToken, permission: true, err: app.ErrNotAllowed, status: 404, calls: 1, wantThread: childThread, wantActorID: 11},
		{name: "other user without permission on fanout child read", method: http.MethodGet, project: "7", query: "graph_thread=" + childThread, actor: "99", auth: authSession, permission: false, status: 403, calls: 0},
		{name: "other user without permission on submit", method: http.MethodPost, project: "7", body: string(actionBody), actor: "99", auth: authToken, permission: false, status: 403, calls: 0},
		{name: "unauthenticated read rejected 401", method: http.MethodGet, project: "7", query: "graph_thread=" + childThread, auth: authNone, permission: true, status: 401, calls: 0},
		{name: "unauthenticated submit rejected 401", method: http.MethodPost, project: "7", body: string(actionBody), auth: authNone, permission: true, status: 401, calls: 0},
		// Query bounds & smuggling guards
		{name: "graph_thread at 512 byte limit accepted", method: http.MethodGet, project: "7", query: "graph_thread=" + threadAtLimit, actor: "11", auth: authSession, permission: true, status: 200, calls: 1, wantThread: threadAtLimit, wantActorID: 11},
		{name: "graph_thread at 513 bytes rejected", method: http.MethodGet, project: "7", query: "graph_thread=" + threadOverLimit, actor: "11", auth: authSession, permission: true, status: 400, calls: 0},
		{name: "oversized raw query over 2048 bytes rejected before parse", method: http.MethodGet, project: "7", query: "graph_thread=" + strings.Repeat("a", 2050), actor: "11", auth: authSession, permission: true, status: 400, calls: 0},
		{name: "unknown query param rejected", method: http.MethodGet, project: "7", query: "child_thread_id=" + childThread, actor: "11", auth: authSession, permission: true, status: 400, calls: 0},
		{name: "empty graph_thread rejected", method: http.MethodGet, project: "7", query: "graph_thread=", actor: "11", auth: authSession, permission: true, status: 400, calls: 0},
		{name: "duplicate graph_thread rejected", method: http.MethodGet, project: "7", query: "graph_thread=" + childThread + "&graph_thread=other", actor: "11", auth: authSession, permission: true, status: 400, calls: 0},
		{name: "trailing ampersand smuggling rejected", method: http.MethodGet, project: "7", query: "graph_thread=" + childThread + "&", actor: "11", auth: authSession, permission: true, status: 400, calls: 0},
		{name: "leading ampersand smuggling rejected", method: http.MethodGet, project: "7", query: "&graph_thread=" + childThread, actor: "11", auth: authSession, permission: true, status: 400, calls: 0},
		{name: "semicolon delimiter smuggling rejected", method: http.MethodGet, project: "7", query: "graph_thread=" + childThread + ";extra=1", actor: "11", auth: authSession, permission: true, status: 400, calls: 0},
		{name: "malformed percent encoding rejected", method: http.MethodGet, project: "7", query: "graph_thread=p1:%ZZbad", actor: "11", auth: authSession, permission: true, status: 400, calls: 0},
		{name: "control char graph_thread rejected", method: http.MethodGet, project: "7", query: "graph_thread=p1:bad%0athread", actor: "11", auth: authSession, permission: true, status: 400, calls: 0},
		{name: "unicode line separator U+2028 rejected", method: http.MethodGet, project: "7", query: "graph_thread=p1:bad%E2%80%A8thread", actor: "11", auth: authSession, permission: true, status: 400, calls: 0},
		{name: "unicode paragraph separator U+2029 rejected", method: http.MethodGet, project: "7", query: "graph_thread=p1:bad%E2%80%A9thread", actor: "11", auth: authSession, permission: true, status: 400, calls: 0},
		{name: "post rejects query param", method: http.MethodPost, project: "7", query: "graph_thread=" + childThread, body: string(actionBody), actor: "11", auth: authSession, permission: true, status: 400, calls: 0},
	} {
		t.Run(spec.name, func(t *testing.T) {
			useCase := &recoveryUseCase{err: spec.err}
			config := apimw.AuthConfig{
				PrincipalValidator: currentStartPrincipalValidatorFunc(func(_ context.Context, u auth.User) (auth.User, error) { return u, nil }),
				FormSessions:       authtest.FormSessions{},
				Validator:          authtest.Tokens{},
			}
			permissions := currentStartPermissionResolverFunc(func(_ context.Context, u auth.User, mode, project string) (auth.PermissionResolution, error) {
				if mode != auth.PermissionModeDefault || project != spec.project {
					t.Fatal(mode, project)
				}
				uid, err := strconv.ParseInt(u.ID, 10, 64)
				if err != nil {
					t.Fatalf("parse user id %q: %v", u.ID, err)
				}
				p := []string{}
				if spec.permission {
					p = []string{"models.applications.task.get", "models.chat.messages.create"}
				}
				return auth.PermissionResolution{UserID: uid, Permissions: p}, nil
			})
			route, err := NewCurrentNodeRecoveryRoute(useCase, config, permissions)
			if err != nil {
				t.Fatal(err)
			}
			target := "/api/v2/elitea_core/task/prompt_lib/" + spec.project + "/10000000-0000-4000-8000-000000000043/node_recovery"
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
			switch spec.auth {
			case authSession:
				authtest.SignIn(req, spec.actor)
			case authToken:
				authtest.BearerToken(req, spec.actor, spec.actor)
			case authNone:
				// unauthenticated request
			}
			req.RemoteAddr = "10.0.0.8:43120"
			w := httptest.NewRecorder()
			route.ServeHTTP(w, req)
			if w.Code != spec.status || useCase.calls != spec.calls {
				t.Fatalf("code=%d want=%d calls=%d want=%d body=%s", w.Code, spec.status, useCase.calls, spec.calls, w.Body.String())
			}
			if spec.method == http.MethodGet && spec.calls == 1 {
				if useCase.selector.GraphThread != spec.wantThread {
					t.Fatalf("graph_thread=%q want=%q", useCase.selector.GraphThread, spec.wantThread)
				}
				if useCase.selector.ActorUserID != spec.wantActorID {
					t.Fatalf("actor=%d want=%d", useCase.selector.ActorUserID, spec.wantActorID)
				}
			}
			if spec.method == http.MethodPost && spec.calls == 1 && useCase.submission.ActorUserID != spec.wantActorID {
				t.Fatalf("submit actor=%d want=%d", useCase.submission.ActorUserID, spec.wantActorID)
			}
		})
	}
}
