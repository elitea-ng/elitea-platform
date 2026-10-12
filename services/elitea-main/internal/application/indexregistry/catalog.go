package indexregistry

import (
	"context"
	"errors"
	"fmt"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/indexcatalog"
)

// CatalogSource is the one registry read the frozen catalog needs.
type CatalogSource interface {
	// List returns the live rows of one toolkit, oldest first.
	List(ctx context.Context, projectID, toolkitID int32) ([]Row, error)
}

// ToolkitIndexCatalog projects a toolkit's live registry rows into the frozen
// catalog the Rust worker's index tools read (ADR-0030 I1). Only a row that
// has been stamped with its embedding space (its first run wrote vectors) is
// an index the tools can search; a row that was created and never ran, or
// whose run failed before any write, has no namespace worth listing and is
// left out, as the SDK's list_indexes (distinct collections among stored
// chunks) left it out. The result is never nil: an empty catalog is `[]`,
// which the worker reads as "no indexes yet", not as "no catalog".
func ToolkitIndexCatalog(rows []Row) []map[string]any {
	catalog := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if row.Model == "" || row.Dimension <= 0 || row.Name == "" || row.IndexID == "" {
			continue
		}
		catalog = append(catalog, indexcatalog.Entry(row.Name, row.IndexID, row.Model, row.Dimension))
	}
	return catalog
}

// CatalogResolver answers the frozen catalog of one toolkit from the registry.
// The three freeze paths (agent execution, toolkit.call_tool.v1 and the
// direct read) take it as their index-catalog dependency under the Rust
// indexing runtime.
type CatalogResolver struct {
	store CatalogSource
}

// NewCatalogResolver builds a resolver over store.
func NewCatalogResolver(store CatalogSource) (*CatalogResolver, error) {
	if store == nil {
		return nil, errors.New("index catalog resolver needs a registry store")
	}
	return &CatalogResolver{store: store}, nil
}

// ToolkitIndexCatalog lists the toolkit's searchable indexes.
func (r *CatalogResolver) ToolkitIndexCatalog(ctx context.Context, projectID, toolkitID int32) ([]map[string]any, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("index catalog resolver is unavailable")
	}
	if projectID <= 0 || toolkitID <= 0 {
		return nil, fmt.Errorf("%w: project %d toolkit %d", ErrInvalid, projectID, toolkitID)
	}
	rows, err := r.store.List(ctx, projectID, toolkitID)
	if err != nil {
		return nil, fmt.Errorf("list index registry for the toolkit catalog: %w", err)
	}
	return ToolkitIndexCatalog(rows), nil
}
