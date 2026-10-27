package credentialadapters

import "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"

// Confluence: the same classes as Jira (API token or PAT; Atlassian 3LO
// with rotating refresh tokens). The target is the site base URL.
func Confluence() AdapterDescriptor {
	return AdapterDescriptor{
		ProviderType:       "confluence",
		AuthClasses:        []credentialgrant.CredentialClass{staticSecret, oauthDelegated},
		TargetKind:         TargetBaseURL,
		CanonicalizeTarget: CanonicalBaseURL,
		Operations: []OperationSpec{
			op("confluence.space.read", OperationRead, "space", 1),
			op("confluence.page.read", OperationRead, "page", 1),
			op("confluence.page.search", OperationRead, "space", 2),
			op("confluence.page.write", OperationWrite, "page", 1),
		},
		Probe: probe("confluence.space.read"),
		MaterialKeys: map[credentialgrant.CredentialClass][]string{
			staticSecret:   {"token", "username"},
			oauthDelegated: {"access_token"},
		},
		OAuthAuthority: credentialgrant.OAuthAuthorityProvider,
		Qualification: map[credentialgrant.CredentialClass]string{
			staticSecret:   QualificationFixture,
			oauthDelegated: QualificationFixture,
		},
		RateLimitHint: atlassianRateLimit,
	}
}
