package agentexecution

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

// THE ROUTE.
//
// One path, two verbs, two permissions — the split legacy declared
// (legacy/plugins/elitea_core/api/v2/application_task.py). The permission
// strings are legacy's own; the LEGACY default-mode matrix
// (testdata/postgres/legacy-rbac-matrix.json) grants both to admin, editor and
// viewer, and migrations/shared/0120 seeds `models.applications.task.get` the
// way 0068 already seeds `models.applications.task.delete`.
//
// The path parameter is called `responseMessageID`, not `taskID`. The legacy
// id was an arbiter task id the client held; this transport resolves the
// durable execution from the response message it produced, which is the same
// substitution the neighbouring cancel route (cancel.go) already made and for
// the same reason. Both verbs of this route therefore take exactly the
// identifier `DELETE /task/prompt_lib/{projectID}/{responseMessageID}` takes.
//
// The DELETE runs the SAME cancellation use case as that route. It is not a
// second stop implementation: legacy's own docstring called this verb
// deprecated in favour of the other one, and two stop paths that could
// disagree about what "stopped" means is the defect this avoids.
const (
	CurrentApplicationTaskPath = "/api/v2/elitea_core/application_task/prompt_lib/{projectID}/{responseMessageID}"
	CurrentApplicationTaskMode = auth.PermissionModeDefault
	// Legacy: application_task.py `get` check_api.
	CurrentApplicationTaskStatusPermission = "models.applications.task.get"
	// Legacy: application_task.py `delete` check_api. 0068 already grants it.
	CurrentApplicationTaskCancelPermission = "models.applications.task.delete"
)

var ErrInvalidCurrentApplicationTaskRoute = errors.New("invalid current application-task route dependencies")

type CurrentAgentTaskStatusReader interface {
	Status(
		context.Context,
		agentexecutionapp.CurrentAgentTaskStatusRequest,
	) (agentexecutionapp.CurrentAgentTaskStatusOutcome, error)
}

var _ CurrentAgentTaskStatusReader = (*agentexecutionapp.CurrentAgentTaskStatusService)(nil)

// CurrentApplicationTaskRoute serves the legacy application_task path.
type CurrentApplicationTaskRoute struct {
	handler http.Handler
}

func NewCurrentApplicationTaskRoute(
	reader CurrentAgentTaskStatusReader,
	canceller CurrentAgentCanceller,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentApplicationTaskRoute, error) {
	if reader == nil || canceller == nil || authConfig.PrincipalValidator == nil ||
		authConfig.ForwardedIdentityVerifier == nil || permissions == nil {
		return nil, ErrInvalidCurrentApplicationTaskRoute
	}
	handler := &currentApplicationTaskHandler{reader: reader}
	cancelHandler := &currentAgentCancelHandler{canceller: canceller}

	// The two gates are built inline, one per verb, with the permission as a
	// literal constant. A shared helper taking the permission as a PARAMETER
	// would hide both gates from
	// internal/api/router_permission_grant_gate_test.go, which resolves gate
	// arguments through the AST — and an unreadable gate is an ungranted
	// permission nothing reports (issue 372).
	statusEndpoint := http.Handler(http.HandlerFunc(handler.status))
	statusEndpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentApplicationTaskMode,
		currentApplicationTaskProject,
		CurrentApplicationTaskStatusPermission,
	)(statusEndpoint)
	statusEndpoint = apimw.Auth(authConfig)(statusEndpoint)

	cancelEndpoint := http.Handler(http.HandlerFunc(cancelHandler.cancel))
	cancelEndpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentApplicationTaskMode,
		currentApplicationTaskProject,
		CurrentApplicationTaskCancelPermission,
	)(cancelEndpoint)
	cancelEndpoint = apimw.Auth(authConfig)(cancelEndpoint)

	router := chi.NewRouter()
	router.Method(http.MethodGet, CurrentApplicationTaskPath, statusEndpoint)
	router.Method(http.MethodDelete, CurrentApplicationTaskPath, cancelEndpoint)
	return &CurrentApplicationTaskRoute{handler: router}, nil
}

// currentApplicationTaskProject binds {projectID} for the RBAC middleware. It
// is the cancel route's own selector, unchanged.
func currentApplicationTaskProject(request *http.Request) (string, bool) {
	projectID := chi.URLParam(request, "projectID")
	_, valid := positiveCanonicalID(projectID)
	return projectID, valid
}

func (route *CurrentApplicationTaskRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentApplicationTaskHandler struct {
	reader CurrentAgentTaskStatusReader
}

// currentApplicationTaskStatusResponse is the exact answer body. `status` keeps
// legacy's vocabulary; `stopping` and `settled_at` are the two additions
// documented in internal/application/agentexecution/task_status.go.
type currentApplicationTaskStatusResponse struct {
	Status    string     `json:"status"`
	Stopping  bool       `json:"stopping"`
	SettledAt *time.Time `json:"settled_at"`
	ErrorCode string     `json:"error_code,omitempty"`
}

func (handler *currentApplicationTaskHandler) status(writer http.ResponseWriter, request *http.Request) {
	projectID, validProject := positiveCanonicalID(chi.URLParam(request, "projectID"))
	user, authenticated := auth.UserFromContext(request.Context())
	if !authenticated {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	actorUserID, owningUser := user.OwningUserID()
	if !owningUser {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	// Legacy's `?meta=yes` / `?result=yes` expansions are refused rather than
	// silently ignored. See the contract note in the application package.
	if truthyCurrentApplicationTaskFlag(request.URL.Query().Get("meta")) ||
		truthyCurrentApplicationTaskFlag(request.URL.Query().Get("result")) {
		writeError(
			writer,
			http.StatusBadRequest,
			"Task meta and task result are not served on this transport; read the response message items",
		)
		return
	}
	requestModel := agentexecutionapp.CurrentAgentTaskStatusRequest{
		ProjectID:         projectID,
		ActorUserID:       actorUserID,
		ResponseMessageID: chi.URLParam(request, "responseMessageID"),
	}
	if !validProject || requestModel.Validate() != nil {
		writeError(writer, http.StatusBadRequest, "Invalid agent task status request")
		return
	}
	outcome, err := handler.reader.Status(request.Context(), requestModel)
	if err != nil {
		writeCurrentApplicationTaskStatusError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, currentApplicationTaskStatusResponse{
		Status:    outcome.Status,
		Stopping:  outcome.Stopping,
		SettledAt: outcome.SettledAt,
		ErrorCode: outcome.ErrorCode,
	})
}

// truthyCurrentApplicationTaskFlag reproduces legacy's own parse:
// `request.args.get("meta", "no").lower().strip() in ["yes", "true"]`.
func truthyCurrentApplicationTaskFlag(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "yes", "true":
		return true
	default:
		return false
	}
}

func writeCurrentApplicationTaskStatusError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agentexecutionapp.ErrInvalidCurrentAgentTaskStatus):
		writeError(writer, http.StatusBadRequest, "Invalid agent task status request")
	case errors.Is(err, agentexecutionapp.ErrCurrentAgentTaskStatusNotFound):
		// Absent, foreign project, or another actor's run — one answer, so the
		// route cannot enumerate a project's response messages.
		writeError(writer, http.StatusNotFound, "Task not found")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(writer, http.StatusGatewayTimeout, "Agent task status request timed out")
	default:
		writeError(writer, http.StatusBadGateway, "Error occurred while reading task status")
	}
}
