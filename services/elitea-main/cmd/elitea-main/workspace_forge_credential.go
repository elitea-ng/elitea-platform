package main

import (
	"context"
	"errors"
	"math"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
)

// forgeExpander is *configurationapp.CurrentExpansionService: the same
// expansion (and the same vault) a toolkit's configuration reference takes.
type forgeExpander interface {
	Expand(ctx context.Context, request configurationapp.CurrentExpansionRequest) (map[string]any, error)
}

// forgeCredentialResolver expands a thread's forge credential reference
// (ADR-0034 decision 10, amended 2026-10-26) exactly as a GitHub or GitLab
// toolkit's `<type>_configuration` setting is expanded: the project's
// configuration, the user's private one, or a shared one of the public
// project, and only of the reference's type.
type forgeCredentialResolver struct {
	expander forgeExpander
}

// forgeCredentialMetadata are the expansion's own keys; the worker gets the
// configuration's data without them.
var forgeCredentialMetadata = []string{"configuration_uuid", "configuration_project_id", "configuration_type",
	"elitea_title", "private"}

func newForgeCredentialResolver(expander forgeExpander) workspacehandoff.ForgeCredentialResolver {
	if expander == nil {
		return nil
	}
	return forgeCredentialResolver{expander: expander}
}

func (r forgeCredentialResolver) ResolveForgeCredential(ctx context.Context, projectID, userID int64,
	credential workspacehandoff.ForgeCredential, unsecret bool) (map[string]any, error) {
	if projectID <= 0 || projectID > math.MaxInt32 || userID <= 0 || userID > math.MaxInt32 ||
		!workspacehandoff.ValidForgeCredential(credential) {
		return nil, workspacehandoff.ErrForgeCredentialInvalid
	}
	user := int32(userID)
	key := credential.Type + "_configuration"
	expanded, err := r.expander.Expand(ctx, configurationapp.CurrentExpansionRequest{
		Payload: map[string]any{key: map[string]any{
			"elitea_title": credential.EliteaTitle, "private": credential.Private,
		}},
		CurrentProjectID: int32(projectID),
		UserID:           &user,
		Unsecret:         unsecret,
	})
	if err != nil {
		var expansion *configurationapp.CurrentExpansionError
		if errors.As(err, &expansion) && expansion.Code != configurationapp.CurrentExpansionDependencyCode {
			return nil, workspacehandoff.ErrForgeCredentialInvalid
		}
		return nil, errors.Join(workspacehandoff.ErrUnavailable, err)
	}
	configuration, ok := expanded[key].(map[string]any)
	if !ok || configuration["configuration_type"] != credential.Type {
		return nil, workspacehandoff.ErrForgeCredentialInvalid
	}
	for _, key := range forgeCredentialMetadata {
		delete(configuration, key)
	}
	return configuration, nil
}
