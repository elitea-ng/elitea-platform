package credentialadapters

import (
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

func minimal() AdapterDescriptor {
	return AdapterDescriptor{
		ProviderType:       "acme",
		AuthClasses:        []credentialgrant.CredentialClass{staticSecret},
		TargetKind:         TargetBaseURL,
		CanonicalizeTarget: CanonicalBaseURL,
		Operations:         []OperationSpec{op("acme.thing.read", OperationRead, "thing", 1)},
		Probe:              probe("acme.thing.read"),
		MaterialKeys:       map[credentialgrant.CredentialClass][]string{staticSecret: {"token"}},
		Qualification:      map[credentialgrant.CredentialClass]string{staticSecret: QualificationUnqualified},
	}
}

func TestNewRegistryRefusesInvalidDescriptors(t *testing.T) {
	cases := map[string]func(d *AdapterDescriptor){
		"blank provider":      func(d *AdapterDescriptor) { d.ProviderType = "" },
		"upper-case provider": func(d *AdapterDescriptor) { d.ProviderType = "Acme" },
		"no class":            func(d *AdapterDescriptor) { d.AuthClasses = nil },
		"unknown class":       func(d *AdapterDescriptor) { d.AuthClasses = append(d.AuthClasses, "kerberos") },
		"duplicate class":     func(d *AdapterDescriptor) { d.AuthClasses = append(d.AuthClasses, staticSecret) },
		"unknown target kind": func(d *AdapterDescriptor) { d.TargetKind = "ip" },
		"no canonicalizer":    func(d *AdapterDescriptor) { d.CanonicalizeTarget = nil },
		"no operation":        func(d *AdapterDescriptor) { d.Operations = nil; d.Probe = nil },
		"malformed operation": func(d *AdapterDescriptor) {
			d.Operations = append(d.Operations, op("acme.read", OperationRead, "x", 0))
		},
		"foreign operation": func(d *AdapterDescriptor) {
			d.Operations = append(d.Operations, op("jira.x.read", OperationRead, "x", 0))
		},
		"unknown operation class": func(d *AdapterDescriptor) { d.Operations = append(d.Operations, op("acme.x.admin", "admin", "x", 0)) },
		"no resource kind": func(d *AdapterDescriptor) {
			d.Operations = append(d.Operations, op("acme.x.read", OperationRead, " ", 0))
		},
		"duplicate operation":    func(d *AdapterDescriptor) { d.Operations = append(d.Operations, d.Operations[0]) },
		"probe not an operation": func(d *AdapterDescriptor) { d.Probe = probe("acme.other.read") },
		"probe is a write": func(d *AdapterDescriptor) {
			d.Operations = append(d.Operations, op("acme.thing.write", OperationWrite, "thing", 1))
			d.Probe = probe("acme.thing.write")
		},
		"material keys for an undeclared class": func(d *AdapterDescriptor) {
			d.MaterialKeys[oauthDelegated] = []string{"access_token"}
		},
		"declared class without material keys": func(d *AdapterDescriptor) { delete(d.MaterialKeys, staticSecret) },
		"blank material key":                   func(d *AdapterDescriptor) { d.MaterialKeys[staticSecret] = []string{""} },
		"repeated material key":                func(d *AdapterDescriptor) { d.MaterialKeys[staticSecret] = []string{"token", "token"} },
		"qualification for an undeclared class": func(d *AdapterDescriptor) {
			d.Qualification[oauthDelegated] = QualificationFixture
		},
		"declared class without qualification": func(d *AdapterDescriptor) { delete(d.Qualification, staticSecret) },
		"qualified without evidence":           func(d *AdapterDescriptor) { d.Qualification[staticSecret] = "qualified:" },
		"bare qualified":                       func(d *AdapterDescriptor) { d.Qualification[staticSecret] = "qualified" },
		"unknown qualification":                func(d *AdapterDescriptor) { d.Qualification[staticSecret] = "supported" },
		"scopes for an undeclared class": func(d *AdapterDescriptor) {
			d.Scopes = map[credentialgrant.CredentialClass][]string{oauthDelegated: {"x"}}
		},
		"rotation for an undeclared class": func(d *AdapterDescriptor) { d.RotationRequired = []credentialgrant.CredentialClass{oauthDelegated} },
		"negative cost hint":               func(d *AdapterDescriptor) { d.Operations[0].CostHint = -1 },
		"OAuth authority without oauth_delegated": func(d *AdapterDescriptor) {
			d.OAuthAuthority = credentialgrant.OAuthAuthorityProvider
		},
		"oauth_delegated without an OAuth authority": func(d *AdapterDescriptor) {
			d.AuthClasses = append(d.AuthClasses, oauthDelegated)
			d.MaterialKeys[oauthDelegated] = []string{"access_token"}
			d.Qualification[oauthDelegated] = QualificationFixture
		},
		"unknown OAuth authority": func(d *AdapterDescriptor) {
			d.AuthClasses = append(d.AuthClasses, oauthDelegated)
			d.MaterialKeys[oauthDelegated] = []string{"access_token"}
			d.Qualification[oauthDelegated] = QualificationFixture
			d.OAuthAuthority = "saml"
		},
		"Entra on-behalf-of without an audience": func(d *AdapterDescriptor) {
			d.AuthClasses = append(d.AuthClasses, oauthDelegated)
			d.MaterialKeys[oauthDelegated] = []string{"access_token"}
			d.Qualification[oauthDelegated] = QualificationFixture
			d.OAuthAuthority = credentialgrant.OAuthAuthorityEntraOBO
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := minimal()
			mutate(&d)
			if _, err := NewRegistry(d); !errors.Is(err, ErrInvalidDescriptor) {
				t.Fatalf("NewRegistry = %v, want ErrInvalidDescriptor", err)
			}
		})
	}
}

func TestNewRegistryAcceptsQualifiedWithEvidence(t *testing.T) {
	d := minimal()
	d.Qualification[staticSecret] = Qualified("elitea_issues#1124")
	if _, err := NewRegistry(d); err != nil {
		t.Fatal(err)
	}
}

func TestNewRegistryRefusesDuplicateProvidersAndCrossProviderOperations(t *testing.T) {
	if _, err := NewRegistry(minimal(), minimal()); err == nil || !strings.Contains(err.Error(), "registered twice") {
		t.Fatalf("duplicate provider: %v", err)
	}
	// Two providers cannot share an operation id. The provider prefix rule
	// already stops it, so the cross-provider check is proven by the
	// built-ins registering twice under one registry.
	if _, err := NewRegistry(append(Builtin(), Jira())...); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("second jira: %v", err)
	}
}

// The registry keeps its own copy: changing a descriptor, or a value read
// back, after NewRegistry changes nothing.
func TestRegistryIsImmutable(t *testing.T) {
	d := minimal()
	r, err := NewRegistry(d)
	if err != nil {
		t.Fatal(err)
	}
	d.MaterialKeys[staticSecret][0] = "everything"
	d.Operations[0].Class = OperationWrite
	keys := r.MaterialKeys("acme", staticSecret)
	keys[0] = "mutated"
	got, _ := r.Descriptor("acme")
	got.Qualification[staticSecret] = Qualified("x")
	got.AuthClasses[0] = oauthDelegated
	if k := r.MaterialKeys("acme", staticSecret); len(k) != 1 || k[0] != "token" {
		t.Errorf("material keys changed: %v", k)
	}
	if c := r.Class("acme.thing.read"); c != OperationRead {
		t.Errorf("class changed: %q", c)
	}
	again, _ := r.Descriptor("acme")
	if again.Qualification[staticSecret] != QualificationUnqualified || again.AuthClasses[0] != staticSecret {
		t.Errorf("descriptor changed: %+v", again)
	}
}

func TestRegistryUnknownLookupsAnswerNothing(t *testing.T) {
	r, err := NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if r.Known("jira.issue.delete") || r.Class("jira.issue.delete") != "" {
		t.Error("an unlisted operation is known")
	}
	if _, ok := r.ProviderFor("nope.x.read"); ok {
		t.Error("ProviderFor answered an unknown id")
	}
	if k := r.MaterialKeys("qtest", oauthDelegated); k != nil {
		t.Errorf("qtest oauth_delegated keys = %v, want none", k)
	}
	if k := r.MaterialKeys("nope", staticSecret); k != nil {
		t.Errorf("unknown provider keys = %v", k)
	}
	if _, ok := r.Descriptor("nope"); ok {
		t.Error("Descriptor answered an unknown provider")
	}
	var nilRegistry *Registry
	if nilRegistry.Known("jira.issue.read") || nilRegistry.MaterialKeys("jira", staticSecret) != nil {
		t.Error("a nil registry knows something")
	}
	if got := r.Providers(); strings.Join(got, ",") != "jira,confluence,aha,sharepoint,qtest,mcp" {
		t.Errorf("providers = %v", got)
	}
}

func TestCanonicalBaseURL(t *testing.T) {
	good := map[string]string{
		"https://Acme.Atlassian.net/":         "https://acme.atlassian.net",
		"HTTPS://acme.atlassian.net":          "https://acme.atlassian.net",
		"https://jira.example.com:443/jira//": "https://jira.example.com/jira",
		"https://jira.example.com:8443/jira":  "https://jira.example.com:8443/jira",
		"  https://acme.aha.io  ":             "https://acme.aha.io",
		"https://[::1]:8443/x":                "https://[::1]:8443/x",
	}
	for raw, want := range good {
		got, err := CanonicalBaseURL(raw)
		if err != nil || got != want {
			t.Errorf("CanonicalBaseURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"", "http://acme.atlassian.net", "https://user:pw@acme.atlassian.net",
		"https://acme.atlassian.net/?x=1", "https://acme.atlassian.net/?", "https://acme.atlassian.net/#f",
		"https:///path", "acme.atlassian.net", "https://acme.atlassian.net/a/../b", "mailto:x@y",
		"https://acme .net", "https://" + strings.Repeat("a", 600) + ".net",
	} {
		if got, err := CanonicalBaseURL(raw); !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("CanonicalBaseURL(%q) = %q, %v; want ErrInvalidTarget", raw, got, err)
		}
	}
	if got, err := CanonicalTenantSite("https://Contoso.SharePoint.com/sites/Eng/"); err != nil || got != "https://contoso.sharepoint.com/sites/Eng" {
		t.Errorf("tenant site = %q, %v", got, err)
	}
	if _, err := CanonicalTenantSite("https:///sites/eng"); !errors.Is(err, ErrInvalidTarget) {
		t.Errorf("tenant site without a host: %v", err)
	}
}

// #1121: SharePoint's oauth_delegated class is the Entra on-behalf-of
// exchange; Jira, Confluence and Aha! use the provider's own OAuth grant.
func TestBuiltinOAuthAuthorities(t *testing.T) {
	r, err := NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]credentialgrant.OAuthAuthority{
		"sharepoint": credentialgrant.OAuthAuthorityEntraOBO,
		"jira":       credentialgrant.OAuthAuthorityProvider,
		"confluence": credentialgrant.OAuthAuthorityProvider,
		"aha":        credentialgrant.OAuthAuthorityProvider,
		"qtest":      "",
		"unknown":    "",
	}
	for provider, authority := range want {
		if got := r.OAuthAuthority(provider, oauthDelegated); got != authority {
			t.Errorf("%s: OAuthAuthority = %q, want %q", provider, got, authority)
		}
		if got := r.OAuthAuthority(provider, staticSecret); got != "" {
			t.Errorf("%s: static_secret has an OAuth authority %q", provider, got)
		}
	}
}
