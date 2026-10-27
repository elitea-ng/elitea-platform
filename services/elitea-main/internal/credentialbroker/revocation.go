package credentialbroker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/authzcompose"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/cache"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
)

// Circuit breaker settings, per dependency (#1121). Test values: a breaker
// opens after breakerTrips consecutive failures and half-opens after
// breakerOpenFor. While open, every new redemption that needs the
// dependency is denied BACKEND_UNAVAILABLE; nothing cached is served in its
// place.
const (
	breakerTrips   = 5
	breakerOpenFor = 10 * time.Second
	// staticEntryTTL is how long a static secret stays cached when the
	// policy revision allows it: the envelope's own maximum lifetime.
	staticEntryTTL = MaxEnvelopeTTL
)

func breakerSettings(name string, isSuccessful func(error) bool) gobreaker.Settings {
	return gobreaker.Settings{
		Name:        name,
		MaxRequests: 1,
		Timeout:     breakerOpenFor,
		ReadyToTrip: func(c gobreaker.Counts) bool { return c.ConsecutiveFailures >= breakerTrips },
		// A caller that gave up (cancelled, or its own deadline passed)
		// says nothing about the dependency.
		IsExcluded:   func(err error) bool { return errors.Is(err, errCallerGone) },
		IsSuccessful: isSuccessful,
	}
}

// errCallerGone marks a failure that happened after the caller's context
// ended: a breaker never counts it.
var errCallerGone = errors.New("credential broker: the caller's context ended")

// callerAware runs fn and, when it failed after ctx ended, wraps the error
// so the breaker excludes it. errors.Is still sees the original error.
func callerAware[T any](ctx context.Context, fn func() (T, error)) func() (T, error) {
	return func() (T, error) {
		v, err := fn()
		if err != nil && ctx.Err() != nil {
			return v, errors.Join(errCallerGone, err)
		}
		return v, err
	}
}

func breakerOpen(err error) bool {
	return errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests)
}

// maxIdPBreakers bounds the per-tenant identity provider breakers one
// replica keeps; past it, the least recently created is dropped.
const maxIdPBreakers = 1024

// breakerExchanger trips on identity provider unavailability only: an
// interaction requirement, a claims challenge or a rejection is the IdP
// answering. There is one breaker per Entra tenant, so one customer's
// outage never denies another customer's exchanges.
type breakerExchanger struct {
	next     obo.Exchanger
	mu       sync.Mutex
	breakers map[string]*gobreaker.CircuitBreaker[obo.AccessToken]
	order    []string
}

func newBreakerExchanger(next obo.Exchanger) *breakerExchanger {
	return &breakerExchanger{next: next, breakers: map[string]*gobreaker.CircuitBreaker[obo.AccessToken]{}}
}

func (e *breakerExchanger) breaker(tenant string) *gobreaker.CircuitBreaker[obo.AccessToken] {
	e.mu.Lock()
	defer e.mu.Unlock()
	if b, ok := e.breakers[tenant]; ok {
		return b
	}
	if len(e.order) >= maxIdPBreakers {
		delete(e.breakers, e.order[0])
		e.order = e.order[1:]
	}
	b := gobreaker.NewCircuitBreaker[obo.AccessToken](breakerSettings(
		"credential-broker-idp", func(err error) bool { return err == nil || !errors.Is(err, obo.ErrUnavailable) }))
	e.breakers[tenant] = b
	e.order = append(e.order, tenant)
	return b
}

func (e *breakerExchanger) Exchange(ctx context.Context, a obo.Assertion, r obo.ExchangeRequest) (obo.AccessToken, error) {
	return e.breaker(a.TenantID).Execute(callerAware(ctx, func() (obo.AccessToken, error) { return e.next.Exchange(ctx, a, r) }))
}

// epochGuard reads the current epochs through the PostgreSQL breaker.
type epochGuard struct {
	reader  epoch.Reader
	breaker *gobreaker.CircuitBreaker[[]epoch.SecurityEpoch]
}

func newEpochGuard(reader epoch.Reader) *epochGuard {
	if reader == nil {
		return nil
	}
	return &epochGuard{reader: reader, breaker: gobreaker.NewCircuitBreaker[[]epoch.SecurityEpoch](
		breakerSettings("credential-broker-postgres", nil))}
}

func (g *epochGuard) read(ctx context.Context, keys []epoch.SecurityEpoch) ([]epoch.SecurityEpoch, error) {
	return g.breaker.Execute(callerAware(ctx, func() ([]epoch.SecurityEpoch, error) { return g.reader.Read(ctx, keys) }))
}

// redemptionKeys are the epochs one permitted redemption depends on: the
// stored context's binding, mandate and delegation at their admitted epochs,
// the decision's grant and credential reference at the epochs it evaluated,
// and the binding's external identity (which must be active; its current
// epoch is recorded in a cache entry).
func redemptionKeys(sc *securitycontextView, facts requestFacts, decision authzcompose.OperationDecision) []epoch.SecurityEpoch {
	var keys []epoch.SecurityEpoch
	if sc.agentBindingID != "" {
		keys = append(keys, epoch.SecurityEpoch{Kind: epoch.KindBinding, ID: sc.agentBindingID, Value: sc.agentBindingEpoch})
	}
	if sc.mandateID != "" {
		keys = append(keys, epoch.SecurityEpoch{Kind: epoch.KindMandate, ID: sc.mandateID, Value: sc.mandateEpoch})
	}
	if sc.delegationID != "" {
		keys = append(keys, epoch.SecurityEpoch{Kind: epoch.KindDelegation, ID: sc.delegationID, Value: sc.delegationEpoch})
	}
	if id := decision.EvaluatedRefs["grant"]; id != "" {
		keys = append(keys, epoch.SecurityEpoch{Kind: epoch.KindGrant, ID: id, Value: decision.EvaluatedEpochs["grant"]})
	}
	if facts.reference.ID != "" {
		value, ok := decision.EvaluatedEpochs["credential_reference"]
		if !ok {
			value = facts.reference.Generation
		}
		keys = append(keys, epoch.SecurityEpoch{Kind: epoch.KindCredentialRef, ID: facts.reference.ID, Value: value})
	}
	if facts.externalIdentityID != "" {
		keys = append(keys, epoch.SecurityEpoch{Kind: epoch.KindExternalIdentity, ID: facts.externalIdentityID, Value: epoch.Unpinned})
	}
	return keys
}

// checkEpochs re-reads every epoch of a permitted redemption in one
// statement, BEFORE any cache lookup or secret read. A stale key denies
// authority_revoked; an unavailable database (or an open breaker) denies
// BACKEND_UNAVAILABLE. Without an epoch reader it answers nothing and
// changes nothing.
func (b *Broker) checkEpochs(ctx context.Context, sc *securitycontextView, facts requestFacts,
	decision authzcompose.OperationDecision) ([]epoch.SecurityEpoch, *Denial) {
	if b.epochs == nil {
		return nil, nil
	}
	keys := redemptionKeys(sc, facts, decision)
	for _, key := range keys {
		if !key.WellFormed() {
			return nil, &Denial{Code: CodeDenied, reason: "authority_revoked"}
		}
	}
	current, err := b.epochs.read(ctx, keys)
	switch {
	case breakerOpen(err):
		return nil, &Denial{Code: CodeBackendUnavailable, reason: "epoch_circuit_open"}
	case err != nil:
		return nil, &Denial{Code: CodeBackendUnavailable, reason: "epoch_unavailable"}
	}
	if stale := epoch.Stale(keys, current); len(stale) > 0 {
		b.cfg.Invalidations.ObserveDenied(ctx, stale)
		for _, key := range stale {
			b.cache.InvalidateEpoch(epoch.SecurityEpoch{Kind: key.Kind, ID: key.ID, Value: epoch.Unpinned})
		}
		return nil, &Denial{Code: CodeDenied, reason: "authority_revoked"}
	}
	return current, nil
}

// partitionKey is the cache key of a redemption's material.
func (b *Broker) partitionKey(req RedeemRequest, facts requestFacts, scopes []string) cache.PartitionKey {
	r := facts.request
	return cache.PartitionKey{
		TenantProjectID:       req.Run.TenantProjectID,
		AgentBindingID:        r.Agent.BindingID,
		SubjectPrincipalRef:   r.Subject.PrincipalRef,
		AuthorityID:           r.Authority.ID,
		CredentialReferenceID: facts.reference.ID,
		TargetBindingID:       facts.reference.TargetBindingID,
		ScopeDigest:           cache.ScopeDigest(scopes, b.cfg.Catalog.Class(r.Action.OperationID)),
		PolicyRevisionID:      r.Context.PolicyRevisionID,
		Generation:            facts.reference.Generation,
	}
}

// revisionMemo remembers whether a policy revision allows caching static
// secrets. Revisions are immutable, so one read per revision suffices; a
// failed read answers "not allowed" and is retried next time.
type revisionMemo struct {
	store profile.RevisionStore
	mu    sync.Mutex
	known map[string]bool
}

func (m *revisionMemo) staticAllowed(ctx context.Context, id string) bool {
	if m == nil || m.store == nil || id == "" {
		return false
	}
	m.mu.Lock()
	allowed, ok := m.known[id]
	m.mu.Unlock()
	if ok {
		return allowed
	}
	revision, err := m.store.Revision(ctx, id)
	if err != nil {
		return false
	}
	allowed = revision.Document.CacheSettings().CacheStaticSecrets
	m.mu.Lock()
	if len(m.known) < 256 {
		m.known[id] = allowed
	}
	m.mu.Unlock()
	return allowed
}

// fromCache turns a cached entry into an envelope that never outlives the
// entry's not_after.
func fromCache(entry cache.Entry, kind EnvelopeKind, now time.Time) *Envelope {
	notAfter := now.Add(MaxEnvelopeTTL)
	if entry.NotAfter.Before(notAfter) {
		notAfter = entry.NotAfter
	}
	return &Envelope{Kind: kind, Material: entry.Material, NotAfter: notAfter}
}
