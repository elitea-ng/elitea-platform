// Package credentialconsent serves the provider OAuth consent flow of
// ADR-0039 decision 3 (issue #1121):
//
//	POST /api/v2/credential_consent/{provider}:start
//	GET  /api/v2/credential_consent/{provider}:callback
//
// Start answers the provider authorization URL for one (tenant project,
// agent binding, target binding). The state, the PKCE verifier and the
// exact redirect URI stay server side (internal/provideroauth). The
// provider redirects the user's browser to the callback, which consumes the
// state once, exchanges the code and seals the grant with a security audit
// event.
//
// router.go mounts the routes only when RouterConfig.CredentialConsent is
// set, which the composition root does only with credential governance on.
// Start is gated on models.credentials.governed.use in the body's project;
// the callback resolves the same permission in the state's project before
// it exchanges the code. No response holds a token, a code or a verifier.
package credentialconsent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
)

const maxStartBody = 4 << 10

// Flow is the consent flow (*provideroauth.Consent).
type Flow interface {
	Start(ctx context.Context, r provideroauth.StartRequest) (provideroauth.StartResult, error)
	Callback(ctx context.Context, r provideroauth.CallbackRequest, authorize provideroauth.Authorize) (provideroauth.CallbackResult, error)
	RecordRefusal(ctx context.Context, userID int64, provider, reason string) error
}

// Handler serves the consent routes.
type Handler struct {
	flow   Flow
	logger *slog.Logger
}

// New builds the handler.
func New(flow Flow, logger *slog.Logger) (*Handler, error) {
	if flow == nil {
		return nil, errors.New("credential consent: a consent flow is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{flow: flow, logger: logger}, nil
}

type startBody struct {
	ProjectID       int64  `json:"project_id"`
	AgentBindingID  string `json:"agent_binding_id"`
	TargetBindingID string `json:"target_binding_id"`
}

type startKey struct{}

// DecodeStart reads the start body once and keeps it in the request
// context, so the permission gate can read the project from it
// (StartProject) and Start reads the same value.
func DecodeStart(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxStartBody+1))
		if err != nil || len(raw) > maxStartBody {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		var body startBody
		if err := decoder.Decode(&body); err != nil || body.ProjectID <= 0 {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), startKey{}, body)))
	})
}

// StartProject is the permission gate's project: the decoded body's
// project_id.
func StartProject(r *http.Request) (string, bool) {
	body, ok := r.Context().Value(startKey{}).(startBody)
	if !ok || body.ProjectID <= 0 {
		return "", false
	}
	return strconv.FormatInt(body.ProjectID, 10), true
}

func provider(r *http.Request, suffix string) string {
	return strings.TrimSuffix(chi.URLParam(r, "provider"), suffix)
}

type startView struct {
	AuthorizationURL string `json:"authorization_url"`
	ExpiresAt        string `json:"expires_at"`
}

// Start serves POST /credential_consent/{provider}:start. It runs behind
// DecodeStart and the permission gate.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	body, ok := r.Context().Value(startKey{}).(startBody)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	userID, err := strconv.ParseInt(user.UserID, 10, 64)
	if err != nil || userID <= 0 {
		writeError(w, http.StatusForbidden, "the caller has no resolved user id")
		return
	}
	result, err := h.flow.Start(r.Context(), provideroauth.StartRequest{
		UserID: userID, Provider: provider(r, ":start"), TenantProjectID: body.ProjectID,
		AgentBindingID: body.AgentBindingID, TargetBindingID: body.TargetBindingID,
	})
	switch {
	case err == nil:
	case errors.Is(err, provideroauth.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid consent request")
		return
	case errors.Is(err, provideroauth.ErrProviderNotConfigured):
		writeError(w, http.StatusNotFound, "no OAuth application is configured for this provider")
		return
	case errors.Is(err, provideroauth.ErrTargetNotFound):
		writeError(w, http.StatusNotFound, "no such target binding for this provider in the project")
		return
	case errors.Is(err, provideroauth.ErrBindingNotUsable):
		writeError(w, http.StatusConflict, "the agent binding is not active in the project")
		return
	default:
		h.logger.ErrorContext(r.Context(), "credential consent start failed", "error", err)
		writeError(w, http.StatusInternalServerError, "credential consent: internal error")
		return
	}
	writeJSON(w, http.StatusOK, startView{
		AuthorizationURL: result.AuthorizationURL, ExpiresAt: result.ExpiresAt.UTC().Format(time.RFC3339Nano),
	})
}

type callbackView struct {
	Status          string `json:"status"`
	Provider        string `json:"provider"`
	ProjectID       int64  `json:"project_id"`
	AgentBindingID  string `json:"agent_binding_id"`
	TargetBindingID string `json:"target_binding_id"`
}

// errNotPermitted refuses a callback whose caller lost the permission.
var errNotPermitted = errors.New("credential consent: the caller may not consent in the project")

// Callback serves GET /credential_consent/{provider}:callback. resolver
// resolves the caller's permissions in the state's project.
func (h *Handler) Callback(resolver auth.PermissionResolver, mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := auth.UserFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		name := provider(r, ":callback")
		query := r.URL.Query()
		authorize := func(ctx context.Context, tenantProjectID int64) (int64, error) {
			if resolver == nil {
				return 0, errNotPermitted
			}
			resolution, err := resolver.ResolvePermissions(ctx, user, mode, strconv.FormatInt(tenantProjectID, 10))
			if errors.Is(err, auth.ErrPermissionDenied) {
				return 0, errNotPermitted
			}
			if err != nil {
				return 0, err
			}
			for _, p := range resolution.Permissions {
				if p == credentialgrant.PermissionUse {
					return resolution.UserID, nil
				}
			}
			return 0, errNotPermitted
		}
		result, err := h.flow.Callback(r.Context(), provideroauth.CallbackRequest{
			Provider: name, State: query.Get("state"), Code: query.Get("code"), ProviderError: query.Get("error"),
		}, authorize)
		switch {
		case err == nil:
		case errors.Is(err, provideroauth.ErrProviderNotConfigured):
			writeError(w, http.StatusNotFound, "no OAuth application is configured for this provider")
			return
		case errors.Is(err, provideroauth.ErrStateInvalid):
			h.refusal(r.Context(), user, name, "consent.state_invalid")
			writeError(w, http.StatusBadRequest, "the consent request is unknown, expired or already used")
			return
		case errors.Is(err, provideroauth.ErrConsentDenied):
			h.refusal(r.Context(), user, name, "consent.declined")
			writeError(w, http.StatusForbidden, "the provider did not grant consent")
			return
		case errors.Is(err, errNotPermitted):
			h.refusal(r.Context(), user, name, "consent.permission_missing")
			writeError(w, http.StatusForbidden, "insufficient permissions")
			return
		case errors.Is(err, provideroauth.ErrInvalidGrant), errors.Is(err, provideroauth.ErrProviderRejected):
			h.refusal(r.Context(), user, name, "consent.code_rejected")
			writeError(w, http.StatusBadGateway, "the provider refused the authorization code")
			return
		case errors.Is(err, provideroauth.ErrProviderUnavailable):
			writeError(w, http.StatusBadGateway, "the provider token endpoint is unavailable")
			return
		case errors.Is(err, provideroauth.ErrTargetNotFound):
			h.refusal(r.Context(), user, name, "consent.target_missing")
			writeError(w, http.StatusNotFound, "no such target binding for this provider in the project")
			return
		case errors.Is(err, provideroauth.ErrBindingNotUsable):
			h.refusal(r.Context(), user, name, "consent.binding_not_usable")
			writeError(w, http.StatusConflict, "the agent binding is not active in the project")
			return
		case errors.Is(err, provideroauth.ErrGrantConflict):
			h.refusal(r.Context(), user, name, "consent.conflict")
			writeError(w, http.StatusConflict, "another consent for this grant completed first; start consent again")
			return
		case errors.Is(err, provideroauth.ErrInvalid):
			// The exchange succeeded but the provider answered no access token.
			h.refusal(r.Context(), user, name, "consent.token_invalid")
			writeError(w, http.StatusBadGateway, "the provider answered without an access token")
			return
		default:
			h.logger.ErrorContext(r.Context(), "credential consent callback failed", "error", err)
			writeError(w, http.StatusInternalServerError, "credential consent: internal error")
			return
		}
		writeJSON(w, http.StatusOK, callbackView{
			Status: "connected", Provider: result.Provider, ProjectID: result.TenantProjectID,
			AgentBindingID: result.AgentBindingID, TargetBindingID: result.TargetBindingID,
		})
	}
}

// refusal audits a refused callback. The answer does not wait on it: the
// state is already consumed and nothing was written.
func (h *Handler) refusal(ctx context.Context, user auth.User, provider, reason string) {
	userID, err := strconv.ParseInt(user.UserID, 10, 64)
	if err != nil || userID <= 0 {
		return
	}
	if err := h.flow.RecordRefusal(ctx, userID, provider, reason); err != nil {
		h.logger.WarnContext(ctx, "credential consent refusal was not audited", "reason", reason, "error", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
