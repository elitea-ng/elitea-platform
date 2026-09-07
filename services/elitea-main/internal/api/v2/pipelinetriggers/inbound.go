package pipelinetriggers

// The inbound trigger — issue 192.
//
//	POST /api/v2/pipeline_trigger/{projectID}/{tokenID}
//
// Mounted on the ROOT mux, ABOVE the Auth group, exactly where the anonymous
// shared-chat routes, the branding bootstrap and the public icon routes are
// mounted. It is a route registered outside authentication, NOT an exemption
// threaded through the middleware: a path-matching skip inside Auth would be a
// second, weaker copy of the routing table, and the first time the two
// disagreed the exemption would win. internal/api/router.go states the same
// rule at the shared-chat mount.
//
// The four authorization rules this file implements are stated once, in the
// package doc, and are not restated here. What follows is what the WIRE looks
// like.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"crypto/subtle"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// InboundPath is the route pattern. It is exported so the router registers the
// same string the audit row records, rather than two copies that can drift.
const InboundPath = "/api/v2/pipeline_trigger/{projectID}/{tokenID}"

// TriggerTokenHeader is the preferred way to present the secret.
//
// Three carriers are accepted, in this order: `Authorization: Bearer`, this
// header, then `?token=`. The query parameter is LAST and is documented as the
// least safe: a URL is written to proxy and browser logs, and a credential in
// one outlives the request. It is accepted at all because a large share of
// webhook senders cannot set a header, and a trigger nobody can call is not a
// feature. This handler never logs the raw target.
const TriggerTokenHeader = "X-Elitea-Trigger-Token" //nolint:gosec // header NAME, not a credential

// TriggerTokenQueryParam is the last-resort carrier.
const TriggerTokenQueryParam = "token" //nolint:gosec // parameter NAME, not a credential

// maxInboundBody bounds what this route will read before deciding anything.
const maxInboundBody = int64(64 * 1024)

// refusal is the ONE answer to every unusable credential.
//
// Unknown project, unknown token id, wrong secret, revoked trigger, deleted
// version, creator without permission — all of them, one status and one
// sentence. A refusal that named which one it was would be an oracle for
// enumerating a deployment's pipelines and for probing which credentials still
// exist. The cost is stated rather than hidden: an operator debugging their own
// webhook gets no detail from the response, and has to read the audit row,
// which records the same request with the reason attached.
const refusal = "this trigger cannot be used"

// inboundBody is the accepted request body. Everything else in it is ignored.
//
// `input` is the only field, and it is TEXT the pipeline sees. It is not a
// place to select a project, a version or an identity: those come from the
// stored row (package doc, rule 1). A body that carries such keys is not
// refused, because a webhook sender's payload is not this service's to
// validate — it is simply not read.
type inboundBody struct {
	Input string `json:"input"`
}

// Trigger admits one inbound run.
func (h *Handler) Trigger(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	projectIDText := chi.URLParam(r, "projectID")
	tokenID := chi.URLParam(r, "tokenID")

	record := func(status int, projectID, userID, versionID int64, reason string) {
		h.recordInbound(r, started, status, projectID, userID, versionID, reason)
	}

	if h.pool == nil {
		record(http.StatusServiceUnavailable, 0, 0, 0, "no database on this deployment")
		writeError(w, http.StatusServiceUnavailable, "pipeline triggers are not available on this deployment")
		return
	}
	schema, err := tenantschema.Quote(projectIDText)
	if err != nil {
		record(http.StatusUnauthorized, 0, 0, 0, "malformed project id")
		writeError(w, http.StatusUnauthorized, refusal)
		return
	}
	projectID, err := strconv.ParseInt(projectIDText, 10, 64)
	if err != nil || projectID <= 0 {
		record(http.StatusUnauthorized, 0, 0, 0, "malformed project id")
		writeError(w, http.StatusUnauthorized, refusal)
		return
	}

	presented := presentedSecret(r)
	if presented == "" {
		record(http.StatusUnauthorized, projectID, 0, 0, "no credential presented")
		writeError(w, http.StatusUnauthorized, refusal)
		return
	}

	body, err := readInboundBody(r)
	if err != nil {
		record(http.StatusRequestEntityTooLarge, projectID, 0, 0, "body too large")
		writeError(w, http.StatusRequestEntityTooLarge, "the request body is too large")
		return
	}

	trigger, err := h.triggerByTokenID(r.Context(), schema, tokenID)
	switch {
	case errors.Is(err, ErrNotFound):
		// The compare below is skipped, which is the one place this handler's
		// duration differs by cause. It is unavoidable — there is no stored
		// digest to compare against — and it is not an oracle for the SECRET,
		// only for whether a 32-hex token id exists at all, which an attacker
		// would have to guess 128 bits to reach.
		record(http.StatusUnauthorized, projectID, 0, 0, "no such trigger")
		writeError(w, http.StatusUnauthorized, refusal)
		return
	case err != nil:
		h.log().Error("pipelinetriggers: inbound lookup failed", "err", err)
		record(http.StatusServiceUnavailable, projectID, 0, 0, "trigger lookup failed")
		writeError(w, http.StatusServiceUnavailable, "this trigger could not be checked")
		return
	}

	// CONSTANT TIME, on two fixed-width digests. The revoked check comes AFTER
	// it deliberately: checking revocation first would answer a caller holding
	// a wrong secret faster for a revoked trigger than for a live one.
	matches := subtle.ConstantTimeCompare(secretDigest(presented), trigger.TokenHash) == 1
	if !matches {
		record(http.StatusUnauthorized, projectID, trigger.CreatedBy, trigger.VersionID, "wrong secret")
		writeError(w, http.StatusUnauthorized, refusal)
		return
	}
	if trigger.RevokedAt != nil {
		record(http.StatusUnauthorized, projectID, trigger.CreatedBy, trigger.VersionID, "trigger is revoked")
		writeError(w, http.StatusUnauthorized, refusal)
		return
	}

	outcome, err := h.admit(r.Context(), schema, runRequest{
		ProjectID:   projectID,
		ActorUserID: trigger.CreatedBy,
		VersionID:   trigger.VersionID,
		Input:       body.Input,
		Origin:      OriginWebhook,
	})
	switch {
	case errors.Is(err, ErrRunForbidden):
		record(http.StatusUnauthorized, projectID, trigger.CreatedBy, trigger.VersionID,
			"the trigger creator no longer holds "+RunPermission)
		writeError(w, http.StatusUnauthorized, refusal)
		return
	case errors.Is(err, ErrVersionNotRunnable):
		record(http.StatusUnauthorized, projectID, trigger.CreatedBy, trigger.VersionID,
			"the pipeline version is gone or is not a pipeline")
		writeError(w, http.StatusUnauthorized, refusal)
		return
	case errors.Is(err, ErrInputTooLarge):
		record(http.StatusBadRequest, projectID, trigger.CreatedBy, trigger.VersionID, "input too large")
		writeError(w, http.StatusBadRequest, "the run input is too large")
		return
	case errors.Is(err, ErrRuntimeUnavailable):
		record(http.StatusServiceUnavailable, projectID, trigger.CreatedBy, trigger.VersionID,
			"no agent runtime on this deployment")
		writeError(w, http.StatusServiceUnavailable, "this deployment cannot run pipelines")
		return
	case err != nil:
		h.log().Error("pipelinetriggers: inbound run failed",
			"project_id", projectID, "version_id", trigger.VersionID, "err", err)
		record(http.StatusServiceUnavailable, projectID, trigger.CreatedBy, trigger.VersionID,
			"the run could not be started")
		writeError(w, http.StatusServiceUnavailable, "the pipeline run could not be started")
		return
	}

	h.stampTriggerUse(r.Context(), schema, trigger.ID)
	record(http.StatusAccepted, projectID, trigger.CreatedBy, trigger.VersionID, "")

	// 202, not 200: the answer is that the run was ADMITTED. It has not
	// finished, and it will not finish inside this request. The events URL is
	// how the caller follows it, which is what issue 192 asks the response to
	// return.
	writeJSON(w, http.StatusAccepted, map[string]any{
		"execution_id":    outcome.ExecutionID,
		"conversation_id": outcome.ConversationUUID,
		"project_id":      projectID,
		"version_id":      outcome.VersionID,
		"events_url":      EventsURL(projectID, outcome.ExecutionID),
	})
}

// EventsURL is the durable event stream for one execution — the same path the
// browser subscribes to (`runtimeEventsPath` in internal/api/router.go). It is
// built here rather than being hardcoded in the spec description so that the
// two cannot drift.
func EventsURL(projectID int64, executionID string) string {
	return fmt.Sprintf("/api/v2/executions/%d/%s/events", projectID, executionID)
}

// presentedSecret reads the credential from the three accepted carriers.
func presentedSecret(r *http.Request) string {
	if header := r.Header.Get("Authorization"); header != "" {
		if value, found := strings.CutPrefix(header, "Bearer "); found {
			return strings.TrimSpace(value)
		}
	}
	if header := strings.TrimSpace(r.Header.Get(TriggerTokenHeader)); header != "" {
		return header
	}
	return strings.TrimSpace(r.URL.Query().Get(TriggerTokenQueryParam))
}

// readInboundBody reads at most maxInboundBody bytes.
//
// An EMPTY body is valid and common: a webhook that only says "something
// happened" carries nothing this service should require. An unparseable body is
// also accepted, as an empty input, because the sender's payload format is not
// this service's contract — refusing it would break integrations over a field
// the pipeline never reads.
func readInboundBody(r *http.Request) (inboundBody, error) {
	if r.Body == nil {
		return inboundBody{}, nil
	}
	limited := http.MaxBytesReader(nil, r.Body, maxInboundBody)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return inboundBody{}, err
	}
	var body inboundBody
	if len(raw) == 0 {
		return body, nil
	}
	_ = json.Unmarshal(raw, &body)
	if len(body.Input) > maxRunInput {
		body.Input = body.Input[:maxRunInput]
	}
	return body, nil
}

// recordInbound writes the `centry.audit_events` row for one inbound call.
//
// It records EVERY outcome, including every refusal. A trail that holds only
// the successful calls cannot answer the question an operator opens it to ask —
// "is someone trying my webhook URLs?" — and the audit middleware, which
// records 401s produced BELOW it, cannot see this route at all.
//
// `HTTPRoute` is the PATTERN. The raw target may carry the secret in its query
// string, and a credential written into a table the admin page renders would
// outlive every rotation.
func (h *Handler) recordInbound(
	r *http.Request, started time.Time, status int,
	projectID, userID, versionID int64, reason string,
) {
	if h == nil || h.recorder == nil {
		return
	}
	action := "POST " + InboundPath
	if reason != "" {
		action += " — " + reason
	}
	statusCode := int32(status) //nolint:gosec // an HTTP status is far inside int32
	elapsed := float64(time.Since(started).Microseconds()) / 1000
	event := audit.Event{
		Timestamp:  time.Now().UTC(),
		EventType:  "api",
		Action:     action,
		HTTPMethod: http.MethodPost,
		HTTPRoute:  InboundPath,
		StatusCode: &statusCode,
		DurationMS: &elapsed,
		IsError:    status >= http.StatusBadRequest,
		EntityType: "pipeline",
	}
	if projectID > 0 {
		event.ProjectID = audit.ID(projectID)
	}
	if userID > 0 {
		event.UserID = audit.ID(userID)
	}
	if versionID > 0 {
		event.EntityID = audit.ID(versionID)
	}
	h.recorder.Record(r.Context(), event)
}
