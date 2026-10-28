package credentialbroker

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
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
	// Admission froze B (as if B's grant was later revoked): the grant gate,
	// not the frozen-reference check, is what denies.
	w.context.CredentialReferences[0].CredentialReferenceID = tReferenceB
	w.context.CredentialReferences[0].TargetBindingID = tTargetB
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
	t.Run("background mandate carrying the delegation", func(t *testing.T) {
		w := delegated().withMandate(mandate.KindDelegatedBackground)
		m := w.mandates.mandates[tMandate]
		delegation := tDelegation
		m.DelegationID = &delegation
		w.mandates.mandates[tMandate] = m
		w.context.MandateID, w.context.MandateEpoch = tMandate, 5
		result := redeem(t, w.broker(t), w.request(t))
		if result.Envelope == nil {
			t.Fatalf("denied: %+v", result.Denial)
		}
		defer result.Envelope.Destroy()
		if result.Envelope.Kind != KindOAuthAccess || w.exchanger.last.DelegationID != tDelegation {
			t.Fatalf("envelope kind=%s exchange=%+v", result.Envelope.Kind, w.exchanger.last)
		}
	})
	t.Run("background mandate without the delegation", func(t *testing.T) {
		w := delegated().withMandate(mandate.KindDelegatedBackground)
		w.context.MandateID, w.context.MandateEpoch = tMandate, 5
		requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeDenied)
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

// An invocation naming no credential reference takes the one usable
// grant's; none or more than one denies before any secret read.
func TestEmptyCredentialReferenceIsResolvedFromTheGrant(t *testing.T) {
	for name, c := range map[string]struct {
		resolvable []string
		permit     bool
	}{
		"one grant":  {[]string{tReference}, true},
		"no grant":   {nil, false},
		"two grants": {[]string{tReference, tReference}, false},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld().withMandate(mandate.KindDelegatedBackground)
			w.store.resolvable = c.resolvable
			// A toolkit admission froze at its digest alone (#1119).
			w.context.CredentialReferences[0].CredentialReferenceID = ""
			w.context.CredentialReferences[0].TargetBindingID = ""
			req := w.request(t)
			req.Invocation.CredentialReferenceID = ""
			result := redeem(t, w.broker(t), req)
			if !c.permit {
				requireDenial(t, result, CodeDenied)
				if w.secrets.readCount() != 0 {
					t.Fatal("an unresolved reference read a secret")
				}
				return
			}
			if result.Envelope == nil {
				t.Fatalf("denied: %+v", result.Denial)
			}
			result.Envelope.Destroy()
		})
	}
}

// Acceptance (#1119): a redemption names the reference admission froze for
// its toolkit. Another reference, a toolkit admission froze nothing for, or a
// reference named for a toolkit frozen at its digest alone denies before any
// decision or secret read; an empty reference takes the frozen one.
func TestRedemptionMustNameTheFrozenReference(t *testing.T) {
	for name, c := range map[string]struct {
		mutate func(*world, *RedeemRequest)
		permit bool
	}{
		"the frozen reference": {func(*world, *RedeemRequest) {}, true},
		"empty takes the frozen reference": {func(_ *world, r *RedeemRequest) {
			r.Invocation.CredentialReferenceID = ""
		}, true},
		"another reference": {func(_ *world, r *RedeemRequest) {
			r.Invocation.CredentialReferenceID = tReferenceB
		}, false},
		"a toolkit admission did not freeze": {func(w *world, r *RedeemRequest) {
			r.Invocation.ToolkitID = tToolkit + 1
		}, false},
		"a reference for a digest-only toolkit": {func(w *world, r *RedeemRequest) {
			w.context.CredentialReferences[0].CredentialReferenceID = ""
			w.context.CredentialReferences[0].TargetBindingID = ""
			w.store.resolvable = []string{tReference}
			*r = w.request(t)
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld().withMandate(mandate.KindDelegatedBackground)
			req := w.request(t)
			c.mutate(w, &req)
			result := redeem(t, w.broker(t), req)
			if !c.permit {
				denial := requireDenial(t, result, CodeDenied)
				if denial.reason != "credential_reference_not_frozen" || w.secrets.readCount() != 0 || len(w.store.events) != 0 {
					t.Fatalf("denial %+v reads %d events %d", denial, w.secrets.readCount(), len(w.store.events))
				}
				return
			}
			if result.Envelope == nil {
				t.Fatalf("denied: %+v", result.Denial)
			}
			result.Envelope.Destroy()
		})
	}
}

// Acceptance (#1119): the agent binding gate compares the binding with the
// version admission ran and the toolkit binding with the digest admission
// froze, not each record with itself, so drift after admission denies.
func TestBindingAndToolkitDriftAfterAdmissionDenies(t *testing.T) {
	for name, c := range map[string]struct {
		mutate func(*world)
		reason string
	}{
		"binding moved to another version": {func(w *world) { w.bindings.binding.ApplicationVersionID = 5 }, "binding_scope_mismatch"},
		"binding of another application":   {func(w *world) { w.bindings.binding.ApplicationID = 9 }, "binding_scope_mismatch"},
		"run of no application": {func(w *world) {
			w.context.ApplicationID, w.context.ApplicationVersionID = 0, 0
		}, "agent_missing"},
		"toolkit re-bound at another digest": {func(w *world) { w.toolkits.binding.ToolkitDigest = [32]byte{0x71} }, "toolkit_digest_mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld().withMandate(mandate.KindDelegatedBackground)
			c.mutate(w)
			denial := requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeDenied)
			if denial.reason != c.reason || w.secrets.readCount() != 0 {
				t.Fatalf("denial %+v reads %d, want %s", denial, w.secrets.readCount(), c.reason)
			}
		})
	}
}

func TestExchangeScopes(t *testing.T) {
	graph := "https://graph.microsoft.com"
	for name, c := range map[string]struct {
		audience, resource string
		scopes             []string
		want               []string
	}{
		"target audience, no adapter scopes": {"api://jira-a", "", nil, []string{"api://jira-a/.default"}},
		"adapter least privilege":            {"", graph, []string{"Sites.Selected"}, []string{graph + "/Sites.Selected"}},
		"target audience qualified":          {graph + "/", "", []string{"Sites.Selected"}, []string{graph + "/Sites.Selected"}},
		"absolute scope kept":                {"", graph, []string{"api://x/read"}, []string{"api://x/read"}},
		"no resource":                        {"", "", []string{"Sites.Selected"}, nil},
	} {
		got := exchangeScopes(credentialgrant.TargetBinding{Audience: c.audience}, c.resource, c.scopes)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func TestShapeMaterial(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		keys []string
		want map[string]string
	}{
		{"json object keeps named keys", `{"token":"t","username":"u","x":"y"}`, []string{"token", "username"}, map[string]string{"token": "t", "username": "u"}},
		{"raw value is the first key", `plain-token`, []string{"token"}, map[string]string{"token": "plain-token"}},
		{"raw value for two keys is the first", `pat`, []string{"token", "username"}, map[string]string{"token": "pat"}},
		{"optional key may be absent", `{"token":"pat"}`, []string{"token", "username"}, map[string]string{"token": "pat"}},
		{"json without the first key is refused, never raw", `{"qtest_api_token":"a","admin_password":"x"}`, []string{"token"}, nil},
		{"json missing the first key of two", `{"username":"u"}`, []string{"token", "username"}, nil},
		{"empty first member", `{"token":"","username":"u"}`, []string{"token", "username"}, nil},
		{"empty optional member is dropped", `{"token":"t","username":""}`, []string{"token", "username"}, map[string]string{"token": "t"}},
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
