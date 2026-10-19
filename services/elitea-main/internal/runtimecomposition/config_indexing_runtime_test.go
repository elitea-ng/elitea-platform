package runtimecomposition

import (
	"strings"
	"testing"
)

// The Rust runtime is the only indexing implementation. ELITEA_INDEXING_RUNTIME
// is still read so that a deployment pinned to the retired "python" (or a typo)
// is refused at startup rather than silently switched to another store.
func TestConfigIndexingRuntimeAcceptsRustOrUnsetAndRefusesTheRest(t *testing.T) {
	for _, value := range []string{"", IndexingRuntimeRust} {
		environment := validEnvironment()
		environment[IndexingRuntimeEnv] = value
		if _, err := ConfigFromEnv(mapLookup(environment)); err != nil {
			t.Fatalf("indexing runtime %q refused: %v", value, err)
		}
	}
	for _, bad := range []string{"python", "Rust", "go", "true", " rust"} {
		environment := validEnvironment()
		environment[IndexingRuntimeEnv] = bad
		if _, err := ConfigFromEnv(mapLookup(environment)); err == nil {
			t.Fatalf("indexing runtime %q was accepted", bad)
		}
	}
	environment := validEnvironment()
	environment[IndexingRuntimeEnv] = "python"
	if _, err := ConfigFromEnv(mapLookup(environment)); err == nil ||
		!strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("the retired python value is not refused with its reason: %v", err)
	}
	// Even a disabled runtime refuses the retired value or a typo, so it cannot
	// hide behind a switch that is off today.
	for _, bad := range []string{"python", "ruts"} {
		if _, err := ConfigFromEnv(mapLookup(map[string]string{IndexingRuntimeEnv: bad})); err == nil {
			t.Fatalf("indexing runtime %q accepted while the runtime is disabled", bad)
		}
	}
}

// Indexing is native exactly when index ingest dispatch is composed.
func TestConfigRustIndexingFollowsIndexIngestDispatch(t *testing.T) {
	environment := validEnvironment()
	config, err := ConfigFromEnv(mapLookup(environment))
	if err != nil {
		t.Fatal(err)
	}
	if config.RustIndexing() {
		t.Fatal("RustIndexing without index ingest dispatch")
	}
	environment["ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"] = "true"
	environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "ELITEA_RT_V1_INDEX"
	config, err = ConfigFromEnv(mapLookup(environment))
	if err != nil {
		t.Fatal(err)
	}
	if !config.RustIndexing() {
		t.Fatal("index ingest dispatch on, but RustIndexing is false")
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("validate rust config: %v", err)
	}
}
