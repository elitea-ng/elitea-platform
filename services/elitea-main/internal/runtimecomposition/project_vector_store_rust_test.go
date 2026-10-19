package runtimecomposition

import "testing"

// ADR-0031 decision 5: under the Rust indexing runtime no project pgvector
// database is provisioned. The composition root reads this decision from the
// Config, so the provisioner gets no vector store and skips project_pgvector.
// Since the Python worker's removal the Rust runtime is the only one, so the
// decision follows index ingest dispatch alone.
func TestProjectVectorStoreIsNotProvisionedUnderTheRustIndexingRuntime(t *testing.T) {
	for _, tc := range []struct {
		name        string
		config      Config
		rust        bool
		provisioned bool
	}{
		{"index dispatch on", Config{Enabled: true, IndexIngestDispatchEnabled: true}, true, false},
		{"index dispatch off", Config{Enabled: true}, false, true},
		{"runtime disabled", Config{}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.config.RustIndexing(); got != tc.rust {
				t.Fatalf("RustIndexing = %v, want %v", got, tc.rust)
			}
			if got := tc.config.ProjectVectorStoreProvisioned(); got != tc.provisioned {
				t.Fatalf("ProjectVectorStoreProvisioned = %v, want %v", got, tc.provisioned)
			}
		})
	}

	// The same decision from the environment, as main reads it, with and
	// without the (now optional) explicit rust value.
	for _, explicit := range []bool{true, false} {
		environment := validEnvironment()
		if explicit {
			environment[IndexingRuntimeEnv] = "rust"
		}
		environment["ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"] = "true"
		environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "ELITEA_RT_V1_INDEX"
		config, err := ConfigFromEnv(mapLookup(environment))
		if err != nil {
			t.Fatal(err)
		}
		if !config.RustIndexing() || config.ProjectVectorStoreProvisioned() {
			t.Fatalf("explicit=%v: an indexing environment provisions a project vector store: %+v", explicit, config)
		}
	}
}
