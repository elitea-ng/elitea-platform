package credentialbroker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

func redeem(t *testing.T, b *Broker, req RedeemRequest) Result {
	t.Helper()
	result, err := b.Redeem(context.Background(), req)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if (result.Envelope == nil) == (result.Denial == nil) {
		t.Fatalf("result must be exactly one of envelope and denial: %+v", result)
	}
	return result
}

func requireDenial(t *testing.T, result Result, code DenialCode) *Denial {
	t.Helper()
	if result.Denial == nil {
		result.Envelope.Destroy()
		t.Fatalf("got an envelope, want denial %s", code)
	}
	if result.Denial.Code != code {
		t.Fatalf("denial = %s (%s), want %s", result.Denial.Code, result.Denial.reason, code)
	}
	return result.Denial
}

// Acceptance: a governed run with an active binding, a grant and a static
// secret gets a STATIC envelope with not_after set, holding only the
// catalogue's keys, and the decision and the redemption are both audited.
func TestStaticSecretRedemption(t *testing.T) {
	w := newWorld().withMandate(mandate.KindDelegatedBackground)
	result := redeem(t, w.broker(t), w.request(t))
	envelope := result.Envelope
	if envelope == nil {
		t.Fatalf("denied: %+v", result.Denial)
	}
	defer envelope.Destroy()
	if envelope.Kind != KindStatic || !envelope.NotAfter.Equal(tNow.Add(MaxEnvelopeTTL)) ||
		envelope.TargetIdentity != "https://a.example.com" || envelope.DecisionID == "" || envelope.RedemptionID == "" {
		t.Fatalf("envelope: kind=%s not_after=%s target=%s", envelope.Kind, envelope.NotAfter, envelope.TargetIdentity)
	}
	if len(envelope.Material) != 1 || string(envelope.Material["api_token"]) != tSecretValue {
		t.Fatalf("material keys: %d", len(envelope.Material))
	}
	if len(envelope.Obligations) != 1 || envelope.Obligations[0] != credentialgrant.ObligationAuditSync {
		t.Fatalf("obligations: %v", envelope.Obligations)
	}
	if w.secrets.ver != 1 || w.secrets.ref.Path != "jira_token" || w.secrets.last.DecisionID != envelope.DecisionID ||
		w.secrets.last.WorkloadScope != WorkloadScope {
		t.Fatalf("secret read: version=%d ref=%+v ctx=%+v", w.secrets.ver, w.secrets.ref, w.secrets.last)
	}
	if len(w.store.events) != 2 || w.store.events[0].Type != securityaudit.TypeDecision ||
		w.store.events[0].Effect != securityaudit.EffectAllow || w.store.events[1].Type != securityaudit.TypeCredentialRedeem {
		t.Fatalf("audit events: %+v", w.store.events)
	}
	if len(w.store.redemptions) != 1 || w.store.redemptions[0].Kind != KindStatic || w.store.redemptions[0].DecisionID != envelope.DecisionID {
		t.Fatalf("redemptions: %+v", w.store.redemptions)
	}
	material := envelope.Material["api_token"]
	envelope.Destroy()
	if !bytes.Equal(material, make([]byte, len(material))) {
		t.Fatal("Destroy left material bytes")
	}
}

// Hand-off (a): the first redemption opens the run's agent session from the
// stored context; a later one reuses it.
func TestTheAgentSessionIsOpenedOnceAndReused(t *testing.T) {
	w := newWorld().withMandate(mandate.KindDelegatedBackground)
	b := w.broker(t)
	first := redeem(t, b, w.request(t))
	first.Envelope.Destroy()
	second := w.request(t)
	second.Invocation.InvocationID = tInvocation2
	redeem(t, b, second).Envelope.Destroy()
	if len(w.sessions.opened) != 1 {
		t.Fatalf("sessions opened = %d, want 1", len(w.sessions.opened))
	}
	opened := w.sessions.opened[0]
	if opened.ExecutionID != tExecution || opened.Generation != int64(tGeneration) || opened.AgentBindingID != tBinding ||
		opened.AgentBindingEpoch != 2 || opened.MandateID != tMandate || opened.Profile != agentidentity.ProfileAutonomousAttributed ||
		opened.PolicyRevisionID != tRevision || len(opened.Ceiling.Audiences) != 1 {
		t.Fatalf("session admission: %+v", opened)
	}
	if evidence, ok := opened.Evidence.(agentidentity.AssertedUserEvidence); !ok || evidence.Subject != "user:7" || evidence.MandateID != tMandate {
		t.Fatalf("delegated background evidence: %#v", opened.Evidence)
	}
	if !opened.ExpiresAt.Equal(tNow.Add(time.Hour)) {
		t.Fatalf("session expiry %s, want the mandate's", opened.ExpiresAt)
	}
}

// A session that cannot be opened denies before any secret is read.
func TestSessionOpenFailureDeniesWithoutMaterial(t *testing.T) {
	w := newWorld().withMandate(mandate.KindDelegatedBackground)
	w.sessions.err = &agentidentity.Denial{Reason: agentidentity.ReasonProviderNotConfigured}
	denial := requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeDenied)
	if denial.reason != "agent_session.provider_not_configured" || denial.DecisionID == "" || w.secrets.readCount() != 0 {
		t.Fatalf("denial %+v, reads %d", denial, w.secrets.readCount())
	}
}

// Acceptance: binding failures read no secret. The claim binding itself is
// the content server's (see the storage tests); here a legacy pin and a
// context digest mismatch deny before anything else runs.
func TestNotGovernedAndContextMismatchReadNothing(t *testing.T) {
	t.Run("legacy pin", func(t *testing.T) {
		w := newWorld().withMandate(mandate.KindDelegatedBackground)
		w.pins.pin = profile.Pin{ExecutionID: tExecution, Profile: profile.ProfileLegacy}
		requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeNotGoverned)
		if w.secrets.readCount() != 0 || len(w.store.invocations) != 0 || len(w.store.events) != 0 {
			t.Fatal("a legacy run reached the invocation table or the secret store")
		}
	})
	t.Run("digest mismatch", func(t *testing.T) {
		w := newWorld().withMandate(mandate.KindDelegatedBackground)
		req := w.request(t)
		req.SecurityContextDigest[0] ^= 0xff
		requireDenial(t, redeem(t, w.broker(t), req), CodeContextMismatch)
		if w.secrets.readCount() != 0 || len(w.store.invocations) != 0 {
			t.Fatal("a context mismatch reached the invocation table or the secret store")
		}
	})
	t.Run("no stored context", func(t *testing.T) {
		w := newWorld().withMandate(mandate.KindDelegatedBackground)
		req := w.request(t)
		req.Run.ContextBytes = nil
		requireDenial(t, redeem(t, w.broker(t), req), CodeContextMismatch)
	})
	t.Run("tampered stored bytes", func(t *testing.T) {
		w := newWorld().withMandate(mandate.KindDelegatedBackground)
		req := w.request(t)
		req.Run.ContextBytes = append([]byte(nil), req.Run.ContextBytes...)
		req.Run.ContextBytes[len(req.Run.ContextBytes)-1] ^= 0x01
		requireDenial(t, redeem(t, w.broker(t), req), CodeContextMismatch)
	})
	t.Run("pin of another revision", func(t *testing.T) {
		w := newWorld().withMandate(mandate.KindDelegatedBackground)
		w.pins.pin.PolicyRevisionID = "2026-10-r2"
		requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeContextMismatch)
	})
}

// Acceptance: a replay of the invocation id with another operation is
// INVOCATION_MISMATCH; an identical replay is re-evaluated.
func TestInvocationReplay(t *testing.T) {
	w := newWorld().withMandate(mandate.KindDelegatedBackground)
	w.catalog.ops["jira.issue.write"] = "write"
	b := w.broker(t)
	redeem(t, b, w.request(t)).Envelope.Destroy()
	again := redeem(t, b, w.request(t))
	if again.Envelope == nil {
		t.Fatalf("identical replay denied: %+v", again.Denial)
	}
	again.Envelope.Destroy()
	changed := w.request(t)
	changed.Invocation.OperationID = "jira.issue.write"
	requireDenial(t, redeem(t, b, changed), CodeInvocationMismatch)
	if w.secrets.readCount() != 2 {
		t.Fatalf("reads = %d, want 2 (the mismatch reads nothing)", w.secrets.readCount())
	}
	decisions := 0
	for _, event := range w.store.events {
		if event.Type == securityaudit.TypeDecision {
			decisions++
		}
	}
	if decisions != 2 {
		t.Fatalf("decisions = %d, want 2", decisions)
	}
}

// Acceptance: a grant for target A does not release a credential bound to
// target B; the decision is denied and audited, and nothing is read.
func TestGrantForTargetADeniesTargetB(t *testing.T) {
	w := newWorld().withMandate(mandate.KindDelegatedBackground)
	req := w.request(t)
	req.Invocation.CredentialReferenceID = tReferenceB
	denial := requireDenial(t, redeem(t, w.broker(t), req), CodeDenied)
	if denial.DecisionID == "" || denial.reason != "grant_missing" || w.secrets.readCount() != 0 {
		t.Fatalf("denial %+v reads %d", denial, w.secrets.readCount())
	}
	if len(w.store.events) != 1 || w.store.events[0].Effect != securityaudit.EffectDeny {
		t.Fatalf("deny not audited: %+v", w.store.events)
	}
}

// Acceptance: an audit append failure denies and releases nothing.
func TestAuditAppendFailureDenies(t *testing.T) {
	t.Run("decision", func(t *testing.T) {
		w := newWorld().withMandate(mandate.KindDelegatedBackground)
		w.store.failAudit = func(e securityaudit.SecurityEvent) bool { return e.Type == securityaudit.TypeDecision }
		requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeDenied)
		if w.secrets.readCount() != 0 || len(w.store.invocations) != 0 {
			t.Fatal("a failed decision append read a secret or kept the invocation")
		}
	})
	t.Run("redemption", func(t *testing.T) {
		w := newWorld().withMandate(mandate.KindDelegatedBackground)
		w.store.failAudit = func(e securityaudit.SecurityEvent) bool { return e.Type == securityaudit.TypeCredentialRedeem }
		denial := requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeDenied)
		if denial.reason != "audit_failed" || len(w.store.redemptions) != 0 {
			t.Fatalf("denial %+v redemptions %d", denial, len(w.store.redemptions))
		}
	})
}

// Owner decision (hand-off c): the subject of an autonomous run is the
// mandate's approver, at decision time; the evidence is attributed.
func TestAutonomousRunSubjectIsTheMandateApprover(t *testing.T) {
	autonomous := func() *world {
		w := newWorld().withMandate(mandate.KindAutonomous)
		w.context.SubjectPrincipalRef, w.context.Caller = "", securitycontext.Caller{}
		w.context.ActorChain = nil
		return w
	}
	t.Run("approver holds use", func(t *testing.T) {
		w := autonomous()
		delete(w.resolver.perms, tUser)
		result := redeem(t, w.broker(t), w.request(t))
		if result.Envelope == nil {
			t.Fatalf("autonomous run denied: %+v", result.Denial)
		}
		result.Envelope.Destroy()
		decision := w.store.events[0]
		if decision.PrincipalRef != "user:9" || decision.ActorFidelity != securityaudit.FidelityAttributed {
			t.Fatalf("decision subject %q fidelity %q", decision.PrincipalRef, decision.ActorFidelity)
		}
		if opened := w.sessions.opened[0]; opened.Evidence != nil || opened.Profile != agentidentity.ProfileAutonomousAttributed {
			t.Fatalf("autonomous session: %+v", opened)
		}
	})
	t.Run("approver lost use", func(t *testing.T) {
		w := autonomous()
		w.resolver.perms[tApprover] = nil
		denial := requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeDenied)
		if denial.reason != "permission_missing" {
			t.Fatalf("reason %q", denial.reason)
		}
	})
	t.Run("unapproved mandate has no subject", func(t *testing.T) {
		w := autonomous()
		m := w.mandates.mandates[tMandate]
		m.ApprovedBy = ""
		w.mandates.mandates[tMandate] = m
		requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeDenied)
	})
}

// Acceptance: the oauth_delegated path exchanges on behalf of the user and
// returns OAUTH_ACCESS; interaction_required returns AUTHORIZATION_REQUIRED
// with an opaque reference.
func TestOAuthDelegatedRedemption(t *testing.T) {
	delegated := func() *world {
		w := newWorld()
		w.context.MandateID, w.context.MandateEpoch = "", 0
		w.context.DelegationID, w.context.DelegationEpoch = tDelegation, 1
		ref := w.references.refs[tReference]
		ref.Class = credentialgrant.ClassOAuthDelegated
		w.references.refs[tReference] = ref
		return w
	}
	t.Run("access token", func(t *testing.T) {
		w := delegated()
		result := redeem(t, w.broker(t), w.request(t))
		if result.Envelope == nil {
			t.Fatalf("denied: %+v", result.Denial)
		}
		defer result.Envelope.Destroy()
		if result.Envelope.Kind != KindOAuthAccess || string(result.Envelope.Material["access_token"]) != tOAuthValue ||
			!result.Envelope.NotAfter.Equal(tNow.Add(2*time.Minute)) {
			t.Fatalf("envelope kind=%s not_after=%s", result.Envelope.Kind, result.Envelope.NotAfter)
		}
		if w.exchanger.last.DelegationID != tDelegation || len(w.exchanger.last.Scopes) != 1 ||
			w.exchanger.last.Scopes[0] != "api://jira-a/.default" || w.secrets.readCount() != 0 {
			t.Fatalf("exchange: %+v", w.exchanger.last)
		}
		if opened := w.sessions.opened[0]; opened.Profile != agentidentity.ProfileDelegatedVerified || opened.DelegationID != tDelegation {
			t.Fatalf("delegated session: %+v", opened)
		}
	})
	t.Run("interaction required", func(t *testing.T) {
		w := delegated()
		w.exchanger.err = obo.ErrInteractionRequired
		denial := requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeAuthorizationRequired)
		if denial.AuthorizationReference != "car_opaque-reference" || len(w.custody.requirements) != 1 {
			t.Fatalf("denial %+v requirements %d", denial, len(w.custody.requirements))
		}
		if r := w.custody.requirements[0]; r.ExecutionGeneration != "3" || r.ActorUserID != tUser || r.DelegationID != tDelegation {
			t.Fatalf("requirement: %+v", r)
		}
	})
	t.Run("claims challenge", func(t *testing.T) {
		w := delegated()
		w.exchanger.err = &obo.ErrClaimsChallenge{Claims: `{"access_token":{"acrs":{"essential":true}}}`}
		requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeAuthorizationRequired)
		if w.custody.requirements[0].Claims == "" {
			t.Fatal("the claims challenge was not recorded")
		}
	})
	t.Run("token endpoint unavailable", func(t *testing.T) {
		w := delegated()
		w.exchanger.err = obo.ErrUnavailable
		requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeBackendUnavailable)
	})
	t.Run("no exchanger composed", func(t *testing.T) {
		w := delegated()
		b := w.broker(t, func(c *Config) { c.Exchanger, c.Custody = nil, nil })
		requireDenial(t, redeem(t, b, w.request(t)), CodeBackendUnavailable)
	})
}

func TestClassesWithoutAnAdapterAreNotSupported(t *testing.T) {
	t.Run("dynamic lease", func(t *testing.T) {
		w := newWorld().withMandate(mandate.KindDelegatedBackground)
		ref := w.references.refs[tReference]
		ref.Class = credentialgrant.ClassDynamicLease
		w.references.refs[tReference] = ref
		requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeClassNotSupported)
	})
	t.Run("no material keys", func(t *testing.T) {
		w := newWorld().withMandate(mandate.KindDelegatedBackground)
		delete(w.catalog.keys, credentialgrant.ClassStaticSecret)
		requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeClassNotSupported)
		if w.secrets.readCount() != 0 {
			t.Fatal("a class with no keys read a secret")
		}
	})
}

// The empty catalogue (the composition's default) denies every use.
func TestEmptyCatalogDeniesEverything(t *testing.T) {
	w := newWorld().withMandate(mandate.KindDelegatedBackground)
	b := w.broker(t, func(c *Config) { c.Catalog = EmptyCatalog{} })
	requireDenial(t, redeem(t, b, w.request(t)), CodeClassNotSupported)
	if w.secrets.readCount() != 0 {
		t.Fatal("the empty catalogue read a secret")
	}
}

func TestBackendFailuresAreBackendUnavailable(t *testing.T) {
	w := newWorld().withMandate(mandate.KindDelegatedBackground)
	w.secrets.err = secretstore.Errorf(secretstore.ErrUnavailable, secretstore.OpRead, "down")
	requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeBackendUnavailable)
	w = newWorld().withMandate(mandate.KindDelegatedBackground)
	ref := w.references.refs[tReference]
	ref.Backend = secretstore.BackendVault
	w.references.refs[tReference] = ref
	requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeBackendUnavailable)
}

func TestMalformedInvocationsAreInvalid(t *testing.T) {
	w := newWorld().withMandate(mandate.KindDelegatedBackground)
	b := w.broker(t)
	for name, mutate := range map[string]func(*BoundToolInvocation){
		"no invocation id":    func(i *BoundToolInvocation) { i.InvocationID = "" },
		"upper-case uuid":     func(i *BoundToolInvocation) { i.InvocationID = strings.ToUpper(tInvocation) },
		"bad operation":       func(i *BoundToolInvocation) { i.OperationID = "Jira.Read" },
		"no toolkit":          func(i *BoundToolInvocation) { i.ToolkitID = 0 },
		"no arguments digest": func(i *BoundToolInvocation) { i.ArgumentsDigest = [32]byte{} },
		"bad reference":       func(i *BoundToolInvocation) { i.CredentialReferenceID = "x" },
		"control character":   func(i *BoundToolInvocation) { i.ToolCallID = "a\nb" },
	} {
		req := w.request(t)
		mutate(&req.Invocation)
		if _, err := b.Redeem(context.Background(), req); err != ErrInvalidRequest {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// Acceptance: logs contain no material, on the permit path and the deny
// paths alike.
func TestLogsCarryNoMaterial(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	previous := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(previous)

	w := newWorld().withMandate(mandate.KindDelegatedBackground)
	b := w.broker(t, func(c *Config) { c.Logger = logger })
	result := redeem(t, b, w.request(t))
	logger.Info("envelope", "value", result.Envelope)
	result.Envelope.Destroy()
	changed := w.request(t)
	changed.Invocation.ToolCallID = "call-2"
	redeem(t, b, changed)

	o := newWorld()
	o.context.MandateID, o.context.MandateEpoch = "", 0
	o.context.DelegationID, o.context.DelegationEpoch = tDelegation, 1
	ref := o.references.refs[tReference]
	ref.Class = credentialgrant.ClassOAuthDelegated
	o.references.refs[tReference] = ref
	ob := o.broker(t, func(c *Config) { c.Logger = logger })
	oauth := redeem(t, ob, o.request(t))
	oauth.Envelope.Destroy()
	o.exchanger.err = obo.ErrInteractionRequired
	second := o.request(t)
	second.Invocation.InvocationID = tInvocation2
	redeem(t, ob, second)

	for _, secret := range []string{tSecretValue, tOAuthValue, "assertion-never-logged", "drop-me", "ags1_"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs carry %q:\n%s", secret, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "invocation_mismatch") {
		t.Fatalf("the denial was not logged by code:\n%s", logs.String())
	}
}

func TestShapeMaterial(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		keys []string
		want map[string]string
	}{
		{"json object keeps named keys", `{"username":"u","password":"p","x":"y"}`, []string{"username", "password"}, map[string]string{"username": "u", "password": "p"}},
		{"raw value is the one key", `plain-token`, []string{"api_token"}, map[string]string{"api_token": "plain-token"}},
		{"json without the one key is raw", `{"other":"v"}`, []string{"api_token"}, map[string]string{"api_token": `{"other":"v"}`}},
		{"json missing a key of two", `{"username":"u"}`, []string{"username", "password"}, nil},
		{"raw value for two keys", `plain`, []string{"username", "password"}, nil},
		{"empty member", `{"username":"","password":"p"}`, []string{"username", "password"}, nil},
	}
	for _, c := range cases {
		got, ok := shapeMaterial([]byte(c.raw), c.keys)
		if (c.want == nil) == ok {
			t.Errorf("%s: ok=%v", c.name, ok)
			continue
		}
		for key, value := range c.want {
			if string(got[key]) != value {
				t.Errorf("%s: %s=%q", c.name, key, got[key])
			}
		}
		if ok && len(got) != len(c.want) {
			t.Errorf("%s: %d keys", c.name, len(got))
		}
	}
}
