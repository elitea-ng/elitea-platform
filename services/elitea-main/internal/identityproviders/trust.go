package identityproviders

import (
	"slices"
	"strings"
)

// TrustChanges names the trust fields a save from before to after changes, in
// the wire spelling, and nil when it changes none (issue 2558, ADR-0059
// R3B-1).
//
// A trust field decides WHO an assertion says a person is, or whether it is
// believed at all. Whoever can change one can point the provider at an
// identity provider they control and sign in as any account, a super_admin's
// included. So a change to any of them is a super_admin decision, and the
// admin write path refuses it to everyone else (403).
//
// A provider that does not exist yet (before.Key == "") is all trust: the
// answer is ["created"].
//
// The fields, and the direction that counts where only one direction widens
// trust:
//
//   - OIDC: `issuer` (also the discovery and metadata address), `client_id`,
//     `entra`, an ADDED `entra_tenants` entry, `adopt_scim_users` turned on,
//     and `require_email_verified` turned off.
//   - SAML: `idp_entity_id`, `idp_sso_url` (where people type their
//     password), an ADDED `idp_certificates` entry, `email_attribute`, and
//     `adopt_scim_users` turned on.
//
// Removing a certificate or a tenant, or tightening a flag, only narrows
// trust, and stays open to an `admin`. Display, scopes, login domains, the
// redirect, the service-provider side and the client secret are not trust
// fields.
func TrustChanges(before, after Provider) []string {
	if before.Key == "" {
		return []string{"created"}
	}
	var changed []string
	switch {
	case after.OIDC != nil:
		changed = oidcTrustChanges(before.OIDC, after.OIDC)
	case after.SAML != nil:
		changed = samlTrustChanges(before.SAML, after.SAML)
	}
	return changed
}

func oidcTrustChanges(before, after *OIDCDocument) []string {
	if before == nil {
		before = &OIDCDocument{}
	}
	var changed []string
	if strings.TrimSpace(before.Issuer) != strings.TrimSpace(after.Issuer) {
		changed = append(changed, "issuer")
	}
	if strings.TrimSpace(before.ClientID) != strings.TrimSpace(after.ClientID) {
		changed = append(changed, "client_id")
	}
	if before.Entra != after.Entra {
		changed = append(changed, "entra")
	}
	if added(before.EntraTenants, after.EntraTenants) {
		changed = append(changed, "entra_tenants")
	}
	if !before.AdoptSCIMUsers && after.AdoptSCIMUsers {
		changed = append(changed, "adopt_scim_users")
	}
	if before.RequireEmailVerified && !after.RequireEmailVerified {
		changed = append(changed, "require_email_verified")
	}
	return changed
}

func samlTrustChanges(before, after *SAMLDocument) []string {
	if before == nil {
		before = &SAMLDocument{}
	}
	var changed []string
	if strings.TrimSpace(before.IDPEntityID) != strings.TrimSpace(after.IDPEntityID) {
		changed = append(changed, "idp_entity_id")
	}
	if strings.TrimSpace(before.IDPSSOURL) != strings.TrimSpace(after.IDPSSOURL) {
		changed = append(changed, "idp_sso_url")
	}
	if added(before.IDPCertificates, after.IDPCertificates) {
		changed = append(changed, "idp_certificates")
	}
	if strings.TrimSpace(before.EmailAttribute) != strings.TrimSpace(after.EmailAttribute) {
		changed = append(changed, "email_attribute")
	}
	if !before.AdoptSCIMUsers && after.AdoptSCIMUsers {
		changed = append(changed, "adopt_scim_users")
	}
	return changed
}

// added reports whether after holds an entry before does not, ignoring the
// surrounding white space and the order.
func added(before, after []string) bool {
	known := make([]string, 0, len(before))
	for _, entry := range before {
		known = append(known, strings.TrimSpace(entry))
	}
	for _, entry := range after {
		if !slices.Contains(known, strings.TrimSpace(entry)) {
			return true
		}
	}
	return false
}

// TrustIssuer is the configured trust identifier a provider's links are bound
// to (identityrepo.ProviderBinding): the OIDC issuer, or the SAML IdP entity
// id. Shared migration 0258 reads the same two document fields.
func (p Provider) TrustIssuer() string {
	switch {
	case p.OIDC != nil:
		return strings.TrimSpace(p.OIDC.Issuer)
	case p.SAML != nil:
		return strings.TrimSpace(p.SAML.IDPEntityID)
	default:
		return ""
	}
}
