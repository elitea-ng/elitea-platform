package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
)

// Per-interrupt HITL decision API (libs/proto/contracts/fanout-interrupt-decisions-v1.md §6).
// Registered only when ELITEA_RUNTIME_EXECUTION_INTERRUPTS_API_ENABLED is true;
// no client calls it in Wave 1.
const (
	CurrentExecutionInterruptsPath        = "/api/v2/elitea_core/task/prompt_lib/{projectID}/{responseMessageID}/interrupts"
	CurrentExecutionInterruptDecisionPath = CurrentExecutionInterruptsPath + "/{interruptKey}/decision"
)

const (
	interruptListSchema   = "elitea.pipeline.fanout-interrupt-list.v1"
	interruptResultSchema = "elitea.pipeline.fanout-interrupt-decision-result.v1"
)

// ExecutionInterruptUseCase is the ledger as this route needs it.
type ExecutionInterruptUseCase interface {
	List(context.Context, domain.Selector) (domain.List, error)
	Decide(context.Context, domain.DecideInput) (domain.DecideResult, error)
}

type interruptListItem struct {
	Card     json.RawMessage `json:"card"`
	State    domain.State    `json:"state"`
	Revision int64           `json:"revision"`
}

type interruptListBody struct {
	Schema            string              `json:"schema"`
	ResponseMessageID string              `json:"response_message_id"`
	DecisionRevision  int64               `json:"decision_revision"`
	Interrupts        []interruptListItem `json:"interrupts"`
}

type interruptDecisionBody struct {
	Schema       string       `json:"schema"`
	InterruptKey string       `json:"interrupt_key"`
	State        domain.State `json:"state"`
	Revision     int64        `json:"revision"`
	RequestID    string       `json:"request_id"`
	Replay       bool         `json:"replay"`
}

// interruptLogFields is the only shape this route logs: ids, a key prefix,
// states and revisions. Values, display text, tool arguments and credential
// references have no field here, so they cannot reach a log line.
type interruptLogFields struct {
	responseMessageID string
	keyPrefix         string
	actorID           int64
	state             domain.State
	revision          int64
	replay            bool
}

func (f interruptLogFields) attrs() []any {
	return []any{
		"response_message_id", f.responseMessageID, "interrupt_key_prefix", f.keyPrefix,
		"actor_id", f.actorID, "state", string(f.state), "revision", f.revision, "replay", f.replay,
	}
}

func interruptKeyPrefix(key string) string {
	if len(key) < 12 {
		return ""
	}
	return key[:12]
}

func NewCurrentExecutionInterruptRoute(useCase ExecutionInterruptUseCase, authConfig apimw.AuthConfig, permissions auth.PermissionResolver) (http.Handler, error) {
	if useCase == nil || authConfig.PrincipalValidator == nil || authConfig.ForwardedIdentityVerifier == nil || permissions == nil {
		return nil, errors.New("execution interrupt route dependencies are required")
	}
	list := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selector, ok := interruptSelector(w, r)
		if !ok {
			return
		}
		if r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.URL.RawQuery != "" {
			writeError(w, http.StatusBadRequest, "Invalid interrupt request")
			return
		}
		listed, err := useCase.List(r.Context(), selector)
		if err != nil {
			writeInterruptError(w, r, err, interruptLogFields{responseMessageID: selector.ResponseMessageID, actorID: selector.ActorUserID})
			return
		}
		body := interruptListBody{Schema: interruptListSchema, ResponseMessageID: listed.ResponseMessageID, DecisionRevision: listed.DecisionRevision, Interrupts: make([]interruptListItem, 0, len(listed.Interrupts))}
		for _, item := range listed.Interrupts {
			body.Interrupts = append(body.Interrupts, interruptListItem{Card: item.Card, State: item.State, Revision: item.Revision})
		}
		writeJSON(w, http.StatusOK, body)
	})
	decide := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selector, ok := interruptSelector(w, r)
		if !ok {
			return
		}
		key := chi.URLParam(r, "interruptKey")
		fields := interruptLogFields{responseMessageID: selector.ResponseMessageID, keyPrefix: interruptKeyPrefix(key), actorID: selector.ActorUserID}
		if !domain.ValidDigest(key) {
			writeInterruptCode(w, http.StatusNotFound, "agent_interrupt_not_found", "This interrupt does not exist.")
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, domain.MaxDecisionBodyBytes))
		if err != nil || r.URL.RawQuery != "" {
			writeInterruptError(w, r, domain.ErrInvalidDecision, fields)
			return
		}
		decision, canonical, err := domain.ParseDecisionRequest(raw)
		if err != nil {
			writeInterruptError(w, r, err, fields)
			return
		}
		result, err := useCase.Decide(r.Context(), domain.DecideInput{Selector: selector, InterruptKey: key, Decision: decision, Canonical: canonical})
		if err != nil {
			writeInterruptError(w, r, err, fields)
			return
		}
		fields.state, fields.revision, fields.replay = result.State, result.Revision, result.Replay
		slog.InfoContext(r.Context(), "execution interrupt decided", fields.attrs()...)
		writeJSON(w, http.StatusOK, interruptDecisionBody{
			Schema: interruptResultSchema, InterruptKey: result.InterruptKey, State: result.State,
			Revision: result.Revision, RequestID: result.RequestID, Replay: result.Replay,
		})
	})
	resolveProject := func(r *http.Request) (string, bool) {
		id := chi.URLParam(r, "projectID")
		_, ok := positiveCanonicalID(id)
		return id, ok
	}
	router := chi.NewRouter()
	router.Method(http.MethodGet, CurrentExecutionInterruptsPath, apimw.Auth(authConfig)(
		apimw.RequireResolvedPermissionsForProject(permissions, auth.PermissionModeDefault, resolveProject, domain.PermissionList)(list)))
	router.Method(http.MethodPost, CurrentExecutionInterruptDecisionPath, apimw.Auth(authConfig)(
		apimw.RequireResolvedPermissionsForProject(permissions, auth.PermissionModeDefault, resolveProject, domain.PermissionDecide)(decide)))
	return router, nil
}

// interruptSelector reads the authenticated owning user and the path ids. A
// malformed response id cannot name anything, so it answers 404.
func interruptSelector(w http.ResponseWriter, r *http.Request) (domain.Selector, bool) {
	user, authenticated := auth.UserFromContext(r.Context())
	actor, owning := user.OwningUserID()
	if !authenticated || !owning {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return domain.Selector{}, false
	}
	projectID, valid := positiveCanonicalID(chi.URLParam(r, "projectID"))
	selector := domain.Selector{ProjectID: projectID, ActorUserID: actor, ResponseMessageID: chi.URLParam(r, "responseMessageID")}
	if !valid || !selector.Valid() {
		writeInterruptCode(w, http.StatusNotFound, "agent_interrupt_not_found", "This interrupt does not exist.")
		return domain.Selector{}, false
	}
	return selector, true
}

// writeInterruptCode writes the fanout-interrupt-error.v1 envelope.
func writeInterruptCode(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": code, "message": message, "retryable": false})
}

func writeInterruptError(w http.ResponseWriter, r *http.Request, err error, fields interruptLogFields) {
	switch {
	case errors.Is(err, domain.ErrInvalidDecision):
		writeInterruptCode(w, http.StatusBadRequest, "agent_interrupt_invalid_decision", "This decision is not valid for the interrupt.")
	case errors.Is(err, domain.ErrNotFound):
		writeInterruptCode(w, http.StatusNotFound, "agent_interrupt_not_found", "This interrupt does not exist.")
	case errors.Is(err, domain.ErrAlreadyResolved):
		writeInterruptCode(w, http.StatusConflict, "agent_interrupt_already_resolved", "This request was already answered.")
	case errors.Is(err, domain.ErrNotAllowed):
		writeError(w, http.StatusForbidden, "insufficient permissions")
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "Interrupt request timed out")
	default:
		slog.ErrorContext(r.Context(), "execution interrupt request failed", append(fields.attrs(), "err", err)...)
		writeError(w, http.StatusInternalServerError, "Interrupt request failed")
	}
}
