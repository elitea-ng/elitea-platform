package indexregistry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/indexcatalog"
)

// CatalogRow is what the frozen catalog needs of one live, stamped registry
// row: nothing else (no state, no history JSON) is read for it.
type CatalogRow struct {
	ToolkitID int32
	Name      string
	IndexID   string
	Model     string
	Dimension int32
}

// CatalogSource is the one registry read the frozen catalog needs.
type CatalogSource interface {
	// CatalogEntries returns the live, stamped rows of the project's
	// toolkits named, ordered by toolkit then oldest first, at most
	// indexcatalog.MaxEntries+1 per toolkit (the extra row tells the caller
	// the toolkit was truncated). A toolkit with no such rows yields none.
	CatalogEntries(ctx context.Context, projectID int32, toolkitIDs []int32) ([]CatalogRow, error)
}

// ToolkitIndexCatalog projects one toolkit's catalog rows into the frozen
// catalog the Rust worker's index tools read (ADR-0030 I1). Only a row that
// has been stamped with its embedding space (its first run wrote vectors) is
// an index the tools can search; the query already leaves out a row that was
// created and never ran, as the SDK's list_indexes (distinct collections
// among stored chunks) left it out. A row outside indexcatalog.ValidEntry —
// the one rule the worker parses by — is left out too, as is a second row
// of a name already listed, and the catalog is cut at
// indexcatalog.MaxEntries (oldest first); `dropped` and `truncated` report
// those so the resolver can log them. The result is never nil: an empty
// catalog is `[]`, which the worker reads as "no indexes yet", not as "no
// catalog".
func ToolkitIndexCatalog(rows []CatalogRow) (catalog []map[string]any, dropped int, truncated bool) {
	catalog = make([]map[string]any, 0, min(len(rows), indexcatalog.MaxEntries))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if !indexcatalog.ValidEntry(row.Name, row.IndexID, row.Model, int64(row.Dimension)) {
			dropped++
			continue
		}
		if _, duplicate := seen[row.Name]; duplicate {
			dropped++
			continue
		}
		if len(catalog) == indexcatalog.MaxEntries {
			truncated = true
			break
		}
		seen[row.Name] = struct{}{}
		catalog = append(catalog, indexcatalog.Entry(row.Name, row.IndexID, row.Model, row.Dimension))
	}
	return catalog, dropped, truncated
}

// CatalogResolver answers the frozen catalogs of a freeze's toolkits from the
// registry in one narrow query (review round 2). The two freeze paths that
// serve index tools (agent execution and toolkit.call_tool.v1) take it as
// their index-catalog dependency under the Rust indexing runtime; the direct
// read (toolkit.execute.read.v1) freezes no catalog, since its claim carries
// no vector token and the worker serves no index tools there.
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

// ToolkitIndexCatalogs lists the searchable indexes of every toolkit named,
// in one query. Every requested id is a key of the result; a toolkit with no
// indexes maps to `[]`. A repeated id is asked for once.
func (r *CatalogResolver) ToolkitIndexCatalogs(
	ctx context.Context,
	projectID int32,
	toolkitIDs []int32,
) (map[int32][]map[string]any, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("index catalog resolver is unavailable")
	}
	if projectID <= 0 {
		return nil, fmt.Errorf("%w: project %d", ErrInvalid, projectID)
	}
	result := make(map[int32][]map[string]any, len(toolkitIDs))
	distinct := make([]int32, 0, len(toolkitIDs))
	for _, toolkitID := range toolkitIDs {
		if toolkitID <= 0 {
			return nil, fmt.Errorf("%w: toolkit %d", ErrInvalid, toolkitID)
		}
		if _, repeated := result[toolkitID]; repeated {
			continue
		}
		result[toolkitID] = []map[string]any{}
		distinct = append(distinct, toolkitID)
	}
	if len(distinct) == 0 {
		return result, nil
	}
	rows, err := r.store.CatalogEntries(ctx, projectID, distinct)
	if err != nil {
		return nil, fmt.Errorf("list index registry for the toolkit catalogs: %w", err)
	}
	byToolkit := make(map[int32][]CatalogRow, len(distinct))
	for _, row := range rows {
		if _, asked := result[row.ToolkitID]; !asked {
			return nil, fmt.Errorf("index registry answered for toolkit %d, which was not asked", row.ToolkitID)
		}
		byToolkit[row.ToolkitID] = append(byToolkit[row.ToolkitID], row)
	}
	for toolkitID, toolkitRows := range byToolkit {
		catalog, dropped, truncated := ToolkitIndexCatalog(toolkitRows)
		if dropped > 0 || truncated {
			slog.WarnContext(ctx, "index catalog: rows left out of the frozen catalog",
				"event", "index_catalog_rows_left_out",
				"project_id", projectID,
				"toolkit_id", toolkitID,
				"dropped_outside_rule", dropped,
				"truncated_at", indexcatalog.MaxEntries,
				"truncated", truncated)
		}
		result[toolkitID] = catalog
	}
	return result, nil
}

// ToolkitIndexCatalog lists one toolkit's searchable indexes.
func (r *CatalogResolver) ToolkitIndexCatalog(ctx context.Context, projectID, toolkitID int32) ([]map[string]any, error) {
	catalogs, err := r.ToolkitIndexCatalogs(ctx, projectID, []int32{toolkitID})
	if err != nil {
		return nil, err
	}
	return catalogs[toolkitID], nil
}
