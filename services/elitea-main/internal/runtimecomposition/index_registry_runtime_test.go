package runtimecomposition

import (
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// The index registry is the one metadata store of the index runtime; without
// it the runtime cannot start.
func TestIndexRuntimeRefusesAMissingRegistry(t *testing.T) {
	_, err := newCurrentIndexRuntime(
		nil, nil, Config{IndexIngestDispatchEnabled: true},
		repos.IndexIngestDispatchPolicy{}, nil, func(error) {},
	)
	if err == nil || !strings.Contains(err.Error(), "needs the composed index registry") {
		t.Fatalf("err = %v", err)
	}
}

// With the registry the constructor goes on to the ordinary dependency checks
// (here: a nil pool), which proves the store check is not what stops a
// correctly composed runtime.
func TestIndexRuntimeAcceptsTheRegistry(t *testing.T) {
	_, err := newCurrentIndexRuntime(
		nil, nil, Config{IndexIngestDispatchEnabled: true},
		repos.IndexIngestDispatchPolicy{}, &indexRegistryComposition{repo: &repos.IndexRegistryRepository{}}, func(error) {},
	)
	if err == nil || !strings.Contains(err.Error(), "dependencies are required") {
		t.Fatalf("err = %v, want the dependency check", err)
	}
}
