package indexregistry

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestToolkitIndexCatalogListsOnlyStampedRows(t *testing.T) {
	t.Parallel()
	rows := []Row{
		{IndexID: "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b", Name: "docs", State: StateCompleted, Model: "text-embedding-3-small", Dimension: 1536},
		// Created, never ran: no embedding space, nothing to search.
		{IndexID: "1c1f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5c", Name: "pending", State: StateCreated},
		// A run that failed after writing is stamped and listed: its points stand.
		{IndexID: "2d2f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5d", Name: "partial", State: StateFailed, Model: "text-embedding-3-small", Dimension: 1536},
		// Corrupt rows are left out rather than frozen into the toolkit.
		{IndexID: "", Name: "noid", Model: "m", Dimension: 3},
		{IndexID: "3e3f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5e", Name: "", Model: "m", Dimension: 3},
	}
	got := ToolkitIndexCatalog(rows)
	want := []map[string]any{
		{"name": "docs", "index_id": "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b", "embedding_model": "text-embedding-3-small", "embedding_dimension": int32(1536)},
		{"name": "partial", "index_id": "2d2f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5d", "embedding_model": "text-embedding-3-small", "embedding_dimension": int32(1536)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog = %#v, want %#v", got, want)
	}
	if empty := ToolkitIndexCatalog(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("an empty catalog must be [] not nil: %#v", empty)
	}
}

type catalogStoreStub struct {
	rows []Row
	err  error
	seen [][2]int32
}

func (s *catalogStoreStub) List(_ context.Context, projectID, toolkitID int32) ([]Row, error) {
	s.seen = append(s.seen, [2]int32{projectID, toolkitID})
	return s.rows, s.err
}

func TestCatalogResolverReadsTheToolkitAndRefusesBadIdentities(t *testing.T) {
	t.Parallel()
	if _, err := NewCatalogResolver(nil); err == nil {
		t.Fatal("a nil store must be refused")
	}
	store := &catalogStoreStub{rows: []Row{{IndexID: "a1", Name: "docs", Model: "m", Dimension: 8}}}
	resolver, err := NewCatalogResolver(store)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := resolver.ToolkitIndexCatalog(context.Background(), 7, 41)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog) != 1 || catalog[0]["name"] != "docs" {
		t.Fatalf("catalog = %#v", catalog)
	}
	if !reflect.DeepEqual(store.seen, [][2]int32{{7, 41}}) {
		t.Fatalf("store asked for %v", store.seen)
	}
	if _, err := resolver.ToolkitIndexCatalog(context.Background(), 0, 41); !errors.Is(err, ErrInvalid) {
		t.Fatalf("project 0 must be refused, got %v", err)
	}
	store.err = errors.New("db down")
	if _, err := resolver.ToolkitIndexCatalog(context.Background(), 7, 41); err == nil {
		t.Fatal("a store failure must surface")
	}
}
