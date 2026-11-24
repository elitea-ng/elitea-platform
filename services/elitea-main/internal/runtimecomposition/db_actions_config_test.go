package runtimecomposition

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
)

func TestConfigFromEnvRefusesMalformedDBEgressAllowlist(t *testing.T) {
	environment := validEnvironment()
	environment[egress.DBActionAllowlistEnv] = "10.0.0.0/33"
	if _, err := ConfigFromEnv(mapLookup(environment)); err == nil {
		t.Fatal("malformed ELITEA_DB_EGRESS_ALLOWLIST accepted")
	}
	environment[egress.DBActionAllowlistEnv] = "10.20.0.0/16, 192.168.4.7:5432"
	config, err := ConfigFromEnv(mapLookup(environment))
	if err != nil || config.DBEgressAllowlist != "10.20.0.0/16, 192.168.4.7:5432" {
		t.Fatalf("valid ELITEA_DB_EGRESS_ALLOWLIST = %+v, %v", config.DBEgressAllowlist, err)
	}
	delete(environment, egress.DBActionAllowlistEnv)
	if config, err := ConfigFromEnv(mapLookup(environment)); err != nil || config.DBEgressAllowlist != "" {
		t.Fatalf("empty ELITEA_DB_EGRESS_ALLOWLIST = %q, %v", config.DBEgressAllowlist, err)
	}
}
