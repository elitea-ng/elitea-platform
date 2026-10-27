package agentidentity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/fakeprovider"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
)

const (
	tenantID    = int64(7)
	bindingA    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0a01"
	externalA   = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0e01"
	executionID = "exec-1"
	workerClaim = "spiffe://elitea.local/worker/w1#claim-1"
)

type brokerFixture struct {
	store    *memStore
	provider *fakeprovider.Provider
	broker   *agentidentity.Broker
}

func newBrokerFixture(t *testing.T, policy agentidentity.SessionPolicy, providers ...agentidentity.Provider) *brokerFixture {
	t.Helper()
	f := &brokerFixture{store: newMemStore(), provider: fakeprovider.New("fake")}
	if providers == nil {
		providers = []agentidentity.Provider{f.provider}
	}
	registry, err := agentidentity.NewRegistry(providers...)
	if err != nil {
		t.Fatal(err)
	}
	if policy == nil {
		policy = agentidentity.DenyAssertedUser{ModePolicy: allowAllRevision().Document}
	}
	f.store.identities[externalA] = identity.ExternalAgentIdentity{
		ID: externalA, TenantProjectID: tenantID, ApplicationID: 11, Mode: identity.IdentityModeDedicatedAgent,
		Ref:   identity.ExternalRef{Provider: "fake", ProviderTenant: "t1", Kind: "fake.agent", ID: "obj-a"},
		State: identity.ExternalIdentityActive, RevocationEpoch: 3,
	}
	f.store.bindings[bindingA] = identity.AgentIdentityBinding{
		ID: bindingA, TenantProjectID: tenantID, ApplicationID: 11, ApplicationVersionID: 13,
		Mode: identity.IdentityModeDedicatedAgent, ExternalAgentIdentityID: externalA,
		State: identity.BindingActive, RevocationEpoch: 2,
	}
	now := fixedNow
	f.broker, err = agentidentity.NewBroker(agentidentity.BrokerConfig{
		Registry: registry, Policy: policy, Bindings: f.store, Identities: f.store, Sessions: f.store,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func appOnlyAdmission() agentidentity.SessionAdmission {
	return agentidentity.SessionAdmission{
		TenantProjectID: tenantID, ExecutionID: executionID, Generation: 1,
		AgentBindingID: bindingA, AgentBindingEpoch: 2, Profile: agentidentity.ProfileAutonomousAttributed,
		MandateID: "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0d01",
		Ceiling:   agentidentity.Narrowing{Audiences: []string{"api://crm"}, Scopes: []string{"read", "write"}},
		ExpiresAt: fixedNow.Add(30 * time.Minute), PolicyRevisionID: "rev-1",
	}
}

func claimFor(ref agentidentity.SessionRef) agentidentity.SessionClaim {
	return agentidentity.SessionClaim{Ref: ref, ExecutionID: executionID, Generation: 1, WorkerClaimRef: workerClaim}
}

func wantReason(t *testing.T, err error, want agentidentity.DenyReason) {
	t.Helper()
	if !errors.Is(err, agentidentity.ErrDenied) {
		t.Fatalf("err = %v, want a denial", err)
	}
	if got, _ := agentidentity.ReasonOf(err); got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
}

func assertNoTokenInEvents(t *testing.T, store *memStore) {
	t.Helper()
	raw, err := json.Marshal(store.Events())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(fakeprovider.Token)) || bytes.Contains(raw, []byte("ags1_")) {
		t.Fatalf("an audit event holds a token or a session ref: %s", raw)
	}
}

func TestOpenSessionThenCredentialAppOnly(t *testing.T) {
	f := newBrokerFixture(t, nil)
	ctx := context.Background()
	ref, err := f.broker.OpenSession(ctx, appOnlyAdmission())
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if !strings.HasPrefix(string(ref), "ags1_") || strings.Contains(fmt.Sprint(ref), "ags1_") {
		t.Fatalf("ref %q is not opaque, or prints itself", string(ref))
	}
	if calls := f.provider.Calls(); len(calls) != 1 || calls[0] != "Resolve" {
		t.Fatalf("OpenSession minted something: calls = %v", calls)
	}
	cred, err := f.broker.Credential(ctx, claimFor(ref), agentidentity.CredentialRequest{Audience: "api://crm", Scopes: []string{"read"}})
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if cred.Token != fakeprovider.Token {
		t.Fatalf("token = %q", cred.Token)
	}
	events := f.store.Events()
	if len(events) != 2 || events[0].Action != "agent_session.open" || events[1].Action != "agent_session.credential" ||
		events[0].ResourceID == "" || events[0].ResourceID != events[1].ResourceID ||
		events[1].AuthorizationEpoch != 2 || events[1].RevocationEpoch != 3 {
		t.Fatalf("events = %+v", events)
	}
	assertNoTokenInEvents(t, f.store)
}

func TestOpenSessionWithoutConfiguredProviderDeniesWithTypedReason(t *testing.T) {
	// An explicitly empty registry: the operator configured no adapter.
	f := newBrokerFixture(t, nil, []agentidentity.Provider{}...)
	_, err := f.broker.OpenSession(context.Background(), appOnlyAdmission())
	wantReason(t, err, agentidentity.ReasonProviderNotConfigured)
	if events := f.store.Events(); len(events) != 1 || events[0].Effect != "deny" {
		t.Fatalf("a denied OpenSession left events %+v", events)
	}
}

// Acceptance: asserted evidence against a policy that requires verified
// evidence, or against an adapter or policy that does not accept it, is
// denied before any adapter call.
func TestAssertedEvidenceIsDeniedBeforeAnyAdapterCall(t *testing.T) {
	asserted := agentidentity.AssertedUserEvidence{Issuer: "elitea", Subject: "user:42", MandateID: "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0d01"}
	t.Run("delegated_verified never takes asserted evidence", func(t *testing.T) {
		f := newBrokerFixture(t, assertedAllowed{allowAllRevision().Document})
		f.provider.Caps.AcceptsAssertedUser = true
		in := appOnlyAdmission()
		in.Profile, in.Evidence = agentidentity.ProfileDelegatedVerified, asserted
		_, err := f.broker.OpenSession(context.Background(), in)
		wantReason(t, err, agentidentity.ReasonProfileEvidenceMismatch)
		if calls := f.provider.Calls(); len(calls) != 0 {
			t.Fatalf("adapter reached: %v", calls)
		}
	})
	t.Run("adapter does not accept asserted users", func(t *testing.T) {
		f := newBrokerFixture(t, assertedAllowed{allowAllRevision().Document})
		in := appOnlyAdmission()
		in.Evidence = asserted
		_, err := f.broker.OpenSession(context.Background(), in)
		wantReason(t, err, agentidentity.ReasonAssertedUserNotAccepted)
		if calls := f.provider.Calls(); len(calls) != 0 {
			t.Fatalf("adapter reached: %v", calls)
		}
	})
	t.Run("default policy does not accept asserted users", func(t *testing.T) {
		f := newBrokerFixture(t, nil)
		f.provider.Caps.AcceptsAssertedUser = true
		in := appOnlyAdmission()
		in.Evidence = asserted
		_, err := f.broker.OpenSession(context.Background(), in)
		wantReason(t, err, agentidentity.ReasonAssertedUserNotAccepted)
		if calls := f.provider.Calls(); len(calls) != 0 {
			t.Fatalf("adapter reached: %v", calls)
		}
	})
	t.Run("adapter and policy both accept", func(t *testing.T) {
		f := newBrokerFixture(t, assertedAllowed{allowAllRevision().Document})
		f.provider.Caps.AcceptsAssertedUser = true
		in := appOnlyAdmission()
		in.Evidence = asserted
		ref, err := f.broker.OpenSession(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		cred, err := f.broker.Credential(context.Background(), claimFor(ref), agentidentity.CredentialRequest{Audience: "api://crm"})
		if err != nil || cred.Actor.Subject == nil || cred.Actor.Subject.Ref != "user:42" {
			t.Fatalf("Credential = %+v, %v", cred.Actor, err)
		}
	})
}

func verifiedAdmission() agentidentity.SessionAdmission {
	in := appOnlyAdmission()
	in.Profile, in.MandateID = agentidentity.ProfileDelegatedVerified, ""
	in.DelegationID = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0c01"
	in.Evidence = agentidentity.VerifiedUserEvidence{Issuer: "https://login.example/t1/v2.0", TenantID: "t1",
		ObjectID: "oid-1", Audience: "api://blueprint", DelegationID: in.DelegationID}
	return in
}

// Acceptance: an OBO failure answers InteractionRequired or an error, never
// an app-only credential.
func TestOBOFailureNeverFallsBackToAppOnly(t *testing.T) {
	for name, oboErr := range map[string]error{
		"interaction required": &agentidentity.InteractionRequired{Reason: "consent_required"},
		"provider error":       errors.New("upstream 500"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newBrokerFixture(t, nil)
			f.provider.OBOErr = oboErr
			ref, err := f.broker.OpenSession(context.Background(), verifiedAdmission())
			if err != nil {
				t.Fatal(err)
			}
			cred, err := f.broker.Credential(context.Background(), claimFor(ref), agentidentity.CredentialRequest{Audience: "api://crm"})
			if err == nil || cred.Token != "" {
				t.Fatalf("Credential = %v, %v; want a denial and no token", cred, err)
			}
			var interaction *agentidentity.InteractionRequired
			if errors.As(oboErr, &interaction) {
				wantReason(t, err, agentidentity.ReasonInteractionRequired)
				if !errors.As(err, &interaction) {
					t.Fatal("the denial does not carry *InteractionRequired")
				}
			} else {
				wantReason(t, err, agentidentity.ReasonProviderFailed)
			}
			for _, call := range f.provider.Calls() {
				if call == "AssumeApp" {
					t.Fatalf("OBO failure fell back to AssumeApp: %v", f.provider.Calls())
				}
			}
		})
	}
}

// Acceptance: Credential with a ref from another execution, generation or
// worker claim is denied.
func TestCredentialRefusesAForeignClaim(t *testing.T) {
	f := newBrokerFixture(t, nil)
	ctx := context.Background()
	ref, err := f.broker.OpenSession(ctx, appOnlyAdmission())
	if err != nil {
		t.Fatal(err)
	}
	req := agentidentity.CredentialRequest{Audience: "api://crm"}
	otherExecution := claimFor(ref)
	otherExecution.ExecutionID = "exec-2"
	otherGeneration := claimFor(ref)
	otherGeneration.Generation = 2
	for name, claim := range map[string]agentidentity.SessionClaim{"execution": otherExecution, "generation": otherGeneration} {
		_, err := f.broker.Credential(ctx, claim, req)
		if got, _ := agentidentity.ReasonOf(err); got != agentidentity.ReasonSessionClaimMismatch {
			t.Errorf("other %s: %v", name, err)
		}
	}
	// The first good redemption binds the worker claim; another worker is
	// then refused.
	if _, err := f.broker.Credential(ctx, claimFor(ref), req); err != nil {
		t.Fatal(err)
	}
	otherWorker := claimFor(ref)
	otherWorker.WorkerClaimRef = "spiffe://elitea.local/worker/w2#claim-9"
	_, err = f.broker.Credential(ctx, otherWorker, req)
	wantReason(t, err, agentidentity.ReasonSessionClaimMismatch)

	_, err = f.broker.Credential(ctx, agentidentity.SessionClaim{Ref: "ags1_forged", ExecutionID: executionID, Generation: 1, WorkerClaimRef: workerClaim}, req)
	wantReason(t, err, agentidentity.ReasonInvalidRequest)
	forged, _, _ := strings.Cut(string(ref), "_")
	_, err = f.broker.Credential(ctx, claimFor(agentidentity.SessionRef(forged+"_"+strings.Repeat("A", 43))), req)
	wantReason(t, err, agentidentity.ReasonSessionNotFound)
}

// Acceptance: ErrIdentityReplaced (or a generation mismatch) moves the
// identity to replaced and denies.
func TestReplacedIdentityIsMarkedReplacedAndDenied(t *testing.T) {
	t.Run("generation mismatch at open", func(t *testing.T) {
		f := newBrokerFixture(t, nil)
		f.provider.Generation = 1 // the record holds 0
		_, err := f.broker.OpenSession(context.Background(), appOnlyAdmission())
		wantReason(t, err, agentidentity.ReasonExternalIdentityReplaced)
		if got := f.store.identities[externalA]; got.State != identity.ExternalIdentityReplaced || got.RevocationEpoch != 4 {
			t.Fatalf("identity = %s epoch %d", got.State, got.RevocationEpoch)
		}
	})
	t.Run("ErrIdentityReplaced at credential", func(t *testing.T) {
		f := newBrokerFixture(t, nil)
		ref, err := f.broker.OpenSession(context.Background(), appOnlyAdmission())
		if err != nil {
			t.Fatal(err)
		}
		f.provider.AssumeErr = fmt.Errorf("wrapped: %w", agentidentity.ErrIdentityReplaced)
		_, err = f.broker.Credential(context.Background(), claimFor(ref), agentidentity.CredentialRequest{Audience: "api://crm"})
		wantReason(t, err, agentidentity.ReasonExternalIdentityReplaced)
		if f.store.identities[externalA].State != identity.ExternalIdentityReplaced {
			t.Fatal("identity not marked replaced")
		}
		// A replaced identity denies every later call.
		f.provider.AssumeErr = nil
		_, err = f.broker.Credential(context.Background(), claimFor(ref), agentidentity.CredentialRequest{Audience: "api://crm"})
		wantReason(t, err, agentidentity.ReasonExternalIdentityNotActive)
	})
}

func TestCredentialRechecksEpochsAndCeiling(t *testing.T) {
	f := newBrokerFixture(t, nil)
	ctx := context.Background()
	ref, err := f.broker.OpenSession(ctx, appOnlyAdmission())
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.broker.Credential(ctx, claimFor(ref), agentidentity.CredentialRequest{Audience: "api://other"})
	wantReason(t, err, agentidentity.ReasonOutsideCeiling)
	_, err = f.broker.Credential(ctx, claimFor(ref), agentidentity.CredentialRequest{Audience: "api://crm", Scopes: []string{"admin"}})
	wantReason(t, err, agentidentity.ReasonOutsideCeiling)

	b := f.store.bindings[bindingA]
	b.RevocationEpoch++
	f.store.bindings[bindingA] = b
	_, err = f.broker.Credential(ctx, claimFor(ref), agentidentity.CredentialRequest{Audience: "api://crm"})
	wantReason(t, err, agentidentity.ReasonBindingEpochStale)

	b.RevocationEpoch--
	f.store.bindings[bindingA] = b
	e := f.store.identities[externalA]
	e.RevocationEpoch++
	f.store.identities[externalA] = e
	_, err = f.broker.Credential(ctx, claimFor(ref), agentidentity.CredentialRequest{Audience: "api://crm"})
	wantReason(t, err, agentidentity.ReasonExternalIdentityStale)
}

func TestOpenSessionRefusesAStaleOrUnusableBinding(t *testing.T) {
	f := newBrokerFixture(t, nil)
	in := appOnlyAdmission()
	in.AgentBindingEpoch = 1
	_, err := f.broker.OpenSession(context.Background(), in)
	wantReason(t, err, agentidentity.ReasonBindingEpochStale)

	e := f.store.identities[externalA]
	e.State = identity.ExternalIdentitySuspended
	f.store.identities[externalA] = e
	_, err = f.broker.OpenSession(context.Background(), appOnlyAdmission())
	wantReason(t, err, agentidentity.ReasonExternalIdentityNotActive)

	in = appOnlyAdmission()
	in.Profile = agentidentity.ProfileDelegatedVerified // no evidence
	_, err = f.broker.OpenSession(context.Background(), in)
	wantReason(t, err, agentidentity.ReasonProfileEvidenceMismatch)
	if calls := f.provider.Calls(); len(calls) != 0 {
		t.Fatalf("adapter reached: %v", calls)
	}
}

func TestSessionCredentialNeverFormatsItsToken(t *testing.T) {
	cred := agentidentity.SessionCredential{Token: fakeprovider.Token, TokenType: "Bearer"}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("x", "cred", cred)
	encoded, _ := json.Marshal(cred)
	for name, out := range map[string]string{
		"%v": fmt.Sprintf("%v", cred), "%+v": fmt.Sprintf("%+v", cred), "%#v": fmt.Sprintf("%#v", cred),
		"String": cred.String(), "json": string(encoded), "slog": logged.String(),
	} {
		if strings.Contains(out, fakeprovider.Token) {
			t.Errorf("%s prints the token: %s", name, out)
		}
	}
}

func TestRegistryRefusesBadOrDuplicateProviders(t *testing.T) {
	if _, err := agentidentity.NewRegistry(fakeprovider.New("fake"), fakeprovider.New("fake")); err == nil {
		t.Error("duplicate accepted")
	}
	if _, err := agentidentity.NewRegistry(fakeprovider.New("Bad Name")); err == nil {
		t.Error("invalid name accepted")
	}
	if _, err := agentidentity.NewRegistry(nil); err == nil {
		t.Error("nil provider accepted")
	}
	r, err := agentidentity.NewRegistry()
	if err != nil || len(r.Names()) != 0 {
		t.Fatalf("empty registry: %v %v", r.Names(), err)
	}
	if _, ok := r.Provider("fake"); ok {
		t.Error("empty registry answered a provider")
	}
}
