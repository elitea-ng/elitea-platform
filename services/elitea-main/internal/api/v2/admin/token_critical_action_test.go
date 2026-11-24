package admin_test

// The handler half of the token rule (ADR-0059 R1.2b): the identity provider
// save classifies the request, then applies the rule to the entry it
// classified, with the token caller the middleware of internal/api recorded
// (internal/api/token_critical_action_route_test.go holds the middleware).

import (
	"net/http"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// asToken serves router for a personal access token caller.
func asToken(router http.Handler, caller adminaccess.TokenCaller) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r.WithContext(adminaccess.ContextWithTokenCaller(r.Context(), caller)))
	})
}

// An inherit token of a standing global administrator changes a provider's
// metadata address (the OIDC issuer, which is the discovery address: a trust
// action); an inherit token of an admin who holds the trust permission, and a
// scoped token, are refused it with the token code; the admin still renames.
func TestAnInheritTokenChangesTrustOnlyForAStandingGlobalAdministrator(t *testing.T) {
	env := newSplitEnvironment(t)
	const target = "/identity_providers/administration/corporate"
	issuer := func() string { return env.providers.rows["corporate"].OIDC.Issuer }

	admin := asToken(env.as(append([]string{adminaccess.IdentitySSOTrustManage}, plainAdministrator...)),
		adminaccess.TokenCaller{Access: adminaccess.TokenAccessInherit})
	recorder := splitDo(admin, http.MethodPut, target, oidcBody("Corporate SSO", "https://idp2.example.com"))
	if got := refusal(t, recorder); got != adminaccess.CodeTokenCriticalAction {
		t.Fatalf("inherit admin metadata change = %d %s, want the token refusal", recorder.Code, recorder.Body)
	}
	scoped := asToken(env.as(globalAdministrator),
		adminaccess.TokenCaller{Access: adminaccess.TokenAccessScoped, StandingGlobalAdmin: true})
	recorder = splitDo(scoped, http.MethodPut, target, oidcBody("Corporate SSO", "https://idp2.example.com"))
	if got := refusal(t, recorder); got != adminaccess.CodeTokenCriticalAction {
		t.Fatalf("scoped token metadata change = %d %s, want the token refusal", recorder.Code, recorder.Body)
	}
	if got := issuer(); got != "https://idp.example.com" {
		t.Fatalf("a refused save wrote the issuer %q", got)
	}

	super := asToken(env.as(globalAdministrator),
		adminaccess.TokenCaller{Access: adminaccess.TokenAccessInherit, StandingGlobalAdmin: true})
	if recorder := splitDo(super, http.MethodPut, target,
		oidcBody("Corporate SSO", "https://idp2.example.com")); recorder.Code != http.StatusOK {
		t.Fatalf("inherit super_admin metadata change = %d %s, want 200", recorder.Code, recorder.Body)
	}
	if got := issuer(); got != "https://idp2.example.com" {
		t.Fatalf("issuer = %q after the save", got)
	}

	// A rename is a settings save: open to the admin's inherit token.
	if recorder := splitDo(admin, http.MethodPut, target,
		oidcBody("Renamed", "https://idp2.example.com")); recorder.Code != http.StatusOK {
		t.Fatalf("inherit admin rename = %d %s, want 200", recorder.Code, recorder.Body)
	}
}
