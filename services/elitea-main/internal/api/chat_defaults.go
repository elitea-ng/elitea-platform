package api

import (
	"context"
	"errors"
	"strconv"
	"strings"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
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
	if d.models == nil {
		return false, nil
	}
	id, err := strconv.ParseInt(projectID, 10, 32)
	if err != nil {
		return false, err
	}
	items, err := d.models.List(ctx, int32(id), configurationapp.CurrentModelSectionLLM, false)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.Name == name && item.SupportsReasoning != nil {
			return *item.SupportsReasoning, nil
		}
	}
	return false, nil
}
