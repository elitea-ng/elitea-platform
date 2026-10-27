package repos

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/fakeprovider"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity/governance"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Shared 0186 (issue #1151): versions share an external identity and a fork
// does not, the one-live-identity index, the binding's usable state, and the
// broker over the real stores, where suspending agent A's external identity
// denies A and leaves agent B running. CI-only: skipped without the
// integration database.

type identityStack struct {
	pool       *pgxpool.Pool
	provider   *fakeprovider.Provider
	bindings   *AgentIdentityBindings
	identities *ExternalAgentIdentities
	governance *governance.Service
	external   *agentidentity.ExternalIdentityService
	broker     *agentidentity.Broker
}

func newIdentityStack(t *testing.T) *identityStack {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	audit := agentIdentityAudit(t)
	s := &identityStack{pool: pool, provider: fakeprovider.New("fake"),
		bindings: NewAgentIdentityBindings(pool, audit), identities: NewExternalAgentIdentities(pool, audit)}
	revision := governance.FixedRevision(profile.PolicyRevision{ID: agentIdentityTestRevision,
		Document: profile.PolicyDocument{AllowedIdentityModes: domain.IdentityModes()}})
	registry, err := agentidentity.NewRegistry(s.provider)
	require.NoError(t, err)
	s.governance, err = governance.New(governance.Config{Revisions: revision, Bindings: s.bindings,
		Mandates: NewMandates(pool, audit), ExternalIdentities: s.identities})
	require.NoError(t, err)
	s.external, err = agentidentity.NewExternalIdentityService(agentidentity.ExternalIdentityConfig{
		Revisions: revision, Store: s.identities, Registry: registry})
	require.NoError(t, err)
	s.broker, err = agentidentity.NewBroker(agentidentity.BrokerConfig{
		Registry: registry, Policy: agentidentity.DenyAssertedUser{ModePolicy: profile.PolicyDocument{AllowedIdentityModes: domain.IdentityModes()}},
		Bindings: s.bindings, Identities: s.identities, Sessions: NewAgentSessions(pool, audit)})
	require.NoError(t, err)
	return s
}

func externalRequestFor(application int64, mode domain.IdentityMode) domain.ExternalIdentityRequest {
	return domain.ExternalIdentityRequest{TenantProjectID: 7, ApplicationID: application, Mode: mode,
		Provider: "fake", ProviderTenant: "t1", ProviderConfigKey: "blueprint-1"}
}

func (s *identityStack) proposal(application, version int64, external string) domain.Proposal {
	return domain.Proposal{TenantProjectID: 7, ApplicationID: application, ApplicationVersionID: version,
		DefinitionDigest: [32]byte{byte(version)}, Mode: domain.IdentityModeDedicatedAgent, ExternalAgentIdentityID: external}
}

// activeAgent provisions an external identity for application, binds one
// version to it, approves the binding and activates the identity: the order
// an operator follows.
func (s *identityStack) activeAgent(t *testing.T, application, version int64) (domain.AgentIdentityBinding, domain.ExternalAgentIdentity) {
	t.Helper()
	ctx := context.Background()
	e, err := s.external.Create(ctx, externalRequestFor(application, domain.IdentityModeDedicatedAgent), "100")
	require.NoError(t, err)
	e, err = s.external.Provision(ctx, e.ID, "100")
	require.NoError(t, err)
	require.Equal(t, domain.ExternalIdentityPendingActivation, e.State)
	_, err = s.external.Activate(ctx, e.ID, "200")
	require.ErrorIs(t, err, domain.ErrNoApprovedBinding)

	b, err := s.governance.ProposeBinding(ctx, s.proposal(application, version, e.ID), "100")
	require.NoError(t, err)
	b, err = s.governance.TransitionBinding(ctx, b.ID, domain.ActionApprove, "200")
	require.NoError(t, err, "an approval against a pending_activation identity")
	unusable, err := s.bindings.ActiveForDefinition(ctx, 7, version, b.DefinitionDigest)
	require.NoError(t, err)
	require.Equal(t, domain.BindingSuspended, unusable.State, "usable only once the external identity is active")

	e, err = s.external.Activate(ctx, e.ID, "200")
	require.NoError(t, err)
	require.Equal(t, domain.ExternalIdentityActive, e.State)
	usable, err := s.bindings.ActiveForDefinition(ctx, 7, version, b.DefinitionDigest)
	require.NoError(t, err)
	require.Equal(t, domain.BindingActive, usable.State)
	return b, e
}

func sessionFor(b domain.AgentIdentityBinding, execution string) agentidentity.SessionAdmission {
	return agentidentity.SessionAdmission{TenantProjectID: 7, ExecutionID: execution, Generation: 1,
		AgentBindingID: b.ID, AgentBindingEpoch: b.RevocationEpoch, Profile: agentidentity.ProfileAutonomousAttributed,
		Ceiling:   agentidentity.Narrowing{Audiences: []string{"api://crm"}, Scopes: []string{"read"}},
		ExpiresAt: time.Now().Add(30 * time.Minute), PolicyRevisionID: agentIdentityTestRevision}
}

func claimOf(ref agentidentity.SessionRef, execution string) agentidentity.SessionClaim {
	return agentidentity.SessionClaim{Ref: ref, ExecutionID: execution, Generation: 1, WorkerClaimRef: "spiffe://elitea.local/worker/w1"}
}

// Acceptance: two versions of one application bind to the same external
// identity; a fork (another application) cannot, and gets its own.
func TestVersionsShareAnExternalIdentityAndAForkGetsANewOne(t *testing.T) {
	s := newIdentityStack(t)
	ctx := context.Background()
	e, err := s.external.Create(ctx, externalRequestFor(21, domain.IdentityModeDedicatedAgent), "100")
	require.NoError(t, err)
	_, err = s.governance.ProposeBinding(ctx, s.proposal(21, 101, e.ID), "100")
	require.NoError(t, err)
	_, err = s.governance.ProposeBinding(ctx, s.proposal(21, 102, e.ID), "100")
	require.NoError(t, err, "a second version re-binds to the same external identity")

	_, err = s.governance.ProposeBinding(ctx, s.proposal(22, 201, e.ID), "100")
	require.ErrorIs(t, err, domain.ErrInvalidBinding, "a fork must not reuse the original's external identity")
	fork, err := s.external.Create(ctx, externalRequestFor(22, domain.IdentityModeDedicatedAgent), "100")
	require.NoError(t, err)
	require.NotEqual(t, e.ID, fork.ID)
	_, err = s.governance.ProposeBinding(ctx, s.proposal(22, 201, fork.ID), "100")
	require.NoError(t, err)
}

// Acceptance: a second non-retired external identity for the same tenant
// project, application, provider and provider tenant fails on the unique
// index, for dedicated_agent and elitea_native.
func TestOneLiveExternalIdentityPerAgentAndProviderTenant(t *testing.T) {
	s := newIdentityStack(t)
	ctx := context.Background()
	for i, mode := range []domain.IdentityMode{domain.IdentityModeDedicatedAgent, domain.IdentityModeEliteaNative} {
		application := int64(31 + i)
		first, err := s.external.Create(ctx, externalRequestFor(application, mode), "100")
		require.NoError(t, err)
		_, err = s.external.Create(ctx, externalRequestFor(application, mode), "100")
		require.ErrorIs(t, err, domain.ErrDuplicateExternalIdentity, string(mode))

		other := externalRequestFor(application, mode)
		other.ProviderTenant = "t2"
		_, err = s.external.Create(ctx, other, "100")
		require.NoError(t, err, "another provider tenant is another identity")

		retired, err := s.external.Retire(ctx, first.ID, "100")
		require.NoError(t, err)
		require.Equal(t, domain.ExternalIdentityRetired, retired.State)
		_, err = s.external.Create(ctx, externalRequestFor(application, mode), "100")
		require.NoError(t, err, "a retired identity does not count")
	}
	_, err := s.external.Create(ctx, externalRequestFor(41, domain.IdentityModeRuntimeDomainBinding), "100")
	require.NoError(t, err)
	_, err = s.external.Create(ctx, externalRequestFor(41, domain.IdentityModeRuntimeDomainBinding), "100")
	require.NoError(t, err, "the index covers the per-agent modes only")
}

// Acceptance (integration): suspending the external identity of agent A
// denies new sessions and Credential calls for A; agent B continues.
func TestSuspendingAgentAExternalIdentityLeavesAgentBRunning(t *testing.T) {
	s := newIdentityStack(t)
	ctx := context.Background()
	bindingA, externalA := s.activeAgent(t, 51, 501)
	bindingB, _ := s.activeAgent(t, 52, 502)

	refA, err := s.broker.OpenSession(ctx, sessionFor(bindingA, "exec-a"))
	require.NoError(t, err)
	refB, err := s.broker.OpenSession(ctx, sessionFor(bindingB, "exec-b"))
	require.NoError(t, err)
	_, err = s.broker.Credential(ctx, claimOf(refA, "exec-a"), agentidentity.CredentialRequest{Audience: "api://crm"})
	require.NoError(t, err)

	suspended, err := s.external.Suspend(ctx, externalA.ID, "200")
	require.NoError(t, err)
	require.Equal(t, domain.ExternalIdentitySuspended, suspended.State)

	_, err = s.broker.Credential(ctx, claimOf(refA, "exec-a"), agentidentity.CredentialRequest{Audience: "api://crm"})
	reason, _ := agentidentity.ReasonOf(err)
	require.Equal(t, agentidentity.ReasonExternalIdentityNotActive, reason)
	_, err = s.broker.OpenSession(ctx, sessionFor(bindingA, "exec-a2"))
	reason, _ = agentidentity.ReasonOf(err)
	require.Equal(t, agentidentity.ReasonExternalIdentityNotActive, reason)
	stillA, err := s.bindings.ActiveForDefinition(ctx, 7, 501, bindingA.DefinitionDigest)
	require.NoError(t, err)
	require.Equal(t, domain.BindingSuspended, stillA.State)

	cred, err := s.broker.Credential(ctx, claimOf(refB, "exec-b"), agentidentity.CredentialRequest{Audience: "api://crm"})
	require.NoError(t, err)
	require.Equal(t, fakeprovider.Token, cred.Token)
	_, err = s.broker.OpenSession(ctx, sessionFor(bindingB, "exec-b2"))
	require.NoError(t, err)

	// A resumed identity has a new epoch, so A's old session stays denied
	// and a new one works.
	_, err = s.external.Resume(ctx, externalA.ID, "200")
	require.NoError(t, err)
	_, err = s.broker.Credential(ctx, claimOf(refA, "exec-a"), agentidentity.CredentialRequest{Audience: "api://crm"})
	reason, _ = agentidentity.ReasonOf(err)
	require.Equal(t, agentidentity.ReasonExternalIdentityStale, reason)
	_, err = s.broker.OpenSession(ctx, sessionFor(bindingA, "exec-a3"))
	require.NoError(t, err)

	// Ids and epochs only: no session ref and no token reached the outbox,
	// and the session table holds a digest, not the ref.
	var leaked int
	require.NoError(t, s.pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.security_audit_outbox
WHERE event::text LIKE '%' || $1 || '%' OR event::text LIKE '%ags1_%'`, fakeprovider.Token).Scan(&leaked))
	require.Zero(t, leaked)
	var sessions, denied int
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT count(*) FROM elitea_identity.agent_sessions`).Scan(&sessions))
	require.Equal(t, 4, sessions)
	require.NoError(t, s.pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 AND event->'data'->>'effect' = 'deny'`, string(securityaudit.TypeAgentSession)).Scan(&denied))
	require.Equal(t, 3, denied)
}

// SessionByID (#1152) reads the execution and generation the Entra adapter
// derives its client-request-id from; an unknown id is ErrSessionNotFound.
func TestAgentSessionByIDReadsTheRun(t *testing.T) {
	s := newIdentityStack(t)
	ctx := context.Background()
	binding, _ := s.activeAgent(t, 61, 601)
	_, err := s.broker.OpenSession(ctx, sessionFor(binding, "exec-by-id"))
	require.NoError(t, err)
	var id string
	require.NoError(t, s.pool.QueryRow(ctx,
		`SELECT id::text FROM elitea_identity.agent_sessions WHERE execution_id = 'exec-by-id'`).Scan(&id))
	sessions := NewAgentSessions(s.pool, agentIdentityAudit(t))
	session, err := sessions.SessionByID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "exec-by-id", session.ExecutionID)
	require.Equal(t, int64(1), session.Generation)
	require.Equal(t, binding.ID, session.AgentBindingID)
	_, err = sessions.SessionByID(ctx, "00000000-0000-7000-8000-000000000000")
	require.ErrorIs(t, err, agentidentity.ErrSessionNotFound)
}
