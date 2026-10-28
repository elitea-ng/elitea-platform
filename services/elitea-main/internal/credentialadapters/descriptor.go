// Package credentialadapters holds the provider adapter descriptors of
// ADR-0039 (issue #1120): what each governed provider accepts (auth
// classes), what it is bound to (target kind and canonical form), which
// canonical operations it serves, which one probes a credential, which
// envelope keys the credential broker may release per class, and how far
// each class is qualified (gate D6).
//
// Descriptors are data. The Registry built from them is immutable and is
// assembled at the composition root; there is no plugin loading. A new
// provider is one more descriptor passed to NewRegistry, from any package
// (see the synthetic package), with no edit here.
//
// No type here holds a secret value, a token or a provider response.
package credentialadapters

import (
	"slices"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// TargetKind is the shape of the target a credential is bound to.
type TargetKind string

const (
	// TargetBaseURL is a site base URL: https scheme, host and base path.
	TargetBaseURL TargetKind = "base_url"
	// TargetTenantSite is a tenant site URL (for example a SharePoint site
	// collection). Same canonical form as TargetBaseURL.
	TargetTenantSite TargetKind = "tenant_site"
	// TargetResourceURI is an OAuth protected resource URI (RFC 8707), such
	// as a remote MCP server endpoint. Its canonical form is the resource's
	// own (internal/credentialadapters/mcp), not a trimmed base URL.
	TargetResourceURI TargetKind = "resource_uri"
)

// Operation classes. Registry.Class answers one of these.
const (
	OperationRead  = "read"
	OperationWrite = "write"
)

// OperationSpec is one canonical operation a provider serves.
type OperationSpec struct {
	// ID is `<provider>.<resource>.<verb>`; the provider segment equals the
	// descriptor's ProviderType.
	ID credentialgrant.OperationID
	// Class is OperationRead or OperationWrite.
	Class string
	// ResourceKind names what a resource scope of this operation refers to
	// (for example "project" or "issue").
	ResourceKind string
	// CostHint is a relative rate-limit cost (1 = one plain read); 0 means
	// no hint. Atlassian Cloud meters REST calls in points per window, so a
	// search costs more than a single read there.
	CostHint int
}

// ProbeSpec names the operation that checks a credential works without
// changing anything. It must be a read operation of the same provider.
type ProbeSpec struct {
	Operation credentialgrant.OperationID
}

// AdapterDescriptor describes one governed provider.
type AdapterDescriptor struct {
	// ProviderType is the adapter id stored on credential references, for
	// example "jira".
	ProviderType string
	// AuthClasses are the credential classes this provider accepts.
	AuthClasses []credentialgrant.CredentialClass
	TargetKind  TargetKind
	// CanonicalizeTarget turns an operator-supplied target into its
	// canonical identity (credentialgrant.TargetBinding.TargetIdentity).
	CanonicalizeTarget func(raw string) (string, error)
	Operations         []OperationSpec
	// Probe is nil when the provider has no probe.
	Probe *ProbeSpec
	// MaterialKeys are the envelope keys the broker may release for each
	// class. Every declared class lists at least one.
	MaterialKeys map[credentialgrant.CredentialClass][]string
	// Qualification states, for every declared class, how far it is proven:
	// QualificationUnqualified, QualificationFixture or Qualified(ref).
	Qualification map[credentialgrant.CredentialClass]string
	// Audience is the OAuth resource the delegated token is for, when the
	// provider needs one (SharePoint: Microsoft Graph).
	Audience string
	// OAuthAuthority is who issued the consent of the oauth_delegated
	// class: credentialgrant.OAuthAuthorityEntraOBO (SharePoint) or
	// credentialgrant.OAuthAuthorityProvider (Atlassian 3LO, Aha! OAuth2).
	// Required when the descriptor declares oauth_delegated; empty
	// otherwise.
	OAuthAuthority credentialgrant.OAuthAuthority
	// Scopes are the OAuth scopes requested per class, least privilege first.
	Scopes map[credentialgrant.CredentialClass][]string
	// RotationRequired lists the classes whose material is long lived and
	// needs a rotation policy before qualification (gate D5).
	RotationRequired []credentialgrant.CredentialClass
	// RateLimitHint is a short note on how the provider meters calls.
	RateLimitHint string
}

// clone deep-copies d so the registry's copy cannot be changed from outside.
func (d AdapterDescriptor) clone() AdapterDescriptor {
	out := d
	out.AuthClasses = slices.Clone(d.AuthClasses)
	out.Operations = slices.Clone(d.Operations)
	if d.Probe != nil {
		probe := *d.Probe
		out.Probe = &probe
	}
	out.MaterialKeys = cloneClassLists(d.MaterialKeys)
	out.Scopes = cloneClassLists(d.Scopes)
	if d.Qualification != nil {
		out.Qualification = make(map[credentialgrant.CredentialClass]string, len(d.Qualification))
		for class, value := range d.Qualification {
			out.Qualification[class] = value
		}
	}
	out.RotationRequired = slices.Clone(d.RotationRequired)
	return out
}

func cloneClassLists(in map[credentialgrant.CredentialClass][]string) map[credentialgrant.CredentialClass][]string {
	if in == nil {
		return nil
	}
	out := make(map[credentialgrant.CredentialClass][]string, len(in))
	for class, values := range in {
		out[class] = slices.Clone(values)
	}
	return out
}

// Accepts reports whether the descriptor declares class.
func (d AdapterDescriptor) Accepts(class credentialgrant.CredentialClass) bool {
	return slices.Contains(d.AuthClasses, class)
}
