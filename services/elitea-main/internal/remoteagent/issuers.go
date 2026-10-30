package remoteagent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// EnvTrustedIssuers is the bootstrap allowlist (Helm): entries in the
// trusted_issuers grammar, separated by a comma or a new line. They are
// added to the entries stored in the remote_agents section.
const EnvTrustedIssuers = "ELITEA_REMOTE_AGENT_TRUSTED_ISSUERS"

// ParseEnvTrustedIssuers splits and parses the bootstrap variable. A
// malformed entry refuses boot.
func ParseEnvTrustedIssuers(raw string) ([]platformconfig.TrustedIssuer, error) {
	var entries []string
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' }) {
		if line = strings.TrimSpace(line); line != "" {
			entries = append(entries, line)
		}
	}
	return platformconfig.ParseTrustedIssuers(entries)
}

// KeySetFactory builds the key set of one JWKS URL
// (identityoidc.NewRemoteKeySet in production).
type KeySetFactory func(jwksURL string) (coreoidc.KeySet, error)

// IssuerAllowlist is the TrustedKeys of the platform: the stored issuers
// (read at most once per refresh interval) and the bootstrap ones. Key sets
// are built once per (issuer, JWKS URL) and cache their keys themselves.
type IssuerAllowlist struct {
	settings  Settings
	bootstrap []platformconfig.TrustedIssuer
	factory   KeySetFactory
	refresh   time.Duration
	now       func() time.Time

	mu       sync.Mutex
	loadedAt time.Time
	issuers  map[string]string // issuer -> JWKS URL
	keySets  map[[2]string]coreoidc.KeySet
}

// NewIssuerAllowlist builds the allowlist. settings may be nil (bootstrap
// entries only).
func NewIssuerAllowlist(settings Settings, bootstrap []platformconfig.TrustedIssuer, factory KeySetFactory) *IssuerAllowlist {
	return &IssuerAllowlist{
		settings: settings, bootstrap: bootstrap, factory: factory, refresh: 30 * time.Second, now: time.Now,
		keySets: map[[2]string]coreoidc.KeySet{},
	}
}

// KeySet answers the key set of issuer.
func (a *IssuerAllowlist) KeySet(ctx context.Context, issuer string) (coreoidc.KeySet, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.issuers == nil || a.now().Sub(a.loadedAt) >= a.refresh {
		issuers, err := a.load(ctx)
		if err != nil {
			if a.issuers == nil {
				return nil, false, err
			}
			// Keep the last list: a store hiccup does not drop trust.
			slog.WarnContext(ctx, "remote agent trusted issuers not refreshed; the last list is used")
		} else {
			a.issuers, a.loadedAt = issuers, a.now()
		}
	}
	jwks, ok := a.issuers[issuer]
	if !ok {
		return nil, false, nil
	}
	key := [2]string{issuer, jwks}
	if keys, ok := a.keySets[key]; ok {
		return keys, true, nil
	}
	keys, err := a.factory(jwks)
	if err != nil {
		return nil, false, err
	}
	a.keySets[key] = keys
	return keys, true, nil
}

func (a *IssuerAllowlist) load(ctx context.Context) (map[string]string, error) {
	issuers := map[string]string{}
	for _, entry := range a.bootstrap {
		issuers[entry.Issuer] = entry.JWKSURL
	}
	if a.settings == nil {
		return issuers, nil
	}
	settings, err := a.settings(ctx)
	if err != nil {
		return nil, err
	}
	for _, raw := range settings.TrustedIssuers {
		entry, err := platformconfig.ParseTrustedIssuer(raw)
		if err != nil {
			// Validated on save; a hand-written bad row trusts nothing.
			continue
		}
		if _, bootstrapped := issuers[entry.Issuer]; !bootstrapped {
			issuers[entry.Issuer] = entry.JWKSURL
		}
	}
	return issuers, nil
}
