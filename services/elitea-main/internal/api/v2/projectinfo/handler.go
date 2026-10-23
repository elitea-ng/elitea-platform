package projectinfo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	CurrentProjectInfoPath       = "/api/v2/elitea_core/project_info/prompt_lib/{projectID}/project-info"
	CurrentProjectInfoMode       = auth.PermissionModeDefault
	CurrentProjectInfoPermission = "models.project_context.view"
	CurrentProjectInfoTimeout    = 5 * time.Second
)

var (
	ErrInvalidCurrentProjectInfoRoute   = errors.New("invalid current project-info route dependencies")
	ErrInvalidCurrentProjectInfoRequest = errors.New("current project-info request is invalid")
)

// CurrentProjectInfo is the exact successful presentation shape returned by
// the current project_info GET endpoint.
type CurrentProjectInfo struct {
	TeammatesCount int64           `json:"teammates_count"`
	IconMeta       json.RawMessage `json:"icon_meta"`
}

type CurrentProjectInfoReader interface {
	GetCurrentProjectInfo(context.Context, int32) (CurrentProjectInfo, error)
}

// CurrentProjectInfoRepository reads the existing global membership tables
// and the already-authorized project's configuration table. Tenant selection
// is derived from a positive project ID and remains transaction-local.
type CurrentProjectInfoRepository struct {
	pool    *pgxpool.Pool
	tenants *tenant.Executor
}

func NewCurrentProjectInfoRepository(
	pool *pgxpool.Pool,
) (*CurrentProjectInfoRepository, error) {
	if pool == nil {
		return nil, errors.New("current project-info database is required")
	}
	return &CurrentProjectInfoRepository{
		pool:    pool,
		tenants: tenant.NewExecutor(pool),
	}, nil
}

func (repository *CurrentProjectInfoRepository) GetCurrentProjectInfo(
	ctx context.Context,
	projectID int32,
) (CurrentProjectInfo, error) {
	if repository == nil || repository.pool == nil || repository.tenants == nil ||
		ctx == nil || projectID <= 0 {
		return CurrentProjectInfo{}, ErrInvalidCurrentProjectInfoRequest
	}
	if err := ctx.Err(); err != nil {
		return CurrentProjectInfo{}, err
	}

	result := CurrentProjectInfo{IconMeta: json.RawMessage(`null`)}
	systemUserEmail := fmt.Sprintf("system_user_%d@centry.user", projectID)
	membershipCtx, cancelMembership := context.WithTimeout(
		ctx,
		CurrentProjectInfoTimeout,
	)
	membershipErr := repository.pool.QueryRow(membershipCtx, `
SELECT COUNT(DISTINCT assignment.user_id)
FROM public.auth_core__project_user_role AS assignment
JOIN public.auth_core__user AS project_user
  ON project_user.id = assignment.user_id
WHERE assignment.project_id = $1
  AND project_user.email IS DISTINCT FROM $2`,
		projectID,
		systemUserEmail,
	).Scan(&result.TeammatesCount)
	cancelMembership()
	if membershipErr != nil {
		// The current endpoint treats the admin membership dependency as
		// optional and reports zero teammates when that call fails.
		result.TeammatesCount = 0
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CurrentProjectInfo{}, ctxErr
		}
	}

	configurationCtx, cancelConfiguration := context.WithTimeout(
		ctx,
		CurrentProjectInfoTimeout,
	)
	defer cancelConfiguration()
	err := repository.tenants.WithinTx(
		configurationCtx,
		tenant.Project{ID: int64(projectID)},
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
		func(ctx context.Context, tx pgx.Tx) error {
			var iconMeta []byte
			if err := tx.QueryRow(ctx, `
SELECT COALESCE((
    SELECT data -> 'icon_meta'
    FROM configuration
    WHERE project_id = $1
      AND type = 'project_icon'
      AND elitea_title = $2
    LIMIT 1
), 'null'::jsonb)`,
				projectID,
				fmt.Sprintf("project_icon_%d", projectID),
			).Scan(&iconMeta); err != nil {
				return fmt.Errorf("get current project icon: %w", err)
			}
			result.IconMeta = json.RawMessage(bytes.Clone(iconMeta))
			return nil
		},
	)
	if err != nil {
		return CurrentProjectInfo{}, err
	}
	return result, nil
}

var _ CurrentProjectInfoReader = (*CurrentProjectInfoRepository)(nil)

// CurrentProjectInfoRoute owns only the current project-info GET and remains
// unmounted until production composition explicitly selects this reviewed
// slice. The current PUT contract is intentionally outside this route.
type CurrentProjectInfoRoute struct {
	handler http.Handler
}

func NewCurrentProjectInfoRoute(
	reader CurrentProjectInfoReader,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentProjectInfoRoute, error) {
	if reader == nil || !authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidCurrentProjectInfoRoute
	}

	handler := &currentProjectInfoHandler{reader: reader}
	endpoint := http.Handler(http.HandlerFunc(handler.get))
	endpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentProjectInfoMode,
		currentProjectInfoProjectID,
		CurrentProjectInfoPermission,
	)(endpoint)
	endpoint = apimw.Auth(authConfig)(endpoint)

	router := chi.NewRouter()
	router.Method(http.MethodGet, CurrentProjectInfoPath, endpoint)
	return &CurrentProjectInfoRoute{handler: router}, nil
}

func (route *CurrentProjectInfoRoute) ServeHTTP(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentProjectInfoHandler struct {
	reader CurrentProjectInfoReader
}

func (handler *currentProjectInfoHandler) get(
	writer http.ResponseWriter,
	request *http.Request,
) {
	projectID, ok := currentProjectInfoID(chi.URLParam(request, "projectID"))
	if !ok {
		writeCurrentProjectInfoFailure(writer)
		return
	}

	result, err := handler.reader.GetCurrentProjectInfo(request.Context(), projectID)
	if err != nil {
		writeCurrentProjectInfoFailure(writer)
		return
	}
	if len(result.IconMeta) == 0 {
		result.IconMeta = json.RawMessage(`null`)
	}
	writeCurrentProjectInfoJSON(writer, http.StatusOK, result)
}

func currentProjectInfoProjectID(request *http.Request) (string, bool) {
	projectID, ok := currentProjectInfoID(chi.URLParam(request, "projectID"))
	if !ok {
		return "", false
	}
	return strconv.FormatInt(int64(projectID), 10), true
}

func currentProjectInfoID(value string) (int32, bool) {
	if value == "" {
		return 0, false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	return int32(parsed), err == nil && parsed > 0
}

func writeCurrentProjectInfoFailure(writer http.ResponseWriter) {
	writeCurrentProjectInfoJSON(writer, http.StatusInternalServerError, map[string]string{
		"error": "Failed to get project info",
	})
}

func writeCurrentProjectInfoJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
