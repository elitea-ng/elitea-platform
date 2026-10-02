package api

import (
	"context"
	"log/slog"
	"os"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
)

const (
	ssoOIDCLoginPath = browserauth.BasePath + "/auth_oidc/login"
	ssoSAMLLoginPath = browserauth.BasePath + "/auth_saml/login"
)

// ssoLoginOptionSource is the one method the sign-in page needs from a single
// sign-on plane. *v2auth.OIDCHandler and *v2auth.SAMLHandler implement it.
type ssoLoginOptionSource interface {
	LoginOption(ctx context.Context) (v2auth.LoginOption, bool)
}

// newSSOChooser composes the single sign-on sign-in page from the two planes.
//
// It returns nil when neither plane is mounted, and the caller then mounts no
// `/forward-auth/login`. The fallback login route is the OIDC one when OIDC is
// mounted, else the SAML one: with no usable provider, that route states that
// single sign-on is not available, as `/forward-auth/login` did before.
func newSSOChooser(
	oidc *v2auth.OIDCHandler,
	saml *v2auth.SAMLHandler,
	brand browserauth.BrandSource,
) *browserauth.SSOChooser {
	if oidc == nil && saml == nil {
		return nil
	}
	fallback := ssoOIDCLoginPath
	if oidc == nil {
		fallback = ssoSAMLLoginPath
	}
	type plane struct {
		id        string
		loginPath string
		source    ssoLoginOptionSource
	}
	// Display order: OIDC, then SAML. A domain listed by both goes to the
	// first, because the page reads the list in this order.
	var planes []plane
	if oidc != nil {
		planes = append(planes, plane{id: "oidc", loginPath: ssoOIDCLoginPath, source: oidc})
	}
	if saml != nil {
		planes = append(planes, plane{id: "saml", loginPath: ssoSAMLLoginPath, source: saml})
	}
	chooser, err := browserauth.NewSSOChooser(browserauth.SSOChooserConfig{
		Providers: func(ctx context.Context) []browserauth.SSOProvider {
			providers := make([]browserauth.SSOProvider, 0, len(planes))
			for _, candidate := range planes {
				option, ok := candidate.source.LoginOption(ctx)
				if !ok {
					continue
				}
				providers = append(providers, browserauth.SSOProvider{
					ID:           candidate.id,
					DisplayName:  option.DisplayName,
					LoginPath:    candidate.loginPath,
					LoginDomains: option.LoginDomains,
				})
			}
			return providers
		},
		FallbackLoginPath: fallback,
		Brand:             brand,
		SecureCookies:     os.Getenv("COOKIE_SECURE") != "false",
	})
	if err != nil {
		// The configuration above is static, so this is a programming error.
		// Logged rather than fatal: the plane's own login routes still work.
		slog.Error("the single sign-on sign-in page could not be composed", "err", err)
		return nil
	}
	return chooser
}
