package identityrepo

// The provider binding of a federated identity (issue 2558, ADR-0059 R3B-1).
//
// A link in auth_core__user_provider is `oidc:<sub>` or `saml:<NameID>`, and it
// names no provider. Matching a sign-in on the link alone let anyone who could
// repoint the enabled provider (a new issuer, metadata URL or signing
// certificate) assert the subject of any account and sign in as it. A sign-in
// therefore matches (provider key, issuer, subject): the link gives the
// subject, and elitea_auth.identity_bindings (shared migration 0258) gives the
// other two.
//
// THE ISSUER IS THE CONFIGURED ONE: the stored OIDC document's `issuer` (or
// OIDC_ISSUER_URL), and the SAML document's `idp_entity_id`. For a plain OIDC
// provider it is the id_token's verified `iss`, because discovery refuses an
// issuer that differs from the one configured. For an Entra provider the
// token's `iss` is per tenant, and the tenant allow-list is the trust anchor
// beside it; both are trust fields that only a super_admin may change.
//
// HOW A CHANGED ISSUER IS HANDLED. It is NOT re-linked on the next sign-in,
// even with a verified address: the address is a claim of the very identity
// provider whose trust is in question, so a verified-email re-link is the
// takeover again. The links stay bound to the old issuer, every sign-in from
// the new one is refused with 409, and a super_admin moves them with
// RebindLinks (POST /api/v2/admin/identity_providers/administration/{key}/rebind),
// which writes a security audit event.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// EnvironmentProviderKey is the provider key of the OIDC provider configured by
// OIDC_ISSUER_URL. identityproviders.NormalizeKey cannot produce '@', so no
// stored provider can take it.
const EnvironmentProviderKey = "@environment"

// ProviderBinding names the provider that asserts a federated identity: its
// key, and its configured issuer.
type ProviderBinding struct {
	Key    string
	Issuer string
}

// Valid reports whether both halves are present. A sign-in with an incomplete
// binding is a composition defect, and the resolution refuses it.
func (b ProviderBinding) Valid() bool {
	return strings.TrimSpace(b.Key) != "" && strings.TrimSpace(b.Issuer) != ""
}

// Environment reports whether b is the environment-configured provider.
func (b ProviderBinding) Environment() bool { return b.Key == EnvironmentProviderKey }

// ErrInvalidBinding is a binding with no key or no issuer.
var ErrInvalidBinding = errors.New("identityrepo: a provider binding needs a key and an issuer")

// BindLink records that binding may assert the link (userID, providerRef). It
// is idempotent. The caller holds the subject's advisory lock and has just
// created or verified the link, so a stale binding row of the same key (left
// where the foreign key could not be added) is taken over.
func BindLink(ctx context.Context, tx pgx.Tx, binding ProviderBinding, userID int, providerRef string) error {
	if !binding.Valid() {
		return ErrInvalidBinding
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO elitea_auth.identity_bindings (provider_ref, provider_key, issuer, user_id)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (provider_ref, provider_key, issuer) DO UPDATE SET user_id = EXCLUDED.user_id`,
		providerRef, binding.Key, binding.Issuer, userID,
	); err != nil {
		return fmt.Errorf("identityrepo: bind the provider identity: %w", err)
	}
	return nil
}

// CopyLinkBindings gives the link (userID, toRef) every binding the link
// (userID, fromRef) has, and reports how many it copied. oidc-relink uses it
// when it replaces a subject: the same provider re-created the user, so the new
// subject is bound where the old one was.
func CopyLinkBindings(ctx context.Context, tx pgx.Tx, userID int, fromRef, toRef string) (int64, error) {
	tag, err := tx.Exec(ctx,
		`INSERT INTO elitea_auth.identity_bindings (provider_ref, provider_key, issuer, user_id)
		 SELECT $3, provider_key, issuer, user_id
		 FROM elitea_auth.identity_bindings
		 WHERE provider_ref = $2 AND user_id = $1
		 ON CONFLICT (provider_ref, provider_key, issuer) DO NOTHING`,
		userID, fromRef, toRef)
	if err != nil {
		return 0, fmt.Errorf("identityrepo: copy the provider bindings: %w", err)
	}
	return tag.RowsAffected(), nil
}

// providerIssuerSQL is a stored provider's trust issuer in SQL: the OIDC
// issuer or the SAML IdP entity id, trimmed. It is
// identityproviders.Provider.TrustIssuer, and shared migration 0258 spells
// the same expression; identity_binding tests hold the three to one answer.
const providerIssuerSQL = `btrim(CASE provider.kind
                  WHEN 'oidc' THEN provider.config ->> 'issuer'
                  WHEN 'saml' THEN provider.config ->> 'idp_entity_id'
              END)`

// BindLinkToEnabledProvider binds the link (userID, providerRef) to the
// enabled stored provider of its protocol, and reports whether one existed.
// oidc-relink uses it for a subject ADDED to an account with no OIDC link.
// With no enabled stored provider the link stays unbound, which the
// environment provider binds on the next sign-in.
func BindLinkToEnabledProvider(ctx context.Context, tx pgx.Tx, userID int, providerRef string) (bool, error) {
	tag, err := tx.Exec(ctx,
		`INSERT INTO elitea_auth.identity_bindings (provider_ref, provider_key, issuer, user_id)
		 SELECT $2, provider.provider_key, `+providerIssuerSQL+`, $1
		 FROM elitea_auth.identity_providers AS provider
		 WHERE provider.enabled
		   AND left($2::text, length(provider.kind) + 1) = provider.kind || ':'
		   AND coalesce(`+providerIssuerSQL+`, '') <> ''
		 ON CONFLICT (provider_ref, provider_key, issuer) DO NOTHING`,
		userID, providerRef)
	if err != nil {
		return false, fmt.Errorf("identityrepo: bind to the enabled provider: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// BindUnboundLinkToUnchangedProvider binds a link that has NO binding to the
// stored provider signing in, and reports whether it did, only while that
// provider is enabled, still has binding's issuer, and has not been saved
// since shared migration 0258 was applied.
//
// It closes the rolling-upgrade window. The backfill binds the links that
// exist when 0258 runs, but replicas of the previous release keep serving
// until they roll over, and a first sign-in there writes a link with no
// binding. Such a link belongs to the provider that was live through the
// upgrade, which is what "not saved since 0258" proves: every save, a trust
// change included, bumps updated_at, and a trust change is a super_admin's.
// So a provider repointed after the upgrade never takes an unbound link; a
// super_admin re-binds those with the rebind route's from_unbound.
func BindUnboundLinkToUnchangedProvider(
	ctx context.Context, tx pgx.Tx, binding ProviderBinding, userID int, providerRef string,
) (bool, error) {
	if !binding.Valid() || binding.Environment() {
		return false, nil
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO elitea_auth.identity_bindings (provider_ref, provider_key, issuer, user_id)
		 SELECT $1, provider.provider_key, $3, $4
		 FROM elitea_auth.identity_providers AS provider
		 JOIN elitea_runtime.schema_migrations AS ledger
		   ON ledger.target_kind = 'shared' AND ledger.version = 258
		 WHERE provider.provider_key = $2
		   AND provider.enabled
		   AND `+providerIssuerSQL+` = $3
		   AND left($1::text, length(provider.kind) + 1) = provider.kind || ':'
		   AND provider.updated_at <= ledger.applied_at
		   AND NOT EXISTS (SELECT 1 FROM elitea_auth.identity_bindings AS existing
		                   WHERE existing.provider_ref = $1)
		 ON CONFLICT (provider_ref, provider_key, issuer) DO NOTHING`,
		providerRef, binding.Key, binding.Issuer, userID)
	if err != nil {
		return false, fmt.Errorf("identityrepo: bind an upgrade-window link: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RebindRequest moves the links of one protocol onto a provider's current
// binding.
type RebindRequest struct {
	// Prefix is the protocol namespace of the target provider:
	// OIDCProviderRefPrefix or SAMLProviderRefPrefix.
	Prefix string
	// Target is the provider's key and CURRENT issuer.
	Target ProviderBinding
	// From is the binding the links move from. Nil means the links of Prefix
	// with no binding at all (a protocol that had no enabled stored provider
	// at the upgrade).
	From *ProviderBinding
}

// ErrRebindSameBinding is a rebind whose source is its target.
var ErrRebindSameBinding = errors.New("identityrepo: the links are already bound to this provider and issuer")

// RebindLinks applies req in tx and reports how many links it bound to the
// target. A moved link loses its old binding, so an issuer that is retired
// stops matching for good.
func RebindLinks(ctx context.Context, tx pgx.Tx, req RebindRequest) (int64, error) {
	if !req.Target.Valid() {
		return 0, ErrInvalidBinding
	}
	if req.Prefix != OIDCProviderRefPrefix && req.Prefix != SAMLProviderRefPrefix {
		return 0, fmt.Errorf("identityrepo: unknown provider namespace %q", req.Prefix)
	}
	if req.From == nil {
		tag, err := tx.Exec(ctx,
			`INSERT INTO elitea_auth.identity_bindings (provider_ref, provider_key, issuer, user_id)
			 SELECT link.provider_ref, $2, $3, link.user_id
			 FROM public.auth_core__user_provider AS link
			 WHERE left(link.provider_ref, length($1::text)) = $1::text
			   AND NOT EXISTS (
			       SELECT 1 FROM elitea_auth.identity_bindings AS bound
			       WHERE bound.provider_ref = link.provider_ref AND bound.user_id = link.user_id)
			 ON CONFLICT (provider_ref, provider_key, issuer) DO NOTHING`,
			req.Prefix, req.Target.Key, req.Target.Issuer)
		if err != nil {
			return 0, fmt.Errorf("identityrepo: bind the unbound links: %w", err)
		}
		return tag.RowsAffected(), nil
	}
	if !req.From.Valid() {
		return 0, ErrInvalidBinding
	}
	if *req.From == req.Target {
		return 0, ErrRebindSameBinding
	}
	// Only links that still exist are moved: the join drops a binding left
	// behind where the foreign key could not be added.
	tag, err := tx.Exec(ctx,
		`INSERT INTO elitea_auth.identity_bindings (provider_ref, provider_key, issuer, user_id)
		 SELECT bound.provider_ref, $4, $5, bound.user_id
		 FROM elitea_auth.identity_bindings AS bound
		 JOIN public.auth_core__user_provider AS link
		   ON link.provider_ref = bound.provider_ref AND link.user_id = bound.user_id
		 WHERE bound.provider_key = $2 AND bound.issuer = $3
		   AND left(bound.provider_ref, length($1::text)) = $1::text
		 ON CONFLICT (provider_ref, provider_key, issuer) DO NOTHING`,
		req.Prefix, req.From.Key, req.From.Issuer, req.Target.Key, req.Target.Issuer)
	if err != nil {
		return 0, fmt.Errorf("identityrepo: bind the links to the new issuer: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM elitea_auth.identity_bindings
		 WHERE provider_key = $2 AND issuer = $3
		   AND left(provider_ref, length($1::text)) = $1::text`,
		req.Prefix, req.From.Key, req.From.Issuer); err != nil {
		return 0, fmt.Errorf("identityrepo: retire the old bindings: %w", err)
	}
	return tag.RowsAffected(), nil
}
