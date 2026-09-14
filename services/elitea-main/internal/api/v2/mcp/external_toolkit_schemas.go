package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	discovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
)

const externalSchemaDeadline = 60 * time.Second
const maxExternalSchemaBytes = 4 * 1024 * 1024

var errExternalCatalogLimit = errors.New("external MCP catalogue exceeds its limit")
var errExternalSchemaInvalid = errors.New("external MCP instance schema is invalid")

// WithToolkitDiscovery uses the same saved-instance discovery as the toolkit UI.
func WithToolkitDiscovery(source discovery.UseCase) Option {
	return func(handler *Handler) { handler.toolkitDiscovery = source }
}

func dynamicToolkitSchema(toolkitType string) bool {
	return toolkitType == "openapi" || toolkitType == "mcp" || toolkitType == "mcp_config" || strings.HasPrefix(toolkitType, "mcp_")
}

func (p postgresToolSource) toolkitInstanceSchemas(ctx context.Context, schema string, actorID, toolkitID int64, toolkitType string) (map[string]map[string]any, map[string]bool, bool, error) {
	if p.handler == nil || p.handler.toolkitDiscovery == nil || !dynamicToolkitSchema(toolkitType) {
		schemas, _, err := p.toolkitSchemas(toolkitType)
		return schemas, nil, false, err
	}
	// Schema comes from the authenticated route. Require its canonical spelling.
	rawProject := strings.TrimSuffix(strings.TrimPrefix(schema, `"p_`), `"`)
	expected, valid := projectSchema(rawProject)
	projectID, err := strconv.ParseInt(rawProject, 10, 64)
	if !valid || expected != schema || err != nil || actorID <= 0 || toolkitID <= 0 {
		return nil, nil, true, errExternalCatalogIdentity
	}
	result, err := p.handler.toolkitDiscovery.AvailableTools(ctx, discovery.Request{ProjectID: projectID, ActorUserID: actorID, ToolkitID: toolkitID})
	if err != nil {
		return nil, nil, true, err
	}
	schemas, available, err := externalInstanceSchemas(result)
	return schemas, available, true, err
}

func externalInstanceSchemas(result discovery.Result) (map[string]map[string]any, map[string]bool, error) {
	if len(result.Tools) > 4096 || len(result.ArgsSchemas) > 4096 {
		return nil, nil, errExternalCatalogLimit
	}
	schemas := make(map[string]map[string]any, len(result.Tools))
	available := make(map[string]bool, len(result.Tools))
	total := 0
	for _, tool := range result.Tools {
		if tool.Name == "" || len(tool.Name) > 256 || available[tool.Name] {
			return nil, nil, errExternalSchemaInvalid
		}
		raw, exists := result.ArgsSchemas[tool.Name]
		if !exists {
			return nil, nil, errExternalSchemaInvalid
		}
		total += len(raw)
		if total > maxExternalSchemaBytes {
			return nil, nil, errExternalCatalogLimit
		}
		var value map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if !json.Valid(raw) || decoder.Decode(&value) != nil || value == nil || value["type"] != "object" {
			return nil, nil, errExternalSchemaInvalid
		}
		schemas[tool.Name], available[tool.Name] = value, true
	}
	return schemas, available, nil
}
