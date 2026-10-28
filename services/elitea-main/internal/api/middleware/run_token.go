package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/runtoken"
)

// RunTokenValidator authenticates a governed run's run token for one
// audience (#1461, *runtoken.Validator).
type RunTokenValidator interface {
	ValidateRunToken(ctx context.Context, token, audience string) (auth.User, runtoken.Binding, error)
}

// RunTokenScope admits a governed run's run token on ONE route group: the
// group that serves Audience. A group whose AuthConfig carries no scope
// refuses the token with 403.
type RunTokenScope struct {
	// Audience is the audience this group serves (runtoken.Audience*). The
	// token must list it.
	Audience  string
	Validator RunTokenValidator
	// Project answers the project the matched route names (its path
	// parameter); it must be the token's. Nil for a group whose routes name
	// none (/llm, where the selector header is the project): the token's
	// project is then set as the selector when the request names none.
	Project func(*http.Request) string
}

// Refusals of a run token. Neither is a 401: the first is a token presented
// to a route outside its audiences, the second a request that names a
// project or execution the token does not.
const (
	reasonRunTokenAudience = "run_token_audience"
	reasonRunTokenScope    = "run_token_scope"
)

// headerRunExecutionID is the execution-id header the /llm edge verifies
// (llmproxy.HeaderExecutionID). Restated so this package does not import the
// proxy.
const headerRunExecutionID = "X-Elitea-Execution-Id"

// queryRunProjectID is the query parameter a project-by-query route reads.
const queryRunProjectID = "project_id"

// serveRunToken authenticates a request whose bearer or API key is a run
// token (runtoken.HasPrefix).
//
// THE AUDIENCE IS THE ROUTE GROUP. Only the /llm group and the worker's
// artifact routes carry a RunTokenScope, each naming its own audience; on
// every other group the token is refused with 403 before any validator
// runs. The ordinary Validator never sees the token either: it is not a
// personal access token and must not be read as one.
//
// Where it is admitted, the token resolves to the run's actor and the
// actor's personal access token row, the principal check re-reads both, and
// the request may name only the token's own project and execution.
func serveRunToken(next http.Handler, w http.ResponseWriter, r *http.Request, cfg AuthConfig, source, token string) {
	scope := cfg.RunTokens
	if scope == nil || scope.Validator == nil || scope.Audience == "" {
		logCredentialRefusal(r, source, reasonRunTokenAudience)
		writeJSONError(w, http.StatusForbidden, errTypePermission, reasonRunTokenAudience,
			"this run token is not valid for this route")
		return
	}
	user, binding, err := scope.Validator.ValidateRunToken(r.Context(), token, scope.Audience)
	if err != nil {
		if errors.Is(err, auth.ErrCredentialValidationUnavailable) {
			logCredentialRefusal(r, source, reasonTokenStoreUnavailable)
			slog.ErrorContext(r.Context(), "the run token lease could not be read", "err", err)
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
	if !admitRunScope(w, r, source, scope, user, binding) {
		return
	}
	serveAuthenticated(next, w, r, user, auth.AuthenticationSourceToken)
}

// admitRunScope refuses a request that names another project or execution
// than the token, wherever it names one: the route's project, a project
// selector header, a project_id query parameter or the execution header.
func admitRunScope(w http.ResponseWriter, r *http.Request, source string, scope *RunTokenScope, user auth.User, binding runtoken.Binding) bool {
	project := strconv.FormatInt(binding.ProjectID, 10)
	refuse := func() bool {
		logCredentialRefusal(r, source, reasonRunTokenScope)
		writeJSONError(w, http.StatusForbidden, errTypePermission, reasonRunTokenScope,
			"the request names a project or execution this run token is not for")
		return false
	}
	// A personal access token bound to another project could not have
	// reached the run's project either.
	if user.TokenProjectID != nil && *user.TokenProjectID != binding.ProjectID {
		return refuse()
	}
	if scope.Project != nil && scope.Project(r) != project {
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
	for _, value := range r.URL.Query()[queryRunProjectID] {
		if value != project {
			return refuse()
		}
	}
	for _, value := range r.Header.Values(headerRunExecutionID) {
		if value != binding.ExecutionID {
			return refuse()
		}
	}
	if scope.Project == nil && !named {
		r.Header.Set(HeaderProjectSelector, project)
	}
	return true
}
