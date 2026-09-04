package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

type currentPrebuiltMCPStore interface {
	Lookup(context.Context, string) (mcpregistry.PrebuiltServer, error)
}

// currentAgentPrebuiltMCP exposes enabled fixed HTTP definitions to agent execution.
type currentAgentPrebuiltMCP struct {
	store          currentPrebuiltMCPStore
	internalOrigin string
	actorTokens    currentActorTokenIssuer
}

type currentActorTokenIssuer interface {
	IssueToken(context.Context, int64) (string, error)
}

type currentAgentToolkitSettingsResolver struct {
	inner    agentexecutionapp.CurrentAgentToolkitSettingsResolver
	prebuilt *currentAgentPrebuiltMCP
}

func newCurrentAgentPrebuiltMCP(
	store currentPrebuiltMCPStore,
) (*currentAgentPrebuiltMCP, error) {
	if store == nil {
		return nil, errors.New("current agent prebuilt MCP store is required")
	}
	return &currentAgentPrebuiltMCP{store: store}, nil
}

func newCurrentAgentRuntimePrebuiltMCP(
	store currentPrebuiltMCPStore,
	currentMainBaseURL string,
	actorTokens currentActorTokenIssuer,
) (*currentAgentPrebuiltMCP, error) {
	resolver, err := newCurrentAgentPrebuiltMCP(store)
	if err != nil {
		return nil, err
	}
	origin, err := internalMCPOrigin(currentMainBaseURL)
	if err != nil {
		return nil, err
	}
	if actorTokens == nil {
		return nil, errors.New("current agent actor token issuer is required")
	}
	resolver.internalOrigin = origin
	resolver.actorTokens = actorTokens
	return resolver, nil
}

func (resolver currentAgentToolkitSettingsResolver) Resolve(
	ctx context.Context,
	request configurationapp.CurrentToolkitSettingsRequest,
) (map[string]any, error) {
	if resolver.inner == nil {
		return nil, errors.New("current agent toolkit settings resolver is required")
	}
	settings, err := resolver.inner.Resolve(ctx, request)
	if err != nil || settings == nil {
		return settings, err
	}
	if request.ToolkitType != "mcp_config" &&
		!mcpregistry.IsPrebuiltToolkitType(request.ToolkitType) {
		return settings, nil
	}
	if resolver.prebuilt == nil {
		return nil, errors.New("current agent prebuilt MCP resolver is required")
	}
	return resolver.prebuilt.selectSettings(ctx, request.ToolkitType, settings)
}

func (source *currentAgentPrebuiltMCP) FindCurrentActorVisibleToolkitSchema(
	ctx context.Context,
	projectID int32,
	userID int32,
	toolkitType string,
) (configurationapp.CurrentToolkitSchema, bool, error) {
	if ctx == nil || projectID <= 0 || userID <= 0 ||
		!validCurrentToolkitSchemaIdentifier(toolkitType) {
		return configurationapp.CurrentToolkitSchema{}, false, ErrCurrentToolkitSchemaLookupInvalid
	}
	if err := ctx.Err(); err != nil {
		return configurationapp.CurrentToolkitSchema{}, false, err
	}
	if source == nil || source.store == nil {
		return configurationapp.CurrentToolkitSchema{}, false, ErrCurrentDynamicToolkitSchemasUnavailable
	}
	if !mcpregistry.IsPrebuiltToolkitType(toolkitType) {
		return configurationapp.CurrentToolkitSchema{}, false, nil
	}
	entry, err := source.store.Lookup(ctx, toolkitType)
	if errors.Is(err, mcpregistry.ErrPrebuiltNotFound) || (err == nil && !entry.Enabled) {
		return configurationapp.CurrentToolkitSchema{}, false, nil
	}
	if err != nil {
		return configurationapp.CurrentToolkitSchema{}, false, err
	}
	properties, err := mcpregistry.PrebuiltConfigProperties(entry.ConfigSchema)
	if err != nil {
		return configurationapp.CurrentToolkitSchema{}, false, err
	}
	return configurationapp.CurrentToolkitSchema{Properties: properties}, true, nil
}

func (source *currentAgentPrebuiltMCP) ResolveCurrentAgentPrebuiltMCP(
	ctx context.Context,
	projectID int32,
	actorID int32,
	toolkitType string,
	settings map[string]any,
	materialize func(map[string]any) (map[string]any, error),
) (map[string]any, bool, error) {
	if ctx == nil || !validCurrentToolkitSchemaIdentifier(toolkitType) || settings == nil || materialize == nil {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if source == nil || source.store == nil {
		return nil, false, ErrCurrentDynamicToolkitSchemasUnavailable
	}

	lookup := toolkitType
	if toolkitType == "mcp_config" {
		serverName, ok := settings["server_name"].(string)
		if !ok || !validCurrentToolkitSchemaIdentifier(serverName) {
			return nil, false, nil
		}
		lookup = serverName
	} else if !mcpregistry.IsPrebuiltToolkitType(toolkitType) {
		return nil, false, nil
	}

	entry, err := source.store.Lookup(ctx, lookup)
	if errors.Is(err, mcpregistry.ErrPrebuiltNotFound) || (err == nil && !entry.Enabled) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	parameterNames, err := mcpregistry.PrebuiltParameterNames(entry)
	if err != nil {
		return nil, false, err
	}
	admitted, err := selectPrebuiltSettings(entry, settings)
	if err != nil {
		return nil, false, err
	}
	parameters := admitted
	if len(parameterNames) > 0 {
		parameters, err = materialize(admitted)
		if err != nil {
			return nil, false, err
		}
	}
	if source.trustedInternalMCP(entry, projectID) {
		parameters["project_id"] = strconv.FormatInt(int64(projectID), 10)
		if prebuiltTemplateUses(entry, "personal_token") {
			if actorID <= 0 || source.actorTokens == nil {
				return nil, false, errors.New("current agent internal MCP actor token issuer is unavailable")
			}
			token, issueErr := source.actorTokens.IssueToken(ctx, int64(actorID))
			if issueErr != nil {
				return nil, false, fmt.Errorf("issue current agent internal MCP actor token: %w", issueErr)
			}
			parameters["personal_token"] = token
		}
	}
	endpoint, fixedHeaders, err := mcpregistry.MaterializePrebuiltTemplates(entry, parameters)
	if err != nil {
		return nil, false, err
	}

	resolved := map[string]any{
		"server_name": entry.Key,
		"url":         endpoint,
		"ssl_verify":  true,
	}
	if entry.TimeoutSeconds > 0 {
		resolved["timeout"] = entry.TimeoutSeconds
	}
	if len(fixedHeaders) > 0 {
		headers := make(map[string]any, len(fixedHeaders))
		for name, value := range fixedHeaders {
			headers[name] = value
		}
		resolved["headers"] = headers
	}
	for _, name := range []string{
		"selected_tools",
		"excluded_tools",
		"enable_caching",
		"cache_ttl",
	} {
		if value, ok := parameters[name]; ok {
			resolved[name] = value
		}
	}
	return resolved, true, nil
}

func internalMCPOrigin(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("current Main origin is invalid")
	}
	return strings.ToLower(parsed.Scheme + "://" + parsed.Host), nil
}

func (source *currentAgentPrebuiltMCP) trustedInternalMCP(
	entry mcpregistry.PrebuiltServer,
	projectID int32,
) bool {
	if source == nil || source.internalOrigin == "" || projectID <= 0 ||
		!strings.Contains(entry.ServerURL, "{project_id}") {
		return false
	}
	probe := strings.ReplaceAll(entry.ServerURL, "{project_id}", strconv.FormatInt(int64(projectID), 10))
	parsed, err := url.Parse(probe)
	if err != nil || strings.ToLower(parsed.Scheme+"://"+parsed.Host) != source.internalOrigin {
		return false
	}
	prefix := "/app/" + strconv.FormatInt(int64(projectID), 10) + "/mcp/"
	return strings.HasPrefix(parsed.Path, prefix) && len(parsed.Path) > len(prefix)
}

func prebuiltTemplateUses(entry mcpregistry.PrebuiltServer, name string) bool {
	token := "{" + name + "}"
	if strings.Contains(entry.ServerURL, token) {
		return true
	}
	for _, value := range entry.Headers {
		if strings.Contains(value, token) {
			return true
		}
	}
	return false
}

func (source *currentAgentPrebuiltMCP) selectSettings(
	ctx context.Context,
	toolkitType string,
	settings map[string]any,
) (map[string]any, error) {
	lookup, ok := prebuiltLookup(toolkitType, settings)
	if !ok {
		return nil, mcpregistry.ErrPrebuiltNotFound
	}
	entry, err := source.store.Lookup(ctx, lookup)
	if err != nil {
		return nil, err
	}
	if !entry.Enabled {
		return nil, mcpregistry.ErrPrebuiltNotFound
	}
	return selectPrebuiltSettings(entry, settings)
}

func prebuiltLookup(toolkitType string, settings map[string]any) (string, bool) {
	if toolkitType == "mcp_config" {
		serverName, ok := settings["server_name"].(string)
		return serverName, ok && validCurrentToolkitSchemaIdentifier(serverName)
	}
	return toolkitType, mcpregistry.IsPrebuiltToolkitType(toolkitType)
}

func selectPrebuiltSettings(
	entry mcpregistry.PrebuiltServer,
	settings map[string]any,
) (map[string]any, error) {
	names, err := mcpregistry.PrebuiltParameterNames(entry)
	if err != nil {
		return nil, err
	}
	names = append(names,
		"server_name", "selected_tools", "excluded_tools", "enable_caching", "cache_ttl")
	selected := make(map[string]any, len(names))
	for _, name := range names {
		if value, ok := settings[name]; ok {
			selected[name] = value
		}
	}
	return selected, nil
}

var _ CurrentActorVisibleToolkitSchemaSource = (*currentAgentPrebuiltMCP)(nil)
var _ agentexecutionapp.CurrentAgentToolkitSettingsResolver = currentAgentToolkitSettingsResolver{}
