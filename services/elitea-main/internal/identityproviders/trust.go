package identityproviders

import (
	"maps"
	"reflect"
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
//
// It fails closed (ADR-0059 R0.4c): a document field that FieldClasses does
// not list, a field added after this was written, is a trust field. Its change
// is named in its wire spelling until someone lists it as a setting.
func TrustChanges(before, after Provider) []string {
	if before.Key == "" {
		return []string{"created"}
	}
	var changed []string
	if before.Enabled != after.Enabled {
		changed = append(changed, "enabled")
	}
	changed = append(changed, unclassifiedChanges(before, after, providerFieldClasses)...)
	switch {
	case after.OIDC != nil:
		changed = append(changed, oidcTrustChanges(before.OIDC, after.OIDC)...)
		changed = append(changed, unclassifiedChanges(orZero(before.OIDC), *after.OIDC, oidcFieldClasses)...)
	case after.SAML != nil:
		changed = append(changed, samlTrustChanges(before.SAML, after.SAML)...)
		changed = append(changed, unclassifiedChanges(orZero(before.SAML), *after.SAML, samlFieldClasses)...)
	}
	return changed
}

// FieldClass says which half of the identity permissions a field belongs to
// (ADR-0059 decision 2, story R0.4c).
type FieldClass string

const (
	// FieldTrust decides who an assertion says a person is, which identity
	// provider is believed, or whether an assertion is believed at all. A
	// save that changes one is a trust action: identity.sso.trust.manage or
	// super_admin. TrustChanges compares each, in the direction that widens
	// trust where only one direction does.
	FieldTrust FieldClass = "trust"
	// FieldSettings is presentation or routing: changing it cannot make the
	// platform believe a different person. identity.sso.settings.manage or
	// runtime.plugins.
	FieldSettings FieldClass = "settings"
)

// The classification of every field of the stored provider, keyed by the Go
// field name of Provider and by the wire name of each document. A field that
// is missing here is a trust field (unclassifiedChanges), and
// TestEveryProviderFieldIsClassified fails until it is listed.
var (
	providerFieldClasses = map[string]FieldClass{
		// The key is the path segment and the kind cannot change under it
		// (409); both name what a link is bound to.
		"Key":  FieldTrust,
		"Kind": FieldTrust,
		// The button text.
		"DisplayName": FieldSettings,
		// Turning a provider on retires the live provider of its kind; off
		// stops every sign-in through it (issue 2558).
		"Enabled": FieldTrust,
		// Written by the store, never by the body.
		"Revision":  FieldSettings,
		"UpdatedAt": FieldSettings,
		// The sealed credential; the admin write path names a replacement
		// "secret".
		"SecretRef": FieldTrust,
		// The documents; each of their fields is classified below.
		"OIDC": FieldTrust,
		"SAML": FieldTrust,
	}
	oidcFieldClasses = map[string]FieldClass{
		"issuer":                     FieldTrust,
		"client_id":                  FieldTrust,
		"redirect_uri":               FieldTrust,
		"scopes":                     FieldSettings,
		"require_email_verified":     FieldTrust,
		"login_domains":              FieldSettings,
		"adopt_scim_users":           FieldTrust,
		"entra":                      FieldTrust,
		"entra_tenants":              FieldTrust,
		"entra_api_scope":            FieldTrust,
		"entra_federated_token_file": FieldTrust,
		"entra_agent_blueprint_id":   FieldTrust,
	}
	samlFieldClasses = map[string]FieldClass{
		"idp_entity_id":       FieldTrust,
		"idp_sso_url":         FieldTrust,
		"idp_slo_url":         FieldSettings,
		"idp_certificates":    FieldTrust,
		"sp_entity_id":        FieldTrust,
		"acs_url":             FieldTrust,
		"name_id_format":      FieldTrust,
		"email_attribute":     FieldTrust,
		"name_attribute":      FieldSettings,
		"sign_authn_requests": FieldTrust,
		"sp_certificate":      FieldTrust,
		"clock_skew_seconds":  FieldTrust,
		"login_domains":       FieldSettings,
		"adopt_scim_users":    FieldTrust,
	}
)

// FieldClasses answers the classification of the provider row ("provider",
// Go field names) and of the two documents ("oidc", "saml", wire names). The
// maps are copies.
func FieldClasses() map[string]map[string]FieldClass {
	return map[string]map[string]FieldClass{
		"provider": maps.Clone(providerFieldClasses),
		"oidc":     maps.Clone(oidcFieldClasses),
		"saml":     maps.Clone(samlFieldClasses),
	}
}

// FieldName is the name a classification map keys a struct field by: its
// wire name when it has a JSON tag, its Go name otherwise.
func FieldName(field reflect.StructField) string {
	if name, _, _ := strings.Cut(field.Tag.Get("json"), ","); name != "" && name != "-" {
		return name
	}
	return field.Name
}

// unclassifiedChanges names every field of before and after (structs of one
// type) that classes does not list and whose value differs. It is the fail
// closed half of TrustChanges: such a field is a trust field.
func unclassifiedChanges[T any](before, after T, classes map[string]FieldClass) []string {
	beforeValue, afterValue := reflect.ValueOf(before), reflect.ValueOf(after)
	var changed []string
	for i := range beforeValue.NumField() {
		name := FieldName(beforeValue.Type().Field(i))
		if _, classified := classes[name]; classified {
			continue
		}
		if !reflect.DeepEqual(beforeValue.Field(i).Interface(), afterValue.Field(i).Interface()) {
			changed = append(changed, name)
		}
	}
	return changed
}

func orZero[T any](document *T) T {
	if document == nil {
		var zero T
		return zero
	}
	return *document
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
