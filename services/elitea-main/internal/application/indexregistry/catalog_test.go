package indexregistry

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/indexcatalog"
)

func TestToolkitIndexCatalogAppliesTheWorkersEntryRuleAndCap(t *testing.T) {
	t.Parallel()
	rows := []CatalogRow{
		{Name: "docs", IndexID: "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b", Model: "text-embedding-3-small", Dimension: 1536},
		// A run that failed after writing is stamped and listed: its points stand.
		{Name: "partial", IndexID: "2d2f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5d", Model: "text-embedding-3-small", Dimension: 1536},
		// Rows outside the worker's rule are left out rather than frozen into
		// a catalog the worker would refuse whole.
		{Name: "noid", IndexID: "", Model: "m", Dimension: 3},
		{Name: "", IndexID: "3e3f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5e", Model: "m", Dimension: 3},
		{Name: "new\nline", IndexID: "4e3f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5e", Model: "m", Dimension: 3},
		{Name: "upper", IndexID: "4E3F8C1E-6C1A-4D8E-9B1A-2F6D1C3E4A5E", Model: "m", Dimension: 3},
		// A second row of a listed name: the worker refuses a repeated name.
		{Name: "docs", IndexID: "5e3f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5e", Model: "m", Dimension: 3},
	}
	got, dropped, truncated := ToolkitIndexCatalog(rows)
	want := []map[string]any{
		{"name": "docs", "index_id": "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b", "embedding_model": "text-embedding-3-small", "embedding_dimension": int32(1536)},
		{"name": "partial", "index_id": "2d2f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5d", "embedding_model": "text-embedding-3-small", "embedding_dimension": int32(1536)},
	}
	if !reflect.DeepEqual(got, want) || dropped != 5 || truncated {
		t.Fatalf("catalog = %#v dropped=%d truncated=%v, want %#v 5 false", got, dropped, truncated, want)
	}
	if empty, _, _ := ToolkitIndexCatalog(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("an empty catalog must be [] not nil: %#v", empty)
	}
	many := make([]CatalogRow, 0, indexcatalog.MaxEntries+1)
	for i := range indexcatalog.MaxEntries + 1 {
		many = append(many, CatalogRow{Name: fmt.Sprintf("i%d", i), IndexID: "ab", Model: "m", Dimension: 3})
	}
	got, dropped, truncated = ToolkitIndexCatalog(many)
	if len(got) != indexcatalog.MaxEntries || dropped != 0 || !truncated || got[0]["name"] != "i0" {
		t.Fatalf("cap: len=%d dropped=%d truncated=%v first=%v", len(got), dropped, truncated, got[0]["name"])
	}
	got, _, truncated = ToolkitIndexCatalog(many[:indexcatalog.MaxEntries])
	if len(got) != indexcatalog.MaxEntries || truncated {
		t.Fatalf("exactly the cap is not a truncation: len=%d truncated=%v", len(got), truncated)
	}
}

type catalogStoreStub struct {
	rows []CatalogRow
	err  error
	seen [][]int32
}

func (s *catalogStoreStub) CatalogEntries(_ context.Context, projectID int32, toolkitIDs []int32) ([]CatalogRow, error) {
	s.seen = append(s.seen, append([]int32{projectID}, toolkitIDs...))
	if s.err != nil {
		return nil, s.err
	}
	// Like the query: only the toolkits asked, unless the stub is told to
	// misbehave with a row for a toolkit outside them.
	rows := make([]CatalogRow, 0, len(s.rows))
	for _, row := range s.rows {
		for _, toolkitID := range toolkitIDs {
			if row.ToolkitID == toolkitID || row.ToolkitID == 99 {
				rows = append(rows, row)
				break
			}
		}
	}
	return rows, nil
}

func TestCatalogResolverAsksOnceForEveryToolkitAndRefusesBadIdentities(t *testing.T) {
	t.Parallel()
	if _, err := NewCatalogResolver(nil); err == nil {
		t.Fatal("a nil store must be refused")
	}
	store := &catalogStoreStub{rows: []CatalogRow{
		{ToolkitID: 41, Name: "docs", IndexID: "a1", Model: "m", Dimension: 8},
		{ToolkitID: 43, Name: "code", IndexID: "a2", Model: "m", Dimension: 8},
	}}
	resolver, err := NewCatalogResolver(store)
	if err != nil {
		t.Fatal(err)
	}
	// Three toolkits, one repeated: one query naming each once, every one a
	// key of the answer, the toolkit without rows an empty catalog.
	catalogs, err := resolver.ToolkitIndexCatalogs(context.Background(), 7, []int32{41, 42, 43, 41})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.seen, [][]int32{{7, 41, 42, 43}}) {
		t.Fatalf("store asked %v, want one query for the distinct toolkits", store.seen)
	}
	if len(catalogs) != 3 || len(catalogs[41]) != 1 || catalogs[41][0]["name"] != "docs" ||
		catalogs[42] == nil || len(catalogs[42]) != 0 || len(catalogs[43]) != 1 || catalogs[43][0]["name"] != "code" {
		t.Fatalf("catalogs = %#v", catalogs)
	}
	// The single-toolkit read is the same query.
	catalog, err := resolver.ToolkitIndexCatalog(context.Background(), 7, 41)
	if err != nil || len(catalog) != 1 || catalog[0]["name"] != "docs" {
		t.Fatalf("catalog = %#v err=%v", catalog, err)
	}
	// No toolkits: nothing asked.
	before := len(store.seen)
	if none, err := resolver.ToolkitIndexCatalogs(context.Background(), 7, nil); err != nil || len(none) != 0 || len(store.seen) != before {
		t.Fatalf("none = %#v err=%v seen=%v", none, err, store.seen)
	}
	if _, err := resolver.ToolkitIndexCatalogs(context.Background(), 0, []int32{41}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("project 0 must be refused, got %v", err)
	}
	if _, err := resolver.ToolkitIndexCatalogs(context.Background(), 7, []int32{41, 0}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("toolkit 0 must be refused, got %v", err)
	}
	// A row for a toolkit that was not asked is a store defect, not a catalog.
	store.rows = []CatalogRow{{ToolkitID: 99, Name: "x", IndexID: "ab", Model: "m", Dimension: 1}}
	if _, err := resolver.ToolkitIndexCatalogs(context.Background(), 7, []int32{41}); err == nil {
		t.Fatal("an unasked toolkit's rows must surface")
	}
	store.err = errors.New("db down")
	if _, err := resolver.ToolkitIndexCatalogs(context.Background(), 7, []int32{41}); err == nil {
		t.Fatal("a store failure must surface")
	}
}
