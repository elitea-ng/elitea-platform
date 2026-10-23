package indexing

import (
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// These current-baseline contracts remain source-only until their application,
// storage, scheduling, and response semantics are complete. Keeping the exact
// path and RBAC metadata here does not make them mountable.
const (
	SourceOnlyIndexDeleteMethod     = http.MethodDelete
	SourceOnlyIndexDeletePath       = "/api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}"
	SourceOnlyIndexDeleteMode       = auth.PermissionModeDefault
	SourceOnlyIndexDeletePermission = "models.applications.index_meta.delete"

	SourceOnlyIndexScheduleMethod     = http.MethodPatch
	SourceOnlyIndexSchedulePath       = "/api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}"
	SourceOnlyIndexScheduleMode       = auth.PermissionModeDefault
	SourceOnlyIndexSchedulePermission = "models.applications.index_meta.edit"

	SourceOnlyIndexScheduleDeleteMethod     = http.MethodDelete
	SourceOnlyIndexScheduleDeletePath       = "/api/v2/elitea_core/index_schedule/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}"
	SourceOnlyIndexScheduleDeleteMode       = auth.PermissionModeDefault
	SourceOnlyIndexScheduleDeletePermission = "models.applications.index_meta.edit"

	SourceOnlyIndexSearchMethod     = http.MethodGet
	SourceOnlyIndexSearchPath       = "/api/v2/elitea_core/search_options/prompt_lib/{projectID}"
	SourceOnlyIndexSearchMode       = auth.PermissionModeDefault
	SourceOnlyIndexSearchPermission = "models.promptlib_shared.search"
)
