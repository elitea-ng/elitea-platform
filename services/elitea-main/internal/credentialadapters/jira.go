package credentialadapters

import "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"

// atlassianRateLimit is shared by Jira and Confluence Cloud.
const atlassianRateLimit = "Atlassian Cloud: global point-based limits per window; searches cost more than single reads"

// Jira: an API token or PAT (static_secret, with the account user name for
// Cloud basic auth) or Atlassian OAuth 2.0 3LO (oauth_delegated; refresh
// tokens rotate on every use). The target is the site base URL.
func Jira() AdapterDescriptor {
	return AdapterDescriptor{
		ProviderType:       "jira",
		AuthClasses:        []credentialgrant.CredentialClass{staticSecret, oauthDelegated},
		TargetKind:         TargetBaseURL,
		CanonicalizeTarget: CanonicalBaseURL,
		Operations: []OperationSpec{
			op("jira.project.read", OperationRead, "project", 1),
			op("jira.issue.read", OperationRead, "issue", 1),
			op("jira.issue.search", OperationRead, "project", 2),
			op("jira.issue.write", OperationWrite, "issue", 1),
		},
		Probe: probe("jira.project.read"),
		MaterialKeys: map[credentialgrant.CredentialClass][]string{
			staticSecret:   {"token", "username"},
			// api_base: the Atlassian 3LO gateway base (#1464).
			oauthDelegated: {"access_token", "api_base"},
		},
		OAuthAuthority: credentialgrant.OAuthAuthorityProvider,
		Qualification: map[credentialgrant.CredentialClass]string{
			staticSecret:   QualificationFixture,
			oauthDelegated: QualificationFixture,
		},
		RateLimitHint: atlassianRateLimit,
	}
}
