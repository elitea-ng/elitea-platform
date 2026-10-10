package projectprovisioning

// The rollback-versus-deprovision split of the project_pgvector step (#1211).
//
// A create-failure rollback must never drop the project's PgVector database or
// role; an explicit project delete must. Both go through compensate, so the
// test drives compensate itself with only this step attempted — the only way
// the two intents differ is state.deleting, which only Deprovision sets.

import (
	"context"
	"errors"
	"testing"
)

func TestRollbackNeverDropsTheVectorStoreButDeprovisionDoes(t *testing.T) {
	attempted := []StepStatus{{Step: StepProjectPgvector, Initialized: true}}

	t.Run("create-failure rollback", func(t *testing.T) {
		store := &stubVectorStore{}
		p := New(nil, nil, nil, WithVectorStore(store))

		rollback := p.compensate(context.Background(), &provisionState{projectID: 7}, attempted)

		if len(store.removed) != 1 || store.removed[0] != 7 {
			t.Fatalf("rollback removed %v, want the configuration row of [7]", store.removed)
		}
		if len(store.dropped) != 0 {
			t.Fatalf("a create-failure rollback dropped the vector store: %v", store.dropped)
		}
		if len(rollback) != 1 || (rollback[0].OK == nil || !*rollback[0].OK) {
			t.Fatalf("rollback = %+v", rollback)
		}
	})

	t.Run("explicit delete", func(t *testing.T) {
		store := &stubVectorStore{}
		p := New(nil, nil, nil, WithVectorStore(store))
		state := &provisionState{projectID: 7, deleting: true}

		rollback := p.compensate(context.Background(), state, attempted)

		if len(store.removed) != 1 || len(store.dropped) != 1 || store.dropped[0] != 7 {
			t.Fatalf("delete removed %v and dropped %v, want both for [7]", store.removed, store.dropped)
		}
		if len(rollback) != 1 || (rollback[0].OK == nil || !*rollback[0].OK) {
			t.Fatalf("rollback = %+v", rollback)
		}
	})

	t.Run("a failed drop is reported, not swallowed", func(t *testing.T) {
		store := &stubVectorStore{dropErr: errors.New("pg unreachable")}
		p := New(nil, nil, nil, WithVectorStore(store))
		state := &provisionState{projectID: 7, deleting: true}

		rollback := p.compensate(context.Background(), state, attempted)

		if len(rollback) != 1 || rollback[0].OK != nil && *rollback[0].OK {
			t.Fatalf("rollback = %+v, want the step reported as failed", rollback)
		}
		if state.vectorStoreDropErr == nil {
			t.Fatal("the drop failure was not recorded for Deprovision to report")
		}
	})

	t.Run("no vector store", func(t *testing.T) {
		p := New(nil, nil, nil)
		state := &provisionState{projectID: 7, deleting: true}
		if err := deprovisionProjectVectorStore(context.Background(), p, state); err != nil {
			t.Fatalf("delete with no vector store failed: %v", err)
		}
	})
}

func TestOnlyTheVectorStoreStepHasADestructiveDeprovision(t *testing.T) {
	for _, s := range createSteps() {
		if s.deprovision != nil && s.name != StepProjectPgvector {
			t.Errorf("step %s has a deprovision variant; destructive deletes need an explicit decision", s.name)
		}
	}
}
