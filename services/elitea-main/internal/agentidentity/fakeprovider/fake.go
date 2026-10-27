// Package fakeprovider is an in-memory agentidentity.Provider for tests. It
// records every call, so a test can prove a path never reached the adapter.
// Production code never imports it.
package fakeprovider

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
	// Generation is the generation Resolve answers for an object the fake
	// does not track, and the generation Provision gives a new object.
	Generation int64
	// ListErr, when set, is what List answers: a provider outage.
	ListErr error
	// ListOmitsGeneration makes List answer generation 0, as an IdP whose
	// list API does not report one; Resolve still answers it.
	ListOmitsGeneration bool
	// PageSize bounds a List page (2 when zero), so a test pages.
	PageSize int

	mu    sync.Mutex
	calls []string
	// objects is the mock IdP: every object Provision created or a test
	// added, keyed on the immutable id. An object it does not track is
	// answered as enabled, with Generation.
	objects map[string]*Object
	order   []string
	seq     int
}

// Object is one object of the mock IdP.
type Object struct {
	Ref               identity.ExternalRef
	Enabled           bool
	OwnerRef          string
	CredentialChanged bool
	// Deleted: Resolve answers ErrIdentityNotFound and List leaves it out.
	Deleted bool
	// Replaced: the object was recreated under the same name. Resolve of
	// the old id answers ErrIdentityReplaced; List leaves it out.
	Replaced bool
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
	ref := identity.ExternalRef{
		Provider: p.ProviderName, ProviderTenant: spec.ProviderTenant, Kind: kind,
		ID: "obj-" + spec.ExternalAgentIdentityID, Generation: p.Generation,
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.objects[ref.ID]; !ok {
		p.putLocked(&Object{Ref: ref, Enabled: true, OwnerRef: spec.OwnerRef})
	}
	return agentidentity.ProvisionResult{Ref: ref}, nil
}

func (p *Provider) putLocked(o *Object) {
	if p.objects == nil {
		p.objects = map[string]*Object{}
	}
	if _, ok := p.objects[o.Ref.ID]; !ok {
		p.order = append(p.order, o.Ref.ID)
	}
	p.objects[o.Ref.ID] = o
}

// Mutate changes the tracked object id under the fake's lock: the
// out-of-band IdP change a reconcile test simulates. It reports whether the
// object exists.
func (p *Provider) Mutate(id string, change func(*Object)) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.objects[id]
	if ok {
		change(o)
	}
	return ok
}

// AddObject adds an Elitea-tagged object to the mock IdP, for example one
// created outside Elitea (an orphan).
func (p *Provider) AddObject(o Object) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.putLocked(&o)
}

// Recreate deletes the object id and creates another under the same name:
// a new immutable id and the next generation. It answers the new ref.
func (p *Provider) Recreate(id string) (identity.ExternalRef, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old, ok := p.objects[id]
	if !ok {
		return identity.ExternalRef{}, false
	}
	old.Replaced = true
	p.seq++
	ref := old.Ref
	ref.ID = fmt.Sprintf("%s-r%d", id, p.seq)
	ref.Generation = old.Ref.Generation + 1
	p.putLocked(&Object{Ref: ref, Enabled: true, OwnerRef: old.OwnerRef})
	return ref, true
}

func (o *Object) resolved() agentidentity.Resolved {
	return agentidentity.Resolved{Ref: o.Ref, Enabled: o.Enabled, OwnerRef: o.OwnerRef, CredentialChanged: o.CredentialChanged}
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
	p.mu.Lock()
	o, tracked := p.objects[ref.ID]
	var found agentidentity.Resolved
	switch {
	case tracked && o.Replaced:
		p.mu.Unlock()
		return agentidentity.Resolved{}, agentidentity.ErrIdentityReplaced
	case tracked && o.Deleted:
		p.mu.Unlock()
		return agentidentity.Resolved{}, agentidentity.ErrIdentityNotFound
	case tracked:
		found = o.resolved()
	}
	p.mu.Unlock()
	if tracked {
		return found, nil
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

// List pages the live objects of spec.ProviderTenant (every tenant when it
// is empty) in creation order. The page token is the offset.
func (p *Provider) List(_ context.Context, spec agentidentity.ListSpec) (agentidentity.ListPage, error) {
	p.record("List")
	if p.ListErr != nil {
		return agentidentity.ListPage{}, p.ListErr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	live := make([]agentidentity.Resolved, 0, len(p.order))
	for _, id := range p.order {
		o := p.objects[id]
		if o.Deleted || o.Replaced || (spec.ProviderTenant != "" && o.Ref.ProviderTenant != spec.ProviderTenant) {
			continue
		}
		object := o.resolved()
		if p.ListOmitsGeneration {
			object.Ref.Generation = 0
		}
		live = append(live, object)
	}
	offset := 0
	if spec.PageToken != "" {
		n, err := strconv.Atoi(spec.PageToken)
		if err != nil || n < 0 || n > len(live) {
			return agentidentity.ListPage{}, errors.New("fake provider: bad page token")
		}
		offset = n
	}
	size := p.PageSize
	if size <= 0 {
		size = 2
	}
	end := min(offset+size, len(live))
	page := agentidentity.ListPage{Objects: live[offset:end]}
	if end < len(live) {
		page.NextPageToken = strconv.Itoa(end)
	}
	return page, nil
}

var _ agentidentity.Provider = (*Provider)(nil)
