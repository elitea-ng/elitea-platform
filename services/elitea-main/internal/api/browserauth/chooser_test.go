package browserauth

// The single sign-on sign-in page (chooser.go).
//
// The deployment that motivates it: GitHub through a Dex OIDC bridge in the
// OIDC slot, and Microsoft Entra ID through SAML with SCIM-provisioned users.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const (
	oidcLogin = "/auth/oidc/login"
	samlLogin = "/auth/saml/login"
)

func gitHubProvider() SSOProvider {
	return SSOProvider{ID: "oidc", DisplayName: "GitHub", LoginPath: oidcLogin}
}

func entraProvider() SSOProvider {
	return SSOProvider{
		ID: "saml", DisplayName: "Microsoft", LoginPath: samlLogin,
		LoginDomains: []string{"contoso.com", "contoso.onmicrosoft.com"},
	}
}

func newTestChooser(t *testing.T, brand BrandSource, providers ...SSOProvider) *SSOChooser {
	t.Helper()
	chooser, err := NewSSOChooser(SSOChooserConfig{
		Providers:         func(context.Context) []SSOProvider { return providers },
		FallbackLoginPath: oidcLogin,
		Brand:             brand,
		SecureCookies:     true,
	})
	if err != nil {
		t.Fatalf("NewSSOChooser: %v", err)
	}
	return chooser
}

func getLogin(chooser *SSOChooser, rawQuery string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/auth/login?"+rawQuery, nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	chooser.Login(recorder, request)
	return recorder
}

func postEmail(chooser *SSOChooser, email, target string) *httptest.ResponseRecorder {
	form := url.Values{"email": {email}, "target_to": {target}}
	request := httptest.NewRequest(http.MethodPost, "/auth/login/continue", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	chooser.Continue(recorder, request)
	return recorder
}

func locationOf(t *testing.T, recorder *httptest.ResponseRecorder) *url.URL {
	t.Helper()
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location %q: %v", recorder.Header().Get("Location"), err)
	}
	return location
}

func TestNewSSOChooserRefusesAnIncompleteConfiguration(t *testing.T) {
	if _, err := NewSSOChooser(SSOChooserConfig{FallbackLoginPath: oidcLogin}); err == nil {
		t.Fatal("a chooser with no provider source was built")
	}
	if _, err := NewSSOChooser(SSOChooserConfig{
		Providers:         func(context.Context) []SSOProvider { return nil },
		FallbackLoginPath: "https://evil.example/login",
	}); err == nil {
		t.Fatal("a chooser with an off-origin fallback was built")
	}
}

// One usable provider keeps the old behaviour — an immediate redirect — and
// now carries the return target, which the old redirect dropped.
func TestASingleProviderRedirectsAndKeepsTheReturnTarget(t *testing.T) {
	recorder := getLogin(newTestChooser(t, nil, gitHubProvider()), "target_to=%2Fapp%2Fchat%3Fid%3D7")

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", recorder.Code)
	}
	location := locationOf(t, recorder)
	if location.Path != oidcLogin || location.Query().Get("target_to") != "/app/chat?id=7" {
		t.Fatalf("Location = %s", location)
	}
}

func TestNoUsableProviderRedirectsToTheFallbackRoute(t *testing.T) {
	recorder := getLogin(newTestChooser(t, nil), "target_to=%2Fapp")
	location := locationOf(t, recorder)
	if recorder.Code != http.StatusFound || location.Path != oidcLogin || location.Query().Get("target_to") != "/app" {
		t.Fatalf("status = %d, Location = %s", recorder.Code, location)
	}
}

func TestAnOffOriginReturnTargetIsReplacedWithTheRoot(t *testing.T) {
	recorder := getLogin(newTestChooser(t, nil, gitHubProvider()), "target_to=%2F%2Fevil.example%2F")
	if got := locationOf(t, recorder).Query().Get("target_to"); got != "/" {
		t.Fatalf("target_to = %q, want /", got)
	}
}

// Two usable providers render the page: one button per provider, the work
// email field, the brand, and a strict CSP whose hash matches the stylesheet.
func TestTwoProvidersRenderTheBrandedPage(t *testing.T) {
	chooser := newTestChooser(t, brandSourceStub{pack: brandedPack()}, gitHubProvider(), entraProvider())
	recorder := getLogin(chooser, "target_to=%2Fapp%2Fchat")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{
		"Continue with GitHub",
		"Continue with Microsoft",
		`<title>Sign in to Acme &lt;AI&gt;</title>`,
		`alt="Acme &lt;AI&gt;"`,
		`<label class="field-label" for="email">Work email</label>`,
		`action="/auth/login/continue" method="post"`,
		`name="target_to" value="/app/chat"`,
		`href="/auth/login/continue?provider=oidc&amp;target_to=%2Fapp%2Fchat"`,
		`href="/auth/login/continue?provider=saml&amp;target_to=%2Fapp%2Fchat"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// The routing is links and a form; the one script only picks the colour
	// scheme (page_test.go pins it).
	if strings.Count(body, "<script") != 1 {
		t.Errorf("the page carries %d scripts, want the theme script only", strings.Count(body, "<script"))
	}
	// The configured domains are routing data. The page never lists them.
	if strings.Contains(body, "contoso") {
		t.Error("the page reveals a configured login domain")
	}

	csp := recorder.Header().Get("Content-Security-Policy")
	digest := sha256.Sum256([]byte(authStyleSource))
	styleHash := "'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'"
	digest = sha256.Sum256([]byte(authScriptSource))
	scriptHash := "script-src 'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "';"
	for _, want := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'self' https:;", styleHash, scriptHash} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP admits inline content: %q", csp)
	}
	// The brand stylesheet is pinned by its own hash.
	brand := loginBrandFromPack(brandedPack())
	if !strings.Contains(csp, brand.StyleSource) {
		t.Errorf("CSP %q lacks the brand style hash %s", csp, brand.StyleSource)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("headers = %v", recorder.Header())
	}
}

func TestTheDefaultBrandRendersWithoutAPack(t *testing.T) {
	recorder := getLogin(newTestChooser(t, nil, gitHubProvider(), entraProvider()), "")
	body := recorder.Body.String()
	// The product's own logo, inline: the page loads no image to show it.
	if !strings.Contains(body, `<svg class="brand-mark" viewBox="0 0 99 20" fill="none" role="img" aria-label="Elitea"`) ||
		strings.Contains(body, `class="brand-logo"`) || strings.Contains(body, "<style></style>") {
		t.Fatalf("default brand missing:\n%s", body)
	}
	if strings.Count(body, "<style>") != 1 {
		t.Fatal("a page with no pack carries a brand stylesheet")
	}
}

// Without login domains the email field could only ever answer "no single
// sign-on", so it is not shown.
func TestTheEmailFieldIsHiddenWhenNoProviderListsADomain(t *testing.T) {
	saml := entraProvider()
	saml.LoginDomains = nil
	body := getLogin(newTestChooser(t, nil, gitHubProvider(), saml), "").Body.String()
	if strings.Contains(body, `name="email"`) {
		t.Fatal("the email field renders with no routable domain")
	}
}

// A login error renders the page even with one provider, so the browser is
// not sent straight back into the login that just failed.
func TestALoginErrorShowsTheBannerInsteadOfRedirecting(t *testing.T) {
	recorder := getLogin(newTestChooser(t, nil, gitHubProvider()), "error=sso_failed&target_to=%2Fapp")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `role="alert"`) || !strings.Contains(body, "Sign-in did not complete") {
		t.Fatalf("banner missing:\n%s", body)
	}
	if !strings.Contains(body, "Continue with GitHub") {
		t.Fatal("the provider button is missing")
	}
}

func TestTheLastUsedProviderIsMarked(t *testing.T) {
	recorder := getLogin(newTestChooser(t, nil, gitHubProvider(), entraProvider()), "",
		&http.Cookie{Name: LastUsedProviderCookie, Value: "saml"})
	body := recorder.Body.String()
	saml := body[strings.Index(body, `data-provider="saml"`):]
	if !strings.Contains(saml[:strings.Index(saml, "</a>")], "Last used") {
		t.Fatalf("the SAML button is not marked as last used:\n%s", body)
	}
	oidc := body[strings.Index(body, `data-provider="oidc"`):]
	if strings.Contains(oidc[:strings.Index(oidc, "</a>")], "Last used") {
		t.Fatal("the OIDC button is marked as last used")
	}
}

// Home-realm discovery: the domain of the typed address selects the provider,
// and the address goes along as a login hint.
func TestAWorkEmailIsRoutedToItsProviderWithALoginHint(t *testing.T) {
	recorder := postEmail(newTestChooser(t, nil, gitHubProvider(), entraProvider()), " Alice@Contoso.COM ", "/app/chat")

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303\n%s", recorder.Code, recorder.Body.String())
	}
	location := locationOf(t, recorder)
	if location.Path != samlLogin {
		t.Fatalf("Location = %s, want the SAML login", location)
	}
	if got := location.Query().Get("login_hint"); got != "Alice@contoso.com" {
		t.Fatalf("login_hint = %q", got)
	}
	if got := location.Query().Get("target_to"); got != "/app/chat" {
		t.Fatalf("target_to = %q", got)
	}
	cookie := recorder.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Name != LastUsedProviderCookie || cookie[0].Value != "saml" ||
		!cookie[0].HttpOnly || !cookie[0].Secure || cookie[0].Path != "/" {
		t.Fatalf("cookies = %+v", cookie)
	}
}

func TestAnUnknownDomainGetsAnInlineErrorThatNamesOnlyThatDomain(t *testing.T) {
	recorder := postEmail(newTestChooser(t, nil, gitHubProvider(), entraProvider()), "bob@fabrikam.com", "/app")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "No single sign-on is set up for fabrikam.com. Try another option.") {
		t.Fatalf("inline error missing:\n%s", body)
	}
	if !strings.Contains(body, `aria-invalid="true"`) || !strings.Contains(body, `aria-describedby="email-error"`) {
		t.Fatal("the field is not marked invalid for assistive technology")
	}
	if !strings.Contains(body, `value="bob@fabrikam.com"`) {
		t.Fatal("the typed address was not kept")
	}
	if strings.Contains(body, "contoso") {
		t.Fatal("the error reveals a configured domain")
	}
	if len(recorder.Result().Cookies()) != 0 {
		t.Fatal("a failed routing set the last-used cookie")
	}
	if recorder.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("the re-rendered page carries no CSP")
	}
}

func TestAMalformedAddressIsRefusedInline(t *testing.T) {
	for _, email := range []string{"", "alice", "alice@", "@contoso.com", "a@b@contoso.com", "alice@localhost", "<a@contoso.com>"} {
		recorder := postEmail(newTestChooser(t, nil, gitHubProvider(), entraProvider()), email, "/")
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Enter a work email address") {
			t.Errorf("%q: status = %d", email, recorder.Code)
		}
	}
}

func TestAProviderButtonRemembersTheProvider(t *testing.T) {
	chooser := newTestChooser(t, nil, gitHubProvider(), entraProvider())
	recorder := httptest.NewRecorder()
	chooser.Continue(recorder, httptest.NewRequest(http.MethodGet,
		"/auth/login/continue?provider=oidc&target_to=%2Fapp%2Fx", nil))

	location := locationOf(t, recorder)
	if recorder.Code != http.StatusSeeOther || location.Path != oidcLogin || location.Query().Get("target_to") != "/app/x" {
		t.Fatalf("status = %d, Location = %s", recorder.Code, location)
	}
	if location.Query().Has("login_hint") {
		t.Fatal("a button sent a login hint")
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != "oidc" {
		t.Fatalf("cookies = %+v", cookies)
	}
}

func TestAnUnknownProviderGoesBackToThePage(t *testing.T) {
	chooser := newTestChooser(t, nil, gitHubProvider(), entraProvider())
	recorder := httptest.NewRecorder()
	chooser.Continue(recorder, httptest.NewRequest(http.MethodGet,
		"/auth/login/continue?provider=ldap&target_to=%2Fapp", nil))
	location := locationOf(t, recorder)
	if location.Path != "/auth/login" || location.Query().Get("target_to") != "/app" {
		t.Fatalf("Location = %s", location)
	}
}

func TestTheContinueFormRefusesAnotherMediaType(t *testing.T) {
	chooser := newTestChooser(t, nil, gitHubProvider(), entraProvider())
	request := httptest.NewRequest(http.MethodPost, "/auth/login/continue", strings.NewReader(`{"email":"a@contoso.com"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	chooser.Continue(recorder, request)
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", recorder.Code)
	}
}
