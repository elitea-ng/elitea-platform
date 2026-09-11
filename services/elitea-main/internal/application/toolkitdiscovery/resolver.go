package toolkitdiscovery

import (
	"context"
	"encoding/json"
	"errors"
	call "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
)

type CurrentAuthoritativeInputResolver struct {
	resolver *call.CurrentAuthoritativeInputResolver
}

func NewCurrentAuthoritativeInputResolver(toolkits call.CurrentToolkitReader, settings call.CurrentToolkitSettingsValidator, guardrails call.CurrentGuardrailResolver) (*CurrentAuthoritativeInputResolver, error) {
	resolver, err := call.NewCurrentAuthoritativeInputResolver(toolkits, settings, guardrails)
	if err != nil {
		return nil, err
	}
	return &CurrentAuthoritativeInputResolver{resolver}, nil
}
func (r *CurrentAuthoritativeInputResolver) Resolve(ctx context.Context, request Request) (AuthoritativeInputs, error) {
	if err := request.Validate(); err != nil {
		return AuthoritativeInputs{}, err
	}
	source, err := r.resolver.Resolve(ctx, call.RunRequest{ProjectID: request.ProjectID, ActorUserID: request.ActorUserID, ToolkitID: request.ToolkitID, ToolName: "available_tools", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		return AuthoritativeInputs{}, err
	}
	var wrapper struct {
		Settings json.RawMessage `json:"settings"`
	}
	if json.Unmarshal(source.Settings, &wrapper) != nil || !validBoundedJSONObject(wrapper.Settings) {
		return AuthoritativeInputs{}, errors.New("invalid saved toolkit settings")
	}
	return AuthoritativeInputs{ToolkitType: source.ToolkitType, ToolkitID: source.ToolkitID, ToolkitVersion: source.ToolkitVersion, Settings: append(json.RawMessage(nil), wrapper.Settings...), RuntimeContext: append(json.RawMessage(nil), source.RuntimeContext...)}, nil
}
