package auth

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The matching rule, spelled out as a table.
//
// The scripted half of the guarantee. It fixes WHICH spellings of a configured
// entry name this login; whether the grant then reaches the database is what
// first_login_postgres_integration_test.go asserts, because only PostgreSQL can
// answer that.
func TestInitialGlobalAdminMatchingAcceptsBothSpellings(t *testing.T) {
	for _, testCase := range []struct {
		name string
		// admins is `identity.initial_global_admins`.
		admins      []string
		providerRef string
		// verifiedEmail is the address the identity provider STATED is
		// verified. "" means it stated nothing, which is what every SAML login
		// and every OIDC login without `"email_verified": true` passes.
		verifiedEmail string
		want          bool
	}{
		{
			name:        "a bare entry names an OIDC subject",
			admins:      []string{"alice-sub"},
			providerRef: OIDCProviderRefPrefix + "alice-sub",
			want:        true,
		},
		{
			name:        "the prefixed spelling of the same entry also names it",
			admins:      []string{OIDCProviderRefPrefix + "alice-sub"},
			providerRef: OIDCProviderRefPrefix + "alice-sub",
			want:        true,
		},
		{
			name:        "a bare entry names a SAML NameID",
			admins:      []string{"alice-nameid"},
			providerRef: SAMLProviderRefPrefix + "alice-nameid",
			want:        true,
		},
		{
			name:        "the prefixed SAML spelling also names it",
			admins:      []string{SAMLProviderRefPrefix + "alice-nameid"},
			providerRef: SAMLProviderRefPrefix + "alice-nameid",
			want:        true,
		},
		{
			// The namespace is part of the identity, not decoration. An entry
			// written for one protocol must not promote the other protocol's
			// subject of the same name; the two can be different people at
			// different identity providers.
			name:        "a prefixed entry does not cross protocols",
			admins:      []string{OIDCProviderRefPrefix + "alice"},
			providerRef: SAMLProviderRefPrefix + "alice",
			want:        false,
		},
		{
			name:        "an unlisted subject matches nothing",
			admins:      []string{"alice-sub"},
			providerRef: OIDCProviderRefPrefix + "mallory-sub",
			want:        false,
		},
		{
			// A suffix, a substring and a lookalike are all misses. The
			// comparison is on the whole reference.
			name:        "matching is not a prefix or substring test",
			admins:      []string{"alice"},
			providerRef: OIDCProviderRefPrefix + "alice-sub",
			want:        false,
		},
		{
			name:        "an empty list promotes nobody",
			admins:      nil,
			providerRef: OIDCProviderRefPrefix + "alice-sub",
			want:        false,
		},
		{
			// An empty entry would otherwise match a reference this plane
			// cannot produce, but the list is operator input and a stray blank
			// line must never be a grant.
			name:        "an empty entry promotes nobody",
			admins:      []string{""},
			providerRef: OIDCProviderRefPrefix + "",
			want:        false,
		},

		// ── The `email:` spelling ────────────────────────────────────────
		//
		// The spelling that makes a fresh single-sign-on deployment
		// administrable without a database session. The subject inside
		// `oidc:<sub>` is an opaque identifier at Azure AD and Okta; the
		// address is one the operator already knows.
		{
			name:          "an email entry names a login with a verified address",
			admins:        []string{"email:alice@corp.com"},
			providerRef:   OIDCProviderRefPrefix + "00000000-1111-2222-3333-444444444444",
			verifiedEmail: "alice@corp.com",
			want:          true,
		},
		{
			// The single most important negative. `verifiedEmail` is "" for
			// every login the identity provider did not state a verification
			// for, and an address alone is a claim, not proof.
			name:          "an email entry does NOT name a login with an unverified address",
			admins:        []string{"email:alice@corp.com"},
			providerRef:   OIDCProviderRefPrefix + "mallory-sub",
			verifiedEmail: "",
			want:          false,
		},
		{
			name:          "the address comparison is case-insensitive on both sides",
			admins:        []string{"email:Alice@Corp.COM"},
			providerRef:   OIDCProviderRefPrefix + "alice-sub",
			verifiedEmail: "ALICE@corp.com",
			want:          true,
		},
		{
			name:          "a different verified address matches nothing",
			admins:        []string{"email:alice@corp.com"},
			providerRef:   OIDCProviderRefPrefix + "mallory-sub",
			verifiedEmail: "mallory@corp.com",
			want:          false,
		},
		{
			// The prefix is mandatory. A bare address is a REFERENCE entry —
			// it names `saml:alice@corp.com` and the pylon-era bare ref — and
			// it must not silently become an address match.
			name:          "a bare address is not an email entry",
			admins:        []string{"alice@corp.com"},
			providerRef:   OIDCProviderRefPrefix + "alice-sub",
			verifiedEmail: "alice@corp.com",
			want:          false,
		},
		{
			name:          "a bare address still names the SAML NameID that equals it",
			admins:        []string{"alice@corp.com"},
			providerRef:   SAMLProviderRefPrefix + "alice@corp.com",
			verifiedEmail: "",
			want:          true,
		},
		{
			// THE SPOOF THE CLOSED NAMESPACE REFUSES. An identity provider
			// that chooses its own subject asserts `sub = "email:alice@..."`.
			// The prefix strip turns the stored ref into the exact text of the
			// entry, so a reference comparison would hand over the grant with
			// no verified address anywhere.
			name:          "a subject spelled like an email entry collects nothing",
			admins:        []string{"email:alice@corp.com"},
			providerRef:   OIDCProviderRefPrefix + "email:alice@corp.com",
			verifiedEmail: "",
			want:          false,
		},
		{
			name:          "a verified address matches nothing when no email entry is configured",
			admins:        []string{OIDCProviderRefPrefix + "alice-sub"},
			providerRef:   OIDCProviderRefPrefix + "mallory-sub",
			verifiedEmail: "alice@corp.com",
			want:          false,
		},
		{
			name:          "an email entry with no address matches no verified address",
			admins:        []string{"email:"},
			providerRef:   OIDCProviderRefPrefix + "alice-sub",
			verifiedEmail: "alice@corp.com",
			want:          false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			require.Equal(t, testCase.want,
				matchesInitialGlobalAdmin(
					testCase.admins, testCase.providerRef, testCase.verifiedEmail))
		})
	}
}

// verifiedEmailClaim is the gate that decides whether an `email:` entry may be
// consulted at all, so its rule is pinned separately from the matcher's.
func TestVerifiedEmailClaimAcceptsOnlyAnExplicitTrue(t *testing.T) {
	verified := true
	unverified := false

	require.Equal(t, "alice@corp.com", verifiedEmailClaim("alice@corp.com", &verified))
	require.Empty(t, verifiedEmailClaim("alice@corp.com", &unverified))

	// AN ABSENT CLAIM IS NOT A VERIFICATION. Many identity providers omit it,
	// and OIDC_REQUIRE_EMAIL_VERIFIED does not change this answer: that
	// variable gates joinAccountByEmail only, so a new subject still
	// provisions an account without the claim.
	require.Empty(t, verifiedEmailClaim("alice@corp.com", nil))
	t.Setenv("OIDC_REQUIRE_EMAIL_VERIFIED", "true")
	require.Empty(t, verifiedEmailClaim("alice@corp.com", nil))
}

func TestInitialGlobalAdminsFromEnvSplitsAndTrims(t *testing.T) {
	t.Setenv("ELITEA_INITIAL_GLOBAL_ADMINS", " alice-sub , oidc:bob-sub ,, ")
	require.Equal(t,
		[]string{"alice-sub", OIDCProviderRefPrefix + "bob-sub"},
		InitialGlobalAdminsFromEnv())
}

func TestInitialGlobalAdminsFromEnvIsEmptyWhenUnset(t *testing.T) {
	t.Setenv("ELITEA_INITIAL_GLOBAL_ADMINS", "")
	require.Empty(t, InitialGlobalAdminsFromEnv())
}
