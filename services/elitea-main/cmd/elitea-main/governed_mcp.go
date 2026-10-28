package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5/pgxpool"

	v2mcp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/mcp"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	mcpadapter "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters/mcp"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/governedmcp"
	identityoidc "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/oidc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
)

// The governed MCP variables (#1123), one constant each so a search for a
// variable finds where it is read.
const (
	governedMCPInboundIssuerEnv    = "ELITEA_GOVERNED_MCP_INBOUND_ISSUER"
	governedMCPInboundJWKSURLEnv   = "ELITEA_GOVERNED_MCP_INBOUND_JWKS_URL"
	governedMCPInboundScopesEnv    = "ELITEA_GOVERNED_MCP_INBOUND_SCOPES"
	governedMCPInboundUserClaimEnv = "ELITEA_GOVERNED_MCP_INBOUND_USER_CLAIM"
	governedMCPInboundOAuthOnlyEnv = "ELITEA_GOVERNED_MCP_INBOUND_OAUTH_ONLY"
	governedMCPOAuthEnabledEnv     = "ELITEA_GOVERNED_MCP_OAUTH_ENABLED"
	governedMCPAllowDCREnv         = "ELITEA_GOVERNED_MCP_ALLOW_DYNAMIC_REGISTRATION"
)

// governedMCP is the governed MCP composition (ADR-0039, issue #1123):
// the inbound resource server and the outbound MCP OAuth service with its
// authorization references. Every field is nil with the flag off.
type governedMCP struct {
	Inbound    *v2mcp.GovernedInbound
	Outbound   *governedmcp.Service
	References *governedmcp.References
}

// authorizations is the broker's reference minter: a real nil when
// outbound MCP OAuth is not composed, never a nil pointer in the interface.
func (g governedMCP) authorizations() provideroauth.AuthorizationReferences {
	if g.References == nil {
		return nil
	}
	return g.References
}

// governedMCPDescriptors are the adapter descriptors governed MCP adds to
// the operation catalogue. The catalogue is pure data, built whatever the
// flag; only a governed run's broker and decider read it.
func governedMCPDescriptors() []credentialadapters.AdapterDescriptor {
	return []credentialadapters.AdapterDescriptor{mcpadapter.Descriptor()}
}

// governedMCPAtBoot builds governed MCP. Flag off: nothing is read or
// built, so MCP authentication, routes and responses are unchanged and no
// metadata route mounts. Flag on:
//
//   - inbound, with ELITEA_GOVERNED_MCP_INBOUND_ISSUER set: the RFC 9728
//     metadata, the resource_metadata challenge and audience-checked tokens.
//     ELITEA_GOVERNED_MCP_INBOUND_JWKS_URL is then required, as is an https
//     DEPLOYMENT_URL (the resource URI cannot follow Host).
//   - outbound, with ELITEA_GOVERNED_MCP_OAUTH_ENABLED=true: remote MCP
//     OAuth held in provider OAuth custody and released by the broker. It
//     needs DEPLOYMENT_URL and the vault master key.
//
// A partial or malformed configuration refuses boot.
func governedMCPAtBoot(
	admission profile.Admission,
	lookup func(string) (string, bool),
	pool *pgxpool.Pool,
	masterKey func() ([]byte, error),
	publicOrigin string,
	logger *slog.Logger,
) (governedMCP, error) {
	if !admission.Governed() {
		return governedMCP{}, nil
	}
	get := func(name string) string {
		value, _ := lookup(name)
		return strings.TrimSpace(value)
	}
	flag := func(name string) (bool, error) {
		raw := get(name)
		if raw == "" {
			return false, nil
		}
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return false, fmt.Errorf("%s must be true or false", name)
		}
		return value, nil
	}
	var out governedMCP
	origin := strings.TrimSuffix(publicOrigin, "/")

	if issuer := get(governedMCPInboundIssuerEnv); issuer != "" {
		jwksURL := get(governedMCPInboundJWKSURLEnv)
		if jwksURL == "" {
			return governedMCP{}, fmt.Errorf("%s needs %s", governedMCPInboundIssuerEnv, governedMCPInboundJWKSURLEnv)
		}
		oauthOnly, err := flag(governedMCPInboundOAuthOnlyEnv)
		if err != nil {
			return governedMCP{}, err
		}
		keys, err := identityoidc.NewRemoteKeySet(identityoidc.RemoteKeySetConfig{
			JWKSURL: jwksURL, SupportedSigningAlgorithms: []string{"RS256", "ES256", "PS256"},
		}, nil)
		if err != nil {
			return governedMCP{}, fmt.Errorf("%s: %w", governedMCPInboundJWKSURLEnv, err)
		}
		verifier, err := v2mcp.NewOAuthVerifier(v2mcp.OAuthVerifierConfig{
			Issuers:        map[string]coreoidc.KeySet{issuer: keys},
			RequiredScopes: strings.Fields(get(governedMCPInboundScopesEnv)),
			UserClaim:      get(governedMCPInboundUserClaimEnv),
		})
		if err != nil {
			return governedMCP{}, err
		}
		users, err := v2mcp.NewPostgresInboundUsers(pool)
		if err != nil {
			return governedMCP{}, err
		}
		out.Inbound, err = v2mcp.NewGovernedInbound(v2mcp.GovernedInboundConfig{
			PublicOrigin: origin, Verifier: verifier, Users: users, OAuthOnly: oauthOnly, Logger: logger,
		})
		if err != nil {
			return governedMCP{}, err
		}
		logger.Info("governed MCP inbound OAuth enabled", "issuer", issuer, "oauth_only", oauthOnly)
	}

	enabled, err := flag(governedMCPOAuthEnabledEnv)
	if err != nil || !enabled {
		return out, err
	}
	if !strings.HasPrefix(origin, "https://") {
		return governedMCP{}, errors.New("governed MCP OAuth needs an https DEPLOYMENT_URL for its redirect URI and client metadata")
	}
	allowDCR, err := flag(governedMCPAllowDCREnv)
	if err != nil {
		return governedMCP{}, err
	}
	out.Outbound, err = governedmcp.New(governedmcp.Config{
		RedirectURI:              origin + provideroauth.CallbackPathPrefix + governedmcp.ProviderType + provideroauth.CallbackPathSuffix,
		ClientMetadataURL:        origin + governedmcp.ClientMetadataPath,
		AllowDynamicRegistration: allowDCR,
	})
	if err != nil {
		return governedMCP{}, err
	}
	if masterKey == nil {
		return governedMCP{}, errors.New("governed MCP OAuth needs the vault master key")
	}
	key, err := masterKey()
	if err != nil || len(key) == 0 {
		return governedMCP{}, errors.New("governed MCP OAuth needs the vault master key (SECRETS_MASTER_KEY)")
	}
	defer clear(key)
	if out.References, err = governedmcp.NewReferences(key, 0); err != nil {
		return governedMCP{}, err
	}
	logger.Info("governed MCP outbound OAuth enabled", "dynamic_registration", allowDCR)
	return out, nil
}
