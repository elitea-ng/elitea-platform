package mcp

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/entitydiscovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

const internalDiscoveryCategory = "elitea_core/discovery"

type internalDiscoveryOperation string

const (
	internalDiscoveryTags   internalDiscoveryOperation = "list_tags"
	internalDiscoverySearch internalDiscoveryOperation = "search_options"
)

func internalDiscoveryTools() []Tool {
	common := func() map[string]any {
		return map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
			"limit":      integerRangeProperty("Maximum rows per entity page. Zero uses the safe bound.", 0, 1000),
			"offset":     integerRangeProperty("Page offset.", 0, 100000),
			"query":      boundedStringProperty("Entity name or description search.", 0, 1024),
			"author_id":  intProperty("Version author ID."),
			"statuses":   map[string]any{"type": "array", "maxItems": 32, "items": boundedStringProperty("Version status.", 0, 64)},
			"tags":       map[string]any{"type": "array", "maxItems": 100, "items": intProperty("Tag ID.")},
		}
	}
	tags := common()
	tags["entity_coverage"] = enumProperty("Entity coverage.", "all", "application", "pipeline", "skill")
	tags["search"] = boundedStringProperty("Tag name search.", 0, 1024)
	tags["my_liked"] = map[string]any{"type": "boolean"}
	tags["trend_start_period"] = boundedStringProperty("Trend start in YYYY-MM-DDTHH:MM:SS format.", 19, 19)
	tags["trend_end_period"] = boundedStringProperty("Trend end in YYYY-MM-DDTHH:MM:SS format.", 19, 19)
	search := common()
	search["entities"] = map[string]any{"type": "array", "maxItems": 7, "items": enumProperty("Entity section.", "application", "pipeline", "toolkit", "credential", "skill", "tag", "collection")}
	for _, key := range []string{"toolkit_type", "type_filter", "section"} {
		search[key] = boundedStringProperty("Exact type or section filter.", 0, 128)
	}
	search["include_shared"] = map[string]any{"type": "boolean"}
	for _, prefix := range []string{"", "application_", "pipeline_", "toolkit_", "credential_", "skill_", "tag_", "shared_"} {
		search[prefix+"limit"] = integerRangeProperty("Maximum page rows.", 0, 1000)
		search[prefix+"offset"] = integerRangeProperty("Page offset.", 0, 100000)
		if prefix != "shared_" {
			search[prefix+"sort"] = enumProperty("Sort field.", "id", "name", "created_at")
			if prefix == "tag_" {
				search[prefix+"sort"] = enumProperty("Tag sort field.", "id", "name")
			}
			search[prefix+"order"] = enumProperty("Sort order.", "asc", "desc")
		}
	}
	return []Tool{
		{Name: "get_prompt_lib_tags", Description: "List visible tags with distinct entity counts and bounded filters.", InputSchema: objectSchema(tags, "project_id"), permission: "models.promptlib_shared.tags.list", internalDiscoveryOperation: internalDiscoveryTags},
		{Name: "get_prompt_lib_search_options", Description: "List visible entity search options in the current section envelope.", InputSchema: objectSchema(search, "project_id", "entities"), permission: "models.promptlib_shared.search", internalDiscoveryOperation: internalDiscoverySearch},
	}
}
func (h *Handler) callInternalDiscoveryTool(r *http.Request, projectID int64, target Tool, arguments map[string]any) map[string]any {
	return h.callInternalTool(r, projectID, target, arguments, "entity discovery", func(actorID int64) (internalApplicationExecution, error) {
		values, err := internalDiscoveryQuery(target, arguments)
		if err != nil {
			return internalNotificationBadRequest("invalid discovery filters")
		}
		actor := strconv.FormatInt(actorID, 10)
		ctx := auth.ContextWithUser(r.Context(), auth.User{ID: actor, UserID: actor})
		svc := entitydiscovery.New(h.pool)
		var result any
		switch target.internalDiscoveryOperation {
		case internalDiscoveryTags:
			filters, e := entitydiscovery.Parse(values)
			if e != nil {
				return internalNotificationBadRequest("invalid discovery filters")
			}
			result, err = svc.Tags(ctx, strconv.FormatInt(projectID, 10), filters)
		case internalDiscoverySearch:
			result, err = svc.SearchOptions(ctx, strconv.FormatInt(projectID, 10), values)
		default:
			return internalNotificationBadRequest("unknown discovery operation")
		}
		if err != nil {
			var typed *apierr.APIError
			if errors.As(err, &typed) && typed.Status < 500 {
				body, _ := json.Marshal(apierr.Response{Error: typed.Message})
				return internalApplicationExecution{status: typed.Status, body: body}, nil
			}
			slog.ErrorContext(ctx, "entity discovery failed", "project_id", projectID, "error", err)
			return internalApplicationExecution{}, err
		}
		body, err := json.Marshal(result)
		if err != nil {
			return internalApplicationExecution{}, err
		}
		if len(body) > maxInternalApplicationResultBytes {
			return internalApplicationExecution{}, apierr.BadRequest("discovery result exceeds response limit")
		}
		return internalApplicationExecution{status: http.StatusOK, body: body}, nil
	})
}
func internalDiscoveryQuery(target Tool, arguments map[string]any) (url.Values, error) {
	values := url.Values{}
	properties, ok := target.InputSchema["properties"].(map[string]any)
	if !ok {
		return nil, apierr.BadRequest("invalid discovery schema")
	}
	if target.internalDiscoveryOperation == internalDiscoverySearch {
		if _, ok := arguments["entities"]; !ok {
			return nil, apierr.BadRequest("entities is required")
		}
	}
	for key, value := range arguments {
		if key == "project_id" {
			continue
		}
		property, ok := properties[key].(map[string]any)
		if !ok {
			return nil, apierr.BadRequest("unknown discovery filter")
		}
		if !discoveryArgumentType(property, value) {
			return nil, apierr.BadRequest("invalid discovery filter type")
		}
		switch v := value.(type) {
		case []any:
			if len(v) > 100 {
				return nil, apierr.BadRequest("too many filter values")
			}
			for _, entry := range v {
				switch entry.(type) {
				case string, json.Number, float64:
					intText := scalarArgument(entry)
					if len(intText) > 1024 {
						return nil, apierr.BadRequest("invalid filter")
					}
					values.Add(key+"[]", intText)
				default:
					return nil, apierr.BadRequest("invalid filter")
				}
			}
		case string, json.Number, float64, bool:
			text := scalarArgument(v)
			if len(text) > 1024 {
				return nil, apierr.BadRequest("invalid filter")
			}
			values.Set(key, text)
		default:
			return nil, apierr.BadRequest("invalid filter")
		}
	}
	return values, nil
}

// discoveryArgumentType rejects scalar substitution for array and boolean fields.
func discoveryArgumentType(schema map[string]any, value any) bool {
	switch schema["type"] {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer":
		switch value.(type) {
		case json.Number, float64:
			intText := scalarArgument(value)
			_, err := strconv.ParseInt(intText, 10, 64)
			return err == nil
		default:
			return false
		}
	case "array":
		entries, ok := value.([]any)
		if maximum, ok := schema["maxItems"].(int); ok && len(entries) > maximum {
			return false
		}
		if !ok {
			return false
		}
		items, ok := schema["items"].(map[string]any)
		if !ok {
			return false
		}
		for _, entry := range entries {
			if !discoveryArgumentType(items, entry) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
