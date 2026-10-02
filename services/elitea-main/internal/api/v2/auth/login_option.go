package auth

// What the sign-in page needs to know about each single sign-on plane.
//
// The page (internal/api/browserauth/chooser.go) shows one button per USABLE
// provider and routes a typed work email by its domain. "Usable" is decided
// here, from the same sources the login routes read, so the page cannot offer
// a provider whose login route would answer 503 for want of a definition.
//
// Usable means DEFINED, not REACHABLE. Deciding reachability would need OIDC
// discovery — a network round trip to the identity provider — on every page
// view. A defined provider whose identity provider is down still gets its
// button, and its login route says that single sign-on is not available.

import (
	"context"
	"errors"
	"log/slog"
	"net/mail"
	"os"
	"strings"
	"unicode"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/identityproviders"
)

// LoginOption is one plane's entry on the sign-in page.
type LoginOption struct {
	// DisplayName labels the button: "Continue with <DisplayName>".
	DisplayName string
	// LoginDomains route a typed work email to this plane. Lower case.
	LoginDomains []string
}

// DefaultOIDCDisplayName labels an OIDC provider configured only through the
// environment, which has no authored display name.
const DefaultOIDCDisplayName = "Single sign-on"

// oidcEnvironmentOption reads the environment fallback's presentation once,
// at construction. OIDC_DISPLAY_NAME labels the button. OIDC_LOGIN_DOMAINS is
// a comma- or space-separated domain list. An invalid list is logged and
// ignored: a presentation setting must not stop a deployment from starting.
func oidcEnvironmentOption() LoginOption {
	option := LoginOption{DisplayName: strings.TrimSpace(os.Getenv("OIDC_DISPLAY_NAME"))}
	if option.DisplayName == "" {
		option.DisplayName = DefaultOIDCDisplayName
	}
	if raw := strings.TrimSpace(os.Getenv("OIDC_LOGIN_DOMAINS")); raw != "" {
		domains, err := identityproviders.NormalizeLoginDomains(strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ';' || unicode.IsSpace(r)
		}))
		if err != nil {
			slog.Error("OIDC_LOGIN_DOMAINS is ignored", "err", err)
		} else {
			option.LoginDomains = domains
		}
	}
	return option
}

// LoginOption reports this plane's sign-in page entry, and false when no OIDC
// provider is defined. A stored, enabled provider wins over the environment,
// as it does for the login route (oidc_providers.go).
//
// A store read failure is logged and reported as false. The page then offers
// the other plane, and this plane's own route still reports the failure to a
// browser that reaches it.
func (h *OIDCHandler) LoginOption(ctx context.Context) (LoginOption, bool) {
	if h == nil {
		return LoginOption{}, false
	}
	if h.providers != nil {
		provider, err := h.providers.Enabled(ctx, identityproviders.KindOIDC)
		switch {
		case err == nil && provider.OIDC != nil:
			return LoginOption{
				DisplayName:  provider.DisplayName,
				LoginDomains: provider.OIDC.LoginDomains,
			}, true
		case err == nil:
			slog.Error("OIDC: the enabled provider carries no OIDC document", "provider", provider.Key)
			return LoginOption{}, false
		case errors.Is(err, identityproviders.ErrNotFound), identityproviders.IsSchemaMissing(err):
		default:
			slog.Error("OIDC: the sign-in page could not read the enabled provider", "err", err)
			return LoginOption{}, false
		}
	}
	if h.envRuntime != nil {
		return h.envOption, true
	}
	return LoginOption{}, false
}

// LoginOption reports this plane's sign-in page entry, and false when no SAML
// provider is enabled. SAML has no environment fallback.
func (h *SAMLHandler) LoginOption(ctx context.Context) (LoginOption, bool) {
	if h == nil || h.providers == nil {
		return LoginOption{}, false
	}
	provider, err := h.providers.Enabled(ctx, identityproviders.KindSAML)
	switch {
	case err == nil && provider.SAML != nil:
		return LoginOption{
			DisplayName:  provider.DisplayName,
			LoginDomains: provider.SAML.LoginDomains,
		}, true
	case err == nil:
		slog.Error("SAML: the enabled provider carries no SAML document", "provider", provider.Key)
	case errors.Is(err, identityproviders.ErrNotFound), identityproviders.IsSchemaMissing(err):
	default:
		slog.Error("SAML: the sign-in page could not read the enabled provider", "err", err)
	}
	return LoginOption{}, false
}

// maxLoginHintBytes is the longest address RFC 5321 permits in a path.
const maxLoginHintBytes = 254

// loginHint returns the `login_hint` query value when it is one plain email
// address, and "" otherwise.
//
// The hint is forwarded to the identity provider, which pre-fills its own
// sign-in form with it. It is a convenience, never an assertion: the identity
// provider authenticates whoever signs in. The check keeps a caller from
// passing anything but an address through this service to the provider.
func loginHint(raw string) string {
	hint := strings.TrimSpace(raw)
	if hint == "" || len(hint) > maxLoginHintBytes || strings.ContainsFunc(hint, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == '<' || r == '>' || r == '"'
	}) {
		return ""
	}
	parsed, err := mail.ParseAddress(hint)
	if err != nil || parsed.Name != "" || parsed.Address != hint || strings.Count(hint, "@") != 1 {
		return ""
	}
	return hint
}
