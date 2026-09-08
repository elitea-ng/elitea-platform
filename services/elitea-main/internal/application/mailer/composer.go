// Package mailer composes branded outbound e-mail (ADR-0024 WP7, decision 7).
//
// The transport (internal/infra/mailer) submits a message; this package
// decides what the message says and how it looks. Every message is rendered
// from an embedded template at SEND time against the resolved brand pack, so
// a rebrand changes the next e-mail with no deploy:
//
//   - the product name is the heading, the footer and the display sender;
//   - the logo is `assets.logoEmail` — a RASTER, because mail clients render
//     neither SVG nor a relative path — absolutised against PUBLIC_BASE_URL;
//   - colours are a small derived set (brand, on-brand, ground, ink, muted,
//     rule, surface) inlined per element, because mail clients strip
//     stylesheets and know nothing of CSS variables;
//   - the font family is the pack's, with the same system fallback the login
//     page uses.
//
// # What this package refuses
//
// A shadow-mode deployment (the Go service mirroring legacy traffic for
// comparison) must produce no side effect a user can see, and the security
// verification specification names e-mail first among them. A Composer built
// with Suppressed=true renders and returns ErrSuppressed without dialling.
//
// # The invitation link is the login URL
//
// The ADR names a signed, expiring invitation token. Nothing in this service
// consumes one: a Form deployment signs the user in with the credential the
// operator issued, an OIDC/SAML deployment with the identity provider's, and
// in both the account is resolved BY E-MAIL on first login
// (internal/db/queries/auth_provisioning.sql). A token in the link would be
// a parameter no route reads. The link is therefore the deployment's public
// URL, and a token arrives with an acceptance screen when one exists — stated
// here rather than shipped as decoration.
package mailer

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/mail"
	"net/url"
	"strings"
	texttemplate "text/template"

	v2branding "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
	transport "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/mailer"
)

//go:embed templates/*.html templates/*.txt
var templateFS embed.FS

// BrandSource is the seam to the resolved pack; the bootstrap route's
// resolver satisfies it.
type BrandSource interface {
	Current(ctx context.Context) v2branding.Snapshot
}

// ErrSuppressed is returned when sending is disabled by shadow mode.
var ErrSuppressed = errors.New("outbound e-mail is suppressed on a shadow deployment")

// TransportResolver supplies the transport for ONE send.
//
// It exists because outbound e-mail is configurable at runtime (gap G7):
// `internal/emailsettings` merges the admin page's rows over the environment
// defaults, so a corrected relay host takes effect on the next message rather
// than on the next deployment. The resolver is consulted per send, never
// cached here — a Composer that remembered the first answer would be the
// boot-time read this change removed, one layer up.
//
// The seam is declared with plain values rather than the settings package's
// own type so that this package does not import it; the dependency runs one
// way, from the resolver to the transport.
type TransportResolver interface {
	// Transport returns the transport to submit through, the public base URL
	// links are absolutised against, and whether a message can be sent at all.
	Transport(ctx context.Context) (transport.Transport, string, bool)
}

// Config wires a Composer.
type Config struct {
	Transport transport.Transport
	Brand     BrandSource
	// PublicBaseURL is the deployment's browser-facing origin; asset paths
	// and the action link are absolutised against it.
	PublicBaseURL string
	// Suppressed renders but never sends (shadow mode).
	Suppressed bool
	// Resolver, when set, decides the transport and the base URL per send.
	// Transport and PublicBaseURL above then serve only as the answer for a
	// resolver that reports nothing configured.
	Resolver TransportResolver
}

// Composer renders and sends.
type Composer struct {
	transport  transport.Transport
	brand      BrandSource
	baseURL    string
	suppressed bool
	resolver   TransportResolver
	html       *template.Template
	text       *texttemplate.Template
}

// New parses the embedded templates once. A nil transport is replaced by the
// null transport, which reports ErrNotConfigured on every send.
func New(config Config) (*Composer, error) {
	if config.Transport == nil {
		config.Transport = transport.NullTransport{}
	}
	base := strings.TrimRight(strings.TrimSpace(config.PublicBaseURL), "/")
	if base != "" {
		parsed, err := url.Parse(base)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, fmt.Errorf("mailer: PUBLIC_BASE_URL must be an absolute http(s) origin, got %q", config.PublicBaseURL)
		}
	}
	html, err := template.ParseFS(templateFS, "templates/base.html")
	if err != nil {
		return nil, fmt.Errorf("mailer: parse base template: %w", err)
	}
	text, err := texttemplate.ParseFS(templateFS, "templates/*.txt")
	if err != nil {
		return nil, fmt.Errorf("mailer: parse text templates: %w", err)
	}
	return &Composer{
		transport:  config.Transport,
		brand:      config.Brand,
		baseURL:    base,
		suppressed: config.Suppressed,
		resolver:   config.Resolver,
		html:       html,
		text:       text,
	}, nil
}

// resolved is one send's effective transport and origin.
type resolved struct {
	transport  transport.Transport
	baseURL    string
	configured bool
}

// resolve answers what this send may use.
//
// It takes a context because the resolver reads the database, and it is called
// on EVERY send rather than once: the whole point of the settings surface is
// that a save takes effect without a restart, and a Composer that resolved at
// construction would have reintroduced the restart.
func (c *Composer) resolve(ctx context.Context) resolved {
	if c.resolver != nil {
		chosen, base, configured := c.resolver.Transport(ctx)
		if configured {
			if base == "" {
				base = c.baseURL
			}
			return resolved{transport: chosen, baseURL: strings.TrimRight(base, "/"), configured: true}
		}
		// The resolver is authoritative when it can answer. When it cannot,
		// the statically wired transport is still the deployment's, so it is
		// the fallback rather than an immediate refusal.
		if base != "" {
			return resolved{transport: c.transport, baseURL: strings.TrimRight(base, "/"), configured: c.staticConfigured()}
		}
	}
	return resolved{transport: c.transport, baseURL: c.baseURL, configured: c.staticConfigured()}
}

// staticConfigured is the pre-G7 test: a transport that is not the null one.
func (c *Composer) staticConfigured() bool {
	_, null := c.transport.(transport.NullTransport)
	return !null
}

// Configured reports whether a message can actually be submitted — the fact
// the invite handlers report as `invitation_delivered`'s precondition.
//
// It takes a context because the answer now depends on rows an administrator
// can change while the process runs. A parameterless version would have had to
// answer from boot-time state, which is exactly the staleness this surface
// exists to remove.
func (c *Composer) Configured(ctx context.Context) bool {
	if c.suppressed {
		return false
	}
	return c.resolve(ctx).configured
}

// Invitation is the data an invitation e-mail carries.
type Invitation struct {
	Email       string
	Name        string
	InvitedBy   string
	ProjectName string
}

// ModerationDecision is the data a moderation notice carries.
type ModerationDecision struct {
	Email   string
	Message string
}

// SendInvitation mails the invitation.
//
// The recipient is the invited address and the action link is the deployment's
// sign-in URL under the RESOLVED public base URL — so an operator who corrects
// that origin on Admin › E-mail fixes the link in the next invitation, not in
// the next release.
func (c *Composer) SendInvitation(ctx context.Context, invitation Invitation) error {
	send := c.resolve(ctx)
	brand := c.brandView(ctx, send.baseURL)
	brand.Invitation = invitation
	brand.Subject = "You've been invited to " + brand.ProductName
	brand.ActionLabel = "Sign in to " + brand.ProductName
	brand.ActionURL = absolute(send.baseURL, "/")
	return c.send(ctx, send, "invitation", invitation.Email, invitation.Name, brand)
}

// SendModerationDecision mails the pre-rendered decision sentence the
// notification row already carries.
func (c *Composer) SendModerationDecision(ctx context.Context, decision ModerationDecision) error {
	send := c.resolve(ctx)
	brand := c.brandView(ctx, send.baseURL)
	brand.Moderation = decision
	brand.Subject = brand.ProductName + ": moderation decision"
	brand.ActionLabel = "Open " + brand.ProductName
	brand.ActionURL = absolute(send.baseURL, "/")
	return c.send(ctx, send, "moderation", decision.Email, "", brand)
}

// SendTest mails the test message the E-mail and Branding pages both offer.
//
// It goes through the SAME resolution as every other message, which is what
// makes the button a test of the settings rather than a test of the process's
// environment: an operator who has just saved a relay host presses it and
// finds out whether that host works.
func (c *Composer) SendTest(ctx context.Context, to string) error {
	send := c.resolve(ctx)
	brand := c.brandView(ctx, send.baseURL)
	brand.Subject = brand.ProductName + ": test e-mail"
	brand.ActionLabel = "Open " + brand.ProductName
	brand.ActionURL = absolute(send.baseURL, "/")
	return c.send(ctx, send, "test", to, "", brand)
}

// Render returns the HTML and text bodies without sending — the preview and
// the tests use it.
func (c *Composer) Render(ctx context.Context, kind string, view View) (html, text string, err error) {
	return c.render(kind, view)
}

// PreviewKinds are the messages Preview can render.
var PreviewKinds = []string{"invitation", "moderation", "test"}

// Preview renders one message kind with placeholder data under the pack a
// branding package carries, for its preview folder (ADR-0024 decision 9).
// The brand view is derived from `pack` rather than the live resolver, so a
// package previews the brand it contains.
func (c *Composer) Preview(pack *v2branding.Pack, kind string) (html, text string, err error) {
	view := c.viewFor(pack, c.baseURL)
	switch kind {
	case "invitation":
		view.Invitation = Invitation{Email: "ada@example.com", Name: "Ada", InvitedBy: "grace@example.com", ProjectName: "Example project"}
		view.Subject = "You've been invited to " + view.ProductName
		view.ActionLabel = "Sign in to " + view.ProductName
	case "moderation":
		view.Moderation = ModerationDecision{Email: "ada@example.com", Message: "Your application moderation request has been approved."}
		view.Subject = view.ProductName + ": moderation decision"
		view.ActionLabel = "Open " + view.ProductName
	case "test":
		view.Subject = view.ProductName + ": test e-mail"
		view.ActionLabel = "Open " + view.ProductName
	default:
		return "", "", fmt.Errorf("mailer: unknown preview kind %q", kind)
	}
	view.ActionURL = absolute(c.baseURL, "/")
	return c.render(kind, view)
}

// View is everything a template can reach.
type View struct {
	Subject      string
	ProductName  string
	SupportEmail string
	LogoURL      string
	FontFamily   string
	Radius       float64
	Colors       Colors
	ActionURL    string
	ActionLabel  string
	Invitation   Invitation
	Moderation   ModerationDecision
	senderName   string
}

// Colors is the derived set an e-mail inlines. Mail clients have no CSS
// variables and no dark scheme worth relying on, so it is one light set.
type Colors struct {
	Brand, OnBrand, Ground, Surface, Ink, Muted, Rule string
}

// defaultColors is the product's light ground; the brand hue replaces Brand.
var defaultColors = Colors{
	Brand: "#0f7c8c", OnBrand: "#ffffff", Ground: "#f4f6f8", Surface: "#ffffff",
	Ink: "#1c2230", Muted: "#6f7a83", Rule: "#e1e5ea",
}

// BrandView derives the view from the resolved pack, or the product default
// when nothing is served.
func (c *Composer) brandView(ctx context.Context, baseURL string) View {
	view := View{
		ProductName: "Elitea",
		FontFamily:  `Helvetica Neue, Helvetica, Arial, sans-serif`,
		Radius:      8,
		Colors:      defaultColors,
	}
	if c.brand == nil {
		return view
	}
	snapshot := c.brand.Current(ctx)
	return c.viewFor(snapshot.Pack, baseURL)
}

// viewFor derives the view from one pack; nil means the product default.
//
// baseURL is a PARAMETER rather than the Composer's field because the origin
// is resolved per send (gap G7): the logo in a message must be absolutised
// against the origin that message's configuration names, not against the one
// the process booted with.
func (c *Composer) viewFor(pack *v2branding.Pack, baseURL string) View {
	view := View{
		ProductName: "Elitea",
		FontFamily:  `Helvetica Neue, Helvetica, Arial, sans-serif`,
		Radius:      8,
		Colors:      defaultColors,
	}
	if pack == nil {
		return view
	}
	if name := strings.TrimSpace(pack.Product.Name); name != "" {
		view.ProductName = name
	}
	view.senderName = view.ProductName
	if pack.Product.SenderName != nil && strings.TrimSpace(*pack.Product.SenderName) != "" {
		view.senderName = strings.TrimSpace(*pack.Product.SenderName)
	}
	if pack.Product.SupportEmail != nil {
		if address, err := mail.ParseAddress(*pack.Product.SupportEmail); err == nil && address.Address == *pack.Product.SupportEmail {
			view.SupportEmail = address.Address
		}
	}
	if hue := safeHex(pack.Brand.Hue); hue != "" {
		view.Colors.Brand = hue
		if pack.Brand.OnBrand != nil {
			if on := safeHex(*pack.Brand.OnBrand); on != "" {
				view.Colors.OnBrand = on
			}
		}
	}
	if family := safeFontFamily(pack.Typography.FontFamily); family != "" {
		// Unquoted: html/template treats a style attribute as CSS and a
		// quoted family list as a string it must escape, which mail clients
		// then render literally. Single-word families need no quotes.
		view.FontFamily = strings.NewReplacer(`"`, "", "'", "").Replace(family) + ", Helvetica, Arial, sans-serif"
	}
	if radius := pack.Shape.RadiusMd; radius >= 0 && radius <= 64 {
		view.Radius = radius
	}
	if pack.Assets.LogoEmail != nil {
		if path := safeAssetPath(*pack.Assets.LogoEmail); path != "" && baseURL != "" {
			view.LogoURL = baseURL + path
		}
	}
	return view
}

// absolute joins a resolved origin and a path. An empty origin yields an empty
// link rather than a relative one: a relative href in an e-mail resolves
// against the mail client, which is nowhere.
func absolute(baseURL, path string) string {
	if baseURL == "" {
		return ""
	}
	return baseURL + path
}

func (c *Composer) render(kind string, view View) (string, string, error) {
	page, err := c.html.Clone()
	if err != nil {
		return "", "", err
	}
	if _, err := page.ParseFS(templateFS, "templates/"+kind+".html"); err != nil {
		return "", "", fmt.Errorf("mailer: parse %s template: %w", kind, err)
	}
	var html bytes.Buffer
	if err := page.ExecuteTemplate(&html, "base", view); err != nil {
		return "", "", fmt.Errorf("mailer: render %s html: %w", kind, err)
	}
	var text bytes.Buffer
	if err := c.text.ExecuteTemplate(&text, kind+".txt", view); err != nil {
		return "", "", fmt.Errorf("mailer: render %s text: %w", kind, err)
	}
	return html.String(), strings.TrimSpace(text.String()) + "\n", nil
}

// send submits one rendered message through the transport this send resolved.
//
// The transport is a PARAMETER rather than the Composer's field, and the
// caller resolved it before building the view: the view's logo and action link
// are absolutised against the same resolution, so a message cannot carry one
// deployment's origin over another deployment's relay.
func (c *Composer) send(ctx context.Context, send resolved, kind, to, toName string, view View) error {
	address, err := mail.ParseAddress(to)
	if err != nil || address.Address != to {
		return fmt.Errorf("mailer: recipient %q is not a plain address", to)
	}
	html, text, err := c.render(kind, view)
	if err != nil {
		return err
	}
	if c.suppressed {
		slog.Info("mailer: suppressed on a shadow deployment", "kind", kind, "to", to)
		return ErrSuppressed
	}
	err = send.transport.Send(ctx, transport.Message{
		To:       mail.Address{Name: toName, Address: address.Address},
		Subject:  view.Subject,
		Text:     text,
		HTML:     html,
		FromName: view.senderName,
	})
	if err != nil {
		return err
	}
	slog.Info("mailer: sent", "kind", kind, "to", to)
	return nil
}

// --- allowlists, the login page's, repeated here because e-mail inlines
// --- values into attributes and style strings with no CSP behind them.

func safeHex(value string) string {
	value = strings.TrimSpace(value)
	if len(value) != 7 || value[0] != '#' {
		return ""
	}
	for _, r := range value[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return ""
		}
	}
	return strings.ToLower(value)
}

func safeFontFamily(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 200 {
		return ""
	}
	for _, r := range value {
		switch {
		case r == ' ', r == ',', r == '\'', r == '"', r == '-', r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return ""
		}
	}
	return value
}

func safeAssetPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return ""
	}
	for _, r := range value {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`"'()\<>`, r) {
			return ""
		}
	}
	return value
}
