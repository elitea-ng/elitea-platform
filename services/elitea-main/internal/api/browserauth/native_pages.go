package browserauth

// The two pages of the native authorization flow (ADR-0025 WP2): the one-line
// confirmation a user answers after the existing browser sign-in, and the error
// page every refusal that must NOT redirect renders (an unknown client or
// redirect URI is the open-redirect barrier: never a redirect).
//
// They live here, beside the sign-in pages, because they share the partials,
// the stylesheet and the brand under the same CSP discipline (page.go,
// branding.go): `default-src 'none'`, the stylesheet, the theme script and the
// brand stylesheet each admitted by hash.

import (
	"bytes"
	"context"
	_ "embed"
	"html/template"
	"net/http"
)

//go:embed templates/native_consent.html
var nativeConsentTemplateSource string

//go:embed templates/native_error.html
var nativeErrorTemplateSource string

var (
	nativeConsentTemplate = template.Must(parseAuthPage("native_consent", nativeConsentTemplateSource))
	nativeErrorTemplate   = template.Must(parseAuthPage("native_error", nativeErrorTemplateSource))
)

// NativeConsent is what the confirmation page shows. Every value is escaped by
// html/template; DeviceName is the client's CLAIM and the page words it so.
type NativeConsent struct {
	ClientName string
	ClientID   string
	Email      string
	DeviceName string
	Platform   string
	// Origin is the deployment origin, shown in a muted line: a user who
	// reached a phishing deployment has a chance to notice.
	Origin string
	// Action is the decision route; Request is the authorize handle; UserID
	// is the principal at render time, compared again on submit.
	Action  string
	Request string
	UserID  string
	// SwitchAccountURL signs out and comes back here.
	SwitchAccountURL string
	// FormAction is the redirect target's CSP source (`scheme:` or the two
	// loopback hosts): the decision POST answers with a 302 to the app, and
	// browsers enforce form-action on that redirect.
	FormAction string
}

type nativeConsentPage struct {
	NativeConsent
	Style  template.CSS
	Script template.JS
	Brand  loginBrand
}

type nativeErrorPage struct {
	Message string
	Style   template.CSS
	Script  template.JS
	Brand   loginBrand
}

// NativePages renders both pages with the deployment's brand.
type NativePages struct {
	brand BrandSource
}

// NewNativePages builds the renderer. brand may be nil (default presentation).
func NewNativePages(brand BrandSource) *NativePages {
	return &NativePages{brand: brand}
}

func (p *NativePages) currentBrand(ctx context.Context) loginBrand {
	if p == nil || p.brand == nil {
		return defaultLoginBrand()
	}
	snapshot := p.brand.Current(ctx)
	if snapshot.Pack == nil {
		return defaultLoginBrand()
	}
	return loginBrandFromPack(snapshot.Pack)
}

// RenderConsent writes the confirmation page with status 200.
func (p *NativePages) RenderConsent(w http.ResponseWriter, r *http.Request, consent NativeConsent) {
	brand := p.currentBrand(r.Context())
	var body bytes.Buffer
	if err := nativeConsentTemplate.Execute(&body, nativeConsentPage{
		NativeConsent: consent,
		Style:         authPageStyle(),
		Script:        authPageScript(),
		Brand:         brand,
	}); err != nil {
		writeProblem(w, http.StatusServiceUnavailable)
		return
	}
	formAction := "'self'"
	if consent.FormAction != "" {
		formAction += " " + consent.FormAction
	}
	writeNativePage(w, http.StatusOK, authPageCSP(formAction, brand.StyleSource), consentReferrerPolicy, body.Bytes())
}

// consentReferrerPolicy is the consent page's Referrer-Policy, and it is NOT
// the `no-referrer` every other auth page sends. Under `no-referrer` a browser
// serialises the Origin of a POST as "null" (Fetch, "serializing a request
// origin"), and the decision handler refuses "null" by design (defence in
// depth against a cross-site POST). So with `no-referrer` every Continue from
// a real browser was answered 403 "The answer did not come from this server's
// page" -- Chromium and WebKit both, found by the settings.devices journey;
// the Go conformance client sends no Origin, so it never saw it. The page's
// `<meta name="referrer">` (templates/native_consent.html) says the same: a
// meta overrides this header, and the shared auth-page head says no-referrer.
// `same-origin` sends the real Origin on the same-origin decision POST and
// still sends NO Referer on any cross-origin request, so the request handle
// in this page's URL never reaches the app's redirect URI.
const consentReferrerPolicy = "same-origin"

// RenderError writes the error page with the given status.
func (p *NativePages) RenderError(w http.ResponseWriter, r *http.Request, status int, message string) {
	brand := p.currentBrand(r.Context())
	var body bytes.Buffer
	if err := nativeErrorTemplate.Execute(&body, nativeErrorPage{
		Message: message,
		Style:   authPageStyle(),
		Script:  authPageScript(),
		Brand:   brand,
	}); err != nil {
		writeProblem(w, http.StatusServiceUnavailable)
		return
	}
	writeNativePage(w, status, authPageCSP("'none'", brand.StyleSource), "no-referrer", body.Bytes())
}

func writeNativePage(w http.ResponseWriter, status int, csp, referrerPolicy string, body []byte) {
	chooserHeaders(w)
	w.Header().Set("Referrer-Policy", referrerPolicy)
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
