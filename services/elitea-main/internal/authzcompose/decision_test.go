package authzcompose

import (
	"context"
	"errors"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

func requireDeny(t *testing.T, d OperationDecision, gate string, reason ReasonCode) {
	t.Helper()
	if d.Permitted() {
		t.Fatalf("decision permitted; want a deny by %s with %s", gate, reason)
	}
	if gate != "" && !slices.Contains(d.DeniedBy, gate) {
		t.Errorf("DeniedBy = %v, want it to name %s", d.DeniedBy, gate)
	}
	if reason != "" && !slices.Contains(d.Reasons, reason) {
		t.Errorf("Reasons = %v, want %s", d.Reasons, reason)
	}
	if d.Obligations != nil {
		t.Errorf("a deny carries obligations %v", d.Obligations)
	}
}

// One decision reads its grant once: every gate judges the same row, and a
// decision costs one grant read however many gates look at it.
func TestDecideReadsTheGrantOncePerDecision(t *testing.T) {
	w := newWorld()
	decider := w.decider(t)
	if d := decider.Decide(context.Background(), permitRequest()); !d.Permitted() {
		t.Fatalf("decision = %s %v", d.Effect, d.Reasons)
	}
	if w.grants.reads != 1 {
		t.Fatalf("grant reads = %d, want 1", w.grants.reads)
	}
	decider.Decide(context.Background(), permitRequest())
	if w.grants.reads != 2 {
		t.Fatalf("a second decision reused the first one's grant: reads = %d", w.grants.reads)
	}
}

func TestDecideAllGatesPermitIsAPermit(t *testing.T) {
	w := newWorld()
	d := w.decider(t).Decide(context.Background(), permitRequest())
	if !d.Permitted() {
		t.Fatalf("decision = %s %v denied by %v, want permit", d.Effect, d.Reasons, d.DeniedBy)
	}
	if len(d.Reasons) != 0 || len(d.DeniedBy) != 0 {
		t.Errorf("a permit carries reasons %v / denied by %v", d.Reasons, d.DeniedBy)
	}
	if parsed, err := uuid.Parse(d.DecisionID); err != nil || parsed.Version() != 7 {
		t.Errorf("DecisionID %q is not a uuid v7", d.DecisionID)
	}
	if d.Arrangement != "C" || d.PolicyRevisionID != testRevision {
		t.Errorf("arrangement %q, revision %q", d.Arrangement, d.PolicyRevisionID)
	}
	if want := map[string]int64{"agent_binding": 2, "grant": 4, "credential_reference": 1, "mandate": 3}; !mapsEqual(d.EvaluatedEpochs, want) {
		t.Errorf("EvaluatedEpochs = %v, want %v", d.EvaluatedEpochs, want)
	}
	if len(d.Obligations) != 1 || d.Obligations[0].Kind != credentialgrant.ObligationAuditSync {
		t.Errorf("Obligations = %v, want the grant's audit.sync", d.Obligations)
	}
	if got := w.decider(t).GateNames(); !slices.Equal(got, []string{
		"authenticated_context", "rbac", "entity_access", "agent_binding", "grant", "scope", "authority", "policy_revision",
	}) {
		t.Errorf("gate order = %v", got)
	}
}

func mapsEqual(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Flipping exactly one gate's input to deny makes the decision deny, and the
// deny names that gate.
func TestDecideEachGateAloneDenies(t *testing.T) {
	cases := []struct {
		gate   string
		reason ReasonCode
		flip   func(w *world, r *OperationRequest)
	}{
		{"authenticated_context", ReasonAuthSourceNotAllowed, func(w *world, _ *OperationRequest) { w.authSources.allow = false }},
		{"rbac", ReasonPermissionMissing, func(w *world, _ *OperationRequest) {
			w.resolver.perms[testUser] = []string{credentialgrant.PermissionReveal}
		}},
		{"entity_access", ReasonEntityAccessDenied, func(w *world, _ *OperationRequest) { w.entity.allowed = false }},
		{"agent_binding", ReasonBindingNotActive, func(w *world, _ *OperationRequest) {
			w.bindings.binding.State = agentidentity.BindingSuspended
		}},
		{"grant", ReasonGrantNotActive, func(w *world, _ *OperationRequest) {
			w.grants.grants[0].State = credentialgrant.GrantSuspended
		}},
		{"scope", ReasonResourceOutOfScope, func(_ *world, r *OperationRequest) { r.Resource.ResourceRef = "OTHER" }},
		{"authority", ReasonAuthorityNotActive, func(w *world, _ *OperationRequest) { w.mandates.mandate.State = mandate.StateSuspended }},
		{"policy_revision", ReasonPolicyRevisionNotPin, func(w *world, _ *OperationRequest) { w.pins.pin.PolicyRevisionID = "2026-10-r0" }},
	}
	if len(cases) != 8 {
		t.Fatalf("%d cases, want one per gate", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.gate, func(t *testing.T) {
			w, r := newWorld(), permitRequest()
			tc.flip(w, &r)
			d := w.decider(t).Decide(context.Background(), r)
			requireDeny(t, d, tc.gate, tc.reason)
			if !slices.Equal(d.DeniedBy, []string{tc.gate}) {
				t.Errorf("DeniedBy = %v, want only %s", d.DeniedBy, tc.gate)
			}
		})
	}
}

// Every gate that returns an error yields a deny with GATE_ERROR.
func TestDecideGateErrorIsDenyWithGateError(t *testing.T) {
	cases := map[string]func(w *world){
		"rbac":            func(w *world) { w.resolver.err = errBackend },
		"entity_access":   func(w *world) { w.entity.err = errBackend },
		"agent_binding":   func(w *world) { w.bindings.err = errBackend },
		"grant":           func(w *world) { w.references.err = errBackend },
		"authority":       func(w *world) { w.mandates.err = errBackend },
		"policy_revision": func(w *world) { w.pins.err = errBackend },
	}
	for gate, flip := range cases {
		t.Run(gate, func(t *testing.T) {
			w := newWorld()
			flip(w)
			requireDeny(t, w.decider(t).Decide(context.Background(), permitRequest()), gate, ReasonGateError)
		})
	}
}

type stubGate struct {
	name   string
	result GateResult
	err    error
	panics bool
}

func (g stubGate) Name() string { return g.name }
func (g stubGate) Evaluate(context.Context, OperationRequest) (GateResult, error) {
	if g.panics {
		panic("boom")
	}
	return g.result, g.err
}

func TestDecideStubGateErrorAndPanicAreGateErrors(t *testing.T) {
	permit := stubGate{name: "ok", result: GateResult{Permit: true}}
	for name, gate := range map[string]stubGate{
		"error": {name: "bad", err: errors.New("x")},
		"panic": {name: "bad", panics: true},
	} {
		t.Run(name, func(t *testing.T) {
			d, err := NewDecider(permit, gate)
			if err != nil {
				t.Fatal(err)
			}
			requireDeny(t, d.Decide(context.Background(), permitRequest()), "bad", ReasonGateError)
		})
	}
	// A deny without a reason still carries one.
	d, _ := NewDecider(permit, stubGate{name: "quiet"})
	requireDeny(t, d.Decide(context.Background(), permitRequest()), "quiet", "QUIET_DENIED")
}

// A missing agent, authority or policy revision is a deny with a typed
// reason, even when every gate would permit.
func TestDecideMissingInputsDenyWithTypedReasons(t *testing.T) {
	permitAll, err := NewDecider(stubGate{name: "ok", result: GateResult{Permit: true}})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		mutate func(*OperationRequest)
		reason ReasonCode
	}{
		"agent":     {func(r *OperationRequest) { r.Agent = AgentRef{} }, ReasonAgentMissing},
		"authority": {func(r *OperationRequest) { r.Authority = AuthorityRef{} }, ReasonAuthorityMissing},
		"revision":  {func(r *OperationRequest) { r.Context.PolicyRevisionID = "" }, ReasonPolicyRevisionMissing},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := permitRequest()
			tc.mutate(&r)
			requireDeny(t, permitAll.Decide(context.Background(), r), "", tc.reason)
			// And with the real gates.
			requireDeny(t, newWorld().decider(t).Decide(context.Background(), r), "", tc.reason)
		})
	}
	r := permitRequest()
	r.Authority.Kind = "owner"
	requireDeny(t, permitAll.Decide(context.Background(), r), "", ReasonAuthorityUnknown)
	r = permitRequest()
	r.Action.Capability = "admin"
	requireDeny(t, permitAll.Decide(context.Background(), r), "", ReasonCapabilityUnknown)
}

// A use grant never permits reveal, manage or probe, even for a subject
// holding every permission.
func TestUseGrantDoesNotPermitOtherCapabilities(t *testing.T) {
	for _, capability := range []credentialgrant.Capability{credentialgrant.CapReveal, credentialgrant.CapManage, credentialgrant.CapProbe} {
		t.Run(string(capability), func(t *testing.T) {
			w, r := newWorld(), permitRequest()
			r.Action.Capability = capability
			requireDeny(t, w.decider(t).Decide(context.Background(), r), "grant", ReasonCapabilityNotGranted)
		})
	}
}

// A grant for target A does not permit target B with the same credential.
func TestGrantForTargetADoesNotPermitTargetB(t *testing.T) {
	w, r := newWorld(), permitRequest()
	r.Resource.TargetBindingID = testTargetB
	requireDeny(t, w.decider(t).Decide(context.Background(), r), "grant", ReasonGrantMissing)

	// Even with a grant row keyed on B, the credential is bound to A.
	g := w.grants.grants[0]
	g.TargetBindingID = testTargetB
	w.grants.grants = append(w.grants.grants, g)
	requireDeny(t, w.decider(t).Decide(context.Background(), r), "grant", ReasonCredentialTargetDiffer)
}

func TestUnknownOperationIsDenied(t *testing.T) {
	w := newWorld()
	for _, op := range []credentialgrant.OperationID{"jira.issue.delete", "not-canonical"} {
		r := permitRequest()
		r.Action.OperationID = op
		requireDeny(t, w.decider(t).Decide(context.Background(), r), "scope", ReasonOperationUnknown)
	}
	// The empty registry wired until #1120 denies every operation.
	w.registry = credentialgrant.EmptyRegistry
	requireDeny(t, w.decider(t).Decide(context.Background(), permitRequest()), "scope", ReasonOperationUnknown)
}

func TestKnownOperationOutsideGrantScopeIsDenied(t *testing.T) {
	w, r := newWorld(), permitRequest()
	r.Action.OperationID = "jira.issue.write" // known, in the mandate, not in the grant
	requireDeny(t, w.decider(t).Decide(context.Background(), r), "scope", ReasonOperationOutOfScope)
}

func TestStaleEpochsAndDigestsDeny(t *testing.T) {
	cases := map[string]struct {
		mutate func(*OperationRequest)
		gate   string
		reason ReasonCode
	}{
		"binding epoch":    {func(r *OperationRequest) { r.Agent.Epoch = 1 }, "agent_binding", ReasonBindingEpochStale},
		"definition":       {func(r *OperationRequest) { r.Agent.DefinitionDigest = [32]byte{9} }, "agent_binding", ReasonBindingDigestMismatch},
		"toolkit digest":   {func(r *OperationRequest) { r.Resource.ToolkitDigest = [32]byte{9} }, "agent_binding", ReasonToolkitDigestMismatch},
		"mandate epoch":    {func(r *OperationRequest) { r.Authority.Epoch = 2 }, "authority", ReasonAuthorityEpochStale},
		"policy digest":    {func(r *OperationRequest) { r.Context.PolicyDigest = [32]byte{9} }, "policy_revision", ReasonPolicyRevisionNotPin},
		"legacy execution": {func(r *OperationRequest) { r.Context.ExecutionID = "exec-legacy" }, "policy_revision", ReasonPolicyRevisionNotPin},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w, r := newWorld(), permitRequest()
			tc.mutate(&r)
			requireDeny(t, w.decider(t).Decide(context.Background(), r), tc.gate, tc.reason)
		})
	}
}

func TestAuthorityRules(t *testing.T) {
	t.Run("expired grant", func(t *testing.T) {
		w := newWorld()
		past := testNow.Add(-time.Minute)
		w.grants.grants[0].ExpiresAt = &past
		requireDeny(t, w.decider(t).Decide(context.Background(), permitRequest()), "grant", ReasonGrantNotActive)
	})
	t.Run("delegated background names another user", func(t *testing.T) {
		w := newWorld()
		other := int64(6)
		w.mandates.mandate.Kind, w.mandates.mandate.SubjectUserID = mandate.KindDelegatedBackground, &other
		requireDeny(t, w.decider(t).Decide(context.Background(), permitRequest()), "authority", ReasonAuthoritySubjectDiffer)
	})
	t.Run("mandate of another binding", func(t *testing.T) {
		w := newWorld()
		w.mandates.mandate.AgentBindingID = testGrantID
		requireDeny(t, w.decider(t).Decide(context.Background(), permitRequest()), "authority", ReasonAuthorityScopeMismatch)
	})
	t.Run("delegation permits for its subject only", func(t *testing.T) {
		w, r := newWorld(), permitRequest()
		r.Authority = AuthorityRef{Kind: AuthorityDelegation, ID: testDelegation, Epoch: 1}
		r.Subject.ExternalSubjectID = "oid-1"
		if d := w.decider(t).Decide(context.Background(), r); !d.Permitted() {
			t.Fatalf("delegation decision = %v %v", d.Reasons, d.DeniedBy)
		}
		r.Subject.ExternalSubjectID = "oid-2"
		requireDeny(t, w.decider(t).Decide(context.Background(), r), "authority", ReasonAuthoritySubjectDiffer)
	})
	t.Run("no delegations wired", func(t *testing.T) {
		w, r := newWorld(), permitRequest()
		r.Authority = AuthorityRef{Kind: AuthorityDelegation, ID: testDelegation, Epoch: 1}
		gate, err := NewAuthorityGate(w.mandates, NoDelegations{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := gate.Evaluate(context.Background(), r)
		if err != nil || result.Permit {
			t.Fatalf("NoDelegations permitted: %+v %v", result, err)
		}
	})
}

func TestFulfilAndEnforce(t *testing.T) {
	obligations := []Obligation{{Kind: credentialgrant.ObligationAuditSync}, {Kind: credentialgrant.ObligationRequireApproval}}
	if err := Fulfil(obligations, map[string]bool{credentialgrant.ObligationAuditSync: true}); !errors.Is(err, ErrObligationUnsupported) {
		t.Fatalf("Fulfil = %v, want ErrObligationUnsupported", err)
	}
	if err := Fulfil(obligations, map[string]bool{
		credentialgrant.ObligationAuditSync: true, credentialgrant.ObligationRequireApproval: true,
	}); err != nil {
		t.Fatalf("Fulfil with full support = %v", err)
	}

	// A permit whose obligation the enforcement point cannot fulfil is a deny.
	w := newWorld()
	w.grants.grants[0].Obligations = []string{credentialgrant.ObligationRequireApproval}
	d := w.decider(t).Decide(context.Background(), permitRequest())
	if !d.Permitted() {
		t.Fatalf("decision = %v", d.Reasons)
	}
	if err := Enforce(d, map[string]bool{credentialgrant.ObligationAuditSync: true}); !errors.Is(err, ErrDenied) || !errors.Is(err, ErrObligationUnsupported) {
		t.Fatalf("Enforce = %v, want a deny for the unsupported obligation", err)
	}
	if err := Enforce(d, map[string]bool{credentialgrant.ObligationRequireApproval: true}); err != nil {
		t.Fatalf("Enforce with support = %v", err)
	}
	if err := Enforce(OperationDecision{Effect: EffectDeny}, nil); !errors.Is(err, ErrDenied) {
		t.Fatalf("Enforce(deny) = %v", err)
	}
}

func TestNewDeciderRefusesEmptyNilAndDuplicateGates(t *testing.T) {
	ok := stubGate{name: "a", result: GateResult{Permit: true}}
	for name, gates := range map[string][]Gate{
		"empty":     nil,
		"nil":       {ok, nil},
		"unnamed":   {stubGate{}},
		"duplicate": {ok, stubGate{name: "a"}},
	} {
		if _, err := NewDecider(gates...); !errors.Is(err, ErrInvalidDecider) {
			t.Errorf("%s: NewDecider = %v, want ErrInvalidDecider", name, err)
		}
	}
	if _, err := NewGovernedDecider(Dependencies{}); err == nil {
		t.Error("NewGovernedDecider with no readers was accepted")
	}
}

// Decision ids come from the injected generator; a failure is a deny.
func TestDecisionIDFailureDenies(t *testing.T) {
	d := newWorld().decider(t)
	d.newID = func() (uuid.UUID, error) { return uuid.UUID{}, errors.New("no entropy") }
	requireDeny(t, d.Decide(context.Background(), permitRequest()), "", ReasonDecisionIDUnavailable)
}

// Every reason code is a valid security audit reason in its audit form, so
// the broker (#1118) can record decisions without a mapping table.
func TestReasonCodesAreAuditReasons(t *testing.T) {
	codes := []ReasonCode{
		ReasonGateError, ReasonDecisionIDUnavailable, ReasonTenantMissing, ReasonAgentMissing, ReasonAuthorityMissing,
		ReasonAuthorityUnknown, ReasonPolicyRevisionMissing, ReasonCapabilityUnknown, ReasonOperationMissing,
		ReasonResourceMissing, ReasonSubjectMissing, ReasonWorkloadMissing, ReasonAuthSourceNotAllowed,
		ReasonPermissionMissing, ReasonEntityAccessDenied, ReasonBindingNotActive, ReasonBindingEpochStale,
		ReasonBindingDigestMismatch, ReasonBindingScopeMismatch, ReasonToolkitNotBound, ReasonToolkitDigestMismatch,
		ReasonGrantMissing, ReasonGrantNotActive, ReasonCapabilityNotGranted, ReasonCredentialNotUsable,
		ReasonCredentialTargetDiffer, ReasonOperationUnknown, ReasonOperationOutOfScope, ReasonResourceOutOfScope,
		ReasonAuthorityNotActive, ReasonAuthorityEpochStale, ReasonAuthoritySubjectDiffer, ReasonAuthorityScopeMismatch,
		ReasonPolicyRevisionNotPin,
	}
	reasons := make([]string, len(codes))
	seen := map[string]bool{}
	for i, code := range codes {
		reasons[i] = code.AuditCode()
		if seen[reasons[i]] {
			t.Errorf("duplicate reason %s", code)
		}
		seen[reasons[i]] = true
	}
	sort.Strings(reasons)
	for start := 0; start < len(reasons); start += 32 {
		end := min(start+32, len(reasons))
		event := securityaudit.SecurityEvent{
			ID: uuid.NewString(), Type: securityaudit.TypeDecision, Source: "elitea-main/test", Time: testNow,
			Action: "credential.use", Effect: securityaudit.EffectDeny, Reasons: reasons[start:end],
			Arrangement: securityaudit.ArrangementC,
		}
		if err := event.Validate(); err != nil {
			t.Errorf("reasons %v: %v", reasons[start:end], err)
		}
	}
}

func TestDefaultAuthSourcesRefuseUnknownAndDevelopment(t *testing.T) {
	var policy DefaultAuthSources
	for source, want := range map[auth.AuthenticationSource]bool{
		auth.AuthenticationSourceUnknown:     false,
		auth.AuthenticationSourceAPIKey:      true,
		auth.AuthenticationSourceToken:       true,
		auth.AuthenticationSourceSession:     true,
		auth.AuthenticationSourceDevelopment: false,
	} {
		if got := policy.AllowsAuthSource(testRevision, source); got != want {
			t.Errorf("source %d: %v, want %v", source, got, want)
		}
	}
}
