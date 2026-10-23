package indexing_test

import (
	"net/http"
	"testing"

	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

func TestSourceOnlyIndexContractsPreserveLegacyRBAC(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		method     string
		path       string
		mode       string
		permission string
		wantMethod string
		wantPath   string
		want       string
	}{
		{
			name:       "delete",
			method:     handler.SourceOnlyIndexDeleteMethod,
			path:       handler.SourceOnlyIndexDeletePath,
			mode:       handler.SourceOnlyIndexDeleteMode,
			permission: handler.SourceOnlyIndexDeletePermission,
			wantMethod: http.MethodDelete,
			wantPath:   "/api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}",
			want:       "models.applications.index_meta.delete",
		},
		{
			name:       "schedule",
			method:     handler.SourceOnlyIndexScheduleMethod,
			path:       handler.SourceOnlyIndexSchedulePath,
			mode:       handler.SourceOnlyIndexScheduleMode,
			permission: handler.SourceOnlyIndexSchedulePermission,
			wantMethod: http.MethodPatch,
			wantPath:   "/api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}",
			want:       "models.applications.index_meta.edit",
		},
		{
			name:       "schedule delete",
			method:     handler.SourceOnlyIndexScheduleDeleteMethod,
			path:       handler.SourceOnlyIndexScheduleDeletePath,
			mode:       handler.SourceOnlyIndexScheduleDeleteMode,
			permission: handler.SourceOnlyIndexScheduleDeletePermission,
			wantMethod: http.MethodDelete,
			wantPath:   "/api/v2/elitea_core/index_schedule/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}",
			want:       "models.applications.index_meta.edit",
		},
		{
			name:       "search",
			method:     handler.SourceOnlyIndexSearchMethod,
			path:       handler.SourceOnlyIndexSearchPath,
			mode:       handler.SourceOnlyIndexSearchMode,
			permission: handler.SourceOnlyIndexSearchPermission,
			wantMethod: http.MethodGet,
			wantPath:   "/api/v2/elitea_core/search_options/prompt_lib/{projectID}",
			want:       "models.promptlib_shared.search",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.method != test.wantMethod ||
				test.path != test.wantPath ||
				test.mode != auth.PermissionModeDefault ||
				test.permission != test.want {
				t.Fatalf(
					"source-only contract drifted: method=%q path=%q mode=%q permission=%q",
					test.method,
					test.path,
					test.mode,
					test.permission,
				)
			}
		})
	}
}
