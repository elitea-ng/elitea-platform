package repos

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/fakeprovider"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/reconcile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Shared 0191 (issue #1154): the reconcile loop over the real stores. A
// drift transition, its finding and its drift event commit together; the
// broker reads the persisted provider health; the pass lease lets one of
// two instances run. CI-only: skipped without the integration database.

type reconcileStack struct {
	*identityStack
	store  *AgentIdentityReconcile
	broker *agentidentity.Broker
}

func newReconcileStack(t *testing.T) *reconcileStack {
	t.Helper()
	s := &reconcileStack{identityStack: newIdentityStack(t)}
	audit := agentIdentityAudit(t)
	s.store = NewAgentIdentityReconcile(s.pool, audit)
	registry, err := agentidentity.NewRegistry(s.provider)
	require.NoError(t, err)
	// The broker the composition builds: every OpenSession reads the
	// provider's persisted reconcile health.
	s.broker, err = agentidentity.NewBroker(agentidentity.BrokerConfig{
		Registry: registry, Policy: agentidentity.DenyAssertedUser{ModePolicy: profile.PolicyDocument{AllowedIdentityModes: domain.IdentityModes()}},
		Bindings: s.bindings, Identities: s.identities, Sessions: NewAgentSessions(s.pool, audit), ProviderHealth: s.store})
	require.NoError(t, err)
	return s
}

func (s *reconcileStack) reconciler(t *testing.T, holder string) *reconcile.Reconciler {
	t.Helper()
	registry, err := agentidentity.NewRegistry(s.provider)
	require.NoError(t, err)
	r, err := reconcile.New(reconcile.Config{Registry: registry, Store: s.store, Interval: profile.DefaultReconcileInterval,
		OutageThreshold: profile.DefaultReconcileOutageThreshold, Holder: holder})
	require.NoError(t, err)
	return r
}

// expireLease ends the provider's pass lease, as the interval passing would.
func (s *reconcileStack) expireLease(t *testing.T) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(),
		`UPDATE elitea_identity.agent_identity_provider_health SET lease_until = '-infinity' WHERE provider = 'fake'`)
	require.NoError(t, err)
}

func (s *reconcileStack) driftActions(t *testing.T) map[string]int64 {
	t.Helper()
	rows, err := s.pool.Query(context.Background(), `
SELECT event->'data'->>'action', coalesce((event->'data'->>'revocation_epoch')::bigint, 0)
FROM elitea_runtime.security_audit_outbox WHERE event_type = $1 ORDER BY seq`, string(securityaudit.TypeExternalIdentityDrift))
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var action string
		var epoch int64
		require.NoError(t, rows.Scan(&action, &epoch))
		out[action] = epoch
	}
	require.NoError(t, rows.Err())
	return out
}

func reasonOf(err error) agentidentity.DenyReason {
	reason, _ := agentidentity.ReasonOf(err)
	return reason
}

// Acceptance: disabled in the mock IdP → suspended within one pass, epoch
// +1, with the finding and the drift event in the transition's transaction;
// new sessions and Credential calls for the agent are denied; another agent
// continues.
func TestReconcileDisabledIdentityIsSuspendedAndDenied(t *testing.T) {
	s := newReconcileStack(t)
	ctx := context.Background()
	bindingA, externalA := s.activeAgent(t, 61, 601)
	bindingB, _ := s.activeAgent(t, 62, 602)
	refA, err := s.broker.OpenSession(ctx, sessionFor(bindingA, "exec-a"))
	require.NoError(t, err)

	require.True(t, s.provider.Mutate(externalA.Ref.ID, func(o *fakeprovider.Object) { o.Enabled = false }))
	ran, err := s.reconciler(t, "replica-a").Tick(ctx, "fake")
	require.NoError(t, err)
	require.True(t, ran)

	got, err := s.identities.GetExternalIdentity(ctx, externalA.ID)
	require.NoError(t, err)
	require.Equal(t, domain.ExternalIdentitySuspended, got.State)
	require.Equal(t, externalA.RevocationEpoch+1, got.RevocationEpoch)
	require.Equal(t, reconcile.Actor, got.LastTransitionBy)
	require.Equal(t, map[string]int64{"external_identity.drift.disabled": got.RevocationEpoch}, s.driftActions(t))

	open, err := s.store.OpenFindings(ctx, "", 50, 0)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, reconcile.KindDisabled, open[0].Kind)
	require.Equal(t, externalA.ID, open[0].ExternalAgentIdentityID)
	require.Equal(t, got.RevocationEpoch, open[0].RevocationEpoch)

	_, err = s.broker.Credential(ctx, claimOf(refA, "exec-a"), agentidentity.CredentialRequest{Audience: "api://crm"})
	require.Equal(t, agentidentity.ReasonExternalIdentityNotActive, reasonOf(err))
	_, err = s.broker.OpenSession(ctx, sessionFor(bindingA, "exec-a2"))
	require.Equal(t, agentidentity.ReasonExternalIdentityNotActive, reasonOf(err))
	_, err = s.broker.OpenSession(ctx, sessionFor(bindingB, "exec-b"))
	require.NoError(t, err)

	// A repeated pass changes nothing more.
	s.expireLease(t)
	_, err = s.reconciler(t, "replica-a").Tick(ctx, "fake")
	require.NoError(t, err)
	again, err := s.identities.GetExternalIdentity(ctx, externalA.ID)
	require.NoError(t, err)
	require.Equal(t, got.RevocationEpoch, again.RevocationEpoch)
}

// Acceptance: recreate with the same name → replaced; resume is refused.
func TestReconcileRecreatedIdentityIsReplacedAndCannotResume(t *testing.T) {
	s := newReconcileStack(t)
	ctx := context.Background()
	_, externalA := s.activeAgent(t, 63, 603)
	_, ok := s.provider.Recreate(externalA.Ref.ID)
	require.True(t, ok)
	_, err := s.reconciler(t, "replica-a").Pass(ctx, "fake")
	require.NoError(t, err)

	got, err := s.identities.GetExternalIdentity(ctx, externalA.ID)
	require.NoError(t, err)
	require.Equal(t, domain.ExternalIdentityReplaced, got.State)
	require.Equal(t, externalA.RevocationEpoch+1, got.RevocationEpoch)
	_, err = s.external.Resume(ctx, externalA.ID, "200")
	require.ErrorIs(t, err, domain.ErrExternalTransition)

	actions := s.driftActions(t)
	require.Equal(t, got.RevocationEpoch, actions["external_identity.drift.replaced"])
	require.Contains(t, actions, "external_identity.orphan", "the recreated object has no record")
}

// Acceptance: an orphan is an audit event only, once while it is open.
func TestReconcileOrphanIsAuditOnly(t *testing.T) {
	s := newReconcileStack(t)
	ctx := context.Background()
	_, externalA := s.activeAgent(t, 64, 604)
	s.provider.AddObject(fakeprovider.Object{Ref: domain.ExternalRef{Provider: "fake", ProviderTenant: "t1",
		Kind: "fake.agent", ID: "obj-orphan-1154"}, Enabled: true})
	r := s.reconciler(t, "replica-a")
	for range 2 {
		report, err := r.Pass(ctx, "fake")
		require.NoError(t, err)
		require.Empty(t, report.Transitions)
	}
	got, err := s.identities.GetExternalIdentity(ctx, externalA.ID)
	require.NoError(t, err)
	require.Equal(t, externalA.RevocationEpoch, got.RevocationEpoch)
	var orphanEvents int
	require.NoError(t, s.pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 AND event->'data'->>'action' = 'external_identity.orphan'
  AND event->'data'->>'resource_id' = 'obj-orphan-1154'`, string(securityaudit.TypeExternalIdentityDrift)).Scan(&orphanEvents))
	require.Equal(t, 1, orphanEvents)
	open, err := s.store.OpenFindings(ctx, "fake", 50, 0)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, reconcile.KindOrphan, open[0].Kind)
	require.Empty(t, open[0].ExternalAgentIdentityID)
}

// Acceptance: an outage changes no state; after the threshold new sessions
// for the provider are denied on every replica's broker; a good pass admits
// them again.
func TestReconcileOutageDeniesNewSessionsAfterTheThreshold(t *testing.T) {
	s := newReconcileStack(t)
	ctx := context.Background()
	bindingA, externalA := s.activeAgent(t, 65, 605)
	require.True(t, s.provider.Mutate(externalA.Ref.ID, func(o *fakeprovider.Object) { o.Enabled = false }))
	s.provider.ListErr = errors.New("idp down")
	r := s.reconciler(t, "replica-a")
	for pass := 1; pass <= profile.DefaultReconcileOutageThreshold; pass++ {
		denied, err := s.store.ProviderAdmissionDenied(ctx, "fake")
		require.NoError(t, err)
		require.False(t, denied, "admitted before the threshold (pass %d)", pass)
		ran, err := r.Tick(ctx, "fake")
		require.True(t, ran)
		require.ErrorIs(t, err, reconcile.ErrProviderUnavailable)
		s.expireLease(t)
	}
	got, err := s.identities.GetExternalIdentity(ctx, externalA.ID)
	require.NoError(t, err)
	require.Equal(t, domain.ExternalIdentityActive, got.State, "an outage changes no state")
	require.Equal(t, externalA.RevocationEpoch, got.RevocationEpoch)
	require.Empty(t, s.driftActions(t))

	_, err = s.broker.OpenSession(ctx, sessionFor(bindingA, "exec-denied"))
	require.Equal(t, agentidentity.ReasonProviderUnreconciled, reasonOf(err))
	health, err := s.store.Health(ctx)
	require.NoError(t, err)
	require.Len(t, health, 1)
	require.True(t, health[0].AdmissionDenied)
	require.Equal(t, profile.DefaultReconcileOutageThreshold, health[0].ConsecutiveFailures)
	require.True(t, health[0].LastSuccessAt.IsZero())

	s.provider.ListErr = nil
	require.True(t, s.provider.Mutate(externalA.Ref.ID, func(o *fakeprovider.Object) { o.Enabled = true }))
	ran, err := r.Tick(ctx, "fake")
	require.True(t, ran)
	require.NoError(t, err)
	_, err = s.broker.OpenSession(ctx, sessionFor(bindingA, "exec-after"))
	require.NoError(t, err)
}

// Acceptance: with two instances, only one runs a pass.
func TestReconcileOnlyOneOfTwoInstancesRunsAPass(t *testing.T) {
	s := newReconcileStack(t)
	s.activeAgent(t, 66, 606)
	instances := []*reconcile.Reconciler{s.reconciler(t, "replica-a"), s.reconciler(t, "replica-b")}
	s.provider.Reset()
	var ran atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := instances[i%2].Tick(context.Background(), "fake")
			if err != nil {
				t.Error(err)
			}
			if ok {
				ran.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	require.EqualValues(t, 1, ran.Load())
	var holder string
	var until time.Time
	require.NoError(t, s.pool.QueryRow(context.Background(), `
SELECT lease_holder, lease_until FROM elitea_identity.agent_identity_provider_health WHERE provider = 'fake'`).Scan(&holder, &until))
	require.Contains(t, []string{"replica-a", "replica-b"}, holder)
	require.True(t, until.After(time.Now().Add(profile.DefaultReconcileInterval/2)))
}
