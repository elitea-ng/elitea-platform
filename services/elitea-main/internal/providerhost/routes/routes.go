// Package routes is the provider facade's route table: the three paths and
// four methods every facade mounts, each behind the same authentication and
// per-project permission guard, each forwarding to the SPI path the provider
// serves. DeepWiki and Inventory carried this table twice, line for line
// (ADR-0023 context); a third facade would have carried it a third time.
//
// What a facade still owns: its path patterns (the parity gate and the
// router name them), its permission strings and mode, its hop, and — for
// DeepWiki — an invoke handler that rewrites the body before forwarding.
package routes

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/spi"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhub"
)

// ErrIncompleteTable is returned for a table that cannot serve: a missing
// path, permission or hop, or a credential set that authenticates nobody.
var ErrIncompleteTable = errors.New("incomplete provider facade route table")

// Forwarder is the hop: providerhost/proxy's Forward, or a test's recorder.
type Forwarder func(w http.ResponseWriter, r *http.Request, providerPath, projectID, userID string)

// Table describes one facade's routes.
type Table struct {
	// The facade's own path patterns, each carrying {project_id}; Invoke and
	// Invocation also carry {toolkit_name} and {tool_name}, Invocation
	// {invocation_id} as well.
	SlotsPath, InvokePath, InvocationPath string
	// The permission mode and the two permissions: reading (slots, polling)
	// and invoking (starting, cancelling). Polling and cancelling share a
	// path and do NOT share a permission.
	Mode, ReadPermission, InvokePermission string
	Auth                                   apimw.AuthConfig
	Permissions                            auth.PermissionResolver
	// Forward is the hop every route ends in.
	Forward Forwarder
	// Poll, when set, serves GET InvocationPath instead of Forward, and is
	// the ONLY route it replaces.
	//
	// It exists because the terminal poll is the one place a facade sees the
	// answer a provider produced: the browser drains its result THROUGH this
	// hop, so a facade that wants to record a conversation has to observe it
	// here or not at all. DeepWiki's wiki chat does exactly that.
	//
	// Deliberately not applied to DELETE on the same path: a cancel returns
	// no answer to record, and it is guarded by the other permission.
	Poll Forwarder
	// Invoke, when set, serves POST InvokePath instead of a plain forward —
	// DeepWiki rewrites the body (credentials, the callback grant) first.
	// It runs behind the same guard.
	Invoke http.HandlerFunc
	// UserID resolves the acting user for the signed identity headers;
	// nil means facade.UserID.
	UserID func(*http.Request) string
	// Admission, when set, is asked before an INVOKE is served: whether this
	// deployment still admits the provider (internal/providerhost/admission).
	// Nil forwards unchanged.
	//
	// THE INVOKE ONLY, and the other three routes are not an oversight:
	//
	//   - /slots is what the UI polls to decide whether to offer the button.
	//     Refusing it would blank the page rather than explain the refusal,
	//     and it starts nothing.
	//   - POLLING an invocation must keep working, or a run accepted before a
	//     revocation becomes unobservable — the caller is left with a provider
	//     doing work they can no longer see.
	//   - CANCELLING must keep working for that same run. A gate that refused
	//     the cancel would leave the only way to stop the work behind the
	//     control that just turned the provider off.
	//
	// Admission decides what may START. What is already running is finished
	// or stopped by its owner.
	Admission facade.AdmissionHook
}

// Build returns the table's handler, or ErrIncompleteTable.
func Build(t Table) (http.Handler, error) {
	if t.SlotsPath == "" || t.InvokePath == "" || t.InvocationPath == "" ||
		t.Mode == "" || t.ReadPermission == "" || t.InvokePermission == "" ||
		t.Forward == nil || !facade.Composable(t.Auth, t.Permissions) {
		return nil, ErrIncompleteTable
	}
	userID := t.UserID
	if userID == nil {
		userID = facade.UserID
	}
	guard := func(permission string) func(http.Handler) http.Handler {
		return facade.Guard(t.Auth, t.Permissions, t.Mode, permission)
	}
	forward := func(providerPath func(*http.Request) string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Forward(w, r, providerPath(r), chi.URLParam(r, "project_id"), userID(r))
		})
	}
	invoke := forward(func(r *http.Request) string {
		return spi.InvokePath(chi.URLParam(r, "toolkit_name"), chi.URLParam(r, "tool_name"))
	})
	if t.Invoke != nil {
		invoke = t.Invoke
	}
	if t.Admission != nil {
		invoke = admitted(t.Admission, invoke)
	}
	router := chi.NewRouter()
	router.Method(http.MethodGet, t.SlotsPath,
		guard(t.ReadPermission)(forward(func(*http.Request) string { return spi.SlotsPath })))
	router.Method(http.MethodPost, t.InvokePath, guard(t.InvokePermission)(invoke))
	poll := forward(invocationPath)
	if t.Poll != nil {
		poll = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Poll(w, r, invocationPath(r), chi.URLParam(r, "project_id"), userID(r))
		})
	}
	router.Method(http.MethodGet, t.InvocationPath, guard(t.ReadPermission)(poll))
	router.Method(http.MethodDelete, t.InvocationPath, guard(t.InvokePermission)(forward(invocationPath)))
	return router, nil
}

// admitted asks the hook before serving, and answers 503 when it refuses.
//
// It wraps the HANDLER rather than the route, so it runs INSIDE the guard:
// whether this deployment admits a provider is information about the
// deployment, and an unauthenticated caller learns nothing from a 401.
func admitted(hook facade.AdmissionHook, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allow, reason := hook(r); !allow {
			// 503 and not 403: the caller's permissions are fine, and
			// retrying with different credentials will not help. This is the
			// deployment declining to route to a provider — which is what the
			// disabled-facade path already answers.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"reason":  reason,
				"message": admissionMessage(reason),
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// admissionMessage is the sentence that goes with a refusal code.
//
// ONE SENTENCE FOR THE ROW AND THE REFUSAL in the inactive case: an operator
// reading the admitted revision and a user reading the 503 must not be told
// two different things, which is why providerhub.InactiveReason is a constant
// and not a literal in either place.
//
// The revocation's own recorded reason is NOT echoed. It is an operator's
// free text, written for the audit trail and shown on the administration
// page, and a provider's callers are not its audience.
func admissionMessage(reason string) string {
	switch reason {
	case "provider_admission_inactive":
		return providerhub.InactiveReason
	case "provider_admission_revoked":
		return "this provider's admission has been revoked in this deployment; " +
			"an administrator must re-register it before it can be invoked again."
	default:
		return "this deployment does not currently admit this provider."
	}
}

func invocationPath(r *http.Request) string {
	return spi.InvocationPath(
		chi.URLParam(r, "toolkit_name"),
		chi.URLParam(r, "tool_name"),
		chi.URLParam(r, "invocation_id"))
}
