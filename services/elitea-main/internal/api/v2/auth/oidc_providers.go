package auth

// Where the OIDC login path gets its configuration.
//
// # Why this exists
//
// Until this file, the only federation configuration this service had was four
// environment variables read once at boot (`OIDCConfigFromEnv`). An operator
// could not change an identity provider without a redeploy, and the admin
// Configuration page's Authentication section — which exists to change one —
// refused with a sentence about the Arbiter bus.
//
// `elitea_auth.identity_providers` (shared migration 0095) now holds the
// authored definition, and this file is what READS it. That is the whole point:
// an editor over a store nothing consults is the failure mode
// `internal/api/v2/admin/config_values.go` was written to remove, and adding a
// second instance of it would be worse than leaving the section refused.
//
// # Precedence, and why the environment is the fallback and not the winner
//
// A stored, enabled provider wins. The environment applies only when no such
// row exists. An operator who authors a provider expects it to take effect, and
// a deployment whose environment silently overrode the admin page would be one
// where the page's saves are invisible — the same defect in new clothes.
//
// The environment path is KEPT, and not as a courtesy: it is how a deployment
// bootstraps: the end-to-end stack sets `OIDC_ISSUER_URL` and has no operator
// to author a row, and an existing deployment must keep working across the
// release that adds this table.
//
// # An unreadable table is not "no provider"
//
// `Enabled` distinguishes ErrNotFound from a read failure, and so does this. A
// database outage does NOT fall through to the environment: falling through
// would federate logins through a provider the operator replaced, using a
// credential they may have rotated away from. It fails the login instead.
//
// # The cache, and what invalidates it
//
// Discovery is a network round trip to the identity provider, so a provider is
// constructed once and reused. The key is `provider_key@revision`, and the
// store bumps the revision on every authored write — so a change to the
// document invalidates the entry exactly when the document changed, and never
// otherwise. There is no timer, and no "reload" button that would have to be
// pressed for a save to take effect.
//
// # What still needs a restart, stated rather than hidden
//
// Mounting the `/auth/oidc/*` routes at all is a boot decision:
// `internal/api/production_router.go` allows exactly one browser-auth plane to
// own `/auth`, so which plane owns it cannot change under a running
// process. Authoring the FIRST OIDC provider on a deployment that had none
// therefore needs a restart. Editing, replacing, or disabling one does not. The
// admin save path says which of the two happened.

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/identityproviders"
)

// IdentityProviderSource is the store seam. It is an interface so this package does
// not depend on a database pool to be tested, and so a test can supply a
// provider that changes between calls.
type IdentityProviderSource interface {
	Enabled(ctx context.Context, kind identityproviders.Kind) (identityproviders.Provider, error)
}

// IdentitySecretSource is the vault seam the client secret is read back through.
//
// The secret is READ ON EVERY CACHE MISS and never stored in this package's own
// state beyond the constructed oauth2 configuration. The vault is the one place
// it lives.
type IdentitySecretSource interface {
	LookupAdminHiddenSecret(ctx context.Context, name string) (string, error)
}

// errNoOIDCProvider reports that this deployment federates no OIDC logins:
// neither a stored enabled provider nor an environment configuration.
var errNoOIDCProvider = errors.New("auth: no OIDC identity provider is configured")

// oidcRuntime is one usable provider: everything the login and callback paths
// need, and nothing they do not.
type oidcRuntime struct {
	oauth2Cfg *oauth2.Config
	verifier  *oidc.IDTokenVerifier

	// requireEmailVerified decides whether an ABSENT `email_verified` claim
	// stops a first login. An explicit `false` is refused whatever this says;
	// see Callback.
	requireEmailVerified bool

	// adoptSCIMUsers lets a first login adopt a SCIM-provisioned account. See
	// identityproviders.SAMLDocument.AdoptSCIMUsers; the environment fallback
	// reads OIDC_ADOPT_SCIM_USERS.
	adoptSCIMUsers bool

	// origin names where this runtime came from, for the log line only. An
	// operator debugging a login needs to know whether the deployment used the
	// row they just saved or the environment it fell back to.
	origin string
}

// WithProviderStore attaches the authored provider store and the vault.
//
// Both or neither. A store with no vault could resolve a provider and then send
// an empty client secret to the identity provider, which fails at the far end
// with a message naming neither this service nor the missing credential.
func (h *OIDCHandler) WithProviderStore(providers IdentityProviderSource, secretSource IdentitySecretSource) *OIDCHandler {
	if providers == nil || secretSource == nil {
		return h
	}
	h.providers = providers
	h.secretSource = secretSource
	return h
}

// runtime resolves the provider this request must use.
func (h *OIDCHandler) runtime(ctx context.Context) (*oidcRuntime, error) {
	if h.providers != nil {
		provider, err := h.providers.Enabled(ctx, identityproviders.KindOIDC)
		switch {
		case err == nil:
			return h.runtimeFor(ctx, provider)
		case errors.Is(err, identityproviders.ErrNotFound),
			identityproviders.IsSchemaMissing(err):
			// Fall through to the environment. These are the ONLY two errors
			// that fall through, and they mean the same thing: this deployment
			// has authored no provider. A table that does not exist holds no
			// rows, and a deployment configured through the environment must
			// keep federating logins while its migrations catch up.
		default:
			return nil, fmt.Errorf("read the enabled identity provider: %w", err)
		}
	}
	if h.envRuntime != nil {
		return h.envRuntime, nil
	}
	return nil, errNoOIDCProvider
}

// runtimeFor returns the cached runtime for one authored revision, building it
// on a miss.
func (h *OIDCHandler) runtimeFor(
	ctx context.Context,
	provider identityproviders.Provider,
) (*oidcRuntime, error) {
	if provider.OIDC == nil {
		return nil, fmt.Errorf("the enabled provider %q carries no OIDC document", provider.Key)
	}
	cacheKey := fmt.Sprintf("%s@%d", provider.Key, provider.Revision)

	h.runtimeMu.Lock()
	cached, ok := h.runtimeCache[cacheKey]
	h.runtimeMu.Unlock()
	if ok {
		return cached, nil
	}

	// Built OUTSIDE the lock. Discovery is a network round trip, and holding
	// the lock across it would make every concurrent login wait on the slowest
	// identity provider. Two logins racing a cache miss build two runtimes and
	// one wins the map; both are correct, and the loser is discarded.
	built, err := h.buildRuntime(ctx, provider)
	if err != nil {
		return nil, err
	}

	h.runtimeMu.Lock()
	defer h.runtimeMu.Unlock()
	if existing, ok := h.runtimeCache[cacheKey]; ok {
		return existing, nil
	}
	// The map holds ONE entry. A superseded revision is never used again — the
	// store returns the current one — so keeping it would be an unbounded map
	// keyed by how many times an operator has pressed save.
	h.runtimeCache = map[string]*oidcRuntime{cacheKey: built}
	return built, nil
}

func (h *OIDCHandler) buildRuntime(
	ctx context.Context,
	provider identityproviders.Provider,
) (*oidcRuntime, error) {
	document := provider.OIDC

	clientSecret := ""
	if provider.SecretRef != "" {
		secret, err := h.secretSource.LookupAdminHiddenSecret(ctx, provider.SecretRef)
		if err != nil {
			// A definition that names a secret and cannot read it is NOT
			// downgraded to a public client. A public client is a different
			// registration at the identity provider, and attempting one would
			// fail at the token endpoint with an error about client
			// authentication that named nothing about this vault.
			return nil, fmt.Errorf("read the client secret for provider %q: %w", provider.Key, err)
		}
		clientSecret = secret
	}

	discovered, err := oidc.NewProvider(ctx, document.Issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery failed for %s: %w", document.Issuer, err)
	}

	scopes := document.Scopes
	if len(scopes) == 0 {
		// Validate guarantees `openid`; this covers a row written before it.
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}

	return &oidcRuntime{
		oauth2Cfg: &oauth2.Config{
			ClientID:     document.ClientID,
			ClientSecret: clientSecret,
			RedirectURL:  document.RedirectURI,
			Endpoint:     discovered.Endpoint(),
			Scopes:       scopes,
		},
		verifier:             discovered.Verifier(&oidc.Config{ClientID: document.ClientID}),
		requireEmailVerified: document.RequireEmailVerified,
		adoptSCIMUsers:       document.AdoptSCIMUsers,
		origin:               fmt.Sprintf("provider %s revision %d", provider.Key, provider.Revision),
	}, nil
}

// newOIDCRuntimeFromEnvironment builds the fallback runtime.
//
// It performs discovery at BOOT, as it always has, so a deployment whose
// environment names an unreachable issuer fails to start rather than failing at
// the first login.
func newOIDCRuntimeFromEnvironment(ctx context.Context, cfg *OIDCConfig) (*oidcRuntime, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery failed for %s: %w", cfg.IssuerURL, err)
	}
	return &oidcRuntime{
		oauth2Cfg: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURI,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier:             provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		requireEmailVerified: oidcRequiresVerifiedEmail(),
		adoptSCIMUsers:       oidcAdoptsSCIMUsers(),
		origin:               "environment",
	}, nil
}

// HasEnabledOIDCProvider reports whether a stored, enabled OIDC provider exists.
//
// The composition root calls it at boot to decide whether to MOUNT the OIDC
// browser routes at all — a decision that cannot be made per request, because
// exactly one browser-auth plane may own `/auth`.
//
// A read failure is returned, not swallowed. A deployment that cannot read its
// provider table must not silently start as an unfederated one.
func HasEnabledOIDCProvider(ctx context.Context, providers IdentityProviderSource) (bool, error) {
	if providers == nil {
		return false, nil
	}
	_, err := providers.Enabled(ctx, identityproviders.KindOIDC)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, identityproviders.ErrNotFound):
		return false, nil
	default:
		// A missing table reaches the caller as an error, not as `false`. The
		// composition root distinguishes it (identityproviders.IsSchemaMissing)
		// and logs it, because starting without single sign-on is worth saying
		// out loud even when it is the right thing to do.
		return false, err
	}
}
