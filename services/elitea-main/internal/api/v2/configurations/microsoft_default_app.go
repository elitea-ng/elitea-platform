package configurations

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftlogin"
)

// The Default app of a Microsoft credential (ADR-0057 decision 5): the credential
// holds only `oauth_app_id`, and the check resolves the client from the admin
// registry (internal/microsoftapps) instead of reading one off the credential.
//
// The registry applies its rule on EVERY use: the app is active, serves this
// credential type, serves this project, and (Dataverse) belongs to the credential's
// tenant. A refusal carries the registry's stable code and never falls back to
// another client. The check never reads the client secret: it describes the app
// (client id, authority, ceiling, refresh setting) and the secret stays in Main.

// MicrosoftAppDescriber is the registry's secret-free read (microsoftapps.Resolver).
type MicrosoftAppDescriber interface {
	Describe(ctx context.Context, req microsoftapps.Request) (microsoftapps.Resolved, error)
}

// WithMicrosoftApps supplies the Default app registry. Without it a credential that
// names an app is refused ("not available on this deployment").
func WithMicrosoftApps(describer MicrosoftAppDescriber) Option {
	return func(handler *Handler) {
		if describer != nil {
			handler.microsoftApps = describer
		}
	}
}

// defaultAppRefusalCode is the stable code a refused Default app carries in the
// answer, so a client can tell "ask an administrator" from "fix the credential".
const defaultAppRefusalCode = "code"

// resolveDefaultApp describes the app a credential names. It writes the refusal and
// reports false when the app cannot be used.
func (h *Handler) resolveDefaultApp(w http.ResponseWriter, r *http.Request, projectID, configType, appID string, data map[string]any) (microsoftapps.Resolved, bool) {
	if h.microsoftApps == nil {
		// No registry is composed: an app reference resolves to nothing, and falling
		// back to another client would sign the person in as someone the credential
		// never named.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "reason": ToolkitCheckReasonAuthFailed,
			"message": "Default apps are not available on this deployment yet. Use a Custom app: enter its client id and secret.",
		})
		return microsoftapps.Resolved{}, false
	}
	project, err := strconv.ParseInt(strings.TrimSpace(projectID), 10, 64)
	if err != nil || project <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "reason": ToolkitCheckReasonAuthFailed, "message": "The project is not valid.",
		})
		return microsoftapps.Resolved{}, false
	}
	tenant, _ := data[microsoftauth.FieldTenantID].(string)
	resolved, err := h.microsoftApps.Describe(r.Context(), microsoftapps.Request{
		AppID: appID, CredentialType: configType, ProjectID: project, TenantID: tenant,
	})
	if err == nil {
		return resolved, true
	}
	var refusal *microsoftapps.ResolveError
	if errors.As(err, &refusal) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "reason": ToolkitCheckReasonAuthFailed, "message": refusal.Message,
			defaultAppRefusalCode: refusal.Code,
		})
		return microsoftapps.Resolved{}, false
	}
	slog.ErrorContext(r.Context(), "check_connection: the Default app registry was not read", "type", configType, "error", err)
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"success": false, "reason": ToolkitCheckReasonUnreachable,
		"message": "The Default app registry could not be read. Try again shortly.",
	})
	return microsoftapps.Resolved{}, false
}

// defaultAppScopes is the sign-in scope list of a credential under a Default app. The
// rule lives with the sign-in (microsoftlogin.DefaultAppSignInScopes), so the check
// and the sign-in cannot ask for different scopes.
func defaultAppScopes(descriptor microsoftauth.Descriptor, app microsoftapps.Resolved, configType string, data map[string]any) (signIn, effective, removed []string, err error) {
	return microsoftlogin.DefaultAppSignInScopes(descriptor, app, configType, data)
}
