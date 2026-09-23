package configurations

import "net/http"

// CurrentConfigurationToolHandler shares the production typed REST handlers
// with internal MCP. Its caller owns authentication and project permissions.
// It never uses the reduced compatibility catalogue or default-model writer.
type CurrentConfigurationToolHandler struct {
	types    currentConfigurationTypesHandler
	models   currentModelCatalogHandler
	defaults currentModelDefaultHandler
}

// NewCurrentConfigurationToolHandler returns nil unless all typed services exist.
// The services retain their existing storage, cancellation, and secret ownership.
func NewCurrentConfigurationToolHandler(
	types CurrentConfigurationTypesReader,
	models CurrentModelCatalogReader,
	defaults CurrentModelDefaultWriter,
	publicProjectID int32,
) *CurrentConfigurationToolHandler {
	if types == nil || models == nil || defaults == nil || publicProjectID <= 0 {
		return nil
	}
	return &CurrentConfigurationToolHandler{
		types:    currentConfigurationTypesHandler{reader: types},
		models:   currentModelCatalogHandler{reader: models, publicProjectID: publicProjectID},
		defaults: currentModelDefaultHandler{writer: defaults},
	}
}

func (handler *CurrentConfigurationToolHandler) Types(writer http.ResponseWriter, request *http.Request) {
	handler.types.get(writer, request)
}

func (handler *CurrentConfigurationToolHandler) Models(writer http.ResponseWriter, request *http.Request) {
	handler.models.get(writer, request)
}

func (handler *CurrentConfigurationToolHandler) SetDefaultModel(writer http.ResponseWriter, request *http.Request) {
	handler.defaults.post(writer, request)
}
