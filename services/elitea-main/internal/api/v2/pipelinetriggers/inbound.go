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

// InboundProviderPath is the same route with the provider SUFFIX a preset
// mints (#970) — `/github` today.
//
// It exists because a sender's configuration screen is given ONE url and must
// be able to keep it: GitHub's webhook form takes a payload URL, and the URL
// this service hands out for a GitHub-preset trigger ends in `/github`. chi
// will not match that against the bare pattern, so it is a second
// registration of the same handler rather than a wildcard — a wildcard would
// also match `/anything`, and a URL that is not the one we minted must not
// quietly work.
//
// The segment is DECORATION. It never selects the validation: the stored
// `auth_mode` does. What the handler does with it is check it against the
// stored provider, so a `/github` suffix on a bearer trigger is refused
// instead of being ignored.
const InboundProviderPath = InboundPath + "/{provider}"

// TriggerTokenHeader is the preferred way to present the secret.
//
// Four carriers are accepted, in this order: `Authorization: Bearer`, this
// header, GitLab's `X-Gitlab-Token` (GitLabTokenHeader), then `?token=`. The query parameter is LAST and is documented as the
// least safe: a URL is written to proxy and browser logs, and a credential in
// one outlives the request. It is accepted at all because a large share of
// webhook senders cannot set a header, and a trigger nobody can call is not a
// feature. This handler never logs the raw target.
const TriggerTokenHeader = "X-Elitea-Trigger-Token" //nolint:gosec // header NAME, not a credential

// TriggerTokenQueryParam is the last-resort carrier.
const TriggerTokenQueryParam = "token" //nolint:gosec // parameter NAME, not a credential

// maxInboundBody bounds a custom BEARER call's body, and every settings body.
// A provider trigger's body is the provider's payload and gets the larger cap
// (inboundBodyWithinCap).
//
// 64 KiB is generous for what this route reads out of a body: one `input`
// string the pipeline sees. A bearer sender is writing to an endpoint of ours
// with a secret we minted, so the body is decorative — there is no reason for
// it to be large and a cap that says so is one less thing to pay for.
const maxInboundBody = int64(64 * 1024)

// maxSignedInboundBody bounds a SIGNED call's body, and it has to be bigger
// because the body is not ours to shape.
//
// A signing sender signs its own payload and we verify over the raw bytes, so
// "the body the sender chose to send" IS the request. GitHub documents 25 MB
// as its delivery maximum, and an ordinary push with many commits, or a
// pull_request event carrying a long description, passes 64 KiB without being
// unusual at all. Under the old single cap those deliveries were answered 413
// BEFORE the trigger row was even looked up: GitHub marked the delivery
// failed, and the pipeline could never run.
//
// 1 MiB rather than GitHub's own 25 MB. This is read into memory by an
// UNAUTHENTICATED caller — the credential check needs the bytes, so the read
// necessarily comes first — and the value is a bound on that exposure, not an
// attempt to accept everything a sender might produce. It covers the events a
// pipeline trigger is actually wired to; a 25 MB delivery is still refused,
// and refused with a 413 that says the body is too large.
const maxSignedInboundBody = int64(1024 * 1024)

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
// `input` is TEXT the pipeline or agent sees. `variables` re-values an
// AGENT's declared variables (agentrun.go) and is ignored for a pipeline.
// Neither is a place to select a project, a version or an identity: those
// come from the stored row (package doc, rule 1). A body that carries such
// keys is not refused, because a webhook sender's payload is not this
// service's to validate — it is simply not read.
//
// `variables` is kept RAW and decoded on its own (bodyVariables), so a
// provider payload that happens to carry a `variables` key of another shape
// does not also lose its `input`.
type inboundBody struct {
	Input     string          `json:"input"`
	Variables json.RawMessage `json:"variables"`
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

	// THE RAW BODY IS READ ONCE, FIRST, AND KEPT.
	//
	// A signature is computed over the bytes the sender signed, so the body
	// cannot be decoded and then re-read: `http.Request.Body` is a stream and
	// a second read of it yields nothing, which would make every signature
	// verify against an empty body — the one failure that would make a wrong
	// signature LOOK right for an empty payload. Reading it once, keeping the
	// bytes, and decoding the same bytes afterwards is what makes the check
	// replay-safe.
	//
	// It also moved AHEAD of the credential read, which #970 changed
	// deliberately: a signing trigger presents no bearer secret at all, so the
	// old "no credential presented" early refusal would have refused every
	// GitHub call before the row that says so was even looked up.
	raw, err := readInboundRaw(r)
	if err != nil {
		record(http.StatusRequestEntityTooLarge, projectID, 0, 0, "body too large")
		writeError(w, http.StatusRequestEntityTooLarge, "the request body is too large")
		return
	}
	body := decodeInboundBody(raw)

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

	// The URL SUFFIX, checked against the stored provider (#970). A suffix the
	// row does not carry is refused rather than ignored: the sender was given
	// one URL, and a second shape that also works is a second thing to get
	// wrong. It is checked before the credential for the same reason the
	// project id is — it says WHERE, not WHO — and it is no oracle, because a
	// caller already holds the whole URL it is comparing against.
	if provider := chi.URLParam(r, "provider"); provider != "" && provider != trigger.Provider {
		record(http.StatusUnauthorized, projectID, trigger.CreatedBy, trigger.VersionID,
			"the URL does not match this trigger's provider")
		writeError(w, http.StatusUnauthorized, refusal)
		return
	}

	// THE BEARER CAP, now that the row says which mode this is. See
	// `maxSignedInboundBody`: the read above is deliberately larger than a
	// bearer call is allowed, so the refusal moves here rather than
	// disappearing.
	if !inboundBodyWithinCap(trigger, raw) {
		record(http.StatusRequestEntityTooLarge, projectID, trigger.CreatedBy, trigger.VersionID,
			"body too large")
		writeError(w, http.StatusRequestEntityTooLarge, "the request body is too large")
		return
	}

	// EITHER a constant-time digest compare of the bearer secret, or a
	// constant-time HMAC compare over the raw body — whichever the STORED row
	// says. Both are one refusal, and neither runs the other's path: a signing
	// trigger does not accept the bearer secret, for the reason authmode.go
	// states.
	//
	// The revoked check comes AFTER it deliberately: checking revocation first
	// would answer a caller holding a wrong secret faster for a revoked
	// trigger than for a live one.
	authorized, unavailable := h.inboundCredentialAccepted(r, trigger, raw)
	if unavailable != "" {
		h.log().Error("pipelinetriggers: inbound credential check failed", "reason", unavailable)
		record(http.StatusServiceUnavailable, projectID, trigger.CreatedBy, trigger.VersionID, unavailable)
		writeError(w, http.StatusServiceUnavailable, "this trigger could not be checked")
		return
	}
	if !authorized {
		record(http.StatusUnauthorized, projectID, trigger.CreatedBy, trigger.VersionID, "wrong secret")
		writeError(w, http.StatusUnauthorized, refusal)
		return
	}
	if trigger.RevokedAt != nil {
		record(http.StatusUnauthorized, projectID, trigger.CreatedBy, trigger.VersionID, "trigger is revoked")
		writeError(w, http.StatusUnauthorized, refusal)
		return
	}

	// ONE RUN PER SIGNED DELIVERY (deliveries.go). Claimed only now, after
	// the signature and the revocation check: an unauthenticated request
	// must not be able to write a row, and a refused one must not occupy a
	// key a genuine delivery will need.
	deliveryKey := signedDeliveryKey(trigger, r.Header, raw)
	if deliveryKey != "" {
		state, previous, claimErr := h.claimDelivery(r.Context(), schema, trigger.TokenID, deliveryKey)
		switch {
		case claimErr != nil:
			// Fail CLOSED. Admitting without the claim is exactly the
			// replay this check exists to stop, and 503 asks the sender to
			// retry, which a provider does.
			h.log().Error("pipelinetriggers: inbound delivery claim failed", "err", claimErr)
			record(http.StatusServiceUnavailable, projectID, trigger.CreatedBy, trigger.VersionID,
				"delivery log unavailable")
			writeError(w, http.StatusServiceUnavailable, "this trigger could not be checked")
			return
		case state == deliveryInFlight:
			record(http.StatusServiceUnavailable, projectID, trigger.CreatedBy, trigger.VersionID,
				"this delivery is already being admitted")
			w.Header().Set("Retry-After", deliveryRetryAfterSeconds)
			writeError(w, http.StatusServiceUnavailable, "this delivery is already being admitted; retry it shortly")
			return
		case state == deliveryAdmitted:
			// The SAME answer the first copy got, and no second run. The
			// caller has proved it holds this exact signed delivery, so
			// handing back the run it started is no disclosure.
			versionID := previous.VersionID
			if versionID == 0 {
				versionID = trigger.VersionID
			}
			record(http.StatusAccepted, projectID, trigger.CreatedBy, trigger.VersionID,
				"a repeated delivery; answered with the run it already started")
			writeAccepted(w, projectID, runOutcome{
				ExecutionID:      previous.ExecutionID,
				ConversationUUID: previous.ConversationUUID,
				VersionID:        versionID,
			})
			return
		}
	}

	outcome, err := h.admit(r.Context(), schema, runRequest{
		ProjectID:   projectID,
		ActorUserID: trigger.CreatedBy,
		VersionID:   trigger.VersionID,
		Input:       body.Input,
		Origin:      OriginWebhook,
		// Legacy issue 6656: a trigger may start an ordinary agent too. The
		// raw payload and the variables are read only on that branch.
		Kinds:     pipelinesAndAgents,
		Payload:   raw,
		Variables: bodyVariables(body),
	})
	if deliveryKey != "" {
		if err != nil {
			h.releaseDelivery(r.Context(), schema, trigger.TokenID, deliveryKey)
		} else {
			h.completeDelivery(r.Context(), schema, trigger.TokenID, deliveryKey, outcome)
		}
	}
	switch {
	case errors.Is(err, ErrRunForbidden):
		record(http.StatusUnauthorized, projectID, trigger.CreatedBy, trigger.VersionID,
			"the trigger creator no longer holds "+RunPermission)
		writeError(w, http.StatusUnauthorized, refusal)
		return
	case errors.Is(err, ErrVersionNotRunnable):
		record(http.StatusUnauthorized, projectID, trigger.CreatedBy, trigger.VersionID,
			"the version is gone")
		writeError(w, http.StatusUnauthorized, refusal)
		return
	case errors.Is(err, ErrInputTooLarge):
		record(http.StatusBadRequest, projectID, trigger.CreatedBy, trigger.VersionID, "input too large")
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("`input` is too large: it may hold at most %d KiB of text", maxRunInput/1024))
		return
	case errors.Is(err, ErrInvalidInput):
		// 422 and the field's name. The credential was accepted, so this is
		// a statement about the BODY the caller chose, and it is no oracle.
		record(http.StatusUnprocessableEntity, projectID, trigger.CreatedBy, trigger.VersionID, "input is not valid")
		writeError(w, http.StatusUnprocessableEntity,
			"`input` is not valid: it must be UTF-8 text without NUL characters")
		return
	case errors.Is(err, ErrAgentInputRequired):
		// 422 and the field's name, for the reason ErrInvalidInput gives. An
		// agent answers a message, so a call with no `input` and no payload
		// has nothing for it to answer.
		record(http.StatusUnprocessableEntity, projectID, trigger.CreatedBy, trigger.VersionID,
			"an agent run needs input")
		writeError(w, http.StatusUnprocessableEntity,
			"`input` is required: this trigger starts an agent, and an agent needs text or a payload to read")
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
	writeAccepted(w, projectID, outcome)
}

// writeAccepted is the 202 body, for an admitted run and for a repeated
// delivery answered with the run its first copy admitted.
func writeAccepted(w http.ResponseWriter, projectID int64, outcome runOutcome) {
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

// presentedSecret reads the credential from the four accepted carriers.
//
// `X-Gitlab-Token` is GitLab's: a GitLab webhook's SECRET TOKEN is sent
// verbatim in it, and GitLab cannot be made to send it anywhere else. Before
// legacy issue 6664 a GitLab sender therefore had no way to present the
// secret except the query string. It is a carrier for a bearer trigger only;
// a signing trigger reads no carrier at all (inboundCredentialAccepted).
func presentedSecret(r *http.Request) string {
	if header := r.Header.Get("Authorization"); header != "" {
		if value, found := strings.CutPrefix(header, "Bearer "); found {
			return strings.TrimSpace(value)
		}
	}
	if header := strings.TrimSpace(r.Header.Get(TriggerTokenHeader)); header != "" {
		return header
	}
	if header := strings.TrimSpace(r.Header.Get(GitLabTokenHeader)); header != "" {
		return header
	}
	return strings.TrimSpace(r.URL.Query().Get(TriggerTokenQueryParam))
}

// inboundCredentialAccepted answers whether THIS request may use THIS trigger.
//
// It reports two things, because they are two different answers: `accepted` is
// the credential verdict, and `unavailable` is a non-empty reason this
// deployment could not reach one. A vault that will not open must not be
// reported as a wrong secret — that would tell a sender to change a credential
// that is correct, and would hide an outage behind a 401.
func (h *Handler) inboundCredentialAccepted(
	r *http.Request, trigger triggerRow, raw []byte,
) (accepted bool, unavailable string) {
	if !modeSigns(trigger.AuthMode) {
		presented := presentedSecret(r)
		if presented == "" {
			return false, ""
		}
		return subtle.ConstantTimeCompare(secretDigest(presented), trigger.TokenHash) == 1, ""
	}

	// A SIGNATURE CANNOT BE CHECKED AGAINST A DIGEST, so this is the one
	// inbound path that opens the hidden vault. 0133's header says the inbound
	// path never does; 0138's says that sentence is now true of the `token`
	// mode only, and this is where the exception lives.
	if h.vault == nil {
		return false, "the credential store is not available on this deployment"
	}
	header := trigger.SignatureHeader
	if header == "" {
		// Unstorable since 0138's CHECK, so a row like this predates the mode
		// or was written around the route. It is a configuration fault, not a
		// sender's mistake.
		return false, "this trigger has a signature mode and no signature header"
	}
	presented := r.Header.Get(header)
	if strings.TrimSpace(presented) == "" {
		return false, ""
	}
	secret, err := h.vault.LookupAdminHiddenSecret(r.Context(), trigger.SecretName)
	if err != nil {
		return false, "the stored credential for this trigger could not be read"
	}
	if trigger.AuthMode == AuthModeStandardWebhooks {
		return standardWebhooksSignatureMatches(r.Header, raw, secret, time.Now()), ""
	}
	return signatureMatches(presented, raw, secret), ""
}

// readInboundRaw reads at most maxSignedInboundBody bytes and returns them.
//
// The BYTES are what the caller signed, so they are what a signature is
// verified over. Everything this handler needs from the body is derived from
// this one read; see the call site for why a second read would be a defect
// rather than an inefficiency.
//
// IT READS TO THE LARGER CAP because the mode is not known yet — the trigger
// row that says which one this is has not been looked up, and it cannot be:
// the lookup is keyed by a token id, and refusing before it would refuse every
// signed delivery, which is the defect this pair of caps closes. The bearer
// cap is applied AFTER the row is read (`inboundBodyWithinCap`), so a bearer
// caller gains nothing from the larger read but the same 413 one step later.
func readInboundRaw(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	limited := http.MaxBytesReader(nil, r.Body, maxSignedInboundBody)
	return io.ReadAll(limited)
}

// inboundBodyWithinCap applies the cap the STORED row calls for.
//
// The question is not "does this mode sign" but "who shapes the body". A
// signing trigger's body is the sender's own payload, and so is a GitLab
// SECRET-TOKEN trigger's: its mode is `token`, but what arrives is GitLab's
// event JSON — a push listing up to twenty commits with their file lists, a
// merge request carrying its description and changes — which passes 64 KiB as
// easily as the GitHub payloads `maxSignedInboundBody` describes. Holding it to
// the bearer cap answered those deliveries 413 after the lookup, GitLab marked
// the webhook failed, and the pipeline never ran: #970's defect again, on the
// preset meant for GitLab.
//
// Everything else — a custom bearer trigger, whose body a person or a script
// of ours writes — is held to the 64 KiB a decorative body has no reason to
// exceed.
func inboundBodyWithinCap(trigger triggerRow, raw []byte) bool {
	if senderShapesBody(trigger) {
		return true
	}
	return int64(len(raw)) <= maxInboundBody
}

// senderShapesBody reports whether the trigger's body is a provider's own
// event payload rather than one written for this route.
func senderShapesBody(trigger triggerRow) bool {
	return modeSigns(trigger.AuthMode) || trigger.Provider == ProviderGitLab || trigger.Provider == ProviderGitHub
}

// decodeInboundBody reads the accepted fields out of the raw bytes.
//
// An EMPTY body is valid and common: a webhook that only says "something
// happened" carries nothing this service should require. An unparseable body is
// also accepted, as an empty input, because the sender's payload format is not
// this service's contract — refusing it would break integrations over a field
// the pipeline never reads. A GitHub push payload is exactly that case: it is
// valid JSON with no `input` key at all.
//
// `input` is NOT CUT here. It used to be cut at maxRunInput BYTES, which split
// a multi-byte character: a valid 18000-byte `input` of '€' became 16384 bytes
// that were not UTF-8, and the sender was told 422 "`input` is not valid" for
// a body that was fine. It also made admit's ErrInputTooLarge unreachable, so
// an agent's explicit `input` was silently shortened. An `input` over the
// limit now reaches admit whole and is refused there with 400, which names the
// limit. Nothing is run on a text the caller did not send.
func decodeInboundBody(raw []byte) inboundBody {
	var body inboundBody
	if len(raw) == 0 {
		return body
	}
	_ = json.Unmarshal(raw, &body)
	return body
}

// bodyVariables decodes the body's `variables` object. Anything that is not a
// JSON object supplies no variables.
func bodyVariables(body inboundBody) map[string]json.RawMessage {
	if len(body.Variables) == 0 {
		return nil
	}
	var variables map[string]json.RawMessage
	if err := json.Unmarshal(body.Variables, &variables); err != nil {
		return nil
	}
	return variables
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
