package identityproviders

import (
	"errors"
	"strings"
	"testing"
)

// Login domains route a typed work address to its provider. An entry the page
// cannot match is refused at the write, and the stored form is lower case.
func TestLoginDomainsAreNormalisedAndValidated(t *testing.T) {
	provider := validOIDC()
	provider.OIDC.LoginDomains = []string{" Corp.COM ", "@sub.corp.com", "corp.com", ""}
	stored, err := Validate(provider)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := strings.Join(stored.OIDC.LoginDomains, ",")
	if got != "corp.com,sub.corp.com" {
		t.Fatalf("login domains = %q, want corp.com,sub.corp.com", got)
	}

	saml := validSAML(t)
	saml.SAML.LoginDomains = []string{"Contoso.onmicrosoft.com"}
	stored, err = Validate(saml)
	if err != nil {
		t.Fatalf("Validate SAML: %v", err)
	}
	if len(stored.SAML.LoginDomains) != 1 || stored.SAML.LoginDomains[0] != "contoso.onmicrosoft.com" {
		t.Fatalf("SAML login domains = %v", stored.SAML.LoginDomains)
	}

	for _, bad := range []string{"localhost", "corp..com", "-corp.com", "corp.com/path", "a b.com", "user@corp.com", "*.corp.com"} {
		provider := validOIDC()
		provider.OIDC.LoginDomains = []string{bad}
		_, err := Validate(provider)
		var validation ValidationError
		if !errors.As(err, &validation) || validation.Field != "login_domains" {
			t.Errorf("%q: error = %v, want a login_domains refusal", bad, err)
		}
	}
}

func TestNoLoginDomainsStoresNone(t *testing.T) {
	stored, err := Validate(validOIDC())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if stored.OIDC.LoginDomains != nil {
		t.Fatalf("login domains = %v, want nil", stored.OIDC.LoginDomains)
	}
}
