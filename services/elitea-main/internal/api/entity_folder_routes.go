package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/entityfolders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// entityFolderPermissions are the permissions the list of each kind already
// uses. Pipelines are applications, so they share the applications set.
type entityFolderPerms struct{ list, create, update string }

var entityFolderPermissions = map[entityfolders.Kind]entityFolderPerms{
	entityfolders.KindAgent:    {"models.applications.applications.list", "models.applications.applications.create", "models.applications.application.update"},
	entityfolders.KindPipeline: {"models.applications.applications.list", "models.applications.applications.create", "models.applications.application.update"},
	entityfolders.KindSkill:    {"models.applications.skills.list", "models.applications.skills.create", "models.applications.skills.update"},
	entityfolders.KindToolkit:  {"models.applications.tools.list", "models.applications.tools.create", "models.applications.tool.update"},
	entityfolders.KindMCP:      {"models.applications.tools.list", "models.applications.tools.create", "models.applications.tool.update"},
}

// registerEntityFolderRoutes mounts
//
//	GET    /entity_folders/prompt_lib/{projectID}/{kind}                    list (with item ids)
//	POST   /entity_folders/prompt_lib/{projectID}/{kind}                    create
//	PUT    /entity_folders/prompt_lib/{projectID}/{kind}/{folderID}         rename / reorder
//	DELETE /entity_folders/prompt_lib/{projectID}/{kind}/{folderID}         delete, items return to the root
//	PUT    /entity_folders/prompt_lib/{projectID}/{kind}/items/{entityID}   move into / out of a folder
//
// `{kind}` is one segment, so the spec describes five operations and not
// twenty-five. The permission depends on the kind, so the gate is chosen per
// request from a table built once here. An unknown kind still passes a project
// gate first (the agents list permission) and only then answers 404, so a caller
// from another project is refused before learning anything about the path.
func registerEntityFolderRoutes(r chi.Router, h *entityfolders.Handler, gate func(string) func(http.Handler) http.Handler) {
	base := "/entity_folders/prompt_lib/{projectID}/{kind}"
	r.With(kindGate(gate, func(p entityFolderPerms) string { return p.list })).Get(base, h.List)
	r.With(kindGate(gate, func(p entityFolderPerms) string { return p.create })).Post(base, h.Create)
	update := kindGate(gate, func(p entityFolderPerms) string { return p.update })
	r.With(update).Put(base+"/items/{entityID}", h.Move)
	r.With(update).Put(base+"/{folderID}", h.Update)
	r.With(update).Delete(base+"/{folderID}", h.Delete)
}

// kindGate applies, per request, the project permission `pick` names for the
// `{kind}` of the path.
func kindGate(gate func(string) func(http.Handler) http.Handler, pick func(entityFolderPerms) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		gated := make(map[entityfolders.Kind]http.Handler, len(entityFolderPermissions))
		for kind, perms := range entityFolderPermissions {
			gated[kind] = gate(pick(perms))(next)
		}
		unknown := gate(entityFolderPermissions[entityfolders.KindAgent].list)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			_, err := entityfolders.KindOf(req)
			apierr.Write(w, err)
		}))
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			kind, err := entityfolders.KindOf(req)
			if err != nil {
				unknown.ServeHTTP(w, req)
				return
			}
			gated[kind].ServeHTTP(w, req)
		})
	}
}
