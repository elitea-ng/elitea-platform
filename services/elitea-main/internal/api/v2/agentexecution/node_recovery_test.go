package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/noderecovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
)

type recoveryUseCase struct {
	calls      int
	submission app.Submission
	err        error
}

func (s *recoveryUseCase) Read(context.Context, app.Selector) (app.State, error) {
	s.calls++
	return app.State{}, s.err
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
			config := apimw.AuthConfig{PrincipalValidator: currentStartPrincipalValidatorFunc(func(_ context.Context, u auth.User) (auth.User, error) { return u, nil }), ForwardedIdentityVerifier: currentStartPeerVerifierFunc(func(r *http.Request) error {
				if r.RemoteAddr != "10.0.0.8:43120" {
					return errors.New("peer denied")
				}
				return nil
			})}
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
			req.Header.Set("X-Auth-Type", "user")
			req.Header.Set("X-Auth-ID", "11")
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
