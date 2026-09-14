package predict

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// ModelCatalog supplies the authorized project catalog and its configured default.
type ModelCatalog interface {
	Get(context.Context, configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error)
}

// ErrDefaultModelUnavailable prevents dispatch without an available configured default.
var ErrDefaultModelUnavailable = errors.New("predict: project default model is unavailable")

type defaultModelCompleter struct {
	next            Completer
	models          ModelCatalog
	publicProjectID int32
}

// WithDefaultModel resolves omitted models through the shared configuration catalog.
// Explicit models and the caller's billing and authorization identity stay unchanged.
func WithDefaultModel(next Completer, models ModelCatalog, publicProjectID int32) (Completer, error) {
	if next == nil || models == nil || publicProjectID <= 0 {
		return nil, errors.New("predict: default model dependencies are required")
	}
	return &defaultModelCompleter{next: next, models: models, publicProjectID: publicProjectID}, nil
}

func (c *defaultModelCompleter) Complete(ctx context.Context, req CompletionRequest) (string, error) {
	if req.Model != "" {
		return c.next.Complete(ctx, req)
	}
	projectID, err := strconv.ParseInt(req.ProjectID, 10, 32)
	if err != nil || projectID <= 0 {
		return "", ErrDefaultModelUnavailable
	}
	catalog, err := c.models.Get(ctx, configurationapp.CurrentModelCatalogQuery{
		Section: configurationapp.CurrentModelSectionLLM, ProjectID: int32(projectID),
		PublicProjectID: c.publicProjectID, IncludeShared: true,
	})
	if err != nil {
		return "", fmt.Errorf("predict: resolve default model: %w", err)
	}
	if catalog.DefaultModelName == nil || catalog.DefaultModelProjectID == nil || *catalog.DefaultModelName == "" {
		return "", ErrDefaultModelUnavailable
	}
	for _, item := range catalog.Items {
		if item.Name == *catalog.DefaultModelName && item.ProjectID == *catalog.DefaultModelProjectID {
			req.Model = item.Name
			return c.next.Complete(ctx, req)
		}
	}
	return "", ErrDefaultModelUnavailable
}
