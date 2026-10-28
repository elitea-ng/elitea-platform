package configurations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

const (
	governedTenant    = int64(7)
	governedBinding   = "0192f0aa-0000-7000-8000-000000000001"
	governedReference = "0192f0aa-0000-7000-8000-0000000000c1"
	governedTarget    = "0192f0aa-0000-7000-8000-0000000000d1"
	governedToolkit   = int64(21)
)

var (
	governedDigest = [32]byte{0x70}
	governedNow    = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
)

// governedReaders fakes the credential grant readers and records every
// tenant project they were asked about.
type governedReaders struct {
	references map[string]credentialgrant.CredentialReference
	toolkit    *credentialgrant.AgentToolkitBinding
	grant      *credentialgrant.CredentialUseGrant
	tenants    []int64
}

func (r *governedReaders) CredentialReference(_ context.Context, tenant int64, id string) (credentialgrant.CredentialReference, error) {
	r.tenants = append(r.tenants, tenant)
	ref, ok := r.references[id]
	if !ok || ref.TenantProjectID != tenant {
		return credentialgrant.CredentialReference{}, credentialgrant.ErrReferenceNotFound
	}
	return ref, nil
}

func (r *governedReaders) AgentToolkitBinding(_ context.Context, binding string, toolkit int64) (credentialgrant.AgentToolkitBinding, error) {
	if r.toolkit == nil || r.toolkit.AgentBindingID != binding || r.toolkit.ToolkitID != toolkit {
		return credentialgrant.AgentToolkitBinding{}, credentialgrant.ErrToolkitNotBound
	}
	return *r.toolkit, nil
}

func (r *governedReaders) GrantForKey(_ context.Context, key credentialgrant.GrantKey) (credentialgrant.CredentialUseGrant, error) {
	r.tenants = append(r.tenants, key.TenantProjectID)
	if r.grant == nil || r.grant.CredentialReferenceID != key.CredentialReferenceID || r.grant.TargetBindingID != key.TargetBindingID {
		return credentialgrant.CredentialUseGrant{}, credentialgrant.ErrGrantNotFound
	}
	return *r.grant, nil
}

func newGovernedReaders() *governedReaders {
	return &governedReaders{
		references: map[string]credentialgrant.CredentialReference{governedReference: {
			ID: governedReference, TenantProjectID: governedTenant, Class: credentialgrant.ClassStaticSecret,
			TargetBindingID: governedTarget, Generation: 3, State: credentialgrant.ReferenceActive,
		}},
		toolkit: &credentialgrant.AgentToolkitBinding{AgentBindingID: governedBinding, TenantProjectID: governedTenant,
			ToolkitID: governedToolkit, ToolkitDigest: governedDigest},
		grant: &credentialgrant.CredentialUseGrant{TenantProjectID: governedTenant, AgentBindingID: governedBinding,
			ToolkitID: governedToolkit, CredentialReferenceID: governedReference, TargetBindingID: governedTarget,
			Capabilities: []credentialgrant.Capability{credentialgrant.CapUse}, State: credentialgrant.GrantActive},
	}
}

func newGovernedResolverForTest(t *testing.T, readers *governedReaders) *GovernedReferenceResolver {
	t.Helper()
	resolver, err := NewGovernedReferenceResolver(readers, readers, readers, func() time.Time { return governedNow })
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func exactRef() ToolkitCredentialRef {
	return ToolkitCredentialRef{CredentialReferenceID: governedReference, Title: "jira-prod"}
}

// Acceptance: an exact reference with a bound toolkit and an active grant
// resolves to that reference, its target and its generation.
func TestGovernedResolverResolvesTheExactReference(t *testing.T) {
	got, err := newGovernedResolverForTest(t, newGovernedReaders()).Resolve(context.Background(),
		governedTenant, governedBinding, governedToolkit, governedDigest, exactRef())
	if err != nil {
		t.Fatal(err)
	}
	want := GovernedResolution{ToolkitID: governedToolkit, CredentialReferenceID: governedReference, TargetBindingID: governedTarget, Generation: 3}
	if got != want {
		t.Fatalf("resolution = %+v, want %+v", got, want)
	}
}

// Acceptance: every rule returns its typed error.
func TestGovernedResolverTypedErrors(t *testing.T) {
	expires := governedNow.Add(-time.Minute)
	for name, c := range map[string]struct {
		mutate func(*governedReaders, *ToolkitCredentialRef, *[32]byte)
		want   error
	}{
		"title only": {func(_ *governedReaders, r *ToolkitCredentialRef, _ *[32]byte) { r.CredentialReferenceID = "" }, ErrGovernedReferenceRequired},
		"private (D1 default)": {func(_ *governedReaders, r *ToolkitCredentialRef, _ *[32]byte) { r.Private = true },
			ErrPrivateCredentialNotGoverned},
		"private title only": {func(_ *governedReaders, r *ToolkitCredentialRef, _ *[32]byte) {
			r.Private, r.CredentialReferenceID = true, ""
		}, ErrPrivateCredentialNotGoverned},
		"malformed id": {func(_ *governedReaders, r *ToolkitCredentialRef, _ *[32]byte) { r.CredentialReferenceID = "jira-prod" },
			ErrGovernedReferenceInvalid},
		"another tenant's reference": {func(g *governedReaders, _ *ToolkitCredentialRef, _ *[32]byte) {
			ref := g.references[governedReference]
			ref.TenantProjectID = 8
			g.references[governedReference] = ref
		}, ErrGovernedReferenceNotFound},
		"retiring reference": {func(g *governedReaders, _ *ToolkitCredentialRef, _ *[32]byte) {
			ref := g.references[governedReference]
			ref.State = credentialgrant.ReferenceRetiring
			g.references[governedReference] = ref
		}, ErrGovernedReferenceNotActive},
		"no toolkit binding": {func(g *governedReaders, _ *ToolkitCredentialRef, _ *[32]byte) { g.toolkit = nil }, ErrGovernedToolkitNotBound},
		"toolkit binding of another tenant": {func(g *governedReaders, _ *ToolkitCredentialRef, _ *[32]byte) {
			g.toolkit.TenantProjectID = 8
		}, ErrGovernedToolkitNotBound},
		"digest mismatch": {func(_ *governedReaders, _ *ToolkitCredentialRef, d *[32]byte) { d[0] = 0x71 }, ErrGovernedToolkitDigestMismatch},
		"no grant":        {func(g *governedReaders, _ *ToolkitCredentialRef, _ *[32]byte) { g.grant = nil }, ErrGovernedGrantMissing},
		"suspended grant": {func(g *governedReaders, _ *ToolkitCredentialRef, _ *[32]byte) {
			g.grant.State = credentialgrant.GrantSuspended
		}, ErrGovernedGrantMissing},
		"expired grant": {func(g *governedReaders, _ *ToolkitCredentialRef, _ *[32]byte) { g.grant.ExpiresAt = &expires },
			ErrGovernedGrantMissing},
		"grant without use": {func(g *governedReaders, _ *ToolkitCredentialRef, _ *[32]byte) {
			g.grant.Capabilities = []credentialgrant.Capability{credentialgrant.CapProbe}
		}, ErrGovernedGrantMissing},
	} {
		t.Run(name, func(t *testing.T) {
			readers, ref, digest := newGovernedReaders(), exactRef(), governedDigest
			c.mutate(readers, &ref, &digest)
			_, err := newGovernedResolverForTest(t, readers).Resolve(context.Background(),
				governedTenant, governedBinding, governedToolkit, digest, ref)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if _, ok := GovernedResolutionCode(err); !ok {
				t.Fatalf("%v has no safe code", err)
			}
		})
	}
}

// Acceptance: a reference of another tenant is denied, and the resolver
// asks only about the run's tenant project. It has no configuration finder
// or project scope at all, so no personal- or public-project lookup can
// run; the legacy expansion, given the same reference, is refused under
// the governed mark before it reads either.
func TestGovernedResolverNeverLeavesTheRunsTenant(t *testing.T) {
	readers := newGovernedReaders()
	ref := readers.references[governedReference]
	ref.TenantProjectID = 1 // the public project
	readers.references[governedReference] = ref
	ctx := WithGovernedRun(context.Background())
	if _, err := newGovernedResolverForTest(t, readers).Resolve(ctx, governedTenant, governedBinding, governedToolkit,
		governedDigest, exactRef()); !errors.Is(err, ErrGovernedReferenceNotFound) {
		t.Fatalf("err = %v", err)
	}
	for _, tenant := range readers.tenants {
		if tenant != governedTenant {
			t.Fatalf("a read asked about tenant %d", tenant)
		}
	}

	scope := &currentExpansionScopeStub{publicID: 1, personalID: 99}
	finder := &currentExpansionFinderStub{}
	legacy, err := NewCurrentExpansionService(scope, finder, &currentExpansionUnsecreterStub{})
	if err != nil {
		t.Fatal(err)
	}
	userID := int32(5)
	_, err = legacy.Expand(ctx, CurrentExpansionRequest{
		Payload:          map[string]any{"elitea_title": "jira-prod", "private": true},
		CurrentProjectID: int32(governedTenant), UserID: &userID,
	})
	if !errors.Is(err, ErrLegacyResolverInGovernedRun) {
		t.Fatalf("legacy expansion under a governed run: %v", err)
	}
	if scope.publicCalls != 0 || scope.personalCalls != 0 || len(finder.calls) != 0 {
		t.Fatalf("lookups ran: public %d personal %d finder %v", scope.publicCalls, scope.personalCalls, finder.calls)
	}
}

// Acceptance: a reference whose title is also a public-project
// configuration's still resolves only by its id; the title is never read.
func TestGovernedResolverIgnoresTheTitle(t *testing.T) {
	finder := &currentExpansionFinderStub{rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
		{projectID: 1, title: "jira-prod", sharedOnly: true}: expansionConfiguration(1, "jira", "public"),
	}}
	readers := newGovernedReaders()
	got, err := newGovernedResolverForTest(t, readers).Resolve(context.Background(),
		governedTenant, governedBinding, governedToolkit, governedDigest, exactRef())
	if err != nil || got.CredentialReferenceID != governedReference {
		t.Fatalf("resolution %+v err=%v", got, err)
	}
	if len(finder.calls) != 0 {
		t.Fatalf("the title was looked up: %v", finder.calls)
	}
}

func TestGovernedResolverBindToolkit(t *testing.T) {
	readers := newGovernedReaders()
	resolver := newGovernedResolverForTest(t, readers)
	if bound, err := resolver.BindToolkit(context.Background(), governedTenant, governedBinding, governedToolkit, governedDigest); !bound || err != nil {
		t.Fatalf("bound toolkit: %v %v", bound, err)
	}
	if _, err := resolver.BindToolkit(context.Background(), governedTenant, governedBinding, governedToolkit, [32]byte{1}); !errors.Is(err, ErrGovernedToolkitDigestMismatch) {
		t.Fatalf("digest mismatch: %v", err)
	}
	if bound, err := resolver.BindToolkit(context.Background(), governedTenant, governedBinding, governedToolkit+1, governedDigest); bound || err != nil {
		t.Fatalf("unbound toolkit: %v %v", bound, err)
	}
}

func TestToolkitCredentialRefsReadsStoredReferences(t *testing.T) {
	refs := ToolkitCredentialRefs(map[string]any{
		"jira_configuration": map[string]any{"elitea_title": "jira-prod", "private": false, CredentialReferenceIDField: governedReference},
		"pgvector":           map[string]any{"elitea_title": "x"},
		"base_url":           "https://jira.example.test",
		"aha_configuration":  map[string]any{"elitea_title": "mine", "private": true},
	})
	want := []ToolkitCredentialRef{{Title: "mine", Private: true}, {CredentialReferenceID: governedReference, Title: "jira-prod"}}
	if len(refs) != len(want) || refs[0] != want[0] || refs[1] != want[1] {
		t.Fatalf("refs = %+v", refs)
	}
}

// The legacy expansion without the governed mark is unchanged.
func TestLegacyExpansionRunsWithoutTheGovernedMark(t *testing.T) {
	finder := &currentExpansionFinderStub{rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
		{projectID: 7, title: "jira-prod"}: expansionConfiguration(7, "jira", "project"),
	}}
	service := newCurrentExpansionTestService(t, finder, &currentExpansionUnsecreterStub{})
	result, err := service.Expand(context.Background(), CurrentExpansionRequest{
		Payload: map[string]any{"elitea_title": "jira-prod", "private": false}, CurrentProjectID: 7,
	})
	if err != nil || result["value"] != "project" {
		t.Fatalf("result %v err=%v", result, err)
	}
}
