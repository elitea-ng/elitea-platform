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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	v2retention "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/retention"
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

// An inherit token is refused set_admin_role, and reaches the same route's
// non-critical action (delete: the route carries no other; a user suspend is
// PUT /admin/user_suspend, not on this route); the body the rule read still
// reaches the handler.
func TestAnInheritTokenIsRefusedSetAdminRoleOnly(t *testing.T) {
	router := newTokenRuleRouter(t, auth.CredentialKindToken, adminaccess.TokenAccessInherit, tokenRuleSuperAdmin)
	if status, code := tokenRuleAnswer(t, router, http.MethodPost, tokenRuleUsersPath,
		`{"action":"set_admin_role","user_ids":[2],"role":"admin"}`); status != http.StatusForbidden ||
		code != adminaccess.CodeTokenCriticalAction {
		t.Errorf("set_admin_role = %d %q, want 403 %s", status, code, adminaccess.CodeTokenCriticalAction)
	}
	for _, action := range []string{"delete"} {
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

// The sealed credential list (ADR-0059 R2.17) says `token: never`: a scoped
// or inherit token is refused by the token rule, whatever it holds. The
// access posture (R2.14a) is an ordinary read: a scoped token that holds
// access.roles.view passes the rule. Without a database the handlers answer
// what they answer; what is held here is the rule's refusal.
func TestNoTokenReadsTheSealedCredentialListAndAScopedTokenReadsThePosture(t *testing.T) {
	const sealed = "/api/v2/admin/access/sealed_credentials"
	granted := append([]string{adminaccess.AccessBreakglassManage, adminaccess.AccessRolesView}, tokenRuleSuperAdmin...)
	for _, access := range []string{adminaccess.TokenAccessScoped, adminaccess.TokenAccessInherit} {
		router := newTokenRuleRouter(t, auth.CredentialKindToken, access, granted)
		if status, code := tokenRuleAnswer(t, router, http.MethodGet, sealed, ""); status != http.StatusForbidden ||
			code != adminaccess.CodeTokenCriticalAction {
			t.Errorf("%s token: %d %q, want 403 %s", access, status, code, adminaccess.CodeTokenCriticalAction)
		}
	}
	session := newTokenRuleRouter(t, auth.CredentialKindNone, "", granted)
	if status, code := tokenRuleAnswer(t, session, http.MethodGet, sealed, ""); status == http.StatusForbidden {
		t.Errorf("a session that holds access.breakglass.manage: %d %q, want it past the gates", status, code)
	}
	scoped := newTokenRuleRouter(t, auth.CredentialKindToken, adminaccess.TokenAccessScoped,
		[]string{adminaccess.AccessRolesView})
	if status, code := tokenRuleAnswer(t, scoped, http.MethodGet, "/api/v2/admin/access/posture", ""); status == http.StatusForbidden {
		t.Errorf("a scoped token with access.roles.view on the posture: %d %q, want it past the gates", status, code)
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

// The gateway governance rows, the global provider update and the retention
// writes are decided in their handlers (issue 2993): the middleware lets an
// inherit token through with the token caller on the context, so the handler
// refuses the critical outcome and nothing else.
func TestTheClassifiedRoutesAreDecidedInTheirHandlers(t *testing.T) {
	gate := &tokenCriticalActions{resolver: tokenRuleResolver{granted: tokenRuleSuperAdmin,
		access: adminaccess.TokenAccessInherit}}
	user := authenticatedTestUser()
	user.CredentialKind, user.TokenID = auth.CredentialKindToken, "5"
	gate.mark(NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: user},
		PrincipalValidator: testPrincipalValidator{},
		PermissionResolver: tokenRuleResolver{access: adminaccess.TokenAccessInherit},
		RetentionAdmin:     v2retention.NewHandler(nil),
	}))
	for _, tc := range []struct {
		method, path, pattern, critical string
	}{
		{http.MethodPost, "/api/v2/admin/gateway/governance", adminaccess.GovernanceCreatePattern, adminaccess.OutcomeGuardrail},
		{http.MethodPut, "/api/v2/admin/gateway/governance/id1", adminaccess.GovernanceRowPattern, adminaccess.OutcomeGuardrail},
		{http.MethodDelete, "/api/v2/admin/gateway/governance/id1", adminaccess.GovernanceRowPattern, adminaccess.OutcomeGuardrail},
		{http.MethodPut, "/api/v2/admin/gateway/providers/7", adminaccess.GlobalProviderUpdatePattern, adminaccess.OutcomeRoute},
		{http.MethodPut, "/api/v2/admin/retention/policies/conversation", adminaccess.RetentionPolicyPattern, adminaccess.OutcomeShorten},
		{http.MethodPut, "/api/v2/admin/retention/policies/conversation/projects/1",
			adminaccess.RetentionProjectPolicyPattern, adminaccess.OutcomeShorten},
		{http.MethodDelete, "/api/v2/admin/retention/policies/conversation/projects/1",
			adminaccess.RetentionProjectPolicyPattern, adminaccess.OutcomeShorten},
	} {
		reached, holdsCaller := false, false
		handler := gate.middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			reached = true
			holdsCaller = adminaccess.TokenRefused(r.Context(), tc.method, tc.pattern, tc.critical)
		}))
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "1", TokenID: "5",
			CredentialKind: auth.CredentialKindToken}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if !reached || !holdsCaller {
			t.Errorf("%s %s = %d (reached %v, caller on the context %v), want the handler to decide",
				tc.method, tc.path, rec.Code, reached, holdsCaller)
		}
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

// The selector is read the way the handler's struct decode reads it, and a
// value the rule cannot read unambiguously refuses the token: a key in
// another case, two spellings, a padded body, a body that is not one object,
// an unknown action.
func TestATokenCannotHideTheActionFromTheRule(t *testing.T) {
	router := newTokenRuleRouter(t, auth.CredentialKindToken, adminaccess.TokenAccessInherit, tokenRuleSuperAdmin)
	padded := `{"action":"set_admin_role","user_ids":[2],"role":"admin"}` + strings.Repeat(" ", maxTokenRuleBody+1)
	for name, body := range map[string]string{
		"another case":   `{"Action":"set_admin_role","user_ids":[2],"role":"admin"}`,
		"upper case":     `{"ACTION":"set_admin_role","user_ids":[2],"role":"admin"}`,
		"two spellings":  `{"action":"delete","Action":"set_admin_role","user_ids":[2]}`,
		"padded":         padded,
		"two objects":    `{"action":"delete"} {"action":"set_admin_role"}`,
		"unknown action": `{"action":"set_admin_role_v2","user_ids":[2]}`,
		"no action":      `{"user_ids":[2]}`,
	} {
		if status, code := tokenRuleAnswer(t, router, http.MethodPost, tokenRuleUsersPath, body); code != adminaccess.CodeTokenCriticalAction {
			t.Errorf("%s = %d %q, want the token refusal", name, status, code)
		}
	}
}

// The body the rule read reaches the handler whole, past the bound too.
func TestTheRulePutsTheWholeBodyBack(t *testing.T) {
	for _, body := range []string{`{"action":"delete"}`, `{"action":"delete"}` + strings.Repeat(" ", maxTokenRuleBody+10)} {
		req := httptest.NewRequest(http.MethodPost, tokenRuleUsersPath, strings.NewReader(body))
		_, restored := bodyField(req, "action")
		got, err := io.ReadAll(restored.Body)
		if err != nil || string(got) != body {
			t.Fatalf("restored %d bytes (%v), want %d", len(got), err, len(body))
		}
	}
}
