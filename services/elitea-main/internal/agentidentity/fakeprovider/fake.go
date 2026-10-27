// Package fakeprovider is an in-memory agentidentity.Provider for tests. It
// records every call, so a test can prove a path never reached the adapter.
// Production code never imports it.
package fakeprovider

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
)

// Provider is the fake. Set the fields before use; every method is safe for
// concurrent calls.
type Provider struct {
	ProviderName string
	Caps         agentidentity.Capabilities
	// Kind is the ref kind Provision answers ("fake.agent" when empty).
	Kind string
	// OBOErr, when set, is what AssumeOnBehalfOf answers.
	OBOErr error
	// ResolveErr, when set, is what Resolve answers.
	ResolveErr error
	// AssumeErr, when set, is what AssumeApp answers.
	AssumeErr error
	// Generation is the generation Resolve answers.
	Generation int64

	mu    sync.Mutex
	calls []string
}

// New builds a fake that supports app-only, verified OBO, and nothing else.
func New(name string) *Provider {
	return &Provider{ProviderName: name, Caps: agentidentity.Capabilities{
		AgentObject: true, AppOnly: true, VerifiesUser: true,
		DelegationFidelity: agentidentity.FidelityNativeAct,
		NarrowingKinds:     []agentidentity.NarrowingKind{agentidentity.NarrowAudience, agentidentity.NarrowScope},
		MaxTTL:             time.Hour, RevocationGranularity: agentidentity.RevokeIdentity,
	}}
}

func (p *Provider) record(call string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call)
}

// Calls returns the methods called so far, in order.
func (p *Provider) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

// Reset forgets the recorded calls.
func (p *Provider) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = nil
}

func (p *Provider) Name() string                             { return p.ProviderName }
func (p *Provider) Capabilities() agentidentity.Capabilities { return p.Caps }

func (p *Provider) Provision(_ context.Context, spec agentidentity.ProvisionSpec) (agentidentity.ProvisionResult, error) {
	p.record("Provision")
	kind := p.Kind
	if kind == "" {
		kind = "fake.agent"
	}
	// Idempotent on the record id: the object id is derived from it.
	return agentidentity.ProvisionResult{Ref: identity.ExternalRef{
		Provider: p.ProviderName, ProviderTenant: spec.ProviderTenant, Kind: kind,
		ID: "obj-" + spec.ExternalAgentIdentityID, Generation: p.Generation,
	}}, nil
}

func (p *Provider) Activate(context.Context, identity.ExternalRef) error {
	p.record("Activate")
	return nil
}

func (p *Provider) Resolve(_ context.Context, ref identity.ExternalRef) (agentidentity.Resolved, error) {
	p.record("Resolve")
	if p.ResolveErr != nil {
		return agentidentity.Resolved{}, p.ResolveErr
	}
	out := ref
	out.Generation = p.Generation
	return agentidentity.Resolved{Ref: out, Enabled: true}, nil
}

// Token is the canary every fake credential carries.
const Token = "fake-token-canary-1151"

func (p *Provider) AssumeApp(_ context.Context, spec agentidentity.AssumeSpec) (agentidentity.SessionCredential, error) {
	p.record("AssumeApp")
	if p.AssumeErr != nil {
		return agentidentity.SessionCredential{}, p.AssumeErr
	}
	return agentidentity.SessionCredential{Token: Token, TokenType: "Bearer", ExpiresAt: time.Now().Add(spec.TTL),
		Actor: agentidentity.NormalizedActor{
			Current:  agentidentity.Principal{Kind: "agent", Ref: spec.Ref.ID},
			Fidelity: agentidentity.FidelityNone,
		}}, nil
}

func (p *Provider) AssumeOnBehalfOf(_ context.Context, spec agentidentity.OBOSpec) (agentidentity.SessionCredential, error) {
	p.record("AssumeOnBehalfOf")
	if p.OBOErr != nil {
		return agentidentity.SessionCredential{}, p.OBOErr
	}
	subject := agentidentity.Principal{Kind: "user"}
	switch e := spec.Evidence.(type) {
	case agentidentity.VerifiedUserEvidence:
		subject.Ref = e.ObjectID
	case agentidentity.AssertedUserEvidence:
		subject.Ref = e.Subject
	default:
		return agentidentity.SessionCredential{}, errors.New("fake provider: OBO without evidence")
	}
	agent := agentidentity.Principal{Kind: "agent", Ref: spec.Ref.ID}
	return agentidentity.SessionCredential{Token: Token, TokenType: "Bearer", ExpiresAt: time.Now().Add(spec.TTL),
		Actor: agentidentity.NormalizedActor{Subject: &subject, Current: agent, Chain: []agentidentity.Principal{agent},
			Fidelity: agentidentity.FidelityNativeAct}}, nil
}

func (p *Provider) Suspend(context.Context, identity.ExternalRef) error {
	p.record("Suspend")
	return nil
}

func (p *Provider) Resume(context.Context, identity.ExternalRef) error {
	p.record("Resume")
	return nil
}

func (p *Provider) Revoke(context.Context, agentidentity.RevokeSpec) error {
	p.record("Revoke")
	return nil
}

func (p *Provider) List(context.Context, agentidentity.ListSpec) (agentidentity.ListPage, error) {
	p.record("List")
	return agentidentity.ListPage{}, nil
}

var _ agentidentity.Provider = (*Provider)(nil)
