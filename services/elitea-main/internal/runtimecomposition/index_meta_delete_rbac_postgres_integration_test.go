package runtimecomposition

import (
	"context"
	"net/http"
	"testing"

	publicapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	indexingapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	indexmetaapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexmeta"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

type currentIndexMetaDeleteRBACSpy struct {
	calls   int
	request indexmetaapp.DeleteRequest
}

func (spy *currentIndexMetaDeleteRBACSpy) Delete(
	_ context.Context,
	request indexmetaapp.DeleteRequest,
) error {
	spy.calls++
	spy.request = request
	return nil
}

func TestCurrentIndexMetaDeleteRoutePostgresPermissionMatrix(t *testing.T) {
	pool := newIndexRBACPostgresPool(t)
	prepareIndexRBACFixtures(t, pool)
	resolver := legacyrbac.NewPostgresResolver(pool)
	authConfig := apimw.AuthConfig{
		PrincipalValidator: authsvc.NewPrincipalValidator(pool),
		FormSessions:       authtest.FormSessions{},
	}

	for _, test := range []struct {
		name      string
		userID    string
		actorID   int64
		want      int
		wantCalls int
	}{
		{
			name:      "project admin allowed",
			userID:    "3",
			actorID:   3,
			want:      http.StatusOK,
			wantCalls: 1,
		},
		{
			name:      "project editor allowed",
			userID:    "4",
			actorID:   4,
			want:      http.StatusOK,
			wantCalls: 1,
		},
		{
			name:   "project viewer denied before delete",
			userID: "5",
			want:   http.StatusForbidden,
		},
		{
			name:   "other project editor denied before delete",
			userID: "8",
			want:   http.StatusForbidden,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			deleter := &currentIndexMetaDeleteRBACSpy{}
			route, err := indexingapi.NewCurrentIndexMetaDeleteRoute(
				deleter,
				authConfig,
				resolver,
			)
			if err != nil {
				t.Fatal(err)
			}
			router := publicapi.NewRouter(publicapi.RouterConfig{
				CurrentIndexMetaDelete: route,
			})
			response := newIndexRBACStreamingRecorder()
			router.ServeHTTP(
				response,
				newIndexRBACRequest(
					http.MethodDelete,
					"/api/v2/elitea_core/index_meta/prompt_lib/1/9/meta-1",
					test.userID,
					nil,
				),
			)
			if response.Code != test.want || deleter.calls != test.wantCalls {
				t.Fatalf(
					"status=%d want=%d calls=%d want=%d body=%s",
					response.Code,
					test.want,
					deleter.calls,
					test.wantCalls,
					response.Body.String(),
				)
			}
			if test.wantCalls == 1 &&
				deleter.request != (indexmetaapp.DeleteRequest{
					ProjectID:   1,
					ActorUserID: test.actorID,
					ToolkitID:   9,
					IndexMetaID: "meta-1",
				}) {
				t.Fatalf("request=%+v", deleter.request)
			}
		})
	}
}
