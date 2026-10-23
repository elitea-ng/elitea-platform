package toolkitexecution

import (
	"context"
	"errors"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

var ErrInvalidDelegatedAuthToolkitSettings = errors.New("invalid delegated authorization toolkit settings request")

// DelegatedAuthToolkitSettings is the short-lived plaintext view needed by
// Main's OAuth token proxy. It must not be logged, persisted, or returned to a
// browser. The project toolkit row remains reference-only at rest.
type DelegatedAuthToolkitSettings struct {
	ToolkitType string
	Settings    map[string]any
	// ProjectOwnedSettings is the same toolkit resolved WITHOUT the admin-vault
	// fallback (#1286): only literals and the owning project's vault. A value
	// that differs from Settings drew on a global secret. nil means provenance
	// is unavailable, and a consumer must then treat every secret as foreign.
	ProjectOwnedSettings map[string]any
}

// CurrentDelegatedAuthToolkitSettings resolves one actor-visible toolkit
// through the same schema, configuration-expansion, and vault path used at
// worker claim time. OAuth is not limited to remote MCP rows: SharePoint and
// OpenAPI use the same compatibility-named delegated authorization flow.
type CurrentDelegatedAuthToolkitSettings struct {
	toolkits CurrentMCPToolkitReader
	settings CurrentToolkitSettingsResolver
	// projectOwned resolves the provenance twin. nil leaves
	// ProjectOwnedSettings nil. See WithProjectOwnedToolkitSettings.
	projectOwned CurrentToolkitSettingsResolver
}

// WithProjectOwnedToolkitSettings supplies the resolver whose unsecreter never
// falls back to the admin vault, so the OAuth proxy can tell a project secret
// from a global one (#1286).
func (s *CurrentDelegatedAuthToolkitSettings) WithProjectOwnedToolkitSettings(
	resolver CurrentToolkitSettingsResolver,
) *CurrentDelegatedAuthToolkitSettings {
	if s != nil {
		s.projectOwned = resolver
	}
	return s
}

func NewCurrentDelegatedAuthToolkitSettings(
	toolkits CurrentMCPToolkitReader,
	settings CurrentToolkitSettingsResolver,
) (*CurrentDelegatedAuthToolkitSettings, error) {
	if toolkits == nil || settings == nil {
		return nil, errors.New("delegated authorization toolkit settings dependencies are required")
	}
	return &CurrentDelegatedAuthToolkitSettings{toolkits: toolkits, settings: settings}, nil
}

// ResolveDelegatedAuthToolkitSettings loads and materializes one exact toolkit.
// found=false is reserved for an absent or actor-invisible row. Other failures
// stay distinct so the HTTP boundary does not misreport an unavailable vault or
// configuration store as a missing toolkit.
func (s *CurrentDelegatedAuthToolkitSettings) ResolveDelegatedAuthToolkitSettings(
	ctx context.Context,
	projectID int32,
	actorID int32,
	toolkitID int32,
) (DelegatedAuthToolkitSettings, bool, error) {
	if s == nil || s.toolkits == nil || s.settings == nil || ctx == nil ||
		projectID <= 0 || actorID <= 0 || toolkitID <= 0 {
		return DelegatedAuthToolkitSettings{}, false, ErrInvalidDelegatedAuthToolkitSettings
	}
	if err := ctx.Err(); err != nil {
		return DelegatedAuthToolkitSettings{}, false, err
	}

	toolkit, found, err := s.toolkits.GetCurrentMCPToolkit(ctx, projectID, actorID, toolkitID)
	if err != nil || !found {
		return DelegatedAuthToolkitSettings{}, found, err
	}
	if toolkit.ID != toolkitID || toolkit.Type == "" || toolkit.Settings == nil {
		return DelegatedAuthToolkitSettings{}, false, ErrInvalidDelegatedAuthToolkitSettings
	}

	request := configurationapp.CurrentToolkitSettingsRequest{
		ToolkitType: toolkit.Type,
		Settings:    toolkit.Settings,
		ProjectID:   projectID,
		UserID:      actorID,
		Mode:        configurationapp.CurrentToolkitSettingsClaimMode,
	}
	resolved, err := s.settings.Resolve(ctx, request)
	if err != nil {
		return DelegatedAuthToolkitSettings{}, false, err
	}
	if resolved == nil {
		return DelegatedAuthToolkitSettings{}, false, ErrInvalidDelegatedAuthToolkitSettings
	}
	result := DelegatedAuthToolkitSettings{ToolkitType: toolkit.Type, Settings: resolved}
	if s.projectOwned != nil {
		// The SAME stored row, so the two answers differ only in where a
		// secret was redeemed from. A failure here fails the request: a
		// provenance that could not be computed must not read as "owned".
		owned, err := s.projectOwned.Resolve(ctx, request)
		if err != nil {
			return DelegatedAuthToolkitSettings{}, false, err
		}
		if owned == nil {
			return DelegatedAuthToolkitSettings{}, false, ErrInvalidDelegatedAuthToolkitSettings
		}
		result.ProjectOwnedSettings = owned
	}
	return result, true, nil
}
