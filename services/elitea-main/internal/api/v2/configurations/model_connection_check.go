// model_connection_check.go is the llm_model branch of
// POST /configurations/check_connection/{projectID}/llm_model (legacy tracker
// issue 6793: "Test connection in the LLM model form").
//
// The credential test answers "does this key authenticate?". It reports
// success for a wrong model name, so an admin learns about that mistake only
// at the first chat message. This branch tests the MODEL instead, with the
// values in the form, saved or not:
//
//  1. Read the model name and the AI credentials reference from the posted
//     form, and the id of the saved row when the form edits one. Nothing else
//     of the form is used.
//  2. Resolve the referenced credential ON THE SERVER, through the same
//     expander and vault the runtime uses, and for the same identity: a
//     `private: true` reference names the personal project of the row's
//     AUTHOR, as admission and the stored checks resolve it. The browser never
//     holds the key, and it cannot add fields to the credential: the
//     reference is rebuilt from its title and private flag alone, so a posted
//     api_base cannot redirect a redeemed key.
//  3. Ask the gateway for one real completion with a one-token budget. The
//     gateway owns the egress allowlist and the address-validating dialer
//     (issue 13), so this process never dials the provider.
//
// The test saves nothing and changes nothing. It is a real, billed completion
// that the budget gate does not see, so four rules bound it:
//
//   - the route takes the UPDATE string as well as the CREATE one, because it
//     redeems a stored secret (Routes, handler.go);
//   - each project and user has a rate and a concurrency bound
//     (model_probe_limiter.go), and the gateway bounds its own concurrency;
//   - a credential resolved from the PUBLIC project (a platform credential)
//     tests only a model the platform already publishes with it. Without
//     that rule any editor could spend the platform key on any model name;
//   - every test is logged with the project, the user and the credential's
//     project, so the spend can be audited.
package configurations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
)

const llmModelConfigurationType = "llm_model"

// modelConnectionCheckClientTimeout is how long this process waits for the
// gateway's model probe. The gateway bounds the probe at 30 s; the extra time
// covers the hop itself, so a provider timeout arrives as the gateway's
// "timeout" verdict and not as a transport error here.
const modelConnectionCheckClientTimeout = 40 * time.Second

// modelConnectionMaxResponseBytes bounds the gateway reply this process reads.
const modelConnectionMaxResponseBytes = 64 << 10

// gatewayModelProbeKind is the gateway's marker for a model probe answer. A
// gateway that predates the model probe ignores `model` and answers the
// credential listing instead; that answer must never read as "Connected".
const gatewayModelProbeKind = "completion"

// modelProbeUnsupportedByGatewayMessage is the answer when the gateway has no
// model probe (a gateway built before it, or a rolling deploy). Trying again
// does not help, so the message does not say so.
const modelProbeUnsupportedByGatewayMessage = "Model testing is not supported by this LLM gateway version. " +
	"Update the LLM gateway to test a model."

// ModelConnectionCheck is one llm_model test: the RESOLVED credential and the
// form's model name.
type ModelConnectionCheck struct {
	CredentialType string
	// Credential is the plaintext credential payload. It goes to the gateway
	// and is dropped. It is never logged, stored or returned.
	Credential map[string]any
	Model      string
	// UserID is the caller, signed into the identity headers so the gateway
	// can attribute the probe in its log. Empty sends no user header.
	UserID string
}

// ModelConnectionResult is the gateway's verdict on one model test.
type ModelConnectionResult struct {
	Success bool
	// Reason is the gateway's machine-readable reason ("ok" on success).
	Reason string
	// Message is safe for the browser: a fixed category, then the provider's
	// own message after the gateway scrubbed it.
	Message   string
	LatencyMS int64
}

// ModelConnectionChecker tests one model through the gateway.
//
// Implementations MUST NOT dial a provider directly and MUST NOT report
// Success unless the model answered a completion.
type ModelConnectionChecker interface {
	CheckModel(ctx context.Context, check ModelConnectionCheck) (ModelConnectionResult, error)
}

var _ ModelConnectionChecker = (*GatewayConnectionChecker)(nil)

// modelConnectionCategories maps the gateway's reasons onto the six
// categories the form shows.
var modelConnectionCategories = map[string]string{
	"unauthorized":       "Authentication failed",
	"model_not_found":    "Model not found",
	"protocol_error":     "Wrong API protocol or route",
	"timeout":            "Timed out",
	"rate_limited":       "Rate limited",
	"unreachable":        "Connection failed",
	"upstream_error":     "Connection failed",
	"egress_not_allowed": "Connection failed",
}

// modelConnectionMessageFor builds the browser message for a failed test.
func modelConnectionMessageFor(reason, detail string) string {
	category, known := modelConnectionCategories[reason]
	if !known {
		// The credential's own field problems (no api_base, a bad region)
		// keep the credential test's wording.
		return connectionCheckMessageFor(reason, detail)
	}
	switch {
	case reason == "egress_not_allowed":
		detail = connectionCheckMessages[reason]
	case reason == "timeout" && detail == "":
		detail = "The model did not answer within 30 seconds."
	}
	if detail == "" {
		return category
	}
	return category + ": " + detail
}

// CheckModel implements ModelConnectionChecker over the same mTLS hop and the
// same identity signature as Check.
func (c *GatewayConnectionChecker) CheckModel(ctx context.Context, check ModelConnectionCheck) (ModelConnectionResult, error) {
	if c == nil || c.modelHTTPClient == nil {
		return ModelConnectionResult{}, errors.New("check model connection: the gateway client is not composed")
	}
	body := gatewayCredentialRequestBody(check.CredentialType, check.Credential)
	body.Model = check.Model
	raw, err := json.Marshal(body)
	if err != nil {
		return ModelConnectionResult{}, fmt.Errorf("check model connection: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/llm/v1/check_connection", bytes.NewReader(raw))
	if err != nil {
		return ModelConnectionResult{}, fmt.Errorf("check model connection: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	llmproxy.SignIdentityHeaders(req.Header, c.identitySecret, connectionCheckProjectIDFrom(ctx), check.UserID, "", "")

	resp, err := c.modelHTTPClient.Do(req)
	if err != nil {
		return ModelConnectionResult{}, fmt.Errorf("check model connection: call gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ModelConnectionResult{}, fmt.Errorf("check model connection: gateway responded with status %d", resp.StatusCode)
	}
	var out checkConnectionResponseBody
	if err := json.NewDecoder(io.LimitReader(resp.Body, modelConnectionMaxResponseBytes)).Decode(&out); err != nil {
		return ModelConnectionResult{}, fmt.Errorf("check model connection: decode gateway response: %w", err)
	}
	if out.Probe != gatewayModelProbeKind {
		// A gateway without the model probe answered the credential listing.
		// That is a permanent property of the deployed gateway, not a
		// transient failure, so it gets its own reason and message.
		return ModelConnectionResult{
			Reason: ToolkitCheckReasonUnsupportedType, Message: modelProbeUnsupportedByGatewayMessage,
		}, nil
	}
	if out.Success {
		return ModelConnectionResult{Success: true, Reason: out.Reason, Message: "Connected", LatencyMS: out.LatencyMS}, nil
	}
	return ModelConnectionResult{
		Reason: out.Reason, Message: modelConnectionMessageFor(out.Reason, out.Detail), LatencyMS: out.LatencyMS,
	}, nil
}

// gatewayCredentialRequestBody copies the credential fields the gateway reads
// into its wire body.
func gatewayCredentialRequestBody(configType string, data map[string]any) checkConnectionRequestBody {
	useAnthropic, _ := data["use_anthropic_endpoints"].(bool)
	return checkConnectionRequestBody{
		Type:                  configType,
		APIBase:               strVal(data, "api_base"),
		APIKey:                firstStrVal(data, "api_key", "api_token"),
		APIVersion:            strVal(data, "api_version"),
		AWSAccessKeyID:        strVal(data, "aws_access_key_id"),
		AWSSecretAccessKey:    strVal(data, "aws_secret_access_key"),
		AWSSessionToken:       strVal(data, "aws_session_token"),
		AWSRegionName:         strVal(data, "aws_region_name"),
		VertexProject:         strVal(data, "vertex_project"),
		VertexLocation:        strVal(data, "vertex_location"),
		VertexCredentials:     data["vertex_credentials"],
		UseAnthropicEndpoints: useAnthropic,
	}
}

// modelConnectionChecker reads the model-test capability off the composed
// gateway client, for the reason providerModelLister gives: one client, one
// configuration, no second option for a composition root to forget.
func (h *Handler) modelConnectionChecker() ModelConnectionChecker {
	if h.connectionChecker == nil {
		return nil
	}
	checker, ok := h.connectionChecker.(ModelConnectionChecker)
	if !ok {
		return nil
	}
	return checker
}

// llmModelCheckInput is the part of a posted llm_model form the test uses.
type llmModelCheckInput struct {
	model      string
	title      string
	private    bool
	credential map[string]any
	// configID is the saved row the form edits; "" on the create form.
	configID string
}

// llmModelConfigurationIDField is the body key that names the saved row the
// form edits. It is not a field of the llm_model form: the edit screen adds
// it, so the server resolves a private reference for the row's author.
const llmModelConfigurationIDField = "configuration_id"

// readLLMModelCheckInput reads the form values the test needs and says which
// required value is missing. The reference is REBUILT from its title and
// private flag: any other key the browser put beside them is dropped.
func readLLMModelCheckInput(data map[string]any) (llmModelCheckInput, string) {
	input := llmModelCheckInput{model: strings.TrimSpace(strVal(data, "name"))}
	switch id := data[llmModelConfigurationIDField].(type) {
	case string:
		input.configID = strings.TrimSpace(id)
	case float64:
		if id > 0 && id <= math.MaxInt32 && id == math.Trunc(id) {
			input.configID = strconv.FormatInt(int64(id), 10)
		}
	}
	reference, _ := data["ai_credentials"].(map[string]any)
	input.title = strings.TrimSpace(firstStrVal(reference, "elitea_title", "alita_title"))
	switch {
	case input.title == "":
		return input, "Select AI credentials before you test the connection."
	case input.model == "":
		return input, "Enter the model name before you test the connection."
	}
	input.private, _ = reference["private"].(bool)
	input.credential = map[string]any{"elitea_title": input.title, "private": input.private}
	return input, ""
}

// writeModelCheckFailure writes the 400 every failed test answers with.
func writeModelCheckFailure(w http.ResponseWriter, message string, extra map[string]any) {
	body := map[string]any{"success": false, "message": message}
	for key, value := range extra {
		body[key] = value
	}
	writeJSON(w, http.StatusBadRequest, body)
}

// checkLLMModelConnection runs the llm_model test. The HTTP contract is the
// credential test's: 200 only when the model answered, 400 with
// {"success":false,"message":...} for every failure, and 429 when the caller
// is over the test rate.
func (h *Handler) checkLLMModelConnection(w http.ResponseWriter, r *http.Request, projectID string, data map[string]any) {
	ctx := r.Context()
	input, missing := readLLMModelCheckInput(data)
	if missing != "" {
		writeModelCheckFailure(w, missing, map[string]any{"reason": "missing_fields"})
		return
	}
	checker := h.modelConnectionChecker()
	if checker == nil || h.storedResolver == nil {
		slog.ErrorContext(ctx, "check_connection: the llm_model test is not composed",
			"project_id", projectID, "checker", checker != nil, "resolver", h.storedResolver != nil)
		writeModelCheckFailure(w, storedConnectionCheckUnavailableMessage, nil)
		return
	}
	owner, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || owner <= 0 || owner > math.MaxInt32 {
		writeModelCheckFailure(w, "invalid project", nil)
		return
	}
	caller, hasCaller := currentConfigurationMutationAuthorID(ctx)

	release := h.acquireModelProbe(projectID, caller, hasCaller)
	if release == nil {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"success": false, "reason": "rate_limited",
			"message": "Rate limited: too many model tests. Wait a minute, then try again.",
		})
		return
	}
	defer release()

	authorID, refusal, status := h.llmModelResolutionAuthor(ctx, projectID, input, caller, hasCaller)
	if refusal != "" {
		writeJSON(w, status, map[string]any{"success": false, "message": refusal})
		return
	}
	resolution := StoredConfigurationResolution{
		ProjectID: int32(owner),
		AuthorID:  authorID,
		Data:      map[string]any{"ai_credentials": input.credential},
	}
	resolved, err := h.storedResolver.ResolveStoredConfiguration(ctx, resolution)
	credential, _ := resolved["ai_credentials"].(map[string]any)
	if err != nil || credential == nil {
		slog.WarnContext(ctx, "check_connection: the llm_model credential did not resolve",
			"project_id", projectID, "err", err)
		writeModelCheckFailure(w,
			"The selected AI credentials could not be resolved. Check that they still exist and that their secret is set.", nil)
		return
	}
	credentialType := strVal(credential, "configuration_type")
	if _, checkable := checkableConnectionTypes[credentialType]; !checkable {
		writeModelCheckFailure(w, "Testing a model is not supported yet for this credential type.",
			map[string]any{"reason": ToolkitCheckReasonUnsupportedType, "unsupported": true})
		return
	}
	// The guard runs on the RESOLVED credential: its api_base is the value
	// that reaches the provider.
	if err := validateNotSelfReferential(credential, selfLLMOrigins()); err != nil {
		writeModelCheckFailure(w, err.Error(), nil)
		return
	}
	credentialProject, _ := int64Value(credential["configuration_project_id"])
	if message := h.refusePlatformCredentialProbe(ctx, owner, credentialProject, input); message != "" {
		writeModelCheckFailure(w, message, nil)
		return
	}

	userID := ""
	if hasCaller {
		userID = strconv.FormatInt(int64(caller), 10)
	}
	result, err := checker.CheckModel(WithConnectionCheckProjectID(ctx, projectID), ModelConnectionCheck{
		CredentialType: credentialType,
		Credential:     credential,
		Model:          input.model,
		UserID:         userID,
	})
	// The audit record of the spend: who tested which model with whose key.
	slog.InfoContext(ctx, "check_connection: llm_model test",
		"project_id", projectID, "user_id", userID, "configuration_id", input.configID,
		"credential_project_id", credentialProject, "credential_type", credentialType,
		"model", input.model, "success", err == nil && result.Success, "reason", result.Reason)
	if err != nil {
		slog.ErrorContext(ctx, "check_connection: llm_model gateway call failed",
			"project_id", projectID, "type", credentialType, "err", err)
		writeModelCheckFailure(w, "Could not verify the connection right now. Please try again.", nil)
		return
	}
	if result.Success {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "message": result.Message, "latency_ms": result.LatencyMS,
		})
		return
	}
	extra := map[string]any{"reason": result.Reason, "latency_ms": result.LatencyMS}
	if result.Reason == ToolkitCheckReasonUnsupportedType {
		extra["unsupported"] = true
	}
	writeModelCheckFailure(w, result.Message, extra)
}

// acquireModelProbe takes one test from the project and user's bound.
func (h *Handler) acquireModelProbe(projectID string, caller int32, hasCaller bool) func() {
	if h.modelProbes == nil {
		return func() {}
	}
	user := "-"
	if hasCaller {
		user = strconv.FormatInt(int64(caller), 10)
	}
	return h.modelProbes.acquire(projectID + "/" + user)
}

// llmModelResolutionAuthor returns the identity a `private: true` reference
// resolves against. It is the identity the SAVED row is admitted with:
//
//   - on the create form (no configuration id), the caller, who becomes the
//     row's author;
//   - on the edit form, the row's author, as admission
//     (configurationAdmissionSnapshot) and the stored checks
//     (storedResolutionFor) resolve it.
//
// A caller who is not the author may test a private reference only as it is
// saved: the same model name and the same reference. Any other value would
// spend the author's personal credential on a model the author never chose.
// A non-private reference does not read the author at all.
func (h *Handler) llmModelResolutionAuthor(
	ctx context.Context, projectID string, input llmModelCheckInput, caller int32, hasCaller bool,
) (*int32, string, int) {
	var callerID *int32
	if hasCaller && caller > 0 {
		callerID = &caller
	}
	if input.configID == "" {
		return callerID, "", 0
	}
	row, found, err := h.loadLLMModelRow(ctx, projectID, input.configID)
	if err != nil {
		slog.ErrorContext(ctx, "check_connection: could not read the llm_model row",
			"project_id", projectID, "configuration_id", input.configID, "err", err)
		return nil, storedConnectionCheckUnavailableMessage, http.StatusBadRequest
	}
	if !found || row.configType != llmModelConfigurationType {
		return nil, "configuration not found", http.StatusNotFound
	}
	var author *int32
	if row.authorID != nil && *row.authorID > 0 && *row.authorID <= math.MaxInt32 {
		value := int32(*row.authorID)
		author = &value
	}
	if !input.private {
		return author, "", 0
	}
	if author == nil {
		return nil, "This model has no author, so its private AI credentials cannot be resolved.", http.StatusBadRequest
	}
	if callerID != nil && *callerID == *author {
		return author, "", 0
	}
	if savedLLMModelMatches(row.data, input) {
		return author, "", 0
	}
	return nil, "This model uses its author's private AI credentials. Only the author can test it with " +
		"other values. Save the model, or test it with credentials that are not private.", http.StatusBadRequest
}

// savedLLMModelMatches reports whether the form tests the row as it is saved:
// the same model name and the same reference.
func savedLLMModelMatches(saved map[string]any, input llmModelCheckInput) bool {
	if strings.TrimSpace(strVal(saved, "name")) != input.model {
		return false
	}
	reference, _ := saved["ai_credentials"].(map[string]any)
	private, _ := reference["private"].(bool)
	return strings.TrimSpace(firstStrVal(reference, "elitea_title", "alita_title")) == input.title &&
		private == input.private
}

// loadLLMModelRow reads the saved row the edit form names, from the schema of
// the project in the path. A row of another project is not found.
func (h *Handler) loadLLMModelRow(ctx context.Context, projectID, configID string) (storedConfigurationRow, bool, error) {
	if h.llmModelRow != nil {
		return h.llmModelRow(ctx, projectID, configID)
	}
	schema, err := tenantschema.Quote(projectID)
	if err != nil {
		return storedConfigurationRow{}, false, nil
	}
	return h.loadStoredConfigurationRow(ctx, schema, configID)
}

// refusePlatformCredentialProbe returns a refusal when the resolved credential
// is a PLATFORM credential (a shared row of the public project) and the model
// is not one the platform publishes with it. "" admits the test.
//
// The expander falls back to the public project's shared credentials when the
// title is not in the project. Without this rule, any editor of any project
// could spend the platform key on any model name the key can reach, outside
// every budget. A model the platform publishes is one an operator chose for
// that key, and the runtime already serves it to the project.
func (h *Handler) refusePlatformCredentialProbe(
	ctx context.Context, owner, credentialProject int64, input llmModelCheckInput,
) string {
	platform := false
	switch {
	case h.publicProjectID > 0:
		platform = credentialProject == int64(h.publicProjectID) && owner != int64(h.publicProjectID)
	default:
		// The public project is not configured here, so a credential of
		// another project cannot be told apart from a platform one. A private
		// reference resolves in the author's personal project, which is the
		// one case that is not a platform credential.
		platform = credentialProject != owner && !input.private
	}
	if !platform {
		return ""
	}
	exposed, err := h.platformModelPublished(ctx, input.model, input.title)
	if err != nil {
		slog.ErrorContext(ctx, "check_connection: could not read the platform models", "err", err)
		return storedConnectionCheckUnavailableMessage
	}
	if !exposed {
		return "These AI credentials belong to the platform. A model can be tested with them only " +
			"when the platform publishes that model with them."
	}
	return ""
}

// platformModelPublishedSQL finds a shared llm_model row of the public project
// with this model name and this credential.
const platformModelPublishedSQL = `
	SELECT EXISTS (
	  SELECT 1 FROM %s.configuration
	   WHERE shared = true AND type = 'llm_model'
	     AND btrim(data->>'name') = $1
	     AND btrim(COALESCE(NULLIF(data->'ai_credentials'->>'elitea_title', ''),
	                        data->'ai_credentials'->>'alita_title')) = $2)`

func (h *Handler) platformModelPublished(ctx context.Context, model, title string) (bool, error) {
	if h.platformModelExposed != nil {
		return h.platformModelExposed(ctx, model, title)
	}
	if h.pool == nil || h.publicProjectID <= 0 {
		return false, errConfigurationStoreUnavailable
	}
	schema := pgx.Identifier{fmt.Sprintf("p_%d", h.publicProjectID)}.Sanitize()
	var exists bool
	if err := h.pool.QueryRow(ctx, fmt.Sprintf(platformModelPublishedSQL, schema), model, title).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// int64Value reads a JSON or Go integer.
func int64Value(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int32:
		return int64(number), true
	case int64:
		return number, true
	case float64:
		if number == math.Trunc(number) {
			return int64(number), true
		}
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil
	}
	return 0, false
}
