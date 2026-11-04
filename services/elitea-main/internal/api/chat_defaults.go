package api

import (
	"context"
	"errors"
	"strconv"
	"strings"

	v2secrets "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

type chatDefaults struct {
	vault  *v2secrets.Handler
	models *repos.CurrentModelsRepository
}

func (d chatDefaults) ContextManagementEnabled(ctx context.Context, projectID string) (bool, error) {
	value, err := d.vault.ResolveSecretValue(ctx, projectID, "{{secret.context_manager}}")
	if errors.Is(err, v2secrets.ErrSecretNotFound) || errors.Is(err, v2secrets.ErrVaultAbsent) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return strings.ToLower(value) == "true", nil
}
func (d chatDefaults) SupportsReasoning(ctx context.Context, projectID, name string) (bool, error) {
	item, found, err := d.model(ctx, projectID, name)
	if err != nil || !found || item.SupportsReasoning == nil {
		return false, err
	}
	return *item.SupportsReasoning, nil
}

// ReasoningControl is the model's resolved control form (ADR-0038 decision
// 3), or "" when it declares none.
func (d chatDefaults) ReasoningControl(ctx context.Context, projectID, name string) (string, error) {
	item, found, err := d.model(ctx, projectID, name)
	if err != nil || !found || item.ReasoningControl == nil {
		return "", err
	}
	return *item.ReasoningControl, nil
}

// model is the first catalogue row of the project with this name.
func (d chatDefaults) model(ctx context.Context, projectID, name string) (configurationapp.CurrentModelCatalogItem, bool, error) {
	if d.models == nil {
		return configurationapp.CurrentModelCatalogItem{}, false, nil
	}
	id, err := strconv.ParseInt(projectID, 10, 32)
	if err != nil {
		return configurationapp.CurrentModelCatalogItem{}, false, err
	}
	items, err := d.models.List(ctx, int32(id), configurationapp.CurrentModelSectionLLM, false)
	if err != nil {
		return configurationapp.CurrentModelCatalogItem{}, false, err
	}
	for _, item := range items {
		if item.Name == name {
			return item, true, nil
		}
	}
	return configurationapp.CurrentModelCatalogItem{}, false, nil
}
