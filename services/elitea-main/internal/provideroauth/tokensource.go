package provideroauth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/refresh"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialuse"
)

// partitionLabel is the refresh partition label of a provider grant.
const partitionLabel = "elitea/provider-oauth-refresh/v1"

// DefaultSkew is how long before expiry an access token is refreshed. A
// test value, not a customer requirement (gate D5).
const DefaultSkew = 2 * time.Minute

// ErrProviderNotConfigured is a provider type with no OAuth application.
var ErrProviderNotConfigured = errors.New("provideroauth: no OAuth application is configured for the provider")

// AccessToken is a valid provider access token for one grant. It is
// credential material: never log it or put it in durable input.
type AccessToken struct {
	Token string
	// ExpiresAt is zero when the provider stated no lifetime.
	ExpiresAt  time.Time
	GrantID    string
	Generation int64
}

// String keeps the token out of logs.
func (AccessToken) String() string { return "provideroauth.AccessToken{token=[redacted]}" }

// GoString keeps the token out of %#v.
func (t AccessToken) GoString() string { return t.String() }

// ProviderTokenSource answers a valid access token for a provider grant.
// *TokenSource implements it; the credential broker consumes it, so a cache
// can wrap it.
type ProviderTokenSource interface {
	AccessToken(ctx context.Context, key GrantKey) (AccessToken, error)
}

// TokenSource is the PostgreSQL ProviderTokenSource: it reads the grant and
// refreshes it single-flight across replicas when it is within Skew of
// expiry.
type TokenSource struct {
	grants  *Grants
	leases  *refresh.Leases
	coord   *refresh.Coordinator
	clients map[string]Refresher
	skew    time.Duration
	now     func() time.Time
}

// Refresher trades one grant's refresh token for new tokens. *Client is
// the refresher of a provider with one OAuth application; a provider whose
// token endpoint differs per target (a remote MCP server, #1123) seals
// what its refresh needs into the refresh token it hands the grant, and
// opens it again here.
type Refresher interface {
	Provider() string
	Timeout() time.Duration
	Refresh(ctx context.Context, refreshToken string) (TokenResponse, error)
}

// TokenSourceConfig is the token source's dependencies.
type TokenSourceConfig struct {
	Grants      *Grants
	Leases      *refresh.Leases
	Coordinator *refresh.Coordinator
	// Clients are keyed by provider type.
	Clients []*Client
	// Refreshers are the providers with no single client (keyed by provider
	// type like Clients; a provider in both is configured twice).
	Refreshers []Refresher
	// Skew defaults to DefaultSkew.
	Skew time.Duration
}

// NewTokenSource builds the token source.
func NewTokenSource(cfg TokenSourceConfig) (*TokenSource, error) {
	if cfg.Grants == nil || cfg.Leases == nil || cfg.Coordinator == nil {
		return nil, errors.New("provideroauth: grants, leases and a refresh coordinator are required")
	}
	all := make([]Refresher, 0, len(cfg.Clients)+len(cfg.Refreshers))
	for _, c := range cfg.Clients {
		if c == nil {
			return nil, errors.New("provideroauth: a nil provider client")
		}
		all = append(all, c)
	}
	all = append(all, cfg.Refreshers...)
	clients := make(map[string]Refresher, len(all))
	for _, c := range all {
		if c == nil {
			return nil, errors.New("provideroauth: a nil provider client")
		}
		if _, dup := clients[c.Provider()]; dup {
			return nil, fmt.Errorf("provideroauth: provider %q is configured twice", c.Provider())
		}
		// A refresh must finish inside its lease, or another replica takes
		// the lease while the provider rotates the refresh token.
		if c.Timeout() >= cfg.Coordinator.LeaseTTL() {
			return nil, fmt.Errorf("provideroauth: provider %q timeout must be shorter than the refresh lease", c.Provider())
		}
		clients[c.Provider()] = c
	}
	skew := cfg.Skew
	if skew <= 0 {
		skew = DefaultSkew
	}
	return &TokenSource{
		grants: cfg.Grants, leases: cfg.Leases, coord: cfg.Coordinator, clients: clients,
		skew: skew, now: time.Now,
	}, nil
}

var _ ProviderTokenSource = (*TokenSource)(nil)

// AccessToken answers the grant's access token, refreshed when it is within
// the skew of expiry. No grant, an invalid grant, or a refresh token the
// provider refused is ErrConsentRequired.
func (s *TokenSource) AccessToken(ctx context.Context, key GrantKey) (AccessToken, error) {
	if !key.valid() {
		return AccessToken{}, ErrInvalid
	}
	client, ok := s.clients[key.Provider]
	if !ok {
		return AccessToken{}, ErrProviderNotConfigured
	}
	partition := refresh.Partition(partitionLabel, strconv.FormatInt(key.TenantProjectID, 10), key.Provider,
		key.TargetBindingID, strconv.FormatInt(key.SubjectUserID, 10), key.AgentBindingID)
	// latest is the snapshot the flight read last; Read and Refresh run in
	// order on the flight's goroutine.
	var latest snapshot
	token, err := refresh.Do(ctx, s.coord, refresh.Flight[AccessToken]{
		Partition: partition,
		Read: func(ctx context.Context) (refresh.State[AccessToken], error) {
			snap, err := s.grants.load(ctx, key)
			if err != nil {
				return refresh.State[AccessToken]{}, err
			}
			if snap.State != StateActive || snap.AccessToken == "" {
				return refresh.State[AccessToken]{}, ErrConsentRequired
			}
			latest = snap
			fresh := snap.ExpiresAt.IsZero() || s.now().Add(s.skew).Before(snap.ExpiresAt)
			if !fresh && len(snap.refreshSeal) == 0 {
				if s.now().Before(snap.ExpiresAt) {
					// Inside the skew with nothing to refresh with: the
					// token still works until it expires.
					fresh = true
				} else {
					return refresh.State[AccessToken]{}, ErrConsentRequired
				}
			}
			return refresh.State[AccessToken]{Value: tokenOf(snap), Fresh: fresh, Generation: snap.Generation}, nil
		},
		Refresh: func(ctx context.Context, lease refresh.Lease) (AccessToken, error) {
			return s.refresh(ctx, client, lease, latest)
		},
	})
	if err == nil {
		// The credential inventory's last use (ADR-0044 decision 5).
		credentialuse.Default().Touch(credentialuse.KindProviderOAuthGrant, token.GrantID, key.TenantProjectID)
	}
	return token, err
}

func tokenOf(s snapshot) AccessToken {
	return AccessToken{Token: s.AccessToken, ExpiresAt: s.ExpiresAt, GrantID: s.ID, Generation: s.Generation}
}

func (s *TokenSource) refresh(ctx context.Context, client Refresher, lease refresh.Lease, snap snapshot) (AccessToken, error) {
	refreshToken, err := s.grants.openRefresh(snap)
	if err != nil {
		return AccessToken{}, err
	}
	tokens, err := client.Refresh(ctx, refreshToken)
	if errors.Is(err, ErrInvalidGrant) {
		// The refresh token is used, revoked or expired: the grant is dead
		// until the user consents again.
		if err := s.leases.Commit(ctx, lease, func(ctx context.Context, tx pgx.Tx) error {
			return s.grants.markInvalid(ctx, tx, snap)
		}); err != nil {
			return AccessToken{}, err
		}
		return AccessToken{}, ErrConsentRequired
	}
	if err != nil {
		return AccessToken{}, err
	}
	var generation int64
	err = s.leases.Commit(ctx, lease, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		generation, err = s.grants.writeRefreshed(ctx, tx, snap, tokens)
		return err
	})
	if err != nil {
		return AccessToken{}, err
	}
	return AccessToken{Token: tokens.AccessToken, ExpiresAt: tokens.ExpiresAt, GrantID: snap.ID, Generation: generation}, nil
}
