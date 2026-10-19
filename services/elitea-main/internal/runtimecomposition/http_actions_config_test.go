package runtimecomposition

import (
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/egress"
)

// Fail-closed startup: a malformed HTTP action allowlist refuses the runtime
// configuration instead of running with a wider or narrower bound.
func TestConfigFromEnvRefusesMalformedHTTPActionAllowlist(t *testing.T) {
	environment := validEnvironment()
	environment[egress.HTTPActionAllowlistEnv] = "10.0.0.0/33"
	if _, err := ConfigFromEnv(mapLookup(environment)); err == nil {
		t.Fatal("malformed ELITEA_HTTP_ACTION_EGRESS_ALLOWLIST accepted")
	}
	environment[egress.HTTPActionAllowlistEnv] = "10.20.0.0/16, 192.168.4.7:8443"
	config, err := ConfigFromEnv(mapLookup(environment))
	if err != nil || config.HTTPActionEgressAllowlist != "10.20.0.0/16, 192.168.4.7:8443" {
		t.Fatalf("valid allowlist refused: %v", err)
	}
	delete(environment, egress.HTTPActionAllowlistEnv)
	if config, err := ConfigFromEnv(mapLookup(environment)); err != nil || config.HTTPActionEgressAllowlist != "" {
		t.Fatalf("unset allowlist (public only) refused: %v", err)
	}
}
