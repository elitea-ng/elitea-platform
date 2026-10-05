package auth

// Entra ID and every other identity provider return the assertion with the
// HTTP-POST binding: a cross-site top-level POST to the ACS. Browsers withhold
// SameSite=Lax cookies from that request, so the request-binding cookie has to
// be SameSite=None (which requires Secure). These tests pin the attributes of
// both writes — setting and clearing — and the ACS reading the cookie back.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/identityproviders"
)

func samlCookieHandler(t *testing.T, secure bool) *SAMLHandler {
	t.Helper()
	return NewSAMLHandler(nil, "test-secret", stubProviderSource{providers: map[identityproviders.Kind]identityproviders.Provider{
		identityproviders.KindSAML: {
			Key: "entra", Kind: identityproviders.KindSAML, DisplayName: "Microsoft", Enabled: true, Revision: 1,
			SAML: &identityproviders.SAMLDocument{
				IDPEntityID:     "https://sts.windows.net/tenant/",
				IDPSSOURL:       "https://login.microsoftonline.com/tenant/saml2",
				IDPCertificates: []string{selfSignedCertificate(t)},
				SPEntityID:      "https://elitea.example.com",
				ACSURL:          "https://elitea.example.com/auth/saml/acs",
			},
		},
	}}, nil, secure)
}

func samlRequestSetCookie(t *testing.T, recorder *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == samlRequestCookie {
			return cookie
		}
	}
	t.Fatalf("no %s cookie in %v", samlRequestCookie, recorder.Header()["Set-Cookie"])
	return nil
}

func TestTheSAMLRequestCookieSurvivesACrossSitePost(t *testing.T) {
	for _, tc := range []struct {
		name     string
		secure   bool
		sameSite http.SameSite
	}{
		{"https", true, http.SameSiteNoneMode},
		{"plain http development", false, http.SameSiteLaxMode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := samlCookieHandler(t, tc.secure)

			login := httptest.NewRecorder()
			handler.Login(login, httptest.NewRequest(http.MethodGet, "/auth/saml/login?target_to=%2Fapp", nil))
			if login.Code != http.StatusFound {
				t.Fatalf("login status = %d: %s", login.Code, login.Body.String())
			}
			set := samlRequestSetCookie(t, login)
			if set.SameSite != tc.sameSite || set.Secure != tc.secure || !set.HttpOnly || set.Path != "/" || set.MaxAge <= 0 {
				t.Fatalf("login cookie = %+v", set)
			}
			if tc.secure && !strings.Contains(login.Header().Get("Set-Cookie"), "SameSite=None") {
				t.Fatalf("Set-Cookie = %q", login.Header().Get("Set-Cookie"))
			}

			// The ACS receives the cookie: it gets past the cookie gate and
			// stops at the missing SAMLResponse instead.
			form := url.Values{}
			request := httptest.NewRequest(http.MethodPost, "/auth/saml/acs", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.AddCookie(&http.Cookie{Name: samlRequestCookie, Value: set.Value})
			acs := httptest.NewRecorder()
			handler.ACS(acs, request)
			if body := acs.Body.String(); !strings.Contains(body, "missing SAMLResponse") {
				t.Fatalf("ACS with the cookie: status %d body %q", acs.Code, body)
			}
			cleared := samlRequestSetCookie(t, acs)
			if cleared.MaxAge >= 0 || cleared.SameSite != tc.sameSite || cleared.Secure != tc.secure ||
				!cleared.HttpOnly || cleared.Path != "/" {
				t.Fatalf("clearing cookie must match the original attributes: %+v", cleared)
			}

			// Without the cookie the ACS refuses before verifying anything.
			bare := httptest.NewRecorder()
			handler.ACS(bare, httptest.NewRequest(http.MethodPost, "/auth/saml/acs", strings.NewReader("")))
			if bare.Code != http.StatusBadRequest || !strings.Contains(bare.Body.String(), "missing or invalid login request cookie") {
				t.Fatalf("ACS without the cookie: %d %q", bare.Code, bare.Body.String())
			}
		})
	}
}
