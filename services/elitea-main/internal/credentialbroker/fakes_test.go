package credentialbroker

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/authzcompose"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	tTenant      = int64(42)
	tUser        = int64(7)
	tApprover    = int64(9)
	tRevision    = "2026-10-r1"
	tExecution   = "execution-1"
	tGeneration  = uint64(3)
	tBinding     = "0192f0aa-0000-7000-8000-000000000001"
	tMandate     = "0192f0aa-0000-7000-8000-000000000002"
	tDelegation  = "0192f0aa-0000-7000-8000-000000000003"
	tReference   = "0192f0aa-0000-7000-8000-000000000004"
	tReferenceB  = "0192f0aa-0000-7000-8000-000000000005"
	tTargetA     = "0192f0aa-0000-7000-8000-00000000000a"
	tTargetB     = "0192f0aa-0000-7000-8000-00000000000b"
	tGrant       = "0192f0aa-0000-7000-8000-000000000006"
	tToolkit     = int64(11)
	tOperation   = "jira.issue.read"
	tWorkload    = "spiffe://elitea.internal/runtime/worker-1"
	tSecretValue = "s3cr3t-value-never-logged"
	tOAuthValue  = "oauth-access-never-logged"
	tInvocation  = "0192f0aa-0000-7000-8000-0000000000f1"
	tInvocation2 = "0192f0aa-0000-7000-8000-0000000000f2"
)

var (
	tPolicyDigest  = [32]byte{0xaa}
	tDefinition    = [32]byte{0xdd}
	tToolkitDigest = [32]byte{0x70}
	tNow           = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
)

// world is every dependency of the broker, faked, with the REAL governed
// decider over them: a test that changes a grant changes the decision.
type world struct {
	pins        *fakePins
	references  *fakeReferences
	targets     *fakeTargets
	toolkits    *fakeToolkits
	bindings    *fakeBindings
	grants      *fakeGrants
	mandates    *fakeMandates
	delegations *fakeDelegations
	resolver    *fakeResolver
	secrets     *fakeSecretStore
	store       *memStore
	sessions    *fakeSessions
	exchanger   *fakeExchanger
	custody     *fakeCustody
	catalog     fakeCatalog
	context     securitycontext.Context
}

func newWorld() *world {
	expires := tNow.Add(time.Hour)
	return &world{
		pins: &fakePins{pin: profile.Pin{ExecutionID: tExecution, Profile: profile.ProfileGoverned, PolicyRevisionID: tRevision, PolicyDigest: tPolicyDigest}},
		references: &fakeReferences{refs: map[string]credentialgrant.CredentialReference{
			tReference: {ID: tReference, TenantProjectID: tTenant, Class: credentialgrant.ClassStaticSecret, Backend: secretstore.BackendPGVault,
				Locator: "jira_token", ProviderType: "jira", TargetBindingID: tTargetA, Generation: 1, State: credentialgrant.ReferenceActive},
			tReferenceB: {ID: tReferenceB, TenantProjectID: tTenant, Class: credentialgrant.ClassStaticSecret, Backend: secretstore.BackendPGVault,
				Locator: "jira_token_b", ProviderType: "jira", TargetBindingID: tTargetB, Generation: 1, State: credentialgrant.ReferenceActive},
		}},
		targets: &fakeTargets{targets: map[string]credentialgrant.TargetBinding{
			tTargetA: {ID: tTargetA, TenantProjectID: tTenant, ProviderType: "jira", TargetIdentity: "https://a.example.com", Audience: "api://jira-a"},
			tTargetB: {ID: tTargetB, TenantProjectID: tTenant, ProviderType: "jira", TargetIdentity: "https://b.example.com"},
		}},
		toolkits: &fakeToolkits{binding: credentialgrant.AgentToolkitBinding{AgentBindingID: tBinding, TenantProjectID: tTenant, ToolkitID: tToolkit, ToolkitDigest: tToolkitDigest}},
		bindings: &fakeBindings{binding: identity.AgentIdentityBinding{ID: tBinding, TenantProjectID: tTenant, ApplicationID: 3, ApplicationVersionID: 4,
			DefinitionDigest: tDefinition, State: identity.BindingActive, RevocationEpoch: 2}},
		grants: &fakeGrants{grants: []credentialgrant.CredentialUseGrant{{
			ID: tGrant, TenantProjectID: tTenant, AgentBindingID: tBinding, ToolkitID: tToolkit,
			CredentialReferenceID: tReference, TargetBindingID: tTargetA,
			Capabilities: []credentialgrant.Capability{credentialgrant.CapUse}, OperationScopes: []string{tOperation},
			Obligations: []string{credentialgrant.ObligationAuditSync}, State: credentialgrant.GrantActive, Epoch: 4, ExpiresAt: &expires,
		}}},
		mandates: &fakeMandates{mandates: map[string]mandate.Mandate{}},
		delegations: &fakeDelegations{delegations: map[string]mandate.Delegation{tDelegation: {
			ID: tDelegation, TenantProjectID: tTenant, Issuer: "https://login.microsoftonline.com/t/v2.0", TenantID: "t",
			SubjectObjectID: "oid-7", AgentBindingID: tBinding, Audiences: []string{"api://middle"}, ExpiresAt: expires,
			State: mandate.StateActive, RevocationEpoch: 1,
		}}},
		resolver:  &fakeResolver{perms: map[int64][]string{tUser: credentialgrant.Permissions(), tApprover: credentialgrant.Permissions()}},
		secrets:   &fakeSecretStore{value: []byte(`{"api_token":"` + tSecretValue + `","unused":"drop-me"}`)},
		store:     newMemStore(),
		sessions:  &fakeSessions{},
		exchanger: &fakeExchanger{token: obo.AccessToken{Token: tOAuthValue, ExpiresAt: tNow.Add(2 * time.Minute)}},
		custody:   &fakeCustody{},
		catalog: fakeCatalog{ops: map[credentialgrant.OperationID]string{tOperation: "read"}, keys: map[credentialgrant.CredentialClass][]string{
			credentialgrant.ClassStaticSecret: {"api_token"}, credentialgrant.ClassOAuthDelegated: {"access_token"},
			credentialgrant.ClassDynamicLease: {"lease"},
		}},
		context: securitycontext.Context{
			PolicyRevisionID: tRevision, PolicyDigest: runtimedomain.Digest(tPolicyDigest), TenantProjectID: tTenant,
			SubjectPrincipalRef: strconv.FormatInt(tUser, 10), AgentBindingID: tBinding, AgentBindingEpoch: 2,
			MandateID: tMandate, MandateEpoch: 5,
			Caller: securitycontext.Caller{AuthenticationSource: "session"}, DefinitionDigest: runtimedomain.Digest(tDefinition),
			ActorChain: []securitycontext.ActorLink{{Kind: "user", Ref: "7"}}, IssuedAt: tNow,
		},
	}
}

// withMandate stores a mandate for the context's mandate id.
func (w *world) withMandate(kind mandate.Kind) *world {
	m := mandate.Mandate{
		ID: tMandate, TenantProjectID: tTenant, Kind: kind, AgentBindingID: tBinding, OperationScopes: []string{tOperation},
		NotBefore: tNow.Add(-time.Hour), ExpiresAt: tNow.Add(time.Hour), State: mandate.StateActive, RevocationEpoch: 5,
		ApprovedBy: strconv.FormatInt(tApprover, 10),
	}
	if kind == mandate.KindDelegatedBackground {
		user := tUser
		m.SubjectUserID = &user
	}
	w.mandates.mandates[tMandate] = m
	return w
}

func (w *world) broker(t *testing.T, options ...func(*Config)) *Broker {
	t.Helper()
	decider, err := authzcompose.NewGovernedDecider(authzcompose.Dependencies{
		AuthSources: authzcompose.DefaultAuthSources{}, Permissions: w.resolver, EntityAccess: allowEntities{},
		Bindings: w.bindings, Toolkits: w.toolkits, Grants: w.grants, References: w.references, Operations: w.catalog,
		Mandates: w.mandates, Delegations: w.delegations, Pins: w.pins, Now: func() time.Time { return tNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Decider: decider, References: w.references, Targets: w.targets, Toolkits: w.toolkits, Bindings: w.bindings,
		Mandates: w.mandates, Delegations: w.delegations,
		SecretStores: map[string]secretstore.SecretStore{secretstore.BackendPGVault: w.secrets},
		Exchanger:    w.exchanger, Custody: w.custody, Store: w.store, Pins: w.pins, Sessions: w.sessions,
		Catalog: w.catalog, Now: func() time.Time { return tNow },
	}
	for _, option := range options {
		option(&cfg)
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// request is a redemption of the stored context with invocation tInvocation.
func (w *world) request(t *testing.T) RedeemRequest {
	t.Helper()
	encoded, ref, err := w.context.Marshal(tExecution)
	if err != nil {
		t.Fatal(err)
	}
	return RedeemRequest{
		Run: ClaimedRun{
			ExecutionID: tExecution, Generation: tGeneration, ClaimID: "claim-1", WorkloadID: tWorkload,
			TenantProjectID: tTenant, ActorID: "7",
			ConversationUUID: "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1", ResponseMessageID: "6f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c2",
			ContextBytes: encoded, ContextDigest: ref.Digest,
		},
		Invocation: BoundToolInvocation{
			InvocationID: tInvocation, ToolCallID: "call-1", ToolkitID: tToolkit, OperationID: tOperation,
			ResourceRef: "jira:project/KEY", ArgumentsDigest: [32]byte{0x01}, CredentialReferenceID: tReference,
		},
		SecurityContextDigest: ref.Digest,
	}
}

type fakePins struct{ pin profile.Pin }

func (f *fakePins) ReadPin(context.Context, string) (profile.Pin, error) { return f.pin, nil }

type fakeReferences struct {
	refs map[string]credentialgrant.CredentialReference
}

func (f *fakeReferences) CredentialReference(_ context.Context, tenant int64, id string) (credentialgrant.CredentialReference, error) {
	ref, ok := f.refs[id]
	if !ok || ref.TenantProjectID != tenant {
		return credentialgrant.CredentialReference{}, credentialgrant.ErrReferenceNotFound
	}
	return ref, nil
}

type fakeTargets struct {
	targets map[string]credentialgrant.TargetBinding
}

func (f *fakeTargets) TargetBinding(_ context.Context, tenant int64, id string) (credentialgrant.TargetBinding, error) {
	target, ok := f.targets[id]
	if !ok || target.TenantProjectID != tenant {
		return credentialgrant.TargetBinding{}, credentialgrant.ErrTargetNotFound
	}
	return target, nil
}

type fakeToolkits struct {
	binding credentialgrant.AgentToolkitBinding
}

func (f *fakeToolkits) AgentToolkitBinding(_ context.Context, binding string, toolkit int64) (credentialgrant.AgentToolkitBinding, error) {
	if binding != f.binding.AgentBindingID || toolkit != f.binding.ToolkitID {
		return credentialgrant.AgentToolkitBinding{}, credentialgrant.ErrToolkitNotBound
	}
	return f.binding, nil
}

type fakeBindings struct{ binding identity.AgentIdentityBinding }

func (f *fakeBindings) GetUsable(_ context.Context, id string) (identity.AgentIdentityBinding, error) {
	if id != f.binding.ID {
		return identity.AgentIdentityBinding{}, identity.ErrBindingNotFound
	}
	return f.binding, nil
}

type fakeGrants struct {
	grants []credentialgrant.CredentialUseGrant
}

func (f *fakeGrants) GrantForKey(_ context.Context, key credentialgrant.GrantKey) (credentialgrant.CredentialUseGrant, error) {
	for _, g := range f.grants {
		if g.TenantProjectID == key.TenantProjectID && g.AgentBindingID == key.AgentBindingID && g.ToolkitID == key.ToolkitID &&
			g.CredentialReferenceID == key.CredentialReferenceID && g.TargetBindingID == key.TargetBindingID {
			return g, nil
		}
	}
	return credentialgrant.CredentialUseGrant{}, credentialgrant.ErrGrantNotFound
}

type fakeMandates struct{ mandates map[string]mandate.Mandate }

func (f *fakeMandates) Get(_ context.Context, id string) (mandate.Mandate, error) {
	m, ok := f.mandates[id]
	if !ok {
		return mandate.Mandate{}, mandate.ErrMandateNotFound
	}
	return m, nil
}

type fakeDelegations struct{ delegations map[string]mandate.Delegation }

func (f *fakeDelegations) Delegation(_ context.Context, id string) (mandate.Delegation, error) {
	d, ok := f.delegations[id]
	if !ok {
		return mandate.Delegation{}, mandate.ErrDelegationNotUsable
	}
	return d, nil
}

type fakeResolver struct{ perms map[int64][]string }

func (f *fakeResolver) ResolvePermissions(_ context.Context, user auth.User, _ string, _ string) (auth.PermissionResolution, error) {
	id, _ := strconv.ParseInt(user.UserID, 10, 64)
	perms, ok := f.perms[id]
	if !ok {
		return auth.PermissionResolution{}, auth.ErrPermissionDenied
	}
	return auth.PermissionResolution{UserID: id, Permissions: perms}, nil
}

type allowEntities struct{}

func (allowEntities) AgentAccessible(context.Context, int64, int64, int64) (bool, error) {
	return true, nil
}

type fakeCatalog struct {
	ops  map[credentialgrant.OperationID]string
	keys map[credentialgrant.CredentialClass][]string
}

func (c fakeCatalog) Known(id credentialgrant.OperationID) bool   { _, ok := c.ops[id]; return ok }
func (c fakeCatalog) Class(id credentialgrant.OperationID) string { return c.ops[id] }
func (c fakeCatalog) MaterialKeys(_ string, class credentialgrant.CredentialClass) []string {
	return slices.Clone(c.keys[class])
}

// fakeSecretStore counts reads; it fails the test on any other call.
type fakeSecretStore struct {
	mu    sync.Mutex
	reads int
	last  secretstore.StoreContext
	ref   secretstore.SecretRef
	ver   int64
	value []byte
	err   error
}

func (f *fakeSecretStore) Read(_ context.Context, sc secretstore.StoreContext, ref secretstore.SecretRef, version int64) (secretstore.SecretVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	f.last, f.ref, f.ver = sc, ref, version
	if err := sc.Validate(secretstore.OpRead); err != nil {
		return secretstore.SecretVersion{}, err
	}
	if f.err != nil {
		return secretstore.SecretVersion{}, f.err
	}
	return secretstore.SecretVersion{Version: version, Value: secretstore.NewRuntimeSecret(f.value)}, nil
}

func (f *fakeSecretStore) Write(context.Context, secretstore.StoreContext, secretstore.SecretRef, secretstore.RuntimeSecret, int64) (int64, error) {
	panic("the broker never writes a secret")
}

func (f *fakeSecretStore) Metadata(context.Context, secretstore.StoreContext, secretstore.SecretRef) (secretstore.Metadata, error) {
	panic("the broker never reads metadata")
}

func (f *fakeSecretStore) Retire(context.Context, secretstore.StoreContext, secretstore.SecretRef, int64) error {
	panic("the broker never retires a secret")
}

func (f *fakeSecretStore) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

type invocationKey struct {
	execution  string
	generation uint64
	invocation string
}

// memStore is an in-memory Store whose audit append can fail.
type memStore struct {
	mu          sync.Mutex
	invocations map[invocationKey]BoundToolInvocation
	redemptions []Redemption
	events      []securityaudit.SecurityEvent
	sessions    map[invocationKey]string
	ceiling     []string
	failAudit   func(securityaudit.SecurityEvent) bool
}

func newMemStore() *memStore {
	return &memStore{
		invocations: map[invocationKey]BoundToolInvocation{}, sessions: map[invocationKey]string{},
		ceiling: []string{"api://jira-a"},
	}
}

func (s *memStore) append(event securityaudit.SecurityEvent) error {
	if s.failAudit != nil && s.failAudit(event) {
		return securityaudit.ErrAppendFailed
	}
	event.ID, event.Source = "0192f0aa-0000-7000-8000-0000000000e1", "elitea-main/test"
	if err := event.Validate(); err != nil {
		return err
	}
	s.events = append(s.events, event)
	return nil
}

func (s *memStore) AdmitInvocation(ctx context.Context, run ClaimedRun, inv BoundToolInvocation,
	decide func(context.Context) (securityaudit.SecurityEvent, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := invocationKey{run.ExecutionID, run.Generation, inv.InvocationID}
	stored, replay := s.invocations[key]
	if replay && stored != inv {
		return ErrInvocationMismatch
	}
	event, err := decide(ctx)
	if err != nil {
		return err
	}
	if err := s.append(event); err != nil {
		return errors.Join(ErrAuditFailed, err)
	}
	s.invocations[key] = inv
	return nil
}

func (s *memStore) RecordRedemption(_ context.Context, _ ClaimedRun, r Redemption, event securityaudit.SecurityEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.append(event); err != nil {
		return errors.Join(ErrAuditFailed, err)
	}
	s.redemptions = append(s.redemptions, r)
	return nil
}

func (s *memStore) AgentSession(_ context.Context, execution string, generation uint64) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.sessions[invocationKey{execution, generation, ""}]
	return ref, ok, nil
}

func (s *memStore) LinkAgentSession(_ context.Context, execution string, generation uint64, ref string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := invocationKey{execution, generation, ""}
	if linked, ok := s.sessions[key]; ok {
		return linked, nil
	}
	s.sessions[key] = ref
	return ref, nil
}

func (s *memStore) SessionCeiling(context.Context, int64, string) ([]string, error) {
	return slices.Clone(s.ceiling), nil
}

type fakeSessions struct {
	mu     sync.Mutex
	opened []agentidentity.SessionAdmission
	err    error
}

func (f *fakeSessions) OpenSession(_ context.Context, in agentidentity.SessionAdmission) (agentidentity.SessionRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.opened = append(f.opened, in)
	return agentidentity.SessionRef("ags1_test-session-" + strconv.Itoa(len(f.opened))), nil
}

type fakeExchanger struct {
	calls int
	token obo.AccessToken
	err   error
	last  obo.ExchangeRequest
}

func (f *fakeExchanger) Exchange(_ context.Context, _ obo.Assertion, r obo.ExchangeRequest) (obo.AccessToken, error) {
	f.calls++
	f.last = r
	if f.err != nil {
		return obo.AccessToken{}, f.err
	}
	return f.token, nil
}

type fakeCustody struct {
	loads        int
	requirements []delegationstore.Requirement
	loadErr      error
}

func (f *fakeCustody) Load(_ context.Context, r delegationstore.LoadRequest) (obo.Assertion, delegationstore.Delegation, error) {
	f.loads++
	if f.loadErr != nil {
		return obo.Assertion{}, delegationstore.Delegation{}, f.loadErr
	}
	return obo.Assertion{Token: "assertion-never-logged", TenantID: "t", ObjectID: "oid-7", Audience: "api://middle", ExpiresAt: tNow.Add(time.Hour)},
		delegationstore.Delegation{ID: r.DelegationID}, nil
}

func (f *fakeCustody) RecordRequirement(_ context.Context, r delegationstore.Requirement) (delegationstore.Requirement, error) {
	r.Reference = "car_opaque-reference"
	f.requirements = append(f.requirements, r)
	return r, nil
}
