package adminaccess_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// Every critical entry has a token value, explicit or defaulted, and an entry
// that says allowed has a written reason (ADR-0059 decision 7, R1.2b).
func TestEveryCriticalEntryHasATokenRule(t *testing.T) {
	critical := adminaccess.CriticalEntries()
	if len(critical) == 0 {
		t.Fatal("the action table has no critical entry")
	}
	for _, entry := range critical {
		switch entry.Token {
		case adminaccess.TokenNever:
			if entry.TokenReason != "" {
				t.Errorf("%s says never and carries a token reason", entry.Key())
			}
		case adminaccess.TokenAllowed:
			if strings.TrimSpace(entry.TokenReason) == "" {
				t.Errorf("%s says allowed without a written reason", entry.Key())
			}
		default:
			t.Errorf("%s has no token rule (%q)", entry.Key(), entry.Token)
		}
	}
}

// The exception is the identity trust repair path, and nothing else.
func TestOnlyTheIdentityTrustActionsAllowAToken(t *testing.T) {
	for _, entry := range adminaccess.CriticalEntries() {
		if entry.Token != adminaccess.TokenAllowed {
			continue
		}
		if entry.Permission != adminaccess.IdentitySSOTrustManage {
			t.Errorf("%s allows a token but is not an identity trust action (%s)", entry.Key(), entry.Permission)
		}
	}
	entry, ok := adminaccess.ActionFor(http.MethodPost, adminaccess.AuthUsersActionPattern, "set_admin_role")
	if !ok || entry.Token != adminaccess.TokenNever {
		t.Fatalf("set_admin_role = %+v, %v; want token: never", entry, ok)
	}
}

func TestTheTokenRule(t *testing.T) {
	never, _ := adminaccess.ActionFor(http.MethodPost, adminaccess.AuthUsersActionPattern, "set_admin_role")
	plain, _ := adminaccess.ActionFor(http.MethodPost, adminaccess.AuthUsersActionPattern, "delete")
	trust, _ := adminaccess.ActionFor(http.MethodPut, adminaccess.IdentityProviderSavePattern, adminaccess.OutcomeTrust)
	settings, _ := adminaccess.ActionFor(http.MethodPut, adminaccess.IdentityProviderSavePattern, adminaccess.OutcomeSettings)
	inheritSuper := adminaccess.TokenCaller{Access: adminaccess.TokenAccessInherit, StandingGlobalAdmin: true}
	inheritAdmin := adminaccess.TokenCaller{Access: adminaccess.TokenAccessInherit}
	scopedSuper := adminaccess.TokenCaller{Access: adminaccess.TokenAccessScoped, StandingGlobalAdmin: true}
	none := adminaccess.TokenCaller{Access: adminaccess.TokenAccessNone}

	for _, tc := range []struct {
		name    string
		entry   adminaccess.Action
		caller  adminaccess.TokenCaller
		refused bool
	}{
		{"never refuses inherit", never, inheritSuper, true},
		{"never refuses scoped", never, scopedSuper, true},
		{"none is the gate's", never, none, false},
		{"a dangerous action is open", plain, inheritAdmin, false},
		{"trust: inherit of a standing global administrator", trust, inheritSuper, false},
		{"trust: inherit of an admin", trust, inheritAdmin, true},
		{"trust: scoped, even of a global administrator", trust, scopedSuper, true},
		{"settings is open", settings, inheritAdmin, false},
	} {
		refusal := adminaccess.TokenRefusal(tc.entry, tc.caller)
		if (refusal != nil) != tc.refused {
			t.Errorf("%s: refusal = %+v, want refused %v", tc.name, refusal, tc.refused)
		}
		if refusal != nil && refusal.Code != adminaccess.CodeTokenCriticalAction {
			t.Errorf("%s: code = %q", tc.name, refusal.Code)
		}
	}
}

func TestTheHandlerHalfReadsTheCallerTheMiddlewareRecorded(t *testing.T) {
	ctx := context.Background()
	if refusal := adminaccess.CheckTokenRule(ctx, http.MethodPut, adminaccess.IdentityProviderSavePattern,
		adminaccess.OutcomeTrust); refusal != nil {
		t.Fatalf("a request with no token caller was refused: %+v", refusal)
	}
	ctx = adminaccess.ContextWithTokenCaller(ctx, adminaccess.TokenCaller{Access: adminaccess.TokenAccessInherit})
	if adminaccess.CheckTokenRule(ctx, http.MethodPut, adminaccess.IdentityProviderSavePattern,
		adminaccess.OutcomeTrust) == nil {
		t.Fatal("an inherit token of an admin reached a trust action")
	}
	if refusal := adminaccess.CheckTokenRule(ctx, http.MethodPut, adminaccess.IdentityProviderSavePattern,
		adminaccess.OutcomeSettings); refusal != nil {
		t.Fatalf("a settings save was refused: %+v", refusal)
	}
	if adminaccess.CheckTokenRule(ctx, http.MethodPut, "/api/v2/admin/nowhere", "") == nil {
		t.Fatal("an entry the table does not have was not refused")
	}
	for _, classifier := range []adminaccess.Classifier{adminaccess.ClassifyIdentityProvider,
		adminaccess.ClassifyGovernanceRow, adminaccess.ClassifyGlobalProvider, adminaccess.ClassifyRetention} {
		if !adminaccess.TokenRuleInHandler[classifier] {
			t.Fatalf("the handler of %s no longer applies the rule itself", classifier)
		}
	}
	// Each route the handlers name has the entries they look up.
	for _, tc := range []struct{ method, pattern, action string }{
		{http.MethodPost, adminaccess.GovernanceCreatePattern, adminaccess.OutcomeGuardrail},
		{http.MethodPut, adminaccess.GovernanceRowPattern, adminaccess.OutcomeOther},
		{http.MethodDelete, adminaccess.GovernanceRowPattern, adminaccess.OutcomeGuardrail},
		{http.MethodPut, adminaccess.GlobalProviderUpdatePattern, adminaccess.OutcomeSettings},
		{http.MethodPut, adminaccess.RetentionPolicyPattern, adminaccess.OutcomeLengthen},
		{http.MethodPut, adminaccess.RetentionProjectPolicyPattern, adminaccess.OutcomeShorten},
		{http.MethodDelete, adminaccess.RetentionProjectPolicyPattern, adminaccess.OutcomeLengthen},
	} {
		if _, found := adminaccess.ActionFor(tc.method, tc.pattern, tc.action); !found {
			t.Errorf("no entry %s %s %s", tc.method, tc.pattern, tc.action)
		}
	}
}
