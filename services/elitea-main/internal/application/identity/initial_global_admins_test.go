package identity

// THE GAP THESE TESTS CLOSE.
//
// `identity.initial_global_admins` matched a provider reference and nothing
// else. A fresh database holds exactly one administrator, `dev@elitea.ai`, and
// that account cannot sign in on a single-sign-on-only deployment. With Azure
// AD or Okta the OIDC subject is an opaque identifier, so naming the first real
// administrator meant: deploy, sign in, get nothing, read `provider_ref` out of
// the production database, edit the chart, restart.
//
// The `email:` spelling removes that round trip. It is also the shape that can
// be abused, because an address is something an identity provider ASSERTS. The
// negative cases below are therefore the load-bearing ones.

import "testing"

func TestMatchesInitialGlobalAdmin(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		admins        []string
		reference     string
		verifiedEmail string
		want          bool
	}{
		{
			name:      "an exact reference still matches",
			admins:    []string{"alice-sub"},
			reference: "alice-sub",
			want:      true,
		},
		{
			name:      "an unlisted reference matches nothing",
			admins:    []string{"alice-sub"},
			reference: "mallory-sub",
			want:      false,
		},
		{
			name:      "an empty reference matches no entry",
			admins:    []string{""},
			reference: "",
			want:      false,
		},
		{
			name:          "an email entry matches a verified address",
			admins:        []string{"email:alice@corp.com"},
			reference:     "opaque-guid",
			verifiedEmail: "alice@corp.com",
			want:          true,
		},
		{
			// The rule the whole feature rests on. Without a stated
			// verification the caller passes "", and an `email:` entry then
			// matches nothing at all.
			name:          "an email entry does not match an unverified address",
			admins:        []string{"email:alice@corp.com"},
			reference:     "opaque-guid",
			verifiedEmail: "",
			want:          false,
		},
		{
			name:          "the address comparison ignores case",
			admins:        []string{"email:ALICE@Corp.Com"},
			reference:     "opaque-guid",
			verifiedEmail: "alice@CORP.com",
			want:          true,
		},
		{
			name:          "surrounding space in the configured address is ignored",
			admins:        []string{"email:  alice@corp.com  "},
			reference:     "opaque-guid",
			verifiedEmail: "alice@corp.com",
			want:          true,
		},
		{
			name:          "another verified address does not match",
			admins:        []string{"email:alice@corp.com"},
			reference:     "opaque-guid",
			verifiedEmail: "mallory@corp.com",
			want:          false,
		},
		{
			// THE CLOSED NAMESPACE. An `email:` entry is never compared
			// against a reference, so an identity provider that picks its own
			// subject cannot spell the entry and collect the grant.
			name:          "an email entry never matches a reference of the same text",
			admins:        []string{"email:alice@corp.com"},
			reference:     "email:alice@corp.com",
			verifiedEmail: "",
			want:          false,
		},
		{
			// The other half of the closed namespace: a reference entry is
			// never compared against an address.
			name:          "a bare address entry does not match a verified address",
			admins:        []string{"alice@corp.com"},
			reference:     "opaque-guid",
			verifiedEmail: "alice@corp.com",
			want:          false,
		},
		{
			name:          "one list can hold both shapes",
			admins:        []string{"email:alice@corp.com", "bob-sub"},
			reference:     "bob-sub",
			verifiedEmail: "",
			want:          true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := MatchesInitialGlobalAdmin(
				testCase.admins, testCase.reference, testCase.verifiedEmail)
			if got != testCase.want {
				t.Fatalf("MatchesInitialGlobalAdmin(%q, %q, %q) = %v, want %v",
					testCase.admins, testCase.reference, testCase.verifiedEmail,
					got, testCase.want)
			}
		})
	}
}

// IsInitialGlobalAdmin must stay the no-verified-address case of the matcher.
// A plane that cannot state a verification calls it, and it must never open the
// address path by accident.
func TestIsInitialGlobalAdminConsultsNoAddress(t *testing.T) {
	if IsInitialGlobalAdmin([]string{"email:alice@corp.com"}, "alice@corp.com") {
		t.Fatal("IsInitialGlobalAdmin matched an email entry against a reference")
	}
	if !IsInitialGlobalAdmin([]string{"alice-sub"}, "alice-sub") {
		t.Fatal("IsInitialGlobalAdmin refused an exact reference")
	}
}

func TestClassifyInitialGlobalAdmins(t *testing.T) {
	shapes := ClassifyInitialGlobalAdmins([]string{
		"oidc:alice-sub",
		"saml:bob@corp.com",
		"carol-sub",
		"dave@corp.com",
		"email:erin@corp.com",
		"email:  frank@corp.com  ",
	})
	if shapes.References != 4 {
		t.Fatalf("References = %d, want 4", shapes.References)
	}
	if shapes.Emails != 1 {
		t.Fatalf("Emails = %d, want 1", shapes.Emails)
	}
	// `email:  frank@corp.com  ` is reported: the address comparison trims, but
	// an entry an operator cannot read back as an address is worth a warning.
	if len(shapes.Malformed) != 1 || shapes.Malformed[0] != "email:  frank@corp.com  " {
		t.Fatalf("Malformed = %q, want the padded email entry", shapes.Malformed)
	}
}

// Every entry that cannot ever match must be reported, and no entry that CAN
// match may be.
func TestClassifyInitialGlobalAdminsReportsOnlyUnmatchableEntries(t *testing.T) {
	for _, malformed := range []string{
		"",
		"   ",
		"email:",
		"email:alice",
		"email:@corp.com",
		"email:alice@",
		"email:alice@corp",
		"email:alice corp@corp.com",
	} {
		t.Run("malformed/"+malformed, func(t *testing.T) {
			shapes := ClassifyInitialGlobalAdmins([]string{malformed})
			if len(shapes.Malformed) != 1 {
				t.Fatalf("%q classified as references=%d emails=%d, want malformed",
					malformed, shapes.References, shapes.Emails)
			}
		})
	}

	for _, wellFormed := range []string{
		"oidc:alice-sub",
		"saml:alice@corp.com",
		"alice-sub",
		"alice@corp.com",
		"email:alice@corp.com",
		"email:alice+admin@corp.co.uk",
	} {
		t.Run("well-formed/"+wellFormed, func(t *testing.T) {
			shapes := ClassifyInitialGlobalAdmins([]string{wellFormed})
			if len(shapes.Malformed) != 0 {
				t.Fatalf("%q was reported as malformed", wellFormed)
			}
			if shapes.References+shapes.Emails != 1 {
				t.Fatalf("%q counted as references=%d emails=%d, want exactly one",
					wellFormed, shapes.References, shapes.Emails)
			}
		})
	}
}

// An empty list is not a configuration to warn about.
func TestClassifyInitialGlobalAdminsOnAnEmptyList(t *testing.T) {
	shapes := ClassifyInitialGlobalAdmins(nil)
	if shapes.References != 0 || shapes.Emails != 0 || len(shapes.Malformed) != 0 {
		t.Fatalf("empty list classified as %+v", shapes)
	}
}
