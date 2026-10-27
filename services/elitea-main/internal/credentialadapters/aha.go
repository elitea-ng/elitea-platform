package credentialadapters

import "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"

// Aha: an API key (static_secret) or Aha! OAuth2 (oauth_delegated). Aha!
// has no service accounts, so a static key always acts as a person. The
// target is the account base URL.
func Aha() AdapterDescriptor {
	return AdapterDescriptor{
		ProviderType:       "aha",
		AuthClasses:        []credentialgrant.CredentialClass{staticSecret, oauthDelegated},
		TargetKind:         TargetBaseURL,
		CanonicalizeTarget: CanonicalBaseURL,
		Operations: []OperationSpec{
			op("aha.product.read", OperationRead, "product", 1),
			op("aha.feature.read", OperationRead, "feature", 1),
			op("aha.feature.search", OperationRead, "product", 1),
			op("aha.feature.write", OperationWrite, "feature", 1),
		},
		Probe: probe("aha.product.read"),
		MaterialKeys: map[credentialgrant.CredentialClass][]string{
			staticSecret:   {"token"},
			oauthDelegated: {"access_token"},
		},
		Qualification: map[credentialgrant.CredentialClass]string{
			staticSecret:   QualificationFixture,
			oauthDelegated: QualificationFixture,
		},
		RateLimitHint: "Aha!: per-account request limits shared by every user of the account",
	}
}
