// Package synthetic is the sixth, synthetic provider adapter of issue
// #1120. It exists to prove the seam: a provider registers by passing its
// descriptor to credentialadapters.NewRegistry, built only from the
// package's public API, with no edit to the core packages.
//
// It is never registered in production. Tests compose it as
//
//	credentialadapters.NewRegistry(append(credentialadapters.Builtin(), synthetic.Descriptor())...)
package synthetic

import (
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// ProviderType is the synthetic adapter id.
const ProviderType = "synthetic"

// Descriptor is the synthetic provider: static_secret and oauth_delegated
// against a base URL (a test double such as deploy/mock-mcp). Both classes
// are unqualified.
func Descriptor() credentialadapters.AdapterDescriptor {
	return credentialadapters.AdapterDescriptor{
		ProviderType: ProviderType,
		AuthClasses: []credentialgrant.CredentialClass{
			credentialgrant.ClassStaticSecret, credentialgrant.ClassOAuthDelegated,
		},
		TargetKind:         credentialadapters.TargetBaseURL,
		CanonicalizeTarget: credentialadapters.CanonicalBaseURL,
		Operations: []credentialadapters.OperationSpec{
			{ID: "synthetic.widget.read", Class: credentialadapters.OperationRead, ResourceKind: "widget", CostHint: 1},
			{ID: "synthetic.widget.write", Class: credentialadapters.OperationWrite, ResourceKind: "widget", CostHint: 1},
		},
		Probe: &credentialadapters.ProbeSpec{Operation: "synthetic.widget.read"},
		MaterialKeys: map[credentialgrant.CredentialClass][]string{
			credentialgrant.ClassStaticSecret:   {"token"},
			credentialgrant.ClassOAuthDelegated: {"access_token"},
		},
		OAuthAuthority: credentialgrant.OAuthAuthorityProvider,
		Qualification: map[credentialgrant.CredentialClass]string{
			credentialgrant.ClassStaticSecret:   credentialadapters.QualificationUnqualified,
			credentialgrant.ClassOAuthDelegated: credentialadapters.QualificationUnqualified,
		},
	}
}
