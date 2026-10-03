package runtimecomposition

// #289 (PR #1027) widened the two production runtime routes — the execution
// events stream and configuration validation admission — to the /api/v2
// group's bearer and X-API-Key validator. The route-level test in
// internal/api stubs the handler behind them, so it proves only that a PAT now
// AUTHENTICATES. This file proves what the widening must not have changed:
// that a PAT is still AUTHORIZED per project and per capability, through the
// real composition — apimw.Auth, requireRuntimePrincipal, the real executions
// EventHandler and ValidationHandler, and postgresPublicAuthorizer over a real
// PostgreSQL with the legacy RBAC tables. The fixtures are the index-RBAC
// matrix's (index_rbac_postgres_integration_test.go), plus the rows below.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	publicapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	configurationapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	executionapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/executions"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

// patRoutesTokens is what the group's token validator answers for each bearer.
// The identities are what authsvc.LocalValidator produces — except
// "pat-ambiguous", whose ID is ANOTHER user (8, an editor of project 2) while
// its UserID and TokenID name user 4. A producer that put the token row id or
// any other id in ID must not be read as that user: the principal validator
// normalizes ID to the token's owner, and every authorization below must
// follow the owner.
var patRoutesTokens = map[string]auth.User{
	"pat-member":    {ID: "4", UserID: "4", TokenID: "104", AuthType: "token"},
	"pat-admin":     {ID: "3", UserID: "3", TokenID: "103", AuthType: "token"},
	"pat-nonmember": {ID: "10", UserID: "10", TokenID: "110", AuthType: "token"},
	"pat-ambiguous": {ID: "8", UserID: "4", TokenID: "104", AuthType: "token"},
}

type patRoutesTokenValidator struct{}

func (patRoutesTokenValidator) ValidateToken(_ context.Context, token string) (auth.User, error) {
	user, ok := patRoutesTokens[token]
	if !ok {
		return auth.User{}, auth.ErrCredentialRejected
	}
	return user, nil
}

type patRoutesSubmitSpy struct {
	calls    int
	identity executionapp.AdmissionIdentity
}

func (spy *patRoutesSubmitSpy) Submit(
	_ context.Context, request configurationapp.SubmitValidationRequest,
) (executionapp.AdmissionOutcome, error) {
	spy.calls++
	spy.identity = request.Identity
	return executionapp.AdmissionOutcome{ExecutionID: "validation-execution", CommandID: "command", Created: true}, nil
}

func newPATRuntimeRouter(t *testing.T, pool *pgxpool.Pool) (http.Handler, *indexRBACReplaySpy, *patRoutesSubmitSpy) {
	t.Helper()
	resolver := legacyrbac.NewPostgresResolver(pool)
	authorizer, err := newPostgresPublicAuthorizer(sqlcgen.New(pool), sqlcgen.New(pool), resolver)
	if err != nil {
		t.Fatal(err)
	}
	replay := &indexRBACReplaySpy{}
	events, err := executionapi.NewEventHandler(authorizer, replay, indexRBACWaiter{})
	if err != nil {
		t.Fatal(err)
	}
	submitter := &patRoutesSubmitSpy{}
	validation, err := configurationapi.NewValidationHandler(authorizer, submitter)
	if err != nil {
		t.Fatal(err)
	}
	principal := authsvc.NewPrincipalValidator(pool)
	routes, err := publicapi.NewProductionRuntimeRoutes(
		http.HandlerFunc(validation.Submit),
		http.HandlerFunc(events.Stream),
		principal,
		indexRBACPeerVerifier{},
		// The group's own config, as production passes it: the token
		// validator is what #289 added.
		apimw.AuthConfig{Validator: patRoutesTokenValidator{}, PrincipalValidator: principal},
	)
	if err != nil {
		t.Fatal(err)
	}
	return publicapi.NewRouter(publicapi.RouterConfig{ProductionRuntime: routes}), replay, submitter
}

func preparePATRuntimeFixtures(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	prepareIndexRBACFixtures(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
-- The membership query validation admission asks reads the project owner.
ALTER TABLE centry.project ADD COLUMN owner_id BIGINT;

-- PATs for the project admin (3) and the non-member (10).
INSERT INTO public.auth_core__token (id, user_id, expires) VALUES
    (103, 3, (clock_timestamp() AT TIME ZONE 'UTC') + interval '1 hour'),
    (110, 10, (clock_timestamp() AT TIME ZONE 'UTC') + interval '1 hour');

-- The chat permission an AGENT execution's stream requires: the project admin
-- role holds it, the editor role (user 4) does not.
INSERT INTO public.auth_core__role_permission (id, role_id, permission) VALUES
    (190, 10, 'models.chat.messages.create');

INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, tenant_id, resource_project_id, projection_project_id,
    capability_id, desired_state, state
) VALUES
    ('agent-project-1', 1, '1', 1, 1, '`+executiondomain.AgentApplicationCapability+`', 'RUNNING', 'RUNNING');
`); err != nil {
		t.Fatal(err)
	}
}

func TestProductionRuntimeRoutesAuthorizeAPersonalAccessTokenPerProjectAndCapability(t *testing.T) {
	pool := newIndexRBACPostgresPool(t)
	preparePATRuntimeFixtures(t, pool)

	t.Run("events", func(t *testing.T) {
		for _, test := range []struct {
			name, token, path string
			want              int
		}{
			{"a member's PAT reads its project's execution", "pat-member",
				"/api/v2/executions/1/execution-project-1/events", http.StatusOK},
			{"a non-member's PAT is refused", "pat-nonmember",
				"/api/v2/executions/1/execution-project-1/events", http.StatusForbidden},
			{"a member's PAT on another project's execution is refused", "pat-member",
				"/api/v2/executions/2/execution-project-2/events", http.StatusForbidden},
			{"another project's execution id under the member's project is refused", "pat-member",
				"/api/v2/executions/1/execution-project-2/events", http.StatusForbidden},
			{"a member without models.chat.messages.create cannot read an agent run", "pat-member",
				"/api/v2/executions/1/agent-project-1/events", http.StatusForbidden},
			{"a member with models.chat.messages.create can", "pat-admin",
				"/api/v2/executions/1/agent-project-1/events", http.StatusOK},
			{"a token principal is authorized as its OWNER, not as its ID", "pat-ambiguous",
				"/api/v2/executions/2/execution-project-2/events", http.StatusForbidden},
			{"the owner's own project still works for that principal", "pat-ambiguous",
				"/api/v2/executions/1/execution-project-1/events", http.StatusOK},
		} {
			t.Run(test.name, func(t *testing.T) {
				router, replay, _ := newPATRuntimeRouter(t, pool)
				for _, carrier := range []string{"Authorization", "X-API-Key"} {
					replay.calls = 0
					request := httptest.NewRequest(http.MethodGet, test.path, nil)
					if carrier == "Authorization" {
						request.Header.Set(carrier, "Bearer "+test.token)
					} else {
						request.Header.Set(carrier, test.token)
					}
					response := newIndexRBACStreamingRecorder()
					router.ServeHTTP(response, request)
					if response.Code != test.want {
						t.Fatalf("%s: status = %d, want %d; body = %s",
							carrier, response.Code, test.want, response.Body.String())
					}
					wantReplay := 0
					if test.want == http.StatusOK {
						wantReplay = 1
					}
					// A refusal reads NOTHING from the replay store: no event
					// byte can have been written for it.
					if replay.calls != wantReplay {
						t.Fatalf("%s: replay calls = %d, want %d", carrier, replay.calls, wantReplay)
					}
					if test.want != http.StatusOK && strings.Contains(response.Body.String(), "data:") {
						t.Fatalf("%s: a refused stream carried event bytes: %s", carrier, response.Body.String())
					}
				}
			})
		}
	})

	t.Run("configuration validation", func(t *testing.T) {
		for _, test := range []struct {
			name, token, project string
			want                 int
			actor                string
		}{
			{"a member's PAT admits a validation", "pat-member", "1", http.StatusAccepted, "4"},
			{"a non-member's PAT is refused", "pat-nonmember", "1", http.StatusForbidden, ""},
			{"a member's PAT on another project is refused", "pat-member", "2", http.StatusForbidden, ""},
			{"a token principal is admitted as its OWNER, not as its ID", "pat-ambiguous", "2", http.StatusForbidden, ""},
			{"and its admission identity is the owner", "pat-ambiguous", "1", http.StatusAccepted, "4"},
		} {
			t.Run(test.name, func(t *testing.T) {
				router, _, submitter := newPATRuntimeRouter(t, pool)
				request := httptest.NewRequest(http.MethodPost,
					"/api/v2/configurations/validation/"+test.project+"/revision-1",
					strings.NewReader(`{"settings":{}}`))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer "+test.token)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != test.want {
					t.Fatalf("status = %d, want %d; body = %s", response.Code, test.want, response.Body.String())
				}
				if test.want != http.StatusAccepted {
					if submitter.calls != 0 {
						t.Fatalf("a refused validation reached the submitter %d time(s)", submitter.calls)
					}
					return
				}
				if submitter.calls != 1 || submitter.identity.ActorID != test.actor ||
					submitter.identity.TenantID != test.project {
					t.Fatalf("admission identity = %+v (calls %d), want actor %s in project %s",
						submitter.identity, submitter.calls, test.actor, test.project)
				}
			})
		}
	})
}
