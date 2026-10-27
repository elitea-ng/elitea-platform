package credentialbroker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
)

const tProviderToken = "atlassian-access-token-value"

type fakeProviderTokens struct {
	token provideroauth.AccessToken
	err   error
	keys  []provideroauth.GrantKey
}

func (f *fakeProviderTokens) AccessToken(_ context.Context, key provideroauth.GrantKey) (provideroauth.AccessToken, error) {
	f.keys = append(f.keys, key)
	return f.token, f.err
}

// providerWorld is a delegated_background run whose Jira credential is
// oauth_delegated with the provider as the authority (Atlassian 3LO).
func providerWorld() (*world, *fakeProviderTokens) {
	w := newWorld().withMandate(mandate.KindDelegatedBackground)
	ref := w.references.refs[tReference]
	ref.Class = credentialgrant.ClassOAuthDelegated
	w.references.refs[tReference] = ref
	w.catalog.authority = map[string]credentialgrant.OAuthAuthority{"jira": credentialgrant.OAuthAuthorityProvider}
	tokens := &fakeProviderTokens{token: provideroauth.AccessToken{
		Token: tProviderToken, ExpiresAt: tNow.Add(3 * time.Minute), GrantID: "g", Generation: 2,
	}}
	return w, tokens
}

// Acceptance (#1121): a provider-OAuth use reads the token source, never
// the Entra exchange, for the grant of the run's own user.
func TestProviderOAuthUseRoutesToTheTokenSource(t *testing.T) {
	w, tokens := providerWorld()
	b := w.broker(t, func(c *Config) { c.ProviderTokens = tokens })
	result := redeem(t, b, w.request(t))
	if result.Envelope == nil {
		t.Fatalf("denied: %+v", result.Denial)
	}
	defer result.Envelope.Destroy()
	if result.Envelope.Kind != KindOAuthAccess || string(result.Envelope.Material["access_token"]) != tProviderToken ||
		len(result.Envelope.Material) != 1 || !result.Envelope.NotAfter.Equal(tNow.Add(3*time.Minute)) {
		t.Fatalf("envelope kind=%s not_after=%s keys=%d", result.Envelope.Kind, result.Envelope.NotAfter, len(result.Envelope.Material))
	}
	want := provideroauth.GrantKey{
		TenantProjectID: tTenant, Provider: "jira", TargetBindingID: tTargetA, SubjectUserID: tUser, AgentBindingID: tBinding,
	}
	if len(tokens.keys) != 1 || tokens.keys[0] != want {
		t.Fatalf("token source keys = %+v, want %+v", tokens.keys, want)
	}
	if w.exchanger.last.DelegationID != "" || len(w.custody.requirements) != 0 || w.secrets.readCount() != 0 {
		t.Fatal("a provider-OAuth use reached the Entra exchange or a secret store")
	}
}

// A token that outlives the policy maximum is still bounded by it.
func TestProviderOAuthEnvelopeIsBoundedByTheMaximumTTL(t *testing.T) {
	w, tokens := providerWorld()
	tokens.token.ExpiresAt = time.Time{}
	result := redeem(t, w.broker(t, func(c *Config) { c.ProviderTokens = tokens }), w.request(t))
	if result.Envelope == nil {
		t.Fatalf("denied: %+v", result.Denial)
	}
	defer result.Envelope.Destroy()
	if !result.Envelope.NotAfter.Equal(tNow.Add(MaxEnvelopeTTL)) {
		t.Fatalf("not_after = %s", result.Envelope.NotAfter)
	}
}

func TestProviderOAuthDenials(t *testing.T) {
	cases := map[string]struct {
		err    error
		code   DenialCode
		reason string
	}{
		"no consent":         {provideroauth.ErrConsentRequired, CodeDenied, "provider_consent_required"},
		"not configured":     {provideroauth.ErrProviderNotConfigured, CodeBackendUnavailable, "provider_oauth_not_configured"},
		"provider down":      {provideroauth.ErrProviderUnavailable, CodeBackendUnavailable, "provider_oauth_unavailable"},
		"refresh contention": {errors.New("refresh: waited too long"), CodeBackendUnavailable, "provider_oauth_unavailable"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w, tokens := providerWorld()
			tokens.err = tc.err
			denial := requireDenial(t, redeem(t, w.broker(t, func(c *Config) { c.ProviderTokens = tokens }), w.request(t)), tc.code)
			if denial.reason != tc.reason {
				t.Fatalf("reason %q, want %q", denial.reason, tc.reason)
			}
		})
	}
	t.Run("no token source composed", func(t *testing.T) {
		w, _ := providerWorld()
		denial := requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeBackendUnavailable)
		if denial.reason != "provider_oauth_not_configured" {
			t.Fatalf("reason %q", denial.reason)
		}
	})
}

// A run with no user subject has no provider grant: an autonomous run's
// decision subject is the mandate approver, never a consenting user.
func TestProviderOAuthNeedsTheRunsOwnUser(t *testing.T) {
	w, tokens := providerWorld()
	w.withMandate(mandate.KindAutonomous)
	w.context.SubjectPrincipalRef, w.context.Caller = "", securitycontext.Caller{}
	w.context.ActorChain = nil
	denial := requireDenial(t, redeem(t, w.broker(t, func(c *Config) { c.ProviderTokens = tokens }), w.request(t)), CodeDenied)
	if denial.reason != "provider_oauth_no_user_subject" || len(tokens.keys) != 0 {
		t.Fatalf("reason %q, token source calls %d", denial.reason, len(tokens.keys))
	}
}

// The Entra authority (SharePoint's descriptor) still exchanges on behalf
// of the user and never reads the provider token source.
func TestEntraAuthorityStillUsesTheExchange(t *testing.T) {
	w, tokens := providerWorld()
	w.catalog.authority = map[string]credentialgrant.OAuthAuthority{"jira": credentialgrant.OAuthAuthorityEntraOBO}
	w.context.MandateID, w.context.MandateEpoch = "", 0
	w.context.DelegationID, w.context.DelegationEpoch = tDelegation, 1
	result := redeem(t, w.broker(t, func(c *Config) { c.ProviderTokens = tokens }), w.request(t))
	if result.Envelope == nil {
		t.Fatalf("denied: %+v", result.Denial)
	}
	defer result.Envelope.Destroy()
	if string(result.Envelope.Material["access_token"]) != tOAuthValue || w.exchanger.last.DelegationID != tDelegation ||
		len(tokens.keys) != 0 {
		t.Fatalf("exchange %+v, token source calls %d", w.exchanger.last, len(tokens.keys))
	}
}

// A descriptor that names no authority denies the use.
func TestUnknownOAuthAuthorityIsNotSupported(t *testing.T) {
	w, tokens := providerWorld()
	w.catalog.authority = map[string]credentialgrant.OAuthAuthority{"jira": ""}
	denial := requireDenial(t, redeem(t, w.broker(t, func(c *Config) { c.ProviderTokens = tokens }), w.request(t)), CodeClassNotSupported)
	if denial.reason != "oauth_authority_unknown" || len(tokens.keys) != 0 {
		t.Fatalf("reason %q", denial.reason)
	}
}
