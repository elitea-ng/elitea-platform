package runtimecomposition

import (
	"strings"
	"testing"
)

func TestConfigIndexingRuntimeDefaultsToPythonAndRefusesNonsense(t *testing.T) {
	config, err := ConfigFromEnv(mapLookup(validEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if config.IndexingRuntime != IndexingRuntimePython {
		t.Fatalf("default indexing runtime = %q, want python", config.IndexingRuntime)
	}

	for _, bad := range []string{"Rust", "go", "true", " rust"} {
		environment := validEnvironment()
		environment[IndexingRuntimeEnv] = bad
		if _, err := ConfigFromEnv(mapLookup(environment)); err == nil {
			t.Fatalf("indexing runtime %q was accepted", bad)
		}
	}
	// Even a disabled runtime refuses a typo, so a misspelt value cannot hide
	// behind a switch that is off today.
	if _, err := ConfigFromEnv(mapLookup(map[string]string{IndexingRuntimeEnv: "ruts"})); err == nil {
		t.Fatal("misspelt indexing runtime accepted while the runtime is disabled")
	}
}

func TestConfigIndexingRuntimeRustRequiresIndexIngestDispatch(t *testing.T) {
	environment := validEnvironment()
	environment[IndexingRuntimeEnv] = "rust"
	if _, err := ConfigFromEnv(mapLookup(environment)); err == nil ||
		!strings.Contains(err.Error(), "index ingest dispatch") {
		t.Fatalf("rust indexing without index dispatch was accepted: %v", err)
	}
	if _, err := ConfigFromEnv(mapLookup(map[string]string{IndexingRuntimeEnv: "rust"})); err == nil {
		t.Fatal("rust indexing with the runtime disabled was accepted")
	}

	environment["ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"] = "true"
	environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "ELITEA_RT_V1_INDEX"
	config, err := ConfigFromEnv(mapLookup(environment))
	if err != nil {
		t.Fatal(err)
	}
	if config.IndexingRuntime != IndexingRuntimeRust {
		t.Fatalf("indexing runtime = %q, want rust", config.IndexingRuntime)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("validate rust config: %v", err)
	}
	config.IndexIngestDispatchEnabled = false
	config.IndexIngestCommandStream = ""
	if err := config.Validate(); err == nil {
		t.Fatal("Validate accepted rust indexing without index dispatch")
	}
}
