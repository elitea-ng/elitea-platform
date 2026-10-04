package browserauth

// What the two sign-in pages share: the SSO page (`GET /auth/login`,
// chooser.go) and the Form page (`GET /auth/form/login`, handler.go).
//
// # Presentation
//
// One stylesheet (templates/auth.css) in the web app's tokens, with a light
// and a dark scheme, decorative ring art drawn as inline SVG in the brand
// colours, and the shared markup in templates/partials.html. Nothing is
// loaded from anywhere: no external stylesheet, font, image or script.
//
// # The colour scheme
//
// The web app keeps its colour mode in localStorage under `el-mode` ("light",
// "dark" or "system"). The page reads the same key before first paint with a
// small inline script (templates/theme.js) and sets `data-el-scheme` on
// <html> for an explicit choice; otherwise — "system", an unknown value, no
// value, or no script at all — the stylesheet follows `prefers-color-scheme`,
// which also tracks a live change of the OS setting. With script, a button
// cycles the mode and writes it back under the same key.
//
// # The CSP
//
// `default-src 'none'`. The stylesheet and the script are each admitted by
// their SHA-256, computed here from the bytes embedded in this binary and
// written verbatim into the page; the brand stylesheet, when a pack states
// one, adds its own hash (branding.go). No 'unsafe-inline', ever.

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"html/template"
)

//go:embed templates/auth.css
var authStyleSource string

//go:embed templates/theme.js
var authScriptSource string

//go:embed templates/partials.html
var authPartialsSource string

var (
	authStyleCSPSource  = cspHashSource(authStyleSource)
	authScriptCSPSource = cspHashSource(authScriptSource)
)

// cspHashSource is the CSP hash source that admits exactly content.
func cspHashSource(content string) string {
	digest := sha256.Sum256([]byte(content))
	return "'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'"
}

// parseAuthPage parses one sign-in page with the shared partials.
func parseAuthPage(name, source string) (*template.Template, error) {
	page, err := template.New(name).Parse(source)
	if err != nil {
		return nil, err
	}
	return page.Parse(authPartialsSource)
}

// authPageStyle and authPageScript are the embedded sources as the template
// renders them. Both are compiled into this binary.
func authPageStyle() template.CSS { return template.CSS(authStyleSource) } //nolint:gosec // compiled into this binary

func authPageScript() template.JS { return template.JS(authScriptSource) } //nolint:gosec // compiled into this binary

// authPageCSP is a sign-in page's policy. formAction is the page's
// `form-action` source list; brandStyleSource is the brand stylesheet's hash
// source, "" when the page has none.
func authPageCSP(formAction, brandStyleSource string) string {
	styleSources := authStyleCSPSource
	if brandStyleSource != "" {
		styleSources += " " + brandStyleSource
	}
	return "default-src 'none'; base-uri 'none'; form-action " + formAction + "; frame-ancestors 'none'; " +
		"img-src 'self'; font-src 'self'; script-src " + authScriptCSPSource + "; style-src " + styleSources
}
