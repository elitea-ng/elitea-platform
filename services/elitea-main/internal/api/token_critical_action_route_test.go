package api

// The token rule of the action table on the production router (ADR-0059
// R1.2b, issue 2592). Every request goes through NewRouter with no database,
// a validator that authenticates a credential of the kind under test and a
// resolver that grants exactly the holder's names, with the token row's
// admin_access. Without a database a request the rule lets through gets the
// handler's answer (often 503): what this file holds is whether the rule
// refused it, by its code.
//
// The identity provider save is decided in its handler after classifying;
// internal/api/v2/admin/token_critical_action_test.go proves the 200 of an
// inherit token of a standing global administrator changing the metadata
// address, and the 403 of an admin's.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// tokenRuleResolver grants granted in every mode and reports the token row's
// admin_access, as the administration-mode resolution does.
type tokenRuleResolver struct {
	granted []string
	access  string
}

func (p tokenRuleResolver) ResolvePermissions(
	context.Context, auth.User, string, string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 1, Permissions: append([]string(nil), p.granted...),
		TokenAdminAccess: p.access}, nil
}

func (p tokenRuleResolver) ResolveMembershipPermissions(
	ctx context.Context, user auth.User, mode string,
) (auth.PermissionResolution, error) {
	return p.ResolvePermissions(ctx, user, mode, "")
}

var (
	tokenRuleAdmin = []string{
		adminaccess.AdminAuthUsers, adminaccess.RuntimePlugins,
		adminaccess.IdentitySSOView, adminaccess.IdentitySSOSettingsManage,
	}
	tokenRuleSuperAdmin = append([]string{adminaccess.SuperAdminPermission, adminaccess.IdentitySSOTrustManage},
		tokenRuleAdmin...)
)

func newTokenRuleRouter(t *testing.T, kind auth.CredentialKind, access string, granted []string) chi.Router {
	t.Helper()
	user := authenticatedTestUser()
	user.CredentialKind = kind
	if kind == auth.CredentialKindToken || kind == auth.CredentialKindSystem {
		user.TokenID = "5"
	}
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: user},
		PrincipalValidator: testPrincipalValidator{},
		PermissionResolver: tokenRuleResolver{granted: granted, access: access},
	})
}

// tokenRuleAnswer is the status, and the refusal code of a 403 ("" for the
// route gate's bare refusal).
func tokenRuleAnswer(t *testing.T, router http.Handler, method, path, body string) (int, string) {
	t.Helper()
	req := testAuthHeader(httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body)))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var refusal struct {
		Code string `json:"code"`
	}
	if rec.Code == http.StatusForbidden {
		_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
	}
	return rec.Code, refusal.Code
}

const tokenRuleUsersPath = "/api/v2/admin/auth_users/administration"

// An inherit token of an admin is refused set_admin_role, and reaches the
// same route's non-critical actions; the body the rule read still reaches the
// handler.
func TestAnInheritTokenIsRefusedSetAdminRoleOnly(t *testing.T) {
	router := newTokenRuleRouter(t, auth.CredentialKindToken, adminaccess.TokenAccessInherit, tokenRuleSuperAdmin)
	if status, code := tokenRuleAnswer(t, router, http.MethodPost, tokenRuleUsersPath,
		`{"action":"set_admin_role","user_ids":[2],"role":"admin"}`); status != http.StatusForbidden ||
		code != adminaccess.CodeTokenCriticalAction {
		t.Errorf("set_admin_role = %d %q, want 403 %s", status, code, adminaccess.CodeTokenCriticalAction)
	}
	for _, action := range []string{"delete", "user_suspend"} {
		status, code := tokenRuleAnswer(t, router, http.MethodPost, tokenRuleUsersPath,
			`{"action":"`+action+`","user_ids":[2]}`)
		if code == adminaccess.CodeTokenCriticalAction {
			t.Errorf("%s = %d, refused by the token rule", action, status)
		}
		if status == http.StatusBadRequest {
			t.Errorf("%s = 400: the handler did not get the body the rule read", action)
		}
	}
}

// A scoped token is refused every critical action, and an inherit token every
// critical action outside the identity trust exception.
func TestAScopedTokenIsRefusedCriticalActions(t *testing.T) {
	scoped := newTokenRuleRouter(t, auth.CredentialKindToken, adminaccess.TokenAccessScoped, tokenRuleSuperAdmin)
	inherit := newTokenRuleRouter(t, auth.CredentialKindToken, adminaccess.TokenAccessInherit, tokenRuleSuperAdmin)
	for _, tc := range []struct {
		method, path, body string
		inheritRefused     bool
	}{
		{http.MethodPost, tokenRuleUsersPath, `{"action":"set_admin_role","user_ids":[2],"role":"admin"}`, true},
		{http.MethodPost, "/api/v2/admin/modes/administration", `{"user_ids":[2]}`, true},
		{http.MethodDelete, "/api/v2/admin/identity_providers/administration/corporate", "", false},
		{http.MethodPost, "/api/v2/admin/scim_clients/administration", `{"name":"entra","auth_method":"bearer"}`, false},
	} {
		if status, code := tokenRuleAnswer(t, scoped, tc.method, tc.path, tc.body); code != adminaccess.CodeTokenCriticalAction {
			t.Errorf("scoped %s %s = %d %q, want the token refusal", tc.method, tc.path, status, code)
		}
		status, code := tokenRuleAnswer(t, inherit, tc.method, tc.path, tc.body)
		if refused := code == adminaccess.CodeTokenCriticalAction; refused != tc.inheritRefused {
			t.Errorf("inherit super_admin %s %s = %d %q, refused %v, want %v",
				tc.method, tc.path, status, code, refused, tc.inheritRefused)
		}
	}
}

// The identity trust exception is for a standing global administrator only.
func TestTheTrustExceptionNeedsAStandingGlobalAdministrator(t *testing.T) {
	admin := newTokenRuleRouter(t, auth.CredentialKindToken, adminaccess.TokenAccessInherit,
		append([]string{adminaccess.IdentitySSOTrustManage}, tokenRuleAdmin...))
	if status, code := tokenRuleAnswer(t, admin, http.MethodDelete,
		"/api/v2/admin/identity_providers/administration/corporate", ""); code != adminaccess.CodeTokenCriticalAction {
		t.Errorf("inherit admin delete = %d %q, want the token refusal", status, code)
	}
	// The save is classified in the handler: the rule lets it through to it.
	if _, code := tokenRuleAnswer(t, admin, http.MethodPut,
		"/api/v2/admin/identity_providers/administration/corporate", identityOIDCBody); code == adminaccess.CodeTokenCriticalAction {
		t.Error("the middleware refused a save its handler classifies")
	}
}

// A browser session, a native session, a run token and the system token are
// not personal access tokens: the rule never refuses them.
func TestOtherCredentialsAreNotTokens(t *testing.T) {
	for _, kind := range []auth.CredentialKind{auth.CredentialKindNone, auth.CredentialKindSystem} {
		router := newTokenRuleRouter(t, kind, adminaccess.TokenAccessInherit, tokenRuleSuperAdmin)
		if status, code := tokenRuleAnswer(t, router, http.MethodPost, tokenRuleUsersPath,
			`{"action":"set_admin_role","user_ids":[2],"role":"admin"}`); code == adminaccess.CodeTokenCriticalAction {
			t.Errorf("%q set_admin_role = %d, refused by the token rule", kind, status)
		}
	}
	// The kinds the bearer path cannot carry in this harness, on the
	// middleware itself.
	gate := &tokenCriticalActions{resolver: tokenRuleResolver{granted: tokenRuleSuperAdmin,
		access: adminaccess.TokenAccessInherit}}
	gate.mark(newTokenRuleRouter(t, auth.CredentialKindToken, adminaccess.TokenAccessInherit, nil))
	reached := 0
	handler := gate.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))
	for _, kind := range []auth.CredentialKind{auth.CredentialKindSession, auth.CredentialKindForm,
		auth.CredentialKindSealed, auth.CredentialKindNative, auth.CredentialKindRun, auth.CredentialKindSystem} {
		req := httptest.NewRequest(http.MethodPost, tokenRuleUsersPath,
			strings.NewReader(`{"action":"set_admin_role"}`))
		req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "1", CredentialKind: kind}))
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	if reached != 6 {
		t.Fatalf("the rule let %d of 6 non-token credentials through", reached)
	}
	// And the same middleware refuses a token, so the loop above measured it.
	req := httptest.NewRequest(http.MethodPost, tokenRuleUsersPath, strings.NewReader(`{"action":"set_admin_role"}`))
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "1", TokenID: "5",
		CredentialKind: auth.CredentialKindToken}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || reached != 6 {
		t.Fatalf("a token = %d (reached %d), want 403", rec.Code, reached)
	}
}

// A token whose access cannot be read is refused: the closed failure.
func TestATokenWhoseAccessCannotBeReadIsRefused(t *testing.T) {
	gate := &tokenCriticalActions{}
	gate.mark(newTokenRuleRouter(t, auth.CredentialKindToken, adminaccess.TokenAccessInherit, nil))
	handler := gate.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("reached the handler")
	}))
	req := httptest.NewRequest(http.MethodPost, tokenRuleUsersPath, strings.NewReader(`{"action":"set_admin_role"}`))
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "1", TokenID: "5",
		CredentialKind: auth.CredentialKindToken}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
