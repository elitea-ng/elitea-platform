package runtimecomposition

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

func codeOwnerConfigurationFixture() CodeOwnerConfig {
	return CodeOwnerConfig{MainWorkloadIdentity: "spiffe://elitea/main", CertificateChainPath: "/run/elitea-runtime/owner-client.crt", PrivateKeyPath: "/run/elitea-runtime/owner-client.key", ServerCAPath: "/run/elitea-runtime/runtime-ca.crt", Supervisors: []CodeOwnerSupervisor{{Audience: "spiffe://elitea/supervisor/one", HTTPSOrigin: "https://supervisor.invalid"}}}
}

// withCodeRuntime adds what every Agent-dispatch environment requires: the
// owner config (admitting its supervisor audience) and the three Code
// consumer settings.
func withCodeRuntime(env map[string]string) map[string]string {
	owner := codeOwnerConfigurationFixture()
	workspace := CodeWorkspaceConfig{Revision: 1, RepositoryCapabilities: []string{"github"}, EgressAllowlist: []string{"api.github.com:443"}, Policy: storage.DefaultCodeWorkspacePolicy()}
	platform := CodePlatformDeploymentConfig{Revision: 1, ContentKeysFile: "/run/elitea-runtime/code-platform-content-keys.json", BrokerPolicies: []storage.CodeBrokerPolicy{{Revision: 1, MaxCalls: 32, MaxTotalBytes: 1048576}}}
	for name, value := range map[string]any{
		"ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG": owner,
		"ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG":      workspace,
		"ELITEA_RUNTIME_CODE_PLATFORM_CONFIG":       platform,
	} {
		raw, _ := json.Marshal(value)
		env[name] = string(raw)
	}
	env[CodeDebugAgentStateDSNFileEnv] = "/run/elitea-runtime/agent-checkpoint-connection"
	audience := owner.Supervisors[0].Audience
	if current := env["ELITEA_RUNTIME_SANDBOX_AUDIENCES"]; current == "" {
		env["ELITEA_RUNTIME_SANDBOX_AUDIENCES"] = audience
	} else if !strings.Contains(","+current+",", ","+audience+",") {
		env["ELITEA_RUNTIME_SANDBOX_AUDIENCES"] = current + "," + audience
	}
	return env
}

func TestCodeOwnerRecoveryHasNoSwitchAndIsRequiredWithAgentDispatch(t *testing.T) {
	cfg := codeOwnerConfigurationFixture()
	raw, _ := json.Marshal(cfg)
	audiences := []string{cfg.Supervisors[0].Audience}
	env := map[string]string{}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	// No Agent dispatch, no owner config: nothing to own.
	actual, err := codeOwnerConfigFromEnv(lookup, false, false, nil)
	if err != nil || actual != nil {
		t.Fatal("owner work began without Agent dispatch", err)
	}
	actual, err = codeOwnerConfigFromEnv(lookup, true, false, audiences)
	if err != nil || actual != nil {
		t.Fatal("owner work began without Agent dispatch", err)
	}
	// Agent dispatch without the owner config: refused, naming the variable.
	_, err = codeOwnerConfigFromEnv(lookup, true, true, audiences)
	if err == nil || !strings.Contains(err.Error(), "ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG") {
		t.Fatalf("agents without owner config: %v", err)
	}
	// Agent dispatch with the config: owner recovery is configured.
	env["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG"] = string(raw)
	actual, err = codeOwnerConfigFromEnv(lookup, true, true, audiences)
	if err != nil || actual == nil || actual.MainWorkloadIdentity != cfg.MainWorkloadIdentity {
		t.Fatal(actual, err)
	}
	// The config without Agent dispatch: refused.
	for _, scope := range [][2]bool{{false, false}, {true, false}} {
		if _, err = codeOwnerConfigFromEnv(lookup, scope[0], scope[1], audiences); err == nil {
			t.Fatal("owner settings accepted without Agent dispatch", scope)
		}
	}
	// The removed switch is refused with any value, in every scope.
	for _, value := range []string{"true", "false", "", "yes"} {
		env["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED"] = value
		for _, scope := range [][2]bool{{false, false}, {true, false}, {true, true}} {
			_, err = codeOwnerConfigFromEnv(lookup, scope[0], scope[1], audiences)
			if err == nil || err.Error() != CodeOwnerRecoverySwitchRemoved {
				t.Fatalf("removed switch %q in scope %v: %v", value, scope, err)
			}
		}
	}
	delete(env, "ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED")
	env["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG"] = `{"main_workload_identity":"one","main_workload_identity":"two","certificate_chain_path":"c","private_key_path":"k","server_ca_path":"a","supervisors":[]}`
	if _, err = codeOwnerConfigFromEnv(lookup, true, true, nil); err == nil {
		t.Fatal("duplicate configured authority admitted")
	}
	for _, invalid := range []string{
		strings.Replace(string(raw), `"audience"`, `"AUDIENCE"`, 1),
		strings.Replace(string(raw), `"https_origin"`, `"HTTPS_ORIGIN"`, 1),
	} {
		env["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG"] = invalid
		if _, err = codeOwnerConfigFromEnv(lookup, true, true, []string{cfg.Supervisors[0].Audience}); err == nil {
			t.Fatal("aliased owner authority key admitted")
		}
	}
}
func TestCodeOwnerConfigurationRejectsForeignAndNonFixedOriginWithoutReadingKeys(t *testing.T) {
	valid := codeOwnerConfigurationFixture()
	allowed := []string{valid.Supervisors[0].Audience}
	for _, name := range []string{"runtime off", "agents off", "foreign audience", "duplicate audience", "http", "userinfo", "query", "empty query", "fragment", "path", "missing key path", "relative key path", "unclean key path"} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			cfg.Supervisors = append([]CodeOwnerSupervisor(nil), valid.Supervisors...)
			enabled, agents := true, true
			switch name {
			case "runtime off":
				enabled = false
			case "agents off":
				agents = false
			case "foreign audience":
				cfg.Supervisors[0].Audience = "spiffe://foreign/owner"
			case "duplicate audience":
				cfg.Supervisors = append(cfg.Supervisors, cfg.Supervisors[0])
			case "http":
				cfg.Supervisors[0].HTTPSOrigin = "http://supervisor.invalid"
			case "userinfo":
				cfg.Supervisors[0].HTTPSOrigin = "https://user:secret@supervisor.invalid"
			case "query":
				cfg.Supervisors[0].HTTPSOrigin = "https://supervisor.invalid?route=other"
			case "empty query":
				cfg.Supervisors[0].HTTPSOrigin = "https://supervisor.invalid?"
			case "fragment":
				cfg.Supervisors[0].HTTPSOrigin = "https://supervisor.invalid#other"
			case "path":
				cfg.Supervisors[0].HTTPSOrigin = "https://supervisor.invalid/execute"
			case "missing key path":
				cfg.PrivateKeyPath = ""
			case "relative key path":
				cfg.PrivateKeyPath = "owner-client.key"
			case "unclean key path":
				cfg.PrivateKeyPath = "/run/elitea-runtime/../owner-client.key"
			}
			if validateCodeOwnerConfig(&cfg, enabled, agents, allowed) == nil {
				t.Fatal("invalid owner config admitted")
			}
		})
	}
}

// The same rule at the process boundary (ConfigFromEnv) and on a composed Config.
func TestConfigComposesCodeOwnerRecoveryExactlyWithAgentDispatch(t *testing.T) {
	agents := withCodeRuntime(agentDispatchEnvironment())
	agents["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = "https://main.invalid"
	config, err := ConfigFromEnv(mapLookup(agents))
	if err != nil || config.CodeOwnerRecovery == nil {
		t.Fatalf("agents with owner config: %v", err)
	}

	missing := map[string]string{}
	for name, value := range agents {
		missing[name] = value
	}
	delete(missing, "ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG")
	if _, err := ConfigFromEnv(mapLookup(missing)); err == nil || !strings.Contains(err.Error(), "ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG") {
		t.Fatalf("agents without owner config: %v", err)
	}

	for _, value := range []string{"true", "false"} {
		agents["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED"] = value
		if _, err := ConfigFromEnv(mapLookup(agents)); err == nil || err.Error() != CodeOwnerRecoverySwitchRemoved {
			t.Fatalf("removed switch %q: %v", value, err)
		}
		// Also with the runtime off.
		if _, err := ConfigFromEnv(mapLookup(map[string]string{"ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED": value})); err == nil || err.Error() != CodeOwnerRecoverySwitchRemoved {
			t.Fatalf("removed switch %q with the runtime off: %v", value, err)
		}
	}
	delete(agents, "ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED")

	noAgents := withCodeRuntime(validEnvironment())
	if _, err := ConfigFromEnv(mapLookup(noAgents)); err == nil {
		t.Fatal("owner config accepted without Agent dispatch")
	}
	raw := noAgents["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG"]
	if _, err := ConfigFromEnv(mapLookup(map[string]string{"ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG": raw})); err == nil {
		t.Fatal("owner config accepted with the runtime off")
	}

	config.CodeOwnerRecovery = nil
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG") {
		t.Fatalf("composed Config with agents and no owner: %v", err)
	}
}
