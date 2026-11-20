package identityproviders

import (
	"slices"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identityrepo"
)

// TrustChanges names the trust fields a save from before to after changes, in
// the wire spelling, and nil when it changes none (issue 2558, ADR-0059
// R3B-1).
//
// A trust field decides WHO an assertion says a person is, which identity
// provider is believed, or whether an assertion is believed at all. Whoever
// can change one can point the provider at an identity provider they control
// and sign in as any account, a super_admin's included. So a change to any of
// them is a super_admin decision, and the admin write path refuses it to
// everyone else (403).
//
// A provider that does not exist yet (before.Key == "") is all trust: the
// answer is ["created"].
//
// The fields, and the direction that counts where only one direction widens
// trust:
//
//   - Both kinds: `enabled` (either way: turning a provider on retires the
//     live provider of its kind, so it decides which identity provider is
//     believed), and `adopt_scim_users` turned on.
//   - OIDC: `issuer` (also the discovery and metadata address), `client_id`,
//     `redirect_uri` (where the authorization code is sent), `entra`, an
//     ADDED `entra_tenants` entry, `entra_api_scope`,
//     `entra_federated_token_file`, `entra_agent_blueprint_id`, and
//     `require_email_verified` turned off.
//   - SAML: `idp_entity_id`, `idp_sso_url` (where people type their
//     password), an ADDED `idp_certificates` entry, `email_attribute`,
//     `name_id_format` (it decides which value is the subject),
//     `sp_entity_id` and `acs_url` (the audience and recipient an assertion
//     must name), `sign_authn_requests`, `sp_certificate`, and
//     `clock_skew_seconds` raised (a wider replay window).
//
// The sealed secret (the OIDC client secret, the SAML service provider key)
// is a trust field too, but it is not in the document: the admin write path
// adds "secret" when a save replaces or clears it.
//
// Removing a certificate or a tenant, lowering the clock skew, or tightening a
// flag only narrows trust and stays open to an `admin`, as do the display
// name, the scopes, the login domains and the name attribute.
func TrustChanges(before, after Provider) []string {
	if before.Key == "" {
		return []string{"created"}
	}
	var changed []string
	if before.Enabled != after.Enabled {
		changed = append(changed, "enabled")
	}
	switch {
	case after.OIDC != nil:
		changed = append(changed, oidcTrustChanges(before.OIDC, after.OIDC)...)
	case after.SAML != nil:
		changed = append(changed, samlTrustChanges(before.SAML, after.SAML)...)
	}
	return changed
}

func oidcTrustChanges(before, after *OIDCDocument) []string {
	if before == nil {
		before = &OIDCDocument{}
	}
	var changed []string
	for _, field := range []struct {
		name          string
		before, after string
	}{
		{"issuer", before.Issuer, after.Issuer},
		{"client_id", before.ClientID, after.ClientID},
		{"redirect_uri", before.RedirectURI, after.RedirectURI},
	} {
		if differs(field.before, field.after) {
			changed = append(changed, field.name)
		}
	}
	if before.Entra != after.Entra {
		changed = append(changed, "entra")
	}
	if added(before.EntraTenants, after.EntraTenants) {
		changed = append(changed, "entra_tenants")
	}
	for _, field := range []struct {
		name          string
		before, after string
	}{
		{"entra_api_scope", before.EntraAPIScope, after.EntraAPIScope},
		{"entra_federated_token_file", before.EntraFederatedTokenFile, after.EntraFederatedTokenFile},
		{"entra_agent_blueprint_id", before.EntraAgentBlueprintID, after.EntraAgentBlueprintID},
	} {
		if differs(field.before, field.after) {
			changed = append(changed, field.name)
		}
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
	if differs(before.IDPEntityID, after.IDPEntityID) {
		changed = append(changed, "idp_entity_id")
	}
	if differs(before.IDPSSOURL, after.IDPSSOURL) {
		changed = append(changed, "idp_sso_url")
	}
	if added(before.IDPCertificates, after.IDPCertificates) {
		changed = append(changed, "idp_certificates")
	}
	for _, field := range []struct {
		name          string
		before, after string
	}{
		{"email_attribute", before.EmailAttribute, after.EmailAttribute},
		{"name_id_format", before.NameIDFormat, after.NameIDFormat},
		{"sp_entity_id", before.SPEntityID, after.SPEntityID},
		{"acs_url", before.ACSURL, after.ACSURL},
	} {
		if differs(field.before, field.after) {
			changed = append(changed, field.name)
		}
	}
	if before.SignAuthnRequests != after.SignAuthnRequests {
		changed = append(changed, "sign_authn_requests")
	}
	if differs(before.SPCertificate, after.SPCertificate) {
		changed = append(changed, "sp_certificate")
	}
	if after.ClockSkewSeconds > before.ClockSkewSeconds {
		changed = append(changed, "clock_skew_seconds")
	}
	if !before.AdoptSCIMUsers && after.AdoptSCIMUsers {
		changed = append(changed, "adopt_scim_users")
	}
	return changed
}

// differs compares two document values the way the validator stores them.
func differs(before, after string) bool {
	return strings.TrimSpace(before) != strings.TrimSpace(after)
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
// to: the OIDC issuer, or the SAML IdP entity id, trimmed. Shared migration
// 0258 and identityrepo.BindLinkToEnabledProvider read the same two document
// fields with the same btrim, and identity_bindings_postgres tests hold the
// three to one answer.
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

// Binding is the (provider key, issuer) every link this provider signs in is
// checked against. The OIDC, Entra and SAML sign-in runtimes and the re-bind
// route all take it from here, so they cannot disagree about the issuer.
func (p Provider) Binding() identityrepo.ProviderBinding {
	return identityrepo.ProviderBinding{Key: p.Key, Issuer: p.TrustIssuer()}
}
