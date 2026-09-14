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
	env["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "commands.v1.index.ingest.indexing.shared.1.0"
	env["ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP"] = "elitea-indexer-worker-v1"
	env["ELITEA_RUNTIME_INDEX_INGEST_STREAM_MAX_ENTRIES"] = "64"
	config, err = ConfigFromEnv(mapLookup(env))
	if err != nil || !config.ToolkitDiscoveryEnabled {
		t.Fatalf("rehearsal selection rejected: %v", err)
	}
	env["ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED"] = "yes"
	if _, err := ConfigFromEnv(mapLookup(env)); err == nil {
		t.Fatal("noncanonical flag accepted")
	}
}
