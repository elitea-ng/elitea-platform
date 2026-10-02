package browserauth

// The single sign-on sign-in page: `GET /auth/login` on the SSO plane.
//
// # What it shows
//
// One "Continue with <provider>" button per usable provider, and — when any
// provider lists login domains — a "Work email" field. The field is
// identifier-first sign-in (home-realm discovery): the domain of the typed
// address selects the provider, and the address goes to that provider as a
// login hint so the person does not type it twice.
//
// # When it does NOT show
//
// With exactly one usable provider and no error to report, the page is a
// redirect to that provider's login route, as the sign-in route always was
// on this plane. With none, it redirects to the fallback login route, which
// answers that single sign-on is not available.
//
// # Progressive enhancement
//
// The page has no script. The buttons are links and the email field is a
// plain form; GET and POST `/auth/login/continue` do the routing on
// the server. The CSP is the Form login page's: `default-src 'none'`, the
// stylesheets pinned by hash, same-origin images and fonts.
//
// `form-action` admits `https:` in addition to 'self'. The email form posts
// to this origin, and the answer is a redirect chain that ends at the identity
// provider. Browsers check every hop of that chain against `form-action`, so
// 'self' alone would block the last one.
//
// # The return target
//
// `target_to` is read once, canonicalised (browserflow.CanonicalReturnTarget)
// and carried through every hop: into the links, into the form, and onto the
// provider login route, which keeps it in its own signed state.
//
// # What the email field does not reveal
//
// An address whose domain no provider lists gets one sentence that names only
// the domain the person typed. The page never lists the configured domains.

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"html/template"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/identityproviders"
)

const (
	// ChooserContinuePath is where the page's links and form go, relative to
	// BasePath.
	ChooserContinuePath = "/login/continue"

	// LastUsedProviderCookie remembers which provider this browser used last,
	// so the page can mark it. It holds a provider id ("oidc" or "saml") and
	// nothing else; it is not a credential.
	LastUsedProviderCookie = "elitea_sso_last_used"

	// lastUsedProviderMaxAge is 400 days, the longest lifetime current
	// browsers keep.
	lastUsedProviderMaxAge = 400 * 24 * 60 * 60

	maxChooserFormBytes = int64(4 << 10)
	maxEmailBytes       = 254

	// SignInErrorCode is the `error` value the SSO planes send the browser
	// back with. Any other non-empty value shows the same banner.
	SignInErrorCode = "sso_failed"
)

//go:embed templates/sso_chooser.html
var chooserTemplateSource string

//go:embed templates/sso_chooser.css
var chooserStyleSource string

var chooserStyleCSPSource = func() string {
	digest := sha256.Sum256([]byte(chooserStyleSource))
	return "'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'"
}()

// SSOProvider is one usable provider on the page.
type SSOProvider struct {
	// ID is "oidc" or "saml". It is the `provider` parameter of the continue
	// route and the value of the last-used cookie.
	ID string
	// DisplayName labels the button.
	DisplayName string
	// LoginPath is the provider's same-origin login route. It accepts
	// `target_to` and `login_hint`.
	LoginPath string
	// LoginDomains are lower-case email domains routed to this provider.
	LoginDomains []string
}

// SSOProviderSource lists the usable providers, in display order. It is read
// on every request, so an operator's change to a provider shows at once.
type SSOProviderSource func(ctx context.Context) []SSOProvider

// SSOChooserConfig composes the page.
type SSOChooserConfig struct {
	Providers SSOProviderSource
	// FallbackLoginPath is where the page sends the browser when no provider
	// is usable. That route then states that single sign-on is not available.
	FallbackLoginPath string
	// Brand supplies the product name, logo and colours (branding.go). Nil
	// renders the default presentation.
	Brand BrandSource
	// SecureCookies sets the Secure flag on the last-used cookie.
	SecureCookies bool
}

// SSOChooser serves the sign-in page and its continue route.
type SSOChooser struct {
	providers     SSOProviderSource
	fallback      string
	brand         BrandSource
	secureCookies bool
	page          *template.Template
}

// ErrInvalidChooserConfiguration reports a chooser composed without a provider
// source or with an unsafe fallback path.
var ErrInvalidChooserConfiguration = errors.New("invalid single sign-on page configuration")

// NewSSOChooser builds the page.
func NewSSOChooser(config SSOChooserConfig) (*SSOChooser, error) {
	if config.Providers == nil || browserflow.ValidateReturnTarget(config.FallbackLoginPath) != nil {
		return nil, ErrInvalidChooserConfiguration
	}
	page, err := template.New("sso_chooser.html").Parse(chooserTemplateSource)
	if err != nil {
		return nil, ErrInvalidChooserConfiguration
	}
	return &SSOChooser{
		providers:     config.Providers,
		fallback:      config.FallbackLoginPath,
		brand:         config.Brand,
		secureCookies: config.SecureCookies,
		page:          page,
	}, nil
}

// chooserButton is one provider as the template renders it.
type chooserButton struct {
	ID          string
	DisplayName string
	Href        string
	LastUsed    bool
}

// chooserPage is the template's data.
type chooserPage struct {
	Style      template.CSS
	Brand      loginBrand
	Target     string
	Providers  []chooserButton
	ShowEmail  bool
	Email      string
	EmailError string
	Error      string
}

// Login answers `GET /auth/login`.
func (c *SSOChooser) Login(w http.ResponseWriter, r *http.Request) {
	chooserHeaders(w)
	query := r.URL.Query()
	target := chooserTarget(query.Get("target_to"))
	providers := c.providers(r.Context())
	_, failed := query["error"]

	switch {
	case len(providers) == 0:
		http.Redirect(w, r, providerLoginURL(c.fallback, target, ""), http.StatusFound)
		return
	case len(providers) == 1 && !failed:
		http.Redirect(w, r, providerLoginURL(providers[0].LoginPath, target, ""), http.StatusFound)
		return
	}

	page := c.newPage(r, providers, target)
	if failed {
		page.Error = "Sign-in did not complete. Try again, or choose another option."
	}
	c.render(w, r, page)
}

// Continue answers `GET` and `POST /auth/login/continue`.
//
// `provider` selects a provider by id. Otherwise `email` selects one by the
// domain of the address. Both carry `target_to`.
func (c *SSOChooser) Continue(w http.ResponseWriter, r *http.Request) {
	chooserHeaders(w)
	values := r.URL.Query()
	if r.Method == http.MethodPost {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/x-www-form-urlencoded" {
			writeProblem(w, http.StatusUnsupportedMediaType)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxChooserFormBytes)
		if err := r.ParseForm(); err != nil {
			writeProblem(w, http.StatusBadRequest)
			return
		}
		values = r.PostForm
	}
	target := chooserTarget(values.Get("target_to"))
	providers := c.providers(r.Context())
	if len(providers) == 0 {
		http.Redirect(w, r, providerLoginURL(c.fallback, target, ""), http.StatusSeeOther)
		return
	}

	if id := values.Get("provider"); id != "" {
		for _, provider := range providers {
			if provider.ID == id {
				c.rememberProvider(w, provider.ID)
				http.Redirect(w, r, providerLoginURL(provider.LoginPath, target, ""), http.StatusSeeOther)
				return
			}
		}
		// A provider that was disabled since the page rendered. The page
		// shows the providers that are usable now.
		http.Redirect(w, r, BasePath+LoginPath+"?"+url.Values{"target_to": {target}}.Encode(), http.StatusSeeOther)
		return
	}

	email := strings.TrimSpace(values.Get("email"))
	page := c.newPage(r, providers, target)
	page.Email = email
	domain, ok := emailDomain(email)
	if !ok {
		page.EmailError = "Enter a work email address, such as name@example.com."
		c.render(w, r, page)
		return
	}
	for _, provider := range providers {
		for _, listed := range provider.LoginDomains {
			if listed == domain {
				c.rememberProvider(w, provider.ID)
				hint := email[:strings.LastIndexByte(email, '@')+1] + domain
				http.Redirect(w, r, providerLoginURL(provider.LoginPath, target, hint), http.StatusSeeOther)
				return
			}
		}
	}
	page.EmailError = "No single sign-on is set up for " + domain + ". Try another option."
	c.render(w, r, page)
}

func (c *SSOChooser) newPage(r *http.Request, providers []SSOProvider, target string) chooserPage {
	lastUsed := ""
	if cookie, err := r.Cookie(LastUsedProviderCookie); err == nil {
		lastUsed = cookie.Value
	}
	page := chooserPage{
		Style:  template.CSS(chooserStyleSource), //nolint:gosec // compiled into this binary
		Brand:  loginBrand{ProductName: DefaultProductName},
		Target: target,
	}
	if c.brand != nil {
		if snapshot := c.brand.Current(r.Context()); snapshot.Pack != nil {
			page.Brand = loginBrandFromPack(snapshot.Pack)
		}
	}
	for _, provider := range providers {
		page.Providers = append(page.Providers, chooserButton{
			ID:          provider.ID,
			DisplayName: provider.DisplayName,
			Href: BasePath + ChooserContinuePath + "?" + url.Values{
				"provider":  {provider.ID},
				"target_to": {target},
			}.Encode(),
			LastUsed: provider.ID == lastUsed,
		})
		if len(provider.LoginDomains) > 0 {
			page.ShowEmail = true
		}
	}
	return page
}

func (c *SSOChooser) render(w http.ResponseWriter, _ *http.Request, page chooserPage) {
	var body bytes.Buffer
	if err := c.page.Execute(&body, page); err != nil {
		writeProblem(w, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Security-Policy", chooserContentSecurityPolicy(page.Brand.StyleSource))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body.Bytes())
}

func (c *SSOChooser) rememberProvider(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:  LastUsedProviderCookie,
		Value: id,
		// Root, so the page reads it under BasePath and LegacyBasePath alike.
		Path:     "/",
		MaxAge:   lastUsedProviderMaxAge,
		HttpOnly: true,
		Secure:   c.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// chooserTarget canonicalises a caller-supplied return target, and falls back
// to "/" for anything that is not a same-origin path.
func chooserTarget(raw string) string {
	if canonical, err := browserflow.CanonicalReturnTarget(raw); err == nil {
		return canonical
	}
	return "/"
}

// providerLoginURL is a provider login route with the return target and,
// when there is one, the login hint.
func providerLoginURL(loginPath, target, hint string) string {
	values := url.Values{"target_to": {target}}
	if hint != "" {
		values.Set("login_hint", hint)
	}
	return loginPath + "?" + values.Encode()
}

// emailDomain returns the lower-case domain of a plain address, and false
// when the value is not one.
func emailDomain(email string) (string, bool) {
	if email == "" || len(email) > maxEmailBytes || strings.Count(email, "@") != 1 ||
		strings.ContainsFunc(email, func(r rune) bool { return r <= ' ' || r == 0x7f || strings.ContainsRune(`"<>()[]\,;:`, r) }) {
		return "", false
	}
	at := strings.IndexByte(email, '@')
	if at == 0 {
		return "", false
	}
	domain := strings.ToLower(email[at+1:])
	if !identityproviders.IsLoginDomain(domain) {
		return "", false
	}
	return domain, true
}

func chooserContentSecurityPolicy(brandStyleSource string) string {
	styleSources := chooserStyleCSPSource
	if brandStyleSource != "" {
		styleSources += " " + brandStyleSource
	}
	return "default-src 'none'; base-uri 'none'; form-action 'self' https:; frame-ancestors 'none'; " +
		"img-src 'self'; font-src 'self'; style-src " + styleSources
}

func chooserHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}
