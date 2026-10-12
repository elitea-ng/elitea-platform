package runtimecomposition

import "testing"

// ADR-0031 decision 5: under the Rust indexing runtime no project pgvector
// database is provisioned. The composition root reads this decision from the
// Config, so the provisioner gets no vector store and skips project_pgvector,
// and a pgvector bootstrap is not a requirement of a rust deployment. Every
// other shape (python indexing, the index dispatch off, the runtime off) keeps
// provisioning exactly as before.
func TestProjectVectorStoreIsNotProvisionedUnderTheRustIndexingRuntime(t *testing.T) {
	for _, tc := range []struct {
		name        string
		config      Config
		rust        bool
		provisioned bool
	}{
		{"rust indexing", Config{Enabled: true, IndexIngestDispatchEnabled: true, IndexingRuntime: IndexingRuntimeRust}, true, false},
		{"python indexing", Config{Enabled: true, IndexIngestDispatchEnabled: true, IndexingRuntime: IndexingRuntimePython}, false, true},
		{"default runtime", Config{Enabled: true, IndexIngestDispatchEnabled: true}, false, true},
		// ConfigFromEnv refuses rust without the dispatch; the method stays
		// conservative on a hand-built Config anyway.
		{"rust named without the dispatch", Config{Enabled: true, IndexingRuntime: IndexingRuntimeRust}, false, true},
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

	// The same decision from the environment, as main reads it.
	environment := validEnvironment()
	environment[IndexingRuntimeEnv] = "rust"
	environment["ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"] = "true"
	environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "ELITEA_RT_V1_INDEX"
	config, err := ConfigFromEnv(mapLookup(environment))
	if err != nil {
		t.Fatal(err)
	}
	if !config.RustIndexing() || config.ProjectVectorStoreProvisioned() {
		t.Fatalf("a rust environment provisions a project vector store: %+v", config)
	}
	delete(environment, IndexingRuntimeEnv)
	config, err = ConfigFromEnv(mapLookup(environment))
	if err != nil {
		t.Fatal(err)
	}
	if config.RustIndexing() || !config.ProjectVectorStoreProvisioned() {
		t.Fatalf("a python environment stopped provisioning a project vector store: %+v", config)
	}
}
