package identityproviders

// The identity split (ADR-0059 R0.4c) rests on one list: which field of a
// provider is trust and which is a setting. A field added later must not slip
// into the settings half because nobody thought about it, so these tests walk
// the stored types by reflection rather than trusting the list.

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

// classifiedTypes are the stored provider type and its two documents, with
// the map that classifies each.
func classifiedTypes() map[string]reflect.Type {
	return map[string]reflect.Type{
		"provider": reflect.TypeFor[Provider](),
		"oidc":     reflect.TypeFor[OIDCDocument](),
		"saml":     reflect.TypeFor[SAMLDocument](),
	}
}

// Every field of the stored provider type is classified trust or settings,
// and the classification names no field the type does not have.
func TestEveryProviderFieldIsClassified(t *testing.T) {
	classes := FieldClasses()
	for kind, typ := range classifiedTypes() {
		seen := map[string]bool{}
		for i := range typ.NumField() {
			name := FieldName(typ.Field(i))
			seen[name] = true
			switch classes[kind][name] {
			case FieldTrust, FieldSettings:
			default:
				t.Errorf("%s field %q (%s.%s) is not classified: list it as trust or settings in "+
					"internal/identityproviders/trust.go; until then a change to it is a trust change",
					kind, name, typ.Name(), typ.Field(i).Name)
			}
		}
		for name := range classes[kind] {
			if !seen[name] {
				t.Errorf("%s classifies %q, which %s does not have", kind, name, typ.Name())
			}
		}
	}
}

// An unclassified field is a trust field at run time too, not only in the
// test above: the fallback names it.
func TestAnUnclassifiedFieldIsATrustChange(t *testing.T) {
	type document struct {
		Listed   string `json:"listed"`
		Unlisted string `json:"unlisted"`
	}
	classes := map[string]FieldClass{"listed": FieldSettings}
	if got := unclassifiedChanges(document{Listed: "a"}, document{Listed: "b"}, classes); len(got) != 0 {
		t.Errorf("a listed field reported %q", got)
	}
	got := unclassifiedChanges(document{Unlisted: "a"}, document{Unlisted: "b"}, classes)
	if !slices.Equal(got, []string{"unlisted"}) {
		t.Errorf("an unlisted field change reported %q, want [unlisted]", got)
	}

	// An unexported field cannot be compared through reflection: it is a
	// trust change on every save, and never a panic.
	type withHidden struct {
		Listed string `json:"listed"`
		hidden string
	}
	hidden := unclassifiedChanges(withHidden{hidden: "a"}, withHidden{hidden: "a"}, classes)
	if !slices.Equal(hidden, []string{"hidden"}) {
		t.Errorf("an unexported unlisted field reported %q, want [hidden]", hidden)
	}
}

// storedOIDC and storedSAML are full stored providers, every field set, so a
// mutation of any field is a real change.
func storedOIDC() Provider {
	return Provider{
		Key: "corporate", Kind: KindOIDC, DisplayName: "Corporate", Enabled: true, Revision: 3,
		SecretRef: "ref", UpdatedAt: time.Unix(1, 0),
		OIDC: &OIDCDocument{
			Issuer: "https://idp.example.com", ClientID: "elitea", RedirectURI: "https://e.example.com/cb",
			Scopes: []string{"openid"}, RequireEmailVerified: true, LoginDomains: []string{"example.com"},
			AdoptSCIMUsers: false, Entra: false, EntraTenants: []string{"tenant-a"},
			EntraAPIScope: "api://x", EntraFederatedTokenFile: "/var/run/token", EntraAgentBlueprintID: "bp",
		},
	}
}

func storedSAML() Provider {
	return Provider{
		Key: "corporate", Kind: KindSAML, DisplayName: "Corporate", Enabled: true, Revision: 3,
		SecretRef: "ref", UpdatedAt: time.Unix(1, 0),
		SAML: &SAMLDocument{
			IDPEntityID: "https://idp.example.com", IDPSSOURL: "https://idp.example.com/sso",
			IDPSLOURL: "https://idp.example.com/slo", IDPCertificates: []string{"cert-a"},
			SPEntityID: "https://e.example.com", ACSURL: "https://e.example.com/acs",
			NameIDFormat: "email", EmailAttribute: "mail", NameAttribute: "cn",
			SignAuthnRequests: false, SPCertificate: "sp", ClockSkewSeconds: 60,
			LoginDomains: []string{"example.com"}, AdoptSCIMUsers: false,
		},
	}
}

// mutations answer the two changes of a field worth trying: a value that is
// added or raised, and one that is removed or cleared. A trust field whose
// trust widens in one direction only must be named in that direction.
func mutations(value reflect.Value) []reflect.Value {
	widened, cleared := reflect.New(value.Type()).Elem(), reflect.New(value.Type()).Elem()
	switch value.Kind() {
	case reflect.String:
		widened.SetString(value.String() + "-changed")
	case reflect.Bool:
		widened.SetBool(!value.Bool())
		return []reflect.Value{widened}
	case reflect.Int:
		widened.SetInt(value.Int() + 1)
	case reflect.Slice:
		widened = reflect.Append(value, reflect.ValueOf("added"))
	default:
		return nil
	}
	return []reflect.Value{widened, cleared}
}

// Every document field classified trust is compared by TrustChanges: some
// change of it is named. Every field classified settings is never named.
func TestTrustChangesComparesEveryTrustFieldAndNoSetting(t *testing.T) {
	classes := FieldClasses()
	// Each call of storedOIDC or storedSAML builds a new document, so a
	// mutation of one never reaches the provider it is compared with.
	for kind, base := range map[string]func() Provider{"oidc": storedOIDC, "saml": storedSAML} {
		document := func(p Provider) reflect.Value {
			if kind == "oidc" {
				return reflect.ValueOf(p.OIDC).Elem()
			}
			return reflect.ValueOf(p.SAML).Elem()
		}
		original := document(base())
		for i := range original.NumField() {
			name := FieldName(original.Type().Field(i))
			named := false
			for _, mutated := range mutations(original.Field(i)) {
				after := base()
				document(after).Field(i).Set(mutated)
				changes := TrustChanges(base(), after)
				if slices.Contains(changes, name) {
					named = true
				}
				if classes[kind][name] == FieldSettings && len(changes) != 0 {
					t.Errorf("%s settings field %q: a change to %v is named as trust %q", kind, name, mutated, changes)
				}
			}
			if classes[kind][name] == FieldTrust && !named {
				t.Errorf("%s trust field %q: no change of it is named by TrustChanges", kind, name)
			}
		}
	}
}

// The provider row: the display name is a setting; enabling and disabling
// stay trust (issue 2558), and a key that does not exist is a create.
func TestTrustChangesOnTheProviderRow(t *testing.T) {
	before := storedOIDC()
	renamed := storedOIDC()
	renamed.DisplayName = "Corporate sign-in"
	renamed.Revision, renamed.UpdatedAt = 0, time.Time{}
	if got := TrustChanges(before, renamed); len(got) != 0 {
		t.Errorf("a rename is named as trust %q", got)
	}
	disabled := storedOIDC()
	disabled.Enabled = false
	if got := TrustChanges(before, disabled); !slices.Equal(got, []string{"enabled"}) {
		t.Errorf("disabling = %q, want [enabled]", got)
	}
	if got := TrustChanges(Provider{}, renamed); !slices.Equal(got, []string{"created"}) {
		t.Errorf("a save on a key that does not exist = %q, want [created]", got)
	}
}
