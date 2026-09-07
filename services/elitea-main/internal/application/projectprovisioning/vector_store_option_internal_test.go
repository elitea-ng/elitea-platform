package projectprovisioning

// The project_pgvector step's WIRING, at the level that needs no database
// (#377).
//
// The defect this pins is not in the step. It is in what the step was given: a
// composition root that declares `var store *runtimecomposition.ProjectVectorStore`
// and assigns it into an interface field hands over a TYPED NIL. The interface
// is not nil, so `cfg.ProjectVectorStore != nil` in internal/api/router.go
// passes and the option stores it; the step then calls a nil receiver, which
// answers "project vector store is not configured", and the whole create
// pipeline fails and rolls a fully built tenant back.
//
// Measured on the end-to-end stack before the fix: a deployment that composes
// no Configurations runtime — the shape the E2E stack and every install without
// ELITEA_CONFIGURATIONS_ENABLED have — could not create a project at all. Eight
// of nine steps reported `ok`, the ninth reported "did not complete", and the
// operator read a 500.
//
// Both directions are asserted, because a guard that dropped every collaborator
// would satisfy the first case alone and would silently stop provisioning any
// vector store at all.

import (
	"context"
	"errors"
	"testing"
)

type stubVectorStore struct {
	provisioned  []int64
	removed      []int64
	provisionErr error
}

func (s *stubVectorStore) ProvisionProjectVectorStore(_ context.Context, projectID int64) error {
	s.provisioned = append(s.provisioned, projectID)
	return s.provisionErr
}

func (s *stubVectorStore) RemoveProjectVectorStore(_ context.Context, projectID int64) error {
	s.removed = append(s.removed, projectID)
	return nil
}

// TestWithVectorStoreIgnoresATypedNil: a nil pointer in a non-nil interface is
// the same thing as no vector store, and the step must be INERT rather than
// failing the pipeline.
func TestWithVectorStoreIgnoresATypedNil(t *testing.T) {
	var absent *stubVectorStore

	provisioner := New(nil, nil, nil, WithVectorStore(absent))
	if provisioner.vectorStore != nil {
		t.Fatalf("a typed nil was stored as a collaborator: %#v", provisioner.vectorStore)
	}

	state := &provisionState{projectID: 7}
	if err := createProjectVectorStore(context.Background(), provisioner, state); err != nil {
		t.Fatalf("the step failed for a deployment with no vector store: %v", err)
	}
	// The compensation has to tolerate it too: it runs for every attempted step,
	// so a failure here would turn one failed step into a failed rollback.
	if err := removeProjectVectorStore(context.Background(), provisioner, state); err != nil {
		t.Fatalf("the compensation failed for a deployment with no vector store: %v", err)
	}
}

// TestWithVectorStoreKeepsAComposedCollaborator is the other direction: the
// guard must not swallow a real one, or every project would silently be created
// unable to index.
func TestWithVectorStoreKeepsAComposedCollaborator(t *testing.T) {
	store := &stubVectorStore{}

	provisioner := New(nil, nil, nil, WithVectorStore(store))
	if provisioner.vectorStore == nil {
		t.Fatal("a composed vector store was dropped by the option")
	}

	state := &provisionState{projectID: 7}
	if err := createProjectVectorStore(context.Background(), provisioner, state); err != nil {
		t.Fatalf("the step failed with a composed vector store: %v", err)
	}
	if len(store.provisioned) != 1 || store.provisioned[0] != 7 {
		t.Fatalf("the step reached the collaborator with %v, want [7]", store.provisioned)
	}

	// And a real failure still fails the step. The guard is about the WIRING,
	// not about tolerating a vector store that cannot provision.
	store.provisionErr = errors.New("bootstrap unreachable")
	if err := createProjectVectorStore(context.Background(), provisioner, state); err == nil {
		t.Fatal("a failing vector store reported success")
	}
}
