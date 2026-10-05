package runtimecomposition

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

func codeConsumerEnvironment(t *testing.T) map[string]string {
	t.Helper()
	owner := codeOwnerConfigurationFixture()
	workspace := CodeWorkspaceConfig{Revision: 1, RepositoryCapabilities: []string{"github"}, EgressAllowlist: []string{"api.github.com:443"}, Policy: storage.DefaultCodeWorkspacePolicy()}
	platform := CodePlatformDeploymentConfig{Revision: 1, ContentKeysFile: "/run/elitea-runtime/code-platform-content-keys.json", BrokerPolicies: []storage.CodeBrokerPolicy{{Revision: 1, MaxCalls: 32, MaxTotalBytes: 1048576}}}
	values := oneDirectoryEnvironment()
	values["ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED"] = "true"
	values["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = "https://main.invalid"
	values["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = "commands.v1.agent.execute.agents.shared.1.0"
	values["ELITEA_RUNTIME_AGENT_EXECUTION_CONSUMER_GROUP"] = "elitea-agent-worker-v1"
	values["ELITEA_RUNTIME_AGENT_EXECUTION_STREAM_MAX_ENTRIES"] = "64"
	values["ELITEA_RUNTIME_SANDBOX_AUDIENCES"] = owner.Supervisors[0].Audience
	values["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED"] = "true"
	values["ELITEA_RUNTIME_CODE_WORKSPACE_ENABLED"] = "true"
	values["ELITEA_RUNTIME_CODE_PLATFORM_ENABLED"] = "true"
	values["ELITEA_RUNTIME_CODE_DEBUG_ARTIFACTS_ENABLED"] = "true"
	values[CodeDebugAgentStateDSNFileEnv] = "/run/elitea-runtime/agent-checkpoint-connection"
	for name, value := range map[string]any{
		"ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG": owner,
		"ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG":      workspace,
		"ELITEA_RUNTIME_CODE_PLATFORM_CONFIG":       platform,
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		values[name] = string(raw)
	}
	return values
}

func TestCodeConsumersDefaultOffAndRequireExplicitOwner(t *testing.T) {
	config, err := ConfigFromEnv(mapLookup(nil))
	if err != nil || config.CodeWorkspace != nil || config.CodePlatform != nil || config.CodeDebugArtifacts != nil {
		t.Fatal("default changed Code consumer composition", err)
	}
	for _, feature := range []string{"WORKSPACE", "PLATFORM", "DEBUG_ARTIFACTS"} {
		for _, flag := range []string{"true", "yes", "TRUE", " true"} {
			values := map[string]string{"ELITEA_RUNTIME_CODE_" + feature + "_ENABLED": flag}
			if _, err := ConfigFromEnv(mapLookup(values)); err == nil {
				t.Fatal("unowned or malformed enablement was accepted", feature, flag)
			}
		}
	}
	for _, name := range []string{"ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG", "ELITEA_RUNTIME_CODE_PLATFORM_CONFIG", CodeDebugAgentStateDSNFileEnv} {
		if _, err := ConfigFromEnv(mapLookup(map[string]string{name: "/missing/material"})); err == nil {
			t.Fatal("disabled consumer settings were accepted", name)
		}
	}
	values := codeConsumerEnvironment(t)
	delete(values, "ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED")
	delete(values, "ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG")
	if _, err := ConfigFromEnv(mapLookup(values)); err == nil {
		t.Fatal("consumers enabled without original owner")
	}
}

func TestCodeConsumersStrictTypedOperatorConfiguration(t *testing.T) {
	values := codeConsumerEnvironment(t)
	config, err := ConfigFromEnv(mapLookup(values))
	if err != nil || config.CodeWorkspace == nil || config.CodePlatform == nil || config.CodeDebugArtifacts == nil {
		t.Fatal("valid frozen operator configuration was refused", err)
	}
	cases := []struct {
		name, key, old, replacement string
	}{
		{"unknown workspace key", "ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG", `"revision":1`, `"revision":1,"token":"forbidden"`},
		{"duplicate workspace key", "ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG", `"revision":1`, `"revision":1,"revision":1`},
		{"unknown workspace policy key", "ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG", `"max_files":1024`, `"max_files":1024,"network":true`},
		{"aliased workspace policy key", "ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG", `"max_files":1024`, `"MAX_FILES":1024`},
		{"null workspace policy", "ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG", `"max_files":1024`, `"max_files":null`},
		{"unsupported repository capability", "ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG", `"github"`, `"gitlab"`},
		{"unbounded workspace files", "ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG", `"max_files":1024`, `"max_files":4097`},
		{"unknown platform policy key", "ELITEA_RUNTIME_CODE_PLATFORM_CONFIG", `"max_calls":32`, `"max_calls":32,"credential_scope":"all"`},
		{"aliased platform policy key", "ELITEA_RUNTIME_CODE_PLATFORM_CONFIG", `"max_calls":32`, `"MAX_CALLS":32`},
		{"duplicate platform policy key", "ELITEA_RUNTIME_CODE_PLATFORM_CONFIG", `"max_calls":32`, `"max_calls":32,"max_calls":32`},
		{"unbounded platform calls", "ELITEA_RUNTIME_CODE_PLATFORM_CONFIG", `"max_calls":32`, `"max_calls":4097`},
		{"unknown platform top key", "ELITEA_RUNTIME_CODE_PLATFORM_CONFIG", `"revision":1`, `"revision":1,"token":"forbidden"`},
		{"relative content key path", "ELITEA_RUNTIME_CODE_PLATFORM_CONFIG", `/run/elitea-runtime/code-platform-content-keys.json`, `content-keys.json`},
		{"unclean content key path", "ELITEA_RUNTIME_CODE_PLATFORM_CONFIG", `/run/elitea-runtime/code-platform-content-keys.json`, `/run/../content-keys.json`},
		{"private keys alias public verifier", "ELITEA_RUNTIME_CODE_PLATFORM_CONFIG", `/run/elitea-runtime/code-platform-content-keys.json`, `/run/elitea-runtime/command-signing-keyring.json`},
		{"private keys alias debug DSN", "ELITEA_RUNTIME_CODE_PLATFORM_CONFIG", `/run/elitea-runtime/code-platform-content-keys.json`, `/run/elitea-runtime/agent-checkpoint-connection`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			values := codeConsumerEnvironment(t)
			values[test.key] = strings.Replace(values[test.key], test.old, test.replacement, 1)
			if _, err := ConfigFromEnv(mapLookup(values)); err == nil {
				t.Fatal("unsafe or unknown operator setting accepted")
			}
		})
	}
	for _, path := range []string{"", "relative", "/run/../dsn", "/run/dsn\n", "/run/dsn\x00"} {
		values := codeConsumerEnvironment(t)
		values[CodeDebugAgentStateDSNFileEnv] = path
		if _, err := ConfigFromEnv(mapLookup(values)); err == nil {
			t.Fatal("unsafe debug material path accepted", path)
		}
	}
}

func TestCodeWorkspaceEgressRequiresExactCanonicalDestinations(t *testing.T) {
	config := CodeWorkspaceConfig{Revision: 1, RepositoryCapabilities: []string{"github"}, EgressAllowlist: []string{"api.github.com:443"}, Policy: storage.DefaultCodeWorkspacePolicy()}
	for _, entries := range [][]string{nil, {""}, {"*.github.com:443"}, {"10.0.0.0/8"}, {"api.github.com"},
		{"https://api.github.com:443"}, {"api.github.com:0443"}, {"api.github.com:0"}, {"API.GITHUB.COM:443"},
		{"api.github.com:443", "api.github.com:443"}, {"api.github.com:443/path"}, {"bad_name:443"},
		{"127.0.0.1:443"}, {"169.254.169.254:443"}, {"[::1]:443"}, {"api.github.com:443\n"}} {
		config.EgressAllowlist = entries
		if config.Validate() == nil {
			t.Fatal("unsafe egress policy accepted", entries)
		}
	}
	for _, entries := range [][]string{{"api.github.com:443"}, {"github.enterprise:8443"}, {"10.1.2.3:443"}} {
		config.EgressAllowlist = entries
		capabilities, closeOwner, err := config.OpenCapabilities()
		if err != nil || capabilities == nil || closeOwner == nil {
			t.Fatal("explicit exact egress policy refused", entries, err)
		}
		closeOwner()
		closeOwner()
	}
}

func TestCodeConsumerMaterialInventoryAndSingleDirectoryOwner(t *testing.T) {
	config, err := ConfigFromEnv(mapLookup(codeConsumerEnvironment(t)))
	if err != nil {
		t.Fatal(err)
	}
	files, err := config.MaterialFiles()
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]securefile.Permissions{}
	for _, file := range files {
		if _, duplicate := actual[file.Path]; duplicate {
			t.Fatal("material file has duplicate owners", file.Path)
		}
		actual[file.Path] = file.Permissions
	}
	for path, expected := range map[string]securefile.Permissions{
		config.CodeOwnerRecovery.CertificateChainPath: securefile.PublicMaterial,
		config.CodeOwnerRecovery.PrivateKeyPath:       securefile.PrivateMaterial,
		config.CodeOwnerRecovery.ServerCAPath:         securefile.PublicMaterial,
		config.CodePlatform.ContentKeysFile:           securefile.PrivateMaterial,
		config.CodeDebugArtifacts.AgentStateDSNFile:   securefile.PrivateMaterial,
	} {
		if actual[path] != expected {
			t.Fatal("Code material has the wrong permission profile", path)
		}
	}
	if directory, err := config.MaterialDirectory(); err != nil || directory != "/run/elitea-runtime" {
		t.Fatal("shared material owner failed", directory, err)
	}
	config.CodePlatform.ContentKeysFile = "/run/elsewhere/content-keys.json"
	if _, err := config.MaterialDirectory(); err == nil {
		t.Fatal("Code material escaped the shared material directory")
	}
}

func TestConfiguredCodeConsumersRefuseAbsentDependencies(t *testing.T) {
	config, err := ConfigFromEnv(mapLookup(codeConsumerEnvironment(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := configureCodeSourceCapture(config, Dependencies{}); err == nil {
		t.Fatal("configured Code consumers were silently omitted")
	}
	if err := validateCodeConsumerDependencies(Config{}, Dependencies{CodePlatform: &CodePlatformConfig{Enabled: true}}); err == nil {
		t.Fatal("Code consumers enabled without object storage")
	}
}
