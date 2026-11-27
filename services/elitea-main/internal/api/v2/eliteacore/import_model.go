package eliteacore

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"strconv"
	"strings"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// ImportModelCatalog is the project's model catalogue as an import reads it:
// the same catalogue (and the same default) a run resolves the agent's model
// against (agentexecution resolveCurrentAgentModel).
type ImportModelCatalog interface {
	Get(context.Context, configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error)
}

// WithImportModelCatalog lets an import pin each imported agent to a model of
// THIS install (issue 3143). A file names its model by the source install's
// name and project (a blueprint names none). Stored as it came, a model the
// catalogue does not hold resolved at every run, silently, to the project's
// default model, whose context window is not the one the person chose. The
// import now writes the model it resolves to and says which one it is.
// Without a catalogue (a handler built for a test) the import stores the
// file's settings as they are.
func WithImportModelCatalog(catalog ImportModelCatalog) Option {
	return func(handler *Handler) {
		if catalog != nil {
			handler.importModels = catalog
		}
	}
}

// importModelResolution is what the import did with one agent's model. It is
// answered on the imported agent (`model`), so the dialog can say which model
// the agent runs on and when it is not the one the file named.
type importModelResolution struct {
	Name          string `json:"name"`
	ProjectID     int32  `json:"project_id"`
	ContextWindow *int   `json:"context_window,omitempty"`
	// Defaulted is true when the file named no model this install holds and
	// the agent was given the project's default model instead.
	Defaulted bool `json:"defaulted"`
	// Requested is the model the file named; empty when it named none.
	Requested string `json:"requested,omitempty"`
}

// importModelResolver resolves the models of one import against one read of
// the project's catalogue.
type importModelResolver struct {
	catalog   configurationapp.CurrentModelCatalogResponse
	projectID int32
	// ready is false when there is no catalogue to read or the read failed:
	// the import then stores the file's settings as they are, as before.
	ready bool
}

// importModelResolverFor reads the project's LLM catalogue once for an import.
func (h *Handler) importModelResolverFor(ctx context.Context, projectID string) importModelResolver {
	if h.importModels == nil {
		return importModelResolver{}
	}
	project, err := strconv.ParseInt(projectID, 10, 32)
	if err != nil || project <= 0 {
		return importModelResolver{}
	}
	public, err := strconv.ParseInt(publicProjectIDOrDefault(), 10, 32)
	if err != nil || public <= 0 {
		return importModelResolver{}
	}
	catalog, err := h.importModels.Get(ctx, configurationapp.CurrentModelCatalogQuery{
		Section: configurationapp.CurrentModelSectionLLM, ProjectID: int32(project),
		PublicProjectID: int32(public), IncludeShared: true,
	})
	if err != nil {
		// The run resolves the model again; an import is not refused because
		// the catalogue could not be read now.
		slog.WarnContext(ctx, "import: model catalogue unavailable; imported agents keep the file's model settings",
			"project_id", projectID, "error", err)
		return importModelResolver{}
	}
	return importModelResolver{catalog: catalog, projectID: int32(project), ready: true}
}

// importedModelProjectID reads `model_project_id` as an export writes it (a
// decimal string) or as the editor does (a number).
func importedModelProjectID(value any) (int32, bool) {
	var parsed int64
	switch typed := value.(type) {
	case string:
		number, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return 0, false
		}
		parsed = number
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		parsed = number
	case float64:
		if typed != math.Trunc(typed) {
			return 0, false
		}
		parsed = int64(typed)
	case int:
		parsed = int64(typed)
	case int64:
		parsed = typed
	default:
		return 0, false
	}
	if parsed <= 0 || parsed > math.MaxInt32 {
		return 0, false
	}
	return int32(parsed), true
}

// find is the catalogue item the settings name: the exact (name, project)
// pair, else the name in this project, else the name as a shared model.
func (r importModelResolver) find(name string, settings map[string]any) *configurationapp.CurrentModelCatalogItem {
	if name == "" {
		return nil
	}
	if project, ok := importedModelProjectID(settings["model_project_id"]); ok {
		for index := range r.catalog.Items {
			item := &r.catalog.Items[index]
			if item.Name == name && item.ProjectID == project {
				return item
			}
		}
	}
	return configurationapp.FindModelCatalogItem(r.catalog.Items, name, r.projectID)
}

// defaultItem is the project's default model, when the catalogue holds it.
func (r importModelResolver) defaultItem() *configurationapp.CurrentModelCatalogItem {
	if r.catalog.DefaultModelName == nil || r.catalog.DefaultModelProjectID == nil || *r.catalog.DefaultModelName == "" {
		return nil
	}
	for index := range r.catalog.Items {
		item := &r.catalog.Items[index]
		if item.Name == *r.catalog.DefaultModelName && item.ProjectID == *r.catalog.DefaultModelProjectID {
			return item
		}
	}
	return nil
}

// resolve pins one version's llm_settings to a model of this install, in
// place: the model the file names when the catalogue holds it, else the
// project's default model. It answers what it did, or nil when it could not
// resolve (no catalogue, or a catalogue with no model at all): the settings
// are then left as they came, and the run refuses or resolves as before.
func (r importModelResolver) resolve(settings map[string]any) *importModelResolution {
	if !r.ready || settings == nil {
		return nil
	}
	requested, _ := settings["model_name"].(string)
	requested = strings.TrimSpace(requested)
	item := r.find(requested, settings)
	defaulted := false
	if item == nil {
		item = r.defaultItem()
		defaulted = true
	}
	if item == nil {
		return nil
	}
	settings["model_name"] = item.Name
	settings["model_project_id"] = item.ProjectID
	resolution := &importModelResolution{
		Name: item.Name, ProjectID: item.ProjectID, Defaulted: defaulted, Requested: requested,
	}
	if item.ContextWindow != nil && !item.ContextWindowFallback {
		window := *item.ContextWindow
		resolution.ContextWindow = &window
	}
	return resolution
}

// pinImportedModel resolves one version entry's model in place. An absent or
// null `llm_settings` becomes an object that names the resolved model; one of
// the wrong JSON type is left for the column encoder to report.
func pinImportedModel(models importModelResolver, version map[string]any) *importModelResolution {
	raw, present := version["llm_settings"]
	settings, isObject := raw.(map[string]any)
	if present && raw != nil && !isObject {
		return nil
	}
	if settings == nil {
		settings = map[string]any{}
	}
	resolution := models.resolve(settings)
	if resolution != nil {
		version["llm_settings"] = settings
	}
	return resolution
}
