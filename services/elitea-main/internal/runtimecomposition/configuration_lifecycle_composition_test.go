package runtimecomposition

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentConfigurationLifecycleCompositionRejectsPartialGraph(t *testing.T) {
	pool := &pgxpool.Pool{}
	complete := currentConfigurationLifecycleCompositionRuntime()

	tests := []struct {
		name           string
		pool           *pgxpool.Pool
		configurations *CurrentConfigurationsRuntime
	}{
		{name: "database", configurations: complete},
		{name: "configurations", pool: pool},
		{name: "public project", pool: pool, configurations: &CurrentConfigurationsRuntime{
			models: complete.models, expander: complete.expander, unsecreter: complete.unsecreter,
		}},
		{name: "model catalog", pool: pool, configurations: &CurrentConfigurationsRuntime{
			publicProjectID: 1, expander: complete.expander, unsecreter: complete.unsecreter,
		}},
		// The resolution graph is what gates status_ok now, so a Configurations
		// runtime without it must not compose a lifecycle that would mark every
		// provider row usable without ever resolving it.
		{name: "expander", pool: pool, configurations: &CurrentConfigurationsRuntime{
			publicProjectID: 1, models: complete.models, unsecreter: complete.unsecreter,
		}},
		{name: "unsecreter", pool: pool, configurations: &CurrentConfigurationsRuntime{
			publicProjectID: 1, models: complete.models, expander: complete.expander,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NewCurrentConfigurationLifecycleReconciler(
				test.pool,
				test.configurations,
				true,
			)
			if err == nil || got != nil {
				t.Fatalf("reconciler=%#v error=%v", got, err)
			}
		})
	}
}

// The composition package must not reach for a LiteLLM data plane at all — not
// for the lifecycle (which used to push every configuration into the proxy) and
// not for /llm (which used to compose the facade). The Bifrost gateway pulls the
// same configuration rows per project, so both packages that existed only to
// push into LiteLLM are deleted.
//
// This is asserted over the package SOURCE rather than over a constructor
// signature, because the previous version of this test could only name types
// that still existed: once CurrentLLMRuntime was deleted it could not compile,
// let alone catch a reintroduction. An import scan keeps failing no matter which
// new symbol a reintroduction is hidden behind.
func TestRuntimeCompositionHasNoLiteLLMDataPlane(t *testing.T) {
	forbidden := []string{
		"internal/infra/litellm",
		"internal/api/llmproxy",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	scanned := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		scanned++
		for _, imported := range file.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", entry.Name(), err)
			}
			for _, banned := range forbidden {
				if strings.HasSuffix(path, banned) {
					t.Errorf("%s imports %s — the LiteLLM data plane is removed; /llm is served by the Bifrost gateway proxy", entry.Name(), path)
				}
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no Go files — the scan is not actually reading this package")
	}
}

func currentConfigurationLifecycleCompositionRuntime() *CurrentConfigurationsRuntime {
	return &CurrentConfigurationsRuntime{
		publicProjectID: 1,
		models:          &configurationapp.CurrentModelCatalogService{},
		expander:        &configurationapp.CurrentExpansionService{},
		unsecreter:      &storage.CurrentVaultUnsecreter{},
	}
}
