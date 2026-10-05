package authcomposition

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	browserapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
)

func TestParseAcceptsOneCompleteFormSnapshot(t *testing.T) {
	config, err := Parse([]byte(validConfigYAML))
	if err != nil {
		t.Fatal(err)
	}
	if config.SchemaVersion != FormSchemaVersion || config.Provider.Kind != "form" ||
		config.Provider.Form == nil || config.Provider.Form.UsersJSONFile != "/run/secrets/auth/form-users.json" ||
		config.Credentials.PATSigningKeyFile != "/run/secrets/auth/pat-hs512-key" ||
		len(config.TrustedProxyCIDRs) != 2 || config.Cookie.LifetimeSeconds != 604800 ||
		config.Credentials.Headers == nil || config.Mappers.Contract != browserapi.MapperContractTrackedV1 ||
		len(config.Authorization.MainConfiguredPublicRules) != 4 ||
		config.Identity.InitialGlobalAdmins == nil ||
		config.Identity.ProjectEnrollment == nil ||
		len(config.Identity.ProjectEnrollment.AdditionalProjectRolesForGlobalAdmins) != 7 ||
		config.Redirects.DirectAccessDenied != "/access_denied" ||
		config.Redirects.MainAccessDenied != "/app/access_denied" {
		t.Fatalf("unexpected config: %+v", config)
	}
}

func TestMainConfiguredPublicRulesReturnsTypedDetachedRules(t *testing.T) {
	config := parsedValidConfig(t)
	rules, err := config.MainConfiguredPublicRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 4 || rules[0].Name != "config.edge_auth" ||
		len(rules[0].Conditions) != 1 || rules[0].Conditions[0].Field != forwardapp.SourceURI ||
		rules[0].Conditions[0].Pattern != `/auth/.*` {
		t.Fatalf("unexpected rules: %+v", rules)
	}
	rules[0].Name = "mutated"
	rules[0].Conditions[0].Pattern = "mutated"
	again, err := config.MainConfiguredPublicRules()
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Name != "config.edge_auth" || again[0].Conditions[0].Pattern != `/auth/.*` {
		t.Fatalf("configuration aliased returned rules: %+v", again[0])
	}
}

func TestLoadUsesBoundedCanonicalPublicFile(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "auth.yaml")
	if err := os.WriteFile(path, []byte(validConfigYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}

	symlink := filepath.Join(root, "auth-link.yaml")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(symlink); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("symlink error = %v", err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("group-writable error = %v", err)
	}
}

func TestParseRejectsAmbiguousOrExecutableYAML(t *testing.T) {
	tests := map[string]string{
		"empty":              "",
		"multiple documents": validConfigYAML + "\n---\n{}\n",
		"duplicate field":    validConfigYAML + "\nschema_version: elitea.auth.form.v1\n",
		"unknown field":      strings.Replace(validConfigYAML, "public_origin:", "unknown_field: value\npublic_origin:", 1),
		"dormant provider": strings.Replace(
			validConfigYAML,
			"  form:\n    users_json_file: /run/secrets/auth/form-users.json",
			"  form:\n    users_json_file: /run/secrets/auth/form-users.json\n  oidc:\n    client_id: dormant",
			1,
		),
		"anchor": strings.Replace(
			validConfigYAML,
			"trusted_proxy_cidrs:",
			"trusted_proxy_cidrs: &trusted",
			1,
		),
		"alias": strings.Replace(
			validConfigYAML,
			"initial_global_admins:\n    - admin",
			"initial_global_admins: *trusted",
			1,
		),
		"merge key": strings.Replace(
			validConfigYAML,
			"redirects:\n",
			"redirects:\n  <<: {access_denied: /bad}\n",
			1,
		),
		"custom tag": strings.Replace(
			validConfigYAML,
			"public_origin: https://elitea.example",
			"public_origin: !env ELITEA_ORIGIN",
			1,
		),
		"environment expression": strings.Replace(
			validConfigYAML,
			"public_origin: https://elitea.example",
			"public_origin: ${PUBLIC_ORIGIN}",
			1,
		),
		"null": strings.Replace(
			validConfigYAML,
			"same_site: lax",
			"same_site: null",
			1,
		),
		"explicit builtin tag": strings.Replace(
			validConfigYAML,
			"public_origin: https://elitea.example",
			"public_origin: !!bool true",
			1,
		),
		"nested duplicate field": strings.Replace(
			validConfigYAML,
			"        - field: uri\n          pattern: '/auth/.*'",
			"        - field: uri\n          field: host\n          pattern: '/auth/.*'",
			1,
		),
		"JSON duplicate field": `{"schema_version":"elitea.auth.form.v1","schema_version":"duplicate"}`,
		"noncanonical integer": strings.Replace(validConfigYAML, "lifetime_seconds: 604800", "lifetime_seconds: 0604800", 1),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(raw))
			if !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("error = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "dormant") {
				t.Fatalf("input value escaped into error: %v", err)
			}
		})
	}
}

func TestParseRejectsStructuralAndByteBounds(t *testing.T) {
	if _, err := Parse(make([]byte, MaxConfigBytes+1)); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("oversized error = %v", err)
	}
	oversizedScalar := "schema_version: " + strings.Repeat("a", maxScalarBytes+1)
	if _, err := Parse([]byte(oversizedScalar)); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("scalar error = %v", err)
	}

	nested := "value"
	for index := 0; index < maxConfigDepth+1; index++ {
		nested = "level" + strings.Repeat("x", index) + ":\n" + indent(nested, 2)
	}
	if _, err := Parse([]byte(nested)); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("depth error = %v", err)
	}

	var collection strings.Builder
	for index := 0; index <= maxCollectionEntries; index++ {
		collection.WriteString("field")
		collection.WriteString(strings.Repeat("x", index/26))
		collection.WriteByte(byte('a' + index%26))
		collection.WriteString(": value\n")
	}
	if _, err := Parse([]byte(collection.String())); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("collection error = %v", err)
	}

	var sequence strings.Builder
	sequence.WriteString("values:\n")
	for index := 0; index <= maxCollectionEntries; index++ {
		sequence.WriteString("  - value\n")
	}
	if _, err := Parse([]byte(sequence.String())); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("sequence error = %v", err)
	}

	var nodes strings.Builder
	for item := 0; item < 256; item++ {
		nodes.WriteString("- field0: value\n")
		for field := 1; field < 8; field++ {
			nodes.WriteString("  field")
			nodes.WriteByte(byte('0' + field))
			nodes.WriteString(": value\n")
		}
	}
	if _, err := Parse([]byte(nodes.String())); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("node count error = %v", err)
	}
}

func TestConfigValidateRejectsUnsafeOrIncompleteValues(t *testing.T) {
	tests := map[string]func(*Config){
		"schema version":           func(config *Config) { config.SchemaVersion = "elitea.auth.v2" },
		"plaintext origin":         func(config *Config) { config.PublicOrigin = "http://elitea.example" },
		"origin credentials":       func(config *Config) { config.PublicOrigin = "https://user@elitea.example" },
		"origin query":             func(config *Config) { config.PublicOrigin = "https://elitea.example?x=1" },
		"origin noncanonical port": func(config *Config) { config.PublicOrigin = "https://elitea.example:0443" },
		"origin empty port":        func(config *Config) { config.PublicOrigin = "https://elitea.example:" },
		"origin invalid DNS label": func(config *Config) { config.PublicOrigin = "https://bad_host.example" },
		"missing proxies":          func(config *Config) { config.TrustedProxyCIDRs = nil },
		"noncanonical proxy":       func(config *Config) { config.TrustedProxyCIDRs = []string{"10.0.0.1/8"} },
		"duplicate proxy":          func(config *Config) { config.TrustedProxyCIDRs = []string{"10.0.0.0/8", "10.0.0.0/8"} },
		"absolute redirect":        func(config *Config) { config.Redirects.DefaultLogin = "https://attacker.example" },
		"network redirect":         func(config *Config) { config.Redirects.DefaultLogout = "//attacker.example" },
		"cookie name":              func(config *Config) { config.Cookie.Name = "bad cookie" },
		"cookie same site":         func(config *Config) { config.Cookie.SameSite = "none" },
		"cookie lifetime":          func(config *Config) { config.Cookie.LifetimeSeconds = 1 },
		"Redis topology":           func(config *Config) { config.Redis.Topology = "cluster" },
		"plaintext Redis":          func(config *Config) { config.Redis.URL = "redis://auth@redis.example:6379/0" },
		"Redis URL password":       func(config *Config) { config.Redis.URL = "rediss://auth:secret@redis.example:6379/0" },
		"Redis database":           func(config *Config) { config.Redis.URL = "rediss://auth@redis.example:6379/1" },
		"Redis empty port":         func(config *Config) { config.Redis.URL = "rediss://auth@redis.example:/0" },
		"Redis prefix":             func(config *Config) { config.Redis.KeyPrefix = "auth:{shared}" },
		"relative secret path":     func(config *Config) { config.Redis.PasswordFile = "redis-password" },
		"dynamic secret path":      func(config *Config) { config.Redis.PasswordFile = "/run/secrets/${REDIS_PASSWORD}" },
		"missing PAT key":          func(config *Config) { config.Credentials.PATSigningKeyFile = "" },
		"implicit credential list": func(config *Config) { config.Credentials.Headers = nil },
		"authorization alias": func(config *Config) {
			config.Credentials.Headers = []CredentialHeaderConfig{{Name: "authorization", Type: "bearer"}}
		},
		"duplicate credential header": func(config *Config) {
			config.Credentials.Headers = []CredentialHeaderConfig{{Name: "X-Token", Type: "bearer"}, {Name: "x-token", Type: "basic"}}
		},
		"credential type": func(config *Config) {
			config.Credentials.Headers = []CredentialHeaderConfig{{Name: "X-Token", Type: "cookie"}}
		},
		"missing mapper contract": func(config *Config) { config.Mappers.Contract = "" },
		"unknown mapper contract": func(config *Config) { config.Mappers.Contract = "elitea.auth_mappers.custom.v1" },
		"implicit public rules":   func(config *Config) { config.Authorization.MainConfiguredPublicRules = nil },
		"duplicate public rule name": func(config *Config) {
			config.Authorization.MainConfiguredPublicRules[1].Name = config.Authorization.MainConfiguredPublicRules[0].Name
		},
		"empty public rule": func(config *Config) {
			config.Authorization.MainConfiguredPublicRules[0].Conditions = []PublicRuleConditionConfig{}
		},
		"unknown public field": func(config *Config) {
			config.Authorization.MainConfiguredPublicRules[0].Conditions[0].Field = "path"
		},
		"duplicate public field": func(config *Config) {
			config.Authorization.MainConfiguredPublicRules[0].Conditions = append(
				config.Authorization.MainConfiguredPublicRules[0].Conditions,
				config.Authorization.MainConfiguredPublicRules[0].Conditions[0],
			)
		},
		"invalid public regex": func(config *Config) {
			config.Authorization.MainConfiguredPublicRules[0].Conditions[0].Pattern = "["
		},
		"implicit admins": func(config *Config) { config.Identity.InitialGlobalAdmins = nil },
		"duplicate admin": func(config *Config) { config.Identity.InitialGlobalAdmins = []string{"admin", "admin"} },
		"project ID":      func(config *Config) { config.Identity.ProjectEnrollment.ProjectID = 0 },
		"allowed domains": func(config *Config) { config.Identity.ProjectEnrollment.AllowedDomains = "example.com,,internal" },
		"implicit roles":  func(config *Config) { config.Identity.ProjectEnrollment.AdditionalProjectRolesForGlobalAdmins = nil },
		"duplicate role": func(config *Config) {
			config.Identity.ProjectEnrollment.AdditionalProjectRolesForGlobalAdmins = []string{"admin", "admin"}
		},
		"provider kind":         func(config *Config) { config.Provider.Kind = "oidc" },
		"missing Form block":    func(config *Config) { config.Provider.Form = nil },
		"reused private secret": func(config *Config) { config.Redis.AttemptKeyFile = config.Credentials.PATSigningKeyFile },
		"reused CA reference":   func(config *Config) { config.Redis.CAFile = config.Credentials.PATSigningKeyFile },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := parsedValidConfig(t)
			mutate(&config)
			if err := config.Validate(); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestProjectEnrollmentMayBeDisabled(t *testing.T) {
	config := parsedValidConfig(t)
	config.Identity.ProjectEnrollment = nil
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
}

func FuzzParseNeverPanics(f *testing.F) {
	f.Add([]byte(validConfigYAML))
	f.Add([]byte("schema_version: elitea.auth.form.v1"))
	f.Add([]byte("&anchor [*anchor]"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = Parse(raw)
	})
}

func parsedValidConfig(t *testing.T) Config {
	t.Helper()
	config, err := Parse([]byte(validConfigYAML))
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func indent(value string, spaces int) string {
	prefix := strings.Repeat(" ", spaces)
	return prefix + strings.ReplaceAll(value, "\n", "\n"+prefix)
}

const validConfigYAML = `schema_version: elitea.auth.form.v1
public_origin: https://elitea.example
trusted_proxy_cidrs:
  - 10.0.0.0/8
  - 2001:db8::/32
redirects:
  direct_access_denied: /access_denied
  main_access_denied: /app/access_denied
  default_login: /
  default_logout: /
cookie:
  name: centry_auth_session
  same_site: lax
  lifetime_seconds: 604800
redis:
  topology: single_primary_endpoint
  url: rediss://elitea-auth@redis.example:6379/0
  password_file: /run/secrets/auth/redis-password
  ca_file: /run/config/auth/redis-ca.pem
  key_prefix: "centry:auth:v1:"
  attempt_key_file: /run/secrets/auth/attempt-hmac-key
credentials:
  pat_signing_key_file: /run/secrets/auth/pat-hs512-key
  credential_headers: []
mappers:
  contract: elitea.auth_mappers.tracked.v1
authorization:
  main_configured_public_rules:
    - name: config.edge_auth
      conditions:
        - field: uri
          pattern: '/auth/.*'
    - name: config.application_icon
      conditions:
        - field: uri
          pattern: '/applications/application_icon.*'
    - name: config.datasource_icon
      conditions:
        - field: uri
          pattern: '/datasources/datasource_icon.*'
    - name: config.prompt_icon
      conditions:
        - field: uri
          pattern: '/prompt_lib/prompt_icon.*'
identity:
  initial_global_admins:
    - admin
  project_enrollment:
    project_id: 1
    allowed_domains: centry.user
    additional_project_roles_for_global_admins:
      - system
      - admin
      - editor
      - viewer
      - prompt_lib_public
      - prompt_lib_moderators
      - public_admin
provider:
  kind: form
  form:
    users_json_file: /run/secrets/auth/form-users.json
`
