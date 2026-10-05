package runtimecomposition

import (
	"encoding/json"
	"strings"
	"testing"
)

func codeOwnerConfigurationFixture() CodeOwnerConfig {
	return CodeOwnerConfig{MainWorkloadIdentity: "spiffe://elitea/main", CertificateChainPath: "/run/elitea-runtime/owner-client.crt", PrivateKeyPath: "/run/elitea-runtime/owner-client.key", ServerCAPath: "/run/elitea-runtime/runtime-ca.crt", Supervisors: []CodeOwnerSupervisor{{Audience: "spiffe://elitea/supervisor/one", HTTPSOrigin: "https://supervisor.invalid"}}}
}
func TestCodeOwnerConfigurationDefaultAndExplicitScope(t *testing.T) {
	cfg := codeOwnerConfigurationFixture()
	raw, _ := json.Marshal(cfg)
	env := map[string]string{}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	actual, err := codeOwnerConfigFromEnv(lookup, false, false, nil)
	if err != nil || actual != nil {
		t.Fatal("default began owner work")
	}
	env["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG"] = string(raw)
	if _, err = codeOwnerConfigFromEnv(lookup, false, false, nil); err == nil {
		t.Fatal("owner settings accepted without explicit enablement")
	}
	env["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED"] = "true"
	if _, err = codeOwnerConfigFromEnv(lookup, false, false, nil); err == nil {
		t.Fatal("owner enabled without owning runtime")
	}
	actual, err = codeOwnerConfigFromEnv(lookup, true, true, []string{cfg.Supervisors[0].Audience})
	if err != nil || actual == nil || actual.MainWorkloadIdentity != cfg.MainWorkloadIdentity {
		t.Fatal(actual, err)
	}
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
