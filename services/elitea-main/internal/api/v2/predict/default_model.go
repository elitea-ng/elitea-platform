package predict

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
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
	resolved, err := c.resolve(ctx, req)
	if err != nil {
		return "", err
	}
	return c.next.Complete(ctx, resolved)
}

// resolve fills an omitted model from the project catalog default, and
// resolves the model's reasoning control form when the request carries a
// thinking setting. Complete and CompleteStream share it, so the two modes
// cannot pick different models.
func (c *defaultModelCompleter) resolve(ctx context.Context, req CompletionRequest) (CompletionRequest, error) {
	if req.reasoningResolved || (req.Model != "" && req.ReasoningEffort == "") {
		return req, nil
	}
	if req.ReasoningEffort != "" {
		return c.resolveReasoning(ctx, req)
	}
	resolved, _, err := c.resolveModel(ctx, req)
	return resolved, err
}

// resolveReasoning resolves the model (an omitted one from the default) and
// its control form, and refuses a setting the model cannot honour. A
// catalogue that cannot be read for an explicit model keeps the form from
// before ADR-0038: the gateway still governs the call.
func (c *defaultModelCompleter) resolveReasoning(ctx context.Context, req CompletionRequest) (CompletionRequest, error) {
	if req.reasoningResolved {
		return req, nil
	}
	if canonical, ok := configurationapp.CanonicalReasoningEffort(req.ReasoningEffort); ok {
		req.ReasoningEffort = canonical
	}
	resolved, item, err := c.resolveModel(ctx, req)
	if err != nil {
		if req.Model == "" {
			return resolved, err
		}
		slog.WarnContext(ctx, "predict: model catalogue unavailable; the thinking setting keeps the effort form",
			"project_id", req.ProjectID, "model", req.Model, "err", err)
		resolved.reasoningResolved = true
		return resolved, nil
	}
	return applyItemReasoning(resolved, item)
}

// resolveModel reads the project catalog. It fills an omitted model from the
// default and returns the catalogue item of the request's model, or nil when
// the catalogue has none of that name.
func (c *defaultModelCompleter) resolveModel(ctx context.Context, req CompletionRequest) (CompletionRequest, *configurationapp.CurrentModelCatalogItem, error) {
	projectID, err := strconv.ParseInt(req.ProjectID, 10, 32)
	if err != nil || projectID <= 0 {
		return req, nil, ErrDefaultModelUnavailable
	}
	catalog, err := c.models.Get(ctx, configurationapp.CurrentModelCatalogQuery{
		Section: configurationapp.CurrentModelSectionLLM, ProjectID: int32(projectID),
		PublicProjectID: c.publicProjectID, IncludeShared: true,
	})
	if err != nil {
		return req, nil, fmt.Errorf("predict: resolve default model: %w", err)
	}
	if req.Model == "" {
		if catalog.DefaultModelName == nil || catalog.DefaultModelProjectID == nil || *catalog.DefaultModelName == "" {
			return req, nil, ErrDefaultModelUnavailable
		}
		for i := range catalog.Items {
			item := &catalog.Items[i]
			if item.Name == *catalog.DefaultModelName && item.ProjectID == *catalog.DefaultModelProjectID {
				req.Model = item.Name
				return req, item, nil
			}
		}
		return req, nil, ErrDefaultModelUnavailable
	}
	// The project's own model of that name first, then a shared one: the
	// gateway resolves the name the same way.
	var shared *configurationapp.CurrentModelCatalogItem
	for i := range catalog.Items {
		item := &catalog.Items[i]
		if item.Name != req.Model {
			continue
		}
		if item.ProjectID == int32(projectID) {
			return req, item, nil
		}
		if shared == nil {
			shared = item
		}
	}
	return req, shared, nil
}
