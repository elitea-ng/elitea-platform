package mcp

// The fixed internal builder categories, in process, for the desktop's
// executeRemoteToolkitTool (ADR-0029 decisions 3 and 5b, issue 1231).
//
// A cloud turn reaches a platform MCP server (`mcp_elitea_internal_*`) over
// this package's own endpoint, `/app/{pid}/mcp/<category>`, with a token
// minted for the executing actor. The desktop's remote call is already inside
// Main, authenticated as that same actor, so it does not dial the endpoint: it
// runs the same listing and the same dispatch (callInternalTarget) the endpoint
// runs, with the caller's own request. Every operation then re-checks its own
// permission against that caller (authorizePermission) and clamps project_id
// to the call's project, exactly as on the endpoint. Nothing here elevates.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpregistry"
)

// ErrNotPlatformTool is a toolkit type that names no internal builder, or a
// call this package cannot dispatch.
var ErrNotPlatformTool = errors.New("mcp: not a platform MCP tool")

// PlatformToolOutcome is what one in-process platform tool call answered.
type PlatformToolOutcome struct {
	// Disabled: MCP exposure is switched off on this deployment
	// (mcp_configuration.mcp_enabled); nothing ran, as on the endpoint.
	Disabled bool
	// Unknown: the category has no tool of that name; nothing ran.
	// Available names the category's tools, sorted.
	Unknown   bool
	Available []string
	// IsError is the CallToolResult's isError: the operation refused or
	// failed (a missing permission included), and Text says why.
	IsError bool
	// Text is the result's text content (the operation's JSON body on
	// success).
	Text string
}

// PlatformToolInfo is one tool of a platform MCP server, as the endpoint's
// tools/list names it.
type PlatformToolInfo struct {
	Name        string
	Description string
}

// PlatformTools lists the tools of the internal builder toolkitType names, as
// its endpoint lists them in projectID (the desktop's resolved definition
// names them, client contract 1.16). The listing does not depend on the
// caller: each operation checks its own permission when it is called.
func (h *Handler) PlatformTools(ctx context.Context, projectID int64, toolkitType string) ([]PlatformToolInfo, error) {
	builder, ok := mcpregistry.InternalBuilderForType(toolkitType)
	if !ok || h == nil || projectID <= 0 {
		return nil, ErrNotPlatformTool
	}
	schema, ok := projectSchema(strconv.FormatInt(projectID, 10))
	if !ok {
		return nil, ErrNotPlatformTool
	}
	tools, err := h.source.tools(ctx, schema, scope{kind: scopeCategory, category: builder.Category})
	if err != nil {
		return nil, err
	}
	listed := make([]PlatformToolInfo, 0, len(tools))
	for _, tool := range tools {
		listed = append(listed, PlatformToolInfo{Name: tool.Name, Description: tool.Description})
	}
	return listed, nil
}

// CallPlatformTool runs one tool of the internal builder toolkitType names
// in projectID, as the caller authenticated on r. arguments is one JSON
// object (empty means {}).
func (h *Handler) CallPlatformTool(r *http.Request, projectID int64, toolkitType, toolName string, arguments json.RawMessage) (PlatformToolOutcome, error) {
	builder, ok := mcpregistry.InternalBuilderForType(toolkitType)
	if !ok || h == nil || r == nil || projectID <= 0 {
		return PlatformToolOutcome{}, ErrNotPlatformTool
	}
	if !h.mcpEnabled(r.Context()) {
		return PlatformToolOutcome{Disabled: true, IsError: true, Text: mcpDisabledMessage}, nil
	}
	schema, ok := projectSchema(strconv.FormatInt(projectID, 10))
	if !ok {
		return PlatformToolOutcome{}, ErrNotPlatformTool
	}
	tools, err := h.source.tools(r.Context(), schema, scope{kind: scopeCategory, category: builder.Category})
	if err != nil {
		return PlatformToolOutcome{}, err
	}
	var target Tool
	found := false
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
		if tool.Name == toolName {
			target, found = tool, true
		}
	}
	if !found {
		sort.Strings(names)
		return PlatformToolOutcome{Unknown: true, Available: names}, nil
	}
	decoded := map[string]any{}
	if trimmed := bytes.TrimSpace(arguments); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil || decoded == nil {
			return PlatformToolOutcome{IsError: true, Text: "the arguments must be one JSON object; nothing was executed"}, nil
		}
	}
	result, internal := h.callInternalTarget(r, projectID, target, decoded)
	if !internal {
		return PlatformToolOutcome{}, ErrNotPlatformTool
	}
	return platformToolOutcome(result), nil
}

// platformToolOutcome reads a CallToolResult this package built.
func platformToolOutcome(result map[string]any) PlatformToolOutcome {
	outcome := PlatformToolOutcome{IsError: result["isError"] == true}
	var texts []string
	switch content := result["content"].(type) {
	case []map[string]any:
		for _, item := range content {
			if text, ok := item["text"].(string); ok {
				texts = append(texts, text)
			}
		}
	case []any:
		for _, raw := range content {
			if item, ok := raw.(map[string]any); ok {
				if text, ok := item["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
	}
	outcome.Text = strings.Join(texts, "\n")
	return outcome
}
