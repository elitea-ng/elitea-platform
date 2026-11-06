package repos

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// A grant answer is ledgered only for the kind its grant covers: hook asks,
// "Continue?", and a browser submit only on a host the grant's
// browser_submits names (ADR-0048 decision 8).
func TestGrantCoversExchangeKinds(t *testing.T) {
	none := unattended.Grant{}
	all := unattended.Grant{HookAsks: true, ContinueStepLimit: true, BrowserSubmits: []string{"shop.example.com"}}
	for _, exchange := range []localturn.HITLExchange{
		{Kind: exchangeKindCommandApproval},
		{Kind: exchangeKindContinue},
		{Kind: exchangeKindBrowserSubmit, Host: "shop.example.com"},
	} {
		if grantCoversExchange(none, exchange) {
			t.Errorf("an empty grant covers %s", exchange.Kind)
		}
		if !grantCoversExchange(all, exchange) {
			t.Errorf("the grant does not cover %s", exchange.Kind)
		}
	}
	if grantCoversExchange(all, localturn.HITLExchange{Kind: "tool_confirmation"}) ||
		grantCoversExchange(all, localturn.HITLExchange{Kind: "question"}) {
		t.Error("only the three kinds are grant answers")
	}
}

// Finding 8: the answered host must be one the stored grant names.
func TestGrantCoversBrowserSubmitHost(t *testing.T) {
	grant := unattended.Grant{BrowserSubmits: []string{"shop.example.com", "*.docs.example.org"}}
	for host, want := range map[string]bool{
		"shop.example.com":     true,
		"SHOP.example.com.":    true,
		"pay.shop.example.com": false,
		"evil.example":         false,
		"":                     false,
		"a.docs.example.org":   true,
		"docs.example.org":     false,
		"xdocs.example.org":    false,
	} {
		exchange := localturn.HITLExchange{Kind: exchangeKindBrowserSubmit, Host: host}
		if got := grantCoversExchange(grant, exchange); got != want {
			t.Errorf("host %q: covered=%v, want %v", host, got, want)
		}
	}
}
