package runtimecomposition

import "testing"

func TestDiscoveryActivationDefaultsClosedAndRequiresRuntime(t *testing.T) {
	env := validEnvironment()
	config, err := ConfigFromEnv(mapLookup(env))
	if err != nil || config.ToolkitDiscoveryEnabled {
		t.Fatal("discovery unexpectedly active")
	}
	env["ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED"] = "true"
	if _, err := ConfigFromEnv(mapLookup(env)); err == nil {
		t.Fatal("discovery enabled without input runtime")
	}
	env["ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"] = "true"
	env["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "ELITEA_RT_V1_INDEX"
	config, err = ConfigFromEnv(mapLookup(env))
	if err != nil || !config.ToolkitDiscoveryEnabled {
		t.Fatalf("rehearsal selection rejected: %v", err)
	}
	env["ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED"] = "yes"
	if _, err := ConfigFromEnv(mapLookup(env)); err == nil {
		t.Fatal("noncanonical flag accepted")
	}
}
