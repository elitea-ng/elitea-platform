package repos

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Shared 0181 (issue #1112): the approver CHECK, the one-live-binding index,
// the FOR UPDATE transition under concurrent approvals, the mandate round
// trip, the binding → mandate cascade, the locked mandate approval racing a
// binding revoke, the delegation scope, and the security audit event written
// and rolled back with each change. CI-only: skipped without the
// integration database.

const agentIdentityTestRevision = "rev-1112"

type agentIdentityAllowAll struct{}

func (agentIdentityAllowAll) AllowsIdentityMode(agentidentity.IdentityMode) bool { return true }

type agentIdentityTTL time.Duration

func (d agentIdentityTTL) MaxMandateTTL() time.Duration { return time.Duration(d) }

func seedAgentIdentityPolicyRevision(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	canonical := []byte(`{"schema":"elitea.credential-policy.v1"}`)
	digest := sha256.Sum256(canonical)
	_, err := pool.Exec(context.Background(), `
INSERT INTO elitea_identity.credential_policy_revisions (id, digest, canonical, document, created_by)
VALUES ($1, $2, $3, $4::jsonb, 'integration-test') ON CONFLICT (id) DO NOTHING`,
		agentIdentityTestRevision, digest[:], canonical, string(canonical))
	require.NoError(t, err)
}

func proposedTestBinding(t *testing.T, id string, version int64, digestByte byte) agentidentity.AgentIdentityBinding {
	t.Helper()
	return proposedTestBindingIn(t, 7, id, version, digestByte)
}

func proposedTestBindingIn(t *testing.T, tenant int64, id string, version int64, digestByte byte) agentidentity.AgentIdentityBinding {
	t.Helper()
	b, err := agentidentity.NewBinding(id, agentidentity.Proposal{
		TenantProjectID: tenant, ApplicationID: 3, ApplicationVersionID: version,
		DefinitionDigest: [32]byte{digestByte}, Mode: agentidentity.IdentityModeDedicatedAgent,
		ExternalAgentIdentityID: "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f22",
	}, agentIdentityAllowAll{}, agentIdentityTestRevision, "100", time.Now().UTC().Truncate(time.Microsecond))
	require.NoError(t, err)
	return b
}

func agentIdentityAudit(t *testing.T) securityaudit.Appender {
	t.Helper()
	appender, err := securityaudit.NewPostgresAppender("elitea-main/integration-test")
	require.NoError(t, err)
	return appender
}

// allowMandate accepts every binding and delegation, so a test reaches the
// table constraints.
func allowMandate(agentidentity.AgentIdentityBinding, *mandate.Delegation) error { return nil }

func governedMandateCheck(m mandate.Mandate) func(agentidentity.AgentIdentityBinding, *mandate.Delegation) error {
	return func(binding agentidentity.AgentIdentityBinding, delegation *mandate.Delegation) error {
		if binding.TenantProjectID != m.TenantProjectID || binding.State == agentidentity.BindingRevoked {
			return mandate.ErrBindingNotUsable
		}
		return mandate.CheckDelegation(m, delegation, time.Now().UTC())
	}
}

// approveMandateBy approves and, like governance.Service, requires the
// binding the store locked to be active.
func approveMandateBy(approver string) func(mandate.Mandate, agentidentity.AgentIdentityBinding) (mandate.Mandate, error) {
	return func(current mandate.Mandate, binding agentidentity.AgentIdentityBinding) (mandate.Mandate, error) {
		next, err := current.Transition(mandate.ActionApprove, approver, agentIdentityTestRevision, time.Now().UTC())
		if err != nil {
			return mandate.Mandate{}, err
		}
		if binding.State != agentidentity.BindingActive || binding.TenantProjectID != current.TenantProjectID {
			return mandate.Mandate{}, mandate.ErrBindingNotUsable
		}
		return next, nil
	}
}

func transitionBindingBy(action agentidentity.Action, actor string) func(agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
	return func(current agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
		return current.Transition(action, actor, agentIdentityTestRevision, time.Now().UTC())
	}
}

func newTestMandate(t *testing.T, id, bindingID string, delegationID *string) mandate.Mandate {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	req := mandate.Request{
		TenantProjectID: 7, Kind: mandate.KindAutonomous, AgentBindingID: bindingID,
		OperationScopes: []string{"github.issues.read"}, ExpiresAt: now.Add(time.Hour),
	}
	if delegationID != nil {
		subject := int64(5)
		req.Kind, req.SubjectUserID, req.DelegationID = mandate.KindDelegatedBackground, &subject, delegationID
	}
	m, err := mandate.New(id, req, agentIdentityTTL(24*time.Hour), agentIdentityTestRevision, "100", now)
	require.NoError(t, err)
	return m
}

// activeTestBinding stores a binding and approves it.
func activeTestBinding(t *testing.T, store *AgentIdentityBindings, id string, version int64, digestByte byte) agentidentity.AgentIdentityBinding {
	t.Helper()
	ctx := context.Background()
	_, err := store.CreateBinding(ctx, proposedTestBinding(t, id, version, digestByte))
	require.NoError(t, err)
	active, err := store.TransitionBinding(ctx, id, approveBy("200"))
	require.NoError(t, err)
	return active
}

type auditRow struct {
	Action, Principal string
	Epoch             int64
	Reasons           []string
}

// auditEvents reads the outbox events of one resource, oldest first.
func auditEvents(t *testing.T, pool *pgxpool.Pool, eventType securityaudit.EventType, resourceID string) []auditRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT event->'data'->>'action', event->'data'->>'principal_ref',
       (event->'data'->>'revocation_epoch')::bigint,
       ARRAY(SELECT jsonb_array_elements_text(event->'data'->'reasons'))
FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 AND event->'data'->>'resource_id' = $2
ORDER BY seq`, string(eventType), resourceID)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (auditRow, error) {
		var r auditRow
		err := row.Scan(&r.Action, &r.Principal, &r.Epoch, &r.Reasons)
		return r, err
	})
	require.NoError(t, err)
	return out
}

func approveBy(approver string) func(agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
	return func(current agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
		return current.Transition(agentidentity.ActionApprove, approver, agentIdentityTestRevision, time.Now().UTC())
	}
}

func TestAgentIdentityBindingApproverCheckIsEnforcedByTheDatabase(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	store := NewAgentIdentityBindings(pool, agentIdentityAudit(t))
	ctx := context.Background()

	created, err := store.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000001", 1, 1))
	require.NoError(t, err)

	// Bypass the domain: the CHECK alone must refuse the proposer as approver.
	_, err = pool.Exec(ctx, `
UPDATE elitea_identity.agent_identity_bindings SET approved_by = proposed_by, state = 'active' WHERE id = $1::uuid`, created.ID)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "want a constraint violation, got %v", err)
	require.Equal(t, "23514", pgErr.Code)
	require.Equal(t, agentIdentityBindingApproverCheck, pgErr.ConstraintName)
	require.ErrorIs(t, mapAgentIdentityWriteError(err), agentidentity.ErrSelfApproval)

	// And a transition whose apply skipped the domain check maps to the typed error.
	_, err = store.TransitionBinding(ctx, created.ID, func(current agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
		next := current
		next.State, next.ApprovedBy, next.RevocationEpoch = agentidentity.BindingActive, current.ProposedBy, current.RevocationEpoch+1
		return next, nil
	})
	require.ErrorIs(t, err, agentidentity.ErrSelfApproval)

	// Active without an approver is refused as well.
	_, err = pool.Exec(ctx, `UPDATE elitea_identity.agent_identity_bindings SET state = 'active' WHERE id = $1::uuid`, created.ID)
	require.Error(t, err)
}

func TestAgentIdentityBindingUniqueIndexCoversOnlyNonRevokedBindings(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	store := NewAgentIdentityBindings(pool, agentIdentityAudit(t))
	ctx := context.Background()

	first, err := store.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000011", 5, 9))
	require.NoError(t, err)
	_, err = store.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000012", 5, 9))
	require.ErrorIs(t, err, agentidentity.ErrDuplicateBinding)

	// Another digest of the same version, and the same digest in another version, are fine.
	_, err = store.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000013", 5, 10))
	require.NoError(t, err)
	_, err = store.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000014", 6, 9))
	require.NoError(t, err)

	revoked, err := store.TransitionBinding(ctx, first.ID, func(current agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
		return current.Transition(agentidentity.ActionRevoke, "100", "", time.Now().UTC())
	})
	require.NoError(t, err)
	require.Equal(t, agentidentity.BindingRevoked, revoked.State)
	require.Equal(t, int64(1), revoked.RevocationEpoch)

	again, err := store.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000015", 5, 9))
	require.NoError(t, err, "a revoked binding must not block a new proposal")
	require.Equal(t, agentidentity.BindingProposed, again.State)
}

func TestAgentIdentityBindingConcurrentApprovalsGiveOneSuccess(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	store := NewAgentIdentityBindings(pool, agentIdentityAudit(t))
	ctx := context.Background()

	created, err := store.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000021", 1, 2))
	require.NoError(t, err)

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, approver := range []string{"201", "202"} {
		wg.Add(1)
		go func(i int, approver string) {
			defer wg.Done()
			<-start
			_, errs[i] = store.TransitionBinding(ctx, created.ID, approveBy(approver))
		}(i, approver)
	}
	close(start)
	wg.Wait()

	successes, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, agentidentity.ErrInvalidTransition):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, conflicts)

	stored, err := store.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, agentidentity.BindingActive, stored.State)
	require.Equal(t, int64(1), stored.RevocationEpoch, "exactly one transition was written")

	active, err := store.ActiveForDefinition(ctx, 7, 1, [32]byte{2})
	require.NoError(t, err)
	require.Equal(t, created.ID, active.ID)
	require.Equal(t, [32]byte{2}, active.DefinitionDigest)
}

func TestMandatesRoundTripAndApproverCheck(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	bindings := NewAgentIdentityBindings(pool, agentIdentityAudit(t))
	mandates := NewMandates(pool, agentIdentityAudit(t))
	ctx := context.Background()

	binding, err := bindings.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000031", 1, 3))
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	m, err := mandate.New("0192f5e2-7c3a-7b1e-8a9d-000000000032", mandate.Request{
		TenantProjectID: 7, Kind: mandate.KindAutonomous, AgentBindingID: binding.ID,
		OperationScopes: []string{"github.issues.read"}, ExpiresAt: now.Add(time.Hour),
	}, agentIdentityTTL(24*time.Hour), agentIdentityTestRevision, "100", now)
	require.NoError(t, err)

	created, err := mandates.CreateMandate(ctx, m, allowMandate)
	require.NoError(t, err)
	require.Equal(t, []string{"github.issues.read"}, created.OperationScopes)
	require.Empty(t, created.ResourceScopes)
	require.Nil(t, created.SubjectUserID)
	require.True(t, created.ExpiresAt.Equal(m.ExpiresAt))

	_, err = pool.Exec(ctx, `UPDATE elitea_identity.mandates SET approved_by = issued_by WHERE id = $1::uuid`, created.ID)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, mandateApproverCheck, pgErr.ConstraintName)

	// The store hands the binding to apply; the binding rule lives in
	// governance.Service, so this apply approves without it.
	approved, err := mandates.TransitionMandate(ctx, created.ID, func(current mandate.Mandate, _ agentidentity.AgentIdentityBinding) (mandate.Mandate, error) {
		return current.Transition(mandate.ActionApprove, "200", agentIdentityTestRevision, time.Now().UTC())
	})
	require.NoError(t, err)
	require.Equal(t, mandate.StateActive, approved.State)
	require.Equal(t, int64(1), approved.RevocationEpoch)

	listed, err := mandates.ListMandates(ctx, mandate.ListFilter{TenantProjectID: 7, State: mandate.StateActive})
	require.NoError(t, err)
	require.Len(t, listed, 1)

	// An autonomous mandate naming a user is refused by the table as well.
	_, err = pool.Exec(ctx, `UPDATE elitea_identity.mandates SET subject_user_id = 5 WHERE id = $1::uuid`, created.ID)
	require.Error(t, err)
}

// failAfter passes the first n appends to next and fails every later one.
type failAfter struct {
	mu   sync.Mutex
	n    int
	next securityaudit.Appender
}

func (f *failAfter) Append(ctx context.Context, tx pgx.Tx, e securityaudit.SecurityEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n <= 0 {
		return securityaudit.ErrAppendFailed
	}
	f.n--
	return f.next.Append(ctx, tx, e)
}

func storedLastTransitionBy(t *testing.T, pool *pgxpool.Pool, table, id string) string {
	t.Helper()
	var by string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT last_transition_by FROM elitea_identity.`+table+` WHERE id = $1::uuid`, id).Scan(&by))
	return by
}

func TestAgentIdentityChangesAppendAuditEventsInTheirTransaction(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	audit := agentIdentityAudit(t)
	bindings, mandates := NewAgentIdentityBindings(pool, audit), NewMandates(pool, audit)
	ctx := context.Background()

	const bindingID = "0192f5e2-7c3a-7b1e-8a9d-000000000041"
	binding := activeTestBinding(t, bindings, bindingID, 1, 4)
	m, err := mandates.CreateMandate(ctx, newTestMandate(t, "0192f5e2-7c3a-7b1e-8a9d-000000000042", binding.ID, nil), governedMandateCheck(mandate.Mandate{TenantProjectID: 7}))
	require.NoError(t, err)
	_, err = mandates.TransitionMandate(ctx, m.ID, approveMandateBy("200"))
	require.NoError(t, err)
	suspended, err := bindings.TransitionBinding(ctx, bindingID, transitionBindingBy(agentidentity.ActionSuspend, "300"))
	require.NoError(t, err)
	require.Equal(t, "300", suspended.LastTransitionBy)
	require.Equal(t, "300", storedLastTransitionBy(t, pool, "agent_identity_bindings", bindingID))

	require.Equal(t, []auditRow{
		{Action: "binding.propose", Principal: "user:100", Epoch: 0, Reasons: []string{"state.proposed"}},
		{Action: "binding.approve", Principal: "user:200", Epoch: 1, Reasons: []string{"state.active", "from.proposed"}},
		{Action: "binding.suspend", Principal: "user:300", Epoch: 2, Reasons: []string{"state.suspended", "from.active"}},
	}, auditEvents(t, pool, securityaudit.TypeBindingChanged, bindingID))
	require.Equal(t, []auditRow{
		{Action: "mandate.propose", Principal: "user:100", Epoch: 0, Reasons: []string{"state.proposed", "kind.autonomous"}},
		{Action: "mandate.approve", Principal: "user:200", Epoch: 1, Reasons: []string{"state.active", "kind.autonomous"}},
		{Action: "mandate.suspend", Principal: "user:300", Epoch: 2,
			Reasons: []string{"state.suspended", "kind.autonomous", "cascade.binding_suspended"}},
	}, auditEvents(t, pool, securityaudit.TypeMandateChanged, m.ID))
}

func TestAgentIdentityChangeRollsBackWhenItsAuditAppendFails(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	audit := agentIdentityAudit(t)
	failing := securityaudit.FailingAppender{}
	ctx := context.Background()

	// A proposal whose event cannot be written leaves no row and no event.
	const lost = "0192f5e2-7c3a-7b1e-8a9d-000000000051"
	_, err := NewAgentIdentityBindings(pool, failing).CreateBinding(ctx, proposedTestBinding(t, lost, 1, 5))
	require.ErrorIs(t, err, securityaudit.ErrAppendFailed)
	_, err = NewAgentIdentityBindings(pool, audit).Get(ctx, lost)
	require.ErrorIs(t, err, agentidentity.ErrBindingNotFound)
	require.Empty(t, auditEvents(t, pool, securityaudit.TypeBindingChanged, lost))

	// A transition whose event cannot be written leaves the row as it was.
	bindings := NewAgentIdentityBindings(pool, audit)
	const bindingID = "0192f5e2-7c3a-7b1e-8a9d-000000000052"
	binding := activeTestBinding(t, bindings, bindingID, 2, 5)
	_, err = NewAgentIdentityBindings(pool, failing).TransitionBinding(ctx, bindingID, transitionBindingBy(agentidentity.ActionSuspend, "300"))
	require.ErrorIs(t, err, securityaudit.ErrAppendFailed)
	stored, err := bindings.Get(ctx, bindingID)
	require.NoError(t, err)
	require.Equal(t, agentidentity.BindingActive, stored.State)
	require.Equal(t, binding.RevocationEpoch, stored.RevocationEpoch)
	require.Equal(t, "200", stored.LastTransitionBy)

	// Mandate issue and transition likewise.
	mandates := NewMandates(pool, audit)
	_, err = NewMandates(pool, failing).CreateMandate(ctx, newTestMandate(t, "0192f5e2-7c3a-7b1e-8a9d-000000000053", bindingID, nil), allowMandate)
	require.ErrorIs(t, err, securityaudit.ErrAppendFailed)
	_, err = mandates.Get(ctx, "0192f5e2-7c3a-7b1e-8a9d-000000000053")
	require.ErrorIs(t, err, mandate.ErrMandateNotFound)
	m, err := mandates.CreateMandate(ctx, newTestMandate(t, "0192f5e2-7c3a-7b1e-8a9d-000000000054", bindingID, nil), allowMandate)
	require.NoError(t, err)
	_, err = NewMandates(pool, failing).TransitionMandate(ctx, m.ID, approveMandateBy("200"))
	require.ErrorIs(t, err, securityaudit.ErrAppendFailed)
	storedMandate, err := mandates.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, mandate.StateProposed, storedMandate.State)
	require.Len(t, auditEvents(t, pool, securityaudit.TypeMandateChanged, m.ID), 1, "only the issue event")

	// The binding event is written but a cascaded mandate event fails: the
	// binding revoke, the mandate revoke and the binding event all roll back.
	_, err = mandates.TransitionMandate(ctx, m.ID, approveMandateBy("200"))
	require.NoError(t, err)
	partial := NewAgentIdentityBindings(pool, &failAfter{n: 1, next: audit})
	_, err = partial.TransitionBinding(ctx, bindingID, transitionBindingBy(agentidentity.ActionRevoke, "300"))
	require.ErrorIs(t, err, securityaudit.ErrAppendFailed)
	stored, err = bindings.Get(ctx, bindingID)
	require.NoError(t, err)
	require.Equal(t, agentidentity.BindingActive, stored.State)
	storedMandate, err = mandates.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, mandate.StateActive, storedMandate.State)
	require.Len(t, auditEvents(t, pool, securityaudit.TypeBindingChanged, bindingID), 2, "propose and approve only")

	// A store composed without an appender refuses to write at all.
	_, err = NewAgentIdentityBindings(pool, nil).CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000055", 3, 5))
	require.Error(t, err)
}

func TestBindingRevokeAndSuspendCascadeToItsMandates(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	audit := agentIdentityAudit(t)
	bindings, mandates := NewAgentIdentityBindings(pool, audit), NewMandates(pool, audit)
	ctx := context.Background()

	issue := func(id, bindingID string) mandate.Mandate {
		m, err := mandates.CreateMandate(ctx, newTestMandate(t, id, bindingID, nil), allowMandate)
		require.NoError(t, err)
		return m
	}
	transition := func(id string, action mandate.Action) mandate.Mandate {
		next, err := mandates.TransitionMandate(ctx, id, func(current mandate.Mandate, _ agentidentity.AgentIdentityBinding) (mandate.Mandate, error) {
			return current.Transition(action, "200", agentIdentityTestRevision, time.Now().UTC())
		})
		require.NoError(t, err)
		return next
	}

	// Revoke: every non-terminal mandate is revoked, epoch +1, actor recorded.
	const revokedBinding = "0192f5e2-7c3a-7b1e-8a9d-000000000061"
	activeTestBinding(t, bindings, revokedBinding, 1, 6)
	proposed := issue("0192f5e2-7c3a-7b1e-8a9d-000000000062", revokedBinding)
	active := issue("0192f5e2-7c3a-7b1e-8a9d-000000000063", revokedBinding)
	active = transition(active.ID, mandate.ActionApprove)
	paused := issue("0192f5e2-7c3a-7b1e-8a9d-000000000064", revokedBinding)
	transition(paused.ID, mandate.ActionApprove)
	paused = transition(paused.ID, mandate.ActionSuspend)
	gone := issue("0192f5e2-7c3a-7b1e-8a9d-000000000065", revokedBinding)
	gone = transition(gone.ID, mandate.ActionRevoke)

	_, err := bindings.TransitionBinding(ctx, revokedBinding, transitionBindingBy(agentidentity.ActionRevoke, "300"))
	require.NoError(t, err)
	for _, before := range []mandate.Mandate{proposed, active, paused} {
		after, err := mandates.Get(ctx, before.ID)
		require.NoError(t, err)
		require.Equal(t, mandate.StateRevoked, after.State, "mandate %s", before.ID)
		require.Equal(t, before.RevocationEpoch+1, after.RevocationEpoch, "mandate %s", before.ID)
		require.Equal(t, "300", after.LastTransitionBy)
		events := auditEvents(t, pool, securityaudit.TypeMandateChanged, before.ID)
		last := events[len(events)-1]
		require.Equal(t, "mandate.revoke", last.Action)
		require.Equal(t, "user:300", last.Principal)
		require.Contains(t, last.Reasons, "cascade.binding_revoked")
	}
	untouched, err := mandates.Get(ctx, gone.ID)
	require.NoError(t, err)
	require.Equal(t, gone.RevocationEpoch, untouched.RevocationEpoch, "an already revoked mandate is not touched")
	_, err = mandates.CreateMandate(ctx, newTestMandate(t, "0192f5e2-7c3a-7b1e-8a9d-000000000066", revokedBinding, nil),
		governedMandateCheck(mandate.Mandate{TenantProjectID: 7}))
	require.ErrorIs(t, err, mandate.ErrBindingNotUsable)

	// Suspend: the active mandate is suspended; the proposed one stays
	// proposed and cannot be approved while the binding is suspended. A
	// reactivated binding does not reactivate the suspended mandate.
	const suspendedBinding = "0192f5e2-7c3a-7b1e-8a9d-000000000071"
	activeTestBinding(t, bindings, suspendedBinding, 2, 7)
	waiting := issue("0192f5e2-7c3a-7b1e-8a9d-000000000072", suspendedBinding)
	running := issue("0192f5e2-7c3a-7b1e-8a9d-000000000073", suspendedBinding)
	running = transition(running.ID, mandate.ActionApprove)
	_, err = bindings.TransitionBinding(ctx, suspendedBinding, transitionBindingBy(agentidentity.ActionSuspend, "300"))
	require.NoError(t, err)
	after, err := mandates.Get(ctx, running.ID)
	require.NoError(t, err)
	require.Equal(t, mandate.StateSuspended, after.State)
	require.Equal(t, running.RevocationEpoch+1, after.RevocationEpoch)
	stillWaiting, err := mandates.Get(ctx, waiting.ID)
	require.NoError(t, err)
	require.Equal(t, mandate.StateProposed, stillWaiting.State)
	require.Equal(t, waiting.RevocationEpoch, stillWaiting.RevocationEpoch)
	_, err = mandates.TransitionMandate(ctx, waiting.ID, approveMandateBy("200"))
	require.ErrorIs(t, err, mandate.ErrBindingNotUsable)
	_, err = mandates.TransitionMandate(ctx, running.ID, approveMandateBy("200"))
	require.ErrorIs(t, err, mandate.ErrBindingNotUsable, "a suspended mandate cannot be reactivated on a suspended binding")

	_, err = bindings.TransitionBinding(ctx, suspendedBinding, approveBy("201"))
	require.NoError(t, err)
	after, err = mandates.Get(ctx, running.ID)
	require.NoError(t, err)
	require.Equal(t, mandate.StateSuspended, after.State, "reactivating the binding needs a fresh mandate approval")
}

// The approval locks the binding in its own transaction: a revoke that holds
// the binding row makes the approval wait, and the approval then sees the
// revoked binding.
func TestMandateApprovalWaitsForAnInFlightBindingRevoke(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	audit := agentIdentityAudit(t)
	bindings, mandates := NewAgentIdentityBindings(pool, audit), NewMandates(pool, audit)
	ctx := context.Background()

	const bindingID = "0192f5e2-7c3a-7b1e-8a9d-000000000081"
	activeTestBinding(t, bindings, bindingID, 1, 8)
	m, err := mandates.CreateMandate(ctx, newTestMandate(t, "0192f5e2-7c3a-7b1e-8a9d-000000000082", bindingID, nil), allowMandate)
	require.NoError(t, err)

	revokeEntered := make(chan struct{})
	releaseRevoke := make(chan struct{})
	revokeDone := make(chan error, 1)
	go func() {
		_, err := bindings.TransitionBinding(ctx, bindingID, func(current agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
			close(revokeEntered) // the binding row is locked FOR UPDATE now
			<-releaseRevoke
			return current.Transition(agentidentity.ActionRevoke, "300", "", time.Now().UTC())
		})
		revokeDone <- err
	}()
	<-revokeEntered

	approveDone := make(chan error, 1)
	go func() {
		_, err := mandates.TransitionMandate(ctx, m.ID, approveMandateBy("200"))
		approveDone <- err
	}()
	select {
	case err := <-approveDone:
		t.Fatalf("the approval did not wait for the binding lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(releaseRevoke)
	require.NoError(t, <-revokeDone)
	require.ErrorIs(t, <-approveDone, mandate.ErrInvalidTransition, "the cascade revoked the mandate before the approval read it")

	stored, err := mandates.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, mandate.StateRevoked, stored.State)
}

// Racing a mandate approval against a binding revoke never leaves an active
// mandate on a revoked binding, whichever wins.
func TestMandateApprovalRacingBindingRevokeNeverLeavesAnActiveMandate(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	audit := agentIdentityAudit(t)
	bindings, mandates := NewAgentIdentityBindings(pool, audit), NewMandates(pool, audit)
	ctx := context.Background()

	for i := 0; i < 8; i++ {
		bindingID := fmt.Sprintf("0192f5e2-7c3a-7b1e-8a9d-0000000009%02d", i)
		mandateID := fmt.Sprintf("0192f5e2-7c3a-7b1e-8a9d-0000000019%02d", i)
		activeTestBinding(t, bindings, bindingID, int64(100+i), 9)
		_, err := mandates.CreateMandate(ctx, newTestMandate(t, mandateID, bindingID, nil), allowMandate)
		require.NoError(t, err)

		start := make(chan struct{})
		var approveErr, revokeErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, approveErr = mandates.TransitionMandate(ctx, mandateID, approveMandateBy("200"))
		}()
		go func() {
			defer wg.Done()
			<-start
			_, revokeErr = bindings.TransitionBinding(ctx, bindingID, transitionBindingBy(agentidentity.ActionRevoke, "300"))
		}()
		close(start)
		wg.Wait()

		require.NoError(t, revokeErr)
		if approveErr != nil && !errors.Is(approveErr, mandate.ErrBindingNotUsable) && !errors.Is(approveErr, mandate.ErrInvalidTransition) {
			t.Fatalf("round %d: unexpected approval error %v", i, approveErr)
		}
		stored, err := mandates.Get(ctx, mandateID)
		require.NoError(t, err)
		require.Equal(t, mandate.StateRevoked, stored.State, "round %d", i)
	}
}

func TestMandateDelegationMustShareTenantAndBinding(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	audit := agentIdentityAudit(t)
	bindings, mandates := NewAgentIdentityBindings(pool, audit), NewMandates(pool, audit)
	ctx := context.Background()

	const (
		bindingID      = "0192f5e2-7c3a-7b1e-8a9d-0000000000a1"
		siblingBinding = "0192f5e2-7c3a-7b1e-8a9d-0000000000a2"
		foreignBinding = "0192f5e2-7c3a-7b1e-8a9d-0000000000a3"
		sameScope      = "0192f5e2-7c3a-7b1e-8a9d-0000000000b1"
		otherBinding   = "0192f5e2-7c3a-7b1e-8a9d-0000000000b2"
		otherTenant    = "0192f5e2-7c3a-7b1e-8a9d-0000000000b3"
		missing        = "0192f5e2-7c3a-7b1e-8a9d-0000000000b4"
	)
	activeTestBinding(t, bindings, bindingID, 1, 10)
	activeTestBinding(t, bindings, siblingBinding, 2, 10)
	_, err := bindings.CreateBinding(ctx, proposedTestBindingIn(t, 8, foreignBinding, 1, 10))
	require.NoError(t, err)
	for _, d := range []struct {
		id, binding string
		tenant      int64
	}{{sameScope, bindingID, 7}, {otherBinding, siblingBinding, 7}, {otherTenant, foreignBinding, 8}} {
		_, err := pool.Exec(ctx, `
INSERT INTO elitea_identity.delegations
    (id, tenant_project_id, issuer, tenant_id, subject_object_id, agent_binding_id, consent_recorded_at, expires_at, state)
VALUES ($1::uuid, $2, 'https://login.example/tenant', 'tenant', 'subject', $3::uuid, now(), now() + interval '1 hour', 'active')`,
			d.id, d.tenant, d.binding)
		require.NoError(t, err)
	}

	for _, delegation := range []string{otherBinding, otherTenant, missing} {
		delegation := delegation
		m := newTestMandate(t, "0192f5e2-7c3a-7b1e-8a9d-0000000000c1", bindingID, &delegation)
		_, err := mandates.CreateMandate(ctx, m, governedMandateCheck(m))
		require.ErrorIs(t, err, mandate.ErrDelegationNotUsable, "delegation %s", delegation)
	}
	_, err = mandates.Get(ctx, "0192f5e2-7c3a-7b1e-8a9d-0000000000c1")
	require.ErrorIs(t, err, mandate.ErrMandateNotFound)

	// The table refuses the cross-scope delegation even past the code check.
	crossBinding := otherBinding
	m := newTestMandate(t, "0192f5e2-7c3a-7b1e-8a9d-0000000000c2", bindingID, &crossBinding)
	_, err = mandates.CreateMandate(ctx, m, allowMandate)
	require.ErrorIs(t, err, mandate.ErrInvalidMandate)

	same := sameScope
	m = newTestMandate(t, "0192f5e2-7c3a-7b1e-8a9d-0000000000c3", bindingID, &same)
	created, err := mandates.CreateMandate(ctx, m, governedMandateCheck(m))
	require.NoError(t, err)
	require.NotNil(t, created.DelegationID)
	require.Equal(t, sameScope, *created.DelegationID)
}
