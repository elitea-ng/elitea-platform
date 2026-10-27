package authzcompose

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

// In-memory readers: the "warm readers" of the issue. Each holds a value
// and an optional error, so a test can flip exactly one input.

var errBackend = errors.New("backend unavailable")

const (
	testTenant     = int64(7)
	testUser       = int64(5)
	testBindingID  = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f01"
	testMandateID  = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f02"
	testCredential = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f03"
	testTargetA    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f04"
	testTargetB    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f05"
	testGrantID    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f06"
	testDelegation = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f07"
	testExecution  = "exec-1"
	testRevision   = "2026-10-r1"
	testOperation  = credentialgrant.OperationID("jira.issue.read")
	testToolkitID  = int64(11)
)

var (
	testDefinitionDigest = [32]byte{1}
	testToolkitDigest    = [32]byte{2}
	testPolicyDigest     = [32]byte{3}
	testNow              = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
)

type fakeResolver struct {
	perms map[int64][]string
	err   error
}

func (f *fakeResolver) ResolvePermissions(_ context.Context, principal auth.User, mode, projectID string) (auth.PermissionResolution, error) {
	if f.err != nil {
		return auth.PermissionResolution{}, f.err
	}
	if mode != auth.PermissionModeDefault || projectID != strconv.FormatInt(testTenant, 10) {
		return auth.PermissionResolution{}, auth.ErrPermissionDenied
	}
	id, _ := strconv.ParseInt(principal.UserID, 10, 64)
	return auth.PermissionResolution{UserID: id, Permissions: f.perms[id]}, nil
}

type fakeEntityAccess struct {
	allowed bool
	err     error
}

func (f *fakeEntityAccess) AgentAccessible(context.Context, int64, int64, int64) (bool, error) {
	return f.allowed, f.err
}

type fakeBindings struct {
	binding agentidentity.AgentIdentityBinding
	err     error
}

func (f *fakeBindings) GetUsable(_ context.Context, id string) (agentidentity.AgentIdentityBinding, error) {
	if f.err != nil {
		return agentidentity.AgentIdentityBinding{}, f.err
	}
	if id != f.binding.ID {
		return agentidentity.AgentIdentityBinding{}, agentidentity.ErrBindingNotFound
	}
	return f.binding, nil
}

type fakeToolkits struct {
	binding credentialgrant.AgentToolkitBinding
	err     error
}

func (f *fakeToolkits) AgentToolkitBinding(_ context.Context, bindingID string, toolkitID int64) (credentialgrant.AgentToolkitBinding, error) {
	if f.err != nil {
		return credentialgrant.AgentToolkitBinding{}, f.err
	}
	if bindingID != f.binding.AgentBindingID || toolkitID != f.binding.ToolkitID {
		return credentialgrant.AgentToolkitBinding{}, credentialgrant.ErrToolkitNotBound
	}
	return f.binding, nil
}

type fakeGrants struct {
	grants []credentialgrant.CredentialUseGrant
	err    error
	reads  int
}

func (f *fakeGrants) GrantForKey(_ context.Context, key credentialgrant.GrantKey) (credentialgrant.CredentialUseGrant, error) {
	f.reads++
	if f.err != nil {
		return credentialgrant.CredentialUseGrant{}, f.err
	}
	for _, g := range f.grants {
		if g.TenantProjectID == key.TenantProjectID && g.AgentBindingID == key.AgentBindingID &&
			g.ToolkitID == key.ToolkitID && g.CredentialReferenceID == key.CredentialReferenceID &&
			g.TargetBindingID == key.TargetBindingID && g.State != credentialgrant.GrantRevoked {
			return g, nil
		}
	}
	return credentialgrant.CredentialUseGrant{}, credentialgrant.ErrGrantNotFound
}

type fakeReferences struct {
	reference credentialgrant.CredentialReference
	err       error
}

func (f *fakeReferences) CredentialReference(_ context.Context, tenant int64, id string) (credentialgrant.CredentialReference, error) {
	if f.err != nil {
		return credentialgrant.CredentialReference{}, f.err
	}
	if id != f.reference.ID || tenant != f.reference.TenantProjectID {
		return credentialgrant.CredentialReference{}, credentialgrant.ErrReferenceNotFound
	}
	return f.reference, nil
}

type fakeMandates struct {
	mandate mandate.Mandate
	err     error
}

func (f *fakeMandates) Get(_ context.Context, id string) (mandate.Mandate, error) {
	if f.err != nil {
		return mandate.Mandate{}, f.err
	}
	if id != f.mandate.ID {
		return mandate.Mandate{}, mandate.ErrMandateNotFound
	}
	return f.mandate, nil
}

type fakeDelegations struct {
	delegation mandate.Delegation
	err        error
}

func (f *fakeDelegations) Delegation(_ context.Context, id string) (mandate.Delegation, error) {
	if f.err != nil {
		return mandate.Delegation{}, f.err
	}
	if id != f.delegation.ID {
		return mandate.Delegation{}, mandate.ErrDelegationNotUsable
	}
	return f.delegation, nil
}

type fakePins struct {
	pin profile.Pin
	err error
}

func (f *fakePins) ReadPin(_ context.Context, executionID string) (profile.Pin, error) {
	if f.err != nil {
		return profile.Pin{}, f.err
	}
	if executionID != f.pin.ExecutionID {
		return profile.Pin{}, profile.ErrPinNotFound
	}
	return f.pin, nil
}

type fakeAuthSources struct{ allow bool }

func (f *fakeAuthSources) AllowsAuthSource(string, auth.AuthenticationSource) bool { return f.allow }

// world is one fully permitting set of records.
type world struct {
	authSources *fakeAuthSources
	resolver    *fakeResolver
	entity      *fakeEntityAccess
	bindings    *fakeBindings
	toolkits    *fakeToolkits
	grants      *fakeGrants
	references  *fakeReferences
	registry    credentialgrant.StaticRegistry
	mandates    *fakeMandates
	delegations *fakeDelegations
	pins        *fakePins
}

func newWorld() *world {
	expires := testNow.Add(time.Hour)
	return &world{
		authSources: &fakeAuthSources{allow: true},
		resolver:    &fakeResolver{perms: map[int64][]string{testUser: credentialgrant.Permissions()}},
		entity:      &fakeEntityAccess{allowed: true},
		bindings: &fakeBindings{binding: agentidentity.AgentIdentityBinding{
			ID: testBindingID, TenantProjectID: testTenant, ApplicationID: 3, ApplicationVersionID: 4,
			DefinitionDigest: testDefinitionDigest, State: agentidentity.BindingActive, RevocationEpoch: 2,
		}},
		toolkits: &fakeToolkits{binding: credentialgrant.AgentToolkitBinding{
			AgentBindingID: testBindingID, TenantProjectID: testTenant, ToolkitID: testToolkitID, ToolkitDigest: testToolkitDigest,
		}},
		grants: &fakeGrants{grants: []credentialgrant.CredentialUseGrant{{
			ID: testGrantID, TenantProjectID: testTenant, AgentBindingID: testBindingID, ToolkitID: testToolkitID,
			CredentialReferenceID: testCredential, TargetBindingID: testTargetA,
			Capabilities:    []credentialgrant.Capability{credentialgrant.CapUse},
			OperationScopes: []string{string(testOperation)}, ResourceScopes: []string{"PROJ"},
			Obligations: []string{credentialgrant.ObligationAuditSync},
			State:       credentialgrant.GrantActive, Epoch: 4, ExpiresAt: &expires,
			ProposedBy: "100", ApprovedBy: "200",
		}}},
		references: &fakeReferences{reference: credentialgrant.CredentialReference{
			ID: testCredential, TenantProjectID: testTenant, Class: credentialgrant.ClassStaticSecret,
			Backend: "pgvault", Locator: "p7/jira", ProviderType: "jira", TargetBindingID: testTargetA,
			Generation: 1, State: credentialgrant.ReferenceActive,
		}},
		registry: credentialgrant.StaticRegistry{testOperation: "read", "jira.issue.write": "write"},
		mandates: &fakeMandates{mandate: mandate.Mandate{
			ID: testMandateID, TenantProjectID: testTenant, Kind: mandate.KindAutonomous, AgentBindingID: testBindingID,
			OperationScopes: []string{string(testOperation), "jira.issue.write"},
			NotBefore:       testNow.Add(-time.Hour), ExpiresAt: testNow.Add(time.Hour),
			State: mandate.StateActive, RevocationEpoch: 3,
		}},
		delegations: &fakeDelegations{delegation: mandate.Delegation{
			ID: testDelegation, TenantProjectID: testTenant, SubjectObjectID: "oid-1", AgentBindingID: testBindingID,
			ExpiresAt: testNow.Add(time.Hour), State: mandate.StateActive, RevocationEpoch: 1,
		}},
		pins: &fakePins{pin: profile.Pin{
			ExecutionID: testExecution, Profile: profile.ProfileGoverned,
			PolicyRevisionID: testRevision, PolicyDigest: testPolicyDigest,
		}},
	}
}

func (w *world) decider(t testing.TB) *Decider {
	t.Helper()
	d, err := NewGovernedDecider(Dependencies{
		AuthSources: w.authSources, Permissions: w.resolver, EntityAccess: w.entity,
		Bindings: w.bindings, Toolkits: w.toolkits, Grants: w.grants, References: w.references,
		Operations: w.registry, Mandates: w.mandates, Delegations: w.delegations, Pins: w.pins,
		Now: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// permitRequest is permitted by newWorld.
func permitRequest() OperationRequest {
	return OperationRequest{
		Subject: Subject{
			PrincipalRef: "user:5", UserID: testUser, AuthSource: auth.AuthenticationSourceSession,
			WorkloadID: "spiffe://elitea/worker",
		},
		Agent: AgentRef{
			BindingID: testBindingID, Epoch: 2, ApplicationID: 3, ApplicationVersionID: 4,
			DefinitionDigest: testDefinitionDigest,
		},
		Authority: AuthorityRef{Kind: AuthorityMandate, ID: testMandateID, Epoch: 3},
		Action:    Action{OperationID: testOperation, Capability: credentialgrant.CapUse},
		Resource: Resource{
			ToolkitID: testToolkitID, ToolkitDigest: testToolkitDigest,
			CredentialReferenceID: testCredential, TargetBindingID: testTargetA, ResourceRef: "PROJ",
		},
		Context: DecisionContext{
			TenantProjectID: testTenant, PolicyRevisionID: testRevision, PolicyDigest: testPolicyDigest,
			ExecutionID: testExecution, InvocationID: "inv-1",
		},
	}
}
