package repos

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// A grant answer is ledgered only for the kind its grant covers: hook asks,
// "Continue?", and a browser submit only with browser_submits hosts
// (ADR-0048 decision 8).
func TestGrantCoversExchangeKinds(t *testing.T) {
	none := unattended.Grant{}
	all := unattended.Grant{HookAsks: true, ContinueStepLimit: true, BrowserSubmits: []string{"shop.example.com"}}
	for _, kind := range []string{exchangeKindCommandApproval, exchangeKindContinue, exchangeKindBrowserSubmit} {
		if grantCoversExchange(none, kind) {
			t.Errorf("an empty grant covers %s", kind)
		}
		if !grantCoversExchange(all, kind) {
			t.Errorf("the grant does not cover %s", kind)
		}
	}
	if grantCoversExchange(all, "tool_confirmation") || grantCoversExchange(all, "question") {
		t.Error("only the three kinds are grant answers")
	}
}
