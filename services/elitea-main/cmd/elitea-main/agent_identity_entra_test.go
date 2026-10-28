package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/agentid"
)

var entraAgentIDEnv = map[string]string{
	"ELITEA_ENTRA_AGENT_ID_TENANT_ID":               "aaaaaaaa-0000-4000-8000-000000000001",
	"ELITEA_ENTRA_AGENT_ID_BLUEPRINT_APP_ID":        "22222222-2222-2222-2222-222222222222",
	"ELITEA_ENTRA_AGENT_ID_FEDERATED_TOKEN_FILE":    "/var/run/secrets/azure/tokens/azure-identity-token",
	"ELITEA_ENTRA_AGENT_ID_ALLOWED_RESOURCE_SCOPES": "https://graph.microsoft.com/.default, https://graph.microsoft.com/Sites.Selected",
	"ELITEA_ENTRA_AGENT_ID_SPONSOR_GROUPS":          "default=dddddddd-0000-4000-8000-000000000004,7=dddddddd-0000-4000-8000-000000000005",
	"ELITEA_ENTRA_AGENT_ID_IDENTITY_LIMIT":          "200",
}

func entraAgentIDEnvWith(changes map[string]string) func(string) (string, bool) {
	values := map[string]string{}
	for k, v := range entraAgentIDEnv {
		values[k] = v
	}
	for k, v := range changes {
		values[k] = v
	}
	return providerOAuthEnv(values)
}

func dedicatedAgentRevision() profile.PolicyRevision {
	return profile.PolicyRevision{ID: "2026-10-r1", Digest: [32]byte{1},
		Document: profile.PolicyDocument{AllowedIdentityModes: []identity.IdentityMode{identity.IdentityModeDedicatedAgent}}}
}

// Flag off: the block is not read, even an invalid one, and nothing is built.
func TestEntraAgentIDBuildsNothingWithTheFlagOff(t *testing.T) {
	provider, source, err := entraAgentIdentityAtBoot(profile.LegacyAdmission(true), dedicatedAgentRevision(),
		func(string) (string, bool) {
			t.Fatal("the flag-off composition read the environment")
			return "", false
		}, nil, nil, slog.Default())
	if err != nil || provider != nil || source != nil {
		t.Fatalf("flag off built %v %v err=%v", provider, source, err)
	}
}

func TestEntraAgentIDBoot(t *testing.T) {
	revision := dedicatedAgentRevision()
	governed := profile.GovernedAdmission(revision)
	sessions := repos.NewAgentSessions(nil, nil)

	t.Run("no block builds nothing", func(t *testing.T) {
		provider, source, err := entraAgentIdentityAtBoot(governed, revision, providerOAuthEnv(nil), sessions, nil, slog.Default())
		if err != nil || provider != nil || source != nil {
			t.Fatalf("built %v %v err=%v", provider, source, err)
		}
	})
	t.Run("a valid env block builds the provider and the assertion source", func(t *testing.T) {
		provider, source, err := entraAgentIdentityAtBoot(governed, revision, entraAgentIDEnvWith(nil), sessions, nil, slog.Default())
		if err != nil || provider == nil || source == nil {
			t.Fatalf("built %v %v err=%v", provider, source, err)
		}
		if provider.Name() != "entra" || provider.Capabilities().IdentityLimit != 200 || provider.Capabilities().AcceptsAssertedUser {
			t.Fatalf("unexpected provider %v %+v", provider, provider.Capabilities())
		}
	})
	t.Run("a revision without dedicated_agent builds nothing", func(t *testing.T) {
		other := profile.PolicyRevision{ID: "r2", Document: profile.PolicyDocument{
			AllowedIdentityModes: []identity.IdentityMode{identity.IdentityModeEliteaNative}}}
		provider, source, err := entraAgentIdentityAtBoot(profile.GovernedAdmission(other), other, entraAgentIDEnvWith(nil), sessions, nil, slog.Default())
		if err != nil || provider != nil || source != nil {
			t.Fatalf("built %v %v err=%v", provider, source, err)
		}
	})
	// Acceptance: a tenant id of common or organizations, or a client
	// secret, refuses boot in the governed profile.
	for name, changes := range map[string]map[string]string{
		"common tenant":        {"ELITEA_ENTRA_AGENT_ID_TENANT_ID": "common"},
		"organizations tenant": {"ELITEA_ENTRA_AGENT_ID_TENANT_ID": "organizations"},
		"consumers tenant":     {"ELITEA_ENTRA_AGENT_ID_TENANT_ID": "consumers"},
		"client secret":        {"ELITEA_ENTRA_AGENT_ID_CLIENT_SECRET": "s3cr3t-value"},
		"certificate":          {"ELITEA_ENTRA_AGENT_ID_CERTIFICATE_FILE": "/etc/agentid/cert.pem"},
		"no token file":        {"ELITEA_ENTRA_AGENT_ID_FEDERATED_TOKEN_FILE": ""},
		"bad limit":            {"ELITEA_ENTRA_AGENT_ID_IDENTITY_LIMIT": "many"},
		"bad sponsor pair":     {"ELITEA_ENTRA_AGENT_ID_SPONSOR_GROUPS": "dddddddd-0000-4000-8000-000000000004"},
		"env and file":         {"ELITEA_ENTRA_AGENT_ID_CONFIG_FILE": "/etc/agentid.json"},
	} {
		t.Run(name+" refuses boot", func(t *testing.T) {
			provider, _, err := entraAgentIdentityAtBoot(governed, revision, entraAgentIDEnvWith(changes), sessions, nil, slog.Default())
			if !errors.Is(err, profile.ErrBootRefused) || provider != nil {
				t.Fatalf("booted: %v err=%v", provider, err)
			}
			if strings.Contains(err.Error(), "s3cr3t-value") {
				t.Fatal("the refusal echoed the secret")
			}
		})
	}
	t.Run("a session store is required", func(t *testing.T) {
		if _, _, err := entraAgentIdentityAtBoot(governed, revision, entraAgentIDEnvWith(nil), nil, nil, slog.Default()); !errors.Is(err, profile.ErrBootRefused) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestEntraAgentIDFileBlock(t *testing.T) {
	revision := dedicatedAgentRevision()
	governed := profile.GovernedAdmission(revision)
	sessions := repos.NewAgentSessions(nil, nil)
	write := func(t *testing.T, body string) func(string) (string, bool) {
		path := filepath.Join(t.TempDir(), "agentid.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return providerOAuthEnv(map[string]string{"ELITEA_ENTRA_AGENT_ID_CONFIG_FILE": path})
	}
	valid := `{"entra":{"agentid":{"tenant_id":"aaaaaaaa-0000-4000-8000-000000000001",
		"blueprint_app_id":"22222222-2222-2222-2222-222222222222",
		"federated_token_file":"/var/run/secrets/azure/tokens/azure-identity-token",
		"allowed_resource_scopes":["https://graph.microsoft.com/.default"],
		"sponsor_groups":{"default":"dddddddd-0000-4000-8000-000000000004"}}}}`
	provider, source, err := entraAgentIdentityAtBoot(governed, revision, write(t, valid), sessions, nil, slog.Default())
	if err != nil || provider == nil || source == nil {
		t.Fatalf("built %v %v err=%v", provider, source, err)
	}
	if provider.Capabilities().IdentityLimit != agentid.DefaultIdentityLimit {
		t.Fatalf("identity limit %d", provider.Capabilities().IdentityLimit)
	}
	for name, body := range map[string]string{
		"client secret":    strings.Replace(valid, `"sponsor_groups"`, `"client_secret":"s3cr3t-value","sponsor_groups"`, 1),
		"organizations":    strings.Replace(valid, "aaaaaaaa-0000-4000-8000-000000000001", "organizations", 1),
		"unknown field":    strings.Replace(valid, `"sponsor_groups"`, `"secret":"x","sponsor_groups"`, 1),
		"no agentid block": `{"entra":{}}`,
		"bad sponsor key":  strings.Replace(valid, `"default"`, `"project-7"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := entraAgentIdentityAtBoot(governed, revision, write(t, body), sessions, nil, slog.Default())
			if !errors.Is(err, profile.ErrBootRefused) {
				t.Fatalf("booted: err=%v", err)
			}
			if strings.Contains(err.Error(), "s3cr3t-value") {
				t.Fatal("the refusal echoed the secret")
			}
		})
	}
}
