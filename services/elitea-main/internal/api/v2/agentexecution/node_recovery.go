package agentexecution

import (
	"context"
	"errors"
	"io"
	"net/http"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/noderecovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"github.com/go-chi/chi/v5"
)

const CurrentNodeRecoveryPath = "/api/v2/elitea_core/task/prompt_lib/{projectID}/{responseMessageID}/node_recovery"
const CurrentNodeRecoveryActionPath = CurrentNodeRecoveryPath + "/actions"

type CurrentNodeRecoveryUseCase interface {
	Read(context.Context, app.Selector) (app.State, error)
	Submit(context.Context, app.Submission) (app.Outcome, error)
}

func NewCurrentNodeRecoveryRoute(useCase CurrentNodeRecoveryUseCase, authConfig apimw.AuthConfig, permissions auth.PermissionResolver) (http.Handler, error) {
	if useCase == nil || authConfig.PrincipalValidator == nil || authConfig.ForwardedIdentityVerifier == nil || permissions == nil {
		return nil, app.ErrInvalid
	}
	router := chi.NewRouter()
	endpoint := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		projectID, valid := positiveCanonicalID(chi.URLParam(r, "projectID"))
		user, authenticated := auth.UserFromContext(r.Context())
		actor, owning := user.OwningUserID()
		if !authenticated || !owning {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		selector := app.Selector{ProjectID: projectID, ActorUserID: actor, ResponseMessageID: chi.URLParam(r, "responseMessageID")}
		if !valid || selector.Validate() != nil {
			writeError(w, http.StatusBadRequest, "Invalid node recovery request")
			return
		}
		if r.Method == http.MethodGet {
			if r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.URL.RawQuery != "" {
				writeError(w, http.StatusBadRequest, "Invalid node recovery request")
				return
			}
			state, err := useCase.Read(r.Context(), selector)
			if err != nil {
				writeNodeRecoveryError(w, err, true)
				return
			}
			writeJSON(w, http.StatusOK, state)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
		if err != nil || r.URL.RawQuery != "" {
			writeError(w, http.StatusBadRequest, "Invalid node recovery request")
			return
		}
		request, err := domain.DecodeRequest(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid node recovery request")
			return
		}
		outcome, err := useCase.Submit(r.Context(), app.Submission{Selector: selector, Request: request})
		if err != nil {
			writeNodeRecoveryError(w, err, false)
			return
		}
		writeJSON(w, http.StatusAccepted, outcome)
	})
	resolveProject := func(r *http.Request) (string, bool) {
		id := chi.URLParam(r, "projectID")
		_, ok := positiveCanonicalID(id)
		return id, ok
	}
	read := apimw.RequireResolvedPermissionsForProject(permissions, auth.PermissionModeDefault, resolveProject, "models.applications.task.get")(endpoint)
	write := apimw.RequireResolvedPermissionsForProject(permissions, auth.PermissionModeDefault, resolveProject, "models.chat.messages.create")(endpoint)
	router.Method(http.MethodGet, CurrentNodeRecoveryPath, apimw.Auth(authConfig)(read))
	router.Method(http.MethodPost, CurrentNodeRecoveryActionPath, apimw.Auth(authConfig)(write))
	return router, nil
}

func writeNodeRecoveryError(w http.ResponseWriter, err error, read bool) {
	status := http.StatusBadGateway
	if errors.Is(err, app.ErrInvalid) {
		status = http.StatusBadRequest
	}
	if errors.Is(err, app.ErrNotAllowed) {
		status = http.StatusConflict
		if read {
			status = http.StatusNotFound
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	writeError(w, status, "Node recovery is unavailable for this request")
}
