package credentialadapters

import "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"

// graphAudience is the Microsoft Graph resource.
const graphAudience = "https://graph.microsoft.com"

// SharePoint: oauth_delegated only, a Microsoft Graph token from Entra
// on-behalf-of (#1117). Sites.Selected is preferred over tenant-wide site
// scopes, so a grant reaches only the sites an administrator selected. The
// only reference provider that accepts Entra tokens. The target is the
// tenant site URL.
func SharePoint() AdapterDescriptor {
	return AdapterDescriptor{
		ProviderType:       "sharepoint",
		AuthClasses:        []credentialgrant.CredentialClass{oauthDelegated},
		TargetKind:         TargetTenantSite,
		CanonicalizeTarget: CanonicalTenantSite,
		Operations: []OperationSpec{
			op("sharepoint.site.read", OperationRead, "site", 1),
			op("sharepoint.file.read", OperationRead, "file", 1),
			op("sharepoint.list.read", OperationRead, "list", 1),
			op("sharepoint.file.write", OperationWrite, "file", 1),
		},
		Probe: probe("sharepoint.site.read"),
		MaterialKeys: map[credentialgrant.CredentialClass][]string{
			oauthDelegated: {"access_token"},
		},
		OAuthAuthority: credentialgrant.OAuthAuthorityEntraOBO,
		Qualification: map[credentialgrant.CredentialClass]string{
			oauthDelegated: QualificationFixture,
		},
		Audience: graphAudience,
		Scopes: map[credentialgrant.CredentialClass][]string{
			oauthDelegated: {"Sites.Selected"},
		},
		RateLimitHint: "Microsoft Graph: per-app and per-tenant throttling with Retry-After",
	}
}
