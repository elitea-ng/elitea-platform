package runtimecomposition

import (
	"context"
	"strings"
	"testing"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

type nopMetaWriter struct{}

func (nopMetaWriter) MaterializeInitial(
	_ context.Context, _ indexingapp.CurrentIndexMetaTarget, _ indexingapp.CurrentInitialIndexMeta,
) error {
	return nil
}

// ELITEA_INDEXING_RUNTIME picks one metadata store for the whole index runtime.
// Each mode requires its own dependency and refuses the other's, so a runtime
// cannot start half on the pgvector index_meta rows and half on the registry.
func TestIndexRuntimeRefusesAMetadataStoreThatDoesNotMatchTheMode(t *testing.T) {
	registry := &indexRegistryComposition{repo: &repos.IndexRegistryRepository{}}
	writer := nopMetaWriter{}
	cases := []struct {
		name     string
		runtime  string
		writer   indexingapp.CurrentIndexMetaWriter
		registry *indexRegistryComposition
	}{
		{"python with the registry", IndexingRuntimePython, writer, registry},
		{"python with the registry only", IndexingRuntimePython, nil, registry},
		{"default with no store", "", nil, nil},
		{"rust with the python writer", IndexingRuntimeRust, writer, registry},
		{"rust with the python writer only", IndexingRuntimeRust, writer, nil},
		{"rust with no registry", IndexingRuntimeRust, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newCurrentIndexRuntime(
				nil, nil, Config{IndexIngestDispatchEnabled: true, IndexingRuntime: tc.runtime},
				repos.IndexIngestDispatchPolicy{}, tc.writer, tc.registry, func(error) {},
			)
			if err == nil || !strings.Contains(err.Error(), "does not match the indexing runtime") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// With a matching store the constructor goes on to the ordinary dependency
// checks (here: a nil pool), which proves the mode check is not what stops a
// correctly composed runtime.
func TestIndexRuntimeAcceptsTheMatchingStoreInEachMode(t *testing.T) {
	for _, tc := range []struct {
		runtime  string
		writer   indexingapp.CurrentIndexMetaWriter
		registry *indexRegistryComposition
	}{
		{IndexingRuntimePython, nopMetaWriter{}, nil},
		{"", nopMetaWriter{}, nil},
		{IndexingRuntimeRust, nil, &indexRegistryComposition{repo: &repos.IndexRegistryRepository{}}},
	} {
		_, err := newCurrentIndexRuntime(
			nil, nil, Config{IndexIngestDispatchEnabled: true, IndexingRuntime: tc.runtime},
			repos.IndexIngestDispatchPolicy{}, tc.writer, tc.registry, func(error) {},
		)
		if err == nil || !strings.Contains(err.Error(), "dependencies are required") {
			t.Fatalf("runtime %q: err = %v, want the dependency check", tc.runtime, err)
		}
	}
}
