// Package nativepolicy is the one cached reader of the native client policy
// (ADR-0025 decision 5, WP4). Built once at the composition root and shared by
// its three consumers, so they can never disagree about the policy:
//
//   - the discovery document's public subset and per-client minimums;
//   - the `client_policy` object of every native token response;
//   - the minimum-version gate (`426`) on the API and the token endpoint.
//
// The admin save of the `native_client_policy` section invalidates it, so the
// replica that took the write serves the new policy on its next request; other
// replicas within CacheTTL.
package nativepolicy

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/clientversion"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// CacheTTL bounds how stale a replica's policy may be: the maintenance gate's
// figure, and the same argument — a few seconds of lag against one query per
// request.
const CacheTTL = 10 * time.Second

// loadTimeout bounds one refresh; past it the last good value is kept.
const loadTimeout = 3 * time.Second

// Clients is the registry view the per-client minimums come from
// (*nativeauth.Registry). Nil means no registered client.
type Clients interface {
	Clients(ctx context.Context) ([]nativeauth.Client, error)
}

// Service is the cached policy.
type Service struct {
	load    func(context.Context) (platformconfig.NativeClientPolicy, error)
	clients Clients
	now     func() time.Time

	mu        sync.Mutex
	policy    platformconfig.NativeClientPolicy
	loaded    bool
	expiresAt time.Time
}

// New builds the service over the platform_config store. A nil pool serves the
// defaults (the shape of a database-less test router). clients may be nil.
func New(pool *pgxpool.Pool, clients Clients) *Service {
	return NewWithLoader(func(ctx context.Context) (platformconfig.NativeClientPolicy, error) {
		return platformconfig.LoadNativeClientPolicy(ctx, pool)
	}, clients)
}

// NewWithLoader builds the service over any loader (tests).
func NewWithLoader(load func(context.Context) (platformconfig.NativeClientPolicy, error), clients Clients) *Service {
	return &Service{load: load, clients: clients, now: time.Now}
}

// SetClock replaces the clock. Tests only.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Invalidate drops the cached policy; the next read goes to the store.
func (s *Service) Invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.expiresAt = time.Time{}
	s.mu.Unlock()
}

// Policy returns the full policy.
//
// A failed refresh keeps the LAST GOOD value and re-arms the TTL, as the
// maintenance gate does: one timed-out query must not drop a minimum version or
// a device-lock requirement, and a failing database must not be queried once
// per request. Only a COLD cache with a failing store returns the error.
//
// The query never runs under the lock (see maintenance.go's resolve for why);
// the TTL is re-armed before it, so at most one refresh is in flight.
func (s *Service) Policy(ctx context.Context) (platformconfig.NativeClientPolicy, error) {
	s.mu.Lock()
	now := s.now()
	if s.loaded && now.Before(s.expiresAt) {
		policy := s.policy
		s.mu.Unlock()
		return policy, nil
	}
	previous, hadPrevious := s.policy, s.loaded
	if hadPrevious {
		s.expiresAt = now.Add(CacheTTL)
	}
	s.mu.Unlock()

	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loadTimeout)
	defer cancel()
	policy, err := s.load(loadCtx)
	if err != nil {
		if hadPrevious {
			slog.WarnContext(ctx, "native client policy unreadable; keeping the last known value", "err", err)
			return previous, nil
		}
		return platformconfig.NativeClientPolicy{}, err
	}
	s.mu.Lock()
	s.policy, s.loaded, s.expiresAt = policy, true, s.now().Add(CacheTTL)
	s.mu.Unlock()
	return policy, nil
}

// MinimumFor is the effective minimum version for one client: the higher of
// the policy's deployment-wide minimum and the client's own. "" means none.
// An unknown or disabled client gets the deployment-wide minimum.
func (s *Service) MinimumFor(ctx context.Context, clientID string) (string, error) {
	policy, err := s.Policy(ctx)
	if err != nil {
		return "", err
	}
	if s.clients == nil || clientID == "" {
		return policy.MinClientVersion, nil
	}
	clients, err := s.clients.Clients(ctx)
	if err != nil {
		return "", err
	}
	for _, client := range clients {
		if client.ClientID == clientID {
			return clientversion.Max(policy.MinClientVersion, client.MinClientVersion), nil
		}
	}
	return policy.MinClientVersion, nil
}

// Minimums maps every ENABLED registered client to its effective minimum, for
// the discovery document. A client with no effective minimum is absent: a
// client reads `min_client_version[its id]`, falling back to the public
// policy's deployment-wide value, and both give the same answer.
func (s *Service) Minimums(ctx context.Context) (platformconfig.NativeClientPolicy, map[string]string, error) {
	policy, err := s.Policy(ctx)
	if err != nil {
		return platformconfig.NativeClientPolicy{}, nil, err
	}
	out := map[string]string{}
	if s.clients == nil {
		return policy, out, nil
	}
	clients, err := s.clients.Clients(ctx)
	if err != nil {
		return platformconfig.NativeClientPolicy{}, nil, err
	}
	for _, client := range clients {
		if !client.Enabled {
			continue
		}
		if minimum := clientversion.Max(policy.MinClientVersion, client.MinClientVersion); minimum != "" {
			out[client.ClientID] = minimum
		}
	}
	return policy, out, nil
}

// Decorate puts the FULL policy into a native token response as
// `client_policy`, with `min_client_version` resolved for the client the tokens
// were issued to, so a policy change reaches a client within one access-token
// lifetime (decision 5). Its signature is nativeauth's TokenResponseDecorator.
func (s *Service) Decorate(ctx context.Context, clientID string, body map[string]any) error {
	policy, err := s.Policy(ctx)
	if err != nil {
		return err
	}
	minimum, err := s.MinimumFor(ctx, clientID)
	if err != nil {
		return err
	}
	policy.MinClientVersion = minimum
	body["client_policy"] = policy
	return nil
}
