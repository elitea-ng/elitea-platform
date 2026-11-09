package nativeauth_test

// ADR-0052 decision 9 against a real database: one deployment serves the
// native apps of two brands. Each brand's app is its own registered client
// (its bundle id and redirect URI); signing in with one never works with the
// other's redirect, disabling one brand refuses only its sign-in, and a
// per-client minimum version holds only that brand's app back.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	nativeapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/nativeauth"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/nativeauth"
)

type brandClient struct{ id, redirect string }

var (
	// ELITEA's mobile app (brand/elitea): the one native client id it ships.
	eliteaApp = brandClient{id: "ai.elitea.app", redirect: "ai.elitea.app:/oauth/callback"}
	// A second brand built from the same source (brand/<id>/brand.json).
	acmeApp = brandClient{id: "com.example.acme", redirect: "com.example.acme:/oauth/callback"}
)

func (b brandClient) client(name string) domain.Client {
	return domain.Client{
		ClientID: b.id, DisplayName: name, Enabled: true, Source: domain.SourceFile,
		RedirectURIs: []string{b.redirect},
	}
}

func (b brandClient) authorize(extra map[string]string) map[string]string {
	overrides := map[string]string{"client_id": b.id, "redirect_uri": b.redirect}
	for key, value := range extra {
		overrides[key] = value
	}
	return overrides
}

// exchangeAs swaps a code for tokens as brand b.
func (s *stack) exchangeAs(b brandClient, code string, options ...requestOption) *httptest.ResponseRecorder {
	s.t.Helper()
	return s.do(http.MethodPost, nativeapi.TokenPath, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {b.redirect},
		"client_id": {b.id}, "code_verifier": {testVerifier},
	}, options...)
}

func accessTokenOf(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var body tokenResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.AccessToken == "" {
		t.Fatalf("no access token in %s (%v)", recorder.Body.String(), err)
	}
	return body.AccessToken
}

func newBrandsStack(t *testing.T) *stack {
	t.Helper()
	return newStack(t, withFileClients(eliteaApp.client("Elitea AI"), acmeApp.client("Acme Assistant")))
}

func TestTwoBrandsSignInEachWithItsOwnRedirect(t *testing.T) {
	s := newBrandsStack(t)
	userID := s.seedUser("two-brands@example.test")
	for _, b := range []brandClient{eliteaApp, acmeApp} {
		code := s.signInWith(userID, "two-brands@example.test", b.authorize(nil))
		recorder := s.exchangeAs(b, code)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s exchange = %d %s", b.id, recorder.Code, recorder.Body.String())
		}
		clientID, ok, err := s.validator.ClientID(context.Background(), accessTokenOf(t, recorder))
		if err != nil || !ok || clientID != b.id {
			t.Fatalf("%s token belongs to %q (ok %v, err %v)", b.id, clientID, ok, err)
		}
	}

	// A brand's redirect is not another brand's: authorize refuses the pair.
	crossed := s.do(http.MethodGet, authorizeQuery(map[string]string{"client_id": acmeApp.id, "redirect_uri": eliteaApp.redirect}), nil)
	if crossed.Code != http.StatusBadRequest || !strings.Contains(crossed.Body.String(), "return address is not registered") {
		t.Fatalf("acme with ELITEA's redirect = %d %s, want 400", crossed.Code, crossed.Body.String())
	}
	// Nor can one brand's code be redeemed as the other brand.
	code := s.signInWith(userID, "two-brands@example.test", eliteaApp.authorize(nil))
	recorder := s.exchangeAs(acmeApp, code)
	var refusal tokenResponse
	_ = json.Unmarshal(recorder.Body.Bytes(), &refusal)
	if recorder.Code != http.StatusBadRequest || refusal.Error != "invalid_grant" {
		t.Fatalf("ELITEA's code as acme = %d %s, want 400 invalid_grant", recorder.Code, recorder.Body.String())
	}
}

func TestDisablingOneBrandRefusesOnlyItsSignIn(t *testing.T) {
	s := newBrandsStack(t)
	userID := s.seedUser("disable-brand@example.test")
	// A device of each brand, signed in before the switch-off.
	acmeDevice := s.exchangeAs(acmeApp, s.signInWith(userID, "disable-brand@example.test", acmeApp.authorize(nil)))
	eliteaDevice := s.exchangeAs(eliteaApp, s.signInWith(userID, "disable-brand@example.test", eliteaApp.authorize(nil)))
	var acmeTokens, eliteaTokens tokenResponse
	_ = json.Unmarshal(acmeDevice.Body.Bytes(), &acmeTokens)
	_ = json.Unmarshal(eliteaDevice.Body.Bytes(), &eliteaTokens)
	if acmeTokens.DeviceID == "" || eliteaTokens.DeviceID == "" {
		t.Fatalf("sign-ins before the disable: acme %s, ELITEA %s", acmeDevice.Body.String(), eliteaDevice.Body.String())
	}

	disabled := acmeApp.client("Acme Assistant")
	disabled.Enabled = false
	revoked, err := s.store.UpsertClient(context.Background(), disabled, 0)
	if err != nil {
		t.Fatalf("disable acme: %v", err)
	}
	s.registry.Invalidate()
	// The admin save signs out that brand's devices, and only those.
	if revoked != 1 || s.family(acmeTokens.DeviceID).revokedAt == nil {
		t.Fatalf("disable revoked %d devices, acme family %+v; want acme's one device revoked", revoked, s.family(acmeTokens.DeviceID))
	}
	if state := s.family(eliteaTokens.DeviceID); state.revokedAt != nil {
		t.Fatalf("ELITEA's device was revoked by acme's disable: %+v", state)
	}
	if got := s.whoami(eliteaTokens.AccessToken); got.Code != http.StatusOK {
		t.Fatalf("ELITEA's token after acme's disable = %d, want 200", got.Code)
	}

	refused := s.do(http.MethodGet, authorizeQuery(acmeApp.authorize(nil)), nil)
	if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), "not registered with this server") {
		t.Fatalf("disabled brand authorize = %d %s, want 400", refused.Code, refused.Body.String())
	}
	code := s.signInWith(userID, "disable-brand@example.test", eliteaApp.authorize(nil))
	if recorder := s.exchangeAs(eliteaApp, code); recorder.Code != http.StatusOK {
		t.Fatalf("the other brand's exchange = %d %s, want 200", recorder.Code, recorder.Body.String())
	}
}

func TestPerBrandMinimumVersionHoldsOnlyThatBrandBack(t *testing.T) {
	s := newBrandsStack(t)
	userID := s.seedUser("brand-minimum@example.test")
	raised := acmeApp.client("Acme Assistant")
	raised.MinClientVersion = "2.0.0"
	if _, err := s.store.UpsertClient(context.Background(), raised, 0); err != nil {
		t.Fatalf("raise acme minimum: %v", err)
	}
	s.registry.Invalidate()

	old := withHeader(apimw.ClientVersionHeader, "1.5.0")
	acmeCode := s.signInWith(userID, "brand-minimum@example.test", acmeApp.authorize(nil))
	if recorder := s.exchangeAs(acmeApp, acmeCode, old); recorder.Code != http.StatusUpgradeRequired ||
		recorder.Header().Get(apimw.MinClientVersionHeader) != "2.0.0" {
		t.Fatalf("acme 1.5.0 exchange = %d %s, want 426 with minimum 2.0.0", recorder.Code, recorder.Body.String())
	}
	// The refusal did not consume the code: the updated app redeems it.
	if recorder := s.exchangeAs(acmeApp, acmeCode, withHeader(apimw.ClientVersionHeader, "2.0.0")); recorder.Code != http.StatusOK {
		t.Fatalf("acme 2.0.0 exchange = %d %s, want 200", recorder.Code, recorder.Body.String())
	}
	eliteaCode := s.signInWith(userID, "brand-minimum@example.test", eliteaApp.authorize(nil))
	if recorder := s.exchangeAs(eliteaApp, eliteaCode, old); recorder.Code != http.StatusOK {
		t.Fatalf("ELITEA 1.5.0 exchange = %d %s, want 200 (its own minimum is unset)", recorder.Code, recorder.Body.String())
	}
}
