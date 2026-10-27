package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Defaults of Config.
const (
	DefaultConcurrency = 4
	DefaultPageSize    = 100
	DefaultMaxPages    = 1000
	// Actor is the record's last_transition_by of a drift transition.
	Actor = "system:agent-identity-reconcile"

	minTick = 5 * time.Second
	maxTick = time.Minute
)

// metricErrors is written by the Prometheus exporter as
// agentidentity_reconcile_errors_total{provider}.
const metricErrors = "agentidentity.reconcile.errors"

// ErrProviderUnavailable wraps a provider failure during a pass. The pass
// changed nothing.
var ErrProviderUnavailable = errors.New("agent identity reconcile: the provider failed; no state changed")

// Config is the reconciler's dependencies and parameters.
type Config struct {
	Registry *agentidentity.Registry
	Store    Store
	// Interval is the pass interval (policy revision; defaults are the
	// revision's). Required.
	Interval time.Duration
	// OutageThreshold is the consecutive failed passes after which new
	// sessions for the provider are denied. Required.
	OutageThreshold int
	// Holder names this replica in the pass lease. A random one when empty.
	Holder string
	// Concurrency bounds the Resolve calls of one provider's pass.
	Concurrency int
	PageSize    int
	MaxPages    int
	// Notifier is optional (the #1121 cache invalidation seam).
	Notifier Notifier
	Logger   *slog.Logger
	Meter    metric.Meter
	Now      func() time.Time
}

// Reconciler runs the reconcile loop.
type Reconciler struct {
	registry    *agentidentity.Registry
	store       Store
	interval    time.Duration
	threshold   int
	holder      string
	concurrency int
	pageSize    int
	maxPages    int
	notifier    Notifier
	logger      *slog.Logger
	now         func() time.Time
	errors      metric.Int64Counter
}

// New builds the reconciler.
func New(cfg Config) (*Reconciler, error) {
	if cfg.Registry == nil || cfg.Store == nil {
		return nil, errors.New("agent identity reconcile: a registry and a store are required")
	}
	if cfg.Interval <= 0 || cfg.OutageThreshold <= 0 {
		return nil, errors.New("agent identity reconcile: a positive interval and outage threshold are required")
	}
	r := &Reconciler{
		registry: cfg.Registry, store: cfg.Store, interval: cfg.Interval, threshold: cfg.OutageThreshold,
		holder: cfg.Holder, concurrency: cfg.Concurrency, pageSize: cfg.PageSize, maxPages: cfg.MaxPages,
		notifier: cfg.Notifier, logger: cfg.Logger, now: cfg.Now,
	}
	if r.holder == "" {
		r.holder = "elitea-main/" + uuid.NewString()
	}
	if len(r.holder) > 128 {
		return nil, errors.New("agent identity reconcile: the holder must be at most 128 bytes")
	}
	if r.concurrency <= 0 {
		r.concurrency = DefaultConcurrency
	}
	if r.pageSize <= 0 {
		r.pageSize = DefaultPageSize
	}
	if r.maxPages <= 0 {
		r.maxPages = DefaultMaxPages
	}
	if r.logger == nil {
		r.logger = slog.Default()
	}
	if r.now == nil {
		r.now = func() time.Time { return time.Now().UTC() }
	}
	meter := cfg.Meter
	if meter == nil {
		meter = otel.Meter("elitea-main")
	}
	var err error
	if r.errors, err = meter.Int64Counter(metricErrors,
		metric.WithDescription("Failed agent identity reconcile passes, by provider. A failed pass changes no state.")); err != nil {
		return nil, err
	}
	return r, nil
}

// Providers answers the providers the loop runs for: the configured ones.
func (r *Reconciler) Providers() []string { return r.registry.Names() }

// Run runs one loop per configured provider until ctx ends. With no
// provider configured it returns at once.
func (r *Reconciler) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, name := range r.Providers() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.runProvider(ctx, name)
		}()
	}
	wg.Wait()
}

// tick is how often a replica tries the lease: a tenth of the interval,
// within [5s, 1m]. The lease, not the tick, sets the pass cadence.
func (r *Reconciler) tick() time.Duration {
	return min(max(r.interval/10, minTick), maxTick)
}

// jittered spreads d by ±25% so replicas do not tick together.
func jittered(d time.Duration) time.Duration {
	spread := int64(d) / 2
	return d - time.Duration(spread/2) + time.Duration(rand.Int64N(spread+1))
}

func (r *Reconciler) runProvider(ctx context.Context, provider string) {
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(r.tick()) + 1)))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := r.Tick(ctx, provider); err != nil && ctx.Err() == nil {
			r.logger.Warn("agentidentity reconcile: pass failed", "provider", provider, "err", err)
		}
		timer.Reset(jittered(r.tick()))
	}
}

// Tick runs one pass for provider when this replica wins the pass lease,
// records its result and answers whether it ran.
func (r *Reconciler) Tick(ctx context.Context, provider string) (bool, error) {
	if _, ok := r.registry.Provider(provider); !ok {
		return false, agentidentity.ErrProviderNotConfigured
	}
	claimed, err := r.store.ClaimPass(ctx, provider, r.holder, r.interval)
	if err != nil {
		return false, fmt.Errorf("agent identity reconcile: claim the pass lease: %w", err)
	}
	if !claimed {
		return false, nil
	}
	// The pass ends before the lease does, so no second replica starts one
	// while it runs.
	passCtx, cancel := context.WithTimeout(ctx, r.interval*9/10)
	defer cancel()
	_, passErr := r.Pass(passCtx, provider)
	if passErr != nil {
		r.errors.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(attribute.String("provider", provider)))
	}
	before, _ := r.health(ctx, provider)
	health, err := r.store.RecordPass(context.WithoutCancel(ctx), provider, passErr == nil, r.threshold)
	if err != nil {
		return true, errors.Join(passErr, fmt.Errorf("agent identity reconcile: record the pass: %w", err))
	}
	switch {
	case health.AdmissionDenied && !before.AdmissionDenied:
		r.logger.Error("agentidentity reconcile: provider failed the outage threshold; new agent sessions for it are denied",
			"provider", provider, "consecutive_failures", health.ConsecutiveFailures, "threshold", r.threshold)
	case !health.AdmissionDenied && before.AdmissionDenied:
		r.logger.Info("agentidentity reconcile: provider reconciled again; new agent sessions for it are admitted", "provider", provider)
	}
	return true, passErr
}

func (r *Reconciler) health(ctx context.Context, provider string) (ProviderHealth, error) {
	all, err := r.store.Health(ctx)
	if err != nil {
		return ProviderHealth{}, err
	}
	for _, h := range all {
		if h.Provider == provider {
			return h, nil
		}
	}
	return ProviderHealth{Provider: provider}, nil
}

// Report is what one pass found and did.
type Report struct {
	Listed   int
	Resolved int
	// Transitions counts committed drift transitions by kind.
	Transitions map[FindingKind]int
	NewOrphans  int
	// Closed counts findings the pass resolved.
	Closed int
}

type objectKey struct{ tenant, id string }

// observation is the live state of one record: listed, resolved, replaced
// or missing.
type observation struct {
	resolved agentidentity.Resolved
	replaced bool
	missing  bool
}

// Pass runs one reconcile pass for provider without the lease: it observes
// everything first and changes state only when the whole observation
// succeeded, so a provider failure changes nothing. Exported so a test
// drives a pass without a ticker.
func (r *Reconciler) Pass(ctx context.Context, provider string) (Report, error) {
	report := Report{Transitions: map[FindingKind]int{}}
	adapter, ok := r.registry.Provider(provider)
	if !ok {
		return report, agentidentity.ErrProviderNotConfigured
	}
	records, err := r.store.Records(ctx, provider)
	if err != nil {
		return report, fmt.Errorf("agent identity reconcile: read records: %w", err)
	}
	byObject := make(map[objectKey]identity.ExternalAgentIdentity, len(records))
	for _, rec := range records {
		if rec.Ref.ID == "" {
			continue
		}
		// A live record owns its object over a retired or replaced one that
		// once named the same id.
		key := objectKey{rec.Ref.ProviderTenant, rec.Ref.ID}
		if prior, ok := byObject[key]; !ok || !reconcilable(prior) {
			byObject[key] = rec
		}
	}

	// Observe: list, then resolve every live record the list did not show.
	seen := map[string]observation{}
	var orphans []agentidentity.Resolved
	token := ""
	tokens := map[string]bool{}
	for page := 0; ; page++ {
		if page >= r.maxPages {
			return report, fmt.Errorf("%w: list exceeded %d pages", ErrProviderUnavailable, r.maxPages)
		}
		listed, err := adapter.List(ctx, agentidentity.ListSpec{PageToken: token, PageSize: r.pageSize})
		if err != nil {
			return report, fmt.Errorf("%w: list", ErrProviderUnavailable)
		}
		for _, object := range listed.Objects {
			report.Listed++
			if object.Ref.Provider != "" && object.Ref.Provider != provider {
				continue
			}
			rec, known := byObject[objectKey{object.Ref.ProviderTenant, object.Ref.ID}]
			if !known {
				orphans = append(orphans, object)
				continue
			}
			seen[rec.ID] = observation{resolved: object}
		}
		if listed.NextPageToken == "" {
			break
		}
		if tokens[listed.NextPageToken] {
			return report, fmt.Errorf("%w: list repeated a page token", ErrProviderUnavailable)
		}
		tokens[listed.NextPageToken] = true
		token = listed.NextPageToken
	}
	var unseen []identity.ExternalAgentIdentity
	for _, rec := range records {
		if _, ok := seen[rec.ID]; !ok && reconcilable(rec) {
			unseen = append(unseen, rec)
		}
	}
	resolved, err := r.resolveAll(ctx, adapter, unseen)
	if err != nil {
		return report, err
	}
	report.Resolved = len(unseen)
	for id, obs := range resolved {
		seen[id] = obs
	}

	// Act.
	for _, rec := range records {
		obs, ok := seen[rec.ID]
		if !ok || !reconcilable(rec) {
			continue
		}
		kind, action, drift := classify(rec, obs)
		if !drift {
			continue
		}
		written, err := r.store.ApplyDrift(ctx, rec.ID, r.driftChange(rec, kind, action))
		if errors.Is(err, identity.ErrExternalTransition) {
			// The record moved since it was read; the next pass sees it.
			continue
		}
		if err != nil {
			return report, fmt.Errorf("agent identity reconcile: apply %s drift: %w", kind, err)
		}
		report.Transitions[kind]++
		if r.notifier != nil {
			r.notifier.EpochChanged(ctx, EpochChange{Kind: EpochKindExternalIdentity, ID: written.ID, Epoch: written.RevocationEpoch})
		}
	}
	orphanKeys := map[objectKey]bool{}
	for _, object := range orphans {
		orphanKeys[objectKey{object.Ref.ProviderTenant, object.Ref.ID}] = true
		created, err := r.store.RecordOrphan(ctx, r.orphanFinding(provider, object), r.orphanEvent(provider, object))
		if err != nil {
			return report, fmt.Errorf("agent identity reconcile: record an orphan: %w", err)
		}
		if created {
			report.NewOrphans++
		}
	}
	closed, err := r.closeFindings(ctx, provider, orphanKeys)
	report.Closed = closed
	return report, err
}

// reconcilable is a record whose provider object should exist and be
// compared: provisioned and not retiring or terminal.
func reconcilable(rec identity.ExternalAgentIdentity) bool {
	if rec.Ref.ID == "" {
		return false
	}
	switch rec.State {
	case identity.ExternalIdentityPendingActivation, identity.ExternalIdentityActive, identity.ExternalIdentitySuspended:
		return true
	}
	return false
}

// classify applies the diff rules. Only replace leaves a non-active state;
// the other kinds stop an active record and nothing else.
func classify(rec identity.ExternalAgentIdentity, obs observation) (FindingKind, identity.ExternalAction, bool) {
	if obs.replaced || (!obs.missing && (obs.resolved.Ref.ID != rec.Ref.ID || obs.resolved.Ref.Generation != rec.Ref.Generation)) {
		return KindReplaced, identity.ExternalActionReplace, true
	}
	if rec.State != identity.ExternalIdentityActive {
		return "", "", false
	}
	switch {
	case obs.missing:
		return KindMissing, identity.ExternalActionSuspend, true
	case !obs.resolved.Enabled:
		return KindDisabled, identity.ExternalActionSuspend, true
	case obs.resolved.CredentialChanged,
		obs.resolved.OwnerRef != "" && rec.OwnerRef != "" && obs.resolved.OwnerRef != rec.OwnerRef:
		return KindChanged, identity.ExternalActionSuspend, true
	}
	return "", "", false
}

// resolveAll resolves records with at most r.concurrency calls in flight.
// Any failure other than replaced or not found fails the whole pass.
func (r *Reconciler) resolveAll(ctx context.Context, adapter agentidentity.Provider, records []identity.ExternalAgentIdentity) (map[string]observation, error) {
	out := make(map[string]observation, len(records))
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	slots := make(chan struct{}, r.concurrency)
	for _, rec := range records {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil, fmt.Errorf("%w: %w", ErrProviderUnavailable, ctx.Err())
		}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			resolved, err := adapter.Resolve(ctx, rec.Ref)
			var obs observation
			switch {
			case errors.Is(err, agentidentity.ErrIdentityReplaced):
				obs.replaced = true
			case errors.Is(err, agentidentity.ErrIdentityNotFound):
				obs.missing = true
			case err != nil:
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("%w: resolve", ErrProviderUnavailable)
				}
				mu.Unlock()
				return
			default:
				obs.resolved = resolved
			}
			mu.Lock()
			out[rec.ID] = obs
			mu.Unlock()
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func (r *Reconciler) driftChange(rec identity.ExternalAgentIdentity, kind FindingKind, action identity.ExternalAction) DriftChange {
	now := r.now()
	observedEpoch := rec.RevocationEpoch
	return DriftChange{
		Finding: Finding{
			Kind: kind, Provider: rec.Ref.Provider, ProviderTenant: rec.Ref.ProviderTenant, ExternalID: rec.Ref.ID,
			ExternalAgentIdentityID: rec.ID, TenantProjectID: rec.TenantProjectID, ApplicationID: rec.ApplicationID,
			DetectedAt: now, LastSeenAt: now,
		},
		Apply: func(current identity.ExternalAgentIdentity, _ int) (identity.ExternalAgentIdentity, error) {
			// An operator action since the read wins; the next pass looks
			// again.
			if current.RevocationEpoch != observedEpoch {
				return identity.ExternalAgentIdentity{}, identity.ErrExternalTransition
			}
			return current.Transition(action, Actor, identity.ExternalRef{}, "", now)
		},
		Event: func(written identity.ExternalAgentIdentity) securityaudit.SecurityEvent {
			return securityaudit.SecurityEvent{
				Type: securityaudit.TypeExternalIdentityDrift, Time: now, TenantProjectID: written.TenantProjectID,
				Action: kind.Action(), Reasons: []string{"drift." + string(kind), "state." + string(written.State)},
				PrincipalRef: Actor, AuthorityID: "external_agent_identity:" + written.ID,
				ResourceType: "external_agent_identity", ResourceID: written.ID, RevocationEpoch: written.RevocationEpoch,
			}
		},
	}
}

func (r *Reconciler) orphanFinding(provider string, object agentidentity.Resolved) Finding {
	now := r.now()
	return Finding{Kind: KindOrphan, Provider: provider, ProviderTenant: object.Ref.ProviderTenant,
		ExternalID: object.Ref.ID, DetectedAt: now, LastSeenAt: now}
}

// orphanEvent is the platform-partition event of an orphan: the provider
// and the object's immutable id, nothing the provider said about it.
func (r *Reconciler) orphanEvent(provider string, object agentidentity.Resolved) securityaudit.SecurityEvent {
	return securityaudit.SecurityEvent{
		Type: securityaudit.TypeExternalIdentityDrift, Time: r.now(), Action: KindOrphan.Action(),
		Reasons: []string{"drift.orphan"}, PrincipalRef: Actor, AuthorityID: "agent_identity_provider:" + provider,
		ResourceType: "external_agent_object", ResourceID: object.Ref.ID,
	}
}

// closeFindings resolves the provider's open findings the pass no longer
// sees: an orphan that is gone or now has a record; a suspension finding
// whose record is no longer suspended (resumed, retired or replaced); a
// replacement once a live successor record exists for the same agent and
// provider tenant.
func (r *Reconciler) closeFindings(ctx context.Context, provider string, orphanKeys map[objectKey]bool) (int, error) {
	records, err := r.store.Records(ctx, provider)
	if err != nil {
		return 0, fmt.Errorf("agent identity reconcile: re-read records: %w", err)
	}
	byID := make(map[string]identity.ExternalAgentIdentity, len(records))
	for _, rec := range records {
		byID[rec.ID] = rec
	}
	var closing []string
	for offset := 0; ; {
		open, err := r.store.OpenFindings(ctx, provider, 200, offset)
		if err != nil {
			return 0, fmt.Errorf("agent identity reconcile: read findings: %w", err)
		}
		for _, f := range open {
			if !stillOpen(f, byID, records, orphanKeys) {
				closing = append(closing, f.ID)
			}
		}
		if len(open) < 200 {
			break
		}
		offset += len(open)
	}
	if len(closing) == 0 {
		return 0, nil
	}
	if err := r.store.ResolveFindings(ctx, closing); err != nil {
		return 0, fmt.Errorf("agent identity reconcile: resolve findings: %w", err)
	}
	return len(closing), nil
}

func stillOpen(f Finding, byID map[string]identity.ExternalAgentIdentity, records []identity.ExternalAgentIdentity, orphanKeys map[objectKey]bool) bool {
	if f.Kind == KindOrphan {
		return orphanKeys[objectKey{f.ProviderTenant, f.ExternalID}]
	}
	rec, ok := byID[f.ExternalAgentIdentityID]
	if !ok {
		return false
	}
	if f.Kind != KindReplaced {
		return rec.State == identity.ExternalIdentitySuspended
	}
	if rec.State != identity.ExternalIdentityReplaced {
		return false
	}
	for _, other := range records {
		if other.ID != rec.ID && other.TenantProjectID == rec.TenantProjectID && other.ApplicationID == rec.ApplicationID &&
			other.Ref.ProviderTenant == rec.Ref.ProviderTenant && !other.State.Terminal() && other.CreatedAt.After(rec.CreatedAt) {
			return false
		}
	}
	return true
}
