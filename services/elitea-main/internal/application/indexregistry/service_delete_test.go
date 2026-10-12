package indexregistry

import (
	"context"
	"errors"
	"testing"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
)

type deleteToolkits struct{}

func (deleteToolkits) GetCurrentToolkit(_ context.Context, _, _, toolkit int32) (indexingapp.CurrentToolkitSnapshot, bool, error) {
	return indexingapp.CurrentToolkitSnapshot{ID: toolkit, Type: "github"}, true, nil
}

type deleteStore struct {
	row    Row
	purged []string
}

func (s *deleteStore) List(context.Context, int32, int32) ([]Row, error) { return nil, nil }
func (s *deleteStore) FindByName(context.Context, int32, int32, string) (Row, bool, error) {
	return Row{}, false, nil
}
func (s *deleteStore) SaveConfiguration(context.Context, int32, int32, string, []byte) error {
	return nil
}
func (s *deleteStore) MarkDeleted(context.Context, int32, int32, string) (Row, error) {
	return s.row, nil
}
func (s *deleteStore) PurgeDeleted(_ context.Context, indexID string) error {
	s.purged = append(s.purged, indexID)
	return nil
}

type deleteSchedules struct{ deleted []string }

func (s *deleteSchedules) DeleteCurrentIndexSchedule(_ context.Context, _, _ int32, name string) error {
	s.deleted = append(s.deleted, name)
	return nil
}

// hangingVectors is a store that never answers: an elitea-vector that is
// unreachable while the connection is still being attempted, or one that is
// simply slow. It honours the context it is given.
type hangingVectors struct{ bounds []time.Duration }

func (v *hangingVectors) DeleteIndexVectors(ctx context.Context, _ indexingapp.IndexVectorNamespace) error {
	if deadline, ok := ctx.Deadline(); ok {
		v.bounds = append(v.bounds, time.Until(deadline))
	}
	<-ctx.Done()
	return ctx.Err()
}

func (*hangingVectors) Deferred() bool { return false }

const deleteIndexID = "0d7a4a6e-7b4e-4c1a-9d2c-2f0f0a9f5e11"

// The index delete route never waits for the vector store: its own attempt is
// bounded to RouteVectorDeleteBudget, and when the store does not answer in
// time the route still succeeds, the tombstone stays (not purged) for the
// sweeper, the failure is reported, and the schedule entry is removed.
func TestDeleteBoundsTheVectorAttemptAndSucceedsWithoutTheStore(t *testing.T) {
	store := &deleteStore{row: Row{IndexID: deleteIndexID, ProjectID: 7, ToolkitID: 19, Name: "docs"}}
	schedules := &deleteSchedules{}
	vectors := &hangingVectors{}
	var reported []error
	service, err := NewService(deleteToolkits{}, store, schedules, vectors, func(err error) { reported = append(reported, err) })
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = service.Delete(context.Background(), indexmetaapp.DeleteRequest{
		ProjectID: 7, ActorUserID: 3, ToolkitID: 19, IndexMetaID: deleteIndexID,
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Delete with an unreachable store must succeed, got %v", err)
	}
	if elapsed < RouteVectorDeleteBudget || elapsed > RouteVectorDeleteBudget+2*time.Second {
		t.Fatalf("the route took %v; the vector attempt is bounded to %v", elapsed, RouteVectorDeleteBudget)
	}
	if len(vectors.bounds) != 1 || vectors.bounds[0] > RouteVectorDeleteBudget || vectors.bounds[0] <= 0 {
		t.Fatalf("the store was given the bound %v, want at most %v", vectors.bounds, RouteVectorDeleteBudget)
	}
	if len(store.purged) != 0 {
		t.Fatalf("the tombstone was purged although the vectors were not deleted: %v", store.purged)
	}
	if len(reported) != 1 || !errors.Is(reported[0], context.DeadlineExceeded) {
		t.Fatalf("reported = %v, want the attempt's deadline", reported)
	}
	if len(schedules.deleted) != 1 || schedules.deleted[0] != "docs" {
		t.Fatalf("schedule cleanup = %v", schedules.deleted)
	}
	if RouteVectorDeleteBudget > 3*time.Second {
		t.Fatalf("RouteVectorDeleteBudget = %v; a user's request must not hang on the store", RouteVectorDeleteBudget)
	}
}
