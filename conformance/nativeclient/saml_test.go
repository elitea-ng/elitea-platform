//go:build conformance

package nativeclient_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/conformance/nativeclient/browser"
	"github.com/EliteaAI/elitea-platform/conformance/nativeclient/client"
	"github.com/EliteaAI/elitea-platform/conformance/nativeclient/samlidp"
)

const samlProviderPath = "/api/v2/admin/identity_providers/administration/conformance-saml"

// ── 2b. Native sign-in through SAML ──────────────────────────────────────────
//
// The stack has no SAML identity provider, so the suite runs one in process
// and authors it through the admin identity-provider API with its signing
// certificate inline: the deployment never calls it, only the browser does.
// Authoring a SAML provider needs no restart on a deployment whose browser
// plane is already mounted (cmd/elitea-main: the SAML handler is built with
// the OIDC plane), which is the case here.
func (s *suite) signInSAML(t *testing.T) {
	ctx := testContext(t, 3*time.Minute)
	s.requireSession(t)
	idp, err := samlidp.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idp.Close() })

	remove := func() {
		response := need(t)(s.admin.Do(ctx, http.MethodDelete, samlProviderPath, nil, nil))
		if response.Status != http.StatusOK && response.Status != http.StatusNoContent && response.Status != http.StatusNotFound {
			t.Errorf("remove the SAML provider: %s", response)
		}
	}
	remove()
	t.Cleanup(remove)
	saved := need(t)(s.admin.Do(ctx, http.MethodPut, samlProviderPath, map[string]any{
		"kind":         "saml",
		"display_name": "Conformance SAML",
		"enabled":      true,
		"saml": map[string]any{
			"idp_entity_id":    idp.EntityID,
			"idp_sso_url":      idp.SSOURL,
			"idp_certificates": []string{idp.CertificatePEM},
			"sp_entity_id":     s.cfg.Origin + "/auth/saml/metadata",
			"acs_url":          s.cfg.Origin + "/auth/saml/acs",
			"email_attribute":  "email",
		},
	}, nil))
	if saved.Status != http.StatusOK && saved.Status != http.StatusCreated {
		t.Fatalf("author the SAML provider: %s", saved)
	}

	idp.SignInAs(s.cfg.SAMLUser, "")
	signed := s.cfg.signIn(t, s.discovery, s.cfg.SAMLUser, "saml", "conformance saml")
	if idp.Requests() != 1 {
		t.Fatalf("the SAML identity provider answered %d authentication requests, want 1: the sign-in did not go through it",
			idp.Requests())
	}
	devices := mustJSON(t, need(t)(signed.api.Get(ctx, client.DevicesPath)), http.StatusOK)
	if !hasCurrentDevice(devices, signed.tokens.DeviceID, "conformance saml") {
		t.Fatalf("the SAML-signed device is not current in its own list: %v", devices)
	}

	// Negative: an assertion addressed to another service provider is refused
	// (gosaml2 reports a wrong audience as a WARNING, not an error; the
	// deployment must check it itself — memory sso-identity-providers).
	idp.SignInAs(s.cfg.SAMLUser, "urn:elitea:conformance:some-other-service-provider")
	verifier := client.NewVerifier()
	start := client.AuthorizeURL(s.discovery.NativeAuth.AuthorizationEndpoint, client.AuthorizationRequest{
		ClientID: s.cfg.ClientID, RedirectURI: s.cfg.RedirectURI, State: client.NewState(),
		Challenge: client.S256Challenge(verifier), DeviceName: "conformance saml audience", Platform: "linux",
	})
	b := browser.New()
	callback, _, err := b.Drive(ctx, start, "allow", browser.Chooser("saml"))
	if callback != nil || !errors.Is(err, browser.ErrUnrecognisedPage) ||
		!strings.Contains(err.Error(), "/auth/saml/acs → HTTP 401") {
		t.Fatalf("a wrong-audience assertion: callback %v, err %v; want the ACS to answer 401", callback, err)
	}
	if idp.Requests() != 2 {
		t.Fatalf("the wrong-audience leg did not reach the identity provider (%d requests)", idp.Requests())
	}
	s.done(t)
}
