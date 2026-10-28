package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/modelruntoken"
)

// ModelRunTokenValidator authenticates a governed run's model run token
// (#1461, *modelruntoken.Validator).
type ModelRunTokenValidator interface {
	ValidateModelRunToken(ctx context.Context, token string) (auth.User, modelruntoken.Binding, error)
}

// Refusals of a model run token. Neither is a 401: the first is a token
// presented to a route outside its audience, the second a request that names
// a project or execution the token does not.
const (
	reasonModelRunTokenAudience = "model_run_token_audience"
	reasonModelRunTokenScope    = "model_run_token_scope"
)

// headerModelRunExecutionID is the execution-id header the /llm edge
// verifies (llmproxy.HeaderExecutionID). Restated so this package does not
// import the proxy.
const headerModelRunExecutionID = "X-Elitea-Execution-Id"

// serveModelRunToken authenticates a request whose bearer or API key is a
// model run token (modelruntoken.HasPrefix).
//
// THE AUDIENCE IS THE ROUTE GROUP. Only the /llm group's AuthConfig carries
// ModelRunTokens; on every other group the token is refused with 403 before
// any validator runs, so a governed worker's token reaches the model gateway
// and nothing else. The ordinary Validator never sees the token either: it is
// not a personal access token and must not be read as one.
//
// On /llm the token resolves to the run's actor and the actor's personal
// access token row, the principal check re-reads both, and the request may
// name only the token's own project and execution. With no project selector
// the token's project is set as the selector, so the project the request
// bills is always the run's.
func serveModelRunToken(next http.Handler, w http.ResponseWriter, r *http.Request, cfg AuthConfig, source, token string) {
	if cfg.ModelRunTokens == nil {
		logCredentialRefusal(r, source, reasonModelRunTokenAudience)
		writeJSONError(w, http.StatusForbidden, errTypePermission, reasonModelRunTokenAudience,
			"this token is valid only for the model gateway")
		return
	}
	user, binding, err := cfg.ModelRunTokens.ValidateModelRunToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, auth.ErrCredentialValidationUnavailable) {
			logCredentialRefusal(r, source, reasonTokenStoreUnavailable)
			slog.ErrorContext(r.Context(), "the model run token lease could not be read", "err", err)
			w.Header().Set("Retry-After", "5")
			writeJSONError(w, http.StatusServiceUnavailable,
				"server_error", reasonTokenStoreUnavailable, "token store unavailable")
			return
		}
		writeCredentialRefusal(w, r, source, reasonTokenRejected)
		return
	}
	user, err = validatePrincipal(r.Context(), cfg, user)
	if err != nil {
		writePrincipalRefusal(w, r, source, err)
		return
	}
	if !admitModelRunScope(w, r, source, user, binding) {
		return
	}
	serveAuthenticated(next, w, r, user, auth.AuthenticationSourceToken)
}

// admitModelRunScope refuses a request that names another project or
// execution than the token, and sets the token's project as the selector
// when the request names none.
func admitModelRunScope(w http.ResponseWriter, r *http.Request, source string, user auth.User, binding modelruntoken.Binding) bool {
	project := strconv.FormatInt(binding.ProjectID, 10)
	refuse := func() bool {
		logCredentialRefusal(r, source, reasonModelRunTokenScope)
		writeJSONError(w, http.StatusForbidden, errTypePermission, reasonModelRunTokenScope,
			"the request names a project or execution this token is not for")
		return false
	}
	// A personal access token bound to another project could not have
	// billed the run's project either.
	if user.TokenProjectID != nil && *user.TokenProjectID != binding.ProjectID {
		return refuse()
	}
	named := false
	for _, name := range projectSelectorHeaders {
		for _, value := range r.Header.Values(name) {
			if value != project {
				return refuse()
			}
			named = true
		}
	}
	for _, value := range r.Header.Values(headerModelRunExecutionID) {
		if value != binding.ExecutionID {
			return refuse()
		}
	}
	if !named {
		r.Header.Set(HeaderProjectSelector, project)
	}
	return true
}
