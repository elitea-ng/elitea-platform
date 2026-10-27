package reconcile_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/fakeprovider"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/reconcile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity/governance"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	recordA = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0e01"
	recordB = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0e02"
	// tenantCanary and ownerCanary must never reach an audit event.
	tenantCanary = "tenant-canary-1154"
	ownerCanary  = "owner-canary-1154"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type notifications struct {
	mu  sync.Mutex
	got []reconcile.EpochChange
}

func (n *notifications) EpochChanged(_ context.Context, c reconcile.EpochChange) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.got = append(n.got, c)
}

type fixture struct {
	clock    *clock
	store    *memStore
	provider *fakeprovider.Provider
	registry *agentidentity.Registry
	notified *notifications
	reader   *sdkmetric.ManualReader
	r        *reconcile.Reconciler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{clock: &clock{t: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}, provider: fakeprovider.New("fake"),
		notified: &notifications{}, reader: sdkmetric.NewManualReader()}
	f.store = newMemStore(f.clock.Now)
	var err error
	if f.registry, err = agentidentity.NewRegistry(f.provider); err != nil {
		t.Fatal(err)
	}
	f.r = f.reconciler(t, "replica-a")
	return f
}

func (f *fixture) reconciler(t *testing.T, holder string) *reconcile.Reconciler {
	t.Helper()
	r, err := reconcile.New(reconcile.Config{
		Registry: f.registry, Store: f.store, Interval: profile.DefaultReconcileInterval,
		OutageThreshold: profile.DefaultReconcileOutageThreshold, Holder: holder, Notifier: f.notified,
		Meter: sdkmetric.NewMeterProvider(sdkmetric.WithReader(f.reader)).Meter("test"), Now: f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// activeRecord provisions an object in the mock IdP and stores an active
// record for it.
func (f *fixture) activeRecord(t *testing.T, id string, application int64) identity.ExternalAgentIdentity {
	t.Helper()
	result, err := f.provider.Provision(context.Background(), agentidentity.ProvisionSpec{
		ExternalAgentIdentityID: id, ProviderTenant: tenantCanary, OwnerRef: ownerCanary})
	if err != nil {
		t.Fatal(err)
	}
	now := f.clock.Now()
	e := identity.ExternalAgentIdentity{
		ID: id, TenantProjectID: 7, ApplicationID: application, Mode: identity.IdentityModeDedicatedAgent,
		Ref: result.Ref, OwnerRef: ownerCanary, State: identity.ExternalIdentityActive, RevocationEpoch: 2,
		CreatedBy: "100", LastTransitionBy: "200", CreatedAt: now, UpdatedAt: now,
	}
	f.store.put(e)
	return e
}

func (f *fixture) pass(t *testing.T) reconcile.Report {
	t.Helper()
	report, err := f.r.Pass(context.Background(), "fake")
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func driftEvents(store *memStore) []securityaudit.SecurityEvent {
	var out []securityaudit.SecurityEvent
	for _, e := range store.Events() {
		if e.Type == securityaudit.TypeExternalIdentityDrift {
			out = append(out, e)
		}
	}
	return out
}

func (f *fixture) openFindings(t *testing.T) []reconcile.Finding {
	t.Helper()
	open, err := f.store.OpenFindings(context.Background(), "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	return open
}

// Acceptance: disabled in the mock IdP → suspended within one pass, epoch
// +1, drift.disabled; the notifier hears the new epoch; a second pass
// changes nothing.
func TestDisabledOutsideEliteaSuspendsWithinOnePass(t *testing.T) {
	f := newFixture(t)
	a := f.activeRecord(t, recordA, 11)
	f.activeRecord(t, recordB, 12)
	if report := f.pass(t); len(report.Transitions) != 0 || report.Listed != 2 {
		t.Fatalf("a clean pass reported %+v", report)
	}
	f.provider.Mutate(a.Ref.ID, func(o *fakeprovider.Object) { o.Enabled = false })

	report := f.pass(t)
	if report.Transitions[reconcile.KindDisabled] != 1 {
		t.Fatalf("report = %+v", report)
	}
	got := f.store.Identity(recordA)
	if got.State != identity.ExternalIdentitySuspended || got.RevocationEpoch != a.RevocationEpoch+1 || got.LastTransitionBy != reconcile.Actor {
		t.Fatalf("record = %s epoch %d by %s", got.State, got.RevocationEpoch, got.LastTransitionBy)
	}
	if other := f.store.Identity(recordB); other.State != identity.ExternalIdentityActive {
		t.Fatal("another agent's identity was touched")
	}
	events := driftEvents(f.store)
	if len(events) != 1 || events[0].Action != "external_identity.drift.disabled" || events[0].RevocationEpoch != got.RevocationEpoch ||
		events[0].ResourceID != recordA {
		t.Fatalf("drift events = %+v", events)
	}
	if len(f.notified.got) != 1 || f.notified.got[0] != (reconcile.EpochChange{Kind: reconcile.EpochKindExternalIdentity, ID: recordA, Epoch: got.RevocationEpoch}) {
		t.Fatalf("notified = %+v", f.notified.got)
	}
	open := f.openFindings(t)
	if len(open) != 1 || open[0].Kind != reconcile.KindDisabled || open[0].ExternalAgentIdentityID != recordA || open[0].RevocationEpoch != got.RevocationEpoch {
		t.Fatalf("findings = %+v", open)
	}

	before := len(f.store.Events())
	if report := f.pass(t); len(report.Transitions) != 0 {
		t.Fatalf("a repeated pass transitioned again: %+v", report)
	}
	if len(f.store.Events()) != before || len(f.openFindings(t)) != 1 {
		t.Fatal("a repeated pass wrote events or findings")
	}
}

// Not found, and credential or owner changes, suspend an active record.
func TestMissingAndChangedSuspend(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*fakeprovider.Object)
		kind   reconcile.FindingKind
	}{
		"deleted":            {func(o *fakeprovider.Object) { o.Deleted = true }, reconcile.KindMissing},
		"credential added":   {func(o *fakeprovider.Object) { o.CredentialChanged = true }, reconcile.KindChanged},
		"owner changed":      {func(o *fakeprovider.Object) { o.OwnerRef = "someone-else" }, reconcile.KindChanged},
		"owner not reported": {func(o *fakeprovider.Object) { o.OwnerRef = "" }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			a := f.activeRecord(t, recordA, 11)
			f.provider.Mutate(a.Ref.ID, tc.change)
			report := f.pass(t)
			got := f.store.Identity(recordA)
			if tc.kind == "" {
				if got.State != identity.ExternalIdentityActive || len(report.Transitions) != 0 {
					t.Fatalf("an unreported owner must not count as a change: %+v", report)
				}
				return
			}
			if got.State != identity.ExternalIdentitySuspended || got.RevocationEpoch != a.RevocationEpoch+1 || report.Transitions[tc.kind] != 1 {
				t.Fatalf("record = %s epoch %d, report %+v", got.State, got.RevocationEpoch, report)
			}
			if events := driftEvents(f.store); len(events) != 1 || events[0].Action != tc.kind.Action() {
				t.Fatalf("drift events = %+v", events)
			}
		})
	}
}

// Acceptance: recreate with the same name → replaced (terminal); resume is
// refused; the recreated object is an orphan until a new record names it.
func TestRecreatedObjectIsReplacedAndCannotBeResumed(t *testing.T) {
	f := newFixture(t)
	a := f.activeRecord(t, recordA, 11)
	newRef, ok := f.provider.Recreate(a.Ref.ID)
	if !ok {
		t.Fatal("recreate")
	}
	report := f.pass(t)
	got := f.store.Identity(recordA)
	if got.State != identity.ExternalIdentityReplaced || got.RevocationEpoch != a.RevocationEpoch+1 || report.Transitions[reconcile.KindReplaced] != 1 {
		t.Fatalf("record = %s epoch %d, report %+v", got.State, got.RevocationEpoch, report)
	}
	if report.NewOrphans != 1 {
		t.Fatalf("the recreated object is an orphan: %+v", report)
	}

	service, err := agentidentity.NewExternalIdentityService(agentidentity.ExternalIdentityConfig{
		Revisions: governance.FixedRevision(profile.PolicyRevision{ID: "r1",
			Document: profile.PolicyDocument{AllowedIdentityModes: identity.IdentityModes()}}),
		Store: f.store, Registry: f.registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.provider.Reset()
	if _, err := service.Resume(context.Background(), recordA, "200"); !errors.Is(err, identity.ErrExternalTransition) {
		t.Fatalf("resume of a replaced identity: err = %v, want ErrExternalTransition", err)
	}
	if calls := f.provider.Calls(); len(calls) != 0 {
		t.Fatalf("a refused resume reached the provider: %v", calls)
	}
	// Replaced stays replaced through every later pass.
	f.pass(t)
	if f.store.Identity(recordA).State != identity.ExternalIdentityReplaced {
		t.Fatal("replaced is terminal")
	}
	if open := f.openFindings(t); len(open) != 2 {
		t.Fatalf("want the replaced and the orphan finding open, got %+v", open)
	}

	// Recovery is an administrator action: a new record for the agent that
	// names the recreated object closes both findings.
	f.clock.Advance(time.Minute)
	now := f.clock.Now()
	f.store.put(identity.ExternalAgentIdentity{ID: recordB, TenantProjectID: 7, ApplicationID: 11,
		Mode: identity.IdentityModeDedicatedAgent, Ref: newRef, OwnerRef: ownerCanary,
		State: identity.ExternalIdentityPendingActivation, RevocationEpoch: 1, CreatedBy: "100", LastTransitionBy: "100",
		CreatedAt: now, UpdatedAt: now})
	if report := f.pass(t); report.Closed != 2 || len(report.Transitions) != 0 {
		t.Fatalf("report = %+v", report)
	}
	if open := f.openFindings(t); len(open) != 0 {
		t.Fatalf("findings still open: %+v", open)
	}
}

// Acceptance: an Elitea-tagged object with no record → one orphan audit
// event, no state change; it closes when the object goes.
func TestOrphanIsAuditOnly(t *testing.T) {
	f := newFixture(t)
	a := f.activeRecord(t, recordA, 11)
	f.provider.AddObject(fakeprovider.Object{Ref: identity.ExternalRef{Provider: "fake", ProviderTenant: tenantCanary,
		Kind: "fake.agent", ID: "obj-orphan"}, Enabled: true, OwnerRef: ownerCanary})
	report := f.pass(t)
	if report.NewOrphans != 1 || len(report.Transitions) != 0 {
		t.Fatalf("report = %+v", report)
	}
	if got := f.store.Identity(recordA); got != a {
		t.Fatalf("an orphan changed a record: %+v", got)
	}
	events := driftEvents(f.store)
	if len(events) != 1 || events[0].Action != "external_identity.orphan" || events[0].ResourceID != "obj-orphan" ||
		events[0].TenantProjectID != 0 {
		t.Fatalf("drift events = %+v", events)
	}
	if report := f.pass(t); report.NewOrphans != 0 || len(driftEvents(f.store)) != 1 {
		t.Fatal("an open orphan is audited once")
	}
	f.provider.Mutate("obj-orphan", func(o *fakeprovider.Object) { o.Deleted = true })
	if report := f.pass(t); report.Closed != 1 {
		t.Fatalf("a vanished orphan closes its finding: %+v", report)
	}
}

func errorCount(t *testing.T, reader *sdkmetric.ManualReader, provider string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "agentidentity.reconcile.errors" {
				continue
			}
			sum, _ := m.Data.(metricdata.Sum[int64])
			for _, point := range sum.DataPoints {
				if v, ok := point.Attributes.Value("provider"); ok && v.AsString() == provider {
					return point.Value
				}
			}
		}
	}
	return 0
}

// Acceptance: a provider outage changes no state; after the threshold of
// failed passes new sessions for the provider are denied, and one good pass
// admits them again.
func TestOutageChangesNothingAndDeniesAfterTheThreshold(t *testing.T) {
	f := newFixture(t)
	a := f.activeRecord(t, recordA, 11)
	f.provider.Mutate(a.Ref.ID, func(o *fakeprovider.Object) { o.Enabled = false })
	f.provider.ListErr = errors.New("idp down")
	ctx := context.Background()

	for pass := 1; pass <= profile.DefaultReconcileOutageThreshold; pass++ {
		denied, _ := f.store.ProviderAdmissionDenied(ctx, "fake")
		if denied {
			t.Fatalf("denied before the threshold, after %d failures", pass-1)
		}
		ran, err := f.r.Tick(ctx, "fake")
		if !ran || !errors.Is(err, reconcile.ErrProviderUnavailable) {
			t.Fatalf("pass %d: ran %v err %v", pass, ran, err)
		}
		if strings.Contains(err.Error(), "idp down") {
			t.Fatal("the provider's own message leaked")
		}
		// The lease holds for one interval; the next replica tick waits.
		if ran, _ := f.r.Tick(ctx, "fake"); ran {
			t.Fatal("a second pass within the interval")
		}
		f.clock.Advance(profile.DefaultReconcileInterval)
	}
	if got := f.store.Identity(recordA); got != a {
		t.Fatalf("an outage changed state: %+v", got)
	}
	if len(f.store.Events()) != 0 {
		t.Fatal("an outage wrote audit events")
	}
	if denied, _ := f.store.ProviderAdmissionDenied(ctx, "fake"); !denied {
		t.Fatal("new sessions must be denied after the threshold")
	}
	if n := errorCount(t, f.reader, "fake"); n != int64(profile.DefaultReconcileOutageThreshold) {
		t.Fatalf("agentidentity_reconcile_errors_total{provider=fake} = %d", n)
	}

	// A Resolve failure is an outage too.
	f.provider.ListErr = nil
	f.provider.Mutate(a.Ref.ID, func(o *fakeprovider.Object) { o.Deleted = true })
	f.provider.ResolveErr = errors.New("timeout")
	if _, err := f.r.Pass(ctx, "fake"); !errors.Is(err, reconcile.ErrProviderUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if f.store.Identity(recordA) != a {
		t.Fatal("a resolve failure changed state")
	}

	f.provider.ResolveErr = nil
	f.provider.Mutate(a.Ref.ID, func(o *fakeprovider.Object) { o.Deleted, o.Enabled = false, true })
	if ran, err := f.r.Tick(ctx, "fake"); !ran || err != nil {
		t.Fatalf("recovery pass: ran %v err %v", ran, err)
	}
	if denied, _ := f.store.ProviderAdmissionDenied(ctx, "fake"); denied {
		t.Fatal("a good pass admits the provider again")
	}
}

// Acceptance: with two instances only one runs a pass.
func TestOnlyOneInstanceRunsAPass(t *testing.T) {
	f := newFixture(t)
	f.activeRecord(t, recordA, 11)
	instances := []*reconcile.Reconciler{f.r, f.reconciler(t, "replica-b")}
	var ran atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if ok, err := instances[i%2].Tick(context.Background(), "fake"); err != nil {
				t.Error(err)
			} else if ok {
				ran.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ran.Load() != 1 {
		t.Fatalf("%d passes ran, want 1", ran.Load())
	}
	if lists := strings.Count(strings.Join(f.provider.Calls(), ","), "List"); lists != 1 {
		t.Fatalf("the provider was listed %d times", lists)
	}
}

// slowProvider counts concurrent Resolve calls.
type slowProvider struct {
	*fakeprovider.Provider
	inFlight, peak atomic.Int32
}

func (p *slowProvider) Resolve(ctx context.Context, ref identity.ExternalRef) (agentidentity.Resolved, error) {
	n := p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	for {
		peak := p.peak.Load()
		if n <= peak || p.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	return p.Provider.Resolve(ctx, ref)
}

// List shows nothing, so every record is resolved.
func (p *slowProvider) List(context.Context, agentidentity.ListSpec) (agentidentity.ListPage, error) {
	return agentidentity.ListPage{}, nil
}

func TestResolveConcurrencyIsBounded(t *testing.T) {
	f := newFixture(t)
	slow := &slowProvider{Provider: f.provider}
	registry, err := agentidentity.NewRegistry(slow)
	if err != nil {
		t.Fatal(err)
	}
	r, err := reconcile.New(reconcile.Config{Registry: registry, Store: f.store, Interval: time.Minute,
		OutageThreshold: 1, Concurrency: 3, Now: f.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		id := strings.Replace(recordA, "e01", "f"+string(rune('a'+i))+"0", 1)
		f.activeRecord(t, id, int64(20+i))
	}
	report, err := r.Pass(context.Background(), "fake")
	if err != nil {
		t.Fatal(err)
	}
	if report.Resolved != 12 || slow.peak.Load() > 3 || slow.peak.Load() < 2 {
		t.Fatalf("resolved %d with peak %d, want 12 with at most 3", report.Resolved, slow.peak.Load())
	}
}

// A pending or suspended record that went missing or disabled is not
// transitioned (only replace leaves a non-active state); a record an
// operator changed since the pass read it is left to the next pass.
func TestOnlyActiveRecordsAreSuspended(t *testing.T) {
	f := newFixture(t)
	a := f.activeRecord(t, recordA, 11)
	a.State = identity.ExternalIdentitySuspended
	f.store.put(a)
	f.provider.Mutate(a.Ref.ID, func(o *fakeprovider.Object) { o.Enabled = false })
	if report := f.pass(t); len(report.Transitions) != 0 || f.store.Identity(recordA) != a {
		t.Fatalf("a suspended record was transitioned: %+v", report)
	}
}

// Acceptance: audit events contain ids and epochs only.
func TestDriftEventsHoldIdsAndEpochsOnly(t *testing.T) {
	f := newFixture(t)
	a := f.activeRecord(t, recordA, 11)
	b := f.activeRecord(t, recordB, 12)
	f.provider.Mutate(a.Ref.ID, func(o *fakeprovider.Object) { o.Enabled = false })
	f.provider.Recreate(b.Ref.ID)
	f.pass(t)
	events := driftEvents(f.store)
	if len(events) != 3 {
		t.Fatalf("want disabled, replaced and orphan events, got %d", len(events))
	}
	for _, e := range events {
		raw, _ := json.Marshal(e)
		for _, canary := range []string{tenantCanary, ownerCanary} {
			if strings.Contains(string(raw), canary) {
				t.Fatalf("%s carries %q: %s", e.Action, canary, raw)
			}
		}
		if e.ActorChain != nil || e.ExecutionID != "" || e.GrantRef != "" || e.MandateRef != "" || e.DelegationRef != "" ||
			e.CallingApplicationID != "" || e.AuthenticationRef != "" || e.CorrelationID != "" {
			t.Fatalf("%s carries more than ids and epochs: %+v", e.Action, e)
		}
		for _, reason := range e.Reasons {
			if !strings.HasPrefix(reason, "drift.") && !strings.HasPrefix(reason, "state.") {
				t.Fatalf("%s reason %q is not a fixed code", e.Action, reason)
			}
		}
	}
}

func TestNewRefusesMissingParameters(t *testing.T) {
	registry, _ := agentidentity.NewRegistry()
	store := newMemStore(time.Now)
	for name, cfg := range map[string]reconcile.Config{
		"no registry":  {Store: store, Interval: time.Minute, OutageThreshold: 1},
		"no store":     {Registry: registry, Interval: time.Minute, OutageThreshold: 1},
		"no interval":  {Registry: registry, Store: store, OutageThreshold: 1},
		"no threshold": {Registry: registry, Store: store, Interval: time.Minute},
	} {
		if _, err := reconcile.New(cfg); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	r, err := reconcile.New(reconcile.Config{Registry: registry, Store: store, Interval: time.Minute, OutageThreshold: 1})
	if err != nil {
		t.Fatal(err)
	}
	// No provider configured: Run returns at once.
	done := make(chan struct{})
	go func() { r.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run with no provider must return")
	}
}
