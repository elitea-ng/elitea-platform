package api

import (
	"context"
	"errors"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

type fakeEngine struct {
	app      string
	deleted  bool
	err      error
	toolkits []int64
}

func (f *fakeEngine) App() string { return f.app }

func (f *fakeEngine) DeleteProjectData(context.Context, int64) (string, error) {
	return "deleted", nil
}

func (f *fakeEngine) DeleteToolkitData(_ context.Context, _, toolkitID int64) (platformops.ToolkitDeletion, error) {
	f.toolkits = append(f.toolkits, toolkitID)
	return platformops.ToolkitDeletion{Deleted: f.deleted}, f.err
}

type fakeGraphVectors struct {
	err   error
	calls [][2]int64
}

func (f *fakeGraphVectors) DeleteInventoryGraph(_ context.Context, projectID, toolkitID int64) error {
	f.calls = append(f.calls, [2]int64{projectID, toolkitID})
	return f.err
}

func TestAnInventoryToolkitDeletionAlsoDeletesItsGraphVectors(t *testing.T) {
	inventory := &fakeEngine{app: platformops.AppInventory, deleted: true}
	deepwiki := &fakeEngine{app: platformops.AppDeepWiki}
	vectors := &fakeGraphVectors{}
	clients := WithInventoryGraphVectors([]ApplicationDataClient{deepwiki, inventory, nil}, vectors)
	if len(clients) != 3 || clients[0] != deepwiki || clients[2] != nil {
		t.Fatalf("only the Inventory client is wrapped: %v", clients)
	}
	deletion, err := clients[1].DeleteToolkitData(context.Background(), 7, 42)
	if err != nil || !deletion.Deleted {
		t.Fatalf("deletion = %v, %v", deletion, err)
	}
	if len(vectors.calls) != 1 || vectors.calls[0] != [2]int64{7, 42} {
		t.Fatalf("vector deletions = %v", vectors.calls)
	}
	if summary, err := clients[1].DeleteProjectData(context.Background(), 7); err != nil || summary != "deleted" {
		t.Fatalf("the project deletion is the engine's: %q, %v", summary, err)
	}
	if !HoldsInventory(clients) || HoldsInventory([]ApplicationDataClient{deepwiki}) {
		t.Fatal("HoldsInventory")
	}
}

func TestTheGraphVectorsWaitForTheEngineAndKeepTheRowOpen(t *testing.T) {
	// The engine refused (an ingestion runs): the vectors are not touched.
	busy := &fakeEngine{app: platformops.AppInventory, err: platformops.ErrRetryLater}
	vectors := &fakeGraphVectors{}
	clients := WithInventoryGraphVectors([]ApplicationDataClient{busy}, vectors)
	if _, err := clients[0].DeleteToolkitData(context.Background(), 7, 42); !errors.Is(err, platformops.ErrRetryLater) || len(vectors.calls) != 0 {
		t.Fatalf("err = %v, vector calls = %v", err, vectors.calls)
	}
	// A refused certificate keeps the row open rather than closing it.
	refused := &fakeGraphVectors{err: platformops.ErrNotAllowed}
	clients = WithInventoryGraphVectors([]ApplicationDataClient{&fakeEngine{app: platformops.AppInventory}}, refused)
	_, err := clients[0].DeleteToolkitData(context.Background(), 7, 42)
	if !errors.Is(err, platformops.ErrRetryLater) || errors.Is(err, platformops.ErrNotAllowed) {
		t.Fatalf("refused: %v", err)
	}
}

// A replica with no elitea-vector client has no Inventory vectors it could
// reach, so the Inventory client is not wrapped: the toolkit deletion deletes
// the engine's rows and closes, instead of waiting for ever (ErrRetryLater)
// for a vector leg that has nothing to delete.
func TestWithoutAVectorClientTheInventoryToolkitDeletionCloses(t *testing.T) {
	engine := &fakeEngine{app: platformops.AppInventory}
	clients := WithInventoryGraphVectors([]ApplicationDataClient{engine}, nil)
	if clients[0] != engine {
		t.Fatalf("the engine's client was wrapped: %T", clients[0])
	}
	if _, err := clients[0].DeleteToolkitData(context.Background(), 7, 42); err != nil {
		t.Fatalf("no client: %v, want the engine's deletion to close the row", err)
	}
}
